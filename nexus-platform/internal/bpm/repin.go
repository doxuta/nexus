package bpm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// Impact + repin (specs/043 S11, D7 phương án a). Editing or deleting a process never touches a
// hồ sơ already running — each pins the definition it started under (S0/S10) — so the admin who
// saves a new version is told how many still run an older one, and may move them across, on
// purpose and per row, after a dry run. No save is blocked, nothing is cancelled in bulk.

// Reason codes of a refused row — the same strings the API returns.
const (
	RepinNodeMissing      = "APPROVAL_REPIN_NODE_MISSING"
	RepinNodeNotHuman     = "APPROVAL_REPIN_NODE_NOT_HUMAN"
	RepinTargetUnrunnable = "APPROVAL_REPIN_TARGET_UNRUNNABLE"
	// RepinNeedsRouting: the target step finds its holder by a rule at entry (a record field, the
	// submitter — specs/043 S6a) and a hồ sơ already standing on the step is not entering it.
	RepinNeedsRouting = "APPROVAL_REPIN_NEEDS_ROUTING"
	RepinSameVersion  = "APPROVAL_REPIN_SAME_VERSION"
	RepinConflict     = "APPROVAL_CONFLICT"
	RepinNotPending   = "APPROVAL_NOT_PENDING"
	RepinNotFound     = "APPROVAL_INSTANCE_NOT_FOUND"
)

// MaxRepinBatch bounds one repin call: every row is its own transaction, so an unbounded "move
// them all" is a long request. The call reports how many candidates remain; the caller repeats.
const MaxRepinBatch = 200

// ErrRepinBatchTooLarge: an explicit instance list longer than MaxRepinBatch.
var ErrRepinBatchTooLarge = errors.New("approval repin batch too large")

// NodeCount is how many pending instances stand on one step.
type NodeCount struct {
	NodeID string `json:"nodeId"`
	Name   string `json:"name"`
	Count  int    `json:"count"`
}

// Impact answers "what does the active version leave behind": the pending hồ sơ of a process, and
// how many of them run a version other than the active one (NULL = unknown = older).
type Impact struct {
	Version                int         `json:"version"`
	PendingTotal           int         `json:"pendingTotal"`
	PendingOnOldDefinition int         `json:"pendingOnOldDefinition"`
	ByNode                 []NodeCount `json:"byNode"`
}

// ProcessInstanceRow is one hồ sơ of a process in the admin's "Hồ sơ đang chạy" list.
type ProcessInstanceRow struct {
	ID             string     `json:"id"`
	EntityName     string     `json:"entityName"`
	RecordID       string     `json:"recordId"`
	Status         string     `json:"status"`
	ProcessVersion int        `json:"processVersion"` // 0 = unknown (pinned before versions)
	NodeID         string     `json:"nodeId"`
	StepName       string     `json:"stepName"`
	SLADueAt       *time.Time `json:"slaDueAt"`
	Escalated      bool       `json:"escalated"`
	CreatedAt      time.Time  `json:"createdAt"`
	// LastError is why the hồ sơ stands still on an automatic step (specs/043 S7); empty while it
	// runs. Stuck is true when an admin can press "Thử lại": it has an error, or it stands on a
	// machine node nobody is walking (a crash with the job queue off). EffectFailed: some effect
	// after a step — an automatic write, the record write-back — failed at least once (FR-010).
	LastError    string `json:"lastError,omitempty"`
	Stuck        bool   `json:"stuck"`
	EffectFailed bool   `json:"effectFailed"`
}

// RepinInput selects what to move. InstanceIDs empty = every pending hồ sơ of the process that
// does not already run ToVersion (first MaxRepinBatch). ToVersion 0 = the active version.
type RepinInput struct {
	InstanceIDs []string
	ToVersion   int
	DryRun      bool
	By          string
	// After is the keyset cursor of a selection-less call: the NextAfter of the previous one.
	// Rows refused by a call still run the old version, so without it they would crowd the front
	// of every later call and the movable rows behind them would never get a turn.
	After string
}

// RepinResult is the outcome of one row. A dry run reports exactly what an apply would do.
type RepinResult struct {
	InstanceID  string `json:"instanceId"`
	OK          bool   `json:"ok"`
	Reason      string `json:"reason,omitempty"`
	FromVersion int    `json:"fromVersion"`
	ToVersion   int    `json:"toVersion"`
	FromNode    string `json:"fromNode"`
	ToNode      string `json:"toNode"`
}

// RepinOutcome is the answer of a repin call.
type RepinOutcome struct {
	Results []RepinResult `json:"results"`
	// Remaining counts the candidates a selection-less call left for the next one.
	Remaining int `json:"remaining"`
	// NextAfter is the cursor to send as `after` for the next call; empty when nothing is left.
	// Remaining counts only candidates behind this cursor, so a loop on it always converges.
	NextAfter string `json:"nextAfter,omitempty"`
}

// repinPlan is what moving one instance writes.
type repinPlan struct {
	Next     InstanceTransition
	History  []HistoryEntry
	FromNode string
	ToNode   string
}

// planRepin decides where inst lands on target and what is written, or why it cannot ("" reason =
// a plan). Pure: the repository supplies the open seats and persists the plan.
//
//   - The step is found by its STABLE id (seatNodeID), never by index — v2 may have inserted a step
//     in front. An id the target does not have is refused, never mapped onto a neighbour; S10 never
//     re-issues a released id to a different step, so "same id" means "same step".
//   - The seat is rebuilt from the target's step (its approver may differ), except a delegated seat
//     which keeps its delegate: the delegation was a person's decision and the repin is not one to
//     undo it. The deadline, its basis and the escalation stay: moving versions buys no time and
//     does not re-arm an alert already sent.
func planRepin(inst *Instance, open []Seat, target *Definition, by string, now time.Time) (repinPlan, string) {
	from := seatNodeID(inst)
	plan := repinPlan{FromNode: from}
	if target.Graph != nil {
		for _, n := range target.Graph.Nodes {
			if n.ID == from && n.Kind != NodeApproval {
				return plan, RepinNodeNotHuman
			}
		}
	}
	chain := NormalizeSteps(target.Chain())
	if len(chain) == 0 {
		return plan, RepinTargetUnrunnable
	}
	idx := -1
	for i, s := range chain {
		if s.ID == from {
			idx = i
			break
		}
	}
	if idx < 0 {
		return plan, RepinNodeMissing
	}
	step := chain[idx]
	plan.ToNode = step.ID

	basis := inst.CurrentSLABasis()
	var seats []Seat
	for _, s := range trustedSeats(inst, open) {
		if s.DelegatedFrom != "" || s.Kind == SeatKindRework { // a returned record stays with its submitter
			seats = append(seats, s)
		}
	}
	if len(seats) == 0 {
		if step.Assignment != nil && step.Assignment.Dynamic() {
			// The holder of a field / submitter step is looked up when the hồ sơ ENTERS it (FR-017). A
			// repin is not an entry: guessing here would seat the wrong person, so the row stays put.
			return plan, RepinNeedsRouting
		}
		seats = stepSeats(step, inst.SLADueAt, basis)
	}
	for i := range seats {
		seats[i].DueAt, seats[i].DueBasis = inst.SLADueAt, basis
		if inst.Escalated {
			seats[i].BreachNotifiedAt = escalatedAt(inst, now)
		}
	}
	approverType, approverID := projection(seats)

	pin := *target
	if inst.Definition != nil {
		pin.FirstDueBasis = inst.Definition.FirstDueBasis
	}
	fromVersion := inst.Definition.version()
	plan.History = append(append([]HistoryEntry(nil), inst.History...), HistoryEntry{
		Step: idx, Action: HistoryRepinned, Actor: by, Node: step.ID, StepName: step.Name,
		Version: target.version(), FromVersion: fromVersion, At: now,
	})
	plan.Next = InstanceTransition{
		CurrentStep:         idx,
		Status:              StatusPending,
		CurrentApproverType: approverType,
		CurrentApproverID:   approverID,
		SLADueAt:            inst.SLADueAt,
		Escalated:           inst.Escalated,
		Seats: SeatWrite{
			Close: SeatRepinned, Node: from, By: by, Open: seats, SameVisit: true,
		},
		Pin: &pin,
	}
	return plan, ""
}

// ----------------------------------------------------------------------------------------------
// repository
// ----------------------------------------------------------------------------------------------

// ProcessImpact reports how many pending hồ sơ of a tenant-scoped process run a version other than
// its active one, and on which step they stand. ErrProcessNotFound for an unknown / deleted /
// cross-tenant process (the impact of a deleted process is read BEFORE the delete).
func (r *SQLRepository) ProcessImpact(ctx context.Context, tenantID, processID string) (*Impact, error) {
	proc, err := r.GetProcess(ctx, tenantID, processID)
	if err != nil {
		return nil, err
	}
	const count = `SELECT COUNT(*) AS total,
			COALESCE(SUM(process_version IS NULL OR process_version <> ?), 0) AS old
		FROM sys_approval_instances
		WHERE tenant_id = ? AND process_id = ? AND status = 'pending'`
	var c struct {
		Total int `db:"total"`
		Old   int `db:"old"`
	}
	if err := r.db.GetContext(ctx, &c, count, proc.Version, tenantID, processID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ProcessImpact: %w", err)
	}
	out := &Impact{Version: proc.Version, PendingTotal: c.Total, PendingOnOldDefinition: c.Old, ByNode: []NodeCount{}}
	if c.Old == 0 {
		return out, nil
	}
	// ponytail: the per-step breakdown reads the first 2000 older hồ sơ; the totals above are exact.
	rows, err := r.instancesOf(ctx, tenantID, processID, `AND status = 'pending' AND (process_version IS NULL OR process_version <> ?)`, []any{proc.Version}, 2000)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ProcessImpact: %w", err)
	}
	idx := map[string]int{}
	for i := range rows {
		node := seatNodeID(&rows[i])
		_, name := rows[i].Definition.StepRef(rows[i].CurrentStep)
		j, ok := idx[node]
		if !ok {
			j = len(out.ByNode)
			idx[node] = j
			out.ByNode = append(out.ByNode, NodeCount{NodeID: node, Name: name})
		}
		out.ByNode[j].Count++
	}
	return out, nil
}

// ListProcessInstances lists the hồ sơ of a tenant-scoped process, newest first, optionally only
// those running `version` (0 = unknown). status "" = pending. limit is clamped to 1..500.
func (r *SQLRepository) ListProcessInstances(ctx context.Context, tenantID, processID string, version *int, status string, stuck bool, limit int) ([]ProcessInstanceRow, error) {
	if _, err := r.GetProcess(ctx, tenantID, processID); err != nil {
		return nil, err
	}
	if status == "" {
		status = string(StatusPending)
	}
	if limit < 1 || limit > 500 {
		limit = 500
	}
	where, args := `AND status = ?`, []any{status}
	if stuck { // an error is what a hồ sơ stalled on an automatic step carries (specs/043 S7)
		where += ` AND last_error IS NOT NULL`
	}
	if version != nil {
		if *version == 0 {
			where += ` AND process_version IS NULL`
		} else {
			where += ` AND process_version = ?`
			args = append(args, *version)
		}
	}
	insts, err := r.instancesOf(ctx, tenantID, processID, where, args, limit)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListProcessInstances: %w", err)
	}
	out := make([]ProcessInstanceRow, len(insts))
	for i := range insts {
		in := &insts[i]
		_, name := in.Definition.StepRef(in.CurrentStep)
		out[i] = ProcessInstanceRow{
			ID: in.ID, EntityName: in.EntityName, RecordID: in.RecordID, Status: string(in.Status),
			ProcessVersion: in.ProcessVersion, NodeID: seatNodeID(in), StepName: name,
			SLADueAt: in.SLADueAt, Escalated: in.Escalated, CreatedAt: in.CreatedAt,
			LastError: in.LastError, EffectFailed: hasEffectFailed(in.History),
			Stuck: in.Stuck(),
		}
	}
	return out, nil
}

// hasEffectFailed reports whether some effect after a step failed at least once (an effect_failed line).
func hasEffectFailed(history []HistoryEntry) bool {
	for _, h := range history {
		if h.Action == HistoryEffectFailed {
			return true
		}
	}
	return false
}

// instancesOf reads instances of one tenant-scoped process, newest first. extra is a SQL fragment
// of constants (never user text) with its `?` args.
func (r *SQLRepository) instancesOf(ctx context.Context, tenantID, processID, extra string, args []any, limit int) ([]Instance, error) {
	q := `SELECT ` + instanceColumns + `
		FROM sys_approval_instances
		WHERE tenant_id = ? AND process_id = ? ` + extra + `
		ORDER BY created_at DESC, id DESC LIMIT ?`
	all := append([]any{tenantID, processID}, args...)
	all = append(all, limit)
	var rows []instanceRow
	if err := r.db.SelectContext(ctx, &rows, q, all...); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.instancesOf: %w", err)
	}
	return r.decodeInstances("instancesOf", rows), nil
}

// repinTarget builds the definition a repin pins: the version's own steps/graph/outcome, pinned
// exactly like an arm of that version (ids, explicit clocks).
func (r *SQLRepository) repinTarget(ctx context.Context, tenantID, processID string, toVersion int) (*Definition, error) {
	var row processRow
	q := `SELECT ` + processColumns + ` FROM sys_approval_processes WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if err := r.db.GetContext(ctx, &row, q, tenantID, processID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrProcessNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.repinTarget: %w", err)
	}
	doc := definitionOfRow(&row)
	if toVersion != row.Version {
		if toVersion < 1 || toVersion > row.Version {
			return nil, ErrVersionNotFound
		}
		rec, err := r.GetVersion(ctx, tenantID, processID, toVersion)
		if err != nil {
			return nil, err
		}
		doc = DefinitionDoc{}
		if err := json.Unmarshal(rec.Definition, &doc); err != nil {
			return nil, fmt.Errorf("bpm.SQLRepository.repinTarget: decode version %d: %w", toVersion, err)
		}
	}
	pr := processRow{ID: processID, TenantID: tenantID, Version: toVersion, Steps: doc.Steps}
	if len(pr.Steps) == 0 {
		pr.Steps = json.RawMessage("[]")
	}
	if len(doc.Graph) > 0 {
		g := doc.Graph
		pr.Graph = &g
	}
	if len(doc.Outcome) > 0 {
		o := doc.Outcome
		pr.Outcome = &o
	}
	if len(doc.Options) > 0 {
		o := doc.Options
		pr.Options = &o
	}
	p, err := rowToProcess(&pr)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.repinTarget: version %d: %w", toVersion, err)
	}
	def := pinDefinition(p)
	if len(def.Chain()) == 0 {
		return nil, fmt.Errorf("bpm.SQLRepository.repinTarget: version %d has no approval step this engine runs", toVersion)
	}
	return def, nil
}

// Repin moves hồ sơ of a tenant-scoped process onto a version of it. Each row is its own guarded
// transaction (DecideTaskTx — seq CAS, seats, the due job), so a decision made in the same moment
// wins or loses cleanly and is reported per row, never half-applied. DryRun runs the same plan
// and writes nothing. Rows that cannot move are reported with a reason; the call itself fails only
// on an unknown process / version or a store error.
func (r *SQLRepository) Repin(ctx context.Context, tenantID, processID string, in RepinInput) (*RepinOutcome, error) {
	if len(in.InstanceIDs) > MaxRepinBatch {
		return nil, ErrRepinBatchTooLarge
	}
	proc, err := r.GetProcess(ctx, tenantID, processID)
	if err != nil {
		return nil, err
	}
	toVersion := in.ToVersion
	if toVersion == 0 {
		toVersion = proc.Version
	}
	target, err := r.repinTarget(ctx, tenantID, processID, toVersion)
	if err != nil {
		return nil, err
	}

	out := &RepinOutcome{Results: []RepinResult{}}
	ids := dedupe(in.InstanceIDs)
	if len(ids) == 0 {
		const base = `FROM sys_approval_instances
			WHERE tenant_id = ? AND process_id = ? AND status = 'pending' AND (process_version IS NULL OR process_version <> ?)`
		const cursor = ` AND (created_at, id) > (SELECT created_at, id FROM sys_approval_instances WHERE tenant_id = ? AND id = ?)`
		args := []any{tenantID, processID, toVersion}
		where := base
		if in.After != "" {
			where += cursor
			args = append(args, tenantID, in.After)
		}
		if err := r.db.SelectContext(ctx, &ids, `SELECT id `+where+` ORDER BY created_at, id LIMIT ?`, append(args, MaxRepinBatch+1)...); err != nil {
			return nil, fmt.Errorf("bpm.SQLRepository.Repin: select candidates: %w", err)
		}
		if len(ids) > MaxRepinBatch {
			ids = ids[:MaxRepinBatch]
			out.NextAfter = ids[len(ids)-1]
			var n int
			if err := r.db.GetContext(ctx, &n, `SELECT COUNT(*) `+base+` AND (created_at, id) > (SELECT created_at, id FROM sys_approval_instances WHERE tenant_id = ? AND id = ?)`,
				tenantID, processID, toVersion, tenantID, out.NextAfter); err != nil {
				return nil, fmt.Errorf("bpm.SQLRepository.Repin: count remaining: %w", err)
			}
			out.Remaining = n
		}
	}

	now := r.now().UTC()
	for _, id := range ids {
		res, err := r.repinOne(ctx, tenantID, processID, id, target, in, now)
		if err != nil {
			return nil, err
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// repinOne plans and (unless dry-run) applies one row.
func (r *SQLRepository) repinOne(ctx context.Context, tenantID, processID, id string, target *Definition, in RepinInput, now time.Time) (RepinResult, error) {
	res := RepinResult{InstanceID: id, ToVersion: target.version()}
	inst, err := r.GetInstance(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, ErrInstanceNotFound) {
			res.Reason = RepinNotFound
			return res, nil
		}
		return res, fmt.Errorf("bpm.SQLRepository.Repin: %w", err)
	}
	if inst.ProcessID != processID { // another process's hồ sơ is "not found" here, not a leak
		res.Reason = RepinNotFound
		return res, nil
	}
	res.FromVersion = inst.ProcessVersion
	if inst.Status != StatusPending {
		res.Reason = RepinNotPending
		return res, nil
	}
	if inst.ProcessVersion == target.version() {
		res.Reason = RepinSameVersion
		return res, nil
	}
	open, err := r.ListOpenSeats(ctx, tenantID, id)
	if err != nil {
		return res, fmt.Errorf("bpm.SQLRepository.Repin: read seats: %w", err)
	}
	plan, reason := planRepin(inst, open, target, in.By, now)
	res.FromNode, res.ToNode = plan.FromNode, plan.ToNode
	if reason != "" {
		res.Reason = reason
		return res, nil
	}
	if in.DryRun {
		res.OK = true
		return res, nil
	}
	historyJSON, err := json.Marshal(plan.History)
	if err != nil {
		return res, fmt.Errorf("bpm.SQLRepository.Repin: marshal history: %w", err)
	}
	if err := r.DecideTaskTx(ctx, tenantID, id, TaskGuard{ExpectSeq: inst.Seq}, plan.Next, historyJSON); err != nil {
		switch {
		case errors.Is(err, ErrConcurrent):
			res.Reason = RepinConflict
			return res, nil
		case errors.Is(err, ErrNotPending):
			res.Reason = RepinNotPending
			return res, nil
		}
		return res, fmt.Errorf("bpm.SQLRepository.Repin: %w", err)
	}
	r.logger.Info("bpm: approval instance re-pinned",
		zap.String("tenant_id", tenantID), zap.String("process_id", processID), zap.String("instance_id", id),
		zap.Int("from_version", res.FromVersion), zap.Int("to_version", res.ToVersion), zap.String("node", res.ToNode))
	res.OK = true
	return res, nil
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
