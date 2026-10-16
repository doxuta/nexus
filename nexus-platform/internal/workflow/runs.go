package workflow

// W6 — workflow run history (sys_workflow_runs, migration 000032; plan §3.4 "Run history").
// The async engine records EVERY (event × workflow) evaluation — success, failed AND skipped —
// and the scheduler records one run per claimed scheduled firing. Recording rides the EXISTING
// async execution path (worker goroutine / scheduler tick, after the triggering mutation already
// committed), so it adds zero sync latency to record writes; and it is fail-open — a failed
// INSERT into the runs table is logged and the executor continues.
//
// RETENTION (known debt, alongside TOKEN-JANITOR): no janitor in this slice; see the migration
// 000032 header.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// RunStatus is the recorded outcome of one workflow run (sys_workflow_runs.status ENUM).
type RunStatus string

const (
	// RunSuccess: the condition matched and every action ran clean.
	RunSuccess RunStatus = "success"
	// RunFailed: an action (or the scheduled scan) errored; Error carries the first error.
	RunFailed RunStatus = "failed"
	// RunSkipped: the condition evaluated false (event rules) / no record matched (scheduled).
	RunSkipped RunStatus = "skipped"
	// RunBlocked (specs/048): a before_save/before_delete veto refused the save/delete. Needs the
	// before-hooks migration (ENUM widening) applied before any binary writes it.
	RunBlocked RunStatus = "blocked"
	// RunRunning (#338, migration 000087): the row was written BEFORE the actions ran and has no
	// outcome yet. FinishRun turns it into success/failed; the janitor closes one the server died on.
	RunRunning RunStatus = "running"
)

// ActionResult is one action's outcome inside a run's actions_result JSON array.
type ActionResult struct {
	Type  string `json:"type"`
	Field string `json:"field,omitempty"` // field_update target (empty for other types)
	// Via is the relation field a field_update followed to reach the record it wrote (specs/058) —
	// without it a run row reading "status: ok" does not say WHICH record's status changed.
	Via   string `json:"via,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Delivery (#339) is where a QUEUED action's real work is: DeliveryQueued while the job queue
	// still holds it, DeliverySent / DeliveryFailed once it settled (written onto the row by the
	// job handler). Empty for an action that did its work inline.
	Delivery string `json:"delivery,omitempty"`
	// Warning marks an action that DID run, but not the way its config reads. Today that is one
	// case: a business_hours set_deadline that fell back to wall-clock because the tenant had no
	// usable default working calendar (DAILY-QUEUE #82). The run stays `success` — the deadline was
	// stamped — and this is what stops run history calling that a working-hours deadline.
	Warning string `json:"warning,omitempty"`
	// Skipped (specs/048) is why a matched before-rule veto did NOT refuse the write: the write is
	// one of the exempt system writes (approval_outcome, payment_match — FR-019). The run is skipped.
	Skipped string `json:"skipped,omitempty"`
}

// RunRef names the sys_workflow_runs row an action belongs to (specs/059). The run id is minted
// BEFORE the rule's actions run (dispatcher runOne, scheduler fireScheduled) and handed to every
// action through ctx, so work that finishes AFTER the row is written — a queued email delivery —
// can still put its outcome on the row the builder reads. ActionIndex is the action's slot in the
// rule (its entry in actions_result).
type RunRef struct {
	TenantID    string
	WorkflowID  string
	RunID       string
	ActionIndex int
}

type runRefKey struct{}

// WithRunRef returns ctx carrying ref.
func WithRunRef(ctx context.Context, ref RunRef) context.Context {
	return context.WithValue(ctx, runRefKey{}, ref)
}

// RunRefFrom returns the run the current action belongs to; ok=false outside a recorded run (a
// direct call, the dry-run), never a zero-valued ref a consumer could mistake for a real row.
func RunRefFrom(ctx context.Context) (RunRef, bool) {
	ref, ok := ctx.Value(runRefKey{}).(RunRef)
	return ref, ok && ref.RunID != ""
}

// DeliveryNote is what one action tells the engine about WHERE its work went (#339): today only
// "I parked it on the job queue", so the run's slot reads queued instead of looking finished. The
// engine puts a fresh note on each action's ctx (runActions); a sender that enqueues calls MarkQueued.
type DeliveryNote struct{ queued atomic.Bool }

// Queued reports whether the action parked its work on the queue.
func (n *DeliveryNote) Queued() bool { return n != nil && n.queued.Load() }

type deliveryNoteKey struct{}

// WithDeliveryNote returns ctx carrying a fresh note, and the note.
func WithDeliveryNote(ctx context.Context) (context.Context, *DeliveryNote) {
	n := &DeliveryNote{}
	return context.WithValue(ctx, deliveryNoteKey{}, n), n
}

// MarkQueued records on ctx's note that the action's work is now on the job queue. No-op without a
// note (a direct call, the dry run).
func MarkQueued(ctx context.Context) {
	if n, ok := ctx.Value(deliveryNoteKey{}).(*DeliveryNote); ok {
		n.queued.Store(true)
	}
}

type runStartedKey struct{}

// withRunStarted marks ctx: the run's 'running' row was written, so the end-of-run write must FINISH
// it (RunStarter.FinishRun) rather than insert a second row.
func withRunStarted(ctx context.Context) context.Context {
	return context.WithValue(ctx, runStartedKey{}, true)
}

func runStartedFrom(ctx context.Context) bool {
	v, _ := ctx.Value(runStartedKey{}).(bool)
	return v
}

type runWarningKey struct{}

// WithRunWarning returns ctx carrying a note about the WHOLE run that every action result of it
// should show (DAILY-QUEUE #105: a date_offset wake-up parked on the wall-clock fallback — the
// time the run happened at is the thing that is off, not any one action). runActions appends it
// to each ActionResult.Warning, after the action's own note if it has one.
func WithRunWarning(ctx context.Context, note string) context.Context {
	return context.WithValue(ctx, runWarningKey{}, note)
}

// runWarningFrom returns the run-wide note set by WithRunWarning, "" when none.
func runWarningFrom(ctx context.Context) string {
	note, _ := ctx.Value(runWarningKey{}).(string)
	return note
}

// RunInput is the payload the engine records for one run. RecordID is empty for scheduled
// firings (they span many records); ActionsResult is the marshalled []ActionResult (event runs)
// or a {scanned,matched,failed} summary (scheduled runs), nil for skipped event runs.
type RunInput struct {
	// ID is the row id, minted before the actions ran (RunRef.RunID). Empty = mint one on insert.
	ID            string
	TenantID      string
	WorkflowID    string
	TriggerType   string
	RecordID      string // "" → NULL
	Status        RunStatus
	Error         string // "" → NULL
	ActionsResult []byte // JSON or nil
	StartedAt     time.Time
	FinishedAt    time.Time
}

// RunRecorder is the consumer-side sink the dispatcher/scheduler write runs through. Satisfied
// by *SQLRepository (sys_workflow_runs) and by fakes in unit tests. InsertRun must be safe for
// concurrent use (multiple dispatcher workers).
type RunRecorder interface {
	InsertRun(ctx context.Context, in RunInput) error
}

// RunStarter is the optional second half of a RunRecorder (#338): write the run row BEFORE the
// rule's actions run, finish it after. A queued email delivery that gives up on its first attempt
// can then always find the row it must mark — it no longer races the dispatcher's end-of-run write.
// A recorder that does not implement it (unit-test fakes) gets the old single InsertRun at the end.
type RunStarter interface {
	// StartRun inserts the row with status running. in.ActionsResult is the placeholder (one slot
	// per action, or the empty scheduled summary) late outcomes can be written into.
	StartRun(ctx context.Context, in RunInput) error
	// FinishRun writes the outcome onto the row StartRun made, keeping any late delivery outcome
	// (RunRunning rows can be marked failed / sent while the actions are still returning). A row
	// that is missing (the start was lost, fail-open) is inserted whole.
	FinishRun(ctx context.Context, in RunInput) error
}

// RunRecord is the persisted-row view returned by the runs read API (camelCase JSON, forwarded
// verbatim by the handler). ActionsResult is a pointer because skipped runs store NULL (the
// sqlx scan needs the nil case); JSON renders it as null.
type RunRecord struct {
	ID            string           `json:"id" db:"id"`
	TenantID      string           `json:"tenantId" db:"tenant_id"`
	WorkflowID    string           `json:"workflowId" db:"workflow_id"`
	TriggerType   string           `json:"triggerType" db:"trigger_type"`
	RecordID      *string          `json:"recordId" db:"record_id"`
	Status        string           `json:"status" db:"status"`
	Error         *string          `json:"error" db:"error"`
	ActionsResult *json.RawMessage `json:"actionsResult" db:"actions_result"`
	StartedAt     time.Time        `json:"startedAt" db:"started_at"`
	FinishedAt    time.Time        `json:"finishedAt" db:"finished_at"`
}

// InsertRun appends one run row. Empty RecordID/Error store NULL; nil ActionsResult stores NULL.
// tenant_id is always the event's tenant — the engine never writes cross-tenant.
func (r *SQLRepository) InsertRun(ctx context.Context, in RunInput) error {
	var actions any
	if len(in.ActionsResult) > 0 {
		actions = string(in.ActionsResult)
	}
	id := in.ID
	if id == "" {
		id = uuid.NewString()
	}
	const q = `INSERT INTO sys_workflow_runs
		(id, tenant_id, workflow_id, trigger_type, record_id, status, error, actions_result, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		id, in.TenantID, in.WorkflowID, in.TriggerType,
		nullIfEmpty(in.RecordID), string(in.Status), nullIfEmpty(in.Error), actions,
		in.StartedAt.UTC(), in.FinishedAt.UTC(),
	); err != nil {
		return fmt.Errorf("workflow.SQLRepository.InsertRun: %w", err)
	}
	return nil
}

// StartRun implements RunStarter: the row goes in as 'running' with finished_at = started_at.
func (r *SQLRepository) StartRun(ctx context.Context, in RunInput) error {
	in.Status, in.Error, in.FinishedAt = RunRunning, "", in.StartedAt
	if err := r.InsertRun(ctx, in); err != nil {
		return fmt.Errorf("workflow.SQLRepository.StartRun: %w", err)
	}
	return nil
}

// FinishRun implements RunStarter. Under the row lock it merges what the engine learned (in) with
// what late deliveries wrote while the actions were still running:
//   - a row a delivery already marked failed stays failed (the engine's own error, when it has one,
//     is kept as the first error);
//   - an action slot a delivery already settled (delivery "sent"/"failed") keeps that outcome;
//   - a scheduled summary adds the late failed count and keeps lateFailedKeys.
func (r *SQLRepository) FinishRun(ctx context.Context, in RunInput) error {
	// ponytail: one-shot FinishRun; on failure the janitor closes the row as failed after 1h (accepted, rare) — retry once if seen.
	found, err := r.withRun(ctx, in.TenantID, in.ID, "FinishRun", func(row *lockedRun) {
		prevStatus, prevErr := row.status, row.errText
		row.status = string(in.Status)
		row.errText = nil
		if in.Error != "" {
			e := in.Error
			row.errText = &e
		}
		if prevStatus == string(RunFailed) {
			row.status = string(RunFailed)
			if row.errText == nil {
				row.errText = prevErr
			}
		}
		fin := in.FinishedAt
		row.finishedAt = &fin
		prev := &lockedRun{actions: row.actions}
		row.actions = in.ActionsResult
		if oldSlots, ok := prev.slots(); ok {
			if newSlots, ok := row.slots(); ok {
				for i := range newSlots {
					if i < len(oldSlots) && (oldSlots[i]["delivery"] == DeliverySent || oldSlots[i]["delivery"] == DeliveryFailed) {
						newSlots[i]["delivery"] = oldSlots[i]["delivery"]
						if oldSlots[i]["delivery"] == DeliveryFailed {
							newSlots[i]["ok"], newSlots[i]["error"] = false, oldSlots[i]["error"]
						}
					}
				}
				row.setSlots(newSlots)
			}
		} else if oldSum, ok := prev.summary(); ok {
			if newSum, ok := row.summary(); ok {
				late, _ := oldSum["failed"].(float64)
				n, _ := newSum["failed"].(float64)
				newSum["failed"] = int(n + late)
				if keys, ok := oldSum["lateFailedKeys"]; ok {
					newSum["lateFailedKeys"] = keys
				}
				row.setSummary(newSum)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.FinishRun: %w", err)
	}
	if found {
		return nil
	}
	if err := r.InsertRun(ctx, in); err != nil {
		return fmt.Errorf("workflow.SQLRepository.FinishRun: %w", err)
	}
	return nil
}

// MarkRunActionSent (#339) writes a queued delivery that went out onto its run: the action's slot
// turns delivery "sent". The run's status is untouched, and a slot already marked failed stays failed.
// A scheduled summary has no per-action slot, so it is left alone (found is still true). found=false
// means no such run in the tenant.
func (r *SQLRepository) MarkRunActionSent(ctx context.Context, tenantID, runID string, actionIndex int) (bool, error) {
	return r.withRun(ctx, tenantID, runID, "MarkRunActionSent", func(row *lockedRun) {
		slots, ok := row.slots()
		if !ok || actionIndex < 0 || actionIndex >= len(slots) || slots[actionIndex]["delivery"] == DeliveryFailed {
			return
		}
		slots[actionIndex]["delivery"] = DeliverySent
		row.setSlots(slots)
	})
}

// maxLateFailureKeys bounds the per-run list of delivery keys a scheduled summary remembers for
// de-duplication. ponytail: past the cap a repeated report of the same delivery is counted again —
// 200 give-ups on ONE firing means the mail setup is broken, and the first ones already say so.
const maxLateFailureKeys = 200

// MarkRunActionFailed writes a late action failure — a queued email delivery the queue gave up on
// (specs/059) — onto the run it belongs to, under a row lock so two deliveries finishing together
// cannot lose each other's write. status turns failed, error keeps the FIRST error.
//   - Event run (actions_result is the per-action ARRAY): the action's slot gets ok=false + the
//     message; a slot index outside the array is left alone.
//   - Scheduled run (actions_result is the {scanned, matched, failed} summary): failed goes up by one
//     per DISTINCT key (the delivery's job id) — the same delivery reporting twice counts once.
//
// found=false means no such run in the tenant (never written, or another tenant's id).
func (r *SQLRepository) MarkRunActionFailed(ctx context.Context, tenantID, runID string, actionIndex int, key, msg string) (bool, error) {
	return r.withRun(ctx, tenantID, runID, "MarkRunActionFailed", func(row *lockedRun) {
		row.status = string(RunFailed)
		if row.errText == nil {
			row.errText = &msg
		}
		if slots, ok := row.slots(); ok {
			if actionIndex >= 0 && actionIndex < len(slots) {
				slots[actionIndex]["ok"], slots[actionIndex]["error"], slots[actionIndex]["delivery"] = false, msg, DeliveryFailed
				row.setSlots(slots)
			}
			return
		}
		if sum, ok := row.summary(); ok {
			keys, _ := sum["lateFailedKeys"].([]any)
			for _, k := range keys {
				if k == key {
					return // this delivery was already counted
				}
			}
			if len(keys) < maxLateFailureKeys {
				sum["lateFailedKeys"] = append(keys, key)
			}
			n, _ := sum["failed"].(float64)
			sum["failed"] = int(n) + 1
			row.setSummary(sum)
		}
	})
}

// lockedRun is one sys_workflow_runs row read FOR UPDATE inside withRun; mutate it, withRun writes it back.
type lockedRun struct {
	status     string
	errText    *string
	actions    []byte     // nil = SQL NULL
	finishedAt *time.Time // nil = leave finished_at as it is
}

// slots decodes actions_result as the per-action array of an event run.
func (l *lockedRun) slots() ([]map[string]any, bool) {
	var out []map[string]any
	if len(l.actions) == 0 || l.actions[0] != '[' || json.Unmarshal(l.actions, &out) != nil {
		return nil, false
	}
	return out, true
}

func (l *lockedRun) setSlots(s []map[string]any) { l.actions, _ = json.Marshal(s) }

// summary decodes actions_result as the {scanned, matched, failed} object of a scheduled run.
func (l *lockedRun) summary() (map[string]any, bool) {
	var out map[string]any
	if len(l.actions) == 0 || l.actions[0] != '{' || json.Unmarshal(l.actions, &out) != nil {
		return nil, false
	}
	return out, true
}

func (l *lockedRun) setSummary(m map[string]any) { l.actions, _ = json.Marshal(m) }

// withRun reads the tenant's run row FOR UPDATE, lets mutate change it, and writes it back — one
// transaction, tenant_id pinned. found=false (nothing written) when the row does not exist.
func (r *SQLRepository) withRun(ctx context.Context, tenantID, runID, op string, mutate func(*lockedRun)) (bool, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("workflow.SQLRepository.%s: begin: %w", op, err)
	}
	defer func() { _ = tx.Rollback() }()
	var row lockedRun
	var actions sql.NullString
	err = tx.QueryRowxContext(ctx,
		`SELECT status, error, CAST(actions_result AS CHAR) FROM sys_workflow_runs WHERE tenant_id = ? AND id = ? FOR UPDATE`,
		tenantID, runID).Scan(&row.status, &row.errText, &actions)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workflow.SQLRepository.%s: read: %w", op, err)
	}
	if actions.Valid {
		row.actions = []byte(actions.String)
	}
	mutate(&row)
	var finishedAt any
	if row.finishedAt != nil {
		finishedAt = row.finishedAt.UTC()
	}
	var out any
	if row.actions != nil {
		out = string(row.actions)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sys_workflow_runs SET status = ?, error = ?, actions_result = ?, finished_at = COALESCE(?, finished_at)
		 WHERE tenant_id = ? AND id = ?`,
		row.status, row.errText, out, finishedAt, tenantID, runID); err != nil {
		return false, fmt.Errorf("workflow.SQLRepository.%s: write: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("workflow.SQLRepository.%s: commit: %w", op, err)
	}
	return true, nil
}

// ListRuns returns one workflow's runs newest-first (limit/offset paged) plus the total count
// for the pagination envelope. tenant_id + workflow_id pin the query (idx_wfr_tenant_wf_started
// serves both the page scan and the count).
func (r *SQLRepository) ListRuns(ctx context.Context, tenantID, workflowID string, limit, offset int) ([]RunRecord, int, error) {
	const qCount = `SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ?`
	var total int
	if err := r.db.GetContext(ctx, &total, qCount, tenantID, workflowID); err != nil {
		return nil, 0, fmt.Errorf("workflow.SQLRepository.ListRuns: count: %w", err)
	}
	const q = `SELECT id, tenant_id, workflow_id, trigger_type, record_id, status, error, actions_result, started_at, finished_at
		FROM sys_workflow_runs
		WHERE tenant_id = ? AND workflow_id = ?
		ORDER BY started_at DESC, id DESC
		LIMIT ? OFFSET ?`
	var rows []RunRecord
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, workflowID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("workflow.SQLRepository.ListRuns: %w", err)
	}
	return rows, total, nil
}

// ListRecentRuns returns the tenant's most recent runs (admin overview strip). Newest-first,
// capped by limit; no COUNT — this is the cheap "recent activity" read. Empty workflowIDs = every
// rule (idx_wfr_tenant_started serves the scan); otherwise only those rules' runs — the
// automation panel of ONE app lists its rules and must not show another app's runs (queue #297).
// tenant_id pins both shapes, so an id of another tenant's rule matches nothing.
//
// ponytail: the IN shape is not O(limit). EXPLAIN on a copy of the dev DB (northwind, 359 runs, 3 ids):
// MySQL walks idx_wfr_tenant_started backward and filters workflow_id, so it stops after `limit`
// matches when the app's rules run often but reads the tenant's whole retention window (90-day
// janitor) when they rarely or never ran; the other plan (idx_wfr_tenant_wf_started ranges + a
// LIMIT-bounded sort) reads every run of the listed rules. Fine at panel scale; upgrade path if it
// shows up slow: UNION ALL of one `ORDER BY started_at DESC LIMIT ?` per rule (each a backward
// scan of idx_wfr_tenant_wf_started) under an outer ORDER BY … LIMIT.
func (r *SQLRepository) ListRecentRuns(ctx context.Context, tenantID string, limit int, workflowIDs []string) ([]RunRecord, error) {
	const cols = `SELECT id, tenant_id, workflow_id, trigger_type, record_id, status, error, actions_result, started_at, finished_at
		FROM sys_workflow_runs`
	const qAll = cols + `
		WHERE tenant_id = ?
		ORDER BY started_at DESC, id DESC
		LIMIT ?`
	const qOf = cols + `
		WHERE tenant_id = ? AND workflow_id IN (?)
		ORDER BY started_at DESC, id DESC
		LIMIT ?`
	q, args := qAll, []any{tenantID, limit}
	if len(workflowIDs) > 0 {
		expanded, inArgs, err := sqlx.In(qOf, tenantID, workflowIDs, limit)
		if err != nil {
			return nil, fmt.Errorf("workflow.SQLRepository.ListRecentRuns: expand: %w", err)
		}
		q, args = r.db.Rebind(expanded), inArgs
	}
	var rows []RunRecord
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("workflow.SQLRepository.ListRecentRuns: %w", err)
	}
	return rows, nil
}

// Delivery states of a queued action slot (ActionResult.Delivery) — #339.
const (
	// DeliveryQueued: the action parked its work on the job queue; the outcome is not known yet.
	DeliveryQueued = "queued"
	// DeliverySent: the queue delivered it.
	DeliverySent = "sent"
	// DeliveryFailed: the queue gave up (the slot also carries ok=false + the masked reason).
	DeliveryFailed = "failed"
)

// placeholderRunActions is the actions_result a 'running' row starts with: one slot per action
// (type only — no outcome yet) so a delivery finishing before the actions return has a slot to write.
func placeholderRunActions(actions []Action) []byte {
	slots := make([]map[string]any, len(actions))
	for i, a := range actions {
		slots[i] = map[string]any{"type": a.Type}
	}
	b, err := json.Marshal(slots)
	if err != nil {
		return nil // unreachable for strings; the row is still written without slots
	}
	return b
}

// compile-time check: the SQL repository satisfies the recorder contracts.
var (
	_ RunRecorder = (*SQLRepository)(nil)
	_ RunStarter  = (*SQLRepository)(nil)
)
