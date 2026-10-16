// doctor_service.go — DOCTOR-CORE (../specs/008-nexus-doctor/spec.md §4 lát 1): builds the four
// soft-metadata checks (B1 phantom relationships, B2 orphan 1-N/1-1 registry rows, C1 layouts
// referencing dead fields, C2 orphan app-nav items) over the EXISTING repair helpers, and
// orchestrates scan/fix through the doctor.Runner. These four are zero-DDL (tenant-scoped
// sys_* rows only); the five information_schema checks (A1/A2a/A5/A6/B4 — DOCTOR-SCHEMA,
// spec §4 lát 2) live in doctor_schema_checks.go and execute DDL through doctorSchemaRepo;
// the operational checks (C4 zombie workflows + report-only A3/B5/C8) and the flush-caches
// action (DOCTOR-OPS, spec §4 lát 3) live in doctor_ops_checks.go; the D-class record-data checks
// live in doctor_pivot_checks.go (D1) and doctor_currency_check.go (D2, report-only).
//
// Reuse discipline (spec §2.1 — one function body, never a mirror):
//   - B1 fix = metadata.Repository.SoftDeleteRelationshipByID — the per-row equivalent of
//     migration 000038's tombstone UPDATE (tombstone-rename name/join_table_name, then
//     soft-delete), so Doctor B1 IS 000038 run on demand.
//   - B2 fix = the SAME SoftDeleteRelationshipByID the codebase already calls the
//     "in-product repair path" for orphaned registry rows (relation lifecycle W2).
//   - C1 fix persists EXACTLY what applyServeTimeTolerance (layout_service.go) serves — the
//     serve-time prune promoted to a persisted repair; a layout of a tombstoned entity is
//     hard-deleted (LayoutRepository.HardDelete, the same revert-to-default semantic).
//   - C2 fix = appmeta.PurgeNavForRef via the doctor store — the INV-4 cascade helper.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// Check IDs (spec §2.2 numbering).
const (
	DoctorCheckPhantomRelationships = "B1_PHANTOM_RELATIONSHIPS"
	DoctorCheckOrphanRelationRows   = "B2_ORPHAN_RELATION_ROWS"
	DoctorCheckLayoutDeadFields     = "C1_LAYOUT_DEAD_FIELDS"
	DoctorCheckOrphanNavItems       = "C2_ORPHAN_NAV_ITEMS"
)

// doctorLayoutScanWindow bounds the C1 layout scan (see SQLStore.ListLayoutRows's ponytail
// note: ≥600 entities × 3 views before a tenant ever hits it).
const doctorLayoutScanWindow = 2000

// doctorStore is the consumer-side slice of *doctor.SQLStore the checks need (detect queries
// + the shared nav purge). The second block is the DOCTOR-SCHEMA detect surface: tenant-pinned
// sys_* list queries plus BATCHED information_schema probes (one IN-list query per scan, never
// a probe per row).
type doctorStore interface {
	ListPhantomRelationships(ctx context.Context, tenantID string, limit int) ([]doctor.RelationshipRow, error)
	IsPhantomRelationship(ctx context.Context, tenantID, relID string) (bool, error)
	ListOrphanRelationRows(ctx context.Context, tenantID string, limit int) ([]doctor.RelationshipRow, error)
	IsOrphanRelationRow(ctx context.Context, tenantID, relID string) (bool, error)
	ListLayoutRows(ctx context.Context, tenantID string, limit int) ([]doctor.LayoutRow, error)
	ListOrphanNavRefs(ctx context.Context, tenantID string, limit int) ([]doctor.NavRef, error)
	CountOrphanNavRows(ctx context.Context, tenantID, itemType, refID string) (int, error)
	PurgeNavRef(ctx context.Context, tenantID, itemType, refID string) error

	// DOCTOR-SCHEMA (A1/A2a/A5/A6/B4) detect surface.
	ListPhysicalFieldRows(ctx context.Context, tenantID string, limit int) ([]doctor.PhysicalFieldRow, error)
	GetPhysicalFieldRow(ctx context.Context, tenantID, fieldID string) (*doctor.PhysicalFieldRow, error)
	ListActiveEntities(ctx context.Context, tenantID string, limit int) ([]doctor.EntityRef, error)
	ListActiveNNRelationships(ctx context.Context, tenantID string, limit int) ([]doctor.NNRelationshipRow, error)
	GetActiveNNRelationship(ctx context.Context, tenantID, relID string) (*doctor.NNRelationshipRow, error)
	ListExistingTables(ctx context.Context, tables []string) (map[string]bool, error)
	ListTableColumns(ctx context.Context, tables []string) ([]doctor.TableColumn, error)
	ListTableIndexes(ctx context.Context, tables []string) ([]doctor.IndexColumn, error)

	// DOCTOR-OPS (C4 zombie workflows + report-only A3/B5/C8) surface. DisableWorkflow is the
	// one bounded write (C4 fix — disable, never delete; see doctor/ops_store.go for why the
	// admin workflow update path is not reusable). ListOrphanRelTables is deliberately
	// tenant-agnostic (physical rel_* tables are server-global; documented exception).
	ListZombieWorkflows(ctx context.Context, tenantID string, limit int) ([]doctor.ZombieWorkflowRow, error)
	GetZombieWorkflow(ctx context.Context, tenantID, wfID string) (*doctor.ZombieWorkflowRow, error)
	DisableWorkflow(ctx context.Context, tenantID, wfID string) (int64, error)
	ListOrphanRelTables(ctx context.Context, limit int) ([]doctor.OrphanRelTable, error)
	ListPhysicalFieldRowsByEntityNames(ctx context.Context, names []string, limit int) ([]doctor.PhysicalFieldRow, error)
	ListDeadRelationTargets(ctx context.Context, tenantID string, limit int) ([]doctor.DeadRelationTargetRow, error)
}

// doctorSchemaRepo is the consumer-side slice of *dynrepo.Repository the DOCTOR-SCHEMA fixes
// execute through — every method wraps a ddl.Build* statement (spec §2.1: fix via existing
// helper, never bespoke SQL). All of them are ADDITIVE or in-place-rebuild DDL on the SHARED
// obj_*/rel_* tables: safe for co-tenants by construction (adding a missing column/index or
// re-guarding a generated column serves every tenant sharing the table; nothing here drops
// data or a co-tenant's column).
type doctorSchemaRepo interface {
	// AddUniqueGencolNonUnique (RM-28 Slice B) — the A1 repair for a missing unique field's
	// gencol now materializes a NON-unique idx_ index (uniqueness lives solely in the
	// sys_unique_index pivot); a physical UNIQUE index would re-couple every tenant sharing
	// this entity name.
	AddUniqueGencolNonUnique(ctx context.Context, entity, field string) error
	AddRelationColumn(ctx context.Context, entity, field string) error
	AddOneToOneRelationColumn(ctx context.Context, entity, field string) error
	AddIndexedColumn(ctx context.Context, entity, field, fieldType string) error
	// AddPolymorphicColumns materializes a polymorphic_relation field's two gen-columns +
	// composite index (ACTIVITY-CORE #3a) — the A1 repair for a missing polymorphic column.
	AddPolymorphicColumns(ctx context.Context, entity, field string) error
	GuardUniqueColumn(ctx context.Context, entity, field string) error
	GuardOneToOneRelationColumn(ctx context.Context, entity, field string) error
	AddMandatoryColumn(ctx context.Context, entity, column string) error
	EnsureMandatoryIndex(ctx context.Context, entity, index string, rebuild bool) error
	CreateTable(ctx context.Context, entity string, fields []ddl.FieldDef) error
	CreateJoinTable(ctx context.Context, entityA, entityB string) error

	// RM-28 A2 (D1 pivot reconcile) — the sys_unique_index read/write surface. The two READ
	// methods (CountMissingPivotRows/ListOrphanPivotIDs/GetPivotRow/PivotRecordLive) are safe
	// to call from Detect (nothing here mutates except BackfillUniqueField/DeletePivotRow,
	// which the D1 Fix alone calls) — obj_*/sys_unique_index dynamic-table SQL only exists in
	// dynrepo (ddl.SanitizeIdentifier), which is why these live on doctorSchemaRepo rather than
	// doctorStore (mirrors how ColumnExists already mixes read+write on FieldSchemaRepo).
	BackfillUniqueField(ctx context.Context, entity, tenantID, field string) (*dynrepo.BackfillResult, error)
	CountMissingPivotRows(ctx context.Context, entity, tenantID, field string) (int, error)
	ListOrphanPivotIDs(ctx context.Context, entity, tenantID, field string, limit int) ([]string, error)
	GetPivotRow(ctx context.Context, tenantID, id string) (*dynrepo.PivotRow, error)
	PivotRecordLive(ctx context.Context, tenantID, entity, field, recordID, value string) (bool, error)
	DeletePivotRow(ctx context.Context, tenantID, id string) error

	// D2 (queue #63) — read-only: live rows whose stored currency code differs from the field's
	// configured one. obj_* JSON SQL lives in dynrepo (ddl.SanitizeIdentifier), hence here.
	ListCurrencyCodeMismatches(ctx context.Context, entity, tenantID, field, want string, limit int) ([]dynrepo.CurrencyCodeMismatch, error)
}

// doctorRelRepo is the one existing repair helper B1/B2 reuse (*metadata.Repository).
type doctorRelRepo interface {
	SoftDeleteRelationshipByID(ctx context.Context, tenantID, relID string) error
}

// doctorLayoutRepo is the slice of *metadata.LayoutRepository the C1 fix needs.
type doctorLayoutRepo interface {
	GetByID(ctx context.Context, tenantID, id string) (*metadata.LayoutDefinition, error)
	UpdateByID(ctx context.Context, tenantID, id, layoutJSON string) error
	HardDelete(ctx context.Context, tenantID, id string) error
}

// DoctorService orchestrates the Nexus Doctor: Scan (read-only, absolutely) and Fix
// (re-detect per item → repair via existing helper → audit row + zap → cache invalidation
// after the batch). Admin-only at the route layer (RequireAdmin).
type DoctorService struct {
	store   doctorStore
	schema  doctorSchemaRepo
	rels    doctorRelRepo
	meta    LayoutMetaReader // GetEntityByName + ListFields — the RAW repo, not the cache (a doctor must see DB truth)
	layouts doctorLayoutRepo
	cache   *MetaCache // optional; nil-safe (InvalidateAll no-ops)
	// aclCache / wfCache are two more in-proc TTL caches (DOCTOR-OPS): the flush-caches action
	// clears all four (with userCache below), and the C4 fix invalidates the disabled rule's workflow-cache
	// entry. Optional; nil-safe (WithOpsCaches).
	aclCache *CachedACLLoader
	wfCache  *workflow.CachedLoader
	// userCache is the live user-row cache the Auth stage rebuilds every request's Subject from
	// (queue #14 claim-freshness); flush-caches clears it too, so an operator's "flush every
	// cache" really covers every in-proc TTL cache. nil-safe (WithUserClaimCache).
	userCache *CachedUserLoader
	auditor   audit.Auditor
	logger    *zap.Logger
	runner    *doctor.Runner
	// approvalSeats feeds D3 (specs/043 S3a2): pending approvals still read through the projection
	// fallback. Optional (WithApprovalSeats); unwired, D3 is not registered at all.
	approvalSeats doctorApprovalSeats
	// approvalDuplicates feeds D4 (specs/043 S9a): records holding two PENDING approvals, which
	// Migration B refuses to migrate over. Optional (WithApprovalDuplicates); unwired, D4 is absent.
	approvalDuplicates doctorApprovalDuplicates
	// fieldRestrictions feeds C9 (queue #143): field-ACL rows that leave a role unable to create.
	// Optional (WithFieldRestrictions); unwired, C9 is not registered at all.
	fieldRestrictions doctorFieldRestrictions
	// vault + legacyRules feed C10 (specs/050 S3b): inline webhook keys moved into the vault.
	// Optional (WithInlineSecretMigration); unwired, C10 is not registered at all.
	vault           doctorCredentialVault
	legacyRules     doctorLegacyRules
	pendingWebhooks doctorPendingWebhooks
	// keyHealth feeds C11/C12 (specs/050 S7): vault rows on a retired / missing master key.
	// Optional (WithCredentialKeyChecks); unwired, neither is registered.
	keyHealth doctorCredentialKeyHealth
}

// NewDoctorService wires the doctor over the existing repositories. meta must be the RAW
// metadata repository (NOT the MetaCache) so detect never reports against a stale cached
// definition; schema is the sqlx dynamic-record repository the DOCTOR-SCHEMA fixes execute
// DDL through. auditor/logger may be nil (defaulted to noop, matching the other services).
func NewDoctorService(store doctorStore, schema doctorSchemaRepo, rels doctorRelRepo, meta LayoutMetaReader, layouts doctorLayoutRepo, auditor audit.Auditor, logger *zap.Logger) *DoctorService {
	if auditor == nil {
		auditor = audit.NewNoopAuditor()
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &DoctorService{store: store, schema: schema, rels: rels, meta: meta, layouts: layouts, auditor: auditor, logger: logger}
	s.runner = doctor.NewRunner(s.checks())
	return s
}

// checks is the registry in scan order. A check whose reader is optional (D3 — WithApprovalSeats)
// is registered only when wired: an unwired reader would report a clean count it never measured.
func (s *DoctorService) checks() []doctor.Check {
	list := []doctor.Check{
		s.checkMissingGenColumns(),      // A1
		s.checkUnguardedUniqueColumns(), // A2a
		s.checkOrphanUniqueIndexes(),    // A3 (report-only)
		s.checkMandatoryDDL(),           // A5
		s.checkMissingObjTables(),       // A6 (confirm)
		s.checkPivotReconcile(),         // D1 (RM-28 A2 — pivot bidirectional reconcile)
		s.checkCurrencyCodeMismatch(),   // D2 (queue #63 — report-only: stored currency code ≠ configured)
	}
	if s.approvalSeats != nil {
		list = append(list, s.checkApprovalSeatDrift()) // D3 (specs/043 S3a2 — report-only: inbox on the projection fallback)
	}
	if s.approvalDuplicates != nil {
		list = append(list, s.checkApprovalDuplicatePending()) // D4 (specs/043 S9a — report-only: pairs Migration B refuses)
	}
	list = append(list,
		s.checkPhantomRelationships(),  // B1
		s.checkOrphanRelationRows(),    // B2
		s.checkMissingJoinTables(),     // B4 (confirm)
		s.checkUnregisteredRelTables(), // B5 (report-only)
		s.checkLayoutDeadFields(),      // C1
		s.checkOrphanNavItems(),        // C2
		s.checkZombieWorkflows(),       // C4 (confirm)
		s.checkDeadRelationTargets(),   // C8 (report-only)
	)
	if s.fieldRestrictions != nil {
		list = append(list, s.checkLockedRequiredFields()) // C9 (queue #143 — report-only: a role that cannot create)
	}
	if s.vault != nil && s.legacyRules != nil && s.pendingWebhooks != nil {
		list = append(list, s.checkWebhookInlineSecret()) // C10 (specs/050 S3b — confirm: inline webhook keys into the vault)
	}
	list = append(list, s.credentialKeyChecks()...) // C11/C12 (specs/050 S7 — report-only: master key rotation / loss)
	return list
}

// WithMetaCache attaches the shared MetaCache for the post-fix blanket invalidation (same
// chainable pattern as EntityService.WithMetaCache).
func (s *DoctorService) WithMetaCache(c *MetaCache) *DoctorService {
	s.cache = c
	return s
}

// Scan runs every check read-only and returns the findings (spec §2.3 POST /admin/doctor/scan).
func (s *DoctorService) Scan(ctx context.Context, tenantID string) ([]doctor.CheckResult, error) {
	results, err := s.runner.Scan(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.Doctor.Scan: %w", err)
	}
	return results, nil
}

// Fix repairs the requested checks (spec §2.3 POST /admin/doctor/fix). Unknown ids map to
// 422 DOCTOR_CHECK_UNKNOWN, a needs-confirm check without confirm to 422
// DOCTOR_CONFIRM_REQUIRED — both refuse the WHOLE request before any mutation. After the
// batch, the MetaCache is blanket-invalidated when anything was actually fixed (repairs must
// be visible to the next reads immediately, not after the 60s TTL backstop).
func (s *DoctorService) Fix(ctx context.Context, tenantID string, checkIDs []string, confirm bool) ([]doctor.FixResult, error) {
	if len(checkIDs) == 0 {
		return nil, invalid("DOCTOR_CHECK_UNKNOWN", "checkIds rỗng — chọn ít nhất một check")
	}
	results, err := s.runner.Fix(ctx, tenantID, checkIDs, confirm)
	if err != nil {
		if errors.Is(err, doctor.ErrUnknownCheck) {
			return nil, invalid("DOCTOR_CHECK_UNKNOWN", "%v", err)
		}
		if errors.Is(err, doctor.ErrReportOnly) {
			return nil, invalid("DOCTOR_REPORT_ONLY", "%v", err)
		}
		if errors.Is(err, doctor.ErrConfirmRequired) {
			return nil, invalid("DOCTOR_CONFIRM_REQUIRED", "%v", err)
		}
		return nil, fmt.Errorf("services.Doctor.Fix: %w", err)
	}
	fixed := 0
	for i := range results {
		fixed += results[i].Fixed
		// Security W-1: per-item failure detail must land in the SERVER log (a self-repair
		// tool whose failures live only in one response body is untraceable), and the client
		// gets a short stable message instead of raw driver/wrap internals. Exception: an
		// error the check itself typed public (doctorPublicErrf → *doctorPublicError — static
		// text + names we own, e.g. the A2a CROSS_TENANT_SHARED_UNIQUE verdict — RM-28 Slice B: the
		// residual case where a field's index is still physically uq_) passes through
		// so the admin sees an actionable verdict, not a "see server log" dead end. Detection
		// is errors.As on the RAW error chain (ItemErrors, 1:1 with Errors) — a typed
		// sentinel, never a substring marker.
		for j, msg := range results[i].Errors {
			s.logger.Warn("services.Doctor.Fix: item failed",
				zap.String("tenant_id", tenantID),
				zap.String("check_id", results[i].CheckID),
				zap.String("error", msg),
			)
			prefix := ""
			var itemErr error
			if j < len(results[i].ItemErrors) {
				if key := results[i].ItemErrors[j].Key; key != "" {
					prefix = key + ": "
				}
				itemErr = results[i].ItemErrors[j].Err
			}
			var pub *doctorPublicError
			if errors.As(itemErr, &pub) {
				results[i].Errors[j] = prefix + pub.msg
				continue
			}
			results[i].Errors[j] = prefix + "sửa thất bại — xem server log"
		}
	}
	if fixed > 0 {
		s.cache.InvalidateAll()
	}
	return results, nil
}

// recordFix emits the per-fix audit row (RM-19 trail, async + fail-open like every
// emitMetaAudit caller) and the structured log. One uniform shape for every check: the
// after-side diff carries what was repaired.
func (s *DoctorService) recordFix(ctx context.Context, tenantID, checkID string, f doctor.Finding) {
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: audit.ActorFromContext(ctx),
		EntityName:  audit.DoctorMeta,
		Action:      audit.ActionUpdate,
		Diff:        audit.ComputeDiff(nil, map[string]any{"checkId": checkID, "key": f.Key, "detail": f.Detail}),
		At:          time.Now().UTC(),
	})
	s.logger.Info("services.Doctor: fixed finding",
		zap.String("tenant_id", tenantID),
		zap.String("check_id", checkID),
		zap.String("key", f.Key),
	)
}

// ---- B1: phantom relationship rows (migration 000038 run on demand) --------------------------

func (s *DoctorService) checkPhantomRelationships() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckPhantomRelationships,
		Severity:    doctor.SeverityWarn, // ghost ERD edges + UNIQUE-slot squat → 409 brick on recreate
		AutoFixSafe: true,
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListPhantomRelationships(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.B1 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				findings = append(findings, doctor.Finding{
					Key: r.ID,
					Detail: fmt.Sprintf("quan hệ %q (%s, %s → %s) trỏ entity đã xóa — sẽ tombstone + soft-delete",
						r.Name, r.RelationshipType, r.ParentEntityName, r.ChildEntityName),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			// Re-detect the single item before mutating (never trust a stale report).
			still, err := s.store.IsPhantomRelationship(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.B1 re-detect: %w", err)
			}
			if !still {
				return doctor.ErrAlreadyClean
			}
			if err := s.rels.SoftDeleteRelationshipByID(ctx, tenantID, f.Key); err != nil {
				if errors.Is(err, metadata.ErrRelationshipNotFound) {
					return doctor.ErrAlreadyClean // fixed concurrently between re-detect and repair
				}
				return fmt.Errorf("services.Doctor.B1 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckPhantomRelationships, f)
			return nil
		},
	}
}

// ---- B2: orphan 1-N/1-1 registry rows (DeleteField step-3 best-effort window) -----------------

func (s *DoctorService) checkOrphanRelationRows() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckOrphanRelationRows,
		Severity:    doctor.SeverityWarn, // stale inverse relations advertised via GET /metadata/relationships
		AutoFixSafe: true,
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListOrphanRelationRows(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.B2 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(rows))
			for _, r := range rows {
				findings = append(findings, doctor.Finding{
					Key: r.ID,
					Detail: fmt.Sprintf("registry %q (%s): child_fk_column %q không còn field relation active trên %q — sẽ soft-delete",
						r.Name, r.RelationshipType, r.ChildFKColumn, r.ChildEntityName),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			still, err := s.store.IsOrphanRelationRow(ctx, tenantID, f.Key)
			if err != nil {
				return fmt.Errorf("services.Doctor.B2 re-detect: %w", err)
			}
			if !still {
				return doctor.ErrAlreadyClean
			}
			if err := s.rels.SoftDeleteRelationshipByID(ctx, tenantID, f.Key); err != nil {
				if errors.Is(err, metadata.ErrRelationshipNotFound) {
					return doctor.ErrAlreadyClean
				}
				return fmt.Errorf("services.Doctor.B2 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckOrphanRelationRows, f)
			return nil
		},
	}
}

// ---- C1: layouts referencing dead fields (persist the serve-time prune) -----------------------

// doctorPruneLayout decodes the stored layout_json, applies the SAME serve-time prune the
// read path applies (applyServeTimeTolerance — one prune body, never a mirror), and reports
// whether persisting the pruned form would change anything. A malformed/empty layout_json
// errors — C1's class is dead FIELD refs, not corrupt JSON (the read path already falls back
// to the default layout for those).
func doctorPruneLayout(layoutJSON, viewType string, liveFields []metadata.FieldDefinition) (pruned map[string]any, changed bool, err error) {
	var orig map[string]any
	if err := json.Unmarshal([]byte(layoutJSON), &orig); err != nil {
		return nil, false, fmt.Errorf("unmarshal: %w", err)
	}
	pruned, err = applyServeTimeTolerance(layoutJSON, viewType, liveFields)
	if err != nil {
		return nil, false, err
	}
	return pruned, !reflect.DeepEqual(orig, pruned), nil
}

func (s *DoctorService) checkLayoutDeadFields() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckLayoutDeadFields,
		Severity:    doctor.SeverityInfo, // serve path already tolerates; the stored JSON stays dirty
		AutoFixSafe: true,
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			rows, err := s.store.ListLayoutRows(ctx, tenantID, doctorLayoutScanWindow)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C1 detect: %w", err)
			}
			// Memoize live fields per entity — layouts come ≤3 per entity, so this keeps the
			// scan at 2 metadata reads per entity instead of per layout row.
			type entityFields struct {
				fields []metadata.FieldDefinition
				dead   bool
			}
			memo := make(map[string]entityFields)
			findings := make([]doctor.Finding, 0)
			for _, row := range rows {
				if len(findings) > doctor.MaxFindingsPerCheck {
					break // cap+1 collected — runner reports truncation
				}
				ef, ok := memo[row.EntityName]
				if !ok {
					entity, err := s.meta.GetEntityByName(ctx, tenantID, row.EntityName)
					switch {
					case errors.Is(err, metadata.ErrEntityNotFound):
						ef = entityFields{dead: true}
					case err != nil:
						return nil, fmt.Errorf("services.Doctor.C1 detect: resolve entity %q: %w", row.EntityName, err)
					default:
						fields, err := s.meta.ListFields(ctx, tenantID, entity.ID)
						if err != nil {
							return nil, fmt.Errorf("services.Doctor.C1 detect: list fields %q: %w", row.EntityName, err)
						}
						ef = entityFields{fields: fields}
					}
					memo[row.EntityName] = ef
				}
				if ef.dead {
					findings = append(findings, doctor.Finding{
						Key: row.ID,
						Detail: fmt.Sprintf("layout %s của entity đã xóa %q — sẽ xóa hẳn row (revert-to-default)",
							row.ViewType, row.EntityName),
					})
					continue
				}
				_, dirty, err := doctorPruneLayout(row.LayoutJSON, row.ViewType, ef.fields)
				if err != nil {
					// Corrupt stored JSON is NOT C1's class (read path falls back to the default
					// layout) — log and move on rather than failing the whole scan.
					s.logger.Warn("services.Doctor.C1: malformed layout_json skipped",
						zap.String("tenant_id", tenantID), zap.String("layout_id", row.ID), zap.Error(err))
					continue
				}
				if dirty {
					findings = append(findings, doctor.Finding{
						Key: row.ID,
						Detail: fmt.Sprintf("layout %s của %q còn tham chiếu field đã xóa — sẽ lưu lại bản đã prune",
							row.ViewType, row.EntityName),
					})
				}
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			// Re-detect from the CURRENT row — re-resolve, re-prune, only then persist.
			row, err := s.layouts.GetByID(ctx, tenantID, f.Key)
			if err != nil {
				if errors.Is(err, metadata.ErrLayoutNotFound) {
					return doctor.ErrAlreadyClean
				}
				return fmt.Errorf("services.Doctor.C1 re-detect: %w", err)
			}
			entity, err := s.meta.GetEntityByName(ctx, tenantID, row.EntityName)
			if errors.Is(err, metadata.ErrEntityNotFound) {
				// Layout of a tombstoned entity → hard-delete the row (spec §2.2 C1 fix).
				if err := s.layouts.HardDelete(ctx, tenantID, f.Key); err != nil {
					if errors.Is(err, metadata.ErrLayoutNotFound) {
						return doctor.ErrAlreadyClean
					}
					return fmt.Errorf("services.Doctor.C1 fix: hard delete: %w", err)
				}
				s.recordFix(ctx, tenantID, DoctorCheckLayoutDeadFields, f)
				return nil
			}
			if err != nil {
				return fmt.Errorf("services.Doctor.C1 re-detect: resolve entity: %w", err)
			}
			fields, err := s.meta.ListFields(ctx, tenantID, entity.ID)
			if err != nil {
				return fmt.Errorf("services.Doctor.C1 re-detect: list fields: %w", err)
			}
			pruned, dirty, err := doctorPruneLayout(row.LayoutJSON, row.ViewType, fields)
			if err != nil {
				return fmt.Errorf("services.Doctor.C1 fix: prune: %w", err)
			}
			if !dirty {
				return doctor.ErrAlreadyClean
			}
			buf, err := json.Marshal(pruned)
			if err != nil {
				return fmt.Errorf("services.Doctor.C1 fix: marshal: %w", err)
			}
			if err := s.layouts.UpdateByID(ctx, tenantID, f.Key, string(buf)); err != nil {
				if errors.Is(err, metadata.ErrLayoutNotFound) {
					return doctor.ErrAlreadyClean
				}
				return fmt.Errorf("services.Doctor.C1 fix: persist: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckLayoutDeadFields, f)
			return nil
		},
	}
}

// ---- C2: orphan app-nav items (INV-4 cascade window) ------------------------------------------

// navRefKey encodes a C2 finding key as "<itemType>:<refID>" — the purge unit of
// appmeta.PurgeNavForRef (a dead ref may sit in several apps' navs; one fix clears them all).
func navRefKey(itemType, refID string) string { return itemType + ":" + refID }

func (s *DoctorService) checkOrphanNavItems() doctor.Check {
	return doctor.Check{
		ID:          DoctorCheckOrphanNavItems,
		Severity:    doctor.SeverityInfo, // read path already drops orphans silently (GetNav)
		AutoFixSafe: true,
		Detect: func(ctx context.Context, tenantID string) ([]doctor.Finding, error) {
			refs, err := s.store.ListOrphanNavRefs(ctx, tenantID, doctor.MaxFindingsPerCheck+1)
			if err != nil {
				return nil, fmt.Errorf("services.Doctor.C2 detect: %w", err)
			}
			findings := make([]doctor.Finding, 0, len(refs))
			for _, r := range refs {
				findings = append(findings, doctor.Finding{
					Key: navRefKey(r.ItemType, r.RefID),
					Detail: fmt.Sprintf("%d nav item trỏ %s %q đã xóa — sẽ xóa các row tham chiếu",
						r.Rows, r.ItemType, r.RefID),
				})
			}
			return findings, nil
		},
		Fix: func(ctx context.Context, tenantID string, f doctor.Finding) error {
			itemType, refID, ok := strings.Cut(f.Key, ":")
			if !ok || itemType == "" || refID == "" {
				return fmt.Errorf("services.Doctor.C2 fix: malformed finding key %q", f.Key)
			}
			n, err := s.store.CountOrphanNavRows(ctx, tenantID, itemType, refID)
			if err != nil {
				return fmt.Errorf("services.Doctor.C2 re-detect: %w", err)
			}
			if n == 0 {
				return doctor.ErrAlreadyClean
			}
			if err := s.store.PurgeNavRef(ctx, tenantID, itemType, refID); err != nil {
				return fmt.Errorf("services.Doctor.C2 fix: %w", err)
			}
			s.recordFix(ctx, tenantID, DoctorCheckOrphanNavItems, f)
			return nil
		},
	}
}
