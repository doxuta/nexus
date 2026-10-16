package dynrepo

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/ddl"
)

// LinkRow is a single row in a rel_<a>_<b> join table.
type LinkRow struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenantId"`
	SourceID  string    `json:"sourceId"` // value of colA (<first>_id)
	TargetID  string    `json:"targetId"` // value of colB (<second>_id)
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// CreateJoinTable materializes the rel_<a>_<b> join table for an N-N relationship.
// The statement is built by ddl.BuildCreateJoinTable (which sanitizes both identifiers)
// and is idempotent (CREATE TABLE IF NOT EXISTS).
func (r *Repository) CreateJoinTable(ctx context.Context, entityA, entityB string) error {
	stmt, err := ddl.BuildCreateJoinTable(entityA, entityB)
	if err != nil {
		return fmt.Errorf("dynrepo.CreateJoinTable: %w", err)
	}
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.CreateJoinTable: exec: %w", err)
	}
	return nil
}

// JoinTableExists reports whether the rel_<a>_<b> table already exists.
// Used by the registration compensation path to decide whether to drop the table
// if metadata persistence fails AFTER the DDL auto-committed.
func (r *Repository) JoinTableExists(ctx context.Context, entityA, entityB string) (bool, error) {
	tableName, _, _, err := ddl.JoinParts(entityA, entityB)
	if err != nil {
		return false, fmt.Errorf("dynrepo.JoinTableExists: %w", err)
	}
	var n int
	err = r.db.GetContext(ctx, &n,
		`SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, tableName)
	if err != nil {
		return false, fmt.Errorf("dynrepo.JoinTableExists: %w", err)
	}
	return n > 0, nil
}

// DropJoinTable removes the rel_<a>_<b> join table. Used only on the compensation path
// when the metadata write fails after the DDL auto-committed.
func (r *Repository) DropJoinTable(ctx context.Context, entityA, entityB string) error {
	tableName, _, _, err := ddl.JoinParts(entityA, entityB)
	if err != nil {
		return fmt.Errorf("dynrepo.DropJoinTable: %w", err)
	}
	stmt := fmt.Sprintf("DROP TABLE IF EXISTS `%s`", tableName)
	if _, err := r.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dynrepo.DropJoinTable: exec: %w", err)
	}
	return nil
}

// CountRelationFKRefs counts the live (non-soft-deleted) records of an entity whose
// `<field>_id` FK column is non-NULL — i.e. how many child records currently hold a link
// through a 1-N/1-1 relation field. Backs the W2 relationship-delete impact pre-check so
// the Studio canvas can warn "N bản ghi đang giữ liên kết" BEFORE the destructive confirm.
// The entity and field names are sanitized (identifier grammar) before being interpolated;
// tenant_id always travels as a `?` placeholder. The COUNT runs on the (tenant_id,
// deleted_at, ...) composite index prefix + the relation column's own index — O(matching
// rows), an admin-time config operation, never on the record hot path.
func (r *Repository) CountRelationFKRefs(ctx context.Context, entity, tenantID, field string) (int, error) {
	table, err := tableFor(entity)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.CountRelationFKRefs: %w", err)
	}
	col, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.CountRelationFKRefs: field: %w", err)
	}
	var n int
	stmt := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ? AND `deleted_at` IS NULL AND `%s_id` IS NOT NULL",
		table, col)
	if err := r.db.GetContext(ctx, &n, stmt, tenantID); err != nil {
		return 0, fmt.Errorf("dynrepo.CountRelationFKRefs: %w", err)
	}
	return n, nil
}

// ListIDsByRelation returns up to limit ids of the tenant's LIVE records of entity whose 1-N/1-1
// relation field points at parentID, ascending, strictly after afterID ("" = from the start) — the
// keyset page the workflow children field_update walks (specs/084). Runs on the relation's own
// (tenant_id, `<field>_id`) index, so a page is O(limit) however large the table is. Identifiers are
// sanitized before interpolation; tenant, parent and cursor travel as placeholders.
func (r *Repository) ListIDsByRelation(ctx context.Context, entity, tenantID, field, parentID, afterID string, limit int) ([]string, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListIDsByRelation: %w", err)
	}
	col, err := ddl.SanitizeIdentifier(field)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListIDsByRelation: field: %w", err)
	}
	var ids []string
	stmt := fmt.Sprintf(
		"SELECT `id` FROM `%s` WHERE `tenant_id` = ? AND `%s_id` = ? AND `deleted_at` IS NULL AND `id` > ? ORDER BY `id` LIMIT ?",
		table, col)
	if err := r.db.SelectContext(ctx, &ids, stmt, tenantID, parentID, afterID, limit); err != nil {
		return nil, fmt.Errorf("dynrepo.ListIDsByRelation: %w", err)
	}
	return ids, nil
}

// CountJoinRows counts this tenant's link rows in the rel_<a>_<b> join table — the W2
// impact pre-check for deleting an N-N relationship ("M liên kết sẽ bị ngắt"). The count
// is raw (tenant-scoped only): it includes links whose endpoint records were soft-deleted,
// because ALL of the tenant's link rows stop being reachable once the relationship
// registration is removed. Callers must check JoinTableExists first when the table may
// have been reclaimed. Table name derives from ddl.JoinParts (sanitized, canonical order);
// tenant_id travels as a `?` placeholder.
func (r *Repository) CountJoinRows(ctx context.Context, entityA, entityB, tenantID string) (int, error) {
	tableName, _, _, err := ddl.JoinParts(entityA, entityB)
	if err != nil {
		return 0, fmt.Errorf("dynrepo.CountJoinRows: %w", err)
	}
	var n int
	stmt := fmt.Sprintf("SELECT COUNT(*) FROM `%s` WHERE `tenant_id` = ?", tableName)
	if err := r.db.GetContext(ctx, &n, stmt, tenantID); err != nil {
		return 0, fmt.Errorf("dynrepo.CountJoinRows: %w", err)
	}
	return n, nil
}

// LinkRecords inserts a row into rel_<a>_<b> linking sourceID (in entity A's direction)
// to targetID (in entity B's direction). The join-table name and column assignment come
// from ddl.JoinParts so both sides always use the same canonical names.
//
// Idempotent: if the (tenant_id, <a>_id, <b>_id) triple already exists (UNIQUE KEY hit),
// it returns (false, nil) — "already linked". On a fresh insert it returns (true, nil).
//
// IMPORTANT: sourceEntity and targetEntity must be the ORIGINAL caller-facing names (not
// pre-sorted). JoinParts sorts them internally; the values for the sorted columns are
// assigned by matching which input ended up in position "first" vs "second".
func (r *Repository) LinkRecords(
	ctx context.Context,
	tenantID, userID string,
	sourceEntity, sourceID string,
	targetEntity, targetID string,
) (created bool, err error) {
	tableName, colA, colB, err := ddl.JoinParts(sourceEntity, targetEntity)
	if err != nil {
		return false, fmt.Errorf("dynrepo.LinkRecords: %w", err)
	}

	// Determine which ID goes into colA (<first>_id) vs colB (<second>_id).
	// colA is the alphabetically-first entity's column; assign accordingly.
	var valA, valB string
	if colA == sourceEntity+"_id" {
		valA, valB = sourceID, targetID
	} else {
		valA, valB = targetID, sourceID
	}

	id := uuid.New().String()
	stmt := fmt.Sprintf(
		"INSERT INTO `%s` (`id`, `tenant_id`, `%s`, `%s`, `created_by`) VALUES (?, ?, ?, ?, ?)",
		tableName, colA, colB,
	)
	_, execErr := r.db.ExecContext(ctx, stmt, id, tenantID, valA, valB, userID)
	if execErr != nil {
		if _, ok := duplicateIndexName(execErr); ok {
			// Already linked — idempotent success (duplicate of the UNIQUE(tenant, a, b) key).
			return false, nil
		}
		return false, fmt.Errorf("dynrepo.LinkRecords: exec: %w", execErr)
	}
	return true, nil
}

// UnlinkRecords hard-deletes the join-table row linking sourceID to targetID in the N-N
// relationship. Returns ErrRecordNotFound if the link does not exist (for this tenant).
// All three of tenant_id, <a>_id, <b>_id are in the WHERE clause for tenant isolation and
// correctness.
func (r *Repository) UnlinkRecords(
	ctx context.Context,
	tenantID string,
	sourceEntity, sourceID string,
	targetEntity, targetID string,
) error {
	tableName, colA, colB, err := ddl.JoinParts(sourceEntity, targetEntity)
	if err != nil {
		return fmt.Errorf("dynrepo.UnlinkRecords: %w", err)
	}

	var valA, valB string
	if colA == sourceEntity+"_id" {
		valA, valB = sourceID, targetID
	} else {
		valA, valB = targetID, sourceID
	}

	stmt := fmt.Sprintf(
		"DELETE FROM `%s` WHERE `tenant_id` = ? AND `%s` = ? AND `%s` = ?",
		tableName, colA, colB,
	)
	res, err := r.db.ExecContext(ctx, stmt, tenantID, valA, valB)
	if err != nil {
		return fmt.Errorf("dynrepo.UnlinkRecords: exec: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("dynrepo.UnlinkRecords: rows affected: %w", err)
	}
	if affected == 0 {
		return ErrRecordNotFound
	}
	return nil
}

// ListJoinRows returns EVERY link row of one N-N relationship for the tenant (PACK-EXPORT-FULL:
// the vertical-pack export reads the whole join table to emit PackLinks). SourceID carries the
// alphabetically-FIRST entity's column value (colA, matching ddl.JoinParts order) and TargetID the
// second's, so the caller can map each side back to its entity deterministically. Rows are ordered
// by (created_at, id) so two exports of the same tenant serialize identically. The count is raw
// tenant-scoped (soft-deleted endpoint records are NOT filtered here — an export maps ids through
// its sample set and drops the unmatched ones, mirroring CountJoinRows' contract). Table name
// derives from ddl.JoinParts (sanitized); tenant_id travels as a `?` placeholder.
func (r *Repository) ListJoinRows(ctx context.Context, entityA, entityB, tenantID string) ([]LinkRow, error) {
	tableName, colA, colB, err := ddl.JoinParts(entityA, entityB)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListJoinRows: %w", err)
	}
	stmt := fmt.Sprintf(
		"SELECT `id`, `tenant_id`, `%s` AS source_id, `%s` AS target_id, `created_by`, `created_at` FROM `%s` WHERE `tenant_id` = ? ORDER BY `created_at` ASC, `id` ASC",
		colA, colB, tableName)
	rows, err := r.db.QueryContext(ctx, stmt, tenantID)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.ListJoinRows: query: %w", err)
	}
	defer rows.Close()

	var out []LinkRow
	for rows.Next() {
		var lr LinkRow
		if err := rows.Scan(&lr.ID, &lr.TenantID, &lr.SourceID, &lr.TargetID, &lr.CreatedBy, &lr.CreatedAt); err != nil {
			return nil, fmt.Errorf("dynrepo.ListJoinRows: scan: %w", err)
		}
		out = append(out, lr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("dynrepo.ListJoinRows: rows: %w", err)
	}
	return out, nil
}

// ListLinked returns all records in targetEntity that are linked to sourceID via the N-N
// join table. Returns the IDs of linked records so the caller can fetch full records if
// needed. Pagination: page (1-indexed) and limit (max 100).
//
// Multi-tenancy: the JOIN filters tenant_id on BOTH the join table AND the obj_* table
// to prevent cross-tenant data leakage even when two tenants share an obj_* table.
//
// A5 row-level security: scope is applied to the TARGET obj_* table (alias "o") via
// scope.rowFilter(), exactly mirroring ListRecords. The IDs returned match exactly what
// the caller would see on a direct GET/List of the TARGET entity under the SAME scope —
// no privilege escalation via the linked-list path. Crucially, scope must be the
// TARGET entity's resolved row-scope (derived from the target's ACL grant), NOT the
// source entity's scope: the two entities may have different ACL levels for the caller.
// The RelationService is responsible for resolving and passing the correct target scope
// (NN-01 fix); this layer applies whatever scope it receives. The total count also
// respects scope so pagination math is consistent and never over-reports.
func (r *Repository) ListLinked(
	ctx context.Context,
	tenantID string,
	sourceEntity, sourceID string,
	targetEntity string,
	scope RowScope,
	page, limit int,
) ([]string, int, error) {
	tableName, colA, colB, err := ddl.JoinParts(sourceEntity, targetEntity)
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListLinked: %w", err)
	}

	// colA is the alphabetically-first entity's column. Determine which column holds
	// the source (sourceEntity) IDs and which holds the target (targetEntity) IDs.
	var srcCol, tgtCol string
	if colA == sourceEntity+"_id" {
		srcCol, tgtCol = colA, colB
	} else {
		srcCol, tgtCol = colB, colA
	}

	page, limit = ClampPageLimit(page, limit)
	offset := (page - 1) * limit

	targetTable, err := ddl.SanitizeIdentifier(targetEntity)
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListLinked: targetEntity: %w", err)
	}
	objTable := "obj_" + targetTable

	// A5 scope predicate applied to alias "o" (the target obj_* table), NOT the join-table
	// alias "j": rowFilterFor(alias) is the shared, centrally-alias-qualified primitive
	// aggregate.go/report_join.go already use for the identical JOIN-scoping need (was
	// previously a bespoke rewriteScopeForAlias string-rewrite here, which silently could not
	// have picked up new scope columns like owner_id — removed in favor of this).
	scope, err = r.resolveReadScope(ctx, tenantID, targetEntity, scope)
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListLinked: %w", err)
	}
	scopeSQL, scopeArgs := scope.rowFilterFor("o")

	// COUNT query uses the same JOIN+deleted_at+scope guard as the SELECT so pagination math
	// is correct — total reflects only what the caller can see.
	countStmt := fmt.Sprintf(
		"SELECT COUNT(*) FROM `%s` j "+
			"INNER JOIN `%s` o ON o.id = j.`%s` AND o.tenant_id = ? AND o.deleted_at IS NULL "+
			"WHERE j.`tenant_id` = ? AND j.`%s` = ?%s",
		tableName, objTable, tgtCol, srcCol, scopeSQL,
	)
	countArgs := append([]any{tenantID, tenantID, sourceID}, scopeArgs...)
	var total int
	if err := r.db.GetContext(ctx, &total, countStmt, countArgs...); err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListLinked: count: %w", err)
	}

	if total == 0 {
		return []string{}, 0, nil
	}

	// SELECT target IDs via join to obj_* so we only return IDs for records that exist,
	// belong to the same tenant, are not soft-deleted, AND are visible to caller's scope.
	listStmt := fmt.Sprintf(
		`SELECT j.`+"`%s`"+` FROM `+"`%s`"+` j
		 INNER JOIN `+"`%s`"+` o ON o.id = j.`+"`%s`"+` AND o.tenant_id = ? AND o.deleted_at IS NULL
		 WHERE j.tenant_id = ? AND j.`+"`%s`"+` = ?%s
		 ORDER BY j.created_at DESC
		 LIMIT ? OFFSET ?`,
		tgtCol, tableName, objTable, tgtCol, srcCol, scopeSQL,
	)
	listArgs := append([]any{tenantID, tenantID, sourceID}, scopeArgs...)
	listArgs = append(listArgs, limit, offset)
	rows, err := r.db.QueryContext(ctx, listStmt, listArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListLinked: query: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, 0, fmt.Errorf("dynrepo.ListLinked: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("dynrepo.ListLinked: rows: %w", err)
	}
	return ids, total, nil
}
