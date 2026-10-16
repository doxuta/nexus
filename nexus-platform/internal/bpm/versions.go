package bpm

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// Process versions, draft and publish (specs/043 S10, Migration C).
//
// A process has ONE active definition — the columns of its sys_approval_processes row, which is the
// denormalised copy of the active version, so the engine's hot read and the pack export are
// untouched — plus an optional DRAFT (the unpublished edit, never read by the engine: a draft
// starts nothing) and the append-only history of every definition it has been published as
// (sys_approval_process_versions, immutable rows).
//
// Three ways a definition becomes active, all in one transaction with a lock on the process row:
//   - CreateProcess: version 1;
//   - UpdateProcess: the legacy "save = publish now" (admin builder, AI Builder, pack install);
//   - Publish: the draft becomes the next version.

var (
	// ErrDraftNotFound: the process has no draft to discard.
	ErrDraftNotFound = errors.New("approval process has no draft")
	// ErrNoDraft: Publish with nothing to publish.
	ErrNoDraft = errors.New("approval process has no draft to publish")
	// ErrDraftStale: the active version moved (a legacy save, an AI apply, a pack install) after
	// the draft was saved. Publishing it would silently put the OLD base back over that change —
	// the admin re-saves the draft (which rebases it) instead.
	ErrDraftStale = errors.New("approval process draft is based on an older version")
	// ErrVersionNotFound: an unknown version number (or another tenant's process).
	ErrVersionNotFound = errors.New("approval process version not found")
)

// DefinitionDoc is the JSON stored per version (and, with DraftDoc's extras, in the draft): the
// whole of what a publish changes. Raw members are forwarded verbatim; absent = JSON null.
type DefinitionDoc struct {
	Name       string          `json:"name"`
	EntityName string          `json:"entityName"`
	Trigger    string          `json:"trigger"`
	Criteria   json.RawMessage `json:"criteria"`
	Steps      json.RawMessage `json:"steps"`
	Graph      json.RawMessage `json:"graph"`
	Outcome    json.RawMessage `json:"outcome"`
	// Options (S6a) is omitted when absent so a version written before it existed still compares
	// equal to the same definition read back (sameDefinition) — no spurious new version.
	Options json.RawMessage `json:"options,omitempty"`
}

// DraftDoc is a DefinitionDoc waiting to be published. IsActive is applied at publish when set (it
// is process state, not versioned). BaseVersion is the active version the draft was last saved
// over (ErrDraftStale).
type DraftDoc struct {
	DefinitionDoc
	IsActive    *bool `json:"isActive,omitempty"`
	BaseVersion int   `json:"baseVersion"`
}

// VersionRecord is one published version. Definition is absent from the list read.
type VersionRecord struct {
	ID         string          `json:"id"`
	ProcessID  string          `json:"processId"`
	Version    int             `json:"version"`
	Definition json.RawMessage `json:"definition,omitempty"`
	Note       *string         `json:"note"`
	CreatedBy  *string         `json:"createdBy"`
	CreatedAt  time.Time       `json:"createdAt"`
}

type versionRow struct {
	ID         string           `db:"id"`
	ProcessID  string           `db:"process_id"`
	Version    int              `db:"version"`
	Definition *json.RawMessage `db:"definition"`
	Note       *string          `db:"note"`
	CreatedBy  *string          `db:"created_by"`
	CreatedAt  time.Time        `db:"created_at"`
}

func (v versionRow) record() VersionRecord {
	rec := VersionRecord{ID: v.ID, ProcessID: v.ProcessID, Version: v.Version, Note: v.Note, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
	if v.Definition != nil {
		rec.Definition = *v.Definition
	}
	return rec
}

// raw maps an empty byte slice to nil so it marshals as JSON null instead of failing.
func raw(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

func marshalDefinition(d DefinitionDoc) ([]byte, error) {
	d.Criteria, d.Steps, d.Graph, d.Outcome, d.Options = raw(d.Criteria), raw(d.Steps), raw(d.Graph), raw(d.Outcome), raw(d.Options)
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("bpm.marshalDefinition: %w", err)
	}
	return b, nil
}

// definitionOfRow is the ACTIVE definition a process row holds.
func definitionOfRow(row *processRow) DefinitionDoc {
	d := DefinitionDoc{Name: row.Name, EntityName: row.EntityName, Trigger: row.Trigger, Steps: row.Steps}
	if row.Criteria != nil {
		d.Criteria = *row.Criteria
	}
	if row.Graph != nil {
		d.Graph = *row.Graph
	}
	if row.Outcome != nil {
		d.Outcome = *row.Outcome
	}
	if row.Options != nil && len(*row.Options) > 0 && string(*row.Options) != "null" {
		d.Options = *row.Options
	}
	return d
}

// sameDefinition compares two definitions as JSON VALUES (key order, spacing and MySQL's JSON
// re-serialisation do not count).
func sameDefinition(a, b DefinitionDoc) bool {
	decode := func(d DefinitionDoc) (any, bool) {
		raw, err := marshalDefinition(d)
		if err != nil {
			return nil, false
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return nil, false
		}
		return v, true
	}
	va, oka := decode(a)
	vb, okb := decode(b)
	return oka && okb && reflect.DeepEqual(va, vb)
}

// lockProcess reads a live process row FOR UPDATE inside tx, or ErrProcessNotFound.
func lockProcess(ctx context.Context, tx *sqlx.Tx, tenantID, id string) (*processRow, error) {
	var row processRow
	q := `SELECT ` + processColumns + ` FROM sys_approval_processes WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL FOR UPDATE`
	if err := tx.GetContext(ctx, &row, q, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrProcessNotFound
		}
		return nil, fmt.Errorf("bpm.lockProcess: %w", err)
	}
	return &row, nil
}

// insertVersion writes one immutable version row.
func insertVersion(ctx context.Context, tx *sqlx.Tx, tenantID, processID string, version int, definition []byte, note, by string, at time.Time) error {
	const q = `INSERT INTO sys_approval_process_versions
		(id, tenant_id, process_id, version, definition, note, created_by, created_at, updated_by, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q, uuid.NewString(), tenantID, processID, version, string(definition),
		nullIfEmpty(note), nullIfEmpty(by), at, nullIfEmpty(by), at); err != nil {
		return fmt.Errorf("bpm.insertVersion: %w", err)
	}
	return nil
}

// ensureCurrentVersion writes the version row of the process's ACTIVE definition when it has none —
// a row inserted by hand or restored without its history. Without it the first publish over such a
// process would leave a history that starts at version 2.
func ensureCurrentVersion(ctx context.Context, tx *sqlx.Tx, row *processRow, at time.Time) error {
	var n int
	const probe = `SELECT COUNT(*) FROM sys_approval_process_versions WHERE tenant_id = ? AND process_id = ? AND version = ?`
	if err := tx.GetContext(ctx, &n, probe, row.TenantID, row.ID, row.Version); err != nil {
		return fmt.Errorf("bpm.ensureCurrentVersion: %w", err)
	}
	if n > 0 {
		return nil
	}
	doc, err := marshalDefinition(definitionOfRow(row))
	if err != nil {
		return err
	}
	by := ""
	if row.UpdatedBy != nil {
		by = *row.UpdatedBy
	}
	return insertVersion(ctx, tx, row.TenantID, row.ID, row.Version, doc, "", by, at)
}

// SaveDraftInput is the validated payload of PUT .../draft.
type SaveDraftInput struct {
	Doc      DefinitionDoc
	IsActive *bool
	By       string
	// Force: a linear-only draft may replace a stored graph (same guard as UpdateProcess, FR-004).
	Force bool
}

// SaveDraft stores the draft of a tenant-scoped process, replacing the previous one, and stamps the
// active version it is based on. ErrProcessNotFound / ErrProcessGraphOverwrite as UpdateProcess.
func (r *SQLRepository) SaveDraft(ctx context.Context, tenantID, id string, in SaveDraftInput) error {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.SaveDraft: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	row, err := lockProcess(ctx, tx, tenantID, id)
	if err != nil {
		return err
	}
	if erasesV2(row, in.Doc.Graph, in.Doc.Options) && !in.Force {
		return ErrProcessGraphOverwrite
	}
	in.Doc.Criteria, in.Doc.Steps, in.Doc.Graph, in.Doc.Outcome, in.Doc.Options = raw(in.Doc.Criteria), raw(in.Doc.Steps), raw(in.Doc.Graph), raw(in.Doc.Outcome), raw(in.Doc.Options)
	doc, err := json.Marshal(DraftDoc{DefinitionDoc: in.Doc, IsActive: in.IsActive, BaseVersion: row.Version})
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.SaveDraft: marshal: %w", err)
	}
	const q = `UPDATE sys_approval_processes SET draft = ?, draft_updated_by = ?, draft_updated_at = ?
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, q, string(doc), nullIfEmpty(in.By), r.now().UTC(), tenantID, id); err != nil {
		return fmt.Errorf("bpm.SQLRepository.SaveDraft: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bpm.SQLRepository.SaveDraft: commit: %w", err)
	}
	return nil
}

// DiscardDraft drops the draft of a tenant-scoped process. ErrProcessNotFound for an unknown id,
// ErrDraftNotFound when there is nothing to drop. The active version is untouched.
func (r *SQLRepository) DiscardDraft(ctx context.Context, tenantID, id string) error {
	const q = `UPDATE sys_approval_processes SET draft = NULL, draft_updated_by = NULL, draft_updated_at = NULL
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL AND draft IS NOT NULL`
	res, err := r.db.ExecContext(ctx, q, tenantID, id)
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.DiscardDraft: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("bpm.SQLRepository.DiscardDraft: rows affected: %w", err)
	}
	if n > 0 {
		return nil
	}
	var one int
	const probe = `SELECT 1 FROM sys_approval_processes WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if err := r.db.GetContext(ctx, &one, probe, tenantID, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProcessNotFound
		}
		return fmt.Errorf("bpm.SQLRepository.DiscardDraft: probe: %w", err)
	}
	return ErrDraftNotFound
}

// Publish makes the draft the active definition as the next version, clears the draft, and returns
// the new version number. The process row, the version row and the draft clear are ONE transaction
// under a lock on the process row: two publishes cannot take the same number, and an instance armed
// meanwhile pins either the old or the new version, never a mix. ErrProcessNotFound, ErrNoDraft,
// ErrDraftStale.
func (r *SQLRepository) Publish(ctx context.Context, tenantID, id, note, by string) (int, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	row, err := lockProcess(ctx, tx, tenantID, id)
	if err != nil {
		return 0, err
	}
	if row.Draft == nil || len(*row.Draft) == 0 || string(*row.Draft) == "null" {
		return 0, ErrNoDraft
	}
	var d DraftDoc
	if err := json.Unmarshal(*row.Draft, &d); err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: decode draft: %w", err)
	}
	if d.BaseVersion != row.Version {
		return 0, ErrDraftStale
	}
	now := r.now().UTC()
	if err := ensureCurrentVersion(ctx, tx, row, now); err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: %w", err)
	}
	version := row.Version + 1
	doc, err := marshalDefinition(d.DefinitionDoc)
	if err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: %w", err)
	}
	if err := insertVersion(ctx, tx, tenantID, id, version, doc, note, by, now); err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: %w", err)
	}
	steps := "[]" // the column is NOT NULL: a graph definition keeps its steps empty
	if s := raw(d.Steps); len(s) > 0 && string(s) != "null" {
		steps = string(s)
	}
	var active any // COALESCE keeps the process's own switch when the draft did not carry one
	if d.IsActive != nil {
		active = *d.IsActive
	}
	const q = `UPDATE sys_approval_processes
		SET name = ?, entity_name = ?, trigger_event = ?, criteria = ?, steps = ?, graph = ?, outcome = ?, options = ?,
		    is_active = COALESCE(?, is_active), version = ?, updated_by = ?,
		    draft = NULL, draft_updated_by = NULL, draft_updated_at = NULL
		WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if _, err := tx.ExecContext(ctx, q,
		d.Name, d.EntityName, d.Trigger, nullJSON(d.Criteria), steps, nullJSON(d.Graph), nullJSON(d.Outcome), nullJSON(raw(d.Options)),
		active, version, nullIfEmpty(by), tenantID, id,
	); err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("bpm.SQLRepository.Publish: commit: %w", err)
	}
	return version, nil
}

// ListVersions returns the published versions of a tenant-scoped process, newest first, WITHOUT
// their definitions (GetVersion carries them). An unknown process yields none — the service
// resolves the process first.
func (r *SQLRepository) ListVersions(ctx context.Context, tenantID, processID string) ([]VersionRecord, error) {
	const q = `SELECT id, process_id, version, note, created_by, created_at
		FROM sys_approval_process_versions
		WHERE tenant_id = ? AND process_id = ? AND deleted_at IS NULL
		ORDER BY version DESC`
	var rows []versionRow
	if err := r.db.SelectContext(ctx, &rows, q, tenantID, processID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.ListVersions: %w", err)
	}
	out := make([]VersionRecord, len(rows))
	for i := range rows {
		out[i] = rows[i].record()
	}
	return out, nil
}

// GetVersion returns one version of a tenant-scoped process with its definition, or
// ErrVersionNotFound.
func (r *SQLRepository) GetVersion(ctx context.Context, tenantID, processID string, version int) (*VersionRecord, error) {
	const q = `SELECT id, process_id, version, definition, note, created_by, created_at
		FROM sys_approval_process_versions
		WHERE tenant_id = ? AND process_id = ? AND version = ? AND deleted_at IS NULL`
	var row versionRow
	if err := r.db.GetContext(ctx, &row, q, tenantID, processID, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrVersionNotFound
		}
		return nil, fmt.Errorf("bpm.SQLRepository.GetVersion: %w", err)
	}
	rec := row.record()
	return &rec, nil
}

// PublishedNodeIDs is the set of step ids that some PUBLISHED version of the process has used — the
// ids a new step must not be handed (NormalizeStepsAvoiding): an id means "the same step" across
// versions, and an instance is mapped between versions by it (S11).
func (r *SQLRepository) PublishedNodeIDs(ctx context.Context, tenantID, processID string) (map[string]bool, error) {
	const q = `SELECT definition FROM sys_approval_process_versions
		WHERE tenant_id = ? AND process_id = ? AND deleted_at IS NULL`
	var defs []json.RawMessage
	if err := r.db.SelectContext(ctx, &defs, q, tenantID, processID); err != nil {
		return nil, fmt.Errorf("bpm.SQLRepository.PublishedNodeIDs: %w", err)
	}
	ids := map[string]bool{}
	for _, d := range defs {
		var doc struct {
			Steps []Step `json:"steps"`
			Graph *Graph `json:"graph"`
		}
		if err := json.Unmarshal(d, &doc); err != nil {
			return nil, fmt.Errorf("bpm.SQLRepository.PublishedNodeIDs: decode: %w", err)
		}
		for _, s := range NormalizeSteps(doc.Steps) { // a legacy v1 reads as s1..sN, like every reader
			ids[s.ID] = true
		}
		if doc.Graph != nil {
			for _, n := range doc.Graph.normalized().Nodes {
				ids[n.ID] = true
			}
		}
	}
	return ids, nil
}
