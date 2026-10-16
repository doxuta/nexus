// doctor_ops_checks.go — DOCTOR-OPS (../specs/008-nexus-doctor/spec.md §4 lát 3): the operational
// checks plus the flush-caches action.
//
//	C4  active workflow whose entity is gone — disable (is_active=0), NEVER delete   (confirm)
//	A3  unique index (uq_obj_*) on a shared obj_* table — RM-28 Slice B repurposed this
//	    (spec §6): a plain `is_unique` field's index is now non-unique (idx_), so ANY
//	    surviving uq_ backing one is itself the anomaly (half-run/failed demote migration,
//	    or a field the migration's coverage guard declined to touch); a 1-1 relation's own
//	    uq_ index is unaffected/expected (never pivot-backed, never demoted)               (report-only)
//	B5  physical rel_* table with no active registry row in ANY tenant — the
//	    grace-window is a FEATURE (resurrect-on-reregister)                           (report-only)
//	C8  relation field whose options.relatedEntity points at a dead entity — the
//	    DeleteField flow owns the cascade                                             (report-only)
//
// REPORT-ONLY contract (the framework's ReportOnly flag, doctor.Check): these checks carry
// NO Fix at all — the scan result ships `reportOnly: true` so the FE renders a "chỉ báo
// cáo, sửa tay" tag instead of a fix button, and a fix request naming one of them is
// refused whole with 422 DOCTOR_REPORT_ONLY before any mutation. Chosen over a stable
// "fix returns an error" convention because it is impossible to misuse: there is no fixer
// to reach, the FE learns the contract from the wire, and the refusal is symmetrical with
// DOCTOR_CHECK_UNKNOWN/DOCTOR_CONFIRM_REQUIRED (validate-before-mutate).
//
// C4 is the "zombie automation" class: the workflow engine keys rules by entity NAME
// (workflow/cache.go wfKey), so recreating an entity with the same name silently RESURRECTS
// every still-active rule of the dead entity. The fix flips is_active=0 — disable, never
// delete — through the doctor store's bounded UPDATE (see doctor/ops_store.go for why the
// admin update path is not reusable: it validates the entity is alive, which is exactly what
// a zombie violates). Confirm-gated: disabling changes future behavior on re-register, so a
// human reads the resurrection copy first.
package services

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/workflow"
)

// Check IDs (spec §2.2 numbering).
const (
	DoctorCheckOrphanUniqueIndex     = "A3_ORPHAN_UNIQUE_INDEX"
	DoctorCheckUnregisteredRelTables = "B5_UNREGISTERED_REL_TABLES"
	DoctorCheckZombieWorkflows       = "C4_ZOMBIE_WORKFLOWS"
	DoctorCheckDeadRelationTarget    = "C8_RELATION_TARGET_DEAD"
)

// WithOpsCaches attaches the ACL grants cache and the workflow rules cache (alongside the
// MetaCache from WithMetaCache and the #14 user-row cache from WithUserClaimCache) so the
// flush-caches action can clear every in-proc TTL cache, and the C4 fix can invalidate a
// disabled rule's cached entry. Chainable; both may be nil (nil-safe everywhere they are used).
func (s *DoctorService) WithOpsCaches(acl *CachedACLLoader, wf *workflow.CachedLoader) *DoctorService {
	s.aclCache = acl
	s.wfCache = wf
	return s
}

// WithUserClaimCache attaches the live user-row cache the Auth stage reads (queue #14
// claim-freshness, ../specs/001-auth-security/spec.md §5) so flush-caches clears it alongside
// the other three — the operator's escape hatch when a row on another replica is suspected
// stale before its 30 s TTL. Chainable; nil is allowed (nil-safe InvalidateAll).
func (s *DoctorService) WithUserClaimCache(users *CachedUserLoader) *DoctorService {
	s.userCache = users
	return s
}

// FlushCaches clears every wired in-proc TTL cache — MetaCache (entity/field metadata), the
// ACL grants cache, the workflow rules cache and the live user-row cache (#14) — and returns
// the names of the caches that were actually flushed (honest: a cache that is not wired is not
// reported as flushed). This is the operational "escape hatch" for cache-vs-DB drift the DB
// checks cannot see. The caches are IN-PROCESS: on a multi-replica deployment only THIS
// instance flushes; the others converge within their TTL backstops (30–60s) — the handler
// ships that warning as a static note (no replica detection exists; stay honest). The action
// is audited like a fix.
func (s *DoctorService) FlushCaches(ctx context.Context, tenantID string) []string {
	flushed := make([]string, 0, 4)
	if s.cache != nil {
		s.cache.InvalidateAll()
		flushed = append(flushed, "meta")
	}
	if s.aclCache != nil {
		s.aclCache.InvalidateAll()
		flushed = append(flushed, "acl")
	}
	if s.wfCache != nil {
		s.wfCache.InvalidateAll()
		flushed = append(flushed, "workflow")
	}
	if s.userCache != nil {
		s.userCache.InvalidateAll()
		flushed = append(flushed, "users")
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: audit.ActorFromContext(ctx),
		EntityName:  audit.DoctorMeta,
		Action:      audit.ActionUpdate,
		Diff:        audit.ComputeDiff(nil, map[string]any{"action": "flush-caches", "flushed": flushed}),
		At:          time.Now().UTC(),
	})
	s.logger.Info("services.Doctor: caches flushed",
		zap.String("tenant_id", tenantID),
		zap.Strings("flushed", flushed),
	)
	return flushed
}

// ---- C4: active workflows of a dead entity (zombie automation) — needs confirm ----------------

func (s *DoctorService) checkZombieWorkflows() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckZombieWorkflows,
		Severity:    doctor.SeverityWarn, // dormant now, but re-registering the entity name resurrects the rules
		AutoFixSafe: false,               // disabling changes re-register behavior — a human confirms
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListZombieWorkflows(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C4 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				findings = append(findings, doctor.Finding{
					Key: r.ID,
					Detail: fmt.Sprintf("workflow %q (%s, trigger %s) còn BẬT nhưng entity đã xóa — engine khóa rule theo TÊN entity, tạo lại entity trùng tên sẽ HỒI SINH rule cũ âm thầm. Fix tắt rule (is_active=0), KHÔNG xóa — bật lại được nếu chủ đích",
						r.Name, r.EntityName, r.Trigger),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			// Re-detect the single item: still active AND its entity still dead.
			row, err := s.store.GetZombieWorkflow(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.C4 re-detect: %w", err)
			}
			if row == nil {
				return doctor.ErrAlreadyClean // disabled/deleted meanwhile, or the entity came back
			}
			n, err := s.store.DisableWorkflow(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.C4 fix: %w", err)
			}
			if n == 0 {
				return doctor.ErrAlreadyClean // concurrent actor won the flip
			}
			// Same publish-after-commit discipline as WorkflowService writes: drop the cached
			// rule lists of (tenant, entity) so the disable takes effect before the 30s TTL.
			s.wfCache.Invalidate(tenantID, row.EntityName)
			s.recordFix(ctx, tenantID, DoctorCheckZombieWorkflows, f)
			return nil
		},
	}
}

// ---- A3: orphaned unique index on a shared obj_* table — report-only ---------------------------

// a3ExpectedIndexes derives, from the live field rows of EVERY tenant sharing the scanned
// entity names, the set of `uq_` index names the engine STILL legitimately owns per table,
// keyed "<table>\x00<index>". Ownership is global on purpose: obj_* tables are shared, so a
// co-tenant's live 1-1 field legitimately owns the index — a tenant-pinned view would flag it
// forever on a healthy system.
//
// RM-28 Slice B: ONLY "one_to_one" contributes here. A plain `is_unique` field's physical
// index is now non-unique (idx_obj_<entity>_<field> — AddUniqueGencolNonUnique/
// BuildCreateTable), so it never owns a uq_ name; any uq_ this check finds that is NOT a live
// 1-1 field's `uq_obj_<entity>_<field>_id` is therefore itself the anomaly (a plain unique
// field's index that a demote migration failed to touch, or half-run residue) and must be
// reported, not silently expected.
func a3ExpectedIndexes(rows []doctor.PhysicalFieldRow) map[string]bool {
	expected := make(map[string]bool, len(rows))
	for _, row := range rows {
		spec, ok := resolvePhysColSpec(row)
		if !ok {
			continue
		}
		table, ok := objTableOf(row.EntityName)
		if !ok {
			continue
		}
		if spec.kind == "one_to_one" {
			// spec.column is <field>_id — the index name suffix.
			expected[table+"\x00"+"uq_"+table+"_"+spec.column] = true
		}
	}
	return expected
}

func (s *DoctorService) checkOrphanUniqueIndexes() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckOrphanUniqueIndex,
		Severity:    doctor.SeverityInfo,
		AutoFixSafe: false,
		ReportOnly:  true, // dropping a shared-table index can break co-tenants — human decision
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			ents, err := s.store.ListActiveEntities(ctx, tenantID, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A3 detect: %w", err)
			}
			tables := make([]string, 0, len(ents))
			tableOf := make(map[string]string, len(ents))
			names := make([]string, 0, len(ents))
			for _, e := range ents {
				if t, ok := objTableOf(e.Name); ok {
					tables = append(tables, t)
					tableOf[e.Name] = t
					names = append(names, e.Name)
				}
			}
			// Ownership universe = live fields of EVERY tenant sharing these entity names
			// (see a3ExpectedIndexes — a co-tenant's live field owns its index legitimately).
			rows, err := s.store.ListPhysicalFieldRowsByEntityNames(ctx, names, doctorSchemaScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A3 detect: %w", err)
			}
			shapes, err := s.loadTableShapes(ctx, tables)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.A3 detect: %w", err)
			}
			expected := a3ExpectedIndexes(rows)
			findings := make([]doctor.Finding, 0)
			for _, e := range ents {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break
				}
				table := tableOf[e.Name]
				shape := shapes[table]
				if shape == nil {
					continue // table gone — A6's class
				}
				// Engine-owned unique index names of THIS table start with "uq_obj_<entity>_";
				// anything under that prefix with no live unique/1-1 field behind it (for THIS
				// tenant) is A3's class. Iterate sorted for a deterministic report.
				prefix := "uq_" + table + "_"
				names := make([]string, 0, len(shape.indexes))
				for ix := range shape.indexes {
					if strings.HasPrefix(ix, prefix) {
						names = append(names, ix)
					}
				}
				sort.Strings(names)
				for _, ix := range names {
					if expected[table+"\x00"+ix] {
						continue
					}
					findings = append(findings, doctor.Finding{
						Key: e.Name + ":" + ix,
						Detail: fmt.Sprintf("unique index %q trên %s không phải index của field 1-1 relation active nào (MỌI tenant) — RM-28 Slice B: field unique thường KHÔNG còn sở hữu index UNIQUE (đã hạ non-unique). Khả năng: (a) residue sau DeleteField trên bảng dùng chung, hoặc (b) field unique bị migration 000051 coverage-guard BỎ QUA demote (dữ liệu tenant chưa dọn dupe) — chạy Doctor D1_PIVOT_RECONCILE rồi re-run migration sau khi dọn dữ liệu. KHÔNG tự DROP: cột generated được tái dùng khi field trùng tên được tạo lại (grace-window). Xác nhận rồi xử lý tay",
							ix, table),
					})
					if len(findings) > doctor.MaxFindingsPerCheck {
						break
					}
				}
			}
			return findings, nil
		},
	}
}

// ---- B5: physical rel_* table with no registry row — report-only -------------------------------

func (s *DoctorService) checkUnregisteredRelTables() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckUnregisteredRelTables,
		Severity:    doctor.SeverityInfo,
		AutoFixSafe: false,
		ReportOnly:  true, // the grace-window is a FEATURE: re-registering the pair resurrects old links
		Detect: func(ctx context.Context, _ string) ([]doctor.Finding, error) {
			rows, err := s.store.ListOrphanRelTables(ctx, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.B5 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				age := "tuổi bảng không rõ (CREATE_TIME NULL)"
				if r.CreatedAt.Valid {
					age = "tạo lúc " + r.CreatedAt.Time.UTC().Format(time.RFC3339)
				}
				findings = append(findings, doctor.Finding{
					Key: r.TableName,
					Detail: fmt.Sprintf("bảng %s tồn tại nhưng không quan hệ N-N active nào (mọi tenant) đăng ký — grace-window là FEATURE: đăng ký lại đúng cặp entity sẽ hồi sinh link cũ. KHÔNG tự DROP; %s",
						r.TableName, age),
				})
			}
			return findings, nil
		},
	}
}

// ---- C8: relation field pointing at a dead entity — report-only --------------------------------

func (s *DoctorService) checkDeadRelationTargets() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckDeadRelationTarget,
		Severity:    doctor.SeverityWarn, // record writes on the field 422 (RECORD_RELATION_INVALID) until resolved
		AutoFixSafe: false,
		ReportOnly:  true, // the DeleteField flow owns the cascade (registry row + column) — don't fork it
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListDeadRelationTargets(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C8 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				findings = append(findings, doctor.Finding{
					Key: r.FieldID,
					Detail: fmt.Sprintf("field relation %s.%s trỏ entity %q đã xóa — gỡ bằng flow DeleteField (DELETE /admin/fields/%s) để field + registry row đi cùng nhau, hoặc tạo lại entity đích",
						r.EntityName, r.FieldName, r.RelatedEntity, r.FieldID),
				})
			}
			return findings, nil
		},
	}
}
