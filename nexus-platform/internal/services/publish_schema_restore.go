package services

// publish_schema_restore.go — specs/063-draft-publish S7-10: "Quay lại" brings back a field or a table a
// published version deleted. The delete (publish_schema_ops.go) is DML only — no column or table was ever
// dropped — so the restore is DML only too, on the rollback transaction:
//
//	field : un-delete the row, un-tombstone its sys_relationships, put the target attributes back,
//	        rebuild the unique pivot (specs/019) from the values still in obj_*.data
//	table : un-tombstone the name, un-tombstone every relationship, put back the field restrictions and the
//	        role grants it dropped, bring back EXACTLY the records the delete trashed (deleted_at = the
//	        recorded sweptAt), rebuild the pivot of every unique field
//
// A name a live table / relationship holds meanwhile is MySQL 1062 -> ROLLBACK_NAME_TAKEN (mapped by
// PublishService.Rollback); a unique value two live records now share is ROLLBACK_UNIQUE_CONFLICT. Both
// abort the transaction, so nothing is half-restored (FR-067).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
)

// schemaRestoreMeta is the part of a delete version doc's Meta a restore reads.
type schemaRestoreMeta struct {
	SweptAt       string           `json:"sweptAt"`
	SweptCount    *int64           `json:"sweptCount"`
	FieldDef      map[string]any   `json:"fieldDef"`
	EntityDef     map[string]any   `json:"entityDef"`
	Relationships []map[string]any `json:"relationships"`
	FieldACL      []map[string]any `json:"fieldAcl"`
	RoleGrants    []map[string]any `json:"roleGrants"`
}

func parseRestoreMeta(raw json.RawMessage) (*schemaRestoreMeta, error) {
	var m schemaRestoreMeta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("services.parseRestoreMeta: %w", err)
	}
	return &m, nil
}

// restorable reports whether the Meta carries what a restore of this type needs; a delete recorded
// without it (the definition row was not there to read) cannot be undone and the plan says so.
func (m *schemaRestoreMeta) restorable(docType string) bool {
	switch docType {
	case DocTypeField:
		return m.FieldDef != nil
	case DocTypeEntity:
		if m.EntityDef == nil {
			return false
		}
		_, err := time.Parse(time.RFC3339, m.SweptAt)
		return err == nil
	}
	return false
}

func (m *schemaRestoreMeta) relationships() []metadata.RelationshipRestore {
	out := make([]metadata.RelationshipRestore, 0, len(m.Relationships))
	for _, r := range m.Relationships {
		id, _ := r["id"].(string)
		name, _ := r["name"].(string)
		if id == "" || name == "" {
			continue
		}
		rr := metadata.RelationshipRestore{ID: id, Name: name}
		if j, ok := r["join_table_name"].(string); ok && j != "" {
			rr.JoinTableName = &j
		}
		out = append(out, rr)
	}
	return out
}

// restoreFieldACL puts back the field-level restrictions the delete dropped (sys_field_acl is
// default-allow: a missing row shows the field). A role deleted meanwhile has no row in acl_roles and is
// skipped; a row that exists again for the same (role, table, field) is overwritten by the recorded one.
func restoreFieldACL(ctx context.Context, tx *sqlx.Tx, tenantID string, rows []map[string]any) error {
	for _, r := range rows {
		role, _ := r["role_id"].(string)
		entity, _ := r["entity_name"].(string)
		field, _ := r["field_name"].(string)
		if role == "" || entity == "" || field == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sys_field_acl (id, tenant_id, role_id, entity_name, field_name, can_view, can_edit)
			SELECT UUID(), r.tenant_id, r.id, ?, ?, ?, ? FROM acl_roles r WHERE r.id = ? AND r.tenant_id = ?
			ON DUPLICATE KEY UPDATE can_view = VALUES(can_view), can_edit = VALUES(can_edit)`,
			entity, field, truthy(r["can_view"]), truthy(r["can_edit"]), role, tenantID); err != nil {
			return fmt.Errorf("services.restoreFieldACL: %w", err)
		}
	}
	return nil
}

// restoreGrantSQL puts one recorded grant back. The role must still exist in the tenant (a role deleted meanwhile is
// skipped). A grant the role holds again on the same (table, action) is only ever raised to the recorded level: the
// ranks follow the access lattice none < owner < team, field:<f> < all, and team and field:<f> rank alike because
// neither covers the other, so neither replaces the other.
const restoreGrantSQL = `INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level)
	SELECT UUID(), r.id, ?, ?, ? FROM acl_roles r WHERE r.id = ? AND r.tenant_id = ?
	ON DUPLICATE KEY UPDATE access_level = IF(
		(CASE WHEN access_level = 'all' THEN 3 WHEN access_level = 'team' OR access_level LIKE 'field:%' THEN 2 WHEN access_level = 'owner' THEN 1 ELSE 0 END) <
		(CASE WHEN VALUES(access_level) = 'all' THEN 3 WHEN VALUES(access_level) = 'team' OR VALUES(access_level) LIKE 'field:%' THEN 2 WHEN VALUES(access_level) = 'owner' THEN 1 ELSE 0 END),
		VALUES(access_level), access_level)`

// restoreRoleGrants puts back the role grants a table delete dropped (metadata.Repository.SoftDeleteEntity drops
// them so a new table of that name inherits none), as the published delete recorded them.
func restoreRoleGrants(ctx context.Context, tx *sqlx.Tx, tenantID string, rows []map[string]any) error {
	for _, g := range rows {
		role, _ := g["role_id"].(string)
		entity, _ := g["entity_name"].(string)
		action, _ := g["action"].(string)
		level, _ := g["access_level"].(string)
		if role == "" || entity == "" || action == "" || level == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, restoreGrantSQL, entity, action, level, role, tenantID); err != nil {
			return fmt.Errorf("services.restoreRoleGrants: %w", err)
		}
	}
	return nil
}

// truthy reads a tinyint(1) column that went through a generic map scan (int64, float64 after JSON, "1").
func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case int64:
		return x != 0
	case string:
		return x == "1" || x == "true"
	}
	return false
}

// uniqueConflict turns a pivot clash into the rollback's own error code.
func uniqueConflict(field string, count int) error {
	return &ValidationError{Code: "ROLLBACK_UNIQUE_CONFLICT", Field: field,
		Msg:     fmt.Sprintf("Không quay lại được: trường %q có giá trị duy nhất bị trùng giữa các hồ sơ đang sống, xử lý các hồ sơ trùng rồi thử lại", field),
		Details: map[string]any{"field": field, "conflicts": count}}
}

// backfillPivot rebuilds the unique pivot of one field on the rollback tx; any clash aborts the restore.
func backfillPivot(ctx context.Context, tx *sqlx.Tx, tenantID, entityName, field string) error {
	res, err := dynrepo.BackfillUniqueFieldTx(ctx, tx, entityName, tenantID, field)
	var dup *dynrepo.DuplicateUniqueError
	var me *mysql.MySQLError
	switch {
	case errors.As(err, &dup), errors.As(err, &me) && me.Number == 1062: // a pivot slot taken: same meaning as a pre-scan conflict, never a "name taken"
		return uniqueConflict(field, 1)
	case err != nil:
		return fmt.Errorf("services.backfillPivot: %w", err)
	case len(res.Conflicts) > 0:
		return uniqueConflict(field, len(res.Conflicts))
	}
	return nil
}

// restoreFieldRows undoes a field delete's metadata: the row and its relationship registry rows.
func restoreFieldRows(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) (*schemaRestoreMeta, error) {
	m, err := parseRestoreMeta(c.Restore)
	if err != nil {
		return nil, err
	}
	if id, _ := m.FieldDef["id"].(string); id != c.DocKey {
		return nil, fmt.Errorf("services.restoreFieldRows: recorded definition %q is not field %s", id, c.DocKey)
	}
	if err := metadata.RestoreFieldExec(ctx, tx, tenantID, c.DocKey); err != nil {
		return nil, fmt.Errorf("services.restoreFieldRows: %w", err)
	}
	if _, err := metadata.RestoreRelationshipsExec(ctx, tx, tenantID, m.relationships()); err != nil {
		return nil, fmt.Errorf("services.restoreFieldRows: %w", err)
	}
	if err := restoreFieldACL(ctx, tx, tenantID, m.FieldACL); err != nil {
		return nil, err
	}
	return m, nil
}

// restoreFieldPivot rebuilds the pivot of a restored unique field. When its table is deleted (restored
// later in the same rollback, or not at all) there is no live obj_* table to read, and the table's own
// restore rebuilds every unique field.
func restoreFieldPivot(ctx context.Context, tx *sqlx.Tx, tenantID string, m *schemaRestoreMeta) error {
	if !truthy(m.FieldDef["is_unique"]) {
		return nil
	}
	entityID, _ := m.FieldDef["entity_id"].(string)
	name, _ := m.FieldDef["name"].(string)
	var entity string
	err := tx.GetContext(ctx, &entity, `SELECT name FROM sys_entities WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL`, tenantID, entityID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("services.restoreFieldPivot: resolve table: %w", err)
	}
	return backfillPivot(ctx, tx, tenantID, entity, name)
}

// restoreEntity undoes a table delete (ApplyTx of the entity adapter).
func restoreEntity(ctx context.Context, tx *sqlx.Tx, tenantID string, c metaversion.Change) error {
	m, err := parseRestoreMeta(c.Restore)
	if err != nil {
		return err
	}
	if id, _ := m.EntityDef["id"].(string); id != c.DocKey {
		return fmt.Errorf("services.restoreEntity: recorded definition %q is not table %s", id, c.DocKey)
	}
	at, err := time.Parse(time.RFC3339, m.SweptAt)
	if err != nil {
		return fmt.Errorf("services.restoreEntity: sweptAt: %w", err)
	}
	name, _ := m.EntityDef["name"].(string)
	var d entityDoc
	if err := json.Unmarshal(c.After, &d); err != nil {
		return fmt.Errorf("services.restoreEntity: decode %s: %w", c.DocKey, err)
	}
	if err := metadata.RestoreEntityExec(ctx, tx, tenantID, c.DocKey, name, d.Label, d.Description); err != nil {
		return fmt.Errorf("services.restoreEntity: %w", err)
	}
	if _, err := metadata.RestoreRelationshipsExec(ctx, tx, tenantID, m.relationships()); err != nil {
		return fmt.Errorf("services.restoreEntity: %w", err)
	}
	if err := restoreFieldACL(ctx, tx, tenantID, m.FieldACL); err != nil {
		return err
	}
	if err := restoreRoleGrants(ctx, tx, tenantID, m.RoleGrants); err != nil {
		return err
	}
	if _, err := dynrepo.RestoreRecordsSweptAt(ctx, tx, name, tenantID, at); err != nil {
		return fmt.Errorf("services.restoreEntity: %w", err)
	}
	var unique []string
	if err := tx.SelectContext(ctx, &unique, `SELECT name FROM sys_fields WHERE tenant_id = ? AND entity_id = ? AND is_unique = 1 AND deleted_at IS NULL ORDER BY name`,
		tenantID, c.DocKey); err != nil {
		return fmt.Errorf("services.restoreEntity: list unique fields: %w", err)
	}
	for _, f := range unique {
		if err := backfillPivot(ctx, tx, tenantID, name, f); err != nil {
			return err
		}
	}
	return nil
}
