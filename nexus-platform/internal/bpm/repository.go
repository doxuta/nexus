package bpm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"

	"nexus/platform/internal/workflow"
)

// SQLRepository persists and reads sys_approval_processes + sys_approval_instances over sqlx with
// parameterized queries (mirrors the workflow/audit repository style: static identifiers, values
// always travel as `?`, tenant_id pins every statement — multi-tenant invariant). It serves the
// engine hot path (ListActiveProcesses, CreateInstance), the admin process CRUD, and the approval
// decision + SLA scan paths.
type SQLRepository struct {
	db     *sqlx.DB
	now    func() time.Time // injectable clock; defaults to time.Now
	logger *zap.Logger      // reports rows the list reads skip (decodeInstances); never nil
	// dueJobs parks the durable breach wake-up of a step (specs/043 S3c); nil = queue off.
	dueJobs DueJobQueue
}

// NewSQLRepository wires the BPM repository over an existing sqlx connection. logger may be nil
// (→ no-op); it is a constructor argument rather than an optional With-setter so every call site
// has to decide, instead of the production wiring silently keeping a no-op logger.
func NewSQLRepository(db *sqlx.DB, logger *zap.Logger) *SQLRepository {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SQLRepository{db: db, now: time.Now, logger: logger}
}

// ErrProcessNotFound is returned by process Get/Update/Delete for an unknown / cross-tenant id.
var ErrProcessNotFound = fmt.Errorf("approval process not found")

// ErrInstanceNotFound is returned by instance Get / decide for an unknown / cross-tenant id.
var ErrInstanceNotFound = fmt.Errorf("approval instance not found")

// ----------------------------------------------------------------------------------------------
// sys_approval_processes — engine hot path (ListActiveProcesses) + admin CRUD.
// ----------------------------------------------------------------------------------------------

// processRow is the persisted sys_approval_processes row. Criteria/Steps stay raw JSON so the repo
// decodes them once into the domain Process (or forwards them verbatim to the admin read view).
type processRow struct {
	ID         string           `db:"id"`
	TenantID   string           `db:"tenant_id"`
	Name       string           `db:"name"`
	EntityName string           `db:"entity_name"`
	Trigger    string           `db:"trigger_event"`
	Criteria   *json.RawMessage `db:"criteria"`
	Steps      json.RawMessage  `db:"steps"`
	Graph      *json.RawMessage `db:"graph"` // graph form (specs/043 S3a2); NULL = linear
	Outcome    *json.RawMessage `db:"outcome"`
	Options    *json.RawMessage `db:"options"` // process options (specs/043 S6a); NULL = none
	IsActive   bool             `db:"is_active"`
	// Version / Draft* are specs/043 S10 (Migration C): the active version number and the
	// unpublished edit. The engine's hot read (ListActiveProcesses) never loads the draft.
	Version        int              `db:"version"`
	Draft          *json.RawMessage `db:"draft"`
	DraftUpdatedBy *string          `db:"draft_updated_by"`
	DraftUpdatedAt *time.Time       `db:"draft_updated_at"`
	CreatedBy      *string          `db:"created_by"`
	UpdatedBy      *string          `db:"updated_by"`
	CreatedAt      time.Time        `db:"created_at"`
	UpdatedAt      time.Time        `db:"updated_at"`
}

// processColumns is the admin read's column list (every field of processRow).
const processColumns = `id, tenant_id, name, entity_name, trigger_event, criteria, steps, graph, outcome, options,
		is_active, version, draft, draft_updated_by, draft_updated_at, created_by, updated_by, created_at, updated_at`

// ListActiveProcesses returns the ACTIVE, non-deleted approval processes of (tenant, entity,
// trigger) — the hot path the engine runs after every matching record mutation. Ordered by
// created_at so processes arm in a stable order. tenant_id pins the WHERE clause unconditionally;
// the composite index idx_ap_lookup makes this an index range scan.
func (r *SQLRepository) ListActiveProcesses(ctx context.Context, tenantID, entityName string, trigger Trigger) ([]Process, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, criteria, steps, graph, outcome, options,
		is_active, version, created_by, updated_by, created_at, updated_at
		FROM sys_approval_processes
		WHERE tenant_id = ? AND entity_name = ? AND trigger_event = ? AND is_active = 1 AND deleted_at IS NULL
		ORDER BY created_at ASC, id ASC`
	var rows []processRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, entityName, string(trigger)); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListActiveProcesses: %w", err)
	}
	out := make([]Process, 0, len(rows))
	for i := range rows {
		p, err := rowToProcess(&rows[i])
		if err != nil {
			// A single corrupt row must not blind the engine to the rest — skip it, but loudly:
			// a process that never arms again is invisible from every screen (DAILY-QUEUE #68).
			r.logger.Error("bpm: approval process row undecodable — skipped, it will not arm",
				zap.String("op", "ListActiveProcesses"), zap.String("tenant_id", rows[i].TenantID),
				zap.String("process_id", rows[i].ID), zap.String("entity_name", rows[i].EntityName),
				zap.String("trigger", rows[i].Trigger), zap.Error(err))
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// rowToProcess decodes the JSON columns of a persisted row into the domain Process.
func rowToProcess(row *processRow) (Process, error) {
	p := Process{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Name:       row.Name,
		EntityName: row.EntityName,
		Trigger:    Trigger(row.Trigger),
		IsActive:   row.IsActive,
		Version:    row.Version,
	}
	if row.Criteria != nil && len(*row.Criteria) > 0 && string(*row.Criteria) != "null" {
		var c workflow.Condition
		if err := json.Unmarshal(*row.Criteria, &c); err != nil {
			return Process{}, fmt.Errorf("decode criteria: %w", err)
		}
		p.Criteria = &c
	}
	if len(row.Steps) > 0 && string(row.Steps) != "null" {
		if err := json.Unmarshal(row.Steps, &p.Steps); err != nil {
			return Process{}, fmt.Errorf("decode steps: %w", err)
		}
		p.Steps = NormalizeSteps(p.Steps) // a legacy row reads as s1..sN — never rewritten here
	}
	if row.Graph != nil && len(*row.Graph) > 0 && string(*row.Graph) != "null" {
		var g Graph
		if err := json.Unmarshal(*row.Graph, &g); err != nil {
			return Process{}, fmt.Errorf("decode graph: %w", err)
		}
		p.Graph = g.normalized()
	}
	if row.Outcome != nil && len(*row.Outcome) > 0 && string(*row.Outcome) != "null" {
		var o Outcome
		if err := json.Unmarshal(*row.Outcome, &o); err != nil {
			return Process{}, fmt.Errorf("decode outcome: %w", err)
		}
		p.Outcome = &o
	}
	if row.Options != nil && len(*row.Options) > 0 && string(*row.Options) != "null" {
		var o Options
		if err := json.Unmarshal(*row.Options, &o); err != nil {
			return Process{}, fmt.Errorf("decode options: %w", err)
		}
		p.Options = &o
	}
	return p, nil
}

// ProcessRecord is the persisted-row view returned by the admin read API (raw JSON forwarded).
type ProcessRecord struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenantId"`
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Trigger    string          `json:"trigger"`
	Criteria   json.RawMessage `json:"criteria"`
	Steps      json.RawMessage `json:"steps"`
	// Graph is the graph form (specs/043 S3a2) — absent for a linear process; beside it steps is [].
	Graph   json.RawMessage `json:"graph,omitempty"`
	Outcome json.RawMessage `json:"outcome,omitempty"`
	// Options are the process-level settings (specs/043 S6a) — absent when none.
	Options  json.RawMessage `json:"options,omitempty"`
	IsActive bool            `json:"isActive"`
	// Version is the active (published) version number; Draft the unpublished edit when there is
	// one (specs/043 S10). The columns above are always the ACTIVE version — a draft never leaks
	// into them, so the engine, the pack export and the Studio list all read what runs.
	Version        int             `json:"version"`
	Draft          json.RawMessage `json:"draft,omitempty"`
	DraftUpdatedBy *string         `json:"draftUpdatedBy,omitempty"`
	DraftUpdatedAt *time.Time      `json:"draftUpdatedAt,omitempty"`
	CreatedBy      *string         `json:"createdBy"`
	UpdatedBy      *string         `json:"updatedBy"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

// CreateProcessInput is the validated payload to insert a new approval process. Criteria may be nil
// (match-all); Steps is the already-validated, JSON-marshalled step array.
type CreateProcessInput struct {
	TenantID   string
	Name       string
	EntityName string
	Trigger    Trigger
	Criteria   []byte // JSON or nil
	Steps      []byte // JSON (non-nil) — '[]' beside a graph
	Graph      []byte // JSON or nil — the graph form (specs/043 S3a2)
	Outcome    []byte // JSON or nil — the terminal write-back (APPROVAL-OUTCOME)
	Options    []byte // JSON or nil — process options (specs/043 S6a)
	IsActive   bool
	CreatedBy  string
}

// CreateProcess inserts an approval process as version 1 and returns its generated id. The process
// row and its version-1 row are written in ONE transaction (specs/043 S10): a process without a
// version row would have no history to publish over.
func (r *SQLRepository) CreateProcess(ctx context.Context, in CreateProcessInput) (string, error) {
	id := uuid.NewString()
	var crit any
	if len(in.Criteria) > 0 {
		crit = string(in.Criteria)
	}
	createdBy := nullIfEmpty(in.CreatedBy)
	now := r.now().UTC()
	doc, err := marshalDefinition(DefinitionDoc{
		Name: in.Name, EntityName: in.EntityName, Trigger: string(in.Trigger),
		Criteria: in.Criteria, Steps: in.Steps, Graph: in.Graph, Outcome: in.Outcome, Options: in.Options,
	})
	if err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateProcess: %w", err)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateProcess: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	const q = `INSERT INTO sys_approval_processes
		(id, tenant_id, name, entity_name, trigger_event, criteria, steps, graph, outcome, options, is_active, version, created_by, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q,
		id, in.TenantID, in.Name, in.EntityName, string(in.Trigger), crit, string(in.Steps), nullJSON(in.Graph),
		nullJSON(in.Outcome), nullJSON(in.Options), in.IsActive, createdBy, createdBy, now, now,
	); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateProcess: %w", err)
	}
	if err := insertVersion(ctx, tx, in.TenantID, id, 1, doc, "", in.CreatedBy, now); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateProcess: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateProcess: commit: %w", err)
	}
	return id, nil
}

// GetProcess returns one tenant-scoped, non-deleted process, or ErrProcessNotFound.
func (r *SQLRepository) GetProcess(ctx context.Context, tenantID, id string) (*ProcessRecord, error) {
	const q = `SELECT ` + processColumns + `
		FROM sys_approval_processes
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var row processRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrProcessNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.GetProcess: %w", err)
	}
	rec := processRowToRecord(&row)
	return &rec, nil
}

// ListProcesses returns every tenant-scoped, non-deleted process newest-first (admin list page).
func (r *SQLRepository) ListProcesses(ctx context.Context, tenantID string) ([]ProcessRecord, error) {
	const q = `SELECT ` + processColumns + `
		FROM sys_approval_processes
		WHERE tenant_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`
	var rows []processRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListProcesses: %w", err)
	}
	out := make([]ProcessRecord, len(rows))
	for i := range rows {
		out[i] = processRowToRecord(&rows[i])
	}
	return out, nil
}

// UpdateProcessInput carries the validated full-replace update of a process's mutable fields.
type UpdateProcessInput struct {
	Name       string
	EntityName string
	Trigger    Trigger
	Criteria   []byte // JSON or nil
	Steps      []byte // JSON (non-nil) — '[]' beside a graph
	Graph      []byte // JSON or nil — nil writes the linear form (graph NULL)
	Outcome    []byte // JSON or nil — the terminal write-back (APPROVAL-OUTCOME)
	Options    []byte // JSON or nil — process options (specs/043 S6a)
	IsActive   bool
	UpdatedBy  string
	// Force lets a linear-only body (Graph nil) replace a stored graph. Without it that write is
	// refused with ErrProcessGraphOverwrite (specs/043 FR-004).
	Force bool
}

// ErrProcessGraphOverwrite: a linear-only update would erase the graph form stored on the process
// (a Studio/client that does not know graphs saving what it loaded — steps '[]' or a re-typed
// chain). The service answers 409 APPROVAL_PROCESS_V2_OVERWRITE; ?force=true opts in.
//
// Since S6a the same guard covers `options` (the fallback assignee): a body that omits them replaces
// a process that has them only with ?force=true — a client that predates the key (the old Studio
// builder) must not wipe a fallback it never showed. The sentinel keeps its name; the message says both.
var ErrProcessGraphOverwrite = fmt.Errorf("approval process update would erase its graph or options")

// erasesV2 reports whether replacing the stored row by a body carrying graph / options would erase
// configuration only a v2 client knows (the graph form, the process options).
func erasesV2(row *processRow, graph, options []byte) bool {
	hasGraph := row.Graph != nil && len(*row.Graph) > 0 && string(*row.Graph) != "null"
	hasOptions := row.Options != nil && len(*row.Options) > 0 && string(*row.Options) != "null"
	return (hasGraph && len(graph) == 0) || (hasOptions && len(options) == 0)
}

// UpdateProcess is the LEGACY save — "lưu + xuất bản ngay" (specs/043 S10, FR-043): the admin
// builder, the AI Builder and the pack install write the active definition directly. It applies the
// supplied fields to a tenant-scoped, non-deleted process and, when the DEFINITION (name, entity,
// trigger, criteria, steps, graph, outcome — not is_active) differs from the active one, publishes
// it as the next version in the same transaction. A save that changes nothing, or only is_active,
// publishes nothing: version numbers count definitions, not clicks.
//
// Returns ErrProcessNotFound when no live row matches, ErrProcessGraphOverwrite when in.Graph is
// nil, the row stores a graph and in.Force is off (specs/043 FR-004). The row is read FOR UPDATE, so
// the graph guard holds against a concurrent save that just stored a graph, and two concurrent
// saves take distinct version numbers. A pending draft is left alone: it is a separate edit with
// its own publish — and Publish refuses it as stale when this save moved the version under it.
func (r *SQLRepository) UpdateProcess(ctx context.Context, tenantID, id string, in UpdateProcessInput) error {
	var crit any
	if len(in.Criteria) > 0 {
		crit = string(in.Criteria)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.UpdateProcess: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	row, err := lockProcess(ctx, tx, tenantID, id)
	if err != nil {
		return err
	}
	if erasesV2(row, in.Graph, in.Options) && !in.Force {
		return ErrProcessGraphOverwrite
	}
	next := DefinitionDoc{
		Name: in.Name, EntityName: in.EntityName, Trigger: string(in.Trigger),
		Criteria: in.Criteria, Steps: in.Steps, Graph: in.Graph, Outcome: in.Outcome, Options: in.Options,
	}
	changed := !sameDefinition(definitionOfRow(row), next)
	version := row.Version
	now := r.now().UTC()
	if changed {
		if err := ensureCurrentVersion(ctx, tx, row, now); err != nil {
			return fmt.Errorf("bpm.SQLRepository.UpdateProcess: %w", err)
		}
		version++
		doc, err := marshalDefinition(next)
		if err != nil {
			return fmt.Errorf("bpm.SQLRepository.UpdateProcess: %w", err)
		}
		if err := insertVersion(ctx, tx, tenantID, id, version, doc, "", in.UpdatedBy, now); err != nil {
			return fmt.Errorf("bpm.SQLRepository.UpdateProcess: %w", err)
		}
	}
	const q = `UPDATE sys_approval_processes
		SET name = ?, entity_name = ?, trigger_event = ?, criteria = ?, steps = ?, graph = ?, outcome = ?, options = ?, is_active = ?, version = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, q,
		in.Name, in.EntityName, string(in.Trigger), crit, string(in.Steps), nullJSON(in.Graph), nullJSON(in.Outcome), nullJSON(in.Options), in.IsActive, version, nullIfEmpty(in.UpdatedBy),
		tenantID, id,
	); err != nil {
		return fmt.Errorf("bpm.SQLRepository.UpdateProcess: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.UpdateProcess: commit: %w", err)
	}
	return nil
}

// DeleteProcess soft-deletes a tenant-scoped process (stamps deleted_at). Returns
// ErrProcessNotFound when no live row matched.
func (r *SQLRepository) DeleteProcess(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_approval_processes SET deleted_at = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, r.now().UTC(), tenantID, id)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.DeleteProcess: %w", err)
	}
	return rowsAffectedOrNotFound(res, ErrProcessNotFound, "bpm.SQLRepository.DeleteProcess")
}

// processRowToRecord projects a persisted row to the admin read view (raw JSON forwarded verbatim).
func processRowToRecord(row *processRow) ProcessRecord {
	crit := json.RawMessage("null")
	if row.Criteria != nil && len(*row.Criteria) > 0 {
		crit = *row.Criteria
	}
	steps := row.Steps
	if len(steps) == 0 {
		steps = json.RawMessage("null")
	}
	// Legacy rows (saved before step ids existed) answer with their positional ids, on the list
	// AND the single read: the Studio builder loads from the list and round-trips what it got,
	// so an id missing there would be re-assigned by position on the next save — deleting step
	// 1 would silently rename the surviving step from s2 to s1. Undecodable JSON is forwarded
	// verbatim: one bad row must not fail the whole admin read.
	var decoded []Step
	if json.Unmarshal(steps, &decoded) == nil && len(decoded) > 0 {
		if b, err := json.Marshal(NormalizeSteps(decoded)); err == nil {
			steps = b
		}
	}
	var outcome json.RawMessage
	if row.Outcome != nil && len(*row.Outcome) > 0 && string(*row.Outcome) != "null" {
		outcome = *row.Outcome
	}
	var graph json.RawMessage
	if row.Graph != nil && len(*row.Graph) > 0 && string(*row.Graph) != "null" {
		graph = *row.Graph
	}
	var options json.RawMessage
	if row.Options != nil && len(*row.Options) > 0 && string(*row.Options) != "null" {
		options = *row.Options
	}
	var draft json.RawMessage
	if row.Draft != nil && len(*row.Draft) > 0 && string(*row.Draft) != "null" {
		draft = *row.Draft
	}
	rec := ProcessRecord{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Name:       row.Name,
		EntityName: row.EntityName,
		Trigger:    row.Trigger,
		Criteria:   crit,
		Steps:      steps,
		Graph:      graph,
		Outcome:    outcome,
		Options:    options,
		IsActive:   row.IsActive,
		Version:    row.Version,
		CreatedBy:  row.CreatedBy,
		UpdatedBy:  row.UpdatedBy,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
	if draft != nil {
		rec.Draft, rec.DraftUpdatedBy, rec.DraftUpdatedAt = draft, row.DraftUpdatedBy, row.DraftUpdatedAt
	}
	return rec
}

// ----------------------------------------------------------------------------------------------
// sys_approval_instances — engine arming + the "pending for me" read + the decision write.
// ----------------------------------------------------------------------------------------------

// instanceRow is the persisted sys_approval_instances row. History stays raw JSON.
type instanceRow struct {
	ID                  string           `db:"id"`
	TenantID            string           `db:"tenant_id"`
	ProcessID           string           `db:"process_id"`
	ProcessSnapshot     *json.RawMessage `db:"process_snapshot"` // pinned Definition; NULL pre-000065
	EntityName          string           `db:"entity_name"`
	RecordID            string           `db:"record_id"`
	CurrentStep         int              `db:"current_step"`
	Status              string           `db:"status"`
	CurrentApproverType *string          `db:"current_approver_type"`
	CurrentApproverID   *string          `db:"current_approver_id"`
	SLADueAt            *time.Time       `db:"sla_due_at"`
	SLAPausedAt         *time.Time       `db:"sla_paused_at"`
	SLARemaining        *int64           `db:"sla_remaining_seconds"`
	Escalated           bool             `db:"escalated"`
	Seq                 int64            `db:"seq"`        // CAS key (specs/043 D-4); 0 on a row a pre-S3a binary armed
	LastError           *string          `db:"last_error"` // why the step has no holder (S6a); NULL = not stuck
	History             json.RawMessage  `db:"history"`
	Context             *json.RawMessage `db:"context"` // run-time variables: the branches taken (S7)
	CreatedBy           *string          `db:"created_by"`
	CreatedAt           time.Time        `db:"created_at"`
	UpdatedAt           time.Time        `db:"updated_at"`
}

// instanceColumns is the column list of a full instance read (every field of instanceRow).
const instanceColumns = `id, tenant_id, process_id, process_snapshot, entity_name, record_id, current_step, status,
		current_approver_type, current_approver_id, sla_due_at, sla_paused_at, sla_remaining_seconds, escalated, seq, history, last_error, context, created_by, created_at, updated_at`

// CreateInstanceInput is the validated payload to arm a new approval instance on its FIRST step.
type CreateInstanceInput struct {
	TenantID            string
	ProcessID           string
	EntityName          string
	RecordID            string
	CurrentApproverType ApproverType
	CurrentApproverID   string
	SLADueAt            *time.Time // nil → no SLA on the first step
	// Definition pins the process's steps + outcome onto the instance (process_snapshot).
	// Required (≥1 step): NULL in the column means ONE thing — armed before migration 000065 —
	// so a caller that omits it is refused rather than silently re-opening the step-skip bug.
	Definition *Definition
	// Seats are the seats of the first step, written in the same transaction (specs/043 S3a).
	// The current_approver_* projection above is derived from them by the caller (D-5).
	Seats []Seat
	// LastError is set when nobody holds the first step (S6a, FR-018): the instance is created
	// pending with no seat and this says why. "" = not stuck.
	LastError string
	CreatedBy string
	// StartStep is the chain index the instance begins on (specs/043 S7): the graph's start node.
	// 0 for a linear definition. A start on a machine node (a gateway first) carries no seats and
	// parks a bpm_continue job — the walk goes on from there (Runner).
	StartStep int
	// IdempotencyKey/RequestHash (S9b) are written when a send carried an Idempotency-Key.
	IdempotencyKey string
	RequestHash    string
	// Lane is LaneExclusive (default when empty) or LaneIndependent (S12b): the tracking lane never
	// blocks an approval — uq_ai_open keys it by process instead of by record.
	Lane string
}

// mysqlDuplicateEntry is MySQL's ER_DUP_ENTRY.
const mysqlDuplicateEntry = 1062

// HasOpenExclusive reports whether a PENDING approval (lane 'exclusive') already exists for
// (tenant, entity, record) — the engine's early idempotency answer so a record under approval is
// not double-armed (uq_ai_open is the guard that holds under a race). Uses the idx_ai_record index.
func (r *SQLRepository) HasOpenExclusive(ctx context.Context, tenantID, entityName, recordID string) (bool, error) {
	const q = `SELECT 1 FROM sys_approval_instances
		WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND status = 'pending' AND lane = 'exclusive' LIMIT 1`
	var one int
	err := r.db.GetContext(ctx, &one, q, tenantID, entityName, recordID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("bpm.SQLRepository.HasOpenExclusive: %w", err)
	}
	return true, nil
}

// CreateInstance arms a new pending approval instance on its first step and returns its id. The
// history starts as an empty JSON array (decisions are appended on approve/reject/escalate). The
// process snapshot (steps + outcome) is written once here and never updated: an instance finishes
// the process it started under.
func (r *SQLRepository) CreateInstance(ctx context.Context, in CreateInstanceInput) (string, error) {
	if len(in.Definition.Chain()) == 0 {
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: definition with ≥1 approval step this engine runs is required (NULL process_snapshot is reserved for pre-000065 rows)")
	}
	id := uuid.NewString()
	now := r.now().UTC()
	snapshot, err := json.Marshal(in.Definition)
	if err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: marshal snapshot: %w", err)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	// seq starts at 1: the insert IS the instance's first state write (entering its first step).
	// 0 is left to rows a pre-S3a binary wrote, which never counted.
	const q = `INSERT INTO sys_approval_instances
		(id, tenant_id, process_id, process_snapshot, process_version, entity_name, record_id, current_step, status, seq,
		 current_approver_type, current_approver_id, sla_due_at, escalated, history, last_error, created_by, created_at, updated_at,
		 idempotency_key, request_hash, lane)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', 1, ?, ?, ?, 0, '[]', ?, ?, ?, ?, ?, ?, ?)`
	var processVersion any // NULL = no version known (a process this binary never versioned)
	if v := in.Definition.version(); v > 0 {
		processVersion = v
	}
	if _, err := tx.ExecContext(ctx, q,
		id, in.TenantID, in.ProcessID, string(snapshot), processVersion, in.EntityName, in.RecordID, in.StartStep,
		nullIfEmpty(string(in.CurrentApproverType)), nullIfEmpty(in.CurrentApproverID), in.SLADueAt,
		nullIfEmpty(truncateRunes(in.LastError, maxLastErrorRunes)), nullIfEmpty(in.CreatedBy), now, now,
		nullIfEmpty(in.IdempotencyKey), nullIfEmpty(in.RequestHash), laneOrDefault(in.Lane),
	); err != nil {
		// uq_ai_open: another start (a racing event, the bpm_arm job, a double-click) won the
		// record — "already waiting", not a failure. A different key (S9b's uq_ai_idem) is not this.
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry && strings.Contains(me.Message, "uq_ai_open") {
			return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: %w", ErrAlreadyPending)
		}
		if errors.As(err, &me) && me.Number == mysqlDuplicateEntry && strings.Contains(me.Message, "uq_ai_idem") {
			return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: %w", ErrIdempotencyKeyConflict)
		}
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: %w", err)
	}
	if err := writeSeats(ctx, tx, in.TenantID, id, SeatWrite{By: in.CreatedBy, Open: in.Seats}, now); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: %w", err)
	}
	if err := r.enqueueSLADue(ctx, tx, in.TenantID, id, 1, in.SLADueAt, false); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: %w", err)
	}
	if n := in.Definition.NodeAt(in.StartStep); n != nil && MachineKind(n.Kind) {
		if err := r.enqueueContinue(ctx, tx, in.TenantID, id, 1); err != nil {
			return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.CreateInstance: commit: %w", err)
	}
	return id, nil
}

// FindByIdempotencyKey returns the instance a send with this key created, with the hash of that
// request, or ErrInstanceNotFound. Tenant-pinned: uq_ai_idem is (tenant_id, idempotency_key).
func (r *SQLRepository) FindByIdempotencyKey(ctx context.Context, tenantID, key string) (*Instance, string, error) {
	const q = `SELECT id, COALESCE(request_hash, '') AS request_hash FROM sys_approval_instances
		WHERE tenant_id = ? AND idempotency_key = ?`
	var row struct {
		ID   string `db:"id"`
		Hash string `db:"request_hash"`
	}
	if err := r.db.GetContext(ctx, &row, q, tenantID, key); err != nil {
		if err == sql.ErrNoRows {
			return nil, "", ErrInstanceNotFound
		}
		return nil, "", fmt.Errorf("bpm.SQLRepository.FindByIdempotencyKey: %w", err)
	}
	inst, err := r.GetInstance(ctx, tenantID, row.ID)
	if err != nil {
		return nil, "", fmt.Errorf("bpm.SQLRepository.FindByIdempotencyKey: %w", err)
	}
	return inst, row.Hash, nil
}

// GetInstance returns one tenant-scoped instance by id, or ErrInstanceNotFound.
func (r *SQLRepository) GetInstance(ctx context.Context, tenantID, id string) (*Instance, error) {
	const q = `SELECT ` + instanceColumns + `
		FROM sys_approval_instances
		WHERE tenant_id = ? AND id = ?`
	var row instanceRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrInstanceNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.GetInstance: %w", err)
	}
	inst, err := rowToInstance(&row)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.GetInstance: %w", err)
	}
	return &inst, nil
}

// ListPendingForUser returns the PENDING instances awaiting THIS user's decision in the tenant —
// the approver's inbox (specs/043 S3a2: read from the work seats). A candidate is a pending
// instance on which the user holds an open seat (user seat, or a role seat of a role in roleIDs)
// OR which the current_approver_* projection names them on; each candidate is then judged by
// AwaitsActor against ALL its open seats, so a seat left behind by a pre-S3a binary is not
// believed and a seatless instance still shows through the projection (D-6). roleIDs may be empty;
// view (nil = not judged) drops the queue seats of an entity the user may not view.
// Newest-first, the shape of before (the handler serializes bpm.Instance verbatim).
//
// The candidates are a UNION of two index range scans — idx_ai_pending (projection half) and
// idx_bt_inbox (seat half) — materialised, then joined by primary key; tenant_id pins both halves,
// the join and the seat join. Not `projection OR id IN (seat sub-select)` in one WHERE: MySQL
// cannot range-scan that OR, and the plan fell back to every pending instance of EVERY tenant
// (idx_ai_sla) with a dependent sub-select per row (EXPLAIN ANALYZE, 60 000-row clone, S3a2).
func (r *SQLRepository) ListPendingForUser(ctx context.Context, tenantID, userID string, roleIDs []string, view EntityView) ([]Instance, error) {
	projPred, projArgs := holderPredicate("a.current_approver_type", "a.current_approver_id", userID, roleIDs, "")
	seatPred, seatArgs := holderPredicate("s.assignee_type", "s.assignee_id", userID, roleIDs, tenantID)
	candidates := `SELECT a.id FROM sys_approval_instances a WHERE a.tenant_id = ? AND a.status = 'pending' AND (` + projPred + `)
		UNION
		SELECT s.instance_id FROM sys_bpm_tasks s WHERE s.tenant_id = ? AND s.status = 'open' AND (` + seatPred + `)
			AND (s.claimed_by IS NULL OR s.claimed_by = ?)`
	args := append(append(append(append([]any{tenantID}, projArgs...), tenantID), seatArgs...), userID)
	pending, err := r.pendingWithSeats(ctx, "ListPendingForUser", tenantID, candidates, args)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListPendingForUser: %w", err)
	}
	out := make([]Instance, 0, len(pending))
	for _, p := range pending {
		if AwaitsActor(&p.inst, p.open, userID, roleIDs, view) {
			p.inst.InRework = reworkSeatOf(&p.inst, p.open)
			out = append(out, p.inst)
		}
	}
	return out, nil
}

// holderPredicate is "(typeCol = 'user' AND idCol = ?) OR (typeCol = 'role' AND idCol IN (?,…))"
// with its arguments — the role half only when roleIDs is non-empty. Column names are the
// caller's compile-time literals; every value travels as a `?`.
//
// queueTenant != "" adds the third holder kind of a SEAT column (never of the ENUM projection):
// "(typeCol = 'queue' AND idCol IN (live queues of that tenant visible to the user))" — a queue is
// visible to everyone when it names no role, else to holders of its role (specs/043 S5).
func holderPredicate(typeCol, idCol, userID string, roleIDs []string, queueTenant string) (string, []any) {
	args := []any{userID}
	pred := "(" + typeCol + " = 'user' AND " + idCol + " = ?)"
	var in string
	if len(roleIDs) > 0 {
		placeholders := make([]string, len(roleIDs))
		for i, rid := range roleIDs {
			placeholders[i] = "?"
			args = append(args, rid)
		}
		in = strings.Join(placeholders, ",")
		pred += " OR (" + typeCol + " = 'role' AND " + idCol + " IN (" + in + "))"
	}
	if queueTenant != "" && in != "" {
		// a TEAM seat (S6a): roleIDs carries the user's teams too (Actor.HolderIDs). Seat columns
		// only — the ENUM projection never names a team (D-5).
		pred += " OR (" + typeCol + " = 'team' AND " + idCol + " IN (" + in + "))"
		for _, rid := range roleIDs {
			args = append(args, rid)
		}
	}
	if queueTenant != "" {
		args = append(args, queueTenant)
		pred += " OR (" + typeCol + " = 'queue' AND " + idCol + " IN (" + visibleQueuesSQL(in) + "))"
		for _, rid := range roleIDs {
			args = append(args, rid)
		}
	}
	return pred, args
}

// visibleQueuesSQL is the sub-select of the live work queues of the tenant (one `?` for it) that a
// user holding the roles behind inPlaceholders ("?,?,…", "" = none) may see: role-less queues are
// everyone's. The caller appends the role ids after the tenant id.
func visibleQueuesSQL(inPlaceholders string) string {
	role := "q.role_id IS NULL"
	if inPlaceholders != "" {
		role += " OR q.role_id IN (" + inPlaceholders + ")"
	}
	return `SELECT q.id FROM sys_work_queues q WHERE q.tenant_id = ? AND q.deleted_at IS NULL AND q.is_active = 1 AND (` + role + `)`
}

// pendingInstance is one decoded instance with the OPEN seats it has now.
type pendingInstance struct {
	inst Instance
	open []Seat
}

// pendingSeatRow is one instance row joined with one of its open seats (NULL seat columns = the
// instance has none).
type pendingSeatRow struct {
	instanceRow
	SeatNodeID       *string `db:"seat_node_id"`
	SeatAssigneeType *string `db:"seat_assignee_type"`
	SeatAssigneeID   *string `db:"seat_assignee_id"`
	SeatKind         *string `db:"seat_kind"`
	SeatClaimedBy    *string `db:"seat_claimed_by"`
	SeatQueueLive    bool    `db:"queue_live"`
	SeatQueueRoleID  *string `db:"queue_role_id"`
}

// pendingWithSeats reads the PENDING instances of the tenant whose ids the candidates query yields
// (a SELECT of one id column, pinned to the tenant by the caller; args are its arguments) together
// with their open seats, in inbox order, and decodes them — a row that will not decode is skipped
// and logged by decodeInstances (#68), never dropped silently.
func (r *SQLRepository) pendingWithSeats(ctx context.Context, op, tenantID, candidates string, args []any) ([]pendingInstance, error) {
	q := `SELECT i.id, i.tenant_id, i.process_id, i.process_snapshot, i.entity_name, i.record_id, i.current_step,
		i.status, i.current_approver_type, i.current_approver_id, i.sla_due_at, i.escalated, i.last_error, i.history, i.created_by,
		i.created_at, i.updated_at,
		t.node_id AS seat_node_id, t.assignee_type AS seat_assignee_type, t.assignee_id AS seat_assignee_id,
		t.kind AS seat_kind, t.claimed_by AS seat_claimed_by, ` + queueSeatCols + `
		FROM (` + candidates + `) c
		JOIN sys_approval_instances i ON i.id = c.id AND i.tenant_id = ? AND i.status = 'pending'
		LEFT JOIN sys_bpm_tasks t ON t.tenant_id = i.tenant_id AND t.instance_id = i.id AND t.status = 'open'
		` + queueSeatJoin + `
		ORDER BY i.created_at DESC, i.id DESC, t.id ASC`
	var rows []pendingSeatRow
	if err := r.db.SelectContext(ctx, &rows, q, append(args, tenantID)...); err != nil {
		return nil, err
	}
	var order []instanceRow
	seats := map[string][]Seat{}
	for i := range rows {
		row := &rows[i]
		if _, seen := seats[row.ID]; !seen {
			order = append(order, row.instanceRow)
			seats[row.ID] = nil
		}
		if row.SeatNodeID != nil && row.SeatAssigneeType != nil && row.SeatAssigneeID != nil {
			sr := seatReadRow{
				NodeID: *row.SeatNodeID, AssigneeType: *row.SeatAssigneeType, AssigneeID: *row.SeatAssigneeID,
				ClaimedBy: row.SeatClaimedBy, QueueLive: row.SeatQueueLive, QueueRoleID: row.SeatQueueRoleID,
			}
			if row.SeatKind != nil {
				sr.Kind = *row.SeatKind
			}
			seats[row.ID] = append(seats[row.ID], sr.seat())
		}
	}
	insts := r.decodeInstances(op, order)
	out := make([]pendingInstance, len(insts))
	for i := range insts {
		out[i] = pendingInstance{inst: insts[i], open: seats[insts[i].ID]}
	}
	return out, nil
}

// SeatDriftRow names one pending instance whose inbox still rests on the projection fallback.
type SeatDriftRow struct {
	InstanceID string
	EntityName string
	RecordID   string
	Drift      SeatDrift
}

// ListSeatDrift returns up to limit PENDING instances of the tenant whose seats do not carry their
// inbox (seatDriftOf) — the Doctor's count of what the D-6 fallback still answers for; at zero on
// every tenant the fallback can go (specs/043 "Nợ còn mở" 9). Read-only, tenant-pinned. A row that
// will not decode is logged and skipped (decodeInstances), not counted as drift.
//
// ponytail: reads every pending instance of the tenant with its open seats (node consistency needs
// the pinned JSON, judged in Go by the same trustedSeats the inbox uses); fine while pending
// instances per tenant are in the thousands — push the node test into SQL if a tenant outgrows it.
func (r *SQLRepository) ListSeatDrift(ctx context.Context, tenantID string, limit int) ([]SeatDriftRow, error) {
	pending, err := r.pendingWithSeats(ctx, "ListSeatDrift", tenantID,
		`SELECT id FROM sys_approval_instances WHERE tenant_id = ? AND status = 'pending'`, []any{tenantID})
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListSeatDrift: %w", err)
	}
	var out []SeatDriftRow
	for _, p := range pending {
		if len(out) >= limit {
			break
		}
		if d := seatDriftOf(&p.inst, p.open); d != "" {
			out = append(out, SeatDriftRow{InstanceID: p.inst.ID, EntityName: p.inst.EntityName, RecordID: p.inst.RecordID, Drift: d})
		}
	}
	return out, nil
}

// ListOpenSeats returns the OPEN seats of one tenant-scoped instance (who holds the step it stands
// on). Authority to decide and the /approvals/:id visibility gate both judge them with the inbox's
// own rule (AwaitsActor).
func (r *SQLRepository) ListOpenSeats(ctx context.Context, tenantID, instanceID string) ([]Seat, error) {
	out, err := openSeatsOf(ctx, r.db, tenantID, instanceID)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListOpenSeats: %w", err)
	}
	return out, nil
}

// openSeatsOf is the one reader of an instance's open seats, on the pool or inside a transaction.
func openSeatsOf(ctx context.Context, q sqlx.QueryerContext, tenantID, instanceID string) ([]Seat, error) {
	stmt := `SELECT t.node_id, t.assignee_type, t.assignee_id, t.kind, t.claimed_by, t.delegated_from, t.comment, ` + queueSeatCols + `
		FROM sys_bpm_tasks t ` + queueSeatJoin + `
		WHERE t.tenant_id = ? AND t.instance_id = ? AND t.status = 'open' ORDER BY t.id`
	var rows []seatReadRow
	if err := sqlx.SelectContext(ctx, q, &rows, stmt, tenantID, instanceID); err != nil {
		return nil, err
	}
	out := make([]Seat, len(rows))
	for i := range rows {
		out[i] = rows[i].seat()
	}
	return out, nil
}

// queueSeatCols / queueSeatJoin read, with a seat, the work queue a queue seat names (aliased q):
// whether it is still live and the role it is visible to. The join fires only for assignee_type =
// 'queue' and is pinned to the seat's tenant, so a queue of another tenant never matches. The same
// text serves every seat read, so "who holds a queue seat" is answered from one place.
const (
	queueSeatCols = `q.id IS NOT NULL AS queue_live, q.role_id AS queue_role_id`
	queueSeatJoin = `LEFT JOIN sys_work_queues q ON t.assignee_type = 'queue' AND q.id = t.assignee_id
		AND q.tenant_id = t.tenant_id AND q.deleted_at IS NULL AND q.is_active = 1`
)

// seatReadRow is one open seat as read with queueSeatCols (the t.* columns it names).
type seatReadRow struct {
	NodeID       string  `db:"node_id"`
	AssigneeType string  `db:"assignee_type"`
	AssigneeID   string  `db:"assignee_id"`
	Kind         string  `db:"kind"`
	ClaimedBy    *string `db:"claimed_by"`
	QueueLive    bool    `db:"queue_live"`
	QueueRoleID  *string `db:"queue_role_id"`
	// DelegatedFrom: a repin keeps a handed-on seat's delegate (specs/043 S11); only openSeatsOf reads it.
	DelegatedFrom *string `db:"delegated_from"`
	// Comment: a rework seat's return reason; only openSeatsOf reads it, so a repin that rewrites the seat keeps it.
	Comment *string `db:"comment"`
}

func (r seatReadRow) seat() Seat {
	s := Seat{
		NodeID: r.NodeID, AssigneeType: ApproverType(r.AssigneeType), AssigneeID: r.AssigneeID,
		Kind: r.Kind, QueueLive: r.QueueLive,
	}
	if r.DelegatedFrom != nil {
		s.DelegatedFrom = *r.DelegatedFrom
	}
	if r.Comment != nil {
		s.Note = *r.Comment
	}
	if r.ClaimedBy != nil {
		s.ClaimedBy = *r.ClaimedBy
	}
	if r.QueueRoleID != nil {
		s.QueueRoleID = *r.QueueRoleID
	}
	return s
}

// DecideTaskTx is THE state write of an approval instance (specs/043 D-4, S3b): decide, recall,
// delegate and every history append go through it, in ONE transaction —
//
//  1. SELECT the instance FOR UPDATE: every writer of the instance (and of its seats) queues on
//     this row, so what is judged below is still true when it is written;
//  2. status must still be pending (ErrNotPending) and seq must be the one the caller READ
//     (ErrConcurrent) — the CAS key, so a write that left current_step and escalated as they were
//     (a delegation, an ABA) still makes a stale decision lose;
//  3. g.Actor, when set, must hold an open seat of the step, or be an admin (ErrNotAuthorized) —
//     authority is the seat, judged by the inbox's own rule (AwaitsActor);
//  4. an instance no seat mirrors (armed by a pre-S3a binary) gets the seat of its holder now,
//     when an actor acts on it, so the decision leaves the seat row a decision leaves (D-6);
//  5. the guarded UPDATE (seq + 1) and next.Seats.
//
// A caller that loses writes nothing anywhere. The seats are read WITHOUT a lock after the
// instance row is held: every seat writer takes the instance row first, and locking the seat
// range would take gap locks on the neighbouring instances' seats (DAILY-QUEUE #214).
//
// ponytail: one open seat per step until S4, so the instance lock + seq serialize everything and
// the seat is closed by status; the per-seat `version` CAS arrives with parallel seats (S4).
func (r *SQLRepository) DecideTaskTx(ctx context.Context, tenantID, id string, g TaskGuard, next InstanceTransition, historyJSON []byte) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	const lock = `SELECT id, tenant_id, process_id, process_snapshot, entity_name, record_id, current_step, status,
		current_approver_type, current_approver_id, sla_due_at, escalated, seq, last_error, history, created_by, created_at, updated_at
		FROM sys_approval_instances WHERE tenant_id = ? AND id = ? FOR UPDATE`
	var row instanceRow
	if err := tx.GetContext(ctx, &row, lock, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return ErrInstanceNotFound
		}
		return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: lock instance: %w", err)
	}
	if row.Status != string(StatusPending) {
		return ErrNotPending
	}
	if row.Seq != g.ExpectSeq {
		return ErrConcurrent
	}
	now := r.now().UTC()

	if g.Actor != nil {
		inst, err := rowToInstance(&row)
		if err != nil {
			return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: %w", err)
		}
		open, err := openSeatsOf(ctx, tx, tenantID, id)
		if err != nil {
			return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: read seats: %w", err)
		}
		if err := authorize(&inst, open, *g.Actor); err != nil {
			return err
		}
		// An acting actor, a projection that names a holder, and no seat the instance can trust:
		// the seat of that holder is created here so the closing of it below has a row to close.
		if next.Seats.acted() && inst.CurrentApproverType != "" && len(trustedSeats(&inst, open)) == 0 {
			touch := Seat{
				NodeID: seatNodeID(&inst), AssigneeType: inst.CurrentApproverType, AssigneeID: inst.CurrentApproverID,
				DueAt: inst.SLADueAt, DueBasis: inst.CurrentSLABasis(),
			}
			if inst.Escalated {
				touch.BreachNotifiedAt = escalatedAt(&inst, now)
			}
			// entry_seq = the seq it stands at now (COALESCE(NULL, seq) in the insert): the visit
			// a pre-S3a instance has been on, the same one a delegation's SameVisit finds below.
			if err := writeSeats(ctx, tx, tenantID, id, SeatWrite{By: g.Actor.UserID, Open: []Seat{touch}}, now); err != nil {
				return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: seat on touch: %w", err)
			}
		}
	}

	if err := r.commitTransition(ctx, tx, tenantID, id, g.ExpectSeq, next, historyJSON, now); err != nil {
		return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: commit: %w", err)
	}
	return nil
}

// commitTransition is the write half shared by every state change of an instance whose row the
// transaction already holds: the guarded UPDATE (seq + 1, only while pending at expectSeq), the
// seat side of the transition, and the breach job of the step it leaves the instance on.
func (r *SQLRepository) commitTransition(ctx context.Context, tx *sqlx.Tx, tenantID, id string, expectSeq int64, next InstanceTransition, historyJSON []byte, now time.Time) error {
	const q = `UPDATE sys_approval_instances
		SET sla_paused_at = IF(? IS NOT NULL OR current_step <> ?, NULL, sla_paused_at),
		    sla_remaining_seconds = IF(? IS NOT NULL OR current_step <> ?, NULL, sla_remaining_seconds),
		    current_step = ?, status = ?, current_approver_type = ?, current_approver_id = ?,
		    sla_due_at = ?, escalated = ?, history = ?, last_error = ?, context = COALESCE(?, context), seq = seq + 1
		WHERE tenant_id = ? AND id = ? AND status = 'pending' AND seq = ?`
	res, err := tx.ExecContext(ctx, q,
		next.SLADueAt, next.CurrentStep, next.SLADueAt, next.CurrentStep,
		next.CurrentStep, string(next.Status),
		nullIfEmpty(string(next.CurrentApproverType)), nullIfEmpty(next.CurrentApproverID),
		next.SLADueAt, next.Escalated, string(historyJSON), nullIfEmpty(truncateRunes(next.LastError, maxLastErrorRunes)), nullJSON(next.ContextJSON),
		tenantID, id, expectSeq,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n != 1 { // unreachable under the row lock; kept so a lost CAS can never write seats
		return ErrConcurrent
	}
	if c := next.Clock; c != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE sys_approval_instances SET sla_paused_at = ?, sla_remaining_seconds = ? WHERE tenant_id = ? AND id = ?`,
			c.PausedAt, c.RemainingSeconds, tenantID, id); err != nil {
			return fmt.Errorf("bpm.SQLRepository.commitTransition: sla clock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sys_bpm_tasks SET due_at = ? WHERE tenant_id = ? AND instance_id = ? AND status = 'open'`,
			next.SLADueAt, tenantID, id); err != nil {
			return fmt.Errorf("bpm.SQLRepository.commitTransition: seat due: %w", err)
		}
	}
	if next.Pin != nil {
		snapshot, err := json.Marshal(next.Pin)
		if err != nil {
			return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: marshal pin: %w", err)
		}
		var version any // NULL = no version known, as in CreateInstance
		if v := next.Pin.version(); v > 0 {
			version = v
		}
		const pin = `UPDATE sys_approval_instances SET process_snapshot = ?, process_version = ? WHERE tenant_id = ? AND id = ?`
		if _, err := tx.ExecContext(ctx, pin, string(snapshot), version, tenantID, id); err != nil {
			return fmt.Errorf("bpm.SQLRepository.DecideTaskTx: re-pin: %w", err)
		}
	}
	if err := writeSeats(ctx, tx, tenantID, id, next.Seats, now); err != nil {
		return err
	}
	if next.Status == StatusPending {
		if next.Continue && next.LastError == "" {
			if err := r.enqueueContinue(ctx, tx, tenantID, id, expectSeq+1); err != nil {
				return err
			}
		}
		return r.enqueueSLADue(ctx, tx, tenantID, id, expectSeq+1, next.SLADueAt, next.Escalated)
	}
	return nil
}

// ListVisitSeats returns every seat, in any state, of the LATEST visit of node: the seats that
// share the highest entry_seq the instance has on it (a delegation continues the visit, so a
// handed-on seat and its replacement are both here). idx_bt_visit (tenant, instance, node,
// entry_seq) serves both the sub-select and the outer read. tenant_id pins every part.
func (r *SQLRepository) ListVisitSeats(ctx context.Context, tenantID, instanceID, nodeID string) ([]VisitSeat, error) {
	out, err := visitSeatsOf(ctx, r.db, tenantID, instanceID, nodeID)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListVisitSeats: %w", err)
	}
	return out, nil
}

type visitSeatRow struct {
	ID            string     `db:"id"`
	NodeID        string     `db:"node_id"`
	AssigneeType  string     `db:"assignee_type"`
	AssigneeID    string     `db:"assignee_id"`
	Status        string     `db:"status"`
	Outcome       *string    `db:"outcome"`
	DecidedBy     *string    `db:"decided_by"`
	Comment       *string    `db:"comment"`
	DelegatedFrom *string    `db:"delegated_from"`
	Kind          string     `db:"kind"`
	EntrySeq      int64      `db:"entry_seq"`
	Version       int        `db:"version"`
	CompletedAt   *time.Time `db:"completed_at"`
}

func strOf(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// visitSeatsOf is the one reader of a visit, on the pool or inside a transaction.
func visitSeatsOf(ctx context.Context, q sqlx.QueryerContext, tenantID, instanceID, nodeID string) ([]VisitSeat, error) {
	const stmt = `SELECT id, node_id, assignee_type, assignee_id, status, outcome, decided_by, comment,
		delegated_from, kind, entry_seq, version, completed_at
		FROM sys_bpm_tasks
		WHERE tenant_id = ? AND instance_id = ? AND node_id = ?
		  AND entry_seq = (SELECT MAX(entry_seq) FROM sys_bpm_tasks WHERE tenant_id = ? AND instance_id = ? AND node_id = ?)
		ORDER BY created_at, id`
	var rows []visitSeatRow
	if err := sqlx.SelectContext(ctx, q, &rows, stmt, tenantID, instanceID, nodeID, tenantID, instanceID, nodeID); err != nil {
		return nil, err
	}
	out := make([]VisitSeat, len(rows))
	for i, row := range rows {
		out[i] = VisitSeat{
			ID: row.ID, NodeID: row.NodeID, AssigneeType: ApproverType(row.AssigneeType), AssigneeID: row.AssigneeID,
			Status: row.Status, Outcome: SeatOutcome(strOf(row.Outcome)), DecidedBy: strOf(row.DecidedBy),
			Comment: strOf(row.Comment), DelegatedFrom: strOf(row.DelegatedFrom), Kind: row.Kind,
			EntrySeq: row.EntrySeq, Version: row.Version, CompletedAt: row.CompletedAt,
		}
	}
	return out, nil
}

// VoteTaskTx records one vote on a parallel step and counts the visit, in ONE transaction
// (specs/043 S4, D-4):
//
//  1. lock the instance row — every writer of the instance and of its seats queues on it, which is
//     what makes the count below still true when it is written, and two last votes arrive one
//     after the other;
//  2. still pending (ErrNotPending) and still on the step the caller saw (ErrConcurrent);
//  3. read the visit the voter's seat belongs to and let castVote judge it: seat still open
//     (ErrConcurrent), the voter has not voted in this visit (ErrAlreadyDecidedByActor), and what
//     the votes add up to;
//  4. close the voter's seat — CAS on its own version;
//  5. the step is not over: append the history entry (Partial) and bump seq, nothing else moves;
//     the step is over: apply the transition the caller prepared for this outcome, supersede the
//     sibling seats and open the next step's, exactly like a single decision.
//
// No seq is matched (see VoteGuard): a vote only loses when the step moved or its seat closed, never
// because a sibling voted first. The history is re-read under the lock and appended to — never
// overwritten with the caller's copy — so concurrent votes keep each other's entries.
func (r *SQLRepository) VoteTaskTx(ctx context.Context, tenantID, id string, g VoteGuard, onComplete InstanceTransition) (*VoteResult, error) {
	if g.Kind != SeatApproved && g.Kind != SeatRejected {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: vote kind %q is neither approved nor rejected", g.Kind)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	const lock = `SELECT id, tenant_id, process_id, process_snapshot, entity_name, record_id, current_step, status,
		current_approver_type, current_approver_id, sla_due_at, escalated, seq, last_error, history, created_by, created_at, updated_at
		FROM sys_approval_instances WHERE tenant_id = ? AND id = ? FOR UPDATE`
	var row instanceRow
	if err := tx.GetContext(ctx, &row, lock, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrInstanceNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: lock instance: %w", err)
	}
	if row.Status != string(StatusPending) {
		return nil, ErrNotPending
	}
	if row.CurrentStep != g.ExpectStep {
		return nil, ErrConcurrent
	}
	now := r.now().UTC()

	visit, err := visitSeatsOf(ctx, tx, tenantID, id, g.Node)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: read visit: %w", err)
	}
	after, verdict, err := castVote(visit, g.SeatID, g.Actor.UserID, g.Kind, g.policy(), g.Comment, now)
	if err != nil {
		return nil, err
	}
	if g.Unrouted && verdict != VerdictPending {
		return nil, errRouteNeeded // nothing written yet: the deferred rollback leaves no trace
	}
	var version int
	for _, v := range visit {
		if v.ID == g.SeatID {
			version = v.Version
		}
	}
	const closeSeat = `UPDATE sys_bpm_tasks
		SET status = 'done', outcome = ?, decided_by = ?, comment = ?, completed_at = ?, updated_at = ?, version = version + 1
		WHERE tenant_id = ? AND instance_id = ? AND id = ? AND status = 'open' AND version = ?`
	res, err := tx.ExecContext(ctx, closeSeat, string(g.Kind), g.Actor.UserID, nullIfEmpty(g.Comment), now, now,
		tenantID, id, g.SeatID, version)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: close seat: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: close seat rows: %w", err)
	} else if n != 1 {
		return nil, ErrConcurrent
	}

	var history []HistoryEntry
	if err := json.Unmarshal(row.History, &history); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: decode history: %w", err)
	}
	entry := g.Entry
	if verdict == VerdictPending {
		entry.Partial = true
		entry.ReturnedTo, entry.Target = "", "" // a vote that does not finish the step returns nothing
	} else {
		entry.SLABasis = g.CompleteBasis
	}
	history = append(history, entry)
	historyJSON, err := json.Marshal(history)
	if err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: marshal history: %w", err)
	}

	open, approved, rejected := Tally(after)
	out := &VoteResult{Verdict: verdict, Open: open, Approved: approved, Rejected: rejected, History: history}
	if verdict == VerdictPending {
		const bump = `UPDATE sys_approval_instances SET history = ?, seq = seq + 1
			WHERE tenant_id = ? AND id = ? AND status = 'pending' AND seq = ?`
		if _, err := tx.ExecContext(ctx, bump, string(historyJSON), tenantID, id, row.Seq); err != nil {
			return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: record vote: %w", err)
		}
		// seq moved, the deadline did not: the breach job parked for the old seq is now stale, so
		// the step keeps one that matches (handlers skip a job whose seq has moved on).
		if err := r.enqueueSLADue(ctx, tx, tenantID, id, row.Seq+1, row.SLADueAt, row.Escalated); err != nil {
			return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: %w", err)
		}
	} else {
		want := VerdictApproved
		if g.Kind == SeatRejected {
			want = VerdictRejected
		}
		if verdict != want { // cannot happen (see evaluateCompletion); never settle on the wrong outcome
			return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: a %s vote produced verdict %s", g.Kind, verdict)
		}
		// The sibling seats still open are superseded (cancelled, decided by nobody) as the
		// next step's seats open — the seat side of the prepared transition.
		onComplete.Seats = SeatWrite{Close: SeatSuperseded, By: g.Actor.UserID, Open: onComplete.Seats.Open}
		if err := r.commitTransition(ctx, tx, tenantID, id, row.Seq, onComplete, historyJSON, now); err != nil {
			return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: %w", err)
		}
		out.Completed = true
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.VoteTaskTx: commit: %w", err)
	}
	return out, nil
}

// writeSeats applies one transition's seat side inside the caller's transaction: close every OPEN
// seat of the instance with w.Close, then open w.Open. Every statement is pinned to tenantID.
//
// A new seat takes process/entity/record and its visit number (entry_seq) from the instance row
// itself, read inside the same transaction — so entry_seq is the seq this very write produced.
// Every open seat is closed, not only "the actor's": until S4 a step has one seat, and a seat a
// pre-S3a binary left open on a step the instance has since left is healed by the next write of
// this binary (closed as superseded, see SeatSuperseded) instead of lingering beside the new one.
func writeSeats(ctx context.Context, tx *sqlx.Tx, tenantID, instanceID string, w SeatWrite, now time.Time) error {
	// entry: NULL = a new visit (the instance's seq, read in the INSERT below). A continued visit
	// takes the entry_seq of the open seat it replaces — or, on an instance no seat mirrors yet
	// (armed by a pre-S3a binary), the seq it stood at before this write. Read BEFORE closing.
	var entry sql.NullInt64
	if w.SameVisit && len(w.Open) > 0 {
		const q = `SELECT COALESCE(
			(SELECT MAX(entry_seq) FROM sys_bpm_tasks
				WHERE tenant_id = ? AND instance_id = ? AND status = 'open' AND node_id = ?),
			(SELECT seq - 1 FROM sys_approval_instances WHERE tenant_id = ? AND id = ?))`
		if err := tx.GetContext(ctx, &entry, q, tenantID, instanceID, w.Node, tenantID, instanceID); err != nil {
			return fmt.Errorf("read current visit: %w", err)
		}
	}
	if w.Close != "" {
		if w.Node != "" {
			const q = `UPDATE sys_bpm_tasks
				SET status = 'cancelled', outcome = ?, completed_at = ?, updated_at = ?, version = version + 1
				WHERE tenant_id = ? AND instance_id = ? AND status = 'open' AND node_id <> ?`
			if _, err := tx.ExecContext(ctx, q, string(SeatSuperseded), now, now, tenantID, instanceID, w.Node); err != nil {
				return fmt.Errorf("supersede stale seats: %w", err)
			}
		}
		status, acted, err := seatEnd(w.Close)
		if err != nil {
			return err
		}
		var by, note sql.NullString
		if acted { // who acted and why belong to a seat that was acted on, not one that was withdrawn
			by, note = nullIfEmpty(w.By), nullIfEmpty(w.Comment)
		}
		q := `UPDATE sys_bpm_tasks
			SET status = ?, outcome = ?, decided_by = ?, comment = ?, completed_at = ?, updated_at = ?, version = version + 1
			WHERE tenant_id = ? AND instance_id = ? AND status = 'open'`
		args := []any{status, string(w.Close), by, note, now, now, tenantID, instanceID}
		if w.CloseSeat != "" { // only the delegator's own seat of a many-seat step
			q += ` AND id = ?`
			args = append(args, w.CloseSeat)
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("close seats: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("close seats: rows affected: %w", err)
		} else if w.CloseSeat != "" && n != 1 {
			return ErrConcurrent // the seat being handed on is gone
		}
	}
	for _, s := range w.Open {
		const q = `INSERT INTO sys_bpm_tasks
			(id, tenant_id, instance_id, process_id, entity_name, record_id, node_id, entry_seq, kind,
			 assignee_type, assignee_id, status, due_at, due_basis, breach_notified_at, delegated_from, comment,
			 version, created_by, created_at, updated_at)
			SELECT ?, tenant_id, id, process_id, entity_name, record_id, ?, COALESCE(?, seq), ?,
			       ?, ?, 'open', ?, ?, ?, ?, ?, 0, ?, ?, ?
			FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`
		kind := s.Kind
		if kind == "" {
			kind = SeatKindApproval
		}
		res, err := tx.ExecContext(ctx, q, uuid.NewString(), s.NodeID, entry, kind,
			string(s.AssigneeType), s.AssigneeID, s.DueAt, nullIfEmpty(string(s.DueBasis)),
			s.BreachNotifiedAt, nullIfEmpty(s.DelegatedFrom), nullIfEmpty(s.Note), nullIfEmpty(w.By), now, now,
			tenantID, instanceID)
		if err != nil {
			return fmt.Errorf("open seat %s: %w", s.NodeID, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("open seat %s: rows affected: %w", s.NodeID, err)
		}
		if n != 1 {
			return fmt.Errorf("open seat %s: instance %s not found in its tenant", s.NodeID, instanceID)
		}
	}
	return nil
}

// seatEnd maps how a seat ended to its status, and whether its holder acted (a done seat keeps
// decided_by + comment; a cancelled one keeps neither — nobody decided it).
func seatEnd(o SeatOutcome) (status string, acted bool, err error) {
	switch o {
	case SeatApproved, SeatRejected, SeatReassigned, SeatCompleted:
		return "done", true, nil
	case SeatRecalled, SeatSuperseded, SeatRepinned:
		return "cancelled", false, nil
	default:
		return "", false, fmt.Errorf("unknown seat outcome %q", o)
	}
}

// InstanceTransition is the new state DecideTaskTx writes (the step the instance moves TO, or
// the terminal state it settles into). Built by the engine from the process steps.
type InstanceTransition struct {
	CurrentStep         int
	Status              InstanceStatus
	CurrentApproverType ApproverType
	CurrentApproverID   string
	SLADueAt            *time.Time
	Escalated           bool
	// Seats is the sys_bpm_tasks mirror of this transition (specs/043 S3a dual write).
	Seats SeatWrite
	// LastError is written with the transition: "" clears it, a message leaves the instance stuck on
	// the step with nobody holding it (specs/043 S6a, FR-018). A transition that does not change
	// who holds the step carries inst.LastError forward. On a machine step it is why the instance
	// stands still (specs/043 S7).
	LastError string
	// Pin, when set, replaces the instance's pinned definition (and process_version) in the same
	// transaction — a repin (specs/043 S11). Nil = the pin is untouched, as for every other verb.
	Pin *Definition
	// ContextJSON, when set, replaces the instance's run-time context (the branches it has taken);
	// nil leaves it untouched.
	ContextJSON []byte
	// Clock, when set, writes the SLA clock state (S13): PausedAt + RemainingSeconds freeze it, both
	// nil release it; either way the open seats take SLADueAt as their due_at (nil while frozen).
	// Nil leaves it alone — except that a transition which moves to another step or sets a
	// deadline always clears a stale freeze (commitTransition).
	Clock *SLAClockWrite
	// Continue says the instance now stands on a machine node: park a bpm_continue job in the same
	// transaction so a crash before the walk goes on does not strand it (FR-009).
	Continue bool
}

// SLAClockWrite is the frozen-clock state written by a pause or a resume (S13).
type SLAClockWrite struct {
	PausedAt         *time.Time
	RemainingSeconds *int64
}

// ListPendingByRecord returns the PENDING instances of (tenant, entity, record), any lane — the read
// behind the wait signal (S12a): an update of the record asks each one standing on a wait node to
// look at its condition again. idx_ai_record covers it; tenantID pins it unconditionally.
func (r *SQLRepository) ListPendingByRecord(ctx context.Context, tenantID, entityName, recordID string) ([]Instance, error) {
	const q = `SELECT ` + instanceColumns + `
		FROM sys_approval_instances
		WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND status = 'pending'`
	var rows []instanceRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, entityName, recordID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListPendingByRecord: %w", err)
	}
	return r.decodeInstances("ListPendingByRecord", rows), nil
}

// ListByRecord returns every APPROVAL (lane 'exclusive') instance for (tenant, entity, record), all
// statuses, newest first. Used by the per-record approvals read API (GET /:entity/:id/approvals):
// a tracking-lane instance (S12b) never shows in the Duyệt tab.
func (r *SQLRepository) ListByRecord(ctx context.Context, tenantID, entityName, recordID string) ([]Instance, error) {
	return r.ListByRecordLane(ctx, tenantID, entityName, recordID, LaneExclusive)
}

// ListByRecordLane is ListByRecord for one lane (LaneExclusive | LaneIndependent): the SLA card
// reads the tracking lane through it. The idx_ai_record index (tenant_id, entity_name, record_id)
// covers this query. tenantID pins the query unconditionally — no cross-tenant reads.
func (r *SQLRepository) ListByRecordLane(ctx context.Context, tenantID, entityName, recordID, lane string) ([]Instance, error) {
	const q = `SELECT ` + instanceColumns + `
		FROM sys_approval_instances
		WHERE tenant_id = ? AND entity_name = ? AND record_id = ? AND lane = ?
		ORDER BY created_at DESC, id DESC`
	var rows []instanceRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, entityName, recordID, laneOrDefault(lane)); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListByRecord: %w", err)
	}
	return r.decodeInstances("ListByRecord", rows), nil
}

// ----------------------------------------------------------------------------------------------
// SLA scan + claim — the breach detector the SLA scheduler runs each tick.
// ----------------------------------------------------------------------------------------------

// breachBatch bounds one tenant's breach scan per sweep; the rest is picked up by the next one.
const breachBatch = 200

// ListBreachTenants is the sweep's universe: the active tenants of the registry. Ids only — the
// breach reads themselves are one pinned query per tenant (ListBreachedForTenant), so there is no
// query in this package that reads approval rows across tenants.
//
// ponytail: one pinned query per active tenant per tick; fine into the hundreds of tenants — the
// durable per-instance job (S3c) replaces the sweep as the primary path before that ceiling.
func (r *SQLRepository) ListBreachTenants(ctx context.Context) ([]string, error) {
	const q = `SELECT id FROM tenants WHERE status = 'active' AND deleted_at IS NULL ORDER BY id`
	var ids []string
	if err := r.db.SelectContext(ctx, &ids, q); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListBreachTenants: %w", err)
	}
	return ids, nil
}

// ListBreachedForTenant returns the PENDING, not-yet-escalated instances of ONE tenant whose
// current step is past its deadline (oldest deadline first, at most breachBatch). The breach is
// read off the work seats — an open seat with a due_at <= now that nobody has been told about
// (idx_bt_breach, tenant_id first) — plus, as the D-6 fallback, a pending instance with NO open
// seat at all (armed by a pre-S3a binary) read off its own sla_due_at. A row that will not decode
// is logged and skipped (decodeInstances), as in every list read.
//
// ponytail: the seat arm trusts any open overdue seat; it does not check the instance's own
// sla_due_at or that the seat sits on the node the instance stands on. A seat a PRE-S3a binary left
// open on a step the instance has since left (rolling deploy / rollback) could escalate the
// current step early. Unreachable once every writer is S3a+ (every write closes all open seats);
// if a mixed-binary window is ever needed again, add `AND i.sla_due_at <= ?` to the JOIN.
func (r *SQLRepository) ListBreachedForTenant(ctx context.Context, tenantID string, now time.Time) ([]Instance, error) {
	const q = `SELECT i.id, i.tenant_id, i.process_id, i.process_snapshot, i.entity_name, i.record_id, i.current_step,
		i.status, i.current_approver_type, i.current_approver_id, i.sla_due_at, i.escalated, i.seq, i.last_error, i.history,
		i.created_by, i.created_at, i.updated_at
		FROM (
			SELECT t.instance_id AS id FROM sys_bpm_tasks t
			WHERE t.tenant_id = ? AND t.status = 'open' AND t.breach_notified_at IS NULL AND t.due_at IS NOT NULL AND t.due_at <= ?
			UNION
			SELECT a.id FROM sys_approval_instances a
			WHERE a.tenant_id = ? AND a.status = 'pending' AND a.escalated = 0 AND a.sla_due_at IS NOT NULL AND a.sla_due_at <= ?
			  AND NOT EXISTS (SELECT 1 FROM sys_bpm_tasks s WHERE s.tenant_id = a.tenant_id AND s.instance_id = a.id AND s.status = 'open')
		) c
		JOIN sys_approval_instances i ON i.id = c.id AND i.tenant_id = ? AND i.status = 'pending' AND i.escalated = 0
		ORDER BY i.sla_due_at ASC, i.id ASC
		LIMIT ?`
	var rows []instanceRow
	at := now.UTC()
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, at, tenantID, at, tenantID, breachBatch); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListBreachedForTenant: %w", err)
	}
	return r.decodeInstances("ListBreachedForTenant", rows), nil
}

// ResolveSeatUsers turns seats into the user ids that hold them: a user seat is its user, a role
// seat the ACTIVE, not-deleted users of the tenant carrying the role (users.role_id). Deduplicated,
// first-seen order, at most limit ids. Every statement is pinned to tenantID.
func (r *SQLRepository) ResolveSeatUsers(ctx context.Context, tenantID string, seats []Seat, limit int) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] && len(out) < limit {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, st := range seats {
		// A claimed pool seat is one person's: they alone are told (specs/043 S5).
		if st.ClaimedBy != "" && st.Claimable() {
			add(st.ClaimedBy)
			continue
		}
		switch st.AssigneeType {
		case ApproverUser:
			add(st.AssigneeID)
		case ApproverRole:
			if len(out) >= limit {
				continue
			}
			const q = `SELECT id FROM users WHERE tenant_id = ? AND role_id = ? AND status = 'active' AND deleted_at IS NULL ORDER BY id LIMIT ?`
			var ids []string
			if err := r.db.SelectContext(ctx, &ids, q, tenantID, st.AssigneeID, limit); err != nil {
				return out, fmt.Errorf("bpm.SQLRepository.ResolveSeatUsers: %w", err)
			}
			for _, id := range ids {
				add(id)
			}
		case ApproverTeam:
			if len(out) >= limit {
				continue
			}
			// The members of the team. team_memberships has no tenant_id: the team row pins the
			// tenant (teams.tenant_id), and so does the user (a member of another tenant is not one).
			const q = `SELECT u.id FROM team_memberships tm
				JOIN teams t ON t.id = tm.team_id AND t.tenant_id = ?
				JOIN users u ON u.id = tm.user_id AND u.tenant_id = t.tenant_id
				WHERE tm.team_id = ? AND u.status = 'active' AND u.deleted_at IS NULL ORDER BY u.id LIMIT ?`
			var ids []string
			if err := r.db.SelectContext(ctx, &ids, q, tenantID, st.AssigneeID, limit); err != nil {
				return out, fmt.Errorf("bpm.SQLRepository.ResolveSeatUsers: %w", err)
			}
			for _, id := range ids {
				add(id)
			}
		case ApproverQueue:
			if len(out) >= limit {
				continue
			}
			// The holders of the queue's role; a role-less queue is "everyone" and names nobody to
			// ping (told apart in the pool, not by a tenant-wide notice).
			const q = `SELECT u.id FROM users u JOIN sys_work_queues w ON w.tenant_id = u.tenant_id AND w.role_id = u.role_id
				WHERE u.tenant_id = ? AND w.id = ? AND w.deleted_at IS NULL AND w.is_active = 1
				  AND u.status = 'active' AND u.deleted_at IS NULL ORDER BY u.id LIMIT ?`
			var ids []string
			if err := r.db.SelectContext(ctx, &ids, q, tenantID, st.AssigneeID, limit); err != nil {
				return out, fmt.Errorf("bpm.SQLRepository.ResolveSeatUsers: %w", err)
			}
			for _, id := range ids {
				add(id)
			}
		}
	}
	return out, nil
}

// ClaimBreach atomically marks an instance's current step as escalated (escalated=1) and appends
// the escalation history in ONE UPDATE guarded by escalated=0 + status='pending' + the seq the
// scan READ. Returns true when THIS caller won the claim (RowsAffected==1) — so two scheduler
// ticks / instances never double-escalate the same step (the breach action fires AT MOST ONCE per
// step), and a claim on a row that changed since the scan (decided, delegated) loses.
//
// specs/043 S3a: the claim is a state write — it bumps seq and stamps breach_notified_at on the
// instance's open seats in the same transaction (the seat-level mirror of escalated=1), which is
// also what takes the seats out of the next scan.
func (r *SQLRepository) ClaimBreach(ctx context.Context, tenantID, id string, expectSeq int64, historyJSON []byte) (bool, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("bpm.SQLRepository.ClaimBreach: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	const q = `UPDATE sys_approval_instances
		SET escalated = 1, history = ?, seq = seq + 1
		WHERE tenant_id = ? AND id = ? AND status = 'pending' AND seq = ? AND escalated = 0`
	res, err := tx.ExecContext(ctx, q, string(historyJSON), tenantID, id, expectSeq)
	if err != nil {
		return false, fmt.Errorf("bpm.SQLRepository.ClaimBreach: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("bpm.SQLRepository.ClaimBreach: rows affected: %w", err)
	}
	if n != 1 {
		return false, nil
	}
	now := r.now().UTC()
	const qs = `UPDATE sys_bpm_tasks SET breach_notified_at = ?, updated_at = ?, version = version + 1
		WHERE tenant_id = ? AND instance_id = ? AND status = 'open' AND breach_notified_at IS NULL`
	if _, err := tx.ExecContext(ctx, qs, now, now, tenantID, id); err != nil {
		return false, fmt.Errorf("bpm.SQLRepository.ClaimBreach: stamp seats: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("bpm.SQLRepository.ClaimBreach: commit: %w", err)
	}
	return true, nil
}

// ----------------------------------------------------------------------------------------------
// in-app notification sink (sys_notifications) — reused for SLA escalation + decision notices.
// ----------------------------------------------------------------------------------------------

// Notification is the row the BPM engine writes to a user's inbox (SLA breach, decision needed).
// TenantID pins it; UserID is the recipient. EntityName/RecordID link it back to the source record.
type Notification struct {
	TenantID   string
	UserID     string
	Title      string
	Message    string
	EntityName string
	RecordID   string
	// Type drives the inbox icon + tone in the FE (NotificationBell): "approval" | "due" |
	// "assign" | "system". MOT-XUONG: the column was added on purpose by migration 000026
	// ("so the FE notification inbox can render different icons per category") and EXISTS in the
	// DB, but neither INSERT path ever listed it — so every row was NULL and not one of the five
	// FE branches ever matched. Migration landed, FE landed, the join in the middle was never
	// written. Empty stays NULL (back-compat).
	Type string
}

// InsertNotification appends one row to sys_notifications and returns its generated id. tenant_id +
// user_id are mandatory; blank optional link fields are stored NULL. workflow_id is left NULL (this
// is an approval-sourced notification, not a workflow-sourced one).
func (r *SQLRepository) InsertNotification(ctx context.Context, n Notification) (string, error) {
	id := uuid.NewString()
	const q = `INSERT INTO sys_notifications
		(id, tenant_id, user_id, title, message, type, entity_name, record_id, workflow_id, is_read, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		id, n.TenantID, n.UserID,
		nullIfEmpty(n.Title), n.Message, nullIfEmpty(n.Type),
		nullIfEmpty(n.EntityName), nullIfEmpty(n.RecordID),
		r.now().UTC(),
	); err != nil {
		return "", fmt.Errorf("bpm.SQLRepository.InsertNotification: %w", err)
	}
	return id, nil
}

// ----------------------------------------------------------------------------------------------
// helpers
// ----------------------------------------------------------------------------------------------

// decodeInstances decodes the rows a list read scanned. A row that will not decode is SKIPPED —
// one corrupt row must not blind the approver's inbox, a record's approvals tab, or the SLA sweep
// to every other row — but never silently (DAILY-QUEUE #68): it used to vanish from all three
// with no log line anywhere, so nobody could even learn an approval had gone missing. The line
// carries identifiers only, never the history/snapshot content (comments can hold personal data).
//
// Skip, do not rescue: a row whose snapshot fails to decode must not come back with a nil
// Definition, because nil means "armed before 000065, read the live process" — the #8 step-skip.
func (r *SQLRepository) decodeInstances(op string, rows []instanceRow) []Instance {
	out := make([]Instance, 0, len(rows))
	for i := range rows {
		inst, err := rowToInstance(&rows[i])
		if err != nil {
			row := &rows[i]
			r.logger.Error("bpm: approval instance row undecodable — skipped, it is missing from this read",
				zap.String("op", op), zap.String("tenant_id", row.TenantID),
				zap.String("instance_id", row.ID), zap.String("process_id", row.ProcessID),
				zap.String("entity_name", row.EntityName), zap.String("record_id", row.RecordID),
				zap.String("status", row.Status), zap.Error(err))
			continue
		}
		out = append(out, inst)
	}
	return out
}

// rowToInstance decodes a persisted instance row into the domain Instance.
func rowToInstance(row *instanceRow) (Instance, error) {
	inst := Instance{
		ID:          row.ID,
		TenantID:    row.TenantID,
		ProcessID:   row.ProcessID,
		EntityName:  row.EntityName,
		RecordID:    row.RecordID,
		CurrentStep: row.CurrentStep,
		Status:      InstanceStatus(row.Status),
		SLADueAt:    row.SLADueAt,
		SLAPausedAt: row.SLAPausedAt, SLARemainingSeconds: row.SLARemaining,
		Escalated: row.Escalated,
		Seq:       row.Seq,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
	if row.CurrentApproverType != nil {
		inst.CurrentApproverType = ApproverType(*row.CurrentApproverType)
	}
	if row.CurrentApproverID != nil {
		inst.CurrentApproverID = *row.CurrentApproverID
	}
	if row.CreatedBy != nil {
		inst.CreatedBy = *row.CreatedBy
	}
	if row.LastError != nil {
		inst.LastError = *row.LastError
	}
	if row.Context != nil && len(*row.Context) > 0 && string(*row.Context) != "null" {
		if err := json.Unmarshal(*row.Context, &inst.Context); err != nil {
			return Instance{}, fmt.Errorf("decode context: %w", err)
		}
	}
	if len(row.History) > 0 && string(row.History) != "null" {
		if err := json.Unmarshal(row.History, &inst.History); err != nil {
			return Instance{}, fmt.Errorf("decode history: %w", err)
		}
	}
	if row.ProcessSnapshot != nil && len(*row.ProcessSnapshot) > 0 && string(*row.ProcessSnapshot) != "null" {
		var def Definition
		if err := json.Unmarshal(*row.ProcessSnapshot, &def); err != nil {
			return Instance{}, fmt.Errorf("decode process snapshot: %w", err)
		}
		def.Steps = NormalizeSteps(def.Steps) // pinned before S0 → positional ids (Migration A agrees)
		def.Graph = def.Graph.normalized()
		inst.Definition = &def
		inst.ProcessVersion = def.Version
	}
	return inst, nil
}

// rowsAffectedOrNotFound returns notFound when the statement matched no row, else nil.
func rowsAffectedOrNotFound(res sql.Result, notFound error, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if n == 0 {
		return notFound
	}
	return nil
}

// nullIfEmpty maps a blank string to SQL NULL so optional columns stay NULL rather than "".
// nullJSON keeps an absent JSON document NULL rather than the string "" (which MySQL rejects for
// a JSON column). Absent outcome = today's behaviour: a decision writes nothing.
func nullJSON(b []byte) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return string(b)
}

func nullIfEmpty(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// truncateRunes cuts s to at most n runes (the width of a VARCHAR column counts characters).
func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// UserTeamIDs returns the teams the user is a member of, in the tenant (direct membership — the
// team's own pool, not the teams below it in the tree). team_memberships carries no tenant_id, so
// the team row pins it (teams.tenant_id): a membership row pointing at another tenant's team is
// never returned. Used to build the team half of Actor.HolderIDs.
func (r *SQLRepository) UserTeamIDs(ctx context.Context, tenantID, userID string) ([]string, error) {
	const q = `SELECT tm.team_id FROM team_memberships tm
		JOIN teams t ON t.id = tm.team_id AND t.tenant_id = ?
		WHERE tm.user_id = ? ORDER BY tm.team_id`
	var ids []string
	if err := r.db.SelectContext(ctx, &ids, q, tenantID, userID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.UserTeamIDs: %w", err)
	}
	return ids, nil
}

// UserActive reports whether userID is a real, active, not-deleted user of THIS tenant — what a
// routing rule must find before it seats someone (a field that still holds the id of a person who
// left must not park the step on a ghost).
func (r *SQLRepository) UserActive(ctx context.Context, tenantID, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	const q = `SELECT 1 FROM users WHERE tenant_id = ? AND id = ? AND status = 'active' AND deleted_at IS NULL LIMIT 1`
	var one int
	if err := r.db.GetContext(ctx, &one, q, tenantID, userID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("bpm.SQLRepository.UserActive: %w", err)
	}
	return true, nil
}

// HolderExists reports whether the user / role / team exists in THIS tenant — what an assignment or
// the fallback assignee must name at config time (a user must be active; a role or team just exist).
// Queues are judged by the work-queue repository, not here. Every statement is tenant-pinned.
func (r *SQLRepository) HolderExists(ctx context.Context, tenantID string, kind ApproverType, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	var q string
	switch kind {
	case ApproverUser:
		return r.UserActive(ctx, tenantID, id)
	case ApproverRole:
		q = `SELECT 1 FROM acl_roles WHERE tenant_id = ? AND id = ? LIMIT 1`
	case ApproverTeam:
		q = `SELECT 1 FROM teams WHERE tenant_id = ? AND id = ? LIMIT 1`
	default:
		return false, nil
	}
	var one int
	if err := r.db.GetContext(ctx, &one, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("bpm.SQLRepository.HolderExists: %w", err)
	}
	return true, nil
}
