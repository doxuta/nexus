package bpm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// The wait node (specs/043 lát S12a, FR-020…): "chờ phản hồi đầu tiên". Nobody holds it. The walk
// lands on it, reads the record, and either goes on (`until` holds — the record already answered)
// or lets the instance REST there: pending, no seat, current_step on the wait node. Two things wake a
// resting instance:
//
//   - a record signal — every after_update of the record asks the open instances standing on a wait
//     node to look at `until` again (AsyncEngine.signalWaits → Runner.Continue);
//   - the deadline — `sla` picks a row of the matrix by the record's values (priority), times it on
//     the working calendar and parks the usual bpm_sla_due job. A breach has no seat to ping, so it
//     is told to the people `sla.notify` names (SLAScheduler.fireWaitBreach).
//
// A resting wait is recognised by its history (waiting), not by a column or a context key: entering
// the node always appends a line, so a stale marker from an earlier visit (a loop back to the same
// node) can never be mistaken for the present one.

// WaitKeys are the keys S12a adds to a node.
var WaitKeys = []string{"until", "sla"}

// WaitSLAKeys is the closed key set of a wait node's `sla` object. onChange (S14b) joins with the
// code that reads it (D-7).
var WaitSLAKeys = []string{"amount", "unit", "matrix", "writeDueTo", "onChange", "notify"}

// sla.onChange values (S14b).
const (
	OnChangeKeep        = "keep"
	OnChangeTightenOnly = "tighten_only"
)

// Bounds of the config a wait node carries (B6: refused at save, not left to misbehave at run time).
const (
	MaxWaitMatrixRows = 20
	MaxWaitAmount     = 100000 // the same ceiling a workflow timer offset has
)

// SLARow is one row of a wait node's priority matrix: when its condition holds on the record, the
// deadline is Amount Unit after the node was entered. The first row that holds wins.
type SLARow struct {
	When   *workflow.Condition `json:"when"`
	Amount int                 `json:"amount"`
	Unit   string              `json:"unit"`
}

// WaitSLA is a wait node's deadline. Amount+Unit is the base the matrix overrides (either alone is
// enough, both may be set); WriteDueTo names a datetime field of the record that receives the
// deadline; Notify names who is told when it is missed — mandatory (D-18: a deadline nobody hears
// about is not an SLA).
type WaitSLA struct {
	Amount     int      `json:"amount,omitempty"`
	Unit       string   `json:"unit,omitempty"`
	Matrix     []SLARow `json:"matrix,omitempty"`
	WriteDueTo string   `json:"writeDueTo,omitempty"`
	// OnChange: "" / "keep" = the deadline is fixed when the node is entered; "tighten_only" = a
	// record update re-reads the matrix from the entry time and keeps the EARLIER of the two (S14b).
	OnChange string      `json:"onChange,omitempty"`
	Notify   *Assignment `json:"notify,omitempty"`
	// unknown lists the keys outside WaitSLAKeys; a node carrying any is refused (fail-closed).
	unknown []string
}

// UnmarshalJSON decodes the object and records its keys outside WaitSLAKeys.
func (w *WaitSLA) UnmarshalJSON(b []byte) error {
	type plain WaitSLA
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("bpm.WaitSLA.UnmarshalJSON: %w", err)
	}
	*w = WaitSLA(p)
	w.unknown = UnknownKeys(b, WaitSLAKeys...)
	return nil
}

// notifyTypes are the assignment types `sla.notify` may name: one person, a role, a team, the person
// in a user field, the submitter — never a queue (a pool nobody is sure to read) nor a strategy.
func notifyTypeOK(t AssignmentType) bool {
	switch t {
	case AssignUser, AssignRole, AssignTeam, AssignField, AssignSubmitter:
		return true
	}
	return false
}

// CheckWait says why a wait node cannot run, as the key to blame (relative to the node) and a
// reason; ("", "") = runnable. ONE body for the engine (a stored graph it cannot run is refused whole
// — fail-closed) and for the API that answers the admin.
func CheckWait(n Node) (field, reason string) {
	if n.Until == nil || len(n.Until.Rules) == 0 {
		return "until", "bước chờ cần điều kiện `until` (ít nhất một quy tắc) — điều kiện hồ sơ phải đạt để đi tiếp"
	}
	for _, r := range n.Until.Rules {
		if r.Op == workflow.OpChanged || r.Op == workflow.OpChangedTo {
			return "until", "«vừa đổi»/«vừa đổi thành» không dùng được ở bước chờ — nó xét giá trị hiện tại của hồ sơ, không có bản trước; dùng «≠»/«=»"
		}
	}
	s := n.SLA
	if s == nil {
		return "", ""
	}
	if len(s.unknown) > 0 {
		return "sla." + s.unknown[0], "sla có khoá mà engine chưa chạy được: " + strings.Join(s.unknown, ", ")
	}
	if s.Notify == nil {
		return "sla.notify", "bước chờ có hạn (sla) thì phải khai người nhận cảnh báo quá hạn (sla.notify)"
	}
	if len(s.Matrix) > MaxWaitMatrixRows {
		return "sla.matrix", fmt.Sprintf("ma trận SLA có quá %d hàng", MaxWaitMatrixRows)
	}
	if s.Amount == 0 && s.Unit == "" && len(s.Matrix) == 0 {
		return "sla.amount", "sla cần hạn mặc định (amount + unit) hoặc ít nhất một hàng ma trận"
	}
	if s.Amount != 0 || s.Unit != "" {
		if f, why := checkWaitAmount("sla", s.Amount, s.Unit); f != "" {
			return f, why
		}
	}
	for i, row := range s.Matrix {
		if row.When == nil || len(row.When.Rules) == 0 {
			return fmt.Sprintf("sla.matrix[%d].when", i), "hàng ma trận cần điều kiện (ít nhất một quy tắc) — hạn mặc định đặt ở sla.amount/sla.unit"
		}
		for _, r := range row.When.Rules {
			if r.Op == workflow.OpChanged || r.Op == workflow.OpChangedTo {
				return fmt.Sprintf("sla.matrix[%d].when", i), "«vừa đổi»/«vừa đổi thành» không dùng được ở ma trận SLA — nó xét giá trị hiện tại của hồ sơ"
			}
		}
		if f, why := checkWaitAmount(fmt.Sprintf("sla.matrix[%d]", i), row.Amount, row.Unit); f != "" {
			return f, why
		}
	}
	if s.OnChange != "" && s.OnChange != OnChangeKeep && s.OnChange != OnChangeTightenOnly {
		return "sla.onChange", fmt.Sprintf("sla.onChange không hợp lệ: %q (keep/tighten_only)", s.OnChange)
	}
	if s.WriteDueTo != "" && !fieldNamePattern.MatchString(s.WriteDueTo) {
		return "sla.writeDueTo", fmt.Sprintf("sla.writeDueTo là tên trường ngày giờ của hồ sơ, nhận: %q", s.WriteDueTo)
	}
	if !notifyTypeOK(s.Notify.Type) {
		return "sla.notify.type", fmt.Sprintf("sla.notify.type không hợp lệ: %q (user/role/team/field/submitter)", s.Notify.Type)
	}
	if s.Notify.Strategy != "" && s.Notify.Strategy != StrategyPool {
		return "sla.notify.strategy", "sla.notify báo cho cả người/nhóm/vai được khai — không chia theo chiến lược"
	}
	if f, why := CheckAssignment(Step{Assignment: s.Notify}); f != "" {
		return "sla.notify" + strings.TrimPrefix(f, "assignment"), why
	}
	return "", ""
}

// checkWaitAmount judges one {amount, unit} pair: the unit blamed as prefix.unit, the size as prefix.amount.
func checkWaitAmount(prefix string, amount int, unit string) (field, reason string) {
	switch {
	case !workflow.ValidTimerUnit(unit):
		return prefix + ".unit", fmt.Sprintf("đơn vị hạn không hợp lệ: %q (minutes/hours/days/business_hours)", unit)
	case amount < 1 || amount > MaxWaitAmount:
		return prefix + ".amount", fmt.Sprintf("số lượng hạn phải từ 1 đến %d, nhận: %d", MaxWaitAmount, amount)
	}
	return "", ""
}

// pickAmount is the deadline length for a record: the first matrix row whose condition holds on the
// record's CURRENT values, else the base. ok=false when neither applies (no deadline this visit).
func (s *WaitSLA) pickAmount(data map[string]any) (amount int, unit string, ok bool) {
	for _, row := range s.Matrix {
		if workflow.Evaluate(row.When, data) {
			return row.Amount, row.Unit, true
		}
	}
	if s.Amount > 0 {
		return s.Amount, s.Unit, true
	}
	return 0, "", false
}

// waitDeadline times a wait node entered at now: the matrix row for the record, walked on the
// working calendar for business_hours (wall clock for the other units, as set_deadline does). nil
// when the node has no deadline or no row applies. err is the calendar store's, untouched — the
// caller degrades to the wall clock (nobody is waiting to be shown an error).
func waitDeadline(ctx context.Context, calc DeadlineCalculator, tenantID, calendarID string, n *Node, data map[string]any, now time.Time) (*time.Time, SLABasis, error) {
	if n.SLA == nil {
		return nil, "", nil
	}
	amount, unit, ok := n.SLA.pickAmount(data)
	if !ok {
		return nil, "", nil
	}
	var d time.Duration
	switch unit {
	case workflow.TimerUnitMinutes:
		d = time.Duration(amount) * time.Minute
	case workflow.TimerUnitHours, workflow.TimerUnitBusinessHours:
		d = time.Duration(amount) * time.Hour
	default: // days: calendar-blind, like set_deadline
		d = time.Duration(amount) * 24 * time.Hour
	}
	if unit != workflow.TimerUnitBusinessHours || calc == nil { // calc == nil only in unit tests
		t := now.Add(d)
		return &t, SLABasisWall, nil
	}
	due, basis, err := calc.DueAt(ctx, tenantID, calendarID, now, d)
	if err != nil {
		return nil, "", err
	}
	return &due, basis, nil
}

// resting reports whether inst already rests on its wait node: the newest line that is not a breach
// is the waiting mark of THIS visit. Entering the node (a hop, a decision, a resume) appends its own
// line, so a mark left by an earlier visit is never the newest.
func resting(inst *Instance) bool {
	for i := len(inst.History) - 1; i >= 0; i-- {
		switch inst.History[i].Action {
		case HistoryEscalated, HistoryBreachNoRecipient, HistoryEffectFailed, HistoryRepinned, HistoryPaused, HistoryResumed:
			// the deadline passed, the due-field write failed or an admin moved the version while it
			// waited: still the same visit (a repin keeps the deadline and the step — S11)
			continue
		case HistoryWaiting:
			return inst.History[i].Step == inst.CurrentStep
		default:
			return false
		}
	}
	return false
}

// runWait looks at a wait node the instance stands on. The condition holds → the wait is over and
// the walk goes on (a rest, if any, is closed by the line this writes). It does not → the instance
// rests (once): the deadline is stamped and its breach job parked in the same guarded write, then the
// optional writeDueTo is written onto the record — AFTER the rest is committed, because that write is
// itself a record update whose signal must find the instance already resting (else the signal would
// rest it a second time and re-stamp the deadline).
func (r *Runner) runWait(ctx context.Context, inst *Instance, node *Node) (bool, error) {
	if r.reader == nil {
		return false, r.stall(ctx, inst, node, "chưa nối được bộ đọc hồ sơ để xét điều kiện chờ")
	}
	data, err := r.reader.GetRecord(ctx, inst.TenantID, inst.EntityName, inst.RecordID)
	if err != nil {
		return false, r.stall(ctx, inst, node, "không đọc được hồ sơ để xét điều kiện chờ: "+reason(err))
	}
	if workflow.Evaluate(node.Until, data) {
		idx, end, ok := inst.Definition.Successor(inst.CurrentStep)
		if !ok {
			return false, r.stall(ctx, inst, node, fmt.Sprintf("bước kế %q không có trong quy trình", node.Next))
		}
		entry := HistoryEntry{Action: HistoryWaitMet}
		if end {
			return r.settle(ctx, inst, node, StatusApproved, entry)
		}
		return r.hop(ctx, inst, node, idx, entry, nil)
	}
	if resting(inst) {
		// already waiting for the record: nothing to stamp, but a tighten_only deadline may shorten
		return false, r.tightenWait(ctx, inst, node, data)
	}
	now := r.now().UTC()
	due, basis, derr := waitDeadline(ctx, r.deadlines, inst.TenantID, inst.Definition.CalendarID(), node, data, now)
	if derr != nil {
		r.logger.Warn("bpm: business calendar unreadable — wait deadline stamped on the wall clock",
			zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID), zap.Error(derr))
		due, basis = nil, ""
		if amount, unit, ok := node.SLA.pickAmount(data); ok {
			t := now.Add(time.Duration(amount) * waitUnit(unit))
			due, basis = &t, SLABasisWallCalendarError
		}
	}
	entry := r.fill(inst, node, HistoryEntry{Action: HistoryWaiting}, now)
	entry.SLABasis = basis
	historyJSON, err := json.Marshal(append(inst.History, entry))
	if err != nil {
		return false, fmt.Errorf("bpm.Runner.runWait: marshal history: %w", err)
	}
	// A new visit starts un-escalated; the guarded write also parks bpm_sla_due for the new seq.
	tr := InstanceTransition{CurrentStep: inst.CurrentStep, Status: StatusPending, SLADueAt: due}
	if due != nil && node.SLA != nil && node.SLA.WriteDueTo != "" { // remember what WE wrote (S14b)
		if tr.ContextJSON, err = inst.Context.withWrittenDue(*due); err != nil {
			return false, fmt.Errorf("bpm.Runner.runWait: %w", err)
		}
	}
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, tr, historyJSON); err != nil {
		return false, r.lost("bpm.Runner.runWait", err)
	}
	if due != nil && node.SLA != nil && node.SLA.WriteDueTo != "" {
		if r.fields == nil {
			recordEffectFailed(ctx, r.store, r.logger, inst, node.ID, node.Name, "chưa nối được bộ ghi hồ sơ để ghi hạn xử lý", now)
		} else if err := r.fields.SetFields(ctx, inst.TenantID, inst.EntityName, inst.RecordID,
			map[string]any{node.SLA.WriteDueTo: due.UTC().Format(time.RFC3339)}, workflow.SystemActorID); err != nil {
			recordEffectFailed(ctx, r.store, r.logger, inst, node.ID, node.Name, "ghi hạn xử lý lên trường "+node.SLA.WriteDueTo+" không được: "+reason(err), now)
		}
	}
	return false, nil
}

// WithRouter arms the resolution of a wait node's sla.notify when it names a field or the submitter,
// and of the record owner it falls back to. Chainable. Unwired, only a named user / role / team and
// the process's fallback assignee can be told.
func (s *SLAScheduler) WithRouter(rt Router) *SLAScheduler {
	s.router = rt
	return s
}

// waitRecipients names who a missed wait deadline is told to, first non-empty answer wins: the people
// sla.notify names (a user field of the record such as assigned_to, a user, a role, a team, the
// submitter) → the record's owner → the process's fallback assignee. Capped at breachNotifyCap.
// Empty = nobody could be found; the caller records it (recordNoRecipient).
func (s *SLAScheduler) waitRecipients(ctx context.Context, inst Instance, n *Node) []string {
	in := RouteInput{TenantID: inst.TenantID, EntityName: inst.EntityName, RecordID: inst.RecordID,
		ProcessID: inst.ProcessID, NodeID: n.ID, Submitter: inst.CreatedBy}
	holder := func(a Assignment) []Approver {
		if h, ok := a.static(); ok {
			return []Approver{h}
		}
		if s.router == nil {
			return nil
		}
		h, found, err := s.router.Resolve(ctx, in, a)
		if err != nil {
			s.logger.Warn("bpm SLA scheduler: resolving a wait notify target failed (fail-open)",
				zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
			return nil
		}
		if !found {
			return nil
		}
		return []Approver{h}
	}
	candidates := [][]Approver{holder(*n.SLA.Notify), holder(Assignment{Type: AssignField, Field: OwnerField})}
	if o := inst.Definition.Options; o != nil && o.FallbackAssignee != nil {
		candidates = append(candidates, []Approver{*o.FallbackAssignee})
	}
	for _, hs := range candidates {
		seats := make([]Seat, 0, len(hs))
		for _, h := range hs {
			seats = append(seats, Seat{NodeID: n.ID, AssigneeType: h.Type, AssigneeID: h.ID})
		}
		if len(seats) == 0 {
			continue
		}
		users, err := s.store.ResolveSeatUsers(ctx, inst.TenantID, seats, breachNotifyCap)
		if err != nil {
			s.logger.Warn("bpm SLA scheduler: resolve wait notify users failed (fail-open)",
				zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
		}
		if len(users) > 0 {
			return users
		}
	}
	return nil
}

// recordNoRecipient says on the timeline that a missed wait deadline found nobody to tell — the
// Nexus team sees it there, not only in a log. Best-effort: a store without HistoryAppender (a test
// fake) leaves the log line.
func (s *SLAScheduler) recordNoRecipient(ctx context.Context, inst Instance, node, stepName string, at time.Time) {
	s.logger.Warn("bpm SLA scheduler: a wait deadline was missed and nobody could be told",
		zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.String("node", node))
	ha, ok := s.store.(HistoryAppender)
	if !ok {
		return
	}
	if err := ha.AppendHistory(ctx, inst.TenantID, inst.ID, HistoryEntry{
		Step: inst.CurrentStep, Action: HistoryBreachNoRecipient, Node: node, StepName: stepName,
		Comment: "quá hạn nhưng không tìm được người nhận cảnh báo", Version: inst.Definition.version(), At: at.UTC(),
	}); err != nil {
		s.logger.Warn("bpm SLA scheduler: could not record the missing recipient (fail-open)",
			zap.String("instance_id", inst.ID), zap.String("tenant_id", inst.TenantID), zap.Error(err))
	}
}

// RecordWaiters lists the pending instances of a record — the wait signal's read. Satisfied by
// *SQLRepository; an InstanceStore without it (a unit-test fake) simply never signals.
type RecordWaiters interface {
	ListPendingByRecord(ctx context.Context, tenantID, entityName, recordID string) ([]Instance, error)
}

// signalWaits wakes the open instances of the updated record that stand on a wait node so they look at
// `until` again (specs/043 S12a). Only instances of THIS record, only pending ones, only those on a
// wait node that are not stuck. Fail-open: a read or walk error is logged — the record update is long
// committed, and the next update tries again. A lost signal is not retried by the deadline: the SLA
// scheduler does not re-read the record, so until then the instance may get a false breach bell.
func (e *AsyncEngine) signalWaits(ctx context.Context, evt Event) {
	lister, ok := e.store.(RecordWaiters)
	if !ok || e.runner == nil {
		return
	}
	open, err := lister.ListPendingByRecord(ctx, evt.TenantID, evt.EntityName, evt.RecordID)
	if err != nil {
		e.logger.Warn("bpm: listing the open approvals of an updated record failed — wait nodes not signalled",
			zap.String("tenant_id", evt.TenantID), zap.String("entity_name", evt.EntityName),
			zap.String("record_id", evt.RecordID), zap.Error(err))
		return
	}
	for i := range open {
		in := &open[i]
		if n := in.Definition.NodeAt(in.CurrentStep); n == nil || n.Kind != NodeWait || in.LastError != "" {
			continue
		}
		if err := e.runner.Continue(ctx, in.TenantID, in.ID); err != nil {
			e.logger.Warn("bpm: waking a wait node failed",
				zap.String("tenant_id", in.TenantID), zap.String("instance_id", in.ID), zap.Error(err))
		}
	}
}

// waitUnit is the wall-clock length of one unit (business_hours counts as hours on the fallback).
func waitUnit(unit string) time.Duration {
	switch unit {
	case workflow.TimerUnitMinutes:
		return time.Minute
	case workflow.TimerUnitHours, workflow.TimerUnitBusinessHours:
		return time.Hour
	}
	return 24 * time.Hour
}

// tightenWait is S14b: a record update reached an instance resting on a wait node with
// sla.onChange = tighten_only. The matrix is read again with the record's NEW values, timed from the
// moment the node was entered, and the deadline moves only if the result is EARLIER (a lowered
// priority or a repeat changes nothing). The guarded write bumps seq, so the bpm_sla_due job of the
// new deadline is parked with it (T198). The record field is rewritten only while it still holds
// what this engine wrote (context.sla.writtenDue): a hand-typed value is never overwritten.
func (r *Runner) tightenWait(ctx context.Context, inst *Instance, node *Node, data map[string]any) error {
	s := node.SLA
	if s == nil || s.OnChange != OnChangeTightenOnly || inst.SLADueAt == nil || inst.Escalated {
		return nil
	}
	entered, ok := waitEntered(inst)
	if !ok {
		return nil
	}
	due, basis, err := waitDeadline(ctx, r.deadlines, inst.TenantID, inst.Definition.CalendarID(), node, data, entered)
	if err != nil { // calendar unreadable: keep the deadline we have (never guess a shorter one)
		r.logger.Warn("bpm: business calendar unreadable — wait deadline not recomputed",
			zap.String("tenant_id", inst.TenantID), zap.String("instance_id", inst.ID), zap.Error(err))
		return nil
	}
	if due == nil || !due.Before(*inst.SLADueAt) {
		return nil
	}
	now := r.now().UTC()
	prev := inst.Context.writtenDue()
	rewrite := s.WriteDueTo != "" && prev != nil && sameInstant(data[s.WriteDueTo], *prev)
	tr := InstanceTransition{CurrentStep: inst.CurrentStep, Status: StatusPending, SLADueAt: due}
	if rewrite {
		if tr.ContextJSON, err = inst.Context.withWrittenDue(*due); err != nil {
			return fmt.Errorf("bpm.Runner.tightenWait: %w", err)
		}
	}
	entry := r.fill(inst, node, HistoryEntry{Action: HistoryWaitTightened, Comment: "hạn rút ngắn do dữ liệu hồ sơ đổi"}, now)
	entry.SLABasis = basis
	historyJSON, err := json.Marshal(append(inst.History, entry))
	if err != nil {
		return fmt.Errorf("bpm.Runner.tightenWait: marshal history: %w", err)
	}
	if err := r.store.DecideTaskTx(ctx, inst.TenantID, inst.ID, TaskGuard{ExpectSeq: inst.Seq}, tr, historyJSON); err != nil {
		return r.lost("bpm.Runner.tightenWait", err)
	}
	if rewrite && r.fields != nil {
		if err := r.fields.SetFields(ctx, inst.TenantID, inst.EntityName, inst.RecordID,
			map[string]any{s.WriteDueTo: due.UTC().Format(time.RFC3339)}, workflow.SystemActorID); err != nil {
			recordEffectFailed(ctx, r.store, r.logger, inst, node.ID, node.Name, "ghi hạn xử lý lên trường "+s.WriteDueTo+" không được: "+reason(err), now)
		}
	}
	return nil
}

// waitEntered is when the instance started its present rest: the newest waiting line.
func waitEntered(inst *Instance) (time.Time, bool) {
	for i := len(inst.History) - 1; i >= 0; i-- {
		if inst.History[i].Action == HistoryWaiting {
			return inst.History[i].At, !inst.History[i].At.IsZero()
		}
	}
	return time.Time{}, false
}

// sameInstant reports whether the record value v (RFC3339 string or time.Time, as the store hands
// it back) is the instant t.
func sameInstant(v any, t time.Time) bool {
	switch x := v.(type) {
	case time.Time:
		return x.Equal(t)
	case string:
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if p, err := time.Parse(layout, x); err == nil {
				return p.Equal(t)
			}
		}
	}
	return false
}
