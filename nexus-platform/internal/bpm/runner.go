package bpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// ----------------------------------------------------------------------------------------------
// Runner — the walk through the machine nodes of a graph (specs/043 S7).
//
// A person's decision, a submit and an arm each end by landing the instance on a node. When that
// node is a person's (approval / task) the instance waits there, as it always has. When it is a
// machine node — a gateway that picks a branch from the record's values, an auto node that writes
// fields onto the record, an end node that settles the instance — somebody has to keep walking, and
// that is the Runner: it executes the node, hops to the next one, and stops on a person's node, a
// terminal state or a failure.
//
// The instance DURABLY stands on a machine node between hops (current_step points at it, status
// pending, no seat), and every hop is one guarded write (seq CAS) — so a server stopped mid-walk
// leaves a row the bpm_continue job picks up where it stood (FR-009), and a failed node leaves it
// there with last_error until an admin fixes the cause and presses "Thử lại".
// ----------------------------------------------------------------------------------------------

// MaxAutoHops bounds one walk: a graph the validator accepted cannot loop through machine nodes
// alone, but a hand-edited pin or a loop through a person's node that is approved at once by the
// same rule must not spin a worker forever (plan: trần 32 bước nhảy/lần).
const MaxAutoHops = 32

// DefaultBranch is the name recorded in the context (and the history) when no branch of a gateway
// held and its default was taken.
const DefaultBranch = "default"

// RecordReader reads the CURRENT values of a record, bypassing the row scope of any caller: a
// gateway routes on the record's true state, and routing must not depend on who pressed a button
// (the same reason Submit evaluates entry criteria on a system read). Tenant-pinned by contract.
type RecordReader interface {
	GetRecord(ctx context.Context, tenantID, entityName, recordID string) (map[string]any, error)
}

// FieldsWriter writes several fields onto a record in ONE validated update, through the same path a
// person's edit takes (picklist membership, computed-field refusal, before-save rules). All or
// nothing — an auto node never leaves a record half-written.
type FieldsWriter interface {
	SetFields(ctx context.Context, tenantID, entityName, recordID string, fields map[string]any, actorUserID string) error
}

// HistoryAppender appends one line to the history of an instance in ANY state, without moving it
// (no seq bump): the only way to say "the write-back after the settlement failed" on an instance
// that is already terminal. Optional on the store — the SQL repository has it, a unit-test fake
// need not (the failure is then logged only).
type HistoryAppender interface {
	AppendHistory(ctx context.Context, tenantID, id string, e HistoryEntry) error
}

// Continuer walks an instance forward from the machine node it stands on. Satisfied by *Runner;
// the arming engine, the decider and the requester depend on this, not on the struct.
type Continuer interface {
	Continue(ctx context.Context, tenantID, instanceID string) error
}

// Runner executes the machine nodes of the instances it is handed. Safe for concurrent use: every
// state write is a seq CAS, so two walkers on the same instance cannot both advance it.
type Runner struct {
	store     InstanceStore
	reader    RecordReader
	fields    FieldsWriter
	records   RecordWriter // the process outcome write-back an end node performs (optional)
	inbox     InboxWriter
	deadlines DeadlineCalculator
	router    Router // optional: resolves field / submitter assignments of the step a hop opens (S6a)
	now       func() time.Time
	logger    *zap.Logger
}

// NewRunner wires the runner over the instance store. reader / fields may be nil: a graph that
// needs one then stalls its instance with a readable error instead of panicking (unit tests, a
// deploy that has not wired the record service).
func NewRunner(store InstanceStore, reader RecordReader, fields FieldsWriter, deadlines DeadlineCalculator, logger *zap.Logger) *Runner {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Runner{store: store, reader: reader, fields: fields, deadlines: deadlines, now: time.Now, logger: logger}
}

// WithRouter arms the routing of a person's step the walk lands on (S6a): the same router the
// engine, decider and requester use.
func (r *Runner) WithRouter(rt Router) *Runner {
	r.router = rt
	return r
}

// WithRecordWriter arms the end node's outcome write-back (the same writer the Decider uses).
func (r *Runner) WithRecordWriter(w RecordWriter) *Runner {
	r.records = w
	return r
}

// WithInbox arms the notice a person gets when the walk lands on their step.
func (r *Runner) WithInbox(i InboxWriter) *Runner {
	r.inbox = i
	return r
}

// ErrNotStuck: the instance is not standing on a machine step, so there is nothing to retry.
var ErrNotStuck = fmt.Errorf("approval instance is not stuck on an automatic step")

// Continue walks the instance forward while it stands on a machine node. It returns nil when the
// walk ended anywhere legitimate — on a person's node, in a terminal state, stalled with a
// last_error, or having lost a race to another writer (the winner continues). A non-nil error is
// infrastructure (the store failed): the job queue retries it.
func (r *Runner) Continue(ctx context.Context, tenantID, id string) error {
	for hop := 0; ; hop++ {
		inst, err := r.store.GetInstance(ctx, tenantID, id)
		if errors.Is(err, ErrInstanceNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bpm.Runner.Continue: %w", err)
		}
		if inst.Status != StatusPending || inst.LastError != "" || inst.Definition == nil {
			return nil // finished, waiting for an admin's retry, or pre-pin (never on a machine node)
		}
		node := inst.Definition.NodeAt(inst.CurrentStep)
		if node == nil || !MachineKind(node.Kind) {
			return nil // standing on a person's step: nothing to run
		}
		if hop >= MaxAutoHops {
			return r.stall(ctx, inst, node, fmt.Sprintf("đã tự đi quá %d bước liên tiếp mà chưa tới bước có người xử lý — kiểm tra lại quy trình có vòng lặp", MaxAutoHops))
		}
		moved, err := r.runNode(ctx, inst, node)
		if err != nil {
			return err
		}
		if !moved {
			return nil
		}
	}
}

// Retry restarts the walk of an instance that stalled on a machine node (or was left there by a
// crash with no job to wake it): the error is cleared, the press is written to the history, and
// the node runs again. An admin presses it after fixing the cause (the process or the data).
func (r *Runner) Retry(ctx context.Context, tenantID, id string, actor Actor, comment string) (*Instance, error) {
	inst, err := r.store.GetInstance(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if inst.Status != StatusPending {
		return nil, ErrNotPending
	}
	if !inst.OnMachineNode() {
		return nil, ErrNotStuck
	}
	now := r.now().UTC()
	node, stepName := inst.Definition.StepRef(inst.CurrentStep)
	history := append(inst.History, HistoryEntry{
		Step: inst.CurrentStep, Action: HistoryRetried, Actor: actor.UserID, Comment: strings.TrimSpace(comment),
		Node: node, StepName: stepName, Version: inst.Definition.version(), At: now,
	})
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("bpm.Runner.Retry: marshal history: %w", err)
	}
	// A plain state write: LastError left empty clears the error, Continue parks the job.
	if err := r.store.DecideTaskTx(ctx, tenantID, id, TaskGuard{ExpectSeq: inst.Seq}, InstanceTransition{
		CurrentStep: inst.CurrentStep, Status: StatusPending,
		CurrentApproverType: inst.CurrentApproverType, CurrentApproverID: inst.CurrentApproverID,
		SLADueAt: inst.SLADueAt, Escalated: inst.Escalated, Continue: true,
	}, historyJSON); err != nil {
		return nil, txError("bpm.Runner.Retry", err)
	}
	if err := r.Continue(ctx, tenantID, id); err != nil {
		return nil, err
	}
	return r.store.GetInstance(ctx, tenantID, id)
}

// runNode executes one machine node and hops. moved=false with a nil error means the walk should
// stop here (stalled, settled, or another writer moved the instance first).
func (r *Runner) runNode(ctx context.Context, inst *Instance, node *Node) (bool, error) {
	switch node.Kind {
	case NodeGateway:
		return r.runGateway(ctx, inst, node)
	case NodeAuto:
		return r.runAuto(ctx, inst, node)
	case NodeWait:
		return r.runWait(ctx, inst, node)
	default:
		return r.runEnd(ctx, inst, node)
	}
}

func (r *Runner) runGateway(ctx context.Context, inst *Instance, node *Node) (bool, error) {
	if r.reader == nil {
		return false, r.stall(ctx, inst, node, "chưa nối được bộ đọc hồ sơ để xét nhánh")
	}
	// The record as it is NOW, not as it was when the approval started: "trên 50 triệu" is judged
	// on the amount a person may have edited since (FR-024).
	data, err := r.reader.GetRecord(ctx, inst.TenantID, inst.EntityName, inst.RecordID)
	if err != nil {
		return false, r.stall(ctx, inst, node, "không đọc được hồ sơ để xét nhánh: "+reason(err))
	}
	target, branch := node.Default, DefaultBranch
	for _, b := range node.Branches {
		if workflow.Evaluate(b.When, data) {
			target, branch = b.To, b.Name
			break
		}
	}
	idx := inst.Definition.Graph.IndexOf(target)
	if idx < 0 {
		return false, r.stall(ctx, inst, node, fmt.Sprintf("nhánh %q trỏ tới bước %q không có trong quy trình", branch, target))
	}
	ctxNext, err := inst.Context.WithGateway(node.ID, branch)
	if err != nil {
		return false, fmt.Errorf("bpm.Runner.runGateway: %w", err)
	}
	ctxJSON, err := json.Marshal(ctxNext)
	if err != nil {
		return false, fmt.Errorf("bpm.Runner.runGateway: marshal context: %w", err)
	}
	return r.hop(ctx, inst, node, idx, HistoryEntry{Action: HistoryGateway, Comment: branch}, ctxJSON)
}

func (r *Runner) runAuto(ctx context.Context, inst *Instance, node *Node) (bool, error) {
	if r.fields == nil {
		return false, r.stall(ctx, inst, node, "chưa nối được bộ ghi hồ sơ để thực hiện bước tự động")
	}
	fields := make(map[string]any, len(node.Set))
	for _, s := range node.Set {
		fields[s.Field] = s.Value
	}
	// ponytail: a crash between this write and the hop below runs the node again on resume — the
	// writes are constants, so the second pass leaves the record as the first did. A node that
	// ever writes a computed value must move the hop and the write into one unit.
	if err := r.fields.SetFields(ctx, inst.TenantID, inst.EntityName, inst.RecordID, fields, workflow.SystemActorID); err != nil {
		return false, r.stall(ctx, inst, node, "ghi trường lên hồ sơ không được: "+reason(err))
	}
	idx, end, ok := inst.Definition.Successor(inst.CurrentStep)
	if !ok {
		return false, r.stall(ctx, inst, node, fmt.Sprintf("bước kế %q không có trong quy trình", node.Next))
	}
	entry := HistoryEntry{Action: HistoryAuto, Comment: setSummary(node.Set)}
	if end {
		return r.settle(ctx, inst, node, StatusApproved, entry)
	}
	return r.hop(ctx, inst, node, idx, entry, nil)
}

func (r *Runner) runEnd(ctx context.Context, inst *Instance, node *Node) (bool, error) {
	st := StatusApproved
	if node.Result == ResultRejected {
		st = StatusRejected
	}
	return r.settle(ctx, inst, node, st, HistoryEntry{Action: HistoryEnded, Comment: node.Result})
}

// settle ends the instance in a terminal state, then writes the process outcome back onto the
// record (best-effort and recorded: a refused write-back must not undo the settlement, but it must
// not vanish either — FR-010). Always returns moved=false: the walk is over.
func (r *Runner) settle(ctx context.Context, inst *Instance, node *Node, st InstanceStatus, entry HistoryEntry) (bool, error) {
	now := r.now().UTC()
	entry = r.fill(inst, node, entry, now)
	history := append(inst.History, entry)
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return false, fmt.Errorf("bpm.Runner.settle: marshal history: %w", err)
	}
	next := InstanceTransition{CurrentStep: inst.CurrentStep, Status: st, Escalated: inst.Escalated}
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, next, historyJSON); err != nil {
		return false, r.lost("bpm.Runner.settle", err)
	}
	if err := writeOutcome(ctx, r.records, r.logger, inst, inst.Definition.Outcome, st, lastDecider(inst.History)); err != nil {
		recordEffectFailed(ctx, r.store, r.logger, inst, node.ID, node.Name, "ghi kết quả duyệt lên hồ sơ không được: "+reason(err), r.now().UTC())
	}
	return false, nil
}

// hop moves the instance from node to the node at idx, in ONE guarded write. entry describes what
// the node just did (its history line). The destination decides what else the write carries: a
// person's node opens its seats and deadline; a machine node parks the job that continues the walk.
// Returns moved=true when the walk should go on from a machine node.
func (r *Runner) hop(ctx context.Context, inst *Instance, from *Node, idx int, entry HistoryEntry, ctxJSON []byte) (bool, error) {
	chain := inst.Definition.Chain()
	if idx < 0 || idx >= len(chain) {
		return false, r.stall(ctx, inst, from, fmt.Sprintf("bước kế (vị trí %d) không có trong quy trình", idx))
	}
	now := r.now().UTC()
	target := chain[idx]
	next := InstanceTransition{CurrentStep: idx, Status: StatusPending, ContextJSON: ctxJSON}
	var basis SLABasis
	if target.IsMachine() {
		next.Continue = true
	} else {
		due, b, err := stepDue(ctx, r.deadlines, inst.TenantID, inst.Definition.CalendarID(), chain, idx, true, now)
		if err != nil {
			// Nobody is waiting on this path to show an error to (a job, or a decision already
			// committed): stamp the wall clock under its own basis rather than stall the walk (D-8).
			r.logger.Warn("bpm: business calendar unreadable — deadline stamped on the wall clock",
				zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID), zap.Error(err))
			due, b = nil, ""
			if target.SLASeconds > 0 {
				t := now.Add(time.Duration(target.SLASeconds) * time.Second)
				due, b = &t, SLABasisWallCalendarError
			}
		}
		basis = b
		next.SLADueAt = due
		var opts *Options
		if inst.Definition != nil {
			opts = inst.Definition.Options
		}
		seats, stuck, rerr := routeSeats(ctx, r.router,
			RouteInput{TenantID: inst.TenantID, ProcessID: inst.ProcessID, EntityName: inst.EntityName, RecordID: inst.RecordID, Submitter: inst.CreatedBy},
			opts, target, due, basis)
		if rerr != nil {
			return false, r.stall(ctx, inst, from, "không xác định được người xử lý bước kế: "+reason(rerr))
		}
		next.Seats.Open, next.LastError = seats, stuck
		next.CurrentApproverType, next.CurrentApproverID = projection(next.Seats.Open)
	}
	entry = r.fill(inst, from, entry, now)
	entry.SLABasis = basis
	history := append(inst.History, entry)
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return false, fmt.Errorf("bpm.Runner.hop: marshal history: %w", err)
	}
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, next, historyJSON); err != nil {
		return false, r.lost("bpm.Runner.hop", err)
	}
	if !target.IsMachine() {
		notifyHolders(ctx, r.store, r.inbox, r.logger, inst.TenantID, inst.EntityName, inst.RecordID, target, "đang chờ bạn phê duyệt", next.Seats.Open)
	}
	return target.IsMachine(), nil
}

// stall leaves the instance where it is, pending, with the reason — and says so in the history so
// the Nexus team sees it on the record's timeline, not only in a log (FR-009, FR-010). A stall is
// not an error of the walk: only a failed store write is returned.
func (r *Runner) stall(ctx context.Context, inst *Instance, node *Node, msg string) error {
	msg = clip(msg, 480) // last_error VARCHAR(500)
	r.logger.Warn("bpm: instance stalled on an automatic step",
		zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID),
		zap.String("node", node.ID), zap.String("reason", msg))
	entry := r.fill(inst, node, HistoryEntry{Action: HistoryEffectFailed, Comment: msg}, r.now().UTC())
	history := append(inst.History, entry)
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return fmt.Errorf("bpm.Runner.stall: marshal history: %w", err)
	}
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, InstanceTransition{
		CurrentStep: inst.CurrentStep, Status: StatusPending, LastError: msg,
		CurrentApproverType: inst.CurrentApproverType, CurrentApproverID: inst.CurrentApproverID,
		SLADueAt: inst.SLADueAt, Escalated: inst.Escalated,
	}, historyJSON); err != nil {
		return r.lost("bpm.Runner.stall", err)
	}
	return nil
}

// lost maps the benign outcomes of a guarded write — another writer moved the instance, or it is
// no longer pending — to nil (the winner continues the walk); anything else is a real failure.
func (r *Runner) lost(op string, err error) error {
	if errors.Is(err, ErrConcurrent) || errors.Is(err, ErrNotPending) || errors.Is(err, ErrInstanceNotFound) {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

// fill stamps a history line with where it happened and when.
func (r *Runner) fill(inst *Instance, node *Node, e HistoryEntry, now time.Time) HistoryEntry {
	e.Step, e.Node, e.StepName = inst.CurrentStep, node.ID, node.Name
	e.Version, e.At = inst.Definition.version(), now
	return e
}

// recordEffectFailed says in the history of inst that an effect after a committed step failed
// (FR-010) — in addition to the log, because the log is not where the Nexus team looks. Best-effort:
// a store without HistoryAppender (a unit-test fake) leaves only the log line.
func recordEffectFailed(ctx context.Context, store InstanceStore, logger *zap.Logger, inst *Instance, node, stepName, msg string, at time.Time) {
	logger.Error("bpm: step committed but its effect FAILED",
		zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID), zap.String("reason", clip(msg, 200)))
	ha, ok := store.(HistoryAppender)
	if !ok {
		return
	}
	if err := ha.AppendHistory(ctx, inst.TenantID, inst.ID, HistoryEntry{
		Step: inst.CurrentStep, Action: HistoryEffectFailed, Comment: clip(msg, 480),
		Node: node, StepName: stepName, Version: inst.Definition.version(), At: at,
	}); err != nil {
		logger.Error("bpm: could not record the failed effect in the history",
			zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID), zap.Error(err))
	}
}

// lastDecider is the person who last decided or completed a step (the actor an end node's
// write-back is filed under); the system actor when nobody did — an approval settled by the rules
// alone.
func lastDecider(history []HistoryEntry) string {
	for i := len(history) - 1; i >= 0; i-- {
		switch history[i].Action {
		case HistoryApproved, HistoryRejected, HistoryCompleted:
			if history[i].Actor != "" {
				return history[i].Actor
			}
		}
	}
	return workflow.SystemActorID
}

// setSummary renders an auto node's writes for the timeline: "status=Chấp nhận, tier=A", in name
// order so the line is stable.
func setSummary(set []SetField) string {
	parts := make([]string, 0, len(set))
	for _, s := range set {
		parts = append(parts, fmt.Sprintf("%s=%v", s.Field, s.Value))
	}
	sort.Strings(parts)
	return clip(strings.Join(parts, ", "), 400)
}

// reason is the message an admin should read for a failed effect: the innermost error, without the
// "services.X.method:" prefix every layer wraps it in.
func reason(err error) string {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err.Error()
		}
		err = u
	}
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
