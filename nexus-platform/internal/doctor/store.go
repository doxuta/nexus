// store.go — the Doctor's repository: the read-only detect queries behind the DOCTOR-CORE
// soft-metadata checks (B1/B2/C1/C2), plus the one nav purge that IS the existing shared
// helper (appmeta.PurgeNavForRef). Every query pins tenant_id (multi-tenant invariant) and
// every list query is LIMIT-bounded (callers pass MaxFindingsPerCheck+1). Table/column names
// are compile-time literals; every runtime value travels as a `?` parameter.
package doctor

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/appmeta"
)

// SQLStore runs the doctor's detect queries over sqlx. Fixes (except the nav purge, which
// delegates to the shared appmeta helper) live in the service layer on top of the existing
// metadata/layout repositories — this store never issues bespoke repair SQL.
type SQLStore struct {
	db *sqlx.DB
}

// NewSQLStore wires the store over an existing sqlx connection (the same pool the dynamic
// record / appmeta repositories use).
func NewSQLStore(db *sqlx.DB) *SQLStore {
	return &SQLStore{db: db}
}

// RelationshipRow is a sys_relationships row projected for the B1/B2 detectors.
type RelationshipRow struct {
	ID               string `db:"id"`
	Name             string `db:"name"`
	ParentEntityName string `db:"parent_entity_name"`
	ChildEntityName  string `db:"child_entity_name"`
	RelationshipType string `db:"relationship_type"`
	ChildFKColumn    string `db:"child_fk_column"`
}

// LayoutRow is a sys_layouts row projected for the C1 detector.
type LayoutRow struct {
	ID         string `db:"id"`
	EntityName string `db:"entity_name"`
	ViewType   string `db:"view_type"`
	LayoutJSON string `db:"layout_json"`
}

// NavRef is one orphaned (item_type, ref_id) reference group in sys_app_nav_items, with the
// number of nav rows carrying it (a dead entity can be referenced from several apps; the C2
// fix purges the whole group in one statement, mirroring appmeta.PurgeNavForRef).
type NavRef struct {
	ItemType string `db:"item_type"`
	RefID    string `db:"ref_id"`
	Rows     int    `db:"row_count"`
}

// phantomRelWhere is the B1 predicate — the EXACT WHERE of migration
// 000038_tombstone_phantom_relationships (parent OR child entity has no ACTIVE sys_entities
// row under the SAME tenant), plus the tenant pin the per-tenant Doctor adds on top of the
// migration's global sweep. Doctor B1 = 000038 run on demand.
const phantomRelWhere = `
	FROM sys_relationships r
	WHERE r.tenant_id = ?
	  AND r.deleted_at IS NULL
	  AND ( NOT EXISTS (SELECT 1 FROM sys_entities e
	                    WHERE e.tenant_id = r.tenant_id AND e.name = r.parent_entity_name AND e.deleted_at IS NULL)
	     OR NOT EXISTS (SELECT 1 FROM sys_entities e
	                    WHERE e.tenant_id = r.tenant_id AND e.name = r.child_entity_name AND e.deleted_at IS NULL) )`

// ListPhantomRelationships returns up to limit active sys_relationships rows whose parent or
// child entity is gone (B1). Read-only.
func (s *SQLStore) ListPhantomRelationships(ctx context.Context, tenantID string, limit int) ([]RelationshipRow, error) {
	const q = `SELECT r.id, r.name, r.parent_entity_name, r.child_entity_name, r.relationship_type,
	       COALESCE(r.child_fk_column, '') AS child_fk_column` + phantomRelWhere + `
	ORDER BY r.id LIMIT ?`
	var rows []RelationshipRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListPhantomRelationships: %w", err)
	}
	return rows, nil
}

// IsPhantomRelationship re-detects a single B1 item by row id (the per-item guard the fix
// runs before mutating). Read-only.
func (s *SQLStore) IsPhantomRelationship(ctx context.Context, tenantID, relID string) (bool, error) {
	const q = `SELECT COUNT(*)` + phantomRelWhere + ` AND r.id = ?`
	var n int
	if err := s.db.GetContext(ctx, &n, q, tenantID, relID); err != nil {
		return false, fmt.Errorf("doctor.IsPhantomRelationship: %w", err)
	}
	return n > 0, nil
}

// orphanRelWhere is the B2 predicate: an active 1-N/1-1 registry row whose BOTH entities are
// still alive (a dead side is B1's class — the two checks are deliberately disjoint) but
// whose child_fk_column no longer maps to an active `relation` field on the child entity
// (fieldName + "_id" = child_fk_column, the newInverseRelationship derivation). This is the
// DeleteField step-3 best-effort window (field_orchestration.go: the field soft-delete
// commits but the companion registry soft-delete fails).
const orphanRelWhere = `
	FROM sys_relationships r
	WHERE r.tenant_id = ?
	  AND r.deleted_at IS NULL
	  AND r.relationship_type IN ('1-N', '1-1')
	  AND COALESCE(r.child_fk_column, '') <> ''
	  AND EXISTS (SELECT 1 FROM sys_entities p
	              WHERE p.tenant_id = r.tenant_id AND p.name = r.parent_entity_name AND p.deleted_at IS NULL)
	  AND EXISTS (SELECT 1 FROM sys_entities c
	              WHERE c.tenant_id = r.tenant_id AND c.name = r.child_entity_name AND c.deleted_at IS NULL)
	  AND NOT EXISTS (SELECT 1 FROM sys_fields f
	                  JOIN sys_entities ce ON ce.id = f.entity_id AND ce.tenant_id = f.tenant_id
	                  WHERE f.tenant_id = r.tenant_id
	                    AND ce.name = r.child_entity_name
	                    AND ce.deleted_at IS NULL
	                    AND f.deleted_at IS NULL
	                    AND f.field_type = 'relation'
	                    AND CONCAT(f.name, '_id') = r.child_fk_column)`

// ListOrphanRelationRows returns up to limit active 1-N/1-1 registry rows orphaned by a
// vanished owning relation field (B2). Read-only.
func (s *SQLStore) ListOrphanRelationRows(ctx context.Context, tenantID string, limit int) ([]RelationshipRow, error) {
	const q = `SELECT r.id, r.name, r.parent_entity_name, r.child_entity_name, r.relationship_type,
	       COALESCE(r.child_fk_column, '') AS child_fk_column` + orphanRelWhere + `
	ORDER BY r.id LIMIT ?`
	var rows []RelationshipRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListOrphanRelationRows: %w", err)
	}
	return rows, nil
}

// IsOrphanRelationRow re-detects a single B2 item by row id. Read-only.
func (s *SQLStore) IsOrphanRelationRow(ctx context.Context, tenantID, relID string) (bool, error) {
	const q = `SELECT COUNT(*)` + orphanRelWhere + ` AND r.id = ?`
	var n int
	if err := s.db.GetContext(ctx, &n, q, tenantID, relID); err != nil {
		return false, fmt.Errorf("doctor.IsOrphanRelationRow: %w", err)
	}
	return n > 0, nil
}

// ListLayoutRows returns up to limit active sys_layouts rows for the tenant — the C1
// detector's scan window. Read-only.
//
// ponytail: the window bound is the caller's (service passes 2000 — ≥600 entities × 3 views,
// far past any real tenant today); a tenant beyond it gets an honest partial scan and the
// re-run model picks up the rest once earlier drift is fixed. Revisit with keyset paging if
// a tenant ever approaches the window.
func (s *SQLStore) ListLayoutRows(ctx context.Context, tenantID string, limit int) ([]LayoutRow, error) {
	// layout_json is a nullable JSON column — COALESCE so a (theoretical) NULL row scans as ""
	// (which the C1 detector then skips as malformed) instead of failing the whole scan.
	const q = `SELECT id, entity_name, view_type, COALESCE(layout_json, '') AS layout_json
	FROM sys_layouts
	WHERE tenant_id = ? AND deleted_at IS NULL
	ORDER BY id LIMIT ?`
	var rows []LayoutRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListLayoutRows: %w", err)
	}
	return rows, nil
}

// orphanNavWhere is the C2 predicate: a sys_app_nav_items row whose polymorphic ref_id no
// longer resolves to an ACTIVE entity/dashboard (the INV-4 cascade window — the read path
// already drops these silently, app_service.go GetNav; the stored rows stay dirty). Rows of
// an UNKNOWN item_type are deliberately NOT flagged: those are future-v2 rows a v1 binary
// must not destroy (same tolerance as the read path).
const orphanNavWhere = `
	FROM sys_app_nav_items n
	WHERE n.tenant_id = ?
	  AND ( (n.item_type = 'entity' AND NOT EXISTS (SELECT 1 FROM sys_entities e
	             WHERE e.tenant_id = n.tenant_id AND e.id = n.ref_id AND e.deleted_at IS NULL))
	     OR (n.item_type = 'dashboard' AND NOT EXISTS (SELECT 1 FROM sys_dashboards d
	             WHERE d.tenant_id = n.tenant_id AND d.id = n.ref_id AND d.deleted_at IS NULL)) )`

// ListOrphanNavRefs returns up to limit orphaned (item_type, ref_id) groups with their row
// counts (C2). Grouped because the fix purges per reference — the exact unit
// appmeta.PurgeNavForRef operates on. Read-only.
func (s *SQLStore) ListOrphanNavRefs(ctx context.Context, tenantID string, limit int) ([]NavRef, error) {
	const q = `SELECT n.item_type, n.ref_id, COUNT(*) AS row_count` + orphanNavWhere + `
	GROUP BY n.item_type, n.ref_id
	ORDER BY n.item_type, n.ref_id LIMIT ?`
	var rows []NavRef
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListOrphanNavRefs: %w", err)
	}
	return rows, nil
}

// CountOrphanNavRows re-detects a single C2 group: how many nav rows still carry this
// (itemType, refID) AND the reference is still dead. Read-only.
func (s *SQLStore) CountOrphanNavRows(ctx context.Context, tenantID, itemType, refID string) (int, error) {
	const q = `SELECT COUNT(*)` + orphanNavWhere + ` AND n.item_type = ? AND n.ref_id = ?`
	var n int
	if err := s.db.GetContext(ctx, &n, q, tenantID, itemType, refID); err != nil {
		return 0, fmt.Errorf("doctor.CountOrphanNavRows: %w", err)
	}
	return n, nil
}

// PurgeNavRef removes every nav row referencing (itemType, refID) — the C2 fix. It IS the
// shared INV-4 cascade helper (appmeta.PurgeNavForRef), never a re-implementation, so all
// delete paths keep one purge body. Idempotent: zero rows affected is fine.
func (s *SQLStore) PurgeNavRef(ctx context.Context, tenantID, itemType, refID string) error {
	if err := appmeta.PurgeNavForRef(ctx, s.db, tenantID, itemType, refID); err != nil {
		return fmt.Errorf("doctor.PurgeNavRef: %w", err)
	}
	return nil
}
