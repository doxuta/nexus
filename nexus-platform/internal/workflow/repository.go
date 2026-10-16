package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// SQLRepository persists and reads sys_workflows + sys_notifications over sqlx with parameterized
// queries. It mirrors the audit/notify repository style: identifiers are static (fixed tables),
// values always travel as `?` placeholders, and tenant_id pins every statement (multi-tenant
// invariant). It serves BOTH the hot path (ListActive, read on every record mutation) and the
// admin CRUD surface (Create/Get/List/Update/Delete) plus the in_app_notification action sink.
type SQLRepository struct {
	db  *sqlx.DB
	now func() time.Time // injectable clock; defaults to time.Now
}

// NewSQLRepository wires the workflow repository over an existing sqlx connection.
func NewSQLRepository(db *sqlx.DB) *SQLRepository {
	return &SQLRepository{db: db, now: time.Now}
}

// workflowRow is the persisted sys_workflows row. Condition/Actions are kept as raw JSON so the
// repo decodes them once into the domain Workflow (ListActive) or forwards them verbatim to the
// admin read view. Condition is nullable (NULL → match-all).
type workflowRow struct {
	ID              string           `db:"id"`
	TenantID        string           `db:"tenant_id"`
	Name            string           `db:"name"`
	EntityName      string           `db:"entity_name"`
	Trigger         string           `db:"trigger_event"`
	Condition       *json.RawMessage `db:"condition"`
	Actions         json.RawMessage  `db:"actions"`
	IsActive        bool             `db:"is_active"`
	IntervalSeconds int              `db:"interval_seconds"`
	CreatedBy       *string          `db:"created_by"`
	UpdatedBy       *string          `db:"updated_by"`
	CreatedAt       time.Time        `db:"created_at"`
	UpdatedAt       time.Time        `db:"updated_at"`
}

// ListActive returns the ACTIVE, non-deleted workflows of (tenant, entity, trigger) — the hot
// path the Event Engine runs after every record mutation. Ordered by created_at so rules fire in
// a stable, predictable order. tenant_id pins the WHERE clause unconditionally. The composite
// index idx_wf_lookup makes this an index range scan.
func (r *SQLRepository) ListActive(ctx context.Context, tenantID, entityName string, trigger Trigger) ([]Workflow, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions,
		is_active, interval_seconds, created_by, updated_by, created_at, updated_at
		FROM sys_workflows
		WHERE tenant_id = ? AND entity_name = ? AND trigger_event = ? AND is_active = 1 AND deleted_at IS NULL
		ORDER BY created_at ASC, id ASC`
	var rows []workflowRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, entityName, string(trigger)); err != nil {
		return nil, fmt.Errorf("workflow.SQLRepository.ListActive: %w", err)
	}
	out := make([]Workflow, 0, len(rows))
	for i := range rows {
		wf, err := rowToWorkflow(&rows[i])
		if err != nil {
			// A single corrupt row must not blind the engine to the rest — skip it. (Defensive:
			// config validation guards the JSON shape on write, so this is unreachable in practice.)
			continue
		}
		out = append(out, wf)
	}
	return out, nil
}

// CountActive counts the ACTIVE, non-deleted workflows of (tenant, entity, trigger), leaving out
// excludeID (the rule being edited — "" on create). It backs the specs/048 cap on active before
// rules (each one runs inside every save of the entity). tenant_id pins the WHERE clause; the
// predicate is the idx_wf_lookup prefix, so this is an index range count.
func (r *SQLRepository) CountActive(ctx context.Context, tenantID, entityName string, trigger Trigger, excludeID string) (int, error) {
	const q = `SELECT COUNT(*) FROM sys_workflows
		WHERE tenant_id = ? AND entity_name = ? AND trigger_event = ? AND is_active = 1 AND deleted_at IS NULL
		AND id <> ?`
	var n int
	if err := r.db.GetContext(ctx, &n, q, tenantID, entityName, string(trigger), excludeID); err != nil {
		return 0, fmt.Errorf("workflow.SQLRepository.CountActive: %w", err)
	}
	return n, nil
}

// rowToWorkflow decodes the JSON columns of a persisted row into the domain Workflow.
func rowToWorkflow(row *workflowRow) (Workflow, error) {
	wf := Workflow{
		ID:         row.ID,
		TenantID:   row.TenantID,
		Name:       row.Name,
		EntityName: row.EntityName,
		Trigger:    Trigger(row.Trigger),
		IsActive:   row.IsActive,
	}
	if row.Condition != nil && len(*row.Condition) > 0 && string(*row.Condition) != "null" {
		var c Condition
		if err := json.Unmarshal(*row.Condition, &c); err != nil {
			return Workflow{}, fmt.Errorf("decode condition: %w", err)
		}
		wf.Condition = &c
	}
	if len(row.Actions) > 0 && string(row.Actions) != "null" {
		if err := json.Unmarshal(row.Actions, &wf.Actions); err != nil {
			return Workflow{}, fmt.Errorf("decode actions: %w", err)
		}
	}
	return wf, nil
}

// ---------------------------------------------------------------------------------------------
// Admin CRUD (consumed by the workflow service; tenant_id pins every statement).
// ---------------------------------------------------------------------------------------------

// WorkflowRecord is the persisted-row view returned by the admin read API: it carries the raw
// JSON condition/actions plus audit columns so the handler forwards them verbatim.
type WorkflowRecord struct {
	ID         string          `json:"id"`
	TenantID   string          `json:"tenantId"`
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Trigger    string          `json:"trigger"`
	Condition  json.RawMessage `json:"condition"`
	Actions    json.RawMessage `json:"actions"`
	IsActive   bool            `json:"isActive"`
	// IntervalSeconds is the scheduled-rule cadence (0 = fire once / event trigger). Always emitted
	// so the builder can send it back untouched on a re-save (0 is a real value, not "absent").
	IntervalSeconds int       `json:"intervalSeconds"`
	CreatedBy       *string   `json:"createdBy"`
	UpdatedBy       *string   `json:"updatedBy"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	// DraftState is the draft layer's label on a record the builder reads over the overlay
	// (live | edited | new | deleted, specs/063). Not a column; empty everywhere else.
	DraftState string `json:"draftState,omitempty"`
}

// ToWorkflow decodes the record's raw JSON condition/actions into the engine's domain Workflow —
// the TIMER-1 job handler loads a rule through the admin Get (tenant-scoped, returns inactive
// rows too so the handler can no-op on them explicitly) and converts it here before firing. One
// decode body with rowToWorkflow semantics: "null"/empty condition → nil (match-all).
func (rec *WorkflowRecord) ToWorkflow() (Workflow, error) {
	wf := Workflow{
		ID:         rec.ID,
		TenantID:   rec.TenantID,
		Name:       rec.Name,
		EntityName: rec.EntityName,
		Trigger:    Trigger(rec.Trigger),
		IsActive:   rec.IsActive,
	}
	if len(rec.Condition) > 0 && string(rec.Condition) != "null" {
		var c Condition
		if err := json.Unmarshal(rec.Condition, &c); err != nil {
			return Workflow{}, fmt.Errorf("workflow.WorkflowRecord.ToWorkflow: decode condition: %w", err)
		}
		wf.Condition = &c
	}
	if len(rec.Actions) > 0 && string(rec.Actions) != "null" {
		if err := json.Unmarshal(rec.Actions, &wf.Actions); err != nil {
			return Workflow{}, fmt.Errorf("workflow.WorkflowRecord.ToWorkflow: decode actions: %w", err)
		}
	}
	return wf, nil
}

// CreateInput is the validated payload to insert a new workflow. Condition may be nil (match-all);
// Actions is the already-validated, JSON-marshalled action list. CreatedBy is the actor user id.
type CreateInput struct {
	TenantID   string
	Name       string
	EntityName string
	Trigger    Trigger
	Condition  []byte // JSON or nil
	Actions    []byte // JSON (non-nil)
	IsActive   bool
	// IntervalSeconds is the scheduled-rule cadence (0 for event triggers / fire-every-tick).
	IntervalSeconds int
	CreatedBy       string
}

// Create inserts a workflow and returns its generated id. condition NULL when nil.
func (r *SQLRepository) Create(ctx context.Context, in CreateInput) (string, error) {
	id := uuid.NewString()
	if err := InsertWithIDExec(ctx, r.db, id, in, r.now()); err != nil {
		return "", fmt.Errorf("workflow.SQLRepository.Create: %w", err)
	}
	return id, nil
}

// ErrWorkflowNotFound is returned by Get/Update/Delete for an unknown or cross-tenant id.
var ErrWorkflowNotFound = fmt.Errorf("workflow not found")

// ErrWorkflowChanged is returned by ReplaceActions when the rule's actions are no longer the ones
// the caller read (someone saved the rule in between).
var ErrWorkflowChanged = fmt.Errorf("workflow changed since it was read")

// Get returns one tenant-scoped, non-deleted workflow, or ErrWorkflowNotFound.
func (r *SQLRepository) Get(ctx context.Context, tenantID, id string) (*WorkflowRecord, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions,
		is_active, interval_seconds, created_by, updated_by, created_at, updated_at
		FROM sys_workflows
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	var row workflowRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrWorkflowNotFound
		}
		return nil, fmt.Errorf("workflow.SQLRepository.Get: %w", err)
	}
	rec := rowToRecord(&row)
	return &rec, nil
}

// List returns every tenant-scoped, non-deleted workflow newest-first (admin list page).
func (r *SQLRepository) List(ctx context.Context, tenantID string) ([]WorkflowRecord, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions,
		is_active, interval_seconds, created_by, updated_by, created_at, updated_at
		FROM sys_workflows
		WHERE tenant_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`
	var rows []workflowRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID); err != nil {
		return nil, fmt.Errorf("workflow.SQLRepository.List: %w", err)
	}
	out := make([]WorkflowRecord, len(rows))
	for i := range rows {
		out[i] = rowToRecord(&rows[i])
	}
	return out, nil
}

// UpdateInput carries the validated full-replace update of a workflow's mutable fields. Every
// field is supplied (the service re-reads + merges before calling), so the UPDATE sets them all.
type UpdateInput struct {
	Name            string
	EntityName      string
	Trigger         Trigger
	Condition       []byte // JSON or nil
	Actions         []byte // JSON (non-nil)
	IsActive        bool
	IntervalSeconds int
	UpdatedBy       string
}

// Update applies the supplied fields to a tenant-scoped, non-deleted workflow. Returns
// ErrWorkflowNotFound when no live row matches (unknown / cross-tenant / already-deleted id).
func (r *SQLRepository) Update(ctx context.Context, tenantID, id string, in UpdateInput) error {
	if err := UpdateExec(ctx, r.db, tenantID, id, in); err != nil {
		if err == ErrWorkflowNotFound {
			return err
		}
		return fmt.Errorf("workflow.SQLRepository.Update: %w", err)
	}
	return nil
}

// SetActive flips only the is_active flag of a tenant-scoped, non-deleted workflow — the pause /
// resume switch, which must not rewrite the actions (a legacy key sits in them until Doctor C10
// moves it). ErrWorkflowNotFound when no live row matches. MySQL reports 0 affected rows for a write
// that changes nothing, so a miss is re-checked instead of trusted.
func (r *SQLRepository) SetActive(ctx context.Context, tenantID, id string, active bool, updatedBy string) error {
	var by sql.NullString
	if strings.TrimSpace(updatedBy) != "" {
		by = sql.NullString{String: updatedBy, Valid: true}
	}
	const q = `UPDATE sys_workflows SET is_active = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	res, err := r.db.ExecContext(ctx, q, active, by, tenantID, id)
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.SetActive: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.SetActive: rows affected: %w", err)
	}
	if n > 0 {
		return nil
	}
	var live int
	if err := r.db.GetContext(ctx, &live, `SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		tenantID, id); err != nil {
		return fmt.Errorf("workflow.SQLRepository.SetActive: recheck: %w", err)
	}
	if live == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}

// ReplaceActions swaps a live rule's actions for next only while they still equal expect — the
// actions the caller read (JSON equality, so MySQL's own rendering of the column matches). It is the
// Doctor C10 rewrite (specs/050 S3b), which changes one action of a rule and must not overwrite a save
// that landed after its read. ErrWorkflowChanged: the rule was saved in between; ErrWorkflowNotFound:
// no live rule of the tenant has that id.
func (r *SQLRepository) ReplaceActions(ctx context.Context, tenantID, id string, expect, next []byte, updatedBy string) error {
	var by sql.NullString
	if strings.TrimSpace(updatedBy) != "" {
		by = sql.NullString{String: updatedBy, Valid: true}
	}
	const q = `UPDATE sys_workflows SET actions = ?, updated_by = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL AND actions = CAST(? AS JSON)`
	res, err := r.db.ExecContext(ctx, q, string(next), by, tenantID, id, string(expect))
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.ReplaceActions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.ReplaceActions: rows affected: %w", err)
	}
	if n == 1 {
		return nil
	}
	var live int
	if err := r.db.GetContext(ctx, &live, `SELECT COUNT(*) FROM sys_workflows WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`,
		tenantID, id); err != nil {
		return fmt.Errorf("workflow.SQLRepository.ReplaceActions: recheck: %w", err)
	}
	if live == 0 {
		return ErrWorkflowNotFound
	}
	return ErrWorkflowChanged
}

// ListDeleted returns the tenant's soft-deleted workflows. Only Doctor C10 reads them (specs/050 S3b
// review): a deleted row keeps its actions — a pre-vault key included — with no purge and no restore,
// so C10 wipes such keys from it.
func (r *SQLRepository) ListDeleted(ctx context.Context, tenantID string) ([]WorkflowRecord, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions,
		is_active, interval_seconds, created_by, updated_by, created_at, updated_at
		FROM sys_workflows
		WHERE tenant_id = ? AND deleted_at IS NOT NULL
		ORDER BY created_at DESC, id DESC`
	var rows []workflowRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID); err != nil {
		return nil, fmt.Errorf("workflow.SQLRepository.ListDeleted: %w", err)
	}
	out := make([]WorkflowRecord, len(rows))
	for i := range rows {
		out[i] = rowToRecord(&rows[i])
	}
	return out, nil
}

// GetDeleted returns one soft-deleted workflow of the tenant, or ErrWorkflowNotFound (unknown, live
// or another tenant's).
func (r *SQLRepository) GetDeleted(ctx context.Context, tenantID, id string) (*WorkflowRecord, error) {
	const q = `SELECT id, tenant_id, name, entity_name, trigger_event, ` + "`condition`" + `, actions,
		is_active, interval_seconds, created_by, updated_by, created_at, updated_at
		FROM sys_workflows
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	var row workflowRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrWorkflowNotFound
		}
		return nil, fmt.Errorf("workflow.SQLRepository.GetDeleted: %w", err)
	}
	rec := rowToRecord(&row)
	return &rec, nil
}

// ReplaceDeletedActions overwrites a soft-deleted rule's actions (Doctor C10 wiping the keys it still
// holds). No compare-and-swap: nothing else writes a deleted row (Update/ReplaceActions/Delete all pin
// deleted_at IS NULL and there is no restore), and the wipe is idempotent. ErrWorkflowNotFound: no
// deleted row of the tenant has that id (the DSN counts MATCHED rows — specs/057 — so a row that already
// held exactly next is a success, not a not-found; the caller's own re-detect makes that a no-op).
func (r *SQLRepository) ReplaceDeletedActions(ctx context.Context, tenantID, id string, next []byte) error {
	const q = `UPDATE sys_workflows SET actions = ? WHERE tenant_id = ? AND id = ? AND deleted_at IS NOT NULL`
	res, err := r.db.ExecContext(ctx, q, string(next), tenantID, id)
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.ReplaceDeletedActions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("workflow.SQLRepository.ReplaceDeletedActions: rows affected: %w", err)
	}
	if n == 0 {
		return ErrWorkflowNotFound
	}
	return nil
}

// SetInactive switches a tenant-scoped, live workflow OFF and changes nothing else ("Tắt ngay cho
// khách", specs/063 FR-017). ErrWorkflowNotFound when no live row matched.
func (r *SQLRepository) SetInactive(ctx context.Context, tenantID, id, updatedBy string) error {
	if err := SetInactiveExec(ctx, r.db, tenantID, id, updatedBy); err != nil {
		if err == ErrWorkflowNotFound {
			return err
		}
		return fmt.Errorf("workflow.SQLRepository.SetInactive: %w", err)
	}
	return nil
}

// Delete soft-deletes a tenant-scoped workflow (stamps deleted_at), so a removed rule keeps its
// history and never fires again. Returns ErrWorkflowNotFound when no live row matched.
func (r *SQLRepository) Delete(ctx context.Context, tenantID, id string) error {
	if err := SoftDeleteExec(ctx, r.db, tenantID, id, r.now()); err != nil {
		if err == ErrWorkflowNotFound {
			return err
		}
		return fmt.Errorf("workflow.SQLRepository.Delete: %w", err)
	}
	return nil
}

// rowToRecord projects a persisted row to the admin read view (raw JSON forwarded verbatim).
func rowToRecord(row *workflowRow) WorkflowRecord {
	cond := json.RawMessage("null")
	if row.Condition != nil && len(*row.Condition) > 0 {
		cond = *row.Condition
	}
	acts := row.Actions
	if len(acts) == 0 {
		acts = json.RawMessage("null")
	}
	return WorkflowRecord{
		ID:              row.ID,
		TenantID:        row.TenantID,
		Name:            row.Name,
		EntityName:      row.EntityName,
		Trigger:         row.Trigger,
		Condition:       cond,
		Actions:         acts,
		IsActive:        row.IsActive,
		IntervalSeconds: row.IntervalSeconds,
		CreatedBy:       row.CreatedBy,
		UpdatedBy:       row.UpdatedBy,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

// ---------------------------------------------------------------------------------------------
// in_app_notification sink (sys_notifications).
// ---------------------------------------------------------------------------------------------

// Notification is the row the in_app_notification action writes. TenantID pins it; UserID is the
// recipient. EntityName/RecordID/WorkflowID link it back to the source (all optional).
type Notification struct {
	TenantID   string
	UserID     string
	Title      string
	Message    string
	EntityName string
	RecordID   string
	WorkflowID string
	// Type drives the inbox icon + tone in the FE (NotificationBell). See the same field on
	// bpm.Notification for why this was NULL for every row until MOT-XUONG. Empty stays NULL.
	Type string
}

// InsertNotification appends one row to sys_notifications and returns its generated id. Empty
// optional link fields are stored NULL. tenant_id + user_id are mandatory (NOT NULL columns).
func (r *SQLRepository) InsertNotification(ctx context.Context, n Notification) (string, error) {
	id := uuid.NewString()
	const q = `INSERT INTO sys_notifications
		(id, tenant_id, user_id, title, message, type, entity_name, record_id, workflow_id, is_read, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`
	if _, err := r.db.ExecContext(ctx, q,
		id, n.TenantID, n.UserID,
		nullIfEmpty(n.Title), n.Message, nullIfEmpty(n.Type),
		nullIfEmpty(n.EntityName), nullIfEmpty(n.RecordID), nullIfEmpty(n.WorkflowID),
		r.now().UTC(),
	); err != nil {
		return "", fmt.Errorf("workflow.SQLRepository.InsertNotification: %w", err)
	}
	return id, nil
}

// nullIfEmpty maps a blank string to a SQL NULL so optional columns stay NULL rather than "".
func nullIfEmpty(s string) sql.NullString {
	if strings.TrimSpace(s) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
