// schema_store.go — the DOCTOR-SCHEMA detect queries (../specs/008-nexus-doctor/spec.md §2.2
// A1/A2a/A5/A6/B4): the declared side comes from tenant-pinned sys_* reads, the actual side
// from information_schema diffs (the dynrepo.ColumnExists/TableExists pattern, batched).
// All read-only; every list is LIMIT-bounded by the caller. information_schema itself is
// server-global (no tenant column), but every table name probed here derives from THIS
// tenant's own metadata rows, so no cross-tenant name ever enters a query.
package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// PhysicalFieldRow is a sys_fields row (joined to its active entity) that must be backed by a
// physical generated column on the shared obj_* table: unique fields, relation fields
// (FK-on-child `<f>_id`), and D1 indexed fields. The A1/A2a detectors diff these rows against
// information_schema.COLUMNS.
type PhysicalFieldRow struct {
	FieldID    string `db:"id"`
	EntityName string `db:"entity_name"`
	FieldName  string `db:"name"`
	FieldType  string `db:"field_type"`
	IsUnique   int    `db:"is_unique"`
	Options    string `db:"options"` // raw sys_fields.options JSON ("" when NULL)
}

// EntityRef is an active sys_entities row projected for the A5/A6 detectors.
type EntityRef struct {
	ID   string `db:"id"`
	Name string `db:"name"`
}

// NNRelationshipRow is an active N-N sys_relationships row projected for the B4 detector.
type NNRelationshipRow struct {
	ID               string `db:"id"`
	Name             string `db:"name"`
	ParentEntityName string `db:"parent_entity_name"`
	ChildEntityName  string `db:"child_entity_name"`
	JoinTableName    string `db:"join_table_name"`
}

// TableColumn is one information_schema.COLUMNS row: the column's presence backs A1/A5, its
// GENERATION_EXPRESSION ("" for a plain column) backs the A2a guard check (the same
// `NOT LIKE '%deleted_at%'` predicate migration 000015 swept with, evaluated in Go).
type TableColumn struct {
	TableName            string `db:"table_name"`
	ColumnName           string `db:"column_name"`
	GenerationExpression string `db:"generation_expression"`
}

// IndexColumn is one information_schema.STATISTICS row (one row per column of each index,
// ordered by SEQ_IN_INDEX) — the A5 detector reassembles them into ordered column lists to
// compare against ddl.MandatoryIndexes (name AND shape, the RM-47 class).
type IndexColumn struct {
	TableName  string `db:"table_name"`
	IndexName  string `db:"index_name"`
	SeqInIndex int    `db:"seq_in_index"`
	ColumnName string `db:"column_name"`
}

// physicalFieldWhere selects the active fields (of active entities, tenant-pinned) that must
// own a physical column. The JSON probe on options is string-compared through JSON_UNQUOTE so
// a boolean true matches regardless of driver/JSON comparison quirks; sys_fields is a small
// registry table, never a hot record path (B2 discipline does not apply).
const physicalFieldWhere = `
	FROM sys_fields f
	JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
	WHERE f.tenant_id = ?
	  AND f.deleted_at IS NULL
	  AND e.deleted_at IS NULL
	  AND (f.is_unique = 1
	       OR f.field_type = 'relation'
	       OR COALESCE(JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.indexed')), '') = 'true')`

// ListPhysicalFieldRows returns up to limit active fields that must be backed by a physical
// generated column (A1/A2a scan window). Read-only.
func (s *SQLStore) ListPhysicalFieldRows(ctx context.Context, tenantID string, limit int) ([]PhysicalFieldRow, error) {
	const q = `SELECT f.id, e.name AS entity_name, f.name, f.field_type, f.is_unique,
	       COALESCE(f.options, '') AS options` + physicalFieldWhere + `
	ORDER BY e.name, f.name LIMIT ?`
	var rows []PhysicalFieldRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListPhysicalFieldRows: %w", err)
	}
	return rows, nil
}

// GetPhysicalFieldRow re-resolves a single A1/A2a item by sys_fields id (the per-item
// re-detect). Returns (nil, nil) when the field no longer qualifies — deleted, entity gone,
// or no longer unique/relation/indexed — i.e. the finding is stale. Read-only.
func (s *SQLStore) GetPhysicalFieldRow(ctx context.Context, tenantID, fieldID string) (*PhysicalFieldRow, error) {
	const q = `SELECT f.id, e.name AS entity_name, f.name, f.field_type, f.is_unique,
	       COALESCE(f.options, '') AS options` + physicalFieldWhere + ` AND f.id = ?`
	var row PhysicalFieldRow
	if err := s.db.GetContext(ctx, &row, q, tenantID, fieldID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("doctor.GetPhysicalFieldRow: %w", err)
	}
	return &row, nil
}

// ListActiveEntities returns up to limit active sys_entities rows for the tenant (A5/A6 scan
// window). Read-only.
func (s *SQLStore) ListActiveEntities(ctx context.Context, tenantID string, limit int) ([]EntityRef, error) {
	const q = `SELECT id, name FROM sys_entities
	WHERE tenant_id = ? AND deleted_at IS NULL
	ORDER BY name LIMIT ?`
	var rows []EntityRef
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListActiveEntities: %w", err)
	}
	return rows, nil
}

// nnRelationshipWhere selects active N-N registry rows that own a join table (B4's class).
const nnRelationshipWhere = `
	FROM sys_relationships r
	WHERE r.tenant_id = ?
	  AND r.deleted_at IS NULL
	  AND r.relationship_type = 'N-N'
	  AND COALESCE(r.join_table_name, '') <> ''`

// ListActiveNNRelationships returns up to limit active N-N registry rows (B4 scan window).
// Read-only.
func (s *SQLStore) ListActiveNNRelationships(ctx context.Context, tenantID string, limit int) ([]NNRelationshipRow, error) {
	const q = `SELECT r.id, r.name, r.parent_entity_name, r.child_entity_name,
	       COALESCE(r.join_table_name, '') AS join_table_name` + nnRelationshipWhere + `
	ORDER BY r.id LIMIT ?`
	var rows []NNRelationshipRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListActiveNNRelationships: %w", err)
	}
	return rows, nil
}

// GetActiveNNRelationship re-resolves a single B4 item by row id. Returns (nil, nil) when the
// row is gone/soft-deleted (stale finding). Read-only.
func (s *SQLStore) GetActiveNNRelationship(ctx context.Context, tenantID, relID string) (*NNRelationshipRow, error) {
	const q = `SELECT r.id, r.name, r.parent_entity_name, r.child_entity_name,
	       COALESCE(r.join_table_name, '') AS join_table_name` + nnRelationshipWhere + ` AND r.id = ?`
	var row NNRelationshipRow
	if err := s.db.GetContext(ctx, &row, q, tenantID, relID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("doctor.GetActiveNNRelationship: %w", err)
	}
	return &row, nil
}

// ListExistingTables reports which of the given physical tables exist in the current schema —
// ONE batched information_schema.TABLES probe (never a query per table). An empty input skips
// the round-trip entirely (and avoids the invalid `IN ()`). Read-only.
func (s *SQLStore) ListExistingTables(ctx context.Context, tables []string) (map[string]bool, error) {
	if len(tables) == 0 {
		return map[string]bool{}, nil
	}
	q, args, err := sqlx.In(`SELECT TABLE_NAME AS table_name FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE' AND TABLE_NAME IN (?)`, tables)
	if err != nil {
		return nil, fmt.Errorf("doctor.ListExistingTables: %w", err)
	}
	var names []string
	if err := s.db.SelectContext(ctx, &names, q, args...); err != nil {
		return nil, fmt.Errorf("doctor.ListExistingTables: %w", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

// ListTableColumns returns every column (with generation expression) of the given tables —
// ONE batched information_schema.COLUMNS probe. Read-only.
func (s *SQLStore) ListTableColumns(ctx context.Context, tables []string) ([]TableColumn, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	q, args, err := sqlx.In(`SELECT TABLE_NAME AS table_name, COLUMN_NAME AS column_name,
		       COALESCE(GENERATION_EXPRESSION, '') AS generation_expression
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (?)`, tables)
	if err != nil {
		return nil, fmt.Errorf("doctor.ListTableColumns: %w", err)
	}
	var rows []TableColumn
	if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("doctor.ListTableColumns: %w", err)
	}
	return rows, nil
}

// ListTableIndexes returns every index column of the given tables (ordered inside each index
// by SEQ_IN_INDEX) — ONE batched information_schema.STATISTICS probe. Read-only.
func (s *SQLStore) ListTableIndexes(ctx context.Context, tables []string) ([]IndexColumn, error) {
	if len(tables) == 0 {
		return nil, nil
	}
	q, args, err := sqlx.In(`SELECT TABLE_NAME AS table_name, INDEX_NAME AS index_name,
		       SEQ_IN_INDEX AS seq_in_index, COALESCE(COLUMN_NAME, '') AS column_name
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME IN (?)
		ORDER BY TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, tables)
	if err != nil {
		return nil, fmt.Errorf("doctor.ListTableIndexes: %w", err)
	}
	var rows []IndexColumn
	if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("doctor.ListTableIndexes: %w", err)
	}
	return rows, nil
}
