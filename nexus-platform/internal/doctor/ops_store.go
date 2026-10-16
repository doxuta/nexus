// ops_store.go — the DOCTOR-OPS queries (../specs/008-nexus-doctor/spec.md §4 lát 3):
//
//   - C4 zombie workflows: detect (tenant-pinned diff vs active sys_entities) + the ONE
//     bounded write this store owns, DisableWorkflow. The admin workflow update path is NOT
//     reusable here on purpose — WorkflowService.Update validates that the rule's entity
//     still EXISTS (WithEntityChecker), which is exactly the condition a C4 zombie violates,
//     and it is a full-replace UPDATE; the doctor needs a single-column `is_active = 0` flip
//     that never rewrites the rule body. Disable, NEVER delete (spec §2.2 C4): the engine
//     keys rules by entity NAME (workflow/cache.go wfKey), so deleting would hide the rule
//     while a later re-register of the same entity name silently RESURRECTS nothing — but a
//     row left ACTIVE resurrects the OLD automation. is_active=0 keeps the rule visible and
//     auditable, and an admin can re-enable it deliberately.
//   - B5 unregistered rel_* tables (report-only): the ONE deliberately tenant-agnostic
//     detect. rel_* tables are server-global physical objects shared by every tenant, and a
//     table is only "orphaned" when NO tenant's active registry row claims it — a per-tenant
//     pin would flag co-tenants' live tables. Same intentional exception the janitor and the
//     scheduler sweeps make; the query reads NAMES + CREATE_TIME only, never row data.
//   - C8 dead relation targets (report-only): tenant-pinned sys_fields JSON probe.
//   - Scheduled-scan support: ListActiveTenants + ListTenantAdmins (read-only) for the
//     DoctorScheduler's per-tenant sweep + admin notification fan-out.
//
// All list queries are LIMIT-bounded; identifiers are compile-time literals; every runtime
// value travels as a `?` parameter.
package doctor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// ZombieWorkflowRow is an active sys_workflows row whose entity_name no longer resolves to an
// active entity of the same tenant (C4's class).
type ZombieWorkflowRow struct {
	ID         string `db:"id"`
	Name       string `db:"name"`
	EntityName string `db:"entity_name"`
	Trigger    string `db:"trigger_event"`
}

// OrphanRelTable is a physical rel_* table with no active registry row in ANY tenant (B5's
// class). CreatedAt is information_schema CREATE_TIME (cheaply available table age; NULL on
// some storage configurations — reported honestly as unknown).
type OrphanRelTable struct {
	TableName string       `db:"table_name"`
	CreatedAt sql.NullTime `db:"create_time"`
}

// DeadRelationTargetRow is an active relation field whose options.relatedEntity points at an
// entity that is gone for this tenant (C8's class).
type DeadRelationTargetRow struct {
	FieldID       string `db:"id"`
	EntityName    string `db:"entity_name"`
	FieldName     string `db:"name"`
	RelatedEntity string `db:"related_entity"`
}

// zombieWorkflowWhere is the C4 predicate: an ACTIVE (is_active=1, non-deleted) rule of this
// tenant whose entity_name has no active sys_entities row under the SAME tenant.
const zombieWorkflowWhere = `
	FROM sys_workflows w
	WHERE w.tenant_id = ?
	  AND w.deleted_at IS NULL
	  AND w.is_active = 1
	  AND NOT EXISTS (SELECT 1 FROM sys_entities e
	                  WHERE e.tenant_id = w.tenant_id AND e.name = w.entity_name AND e.deleted_at IS NULL)`

// ListZombieWorkflows returns up to limit active workflows pointing at a dead entity (C4).
// Read-only.
func (s *SQLStore) ListZombieWorkflows(ctx context.Context, tenantID string, limit int) ([]ZombieWorkflowRow, error) {
	const q = `SELECT w.id, w.name, w.entity_name, w.trigger_event` + zombieWorkflowWhere + `
	ORDER BY w.id LIMIT ?`
	var rows []ZombieWorkflowRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListZombieWorkflows: %w", err)
	}
	return rows, nil
}

// GetZombieWorkflow re-resolves a single C4 item by row id (the per-item re-detect). Returns
// (nil, nil) when the rule is no longer a zombie — deleted, already disabled, or its entity
// came back alive. Read-only.
func (s *SQLStore) GetZombieWorkflow(ctx context.Context, tenantID, wfID string) (*ZombieWorkflowRow, error) {
	const q = `SELECT w.id, w.name, w.entity_name, w.trigger_event` + zombieWorkflowWhere + ` AND w.id = ?`
	var row ZombieWorkflowRow
	if err := s.db.GetContext(ctx, &row, q, tenantID, wfID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("doctor.GetZombieWorkflow: %w", err)
	}
	return &row, nil
}

// DisableWorkflow flips ONE tenant-pinned workflow to is_active=0 (the C4 fix — disable,
// never delete; see the file header for why the admin update path is not reusable). Returns
// the number of rows changed: 0 means a concurrent actor already disabled/deleted it (the
// caller counts that as skipped). updated_at refreshes so the admin list shows the flip.
func (s *SQLStore) DisableWorkflow(ctx context.Context, tenantID, wfID string) (int64, error) {
	const q = `UPDATE sys_workflows SET is_active = 0, updated_at = ?
	WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL AND is_active = 1`
	res, err := s.db.ExecContext(ctx, q, time.Now().UTC(), tenantID, wfID)
	if err != nil {
		return 0, fmt.Errorf("doctor.DisableWorkflow: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("doctor.DisableWorkflow: rows affected: %w", err)
	}
	return n, nil
}

// ListOrphanRelTables returns up to limit physical rel_* tables that no active N-N registry
// row (of ANY tenant — see the file header for why this one is deliberately tenant-agnostic)
// claims via join_table_name (B5, report-only). Read-only; names + CREATE_TIME only.
func (s *SQLStore) ListOrphanRelTables(ctx context.Context, limit int) ([]OrphanRelTable, error) {
	const q = `SELECT t.TABLE_NAME AS table_name, t.CREATE_TIME AS create_time
	FROM information_schema.TABLES t
	WHERE t.TABLE_SCHEMA = DATABASE()
	  AND t.TABLE_TYPE = 'BASE TABLE'
	  AND t.TABLE_NAME LIKE 'rel\_%'
	  AND NOT EXISTS (SELECT 1 FROM sys_relationships r
	                  WHERE r.join_table_name = t.TABLE_NAME AND r.deleted_at IS NULL)
	ORDER BY t.TABLE_NAME LIMIT ?`
	var rows []OrphanRelTable
	if err := s.db.SelectContext(ctx, &rows, q, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListOrphanRelTables: %w", err)
	}
	return rows, nil
}

// ListDeadRelationTargets returns up to limit active relation fields whose
// options.relatedEntity no longer resolves to an active entity of this tenant (C8,
// report-only). sys_fields is a small registry table — the JSON probe is fine here (same
// note as physicalFieldWhere). Read-only.
func (s *SQLStore) ListDeadRelationTargets(ctx context.Context, tenantID string, limit int) ([]DeadRelationTargetRow, error) {
	const q = `SELECT f.id, e.name AS entity_name, f.name,
	       COALESCE(JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.relatedEntity')), '') AS related_entity
	FROM sys_fields f
	JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
	WHERE f.tenant_id = ?
	  AND f.deleted_at IS NULL
	  AND e.deleted_at IS NULL
	  AND f.field_type = 'relation'
	  AND NOT EXISTS (SELECT 1 FROM sys_entities t
	                  WHERE t.tenant_id = f.tenant_id
	                    AND t.name = JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.relatedEntity'))
	                    AND t.deleted_at IS NULL)
	ORDER BY e.name, f.name LIMIT ?`
	var rows []DeadRelationTargetRow
	if err := s.db.SelectContext(ctx, &rows, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListDeadRelationTargets: %w", err)
	}
	return rows, nil
}

// ListPhysicalFieldRowsByEntityNames returns the active unique/relation/indexed field rows
// of ACTIVE entities carrying the given names across ALL tenants — the A3 ownership
// universe. Deliberately tenant-agnostic (same documented exception as ListOrphanRelTables):
// obj_* tables are shared physical objects, so an index is only ORPHANED when NO tenant's
// live field owns it — a tenant-pinned view would flag a co-tenant's healthy index forever
// (noise that can never be cleared on a healthy system). Reads metadata names/options only,
// never row data. LIMIT-bounded.
func (s *SQLStore) ListPhysicalFieldRowsByEntityNames(ctx context.Context, names []string, limit int) ([]PhysicalFieldRow, error) {
	if len(names) == 0 {
		return nil, nil
	}
	q, args, err := sqlx.In(`SELECT f.id, e.name AS entity_name, f.name, f.field_type, f.is_unique,
	       COALESCE(f.options, '') AS options
	FROM sys_fields f
	JOIN sys_entities e ON e.id = f.entity_id AND e.tenant_id = f.tenant_id
	WHERE f.deleted_at IS NULL
	  AND e.deleted_at IS NULL
	  AND e.name IN (?)
	  AND (f.is_unique = 1
	       OR f.field_type = 'relation'
	       OR COALESCE(JSON_UNQUOTE(JSON_EXTRACT(f.options, '$.indexed')), '') = 'true')
	ORDER BY e.name, f.name LIMIT ?`, names, limit)
	if err != nil {
		return nil, fmt.Errorf("doctor.ListPhysicalFieldRowsByEntityNames: %w", err)
	}
	var rows []PhysicalFieldRow
	if err := s.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("doctor.ListPhysicalFieldRowsByEntityNames: %w", err)
	}
	return rows, nil
}

// ListActiveTenants returns up to limit active tenant ids — the DoctorScheduler's sweep
// universe. Read-only; ids only.
func (s *SQLStore) ListActiveTenants(ctx context.Context, limit int) ([]string, error) {
	const q = `SELECT id FROM tenants WHERE status = 'active' ORDER BY id LIMIT ?`
	var ids []string
	if err := s.db.SelectContext(ctx, &ids, q, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListActiveTenants: %w", err)
	}
	return ids, nil
}

// ListTenantAdmins returns up to limit active admin user ids of the tenant — the recipients
// of the scheduled-scan notification. Read-only; ids only.
func (s *SQLStore) ListTenantAdmins(ctx context.Context, tenantID string, limit int) ([]string, error) {
	const q = `SELECT id FROM users
	WHERE tenant_id = ? AND is_admin = 1 AND status = 'active' AND deleted_at IS NULL
	ORDER BY id LIMIT ?`
	var ids []string
	if err := s.db.SelectContext(ctx, &ids, q, tenantID, limit); err != nil {
		return nil, fmt.Errorf("doctor.ListTenantAdmins: %w", err)
	}
	return ids, nil
}
