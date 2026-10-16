package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/metaversion"
)

// DocTypeEntity is the draft-layer document type of a table (specs/063 S7-9). Its key is the
// sys_entities id and the only draft it ever carries is op=delete: a table is created live (hidden from
// the customer by the schema cutoff, S7-8) and a delete is deferred to Publish.
const DocTypeEntity = "entity"

// entityDoc is the canonical form of a table for the change list and the version history. Nothing in
// it is editable through a draft, so a delete draft's base hash only moves when the table is renamed
// or relabelled live (a real conflict: the builder deleted a table that is no longer the one they saw).
type entityDoc struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// EntityDocAdapter binds the "entity" document type to sys_entities.
type EntityDocAdapter struct{}

// NewEntityDocAdapter returns the adapter (stateless).
func NewEntityDocAdapter() *EntityDocAdapter { return &EntityDocAdapter{} }

// Type implements metaversion.DocAdapter.
func (*EntityDocAdapter) Type() string { return DocTypeEntity }

type liveEntityRow struct {
	Name        string         `db:"name"`
	Label       string         `db:"label"`
	Description sql.NullString `db:"description"`
}

// LoadLive implements metaversion.DocAdapter.
func (*EntityDocAdapter) LoadLive(ctx context.Context, q sqlx.QueryerContext, tenantID, key string, forUpdate bool) (json.RawMessage, bool, error) {
	query := `SELECT name, label, description FROM sys_entities WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var row liveEntityRow
	if err := sqlx.GetContext(ctx, q, &row, query, tenantID, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("services.EntityDocAdapter.LoadLive: %w", err)
	}
	b, err := json.Marshal(entityDoc{Name: row.Name, Label: row.Label, Description: row.Description.String})
	if err != nil {
		return nil, false, fmt.Errorf("services.EntityDocAdapter.LoadLive: %w", err)
	}
	return b, true, nil
}

// ListLiveKeys implements metaversion.DocAdapter (the baseline / late snapshot of every live table).
func (*EntityDocAdapter) ListLiveKeys(ctx context.Context, q sqlx.QueryerContext, tenantID string) ([]metaversion.DocRef, error) {
	var rows []struct {
		ID    string `db:"id"`
		Name  string `db:"name"`
		Label string `db:"label"`
	}
	if err := sqlx.SelectContext(ctx, q, &rows, `SELECT id, name, label FROM sys_entities WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY name`, tenantID); err != nil {
		return nil, fmt.Errorf("services.EntityDocAdapter.ListLiveKeys: %w", err)
	}
	out := make([]metaversion.DocRef, len(rows))
	for i, r := range rows {
		out[i] = metaversion.DocRef{Key: r.ID, Label: r.Label, EntityName: r.Name}
	}
	return out, nil
}

// ApplyTx implements metaversion.DocAdapter. A table delete is NOT applied on the publish tx: the
// documents commit first and PublishService then runs EntityService.SoftDeleteEntity (FR-065), which
// sweeps the shared obj_* table with the version's instant. Nothing else is drafted for a table.
func (*EntityDocAdapter) ApplyTx(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	switch {
	case c.Op == metaversion.OpDelete:
		return nil
	case c.Op == metaversion.OpUpsert && len(c.Restore) > 0:
		// S7-10b: a rollback brings a deleted table back, with the records its delete trashed.
		if err := restoreEntity(ctx, tx, tenantID, c); err != nil {
			return fmt.Errorf("services.EntityDocAdapter.ApplyTx: %w", err)
		}
		return nil
	}
	return fmt.Errorf("services.EntityDocAdapter.ApplyTx: op %q on entity %s is not supported", c.Op, c.DocKey)
}

// ValidateFinal implements metaversion.DocAdapter.
func (*EntityDocAdapter) ValidateFinal(context.Context, metaversion.FinalState) []metaversion.Problem {
	return nil
}

// Redact implements metaversion.DocAdapter: a table carries no secret.
func (*EntityDocAdapter) Redact(doc json.RawMessage) json.RawMessage { return doc }

// Entities implements metaversion.DocAdapter.
func (*EntityDocAdapter) Entities(doc json.RawMessage) []string {
	var d entityDoc
	if len(doc) == 0 || json.Unmarshal(doc, &d) != nil || d.Name == "" {
		return nil
	}
	return []string{d.Name}
}

var _ metaversion.DocAdapter = (*EntityDocAdapter)(nil)
