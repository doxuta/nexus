package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// DocTypeField is the draft-layer document type of a field's editable attributes (specs/063 S7-8).
// Its key is the sys_fields id. A field is only ever drafted AFTER it exists live: a new field is
// created live (hidden from the customer by the schema cutoff), so the draft carries edits to it.
const DocTypeField = "field"

// fieldDoc is the canonical form of a field's editable attributes: exactly what
// metadata.UpdateFieldMutableExec writes, minus options.indexed. The indexed flag is NOT part of the
// document: it is applied at once through the existing DDL path (FR-063), so a draft saved identical
// to the live row hashes identical to it. entityId is immutable and lets Publish drop the right
// MetaCache entry.
type fieldDoc struct {
	EntityID     string          `json:"entityId"`
	Label        string          `json:"label"`
	IsRequired   bool            `json:"isRequired"`
	DefaultValue string          `json:"defaultValue"`
	Options      json.RawMessage `json:"options"`
	FieldOrder   int             `json:"fieldOrder"`
	Description  string          `json:"description"`
}

func (d fieldDoc) marshal() (json.RawMessage, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("services.fieldDoc.marshal: %w", err)
	}
	return b, nil
}

// optionsMap parses a stored options JSON string (nil / empty -> empty map).
func optionsMap(opts *string) (map[string]any, error) {
	m := map[string]any{}
	if opts == nil || *opts == "" || *opts == "null" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(*opts), &m); err != nil {
		return nil, fmt.Errorf("services.optionsMap: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// optionsWithoutIndexed is the draft form of options: the stored object minus "indexed"; an empty
// object is null, so "no options" hashes the same however the live column spells it.
func optionsWithoutIndexed(opts *string) (json.RawMessage, error) {
	m, err := optionsMap(opts)
	if err != nil {
		return nil, err
	}
	delete(m, "indexed")
	if len(m) == 0 {
		return json.RawMessage("null"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("services.optionsWithoutIndexed: %w", err)
	}
	return b, nil
}

// optionsFromDoc rebuilds the stored options string from a draft's options, carrying over whatever
// "indexed" value the LIVE options hold (the draft never owns it).
func optionsFromDoc(docOpts json.RawMessage, liveOpts *string) (*string, error) {
	m := map[string]any{}
	if len(docOpts) > 0 && string(docOpts) != "null" {
		if err := json.Unmarshal(docOpts, &m); err != nil {
			return nil, fmt.Errorf("services.optionsFromDoc: %w", err)
		}
	}
	lm, err := optionsMap(liveOpts)
	if err != nil {
		return nil, err
	}
	if v, ok := lm["indexed"]; ok {
		m["indexed"] = v
	}
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("services.optionsFromDoc: %w", err)
	}
	s := string(b)
	return &s, nil
}

// fieldDocOf is the canonical document of a field row.
func fieldDocOf(f *metadata.FieldDefinition) (fieldDoc, error) {
	opts, err := optionsWithoutIndexed(f.Options)
	if err != nil {
		return fieldDoc{}, err
	}
	return fieldDoc{EntityID: f.EntityID, Label: f.Label, IsRequired: f.IsRequired, DefaultValue: f.DefaultVal,
		Options: opts, FieldOrder: f.FieldOrder, Description: f.Description}, nil
}

// applyFieldDoc lays a draft over a live field row (the builder's overlay and the publish both use it).
func applyFieldDoc(live metadata.FieldDefinition, d fieldDoc) (metadata.FieldDefinition, error) {
	opts, err := optionsFromDoc(d.Options, live.Options)
	if err != nil {
		return live, err
	}
	live.Label, live.IsRequired, live.DefaultVal = d.Label, d.IsRequired, d.DefaultValue
	live.Options, live.FieldOrder, live.Description = opts, d.FieldOrder, d.Description
	return live, nil
}

// FieldDocAdapter binds the "field" document type to sys_fields.
type FieldDocAdapter struct{}

// NewFieldDocAdapter returns the adapter (stateless).
func NewFieldDocAdapter() *FieldDocAdapter { return &FieldDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*FieldDocAdapter) Type() string { return DocTypeField }

type liveFieldRow struct {
	ID          string         `db:"id"`
	EntityID    string         `db:"entity_id"`
	Label       string         `db:"label"`
	IsRequired  bool           `db:"is_required"`
	DefaultVal  sql.NullString `db:"default_val"`
	Options     sql.NullString `db:"options"`
	FieldOrder  int            `db:"field_order"`
	Description sql.NullString `db:"description"`
}

const liveFieldCols = `id, entity_id, label, is_required, default_val, CAST(options AS CHAR) AS options, field_order, description`

func (r liveFieldRow) definition() metadata.FieldDefinition {
	f := metadata.FieldDefinition{ID: r.ID, EntityID: r.EntityID, Label: r.Label, IsRequired: r.IsRequired,
		DefaultVal: r.DefaultVal.String, FieldOrder: r.FieldOrder, Description: r.Description.String}
	if r.Options.Valid {
		o := r.Options.String
		f.Options = &o
	}
	return f
}

// LoadLive implements metaversion.DocAdapter.
func (*FieldDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	query := `SELECT ` + liveFieldCols + ` FROM sys_fields WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var row liveFieldRow
	if err := sqlx.GetContext(ctx, q, &row, query, tenantID, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.FieldDocAdapter.LoadLive: %w", err)
	}
	f := row.definition()
	d, err := fieldDocOf(&f)
	if err != nil {
		return nil, false, fmt.Errorf("services.FieldDocAdapter.LoadLive: %w", err)
	}
	b, err := d.marshal()
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter (the baseline / late snapshot of every live field).
func (*FieldDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		ID         string `db:"id"`
		Label      string `db:"label"`
		EntityName string `db:"entity_name"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT f.id AS id, f.label AS label, e.name AS entity_name
		FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id AND e.deleted_at IS NULL
		WHERE f.tenant_id = ? AND f.deleted_at IS NULL ORDER BY e.name, f.field_order, f.name`, tenantID); err != nil {
		return nil, fmt.Errorf("services.FieldDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: r.ID, Label: r.Label, EntityName: r.EntityName}
	}
	return out, nil
}

// ApplyTx implements metaversion.DocAdapter: the draft goes through the same statement the live edit
// uses, on the publish transaction. The live options.indexed (applied at once by EditField) is kept.
// A delete (S7-9) is NOT applied here: the documents commit first and PublishService then runs
// FieldService.DeleteField through the existing path (FR-065), which owns the pivot, relationship and
// cache cleanup.
func (*FieldDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	if c.Op == metaversion.OpDelete {
		return nil
	}
	if c.Op != metaversion.OpUpsert {
		return fmt.Errorf("services.FieldDocAdapter.ApplyTx: op %q on field %s is not supported", c.Op, c.DocKey)
	}
	var d fieldDoc
	if err := json.Unmarshal(c.After, &d); err != nil {
		return fmt.Errorf("services.FieldDocAdapter.ApplyTx: decode draft %s: %w", c.DocKey, err)
	}
	// S7-10a: a rollback that brings a deleted field back un-deletes it first, then the common path below
	// puts the target's attributes on it and the unique pivot is rebuilt.
	var restored *schemaRestoreMeta
	if len(c.Restore) > 0 {
		var err error
		if restored, err = restoreFieldRows(ctx, tx, tenantID, c); err != nil {
			return fmt.Errorf("services.FieldDocAdapter.ApplyTx: %w", err)
		}
	}
	var row liveFieldRow
	if err := tx.GetContext(ctx, &row, `SELECT `+liveFieldCols+` FROM sys_fields WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL FOR UPDATE`,
		tenantID, c.DocKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("services.FieldDocAdapter.ApplyTx: field %s: %w", c.DocKey, metadata.ErrFieldNotFound)
		}
		return fmt.Errorf("services.FieldDocAdapter.ApplyTx: read live %s: %w", c.DocKey, err)
	}
	merged, err := applyFieldDoc(row.definition(), d)
	if err != nil {
		return fmt.Errorf("services.FieldDocAdapter.ApplyTx: %s: %w", c.DocKey, err)
	}
	// MySQL counts CHANGED rows: right after restoreFieldRows (same second, same values) the UPDATE matches
	// 0 rows although the row is live and locked above, so that is not "not found" on the restore path.
	if err := metadata.UpdateFieldMutableExec(ctx, tx, tenantID, &merged); err != nil && !(restored != nil && errors.Is(err, metadata.ErrFieldNotFound)) {
		return fmt.Errorf("services.FieldDocAdapter.ApplyTx: %s: %w", c.DocKey, err)
	}
	if restored != nil {
		if err := restoreFieldPivot(ctx, tx, tenantID, restored); err != nil {
			return fmt.Errorf("services.FieldDocAdapter.ApplyTx: %s: %w", c.DocKey, err)
		}
	}
	return nil
}

// ValidateFinal implements metaversion.DocAdapter. A field draft is a set of plain attributes the
// service already validated when it was saved; nothing crosses documents.
func (*FieldDocAdapter) ValidateFinal(context.Context, metaversion.FinalState) []metaversion.Problem {
	return nil
}

// Redact implements metaversion.DocAdapter: a field carries no secret.
func (*FieldDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter. It returns the entity ID (not the name): the field
// invalidator registered in main.go drops the MetaCache field list, which is keyed by entity id.
func (*FieldDocAdapter) Entities(doc json.RawMessage) []string {
	var d fieldDoc
	if len(doc) == 0 || json.Unmarshal(doc, &d) != nil || d.EntityID == "" {
		return nil
	}
	return []string{d.EntityID}
}

var _ metaversion.DocAdapter = (*FieldDocAdapter)(nil)
