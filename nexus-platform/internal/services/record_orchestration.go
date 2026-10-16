package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go.uber.org/zap"
	"strconv"
	"strings"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// ErrEntityNotFound and ErrRecordNotFound are the not-found sentinels the handler maps to
// HTTP 404. ErrRecordNotFound aliases the repo error so callers only import this package.
var (
	ErrEntityNotFound = errors.New("entity not found")
	ErrRecordNotFound = dynrepo.ErrRecordNotFound
	// ErrWriteConflict aliases the repo's transient write-contention sentinel (RM-28: MySQL
	// deadlock 1213 / lock-wait timeout 1205 on the obj_ write or a pivot statement) so the
	// handler branches on the services symbol — mirroring the ErrRecordNotFound alias.
	ErrWriteConflict = dynrepo.ErrWriteConflict
)

// EntityCatalog reads entity + field metadata needed to validate and route records.
type EntityCatalog interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// RecordStore performs the dynamic CRUD against obj_* tables. The read/write methods carry a
// dynrepo.RowScope so the store enforces A5 team-based row-level security (owner/team/all/none)
// on top of the mandatory tenant_id filter — see internal/dynrepo/rowscope.go.
type RecordStore interface {
	// InsertRecord inserts a record and dual-writes the uniqueness pivot (RM-28): uniqueFields is
	// the entity's active unique fields (resolved by the service from metadata) so the repo can
	// maintain sys_unique_index in the same tx — the repo cannot import metadata itself.
	InsertRecord(ctx context.Context, entity, tenantID, userID string, data map[string]any, uniqueFields []dynrepo.UniqueFieldSpec) (string, error)
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
	ListRecords(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.ListParams) ([]dynrepo.Record, int, error)
	// ListRecordsCursor executes a keyset (cursor) paginated query (D2). It omits COUNT(*) so
	// every page is O(limit) regardless of depth. Returns records + opaque nextCursor ("" when
	// no more pages). The caller must ensure p.Sort names a physical indexed column (checked
	// against the sort allowlist before calling).
	ListRecordsCursor(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.CursorParams) ([]dynrepo.Record, string, error)
	// UpdateRecord applies a partial merge and reconciles the pivot for every unique field the
	// patch touched (RM-28). uniqueFields is the entity's active unique fields.
	UpdateRecord(ctx context.Context, entity, tenantID, id, userID string, scope dynrepo.RowScope, data map[string]any, expectedVersion *int, uniqueFields []dynrepo.UniqueFieldSpec) error
	SoftDeleteRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) error
	// RestoreRecord clears a soft-deleted record's deleted_at (tenant-scoped + row-level scoped),
	// the inverse of SoftDeleteRecord (W9b recycle bin). A record that is not soft-deleted / not
	// visible to the scope / cross-tenant surfaces as ErrRecordNotFound. A unique collision with a
	// live row created after the delete surfaces as *DuplicateUniqueError (→ 409 RECORD_DUPLICATE).
	RestoreRecord(ctx context.Context, entity, tenantID, id, userID string, scope dynrepo.RowScope, uniqueFields []dynrepo.UniqueFieldSpec) error
	// RecordExists reports whether a live record with the given id exists in the entity's
	// obj_* table for the tenant. Used by relation (1-N) FK validation (M3-T2) to confirm a
	// child's relation value points to a real parent record in the SAME tenant. It is NOT
	// row-level scoped: FK integrity must see any record in the tenant, not just the caller's.
	RecordExists(ctx context.Context, entity, tenantID, id string) (bool, error)
	// BatchRecordExists checks in ONE query per entity whether a set of ids are all live
	// (non-soft-deleted) in the given entity's obj_* table for the tenant (RM-11). Returns
	// id → true for every id found; absent ids are not in the map.
	BatchRecordExists(ctx context.Context, entity, tenantID string, ids []string) (map[string]bool, error)
	// AggregateScalar runs a non-grouped aggregate (SUM/AVG/MIN/MAX/COUNT) over the
	// tenant-scoped + row-scoped + filtered set, returning the metric value and the matched
	// row count (EPIC 6 M1). SUM/AVG/MIN/MAX CAST the JSON value to DECIMAL so currency/number
	// aggregates are exact (no float drift).
	AggregateScalar(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.AggregateParams) (dynrepo.AggregateScalar, error)
	// AggregateGrouped runs a GROUP BY aggregate over an indexed physical column, returning up
	// to GroupLimit groups (key, value, count). Scoping/filtering match AggregateScalar.
	AggregateGrouped(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error)
	// AggregateGrid runs the multi-level / matrix report grid (specs/049): ONE GROUP BY … WITH
	// ROLLUP over 1..3 axes (leaves, sub-totals, grand total), plus the column totals of a matrix
	// in the same read-only snapshot. Scoping/filtering match AggregateGrouped; more than 1000
	// leaf groups → *dynrepo.ErrGridTooLarge (rejected, never truncated).
	AggregateGrid(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.GridParams) (rows, colTotals []dynrepo.GridRow, err error)
	// SetOwner reassigns a record's current owner (TRỤ 2 mục 2 "Giao việc & đổi chủ"). It
	// writes ONLY owner_id (+ modified_by) — created_by is never touched. Row-level scoped
	// like every other write here (defense-in-depth; RecordService.SetOwner already requires
	// access_level "all" or admin before calling this).
	SetOwner(ctx context.Context, entity, tenantID, id, userID string, scope dynrepo.RowScope, newOwnerID string) error
}

// RecordScope is the resolved A5 row-level access context the handler hands to a read/write:
// the VERIFIED user id (auth.Subject.UserID) and the access_level the ACL stage resolved for
// the action (none/owner/team/all). The service translates it to a dynrepo.RowScope so the
// store can enforce row-level security. IsAdmin lets an admin caller see every row in the
// tenant explicitly (the ACL stage already bypasses for admins).
type RecordScope struct {
	UserID      string
	AccessLevel string
	IsAdmin     bool
	// RoleID is the caller's resolved role (auth.Subject.RoleID). Used by ?expand to re-check
	// the caller's ACL on the RELATION TARGET entity (W5-A2) so a label cannot leak across an
	// entity the caller may not view. Empty for a principal with no role (→ expand denied).
	RoleID string
	// IncludeShares: this scope was resolved for the VIEW action, so owner/team reads also reach
	// the records manually shared with UserID (specs/051-record-manual-share). Set by the handler
	// (scopeOf) only when the ACL stage authorized "view"; every write keeps it false, and dynrepo
	// ignores it on write paths regardless (dynrepo.RowScope.IncludeShares).
	IncludeShares bool
}

// toRowScope converts the service-level scope to the repository's RowScope. An unrecognized /
// empty access level fails closed inside dynrepo (treated as "none"), so a mis-wired caller can
// never widen visibility.
func (s RecordScope) toRowScope() dynrepo.RowScope {
	return dynrepo.RowScope{
		UserID:        s.UserID,
		Level:         dynrepo.AccessLevel(s.AccessLevel),
		IsAdmin:       s.IsAdmin,
		IncludeShares: s.IncludeShares,
	}
}

// ToRowScope is the exported form of toRowScope for callers outside the services package
// (e.g. handlers) that need to convert a RecordScope to a dynrepo.RowScope to pass as a
// source scope for Link/Unlink (NN-02). Internal service code continues to use toRowScope.
func (s RecordScope) ToRowScope() dynrepo.RowScope { return s.toRowScope() }

// RecordService resolves an entity from metadata, validates payloads against its fields, and
// delegates CRUD to the record store. It is the engine behind the dynamic /:entity endpoints.

// recordInboxWriter writes an in-app notification. Satisfied by *workflow.SQLRepository — the same
// writer the automation engine uses, so an assignment notice and a rule's notice land in the same
// inbox with the same shape.
type recordInboxWriter interface {
	InsertNotification(ctx context.Context, n workflow.Notification) (string, error)
}

// WithInbox arms the assignment notice (SetOwner). Chainable; nil keeps assignment silent.
func (s *RecordService) WithInbox(w recordInboxWriter, logger *zap.Logger) *RecordService {
	s.inbox = w
	s.logger = logger
	return s
}

// fetcher is optional (nil = expand disabled); when non-nil it backs the ?expand= feature.
type RecordService struct {
	catalog EntityCatalog
	store   RecordStore
	fetcher LabelFetcher // optional; nil → ?expand is a no-op
	// lookupFetcher backs ENG-COMPUTED #2 (read-time lookup fields). Optional: nil → any
	// `lookup` field on the entity resolves to null (no fetch attempted) instead of an error,
	// mirroring the fetcher==nil-disables-expand convention. Production wires *dynrepo.Repository
	// via WithFieldValueFetcher (same concrete type as WithLabelFetcher).
	lookupFetcher FieldValueFetcher
	// authz gates ?expand by the caller's ACL on the relation TARGET entity (W5-A2). Optional:
	// nil leaves expand unchecked (preserves prior behaviour for unit tests); production wires
	// the ACLService via WithACLAuthorizer so a label cannot be harvested across an entity the
	// caller may not view. Also gates lookup resolution on the SOURCE entity (ENG-COMPUTED #2).
	authz entityAuthorizer
	// auditor records every committed mutation (RM-19), async + fail-open. Defaults to a
	// noop so existing wiring/tests run unchanged; the real AsyncDispatcher is injected via
	// WithAuditor. Never nil after the constructor.
	auditor audit.Auditor
	// engine fires workflow automations (EPIC 4 M1) AFTER a record mutation commits — async +
	// fail-open, exactly like the auditor. Defaults to a noop so existing wiring/tests run
	// unchanged; the real workflow.AsyncDispatcher is injected via WithEngine. Never nil after
	// the constructor.
	engine workflow.Engine
	// inbox writes the in-app notice sent when a record is handed to someone (SetOwner). Optional:
	// nil disables the notice, which is the pre-existing behaviour — assignment was silent.
	inbox recordInboxWriter
	// logger reports best-effort failures (a notice that did not send) without failing the action.
	logger *zap.Logger
	// approvals arms BPM approval instances (EPIC 5 M1) AFTER a record mutation commits — async +
	// fail-open, exactly like the workflow engine. Defaults to a noop so existing wiring/tests run
	// unchanged; the real bpm.AsyncEngine is injected via WithApprovalEngine. Never nil after the
	// constructor.
	approvals bpm.Engine
	// fieldACL enforces field-level security (RM-42 / A6): it strips fields the caller's role may
	// not view from every read response and rejects a write payload that touches a field the role
	// may not edit (403 FIELD_FORBIDDEN). Admin bypasses. Optional: nil leaves field-level
	// security DISABLED (default-allow every field — preserves prior behaviour for unit tests and
	// any caller that does not wire it). Production wires *FieldACLService via WithFieldACL.
	fieldACL fieldACLEnforcer
	// rollupMeta resolves cross-entity metadata (child entity + fields, N-N relationship
	// definitions) for ENG-COMPUTED #3 (?rollup=). Optional: nil disables ?rollup= entirely
	// (RECORD_ROLLUP_UNAVAILABLE). Production wires *metadata.Repository (same concrete type
	// satisfies rollupMetaResolver as MetadataWriter/FieldMetadataRepo).
	rollupMeta rollupMetaResolver
	// rollupLinked backs the N-N rollup path (pivot id-set via ListLinked). Optional: nil
	// disables N-N rollup only (FK-on-child rollup is unaffected). Production wires
	// *dynrepo.Repository.
	rollupLinked RollupLinkedLister
	// sequences backs the ENG #6 auto_number seam (assignSequences, Create-only). Optional: nil
	// makes Create fail closed with RECORD_AUTONUMBER_UNAVAILABLE the moment it encounters an
	// auto_number field — mirrors rollupMeta's nil-disables contract (RECORD_ROLLUP_UNAVAILABLE)
	// rather than silently persisting a record with a missing sequence value. An entity with NO
	// auto_number field is entirely unaffected by sequences being nil. Production wires
	// *dynrepo.Repository (same concrete type satisfies SequenceClaimer as every other optional
	// dependency here).
	sequences SequenceClaimer
	// bulkStore backs the ENG-BATCH #1 atomic bulk-create seam (CreateBulk's mode=atomic Phase 2,
	// record_bulk.go). Optional: nil makes CreateBulk fail closed with BULK_ATOMIC_UNAVAILABLE the
	// moment mode=atomic actually reaches Phase 2 (mirrors sequences' nil-disables contract) rather
	// than silently falling back to a non-atomic insert. mode=partial never touches this field (it
	// reuses Create()/store.InsertRecord per row instead). Production wires *dynrepo.Repository.
	bulkStore BulkInserter
	// userStore backs the USER-FIELD write-time existence probe (checkUserTargets, mirrors
	// relationExister for `relation`): auto-detected in NewRecordService via a type-assertion on
	// `store` (the SAME pattern field_orchestration.go's FieldReorderRepo auto-wire uses), so no
	// separate wiring call is needed in cmd/server/main.go — production's *dynrepo.Repository
	// satisfies it the moment BatchActiveUserExists exists (see the compile-time guard below,
	// near userExister). Nil only in hand-rolled test fakes that don't implement it, in which case
	// Create/Update FAIL CLOSED with RECORD_USER_UNAVAILABLE — but ONLY for an entity that
	// actually declares a `user` field with a non-empty value in the payload (mirrors sequences'
	// nil-disables-only-when-relevant contract); an entity with no `user` field is unaffected.
	userStore userExister
	// refRegistry backs the ENG-SAMPLE-REF "@ref" resolution seam of CreateBulk (record_bulk.go /
	// record_ref_registry.go) — lets a bulk-create row's relation field reference a record created
	// by an EARLIER, separate bulk-create call (e.g. the AI Builder's propose_records seeding a
	// parent entity's sample rows, then a child entity's). Always non-nil after NewRecordService
	// (like auditor/engine/approvals); a *RecordService built via a bare struct literal (common in
	// this package's unit tests) leaves it nil, which every registry method treats as "resolves
	// nothing" (fail-closed, never a panic) — existing tests never send a "$ref"/"@ref" payload, so
	// this is a no-op for them.
	refRegistry *recordRefRegistry
	// idem backs Idempotency-Key on POST /:entity (CreateIdempotent, specs/047). Nil makes
	// CreateIdempotent fail closed (500) instead of silently creating without the guard; a request
	// without the header never touches it. Production wires *idempotency.SQLRepository.
	idem IdempotencyStore
	// beforeRules loads the synchronous before_save/before_delete rules (specs/048, record_hooks.go).
	// Production wires the workflow CachedLoader; nil leaves every write unjudged.
	beforeRules workflow.WorkflowLoader
	// beforeRuns writes the run history those rules leave (blocked / skipped) off the write's path.
	// nil records nothing.
	beforeRuns *workflow.RunSink
	// layouts reads the edit layout's visibleWhen conditions for the create path (specs/054 FR-8):
	// a required field the form hides by condition is not demanded. Nil = every required field is
	// demanded. Production wires *metadata.LayoutRepository (WithLayoutReader).
	layouts layoutReader
	// purger backs Purge (specs/056 — the recycle bin's bottom). Auto-detected from `store` in
	// NewRecordService like userStore; nil (a test fake without the seam) makes Purge fail closed.
	purger recordPurger
	// blobs deletes a purged record's attachment bytes after commit (WithBlobRemover — the SAME
	// port the tenant purge uses). Optional: unwired ⇒ the report says the bytes remain.
	blobs blobRemover
	// relationships is the registry Purge reads the dependents graph from (specs/056 US2 —
	// WithRelationshipLister). Nil ⇒ Purge refuses: it cannot see what depends on the record.
	relationships relationshipLister
}

// SequenceClaimer atomically claims and advances the next value of a per-tenant, per-entity,
// per-field auto_number sequence (ENG #6). Satisfied by *dynrepo.Repository.ClaimSequence.
// Declared consumer-side (mirrors FieldValueFetcher / RollupLinkedLister) so the orchestration
// layer never imports the concrete dynrepo type and unit tests can inject a fake.
type SequenceClaimer interface {
	ClaimSequence(ctx context.Context, tenantID, entityName, fieldName string) (int64, error)
}

// fieldACLEnforcer is the field-level-security surface the record service consumes (RM-42).
// Satisfied by *FieldACLService. Declared consumer-side so the orchestration can strip/guard
// fields without importing a concrete type, and so unit tests can inject a fake (or leave it nil
// to disable). The verified isAdmin flag (from scope.IsAdmin) bypasses every method.
type fieldACLEnforcer interface {
	// FilterReadFields strips, in place, the fields the role may not view from data.
	FilterReadFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, data map[string]any) error
	// FilterReadFieldsBatch strips, in place, the not-viewable fields from every record's data,
	// loading the restriction set once (O(1) grant lookup per page).
	FilterReadFieldsBatch(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, datas []map[string]any) error
	// CheckWriteFields rejects (FIELD_FORBIDDEN) a payload touching a field the role may not edit.
	CheckWriteFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, payload map[string]any) error
	// CheckWriteFieldsAgainst is the update-path form: a locked field carrying its CURRENT value
	// is a no-op, not an edit, so it is allowed. current == nil restores the strict behaviour.
	CheckWriteFieldsAgainst(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, payload, current map[string]any) error
	// AssertFieldVisible rejects (FIELD_FORBIDDEN) an operation on a single field the role may not view.
	AssertFieldVisible(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, fieldName string) error
	// AssertFieldsVisible rejects (FIELD_FORBIDDEN) when the role may not view any of fieldNames
	// (used to block filtering/sorting on a hidden field — a probing oracle).
	AssertFieldsVisible(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, fieldNames []string) error
	// HiddenFields returns the set of field names the role may not view on the entity (used to
	// drop hidden columns from a multi-field ?q search rather than rejecting the whole search).
	HiddenFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (map[string]struct{}, error)
}

// entityAuthorizer resolves whether a role may perform an action on an entity (+ its row-level
// access_level). Satisfied by *ACLService. Declared here so the expand path can re-check the
// caller's ACL on a relation's target entity without importing a concrete type.
type entityAuthorizer interface {
	Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error)
}

// NewRecordService wires the record service.
func NewRecordService(catalog EntityCatalog, store RecordStore) *RecordService {
	s := &RecordService{catalog: catalog, store: store, auditor: audit.NewNoopAuditor(), engine: workflow.NewNoopEngine(), approvals: bpm.NewNoopEngine(), refRegistry: newRecordRefRegistry()}
	// USER-FIELD: auto-wire the user-existence probe when `store` also implements it (production
	// *dynrepo.Repository does) — mirrors field_orchestration.go's FieldReorderRepo auto-wire.
	if ue, ok := store.(userExister); ok {
		s.userStore = ue
	}
	// specs/056: same auto-wire for the purge seam (production *dynrepo.Repository has it).
	if p, ok := store.(recordPurger); ok {
		s.purger = p
	}
	return s
}

// WithACLAuthorizer injects the entity-ACL authorizer used to gate ?expand by the caller's
// access to each relation's target entity (W5-A2). Production wires *ACLService; nil leaves
// expand unchecked.
func (s *RecordService) WithACLAuthorizer(a entityAuthorizer) *RecordService {
	s.authz = a
	return s
}

// AuthorizeView resolves the caller's row-level access level for the given entity's "view" action
// and returns the scope to run a READ with. It is the entity-ACL stage the /:entity RequireACL
// middleware performs for a route whose entity is in the URL — done HERE for a read whose entity is
// NOT in the URL and therefore never seen by RequireACL: the report RUN / CSV-export path, whose
// entity is stored on the report record (REPORT-RUN-ACL). It reuses the SAME entityAuthorizer.Authorize
// decision the middleware and resolveReportJoin / the ⌘K assistant's RecordReadAdapter.scopeFor make —
// no second ACL body:
//   - admin → scope returned unchanged (IsAdmin already short-circuits every downstream row/field
//     check to AccessAll, exactly like the middleware's D3 admin bypass).
//   - authz wired + non-admin → Authorize(entity,"view"); DENIED → 403 FORBIDDEN returned BEFORE any
//     data query runs; ALLOWED → a COPY of the scope with AccessLevel set to the resolved grant level
//     (identity fields UserID/RoleID/IsAdmin stay VERBATIM). A5 row-scoping then applies from that
//     resolved level in List/Aggregate — this RESOLVES the level, it never bypasses tenant/row scope.
//   - authz NOT wired (nil) → scope returned unchanged (passthrough), mirroring the ?expand /
//     applyExpand nil-authorizer convention so a unit test / build without ACL wiring keeps prior
//     behaviour. Production always wires WithACLAuthorizer.
func (s *RecordService) AuthorizeView(ctx context.Context, tenantID string, scope RecordScope, entity string) (RecordScope, error) {
	return s.authorizeAction(ctx, tenantID, scope, entity, "view")
}

// AuthorizeRead is AuthorizeView for a surface that READS records on the caller's behalf the way
// the entity's own list would — report run / CSV export, dashboard widget — and so must also count
// the records manually shared with them (DAILY-QUEUE #225, specs/051 FR-004): otherwise a card says 0
// while the list it opens shows the record. It is AuthorizeView plus IncludeShares, set AFTER the
// grant is resolved: the flag only ever widens an owner/team scope on a READ, and dynrepo's write
// methods ignore it whatever it says. Callers whose result feeds a WRITE decision (notes, approvals)
// keep AuthorizeView, which leaves the flag exactly as the route resolved it.
func (s *RecordService) AuthorizeRead(ctx context.Context, tenantID string, scope RecordScope, entity string) (RecordScope, error) {
	scoped, err := s.authorizeAction(ctx, tenantID, scope, entity, "view")
	if err != nil {
		return RecordScope{}, err
	}
	scoped.IncludeShares = true
	return scoped, nil
}

// authorizeAction is AuthorizeView for any RBAC action (the manage-shares gate re-resolves "edit").
// For any action other than "view" the returned scope drops IncludeShares: a manual share is
// read-only (specs/051), so a scope resolved for a write must never carry it.
func (s *RecordService) authorizeAction(ctx context.Context, tenantID string, scope RecordScope, entity, action string) (RecordScope, error) {
	if action != "view" {
		scope.IncludeShares = false
	}
	if scope.IsAdmin || s.authz == nil {
		return scope, nil
	}
	allowed, level, err := s.authz.Authorize(ctx, tenantID, scope.RoleID, entity, action)
	if err != nil {
		// Fail closed: an ACL infrastructure fault must never widen visibility.
		return RecordScope{}, fmt.Errorf("services.RecordService.authorizeAction: %w", err)
	}
	if !allowed {
		verb := map[string]string{"view": "xem", "edit": "sửa"}[action]
		if verb == "" {
			verb = action
		}
		return RecordScope{}, invalid("FORBIDDEN", "không có quyền %s entity %q", verb, entity)
	}
	scoped := scope // identity fields stay VERBATIM — only the resolved level changes
	scoped.AccessLevel = level
	return scoped, nil
}

// WithLabelFetcher injects the expand label-fetcher (RM-07). The real implementation is
// *dynrepo.Repository; injecting it separately keeps the constructor unchanged and lets
// unit tests supply a fake. A nil fetcher leaves expand as a no-op.
func (s *RecordService) WithLabelFetcher(f LabelFetcher) *RecordService {
	s.fetcher = f
	return s
}

// WithFieldValueFetcher injects the read-time lookup-field fetcher (ENG-COMPUTED #2). The real
// implementation is *dynrepo.Repository (same concrete type as WithLabelFetcher); injecting it
// separately keeps the constructor unchanged and lets unit tests supply a fake. A nil fetcher
// leaves every `lookup` field resolving to null (no error).
func (s *RecordService) WithFieldValueFetcher(f FieldValueFetcher) *RecordService {
	s.lookupFetcher = f
	return s
}

// WithRollupMeta injects the cross-entity metadata resolver ?rollup= needs (ENG-COMPUTED #3):
// resolve the child entity + its fields, and resolve an N-N relationship by name. Production
// wires *metadata.Repository. A nil resolver disables ?rollup= (RECORD_ROLLUP_UNAVAILABLE).
func (s *RecordService) WithRollupMeta(m rollupMetaResolver) *RecordService {
	s.rollupMeta = m
	return s
}

// WithRollupLinked injects the N-N pivot id-set lister ?rollup= needs for a relationship-mode
// rollup field (ENG-COMPUTED #3). Production wires *dynrepo.Repository. A nil lister disables
// only the N-N rollup path (FK-on-child rollup is unaffected).
func (s *RecordService) WithRollupLinked(l RollupLinkedLister) *RecordService {
	s.rollupLinked = l
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so committed record mutations emit an
// append-only audit row (async, fail-open). Chainable; a nil auditor is ignored (keeps the
// noop default) so a mis-wired caller never panics on the audit path.
func (s *RecordService) WithAuditor(a audit.Auditor) *RecordService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithEngine injects the EPIC 4 workflow engine so committed record mutations fire automations
// (async, fail-open). Chainable; a nil engine is ignored (keeps the noop default) so a mis-wired
// caller never panics on the automation path.
func (s *RecordService) WithEngine(e workflow.Engine) *RecordService {
	if e != nil {
		s.engine = e
	}
	return s
}

// WithApprovalEngine injects the EPIC 5 BPM approval engine so committed record mutations arm
// approval instances (async, fail-open). Chainable; a nil engine is ignored (keeps the noop
// default) so a mis-wired caller never panics on the approval-arming path.
func (s *RecordService) WithApprovalEngine(e bpm.Engine) *RecordService {
	if e != nil {
		s.approvals = e
	}
	return s
}

// WithFieldACL injects the field-level-security enforcer (RM-42 / A6) so reads strip fields the
// caller's role may not view and writes reject fields the role may not edit (403 FIELD_FORBIDDEN).
// Chainable; a nil enforcer is ignored (leaves field-level security DISABLED → default-allow every
// field), so a mis-wired caller never panics on the strip/guard path. Production wires
// *FieldACLService.
func (s *RecordService) WithFieldACL(f fieldACLEnforcer) *RecordService {
	if f != nil {
		s.fieldACL = f
	}
	return s
}

// WithSequenceClaimer injects the ENG #6 auto_number atomic sequence claimer. Chainable; a nil
// claimer is ignored (keeps the nil default, which fails closed in assignSequences the moment an
// entity actually declares an auto_number field — RECORD_AUTONUMBER_UNAVAILABLE — rather than
// silently persisting a record missing its sequence value). Production wires *dynrepo.Repository.
func (s *RecordService) WithSequenceClaimer(c SequenceClaimer) *RecordService {
	if c != nil {
		s.sequences = c
	}
	return s
}

// WithBulkInserter injects the ENG-BATCH #1 atomic multi-row insert primitive CreateBulk's
// mode=atomic Phase 2 needs. Chainable; a nil inserter is ignored (keeps the nil default, which
// fails closed with BULK_ATOMIC_UNAVAILABLE the moment an atomic bulk-create actually reaches
// Phase 2 — mirrors WithSequenceClaimer's nil-disables contract). mode=partial is entirely
// unaffected (it never reads this field). Production wires *dynrepo.Repository.
func (s *RecordService) WithBulkInserter(b BulkInserter) *RecordService {
	if b != nil {
		s.bulkStore = b
	}
	return s
}

// resolve loads the entity and its field specs, returning ErrEntityNotFound for unknown or
// invalid entity names (an invalid name can never name a real table).
func (s *RecordService) resolve(ctx context.Context, tenantID, entityName string) (*metadata.EntityDefinition, []FieldSpec, error) {
	name, err := ddl.SanitizeIdentifier(entityName)
	if err != nil {
		return nil, nil, ErrEntityNotFound
	}
	e, err := s.catalog.GetEntityByName(ctx, tenantID, name)
	if err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, nil, ErrEntityNotFound
		}
		return nil, nil, fmt.Errorf("services.resolve: %w", err)
	}
	fields, err := s.catalog.ListFields(ctx, tenantID, e.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("services.resolve: %w", err)
	}
	specs, err := toFieldSpecs(fields)
	if err != nil {
		return nil, nil, err
	}
	return e, specs, nil
}

// uniqueFieldSpecs projects the entity's field specs to the pivot's unique-field list threaded into
// the record mutators (RM-28 §3.1). One entry per field declared unique (sys_fields.is_unique). The
// generated column of a unique field is always named AFTER the field (ddl.BuildAddUniqueColumn), so
// GenCol == FieldName; a 1-1 relation (its uniqueness rides options.cardinality, not is_unique, and
// its column is `<field>_id`) is correctly excluded because IsUnique is false for it.
func uniqueFieldSpecs(specs []FieldSpec) []dynrepo.UniqueFieldSpec {
	var out []dynrepo.UniqueFieldSpec
	for _, sp := range specs {
		if sp.IsUnique {
			out = append(out, dynrepo.UniqueFieldSpec{FieldName: sp.Name, GenCol: sp.Name})
		}
	}
	return out
}

// writeBlockedReader is the optional seam behind explainRequiredForCaller, satisfied by
// *FieldACLService. Asserted off the enforcer rather than added to fieldACLEnforcer so the test
// doubles that predate it keep compiling; a double without it keeps the original error.
type writeBlockedReader interface {
	WriteBlockedFields(ctx context.Context, tenantID, roleID, entity string, isAdmin bool) (hidden, readOnly map[string]struct{}, err error)
}

// Pins the seam: production wires *FieldACLService (cmd/server/main.go WithFieldACL); if it ever
// stops satisfying the optional assert, the explainer would go silent with every test green.
var _ writeBlockedReader = (*FieldACLService)(nil)

// requiredLockExplainer is explainRequiredForCaller bound to this service's field-ACL enforcer and
// the request's principal (Create, and CreateBulk once per batch).
func (s *RecordService) requiredLockExplainer(ctx context.Context, scope RecordScope, tenantID, entity string, specs []FieldSpec) func(error) error {
	return explainRequiredForCaller(ctx, s.fieldACL, s.logger, scope.RoleID, scope.IsAdmin, tenantID, entity, specs)
}

// explainRequiredForCaller returns a rewrite for create-path "required field missing" errors —
// RECORD_REQUIRED_MISSING from ValidateRecord (form, "Tạo nhiều dòng") and, on CSV import,
// IMPORT_REQUIRED_FIELD_NOT_MAPPED — when the field is one the caller's role may not write:
//
//   - VISIBLE but locked (queue #62; import since #144): not an omission the caller can fix. The
//     form renders the field as text, the grid has no column for it, and an import mapping it is
//     refused FIELD_FORBIDDEN, because the create guard refuses any locked key. It becomes
//     RECORD_REQUIRED_LOCKED (422, same field) naming the admin's real way out.
//   - HIDDEN (queue #143): the original error named a field the caller may not know exists
//     (details.field + message), contradicting the anti-oracle rule GET /metadata/fields keeps
//     (api-conventions §4.4b). It becomes RECORD_REQUIRED_LOCKED with NO field and NO name or type:
//     the create cannot succeed, so the caller must learn that much. The message sends the admin
//     to the screen a customer admin manages, /admin/field-acl ("Phân quyền Field"), where a hidden
//     required cell carries a warning — not to Doctor C9, which names the field too but sits behind
//     RequirePlatformOperator, so only the deployment team can open it.
//
// It also scrubs RECORD_DEPENDENT_PICKLIST_INVALID (queue #321, Create/CreateBulk/Update): when the
// child picklist or its controlling parent is hidden from the caller, the stored default of the child
// is judged on every create and the error would name the hidden field and its label — it becomes the
// same code with no details.field and no label (hiddenDependentPicklistRefusal).
//
// The PutGrants and EditField guards refuse to create this state; rows saved before them still
// reach it. Nothing is widened: the create is refused either way, only the reason changes. Admins
// bypass locks, so theirs is a real omission. The grant set loads at most once per returned func
// (a bulk batch shares one). A load failure never claims a lock it could not verify, and never
// lets the name out either (the field might be hidden): the original code, without the field.
// acl may be nil or a double without WriteBlockedFields (no rewrite — no enforcement to explain).
func explainRequiredForCaller(ctx context.Context, acl fieldACLEnforcer, logger *zap.Logger, roleID string, isAdmin bool, tenantID, entity string, specs []FieldSpec) func(error) error {
	var loaded bool
	var loadFailed bool
	var hidden, readOnly map[string]struct{}
	return func(err error) error {
		var ve *ValidationError
		if isAdmin || !errors.As(err, &ve) || ve.Field == "" ||
			(ve.Code != "RECORD_REQUIRED_MISSING" && ve.Code != "IMPORT_REQUIRED_FIELD_NOT_MAPPED" &&
				ve.Code != codeRecordDependentInvalid) {
			return err
		}
		reader, ok := acl.(writeBlockedReader)
		if !ok {
			return err
		}
		if !loaded {
			loaded = true
			h, ro, lerr := reader.WriteBlockedFields(ctx, tenantID, roleID, entity, isAdmin)
			if lerr != nil {
				loadFailed = true
				if logger != nil {
					logger.Warn("services.explainRequiredForCaller: field grant load failed; answering without the field name",
						zap.String("tenant_id", tenantID), zap.String("entity_name", entity), zap.Error(lerr))
				}
			}
			hidden, readOnly = h, ro
		}
		if ve.Code == codeRecordDependentInvalid {
			// queue #321: not a required-field case — only the NAMES matter. A load failure cannot
			// prove the child/parent visible, so it answers unnamed too (fail closed).
			if loadFailed || dependentPicklistNamesHidden(specs, hidden, ve.Field) {
				return hiddenDependentPicklistRefusal()
			}
			return err
		}
		if loadFailed {
			return invalid(ve.Code, "Thiếu một trường bắt buộc, nhưng chưa kiểm được quyền xem trường của vai trò bạn lúc này — thử lại sau")
		}
		root := strings.SplitN(ve.Field, ".", 2)[0] // a composite sub-path is locked via its root
		if _, isHidden := hidden[root]; isHidden {
			return invalid("RECORD_REQUIRED_LOCKED",
				"Bản ghi cần một trường bắt buộc mà vai trò của bạn không được xem, nên chưa tạo được bản ghi nào. Báo quản trị viên: ở màn Phân quyền Field của đối tượng này, ô Ẩn mang dấu cảnh báo trong cột vai trò của bạn chính là trường đó — đặt ô về Sửa, hoặc nhờ đội triển khai bỏ \"bắt buộc\" hay đặt giá trị mặc định cho trường")
		}
		if _, locked := readOnly[root]; !locked {
			return err
		}
		label, fieldType := root, ""
		for _, sp := range specs {
			if sp.Name == root {
				fieldType = sp.FieldType
				if sp.Label != "" {
					label = sp.Label
				}
				break
			}
		}
		return invalidField("RECORD_REQUIRED_LOCKED", root,
			"Trường %q bắt buộc nhưng vai trò của bạn không được ghi trường này, nên chưa tạo được bản ghi nào. Báo quản trị viên — cách gỡ: %s",
			label, lockedRequiredRemedy(fieldType))
	}
}

// Create validates and inserts a record, returning its id. scope carries the verified principal
// (RoleID/IsAdmin) so the field-level write guard (RM-42) can reject a payload that touches a
// field the role may not edit (403 FIELD_FORBIDDEN); an admin bypasses. The module ACL (A4) has
// already authorized the create action on the entity upstream.
func (s *RecordService) Create(ctx context.Context, scope RecordScope, tenantID, userID, entityName string, payload map[string]any) (string, error) {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return "", err
	}
	// RM-42 write guard: reject BEFORE validation/insert if the payload names a field the role
	// may not edit. Applied on the raw payload keys (the field names the client actually sent),
	// so a locked field is refused even when its value would otherwise be valid. Admin bypasses.
	if s.fieldACL != nil {
		if err := s.fieldACL.CheckWriteFields(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin, payload); err != nil {
			// A hidden key reads exactly like an undeclared one (queue #317): finish through ValidateRecord.
			return "", finishHiddenKeyRefusal(err, func(h map[string]struct{}) error {
				_, verr := validateRecord(specs, payload, h)
				return verr
			})
		}
	}
	clean, err := ValidateRecord(specs, payload)
	if err != nil {
		// specs/054: a required field the edit form hides by condition is not demanded.
		clean, err = s.hiddenRequiredRejudge(ctx, tenantID, e.Name, specs)(payload, err)
	}
	if err != nil {
		return "", s.requiredLockExplainer(ctx, scope, tenantID, e.Name, specs)(err)
	}
	if err := s.validateRelations(ctx, scope, tenantID, specs, clean); err != nil {
		return "", err
	}
	// ACTIVITY-CORE #3a: tenant-scoped existence probe for every polymorphic_relation value —
	// a {type,id} pointing at a non-existent / cross-tenant record is rejected (mirrors relation).
	if err := s.validatePolymorphicRelations(ctx, scope, tenantID, specs, clean, nil); err != nil {
		return "", err
	}
	// USER-FIELD: tenant-scoped existence probe for every `user` value — must be an ACTIVE user
	// in the same tenant (mirrors relation/polymorphic).
	if err := s.validateUserFields(ctx, tenantID, specs, clean); err != nil {
		return "", err
	}
	// specs/048 S2/S4: the before_save rules fill values into clean and judge the new record AFTER
	// every field check above (their error order is unchanged) and BEFORE the auto_number claim, so a
	// refused create writes nothing and burns no number (FR-020).
	commitHooks, err := s.checkBeforeCreate(ctx, scope, tenantID, e.Name, specs, clean)
	if err != nil {
		return "", err
	}
	// ENG #6: claim + format every auto_number field's value. CREATE-ONLY seam (deliberately NOT
	// part of applyComputedFields, which also runs on Update — that would re-consume a fresh
	// sequence number on every PUT). Runs after relation validation (mirrors formula, which needs
	// FK values already confirmed live) and before InsertRecord so the claimed, formatted value
	// is part of the same payload that gets persisted.
	if err := s.assignSequences(ctx, tenantID, e.Name, specs, clean); err != nil {
		return "", err
	}
	// Compute formula fields from the validated data before persisting. This runs after
	// relation validation so FK values are already confirmed live.
	applyComputedFields(specs, clean)
	seedStoredCounts(specs, clean) // specs/083: a new parent starts at 0
	id, err := s.store.InsertRecord(ctx, e.Name, tenantID, userID, clean, uniqueFieldSpecs(specs))
	if err != nil {
		return id, asDataConstraint(asDuplicate(err, e.Name, specs))
	}
	commitHooks(id)
	// specs/083: parent totals that count this row are refreshed before the automations fire.
	s.refreshStoredRollups(ctx, tenantID, e.Name, clean)
	s.emitCreateEvents(ctx, tenantID, userID, e.Name, id, clean, specs)
	return id, nil
}

// emitCreateEvents fires the three async, fail-open side effects every committed record CREATE
// triggers — audit (RM-19), workflow automations (EPIC 4 after_create), BPM approval arming
// (EPIC 5 after_create). Extracted out of Create() (mechanical refactor, same calls/order/args)
// so CreateBulk's atomic Phase 2 (ENG-BATCH #1, record_bulk.go) can reuse the EXACT same
// notification contract for a batch-inserted row instead of re-implementing it — the two paths
// can never drift on what "a record was created" means downstream (B6). clean is the FULL
// persisted data (post auto_number/formula) for entityName's record id.
func (s *RecordService) emitCreateEvents(ctx context.Context, tenantID, userID, entityName, id string, clean map[string]any, specs []FieldSpec) {
	// RM-19: audit the committed create (after-only diff). Async + fail-open — a write
	// problem never affects the create result.
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: userID,
		EntityName:  entityName,
		RecordID:    id,
		Action:      audit.ActionCreate,
		Diff:        redactSensitiveDiff(specs, audit.ComputeDiff(nil, clean)),
		At:          time.Now().UTC(),
	})
	// specs/064 S9-4: a sample row (dynrepo.WithSampleMark on ctx) is audited above but runs NO
	// automation — no workflow rule (email_alert, webhook, create_record…) and no approval is
	// armed for demo data. before_save/validation already ran before the insert and still apply.
	if _, sample := dynrepo.SampleMarkFrom(ctx); sample {
		return
	}
	// EPIC 4: fire after_create workflows on the committed record. The event carries the full
	// cleaned data (every persisted field) so a rule's condition can reference any field. Async
	// + fail-open — an automation problem never affects the create result. OwnerUserID = userID:
	// the creator IS the record's created_by (no extra read needed) — the "$owner" notification
	// sentinel resolution source (M7 MED-1 fix, runInAppNotification).
	// Depth: CREATE-RECORD recursion guard. A user-driven create leaves ctx without a depth value
	// → 0. A create_record action's nested Create carries depth+1 via ctx (workflow.WithChainDepth),
	// which we stamp here so the created record's after_create event advances the guard's counter —
	// the ONLY place depth crosses into the async event (the worker discards ctx at the channel).
	s.engine.Emit(ctx, workflow.Event{
		TenantID:    tenantID,
		ActorUserID: userID,
		EntityName:  entityName,
		RecordID:    id,
		Trigger:     workflow.TriggerAfterCreate,
		Data:        cloneData(clean),
		OwnerUserID: userID,
		Depth:       workflow.ChainDepthFromContext(ctx),
		// The row was inserted a moment ago and created_at is the DB's CURRENT_TIMESTAMP, so "now"
		// is it to within the same second — no read-back for a value set_elapsed would measure as ~0.
		CreatedAt: time.Now().UTC(),
	})
	// EPIC 5: arm BPM approval processes on the committed record (after_create). Async +
	// fail-open exactly like the workflow engine — an approval-arming problem never affects the
	// create result. The engine matches the criteria against the same cleaned data and, on a
	// match, opens an approval instance on the first step.
	s.approvals.Emit(ctx, bpm.Event{
		TenantID:    tenantID,
		ActorUserID: userID,
		EntityName:  entityName,
		RecordID:    id,
		Trigger:     bpm.TriggerAfterCreate,
		Data:        cloneData(clean),
		// Same counter as the workflow event above: the approval engine refuses to arm past the
		// shared chain cap (specs/043 FR-042).
		Depth: workflow.ChainDepthFromContext(ctx),
	})
}

// assignSequences claims and formats the value of every auto_number field in specs, writing the
// formatted string directly into clean (ENG #6). It is called ONLY from Create — never from
// Update, and never through applyComputedFields — because a sequence claim is a side-effecting,
// non-idempotent operation: calling it on every PUT (the way applyComputedFields recomputes a
// formula on every write) would burn a fresh number on every edit, not just at creation.
//
// entityName is the entity being written (kept as a separate parameter, not re-derived from
// specs, because it is not itself a field). specs with no auto_number field make this a no-op —
// s.sequences is never even nil-checked in that case, so entities without auto_number fields are
// entirely unaffected by whether a SequenceClaimer was wired.
//
// DECISION (gaps-allowed, ../specs/006-auto-number/spec.md §"#6"): the claim happens OUTSIDE the
// store.InsertRecord transaction (Create calls this before InsertRecord, in two separate calls to
// two separate systems — sys_sequences vs obj_*). If InsertRecord subsequently fails (e.g. a
// different field's unique constraint collides), the number already claimed here is burned: no
// record ever holds it, and the next successful create simply skips over it. This mirrors
// Salesforce's own Auto Number field behaviour and is a documented trade-off, not a bug — see the
// migration 000029 header comment and api-conventions.md §7.
func (s *RecordService) assignSequences(ctx context.Context, tenantID, entityName string, specs []FieldSpec, clean map[string]any) error {
	return assignSequenceValues(ctx, s.sequences, tenantID, entityName, specs, clean)
}

// assignSequenceValues claims + assigns auto_number values for every auto_number field in specs,
// writing formatAutoNumber(...) into clean. Shared by the Create/bulk write-path (RecordService,
// via assignSequences) and the CSV import path (ImportService, IMPORT-AUTONUM) so both go through
// ONE atomic sequence claim primitive (ENG #6 / dynrepo.ClaimSequence) — the two paths can never
// drift on how an auto_number value is claimed or formatted (B6). A nil claimer fails closed with
// RECORD_AUTONUMBER_UNAVAILABLE the moment an auto_number field is encountered (same contract as
// before): an entity with NO auto_number field never even nil-checks the claimer, so such entities
// are unaffected by a nil. entityName is the entity being written (a separate parameter, not
// re-derived from specs, because it is not itself a field). This is a side-effecting, NON-idempotent
// operation (each call burns a fresh number), so callers must invoke it only on a create/insert
// path, never on Update — see assignSequences' doc comment for the gaps-allowed rationale.
func assignSequenceValues(ctx context.Context, sequences SequenceClaimer, tenantID, entityName string, specs []FieldSpec, clean map[string]any) error {
	for _, f := range specs {
		if f.FieldType != "auto_number" {
			continue
		}
		if sequences == nil {
			return invalid("RECORD_AUTONUMBER_UNAVAILABLE",
				"field auto_number %q chưa được cấu hình trên server (thiếu sequence claimer)", f.Name)
		}
		n, err := sequences.ClaimSequence(ctx, tenantID, entityName, f.Name)
		if err != nil {
			return fmt.Errorf("services.assignSequenceValues: claim %q.%q: %w", entityName, f.Name, err)
		}
		clean[f.Name] = formatAutoNumber(f.Options, n)
	}
	return nil
}

// formatAutoNumber renders a claimed sequence value n as the field's persisted string, applying
// the optional options.prefix (prepended verbatim) and options.padding (zero-pads the decimal
// digits of n to at least that many characters — never truncates: a value with MORE digits than
// the configured padding is rendered in full, e.g. padding=3 with n=123456 yields "123456", not a
// truncated "456"). No options at all renders the bare decimal (e.g. "42"). Both options were
// already validated at config time (validateAutoNumberConfig): prefix ≤ 20 chars, padding 0..12.
func formatAutoNumber(options map[string]any, n int64) string {
	var prefix string
	if p, ok := options["prefix"].(string); ok {
		prefix = p
	}
	padding := 0
	if p, ok := options["padding"].(float64); ok {
		padding = int(p)
	}
	digits := strconv.FormatInt(n, 10)
	if padding > 0 && len(digits) < padding {
		digits = strings.Repeat("0", padding-len(digits)) + digits
	}
	return prefix + digits
}

// Get returns a single record, or ErrEntityNotFound / ErrRecordNotFound. scope carries the
// caller's verified user id + resolved A5 access_level; a record the scope may not see surfaces
// as ErrRecordNotFound (no cross-team/cross-tenant leak). expand is the list of relation field
// names to eagerly resolve to {id,label} (depth=1, RM-07); an empty/nil slice is a no-op.
// rollup is the ENG-COMPUTED #3 explicit ?rollup=f1,f2 request — an empty/nil slice resolves no
// rollup field (the default; avoids an N+1 aggregate on every plain GET).
func (s *RecordService) Get(ctx context.Context, scope RecordScope, tenantID, entityName, id string, expand []string, rollup []string) (*dynrepo.Record, error) {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return nil, err
	}
	rec, err := s.store.GetRecord(ctx, e.Name, tenantID, id, scope.toRowScope())
	if err != nil {
		return nil, err
	}
	if err := s.markEditability(ctx, scope, tenantID, e.Name, rec); err != nil {
		return nil, err
	}
	// RM-42 read strip: remove fields the role may not view from the record's data BEFORE expand
	// (so a hidden relation field is not even resolved to a label). Admin bypasses. Fail-closed
	// inside the enforcer (a load error hides all business fields). The system columns
	// id/version/createdAt/updatedAt live on the Record struct, not in Data, so they are unaffected.
	if s.fieldACL != nil {
		if err := s.fieldACL.FilterReadFields(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin, rec.Data); err != nil {
			return nil, fmt.Errorf("services.Get: field-acl: %w", err)
		}
	}
	// COMPUTED writes (lookup/rollup) run AFTER the strip above, so they need the caller's
	// hidden-field set to not re-add a can_view=0 field the strip just removed (RM-42/A6).
	hidden, err := s.computedHiddenFields(ctx, scope, tenantID, e.Name)
	if err != nil {
		return nil, fmt.Errorf("services.Get: field-acl: %w", err)
	}
	// ENG-COMPUTED #2: lookup fields are ALWAYS resolved (read-time, always-fresh — not gated
	// by ?expand). Must run BEFORE applyExpand: applyExpand rewrites a `via` relation field's
	// raw FK id string into an {id,label} struct, which would break lookup's read of it.
	recs := []dynrepo.Record{*rec}
	denied := map[string]struct{}{} // read-time operands the caller may not see (formula_derived.go)
	if err := applyLookups(ctx, s.lookupFetcher, s.authz, s.fieldACL, scope, tenantID, recs, specs, hidden, denied); err != nil {
		return nil, fmt.Errorf("services.Get: lookup: %w", err)
	}
	if s.fetcher != nil && len(expand) > 0 {
		if err := s.expandRecords(ctx, scope, tenantID, recs, specs, expand); err != nil {
			return nil, fmt.Errorf("services.Get: expand: %w", err)
		}
	}
	// ENG-COMPUTED #3: rollup fields are resolved ONLY when explicitly requested via ?rollup=.
	if err := s.applyRollups(ctx, scope, tenantID, recs, specs, withDerivedRollups(rollup, specs, hidden), hidden, denied); err != nil {
		return nil, fmt.Errorf("services.Get: rollup: %w", err)
	}
	applyDerivedFormulas(specs, recs, hidden, denied) // queue #284: formulas over lookup/rollup, computed at read
	*rec = recs[0]
	return rec, nil
}

// markEditability sets rec.CanEdit=&false when the caller's EDIT scope does not reach a record their
// VIEW scope does (DAILY-QUEUE #334: view=all / edit=own, a rep opening a colleague's lead), so the
// page can hide Sửa instead of letting the save die as 404 RECORD_NOT_FOUND. It leaves CanEdit nil —
// "no restriction known" — for admins, an unwired authorizer, an edit scope that is "all", and a
// record the edit scope reaches. The edit probe is the same GetRecord the PUT would run (edit level,
// shares OFF), so the flag cannot disagree with what the write does. A probe fault is returned
// wrapped rather than guessed at.
func (s *RecordService) markEditability(ctx context.Context, scope RecordScope, tenantID, entity string, rec *dynrepo.Record) error {
	if scope.IsAdmin || s.authz == nil {
		return nil
	}
	allowed, level, err := s.authz.Authorize(ctx, tenantID, scope.RoleID, entity, "edit")
	if err != nil {
		return fmt.Errorf("services.RecordService.markEditability: %w", err)
	}
	no := false
	switch {
	case !allowed || dynrepo.AccessLevel(level) == dynrepo.AccessNone:
		rec.CanEdit = &no
		return nil
	case dynrepo.AccessLevel(level) == dynrepo.AccessAll:
		return nil
	case rec.SharedWithMe: // reached only through a share, and shares are read-only (specs/051)
		rec.CanEdit = &no
		return nil
	case level == scope.AccessLevel: // the row passed this very predicate on the view read
		return nil
	}
	editScope := scope
	editScope.AccessLevel, editScope.IncludeShares = level, false
	if _, err := s.store.GetRecord(ctx, entity, tenantID, rec.ID, editScope.toRowScope()); err != nil {
		if errors.Is(err, ErrRecordNotFound) {
			rec.CanEdit = &no
			return nil
		}
		return fmt.Errorf("services.RecordService.markEditability: %w", err)
	}
	return nil
}

// FilterInput is a single parsed filter from the query string (?field[op]=value).
// The handler parses raw query keys into these; the service validates them.
type FilterInput struct {
	Field    string
	Operator dynrepo.FilterOperator // empty string == eq (backward compat)
	Values   []string               // one or more raw string values
}

// ListQuery is the resolved query input for a record listing: pagination/ordering plus the
// optional ?field[op]=value filters and ?q substring search. The handler builds it from the
// raw query string; the service validates Filters/Search against the entity's declared fields
// before handing sanitized params down to the store.
// Expand is the set of relation field names to eagerly resolve (depth=1) to {id,label}.
//
// Cursor mode (D2): when Paginate=="cursor" or After is non-empty the service routes to
// ListRecordsCursor (O(1) per page, no COUNT). Sort must be a physical indexed column in cursor
// mode — otherwise 422. Offset mode uses ListRecords with a total count; page numbers beyond
// maxOffsetPage return 422 PAGE_TOO_DEEP. resolvePaginationMode owns the mode decision.
type ListQuery struct {
	Page  int
	Limit int
	Sort  string
	Order string
	// After is the opaque base64 cursor returned as pagination.nextCursor by a prior CURSOR-mode
	// response; non-empty activates cursor (keyset) mode (D2). Cursor mode omits COUNT(*) and
	// returns {limit, nextCursor} in the envelope instead of {page, total}.
	After string
	// Paginate is the raw ?paginate= mode selector (CURSOR-BOOTSTRAP #42): "" (legacy: cursor iff
	// After is set), "offset", or "cursor" — keyset from the FIRST page (After==""), which is how a
	// client obtains its first nextCursor. Any other value is 422 INVALID_PAGINATION_MODE.
	Paginate string
	// PageExplicit reports that the caller sent ?page= at all. Page is defaulted to 1 by the
	// handler, so this is the only way the service can refuse ?page= in cursor mode (422
	// PAGINATION_MODE_CONFLICT) instead of silently ignoring it.
	PageExplicit bool
	// Filters is the parsed filter list. Each entry specifies a field, an operator, and values.
	// The service validates field existence, operator legality per field type, and value arity.
	Filters []FilterInput
	Search  string
	Expand  []string // relation field names to expand (RM-07); depth=1 only
	// Rollup is the ENG-COMPUTED #3 explicit ?rollup=f1,f2 request. Empty/nil (the default)
	// resolves NO rollup field — a rollup is a per-record aggregate query, so eagerly resolving
	// every declared rollup field on every plain List would be an implicit N+1; the caller must
	// opt in per request, exactly like Expand opts into label resolution.
	Rollup []string
	// Overlay is the W4 request-time created_at/created_by overlay (dashboard/report runs and
	// report CSV export). nil (the default) leaves List behavior byte-identical to pre-W4.
	Overlay *RunOverlay
	// OnlyDeleted flips the listing to the recycle bin (W9b): only soft-deleted rows
	// (deleted_at IS NOT NULL). It reuses the ENTIRE live-list pipeline — tenant filter, A5
	// row-scope, field-ACL strip, filters, search, pagination — so a user only ever sees deleted
	// rows they could have seen live, with hidden fields stripped identically. Recycle-bin listing
	// is OFFSET-only (resolvePaginationMode refuses cursor mode with it), so this flag is threaded
	// only into the offset ListParams below, never the cursor path.
	OnlyDeleted bool
}

// searchableTypes are the field types whose values are strings and therefore meaningfully
// matched by the ?q substring search. number/boolean are excluded (LIKE on them is noise).
var searchableTypes = map[string]struct{}{
	"text": {}, "email": {}, "picklist": {}, "date": {}, "datetime": {}, "relation": {},
}

// caseInsensitiveTypes are the field types where eq/neq/in/contains comparisons should be
// case-insensitive (RM-14). Relation fields store UUIDs — exact match required. Unique
// generated columns (codes/SKUs) use utf8mb4_bin — exactness matters.
var caseInsensitiveTypes = map[string]struct{}{
	"text": {}, "email": {}, "picklist": {}, "rich_text": {},
}

// numericCastTypes are the field types where ordered comparisons (gt/gte/lt/lte/between)
// should CAST the JSON path expression to DECIMAL(38,10) to avoid lexicographic comparison.
// currency is included: after B1 fix, currency filters target the ".amount" sub-path which
// is a numeric scalar stored as a JSON number — CAST to DECIMAL gives correct arithmetic.
// date/datetime use CastDatetime instead (see datetimeCastTypes).
var numericCastTypes = map[string]struct{}{
	"number": {}, "currency": {}, "rating": {},
}

// datetimeCastTypes are the field types where ordered comparisons should CAST via
// CAST(... AS DATETIME) so that ordering is on the instant rather than raw strings.
// Without this, mixed timezone offsets ("...+07:00" vs "...Z") sort incorrectly (S3).
var datetimeCastTypes = map[string]struct{}{
	"date": {}, "datetime": {},
}

// orderedOperators are the operators that require ordered comparison (need numeric cast for
// number types, and are only valid for ordered field types).
var orderedOperators = map[dynrepo.FilterOperator]struct{}{
	dynrepo.FilterGt:      {},
	dynrepo.FilterGte:     {},
	dynrepo.FilterLt:      {},
	dynrepo.FilterLte:     {},
	dynrepo.FilterBetween: {},
}

// orderedTypes are the field types that may be compared with ordered operators.
var orderedTypes = map[string]struct{}{
	"number": {}, "currency": {}, "rating": {},
	"date": {}, "datetime": {},
}

// unfilterableTypes are field types that cannot be filtered by any value operator because
// their stored form is a JSON object/array with no meaningful scalar equality. Attempting
// to filter on these returns 422 FILTER_OPERATOR_NOT_ALLOWED immediately (S2: fail-fast
// rather than silently returning 0 rows). Null operators (isnull/notnull) are still allowed.
var unfilterableTypes = map[string]struct{}{
	"composite": {},
	// ACTIVITY-CORE #3a: a polymorphic_relation stores a {type,id} JSON object — no scalar
	// equality on the BARE field. Its two physical sub-columns (<f>_type, <f>_id) ARE filterable
	// (routed via polymorphicSubField below); the bare field itself is not.
	"polymorphic_relation": {},
}

// multiSelectNoEqTypes are the multi_select field types that reject eq/neq/gt/lt because
// the value is a JSON array — there is no scalar equality. contains is allowed (substring
// match within the serialized array), and isnull/notnull are allowed. in is rejected too
// (no "array contains element" semantics in this wave). S2.
var multiSelectNoEqTypes = map[string]struct{}{
	"multi_select": {},
}

// textLikeTypes are the field types that may use the contains operator.
var textLikeTypes = map[string]struct{}{
	"text": {}, "email": {}, "rich_text": {},
}

// multiValueTypes are the field types that may use the in operator.
// USER-FIELD: `user` joins `relation` here — filtering "assigned_to in [userA,userB]" (e.g.
// "records assigned to my team") is the same shape as relation's "account in [...]".
var multiValueTypes = map[string]struct{}{
	"text": {}, "email": {}, "picklist": {}, "multi_select": {}, "relation": {}, "user": {},
}

// ownerPhysicalColumn is the physical owner column EVERY obj_* table carries: migration
// 000061 adds it with its own `idx_owner_id` index (and ddl.MandatoryColumns/MandatoryIndexes
// give every table created since then the same pair at birth), so admitting it as a sort key
// costs no new index and is not a fresh performance liability. Both the reserved sort key and
// the reserved filter key below resolve to it.
//
// WHY THE BARE COLUMN AND NOT COALESCE(owner_id, created_by) — the expression the read path
// folds (dynrepo.recordRow.toRecord) and the A5 row filter enforces (dynrepo.RowScope.rowFilter):
//
//  1. It cannot disagree in the DANGEROUS direction. RowScope.rowFilter is appended to every
//     obj_* read independently of sort/filter, so a predicate here can only ever NARROW the rows
//     the caller may already see — no owner sort/filter can surface a row the COALESCE-based row
//     filter hides, whichever of the two expressions this one uses.
//  2. dynrepo has no expression path: Filter.PhysicalColumn and ListParams.Sort are both run
//     through ddl.SanitizeIdentifier and emitted as one backtick-quoted identifier. COALESCE
//     would mean inventing an expression-column concept in the repository layer.
//  3. The two expressions differ ONLY for rows with owner_id IS NULL, and there is no steady
//     state that produces one: migration 000061 backfills owner_id = created_by, every INSERT
//     path writes it, and the Doctor's A5 schema sweep repairs a table that missed it.
//
// ponytail: bare column, not the COALESCE. Ceiling — a row that somehow keeps owner_id NULL (a
// Doctor generic ADD COLUMN that never backfilled) would not match ?ownerId[eq]=<its creator>
// and would sort as NULL even though the API reports that creator as its ownerId; ?ownerId[neq]=
// is the sharper edge of the same window, since SQL `owner_id != ?` also drops NULL rows that
// SHOULD match (their effective owner is somebody else). Fix by giving dynrepo.Filter/ListParams
// an expression column, never by loosening the row filter to match.
const ownerPhysicalColumn = "owner_id"

// ownerReservedKeys are the wire spellings a client may use for the owner READ dimension, in
// ?sort= and in ?<key>[op]=. Both resolve to ownerPhysicalColumn.
//
// "ownerId" is canonical: it is the ONLY spelling a client ever SEES (dynrepo.Record serializes
// the owner as json:"ownerId" on every row) and the spelling ParseRunOverlay already accepts for
// the dashboard/report overlay. "owner_id" rides along because every OTHER system key in this
// file is snake_case (created_at/updated_at/id), so a client copying ?sort=created_at guesses
// that one — the two spellings already disagree on the wire, and refusing either buys nothing
// but a 422 for a request whose intent is unambiguous.
//
// A DECLARED field always wins over these reserved keys: buildSortAllowlist seeds them before
// the spec loop (so a field named "ownerId" overwrites the seed), and the filter path resolves
// byName — and then polymorphic sub-columns — before falling back here.
var ownerReservedKeys = map[string]struct{}{"ownerId": {}, "owner_id": {}}

// ownerFilterSpec is the FieldSpec a reserved owner filter key resolves through. FieldType
// "user" is not decoration — it hands the owner dimension EXACTLY the semantics a `user` field
// already has here (eq/neq/in/isnull/notnull; no contains, no ordered comparisons; an exact,
// never case-folded UUID match), and Name "owner" makes the existing relation/user rewrite in
// List resolve the physical column to "owner"+"_id" — the real owner_id column — so the filter
// loop needs no owner-specific branch and cannot drift from how `user` fields behave.
func ownerFilterSpec() FieldSpec { return FieldSpec{Name: "owner", FieldType: "user"} }

// buildSortAllowlist constructs the physical column allowlist for ORDER BY for this entity.
// System columns (created_at, updated_at, id, owner_id) are always allowed. In addition:
//   - unique fields: generated column named after the field (e.g. "code" → physical col "code")
//   - relation/user fields: generated column <field>_id (e.g. "account" → physical col
//     "account_id"; USER-FIELD reuses relation's shape — a `user` field's physical column is
//     ALSO <field>_id)
//   - indexed fields (D1): generated column named after the field (e.g. "status" → "status")
//
// The returned map is fieldName → physicalColumnName. It is passed to dynrepo.ListParams so
// the repo's normalize() enforces it instead of the hard-coded 3-col allowlist.
// Non-unique, non-relation, non-user, non-indexed business fields (JSON with no generated
// column) are NOT in the allowlist — requesting them returns SORT_NOT_ALLOWED (RM-09, no
// silent fallback). This SAME allowlist backs groupBy for Aggregate/reports (resolveGroupByIndexed
// in record_aggregate.go), so a `user` field is groupable (kanban-by-assignee) for free.
func buildSortAllowlist(specs []FieldSpec) map[string]string {
	m := map[string]string{
		"created_at": "created_at",
		"updated_at": "updated_at",
		"id":         "id",
	}
	// OWNER-DIMENSION: the owner is a system column like created_at, not a sys_field, so it was
	// in no branch below and ?sort=ownerId returned 422 SORT_NOT_ALLOWED — "ai phụ trách" was
	// readable on every row yet unorderable. Seeded (not appended) so a declared field of the
	// same name still overwrites it in the loop. This also makes owner a legal groupBy for free
	// (resolveGroupByIndexed reuses this map) — kanban/report "theo người phụ trách".
	for k := range ownerReservedKeys {
		m[k] = ownerPhysicalColumn
	}
	for _, f := range specs {
		if f.IsUnique {
			// Physical generated column is named after the field (same name).
			m[f.Name] = f.Name
		}
		if f.FieldType == "relation" || f.FieldType == "user" {
			// Physical generated column is <field>_id.
			m[f.Name] = f.Name + "_id"
		}
		// D1: indexed field has a non-unique generated column with the bare field name.
		// Skip when IsUnique is also true — the unique column already covers it and its name
		// is already in the map from the IsUnique branch above.
		if f.IsIndexed && !f.IsUnique && f.FieldType != "relation" && f.FieldType != "user" {
			m[f.Name] = f.Name
		}
		// ACTIVITY-CORE #3a: a polymorphic_relation field exposes its two physical gen-columns
		// <f>_type / <f>_id (composite-indexed) as sortable — for a stable timeline sort/paginate.
		// The bare field name is NOT sortable (it is a JSON object with no scalar projection).
		if f.FieldType == "polymorphic_relation" {
			m[f.Name+"_type"] = f.Name + "_type"
			m[f.Name+"_id"] = f.Name + "_id"
		}
	}
	return m
}

// polymorphicSubField reports whether fieldKey names the physical `<base>_type` or `<base>_id`
// generated sub-column of a real polymorphic_relation field in byName, returning the base field
// name (ACTIVITY-CORE #3a). Callers look byName[fieldKey] up FIRST, so a real declared field
// literally named "<x>_type" always wins — this only matches keys that are NOT themselves
// declared fields. Used by the List/aggregate filter resolvers (to accept the sub-column as a
// filter key routed to the physical column) and by the field-ACL guard (to map the sub-column
// back to its base field so field-ACL on the polymorphic field applies as normal).
func polymorphicSubField(byName map[string]FieldSpec, fieldKey string) (base string, ok bool) {
	for _, suffix := range []string{"_type", "_id"} {
		if b, cut := strings.CutSuffix(fieldKey, suffix); cut {
			if f, exists := byName[b]; exists && f.FieldType == "polymorphic_relation" {
				return b, true
			}
		}
	}
	return "", false
}

// polymorphicSubSpec synthesizes the FieldSpec used to resolve a filter/sort on a polymorphic
// sub-column (<f>_type / <f>_id). Both sub-columns are physical STORED gen-columns backed by the
// composite (tenant_id, <f>_type, <f>_id) index; treating them as an indexed text field routes
// eq/neq/in straight to the physical column through the existing RM-13 rewrite (physicalCol =
// bare name, cast dropped) — so the non-polymorphic filter path stays byte-identical and only
// this synthesized spec is new.
func polymorphicSubSpec(fieldKey string) FieldSpec {
	return FieldSpec{Name: fieldKey, FieldType: "text", IsIndexed: true}
}

// polymorphicGuardField maps a polymorphic sub-column key back to its base polymorphic field for
// the field-ACL visibility assertion (ACTIVITY-CORE #3a) — so a role that cannot view the
// polymorphic field cannot probe it by filtering/sorting on `<f>_type` / `<f>_id`. Any other key
// (a real declared field, or a system column not in byName) passes through unchanged.
func polymorphicGuardField(byName map[string]FieldSpec, fieldKey string) string {
	if base, ok := polymorphicSubField(byName, fieldKey); ok {
		return base
	}
	return fieldKey
}

// guardedFilterFields is the field-name set to hand AssertFieldsVisible for a filtered read: each
// raw filter key mapped through polymorphicGuardField.
//
// It exists as ONE function because it is the RM-42 anti-oracle guard and it was hand-copied per
// read path: List and Aggregate both wrapped the key, ExportCSV did not, and the omission was
// load-bearing. resolveFiltersForAggregate — the body Export itself calls — deliberately ACCEPTS
// `<poly>_type` / `<poly>_id` as filter keys, while FieldACLService.AssertFieldsVisible only
// rejects names present in the `hidden` set, which is keyed by real sys_fields rows. Generated
// sub-columns are never sys_fields rows, so the assertion returned nil and a role with can_view=0
// on a polymorphic field could filter an EXPORT by exactly that field: the value was stripped from
// the cells, but which ROWS came back reconstructed the hidden association anyway.
//
// So the next filter-accepting read path calls this instead of writing the loop a fourth time.
// Filters only: List additionally guards q.Sort, and Aggregate guards its groupBy through the
// axis's OWN spec map (join.byName for a joined groupBy) — both are separate assertions on purpose
// and must not be folded in here.
func guardedFilterFields(byName map[string]FieldSpec, filters []FilterInput) []string {
	out := make([]string, 0, len(filters))
	for _, fi := range filters {
		out = append(out, polymorphicGuardField(byName, fi.Field))
	}
	return out
}

// canonicalizeFilterValues normalizes date/datetime filter operands to the SAME canonical form
// that normalizeForStore persists, so eq/neq compare identical literals and ordered ops compare
// by instant (D1). datetime → UTC MySQL literal ("2006-01-02 15:04:05"); date → trimmed.
// Values that fail to parse are passed through unchanged (the CAST or the field validator
// surfaces the error downstream — silently dropping them would hide a bad request).
func canonicalizeFilterValues(fieldType string, values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, len(values))
	for i, v := range values {
		switch fieldType {
		case "datetime":
			if canon, ok := canonicalDatetimeUTC(v); ok {
				out[i] = canon
			} else {
				out[i] = strings.TrimSpace(v)
			}
		case "date":
			out[i] = strings.TrimSpace(v)
		default:
			out[i] = v
		}
	}
	return out
}

// FilterFlags carries the dynrepo.Filter boolean flags resolved by validateFilterOperator.
type FilterFlags struct {
	CastNumeric     bool
	CastDatetime    bool
	CaseInsensitive bool
}

// validateFilterOperator checks that the operator is valid for the given field type and
// returns the appropriate FilterFlags plus an optional JSON sub-path (non-empty only for
// currency fields, where the numeric amount lives at "$.field.amount").
// Returns an error with a machine code on mismatch so the handler returns 422.
//
// S2: composite fields reject all value operators (fail-fast 422 instead of silent empty).
// multi_select rejects eq/neq/gt/gte/lt/lte/between/in — contains and isnull/notnull only.
// S3: date/datetime ordered ops set CastDatetime for correct instant-based ordering.
// B1: currency ordered/eq/neq ops target the ".amount" sub-path (numeric scalar).
func validateFilterOperator(f FieldSpec, op dynrepo.FilterOperator, values []string) (flags FilterFlags, jsonSubPath string, err error) {
	// Normalize empty operator to eq.
	if op == "" {
		op = dynrepo.FilterEq
	}
	// specs/083: a stored roll-up lives in data as a plain number / currency and filters as one.
	if isStoredRollup(f) {
		f.FieldType = storedRollupValueType(f)
	}
	// queue #285: a formula's result is written into data at save time, so it filters as its
	// resultType (number by default, like evalFormulaSpec).
	if f.FieldType == "formula" {
		switch rt, _ := f.Options["resultType"].(string); rt {
		case "currency", "text":
			f.FieldType = rt
		default:
			f.FieldType = "number"
		}
	}

	// S2: composite fields cannot be filtered by any value operator.
	if _, unfilterable := unfilterableTypes[f.FieldType]; unfilterable {
		if op != dynrepo.FilterIsNull && op != dynrepo.FilterIsNotNull {
			return FilterFlags{}, "", invalid("FILTER_OPERATOR_NOT_ALLOWED",
				"field type %q (composite) không thể lọc theo giá trị — dùng isnull/notnull", f.FieldType)
		}
	}

	// S2: multi_select is a JSON array — reject all scalar operators except contains/isnull/notnull.
	if _, isMS := multiSelectNoEqTypes[f.FieldType]; isMS {
		switch op {
		case dynrepo.FilterIsNull, dynrepo.FilterIsNotNull, dynrepo.FilterContains:
			// allowed
		default:
			return FilterFlags{}, "", invalid("FILTER_OPERATOR_NOT_ALLOWED",
				"field type %q (multi_select) chỉ hỗ trợ contains/isnull/notnull", f.FieldType)
		}
	}

	switch op {
	case dynrepo.FilterEq, dynrepo.FilterNeq:
		if len(values) != 1 {
			return FilterFlags{}, "", invalid("FILTER_INVALID_ARITY",
				"operator %q trên field %q cần đúng 1 giá trị", op, f.Name)
		}
		_, ci := caseInsensitiveTypes[f.FieldType]
		// D1: eq/neq must be SYMMETRIC with the ordered ops (gt/gte/lt/lte/between) — a raw
		// string comparison makes 10 ≠ 10.0 (number/currency/rating) and mistakes naive vs
		// offset timezones (date/datetime). CAST the JSON path to DECIMAL/DATETIME so equality
		// is by numeric value / by instant, not by lexical bytes. Mutually exclusive with
		// CaseInsensitive: numeric/datetime types are never in caseInsensitiveTypes, so ci is
		// false here and the repo takes the cast branch (record.go buildPredicates eq/neq).
		_, castNum := numericCastTypes[f.FieldType]
		_, castDT := datetimeCastTypes[f.FieldType]
		// B1: currency eq/neq must target the amount sub-path for correct comparison.
		subPath := ""
		if f.FieldType == "currency" {
			subPath = "amount"
		}
		return FilterFlags{CaseInsensitive: ci, CastNumeric: castNum, CastDatetime: castDT}, subPath, nil

	case dynrepo.FilterGt, dynrepo.FilterGte, dynrepo.FilterLt, dynrepo.FilterLte:
		if _, ok := orderedTypes[f.FieldType]; !ok {
			return FilterFlags{}, "", invalid("FILTER_OPERATOR_NOT_ALLOWED",
				"operator %q không được dùng cho field type %q (chỉ dùng cho number/currency/rating/date/datetime)", op, f.FieldType)
		}
		if len(values) != 1 {
			return FilterFlags{}, "", invalid("FILTER_INVALID_ARITY",
				"operator %q trên field %q cần đúng 1 giá trị", op, f.Name)
		}
		_, castNum := numericCastTypes[f.FieldType]
		_, castDT := datetimeCastTypes[f.FieldType]
		// B1: currency ordered ops must target the amount sub-path.
		subPath := ""
		if f.FieldType == "currency" {
			subPath = "amount"
		}
		return FilterFlags{CastNumeric: castNum, CastDatetime: castDT}, subPath, nil

	case dynrepo.FilterBetween:
		if _, ok := orderedTypes[f.FieldType]; !ok {
			return FilterFlags{}, "", invalid("FILTER_OPERATOR_NOT_ALLOWED",
				"operator %q không được dùng cho field type %q (chỉ dùng cho number/currency/rating/date/datetime)", op, f.FieldType)
		}
		if len(values) != 2 {
			return FilterFlags{}, "", invalid("FILTER_INVALID_ARITY",
				"operator between trên field %q cần đúng 2 giá trị (lo,hi), nhận %d", f.Name, len(values))
		}
		_, castNum := numericCastTypes[f.FieldType]
		_, castDT := datetimeCastTypes[f.FieldType]
		subPath := ""
		if f.FieldType == "currency" {
			subPath = "amount"
		}
		return FilterFlags{CastNumeric: castNum, CastDatetime: castDT}, subPath, nil

	case dynrepo.FilterIn:
		if _, ok := multiValueTypes[f.FieldType]; !ok {
			return FilterFlags{}, "", invalid("FILTER_OPERATOR_NOT_ALLOWED",
				"operator %q không được dùng cho field type %q (chỉ dùng cho text/email/picklist/multi_select/relation)", op, f.FieldType)
		}
		if len(values) == 0 {
			return FilterFlags{}, "", invalid("FILTER_INVALID_ARITY",
				"operator in trên field %q cần ít nhất 1 giá trị", f.Name)
		}
		_, ci := caseInsensitiveTypes[f.FieldType]
		return FilterFlags{CaseInsensitive: ci}, "", nil

	case dynrepo.FilterIsNull, dynrepo.FilterIsNotNull:
		if len(values) != 0 {
			return FilterFlags{}, "", invalid("FILTER_INVALID_ARITY",
				"operator %q trên field %q không nhận giá trị", op, f.Name)
		}
		return FilterFlags{}, "", nil

	case dynrepo.FilterContains:
		// text/email/rich_text and multi_select (S2: only allowed value op for multi_select).
		_, isTextLike := textLikeTypes[f.FieldType]
		_, isMultiSelect := multiSelectNoEqTypes[f.FieldType]
		if !isTextLike && !isMultiSelect {
			return FilterFlags{}, "", invalid("FILTER_OPERATOR_NOT_ALLOWED",
				"operator contains không được dùng cho field type %q (chỉ dùng cho text/email/rich_text/multi_select)", f.FieldType)
		}
		if len(values) != 1 {
			return FilterFlags{}, "", invalid("FILTER_INVALID_ARITY",
				"operator contains trên field %q cần đúng 1 giá trị", f.Name)
		}
		return FilterFlags{}, "", nil // contains handles case-insensitivity via COLLATE internally

	default:
		return FilterFlags{}, "", invalid("FILTER_UNKNOWN_OPERATOR",
			"operator %q không hợp lệ (chấp nhận: eq,neq,gt,gte,lt,lte,in,between,isnull,notnull,contains)", op)
	}
}

// ListResult is the unified return value for List: it covers both offset mode (Total set) and
// cursor mode (NextCursor set, Total==0). The handler branches on CursorMode to decide which
// envelope fields to emit.
type ListResult struct {
	Records    []dynrepo.Record
	Total      int    // offset mode only (0 in cursor mode)
	NextCursor string // cursor mode only ("" means last page or offset mode)
	CursorMode bool   // true when the request ran in cursor mode (?paginate=cursor or ?after=)
	// HiddenColumns is the number of DISTINCT declared fields of THIS entity the caller's
	// field-ACL hides (can_view=0) — computed from the SAME hidden set stripReadFields/
	// applyLookups already load per request (HIDDEN-COL-SIGNAL; no second ACL path). It is a
	// per-entity count, NOT per-row. Admins and a nil field-ACL enforcer both yield 0 (they
	// bypass the strip), so the handler can emit meta.hiddenColumns only when > 0 and an admin
	// never sees a false-positive "hidden columns" signal.
	HiddenColumns int
}

// resolvePaginationMode decides offset vs keyset for one List request (CURSOR-BOOTSTRAP #42) and
// refuses every combination it cannot serve honestly, instead of quietly dropping a parameter:
//   - ?paginate= must be absent, "offset" or "cursor" → else 422 INVALID_PAGINATION_MODE;
//   - keyset runs for ?paginate=cursor (from the first page) or a non-empty ?after=;
//   - ?page= in keyset mode, ?paginate=offset with ?after=, and keyset on the offset-only recycle
//     bin are all 422 PAGINATION_MODE_CONFLICT.
func resolvePaginationMode(q ListQuery) (bool, error) {
	switch q.Paginate {
	case "", "offset", "cursor":
	default:
		return false, invalid("INVALID_PAGINATION_MODE",
			"paginate %q không hợp lệ (chấp nhận: cursor, offset)", q.Paginate)
	}
	if q.Paginate == "offset" && q.After != "" {
		return false, invalid("PAGINATION_MODE_CONFLICT",
			"paginate=offset không dùng chung với after (after là con trỏ của chế độ cursor)")
	}
	if q.Paginate != "cursor" && q.After == "" {
		return false, nil
	}
	if q.PageExplicit {
		return false, invalid("PAGINATION_MODE_CONFLICT",
			"chế độ cursor không nhận page; trang kế lấy bằng after=pagination.nextCursor")
	}
	if q.OnlyDeleted {
		return false, invalid("PAGINATION_MODE_CONFLICT",
			"thùng rác chỉ phân trang theo page (offset), không hỗ trợ cursor")
	}
	return true, nil
}

// cursorError maps a dynrepo cursor error to 400 INVALID_CURSOR, naming the sort/order mismatch
// (a token replayed under another ?sort=/?order= than the page that minted it) so the caller knows
// to resend the chain's original sort/order rather than retry the token.
func cursorError(err error) error {
	if errors.Is(err, dynrepo.ErrCursorSortMismatch) {
		return invalid("INVALID_CURSOR",
			"cursor thuộc chuỗi có sort/order khác; mọi trang của một chuỗi cursor phải gửi đúng sort/order của trang đầu")
	}
	return invalid("INVALID_CURSOR", "cursor không hợp lệ hoặc đã hết hạn")
}

// List returns a tenant-scoped page of records. It validates every filter against the
// entity's declared fields (unknown field -> 422, operator mismatch -> 422), builds the sort
// allowlist for dynamic field sorting (RM-09), applies case-insensitive comparisons for
// text/picklist/email (RM-14), resolves ?q to a substring search over the string-typed
// declared columns, then delegates to the store.
//
// Cursor mode (D2): when resolvePaginationMode says so (?paginate=cursor or a non-empty q.After)
// the store's ListRecordsCursor path is used (no COUNT, O(1) per page). Sort MUST be a physical
// indexed column — 422 otherwise. Offset mode: when q.Page > maxOffsetPage → 422 PAGE_TOO_DEEP
// (cap at 1000).
func (s *RecordService) List(ctx context.Context, scope RecordScope, tenantID, entityName string, q ListQuery) (ListResult, error) {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return ListResult{}, err
	}
	cursorMode, err := resolvePaginationMode(q)
	if err != nil {
		return ListResult{}, err
	}

	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}

	// RM-42 anti-oracle (queue #317, api-conventions §4.4b): the caller's hidden fields are resolved
	// ONCE, before anything is validated, and the sort allowlist and the filter-key map are built
	// WITHOUT them — so a hidden name is refused by the very same branch, with the very same
	// error, as a name the entity never declared (SORT_NOT_ALLOWED / LIST_UNKNOWN_FILTER), instead
	// of a 403 FIELD_FORBIDDEN that confirms the field exists (or an operator/type error that does
	// the same one step earlier). A load failure fails closed. Admin / no enforcer → nil, no change.
	hidden, err := s.computedHiddenFields(ctx, scope, tenantID, e.Name)
	if err != nil {
		return ListResult{}, fmt.Errorf("services.List: field-acl: %w", err)
	}
	filterByName := visibleByName(byName, hidden)

	// Build sort allowlist (RM-09): system cols + unique + relation + D1 indexed generated columns.
	sortAllowlist := buildSortAllowlist(visibleSpecs(specs, hidden))

	// Resolve sort: translate fieldName → physicalColName, reject unknown (422, no silent fallback).
	sortPhysical := ""
	if q.Sort != "" {
		col, ok := sortAllowlist[q.Sort]
		if !ok {
			return ListResult{}, invalid("SORT_NOT_ALLOWED",
				"field %q không thể sắp xếp (cần là field unique, relation, hoặc cột hệ thống: id/created_at/updated_at/ownerId)", q.Sort)
		}
		sortPhysical = col
	}

	// Normalize order to upper-case for consistent keyset comparator selection below.
	order := "DESC"
	if q.Order == "asc" || q.Order == "ASC" {
		order = "ASC"
	}

	// Validate and resolve filters (RM-08, RM-14) — shared between offset and cursor modes.
	var resolvedFilters []dynrepo.Filter
	for _, fi := range q.Filters {
		spec, ok := filterByName[fi.Field]
		if !ok {
			// ACTIVITY-CORE #3a: a `<poly>_type` / `<poly>_id` key for a real polymorphic_relation
			// field is a valid filter routed to its physical gen-column (the timeline query). Any
			// other unknown key stays LIST_UNKNOWN_FILTER — the single-table path is byte-identical.
			// (filterByName has the caller's hidden fields removed: a hidden name, and the sub-columns
			// of a hidden polymorphic field, land in the unknown branch below — queue #317.)
			if _, isSub := polymorphicSubField(filterByName, fi.Field); isSub {
				spec = polymorphicSubSpec(fi.Field)
			} else if _, isOwner := ownerReservedKeys[fi.Field]; isOwner {
				// OWNER-DIMENSION: owner_id is a physical system column that is NEVER registered
				// in sys_fields, so byName can never hold it and "lọc theo người phụ trách" — the
				// one question every list gets asked — was a hard 422 for every client. Admitted
				// here, AFTER the polymorphic sub-column check, so a declared polymorphic field
				// literally named "owner" keeps its own `owner_id` sub-column (declared metadata
				// beats a reserved key, the same precedence byName already has). The value stays a
				// `?` placeholder (dynrepo.Filter.Values) and the query keeps both its mandatory
				// `tenant_id = ?` predicate and its A5 row filter — a filter can only narrow.
				spec = ownerFilterSpec()
				// Normalize the wire key to the physical column name before it travels further:
				// dynrepo re-sanitizes Filter.Column with ddl.SanitizeIdentifier, which accepts
				// ^[a-z][a-z0-9_]*$ only — the camelCase "ownerId" spelling would fail there and
				// surface as a 500 instead of a filter. fi is the range COPY, so this rewrites
				// only the Filter built below; the field-ACL guard further down still sees the
				// key the client actually sent.
				fi.Field = ownerPhysicalColumn
			} else {
				return ListResult{}, invalid("LIST_UNKNOWN_FILTER",
					"không thể lọc theo field %q (không có trong metadata)", fi.Field)
			}
		}

		op := fi.Operator
		if op == "" {
			op = dynrepo.FilterEq
		}

		// Wave 3.6: relative-date tokens (today/this_week/…) rewrite the operator+operands
		// server-side, and a garbage date operand is now a loud 422 instead of a silent CAST-NULL
		// empty result. Runs BEFORE validateFilterOperator (a token eq becomes between).
		fiValues := fi.Values
		if spec.FieldType == "date" || spec.FieldType == "datetime" {
			var derr error
			op, fiValues, derr = resolveDateFilters(spec.FieldType, op, fiValues, time.Now())
			if derr != nil {
				return ListResult{}, derr
			}
		}

		flags, jsonSubPath, verr := validateFilterOperator(spec, op, fiValues)
		if verr != nil {
			return ListResult{}, verr
		}

		// For unique fields: redirect eq/neq/in to the physical generated column so the
		// B-tree index is used instead of JSON_EXTRACT (RM-13 rewrite for indexed fields).
		// Relation/user fields: redirect to <field>_id physical column (USER-FIELD reuses
		// relation's shape — see buildSortAllowlist).
		// Indexed fields (D1): redirect to the physical generated column (bare field name).
		// This also makes eq on unique fields exact (the physical column uses utf8mb4_bin).
		// Currency fields use JSONSubPath instead (B1) — no PhysicalColumn needed.
		physicalCol := ""
		indexedTyped := false
		if spec.IsUnique {
			physicalCol = spec.Name
		} else if spec.FieldType == "relation" || spec.FieldType == "user" {
			physicalCol = spec.Name + "_id"
		} else if spec.IsIndexed {
			// D1: indexed field has a TYPED physical generated column (bare field name) —
			// DECIMAL (number/rating/currency-amount), DATETIME (date/datetime), or VARCHAR
			// (text/email/picklist). Route the predicate to it so the B-tree index is used.
			physicalCol = spec.Name
			indexedTyped = true
		}

		// For relation/user fields: UUIDs are always exact — never case-insensitive.
		ci := flags.CaseInsensitive
		if spec.FieldType == "relation" || spec.FieldType == "user" {
			ci = false
		}

		// Drop the redundant CAST when targeting an indexed typed column. The generated
		// column is ALREADY DECIMAL/DATETIME, so CAST(col AS DECIMAL/DATETIME) forces the
		// optimizer from a range scan to a ref scan and defeats the B-tree — verified via
		// EXPLAIN (100 → 2552 rows on a 5k-row table). Compare the typed column directly.
		// Unique columns are VARCHAR(255) (not numeric) so their CAST MUST stay for a correct
		// numeric comparison — hence this applies ONLY to the indexed-typed branch.
		castNumeric := flags.CastNumeric
		castDatetime := flags.CastDatetime
		if indexedTyped {
			castNumeric = false
			castDatetime = false
		}

		// D1: canonicalize date/datetime filter operands the SAME way store-time values are
		// (normalizeForStore) so eq/neq compare identical literals and ordered ops compare by
		// instant. A datetime operand in any offset → UTC MySQL literal; a date operand is
		// trimmed. Values that fail to parse are left untouched (the cast/handler surfaces the
		// error). This must mirror normalizeForStore exactly or eq would silently miss.
		filterValues := fiValues
		if spec.FieldType == "datetime" || spec.FieldType == "date" {
			filterValues = canonicalizeFilterValues(spec.FieldType, fiValues)
		}

		resolvedFilters = append(resolvedFilters, dynrepo.Filter{
			Column:          fi.Field,
			Operator:        op,
			Values:          filterValues,
			Value:           firstOrEmpty(filterValues),
			CastNumeric:     castNumeric,
			CastDatetime:    castDatetime,
			CaseInsensitive: ci,
			JSONSubPath:     jsonSubPath,
			PhysicalColumn:  physicalCol,
		})
	}

	// W4 run overlay: append the created_at/created_by system-column predicates AFTER the
	// declared-field filters (AND semantics; applied identically in offset and cursor mode).
	// System columns are not sys_fields entries → no field-ACL dimension; the declared-filter
	// ACL check below covers q.Filters unchanged.
	resolvedFilters = append(resolvedFilters, overlayFilters(q.Overlay)...)

	// RM-42 anti-oracle: a non-admin may not FILTER or SORT on a field its role cannot view —
	// otherwise eq/gt/lt filters (or sort order) reconstruct a hidden value from row counts even
	// though the value is stripped from the response. Since queue #317 the hidden keys never get this
	// far — filterByName / sortAllowlist (built above without them) already refused them as unknown
	// names. This stays as the fail-closed backstop for a key some future branch lets through: it
	// answers 403 FIELD_FORBIDDEN, which is safe but names the field. Admin bypasses inside the
	// enforcer. (?q search is handled separately by dropping hidden columns from the search set
	// rather than rejecting the whole search.)
	if s.fieldACL != nil {
		guarded := guardedFilterFields(byName, q.Filters)
		if q.Sort != "" {
			guarded = append(guarded, polymorphicGuardField(byName, q.Sort))
		}
		if err := s.fieldACL.AssertFieldsVisible(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin, guarded); err != nil {
			return ListResult{}, err
		}
	}

	// Resolve search columns (shared between both modes).
	var resolvedSearch *dynrepo.Search
	if term := strings.TrimSpace(q.Search); term != "" {
		// RM-42: drop columns the role may not view from the ?q search set so the substring
		// search never matches on a hidden field (which would be a probing oracle). Admin sees
		// an empty hidden set. A load error already failed closed when hidden was resolved above.
		var cols []string
		for _, f := range specs {
			if _, ok := searchableTypes[f.FieldType]; ok {
				if _, isHidden := hidden[f.Name]; isHidden {
					continue
				}
				cols = append(cols, f.Name)
			}
		}
		if len(cols) > 0 {
			resolvedSearch = &dynrepo.Search{Term: term, Columns: cols}
		}
	}

	// ── Cursor mode (D2): ?paginate=cursor (first page, After=="") or a non-empty q.After ────
	// The first page runs the SAME keyset query as every later page, so its nextCursor is taken
	// from the keyset ORDER BY (col IS NULL, col, id) — never from an offset page, whose ORDER BY
	// has no id tie-break or NULL tail and would gap/dupe at the boundary (B1).
	if cursorMode {
		// Cursor mode requires the sort column to be a physical indexed column (RM-09 / D2).
		// System columns (created_at, updated_at, id) are always indexed; unique/relation/D1
		// indexed fields are also in the allowlist. Unindexed JSON fields are not allowed.
		// Note: sortPhysical is "" when q.Sort is "" → falls back to "created_at" in the
		// repo, which IS a system indexed column — allowed.
		if q.Sort != "" {
			if _, inAllowlist := sortAllowlist[q.Sort]; !inAllowlist {
				return ListResult{}, invalid("CURSOR_REQUIRES_INDEXED_SORT",
					"cursor pagination yêu cầu sort trên cột có index (unique/relation/D1/system); field %q không thỏa", q.Sort)
			}
		}

		// The column the keyset runs on: the resolved physical sort, or the repo default when no
		// ?sort= is sent. The cursor is bound to it (and to order), so it is also what we pass down.
		cursorSort := sortPhysical
		if cursorSort == "" {
			cursorSort = dynrepo.DefaultCursorSort
		}

		// Validate the cursor here (in the service layer) so the unit-test fake store can
		// confirm malformed cursors are rejected before they ever reach the DB. The repo
		// re-decodes for actual query construction, but this early check returns a clean
		// INVALID_CURSOR error to the caller before any store call. An empty After is the first
		// page of a ?paginate=cursor chain (DecodeCursor rejects "", so it must not be decoded).
		// A token minted under another sort/order is refused by name (specs/039 §2.1): replaying
		// it would compare the boundary value against the wrong column — a wrong page, or, after
		// MySQL's type coercion, an empty last page that ends a sync job early in silence.
		if q.After != "" {
			if _, _, _, decErr := dynrepo.DecodeCursor(q.After, cursorSort, order); decErr != nil {
				return ListResult{}, cursorError(decErr)
			}
		}

		// D3 (reconciled 2026-06-25): no collation override is needed. The keyset cursor is
		// loss-free on collation-folding text/picklist/email sort columns because the repo's
		// ORDER BY (col, id) and keyset predicate share the same default collation and the unique
		// `id` tie-break follows the sort direction → (col, id) is a strict total order. Verified
		// empirically (abc/ABC/Abc limit-1 walk, ASC+DESC, returns every row). Forcing
		// COLLATE utf8mb4_bin would change the visible sort to codepoint order (wrong for VN) and
		// make cursor-mode disagree with offset-mode (which is ci) — see ListRecordsCursor.
		cp := dynrepo.CursorParams{
			Limit:   q.Limit,
			After:   q.After,
			Sort:    cursorSort,
			Order:   order,
			Filters: resolvedFilters,
			Search:  resolvedSearch,
		}
		records, nextCursor, cerr := s.store.ListRecordsCursor(ctx, e.Name, tenantID, scope.toRowScope(), cp)
		if cerr != nil {
			// Also map dynrepo.ErrInvalidCursor from the repo (belt-and-suspenders).
			if errors.Is(cerr, dynrepo.ErrInvalidCursor) {
				return ListResult{}, cursorError(cerr)
			}
			return ListResult{}, cerr
		}
		// RM-42 read strip BEFORE expand so a hidden relation field is never resolved to a label.
		if err := s.stripReadFields(ctx, scope, tenantID, e.Name, records); err != nil {
			return ListResult{}, fmt.Errorf("services.List: field-acl: %w", err)
		}
		// COMPUTED writes (lookup/rollup) run AFTER the strip — gate them on the same hidden set
		// (resolved once, above, so the filter guard, the strip and these can never disagree).
		// ENG-COMPUTED #2: lookup fields are ALWAYS resolved, BEFORE applyExpand (see Get).
		denied := map[string]struct{}{} // read-time operands the caller may not see (formula_derived.go)
		if err := applyLookups(ctx, s.lookupFetcher, s.authz, s.fieldACL, scope, tenantID, records, specs, hidden, denied); err != nil {
			return ListResult{}, fmt.Errorf("services.List: lookup: %w", err)
		}
		if s.fetcher != nil && len(q.Expand) > 0 {
			if err := s.expandRecords(ctx, scope, tenantID, records, specs, q.Expand); err != nil {
				return ListResult{}, fmt.Errorf("services.List: expand: %w", err)
			}
		}
		if err := s.applyRollups(ctx, scope, tenantID, records, specs, withDerivedRollups(q.Rollup, specs, hidden), hidden, denied); err != nil {
			return ListResult{}, fmt.Errorf("services.List: rollup: %w", err)
		}
		applyDerivedFormulas(specs, records, hidden, denied)
		return ListResult{Records: records, NextCursor: nextCursor, CursorMode: true,
			HiddenColumns: countHiddenColumns(specs, hidden)}, nil
	}

	// ── Offset mode: page cap guard (D2 spec: max 1000 pages → 422 PAGE_TOO_DEEP) ──────────
	if q.Page > dynrepo.MaxOffsetPage {
		return ListResult{}, invalid("PAGE_TOO_DEEP",
			"số trang vượt quá giới hạn (%d); dùng cursor pagination (?paginate=cursor, rồi after=pagination.nextCursor) để phân trang sâu", dynrepo.MaxOffsetPage)
	}

	params := dynrepo.ListParams{
		Page:  q.Page,
		Limit: q.Limit,
		// The WIRE field name, not sortPhysical: the store's normalize() translates Sort through
		// SortAllowlist (keyed by field name) and rejects a miss with ErrUnsortableField. Handing
		// it the already-translated column made every relation/user sort (`account` → `account_id`,
		// where the physical name is not a key) a 500 RECORD_OP_FAILED while the allowlist above
		// had just said "sortable" (DAILY-QUEUE #4 acceptance). The cursor path keeps sortPhysical —
		// ListRecordsCursor takes the physical column and has no allowlist of its own.
		Sort:          q.Sort,
		Order:         order,
		SortAllowlist: sortAllowlist,
		Filters:       resolvedFilters,
		Search:        resolvedSearch,
		OnlyDeleted:   q.OnlyDeleted, // W9b recycle bin (offset-only)
	}

	records, total, err := s.store.ListRecords(ctx, e.Name, tenantID, scope.toRowScope(), params)
	if err != nil {
		return ListResult{}, err
	}
	// RM-42 read strip BEFORE expand so a hidden relation field is never resolved to a label.
	if err := s.stripReadFields(ctx, scope, tenantID, e.Name, records); err != nil {
		return ListResult{}, fmt.Errorf("services.List: field-acl: %w", err)
	}
	// COMPUTED writes (lookup/rollup) run AFTER the strip — gate them on the same hidden set
	// (resolved once, above).
	// ENG-COMPUTED #2: lookup fields are ALWAYS resolved, BEFORE applyExpand (see Get).
	denied := map[string]struct{}{} // read-time operands the caller may not see (formula_derived.go)
	if err := applyLookups(ctx, s.lookupFetcher, s.authz, s.fieldACL, scope, tenantID, records, specs, hidden, denied); err != nil {
		return ListResult{}, fmt.Errorf("services.List: lookup: %w", err)
	}
	if s.fetcher != nil && len(q.Expand) > 0 {
		if err := s.expandRecords(ctx, scope, tenantID, records, specs, q.Expand); err != nil {
			return ListResult{}, fmt.Errorf("services.List: expand: %w", err)
		}
	}
	if err := s.applyRollups(ctx, scope, tenantID, records, specs, withDerivedRollups(q.Rollup, specs, hidden), hidden, denied); err != nil {
		return ListResult{}, fmt.Errorf("services.List: rollup: %w", err)
	}
	applyDerivedFormulas(specs, records, hidden, denied)
	return ListResult{Records: records, Total: total, HiddenColumns: countHiddenColumns(specs, hidden)}, nil
}

// visibleSpecs is specs without the fields in hidden — what a caller may name as a sort/filter key
// (queue #317). hidden empty/nil (admin, no enforcer, nothing hidden) returns specs itself.
func visibleSpecs(specs []FieldSpec, hidden map[string]struct{}) []FieldSpec {
	if len(hidden) == 0 {
		return specs
	}
	out := make([]FieldSpec, 0, len(specs))
	for _, f := range specs {
		if _, h := hidden[f.Name]; !h {
			out = append(out, f)
		}
	}
	return out
}

// visibleByName is byName without the fields in hidden; see visibleSpecs. The input map is never
// mutated (other guards still read the full one).
func visibleByName(byName map[string]FieldSpec, hidden map[string]struct{}) map[string]FieldSpec {
	if len(hidden) == 0 {
		return byName
	}
	out := make(map[string]FieldSpec, len(byName))
	for n, f := range byName {
		if _, h := hidden[n]; !h {
			out[n] = f
		}
	}
	return out
}

// countHiddenColumns returns how many of the entity's DECLARED fields sit in the caller's
// field-ACL hidden set (HIDDEN-COL-SIGNAL). hidden is the exact per-request set
// computedHiddenFields already resolved for the lookup/rollup gate — reused here so the count
// can never drift from what stripReadFields actually delete()s from rows (B6 anti-drift: one
// ACL path, one truth). The intersection with specs matters: a stale ACL restriction naming a
// field later deleted from the entity must not inflate the count the FE turns into an honest
// "N cột ẩn bởi quyền" chip. len(hidden)==0 covers admin/nil-enforcer (both yield an empty set).
func countHiddenColumns(specs []FieldSpec, hidden map[string]struct{}) int {
	if len(hidden) == 0 {
		return 0
	}
	n := 0
	for _, f := range specs {
		if _, ok := hidden[f.Name]; ok {
			n++
		}
	}
	return n
}

// firstOrEmpty returns the first element of a slice, or "" if the slice is empty.
func firstOrEmpty(ss []string) string {
	if len(ss) > 0 {
		return ss[0]
	}
	return ""
}

// stripReadFields applies the RM-42 read strip to a batch of records IN PLACE: each record's
// data loses the fields the role may not view. Admin bypasses (the enforcer short-circuits). It
// loads the (tenant, role, entity) restriction set ONCE per call (the enforcer caches behind the
// ACL cache) and reuses it across rows — so a 100-row page costs one grant lookup, not one per
// row. A nil enforcer (field-level security disabled) is a no-op. A load failure fails closed
// inside the enforcer (drops all business fields) and is surfaced as an error so the caller can
// refuse the whole page rather than return half-stripped rows.
func (s *RecordService) stripReadFields(ctx context.Context, scope RecordScope, tenantID, entity string, records []dynrepo.Record) error {
	if s.fieldACL == nil || scope.IsAdmin || len(records) == 0 {
		return nil
	}
	datas := make([]map[string]any, len(records))
	for i := range records {
		datas[i] = records[i].Data
	}
	return s.fieldACL.FilterReadFieldsBatch(ctx, tenantID, scope.RoleID, entity, scope.IsAdmin, datas)
}

// computedHiddenFields resolves the caller's field-ACL hidden set on the CURRENT entity, used to
// gate the COMPUTED-field writers (applyLookups/applyRollups) that run AFTER stripReadFields —
// without it, a can_view=0 lookup/rollup field stripped from stored data would be silently
// re-added by the computed write (the RM-42/A6 bypass this closes). nil enforcer (field-level
// security unwired) and admin both yield a nil set (no restriction), matching stripReadFields'
// bypass exactly. FAIL-CLOSED: a load error aborts the read (the caller refuses the page rather
// than serving computed values it could not verify).
func (s *RecordService) computedHiddenFields(ctx context.Context, scope RecordScope, tenantID, entity string) (map[string]struct{}, error) {
	if s.fieldACL == nil || scope.IsAdmin {
		return nil, nil
	}
	hidden, err := s.fieldACL.HiddenFields(ctx, tenantID, scope.RoleID, entity, scope.IsAdmin)
	if err != nil {
		return nil, fmt.Errorf("services.computedHiddenFields: %w", err)
	}
	return hidden, nil
}

// HiddenFields is the caller's field-ACL hidden set on entity: the SAME set every read of this
// service strips (nil for an admin or when field-level security is unwired, exactly like the
// reads). Exported for the ⌘K assistant's field listing, so the fields it describes to the model
// are the fields a normal read would serve this caller (queue #143 follow-up). FAIL-CLOSED: a load
// error is returned, never an empty set.
func (s *RecordService) HiddenFields(ctx context.Context, scope RecordScope, tenantID, entity string) (map[string]struct{}, error) {
	return s.computedHiddenFields(ctx, scope, tenantID, entity)
}

// Update applies a partial (apply-when-present) change to a record's data. scope enforces A5
// row-level access: a record the caller may not see (per access_level) is not modified and
// surfaces as ErrRecordNotFound. Only fields PRESENT in payload are validated and merged into
// the stored data JSON; fields absent from the body are left untouched (no null/delete, no
// default re-materialization). This mirrors the partial-safe PUT contract of the static
// admin resources (user/role/team) and the EditField metadata path.
//
// RM-24 optimistic locking (OPT-IN): expectedVersion carries the version the caller is editing
// (from If-Match / body.version). When non-nil the store performs a version-guarded UPDATE and
// a mismatch surfaces as RECORD_VERSION_CONFLICT (409) with details.currentVersion. When nil
// (the client sent no version) the update is unconditional last-writer-wins — backward-compatible.
func (s *RecordService) Update(ctx context.Context, scope RecordScope, tenantID, entityName, id string, payload map[string]any, expectedVersion *int) error {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return err
	}
	// RM-42 write guard: reject BEFORE validation/update if the payload names a field the role
	// may not edit. Applied on the raw payload keys the client actually sent (apply-when-present),
	// so a locked field is refused even when its value would otherwise be valid — mirroring the
	// Create write guard so update can never be a back-door around field-level edit restrictions.
	// Admin bypasses. Fail-closed inside the enforcer.
	// Current values, read ONCE for the whole update path. Both the field-ACL check and the
	// relation-FK check need "what does this record hold today" to tell an EDIT from a resend, and
	// two reads for one question is how the two answers drift. Best-effort: a failed read yields
	// nil, which restores the strict behaviour everywhere below — fail closed, never open.
	var currentVals map[string]any
	rec0, gerr := s.store.GetRecord(ctx, e.Name, tenantID, id, scope.toRowScope())
	if gerr == nil && rec0 != nil {
		currentVals = rec0.Data
	} else if errors.Is(gerr, ErrRecordNotFound) {
		// specs/051 (#226): the caller's edit scope does not reach the record. If it is visible to
		// them only through a manual share, say so NOW — the field/relation checks below run with
		// currentVals == nil (strict) and would otherwise answer 403 FIELD_* / 422 before the
		// read-only verdict FR-005 promises. Any other outcome falls through to the old flow.
		if serr := s.sharedReadOnlyOr(ctx, scope, tenantID, e.Name, id, gerr); errors.Is(serr, ErrRecordSharedReadOnly) {
			return serr
		}
	}
	if s.fieldACL != nil {
		// Compared against the record's CURRENT values, so a locked field carrying its existing
		// value is not counted as an edit. The form sends every field on update (that is what
		// lets a user CLEAR one), so without this a single read-only field made every save on the
		// record fail — marking one field read-only took the whole record away. A locked field
		// carrying a DIFFERENT value is still refused, which is the part that was ever enforcement.
		// A failed read yields nil and the strict behaviour: fail closed, never open.
		if err := s.fieldACL.CheckWriteFieldsAgainst(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin, payload, currentVals); err != nil {
			// A hidden key reads exactly like an undeclared one (queue #317): finish through ValidatePartialRecord.
			return finishHiddenKeyRefusal(err, func(h map[string]struct{}) error {
				_, verr := validatePartialRecord(specs, payload, h)
				return verr
			})
		}
	}
	clean, err := ValidatePartialRecord(specs, payload)
	if err != nil {
		return err
	}
	// specs/052: dependent picklist pairs — judged on the pair this PUT sends, filled in from the
	// stored record, and skipped when it merely resends the stored pair (the form sends every field).
	if err := checkDependentPicklists(specs, clean, currentVals); err != nil {
		// queue #321: the stored child may be hidden from this caller — the refusal must not name it.
		return s.requiredLockExplainer(ctx, scope, tenantID, e.Name, specs)(err)
	}
	// Queue #63: the unchanged-value rule for a LEGACY currency value (stored code ≠ configured,
	// written before the Q7 guard). The form never shows the stored code — it stamps the
	// configured one over it and resends every field — so "edit the name, save" turned USD 100
	// into VND 100. A same-amount resend is not this caller's edit: keep the stored value.
	s.keepLegacyCurrencyValues(tenantID, e.Name, id, specs, clean, currentVals)
	// Same unchanged-value rule, applied to relation FKs — and for the same reason, one step
	// further along. The form resends EVERY field on update, so a relation whose target was later
	// deleted came back verbatim and failed the FK probe on every save: the record became
	// permanently uneditable, and for a REQUIRED relation there was no way out at all (clearing it
	// is 422 RECORD_REQUIRED_MISSING, resending it is 422 RECORD_RELATION_INVALID). Deleting one
	// record took every record that pointed at it away — exactly what the read-only-field version
	// of this bug did, and the comment above is the fix that was written for that one.
	//
	// A relation the caller is actually CHANGING is still probed in full: you may not point a
	// record at a target that does not exist. This only stops re-litigating a value the caller did
	// not touch and cannot repair.
	if err := s.validateRelationsAgainst(ctx, scope, tenantID, specs, clean, currentVals); err != nil {
		return err
	}
	// ACTIVITY-CORE #3a: tenant-scoped existence probe for polymorphic_relation values on PUT too
	// (a partial update may set/replace <f>) — same guard as create (mirrors validateRelations).
	if err := s.validatePolymorphicRelations(ctx, scope, tenantID, specs, clean, currentVals); err != nil {
		return err
	}
	// USER-FIELD: same existence probe on PUT too (a partial update may set/replace a `user` field).
	if err := s.validateUserFields(ctx, tenantID, specs, clean); err != nil {
		return err
	}
	// Capture the before-image (best-effort, scoped) for two purposes:
	// 1. Audit diff — reflects only the fields that actually changed.
	// 2. Formula recompute — partial PUT may only touch one field; computing formula on
	//    `clean` alone would resolve missing refs as 0 (corrupting the result). We merge
	//    before ⊕ clean into a full-record map, run applyComputedFields on it, then copy
	//    each formula result back into `clean` so the store's JSON_MERGE_PATCH receives
	//    the updated computed value. A read failure is fail-open: formula falls back to
	//    computing from `clean` only (missing fields resolve to 0) — same as Create.
	var before map[string]any
	var ownerUserID string
	var createdAt time.Time // set_elapsed's start instant; zero when the read below fails (the action then errors)
	rec, readErr := s.store.GetRecord(ctx, e.Name, tenantID, id, scope.toRowScope())
	found := readErr == nil && rec != nil
	if found {
		before = rec.Data
		createdAt = rec.CreatedAt
		// The record's immutable owner (created_by) — NOT scope.UserID, which is the mutator
		// applying THIS update. Source for the workflow engine's "$owner" notification sentinel
		// (M7 MED-1 fix). Empty when the best-effort read above failed — the engine's resolver
		// then falls through to its existing empty-recipient error rather than a dead insert.
		// EFFECTIVE owner (owner_id when set, else created_by) — the same value ACL enforces on.
		// Was rec.CreatedBy: after "Đổi chủ" a "$owner" notification still went to the CREATOR,
		// and a create_record action still stamped the creator as the new row's owner.
		ownerUserID = rec.OwnerID
	}
	// Recompute formula fields using the full merged data so a partial PUT on one field
	// (e.g. only probability) still yields a correct weighted_value.
	merged := mergeAfterImage(before, clean)
	applyComputedFields(specs, merged)
	// specs/048: the before_save rules fill values into clean + merged (S4), then judge the record
	// exactly as it will be stored — merged, formulas recomputed — AFTER every field check above
	// (their error order is unchanged) and before the one store call, so a vetoed save writes nothing
	// and a filled value travels in the same UPDATE and the same audit row.
	commitHooks, err := s.checkBeforeUpdate(ctx, scope, tenantID, e.Name, id, specs, clean, merged, before, found, readErr)
	if err != nil {
		return err
	}
	// Copy formula results from merged back into clean so UpdateRecord persists them.
	for _, sp := range specs {
		if sp.FieldType == "formula" {
			clean[sp.Name] = merged[sp.Name]
		}
	}
	uerr := s.store.UpdateRecord(ctx, e.Name, tenantID, id, scope.UserID, scope.toRowScope(), clean, expectedVersion, uniqueFieldSpecs(specs))
	if uerr != nil {
		// specs/051: a record the caller sees only through a manual share is read-only → 403.
		return asDataConstraint(asDuplicate(asVersionConflict(s.sharedReadOnlyOr(ctx, scope, tenantID, e.Name, id, uerr)), e.Name, specs))
	}
	commitHooks()
	// specs/083: the row may have changed amount or moved to another parent — refresh both totals.
	s.refreshStoredRollups(ctx, tenantID, e.Name, before, clean)
	// RM-19: audit the committed update. The diff compares the changed fields' prior values
	// (from before) to their new values (clean) — unchanged keys outside `clean` are not in
	// the diff. Async + fail-open.
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Action:      audit.ActionUpdate,
		Diff:        auditUpdateDiff(specs, before, clean),
		At:          time.Now().UTC(),
	})
	// EPIC 4: fire after_update workflows. The event data is the FULL after-image (the prior
	// record merged with the changed fields) so a rule's condition can reference ANY field, not
	// just the ones in this PUT — matching the store's JSON_MERGE_PATCH semantics. When the
	// before-image read failed, fall back to the changed fields alone (best-effort; conditions
	// on unchanged fields simply see them absent). Async + fail-open.
	after := mergeAfterImage(before, clean)
	s.engine.Emit(ctx, workflow.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Trigger:     workflow.TriggerAfterUpdate,
		Data:        after,
		// Before is the committed field map as it stood BEFORE this write. Without it no rule can
		// fire on a CHANGE — only on "is currently in this state" — so "when a quote becomes
		// Accepted" was inexpressible and the closest a builder could get was "whenever a quote is
		// saved AND is Accepted", which re-fires on every later edit.
		Before:      before,
		OwnerUserID: ownerUserID,
		// Depth was missing here. Harmless while nothing re-enters the dispatcher via an update —
		// but the moment any action writes another record, a zero depth on every hop means the
		// recursion guard counts to one forever. Carried now so the guard is correct BEFORE
		// anything relies on it, not after.
		Depth:     workflow.ChainDepthFromContext(ctx),
		CreatedAt: createdAt,
	})
	// EPIC 5: arm BPM approval processes on the committed update (after_update). Async +
	// fail-open. The engine's idempotency check (HasPendingInstance) keeps a record that is
	// already under approval from being double-armed by a subsequent edit.
	s.approvals.Emit(ctx, bpm.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Trigger:     bpm.TriggerAfterUpdate,
		Data:        after,
		// Before + Depth, exactly as the workflow event carries them (specs/043 S2b): without the
		// before-image a process could not start on "status JUST became Chờ duyệt" — only on "is
		// Chờ duyệt", which re-arms on every later save once the first approval has ended.
		Before: before,
		Depth:  workflow.ChainDepthFromContext(ctx),
	})
	return nil
}

// asVersionConflict maps the repo's optimistic-lock sentinel to a coded ValidationError so the
// handler returns 409 RECORD_VERSION_CONFLICT with details.currentVersion (RM-24). The FE reads
// currentVersion to reconcile a stale edit. Other errors pass through unchanged.
func asVersionConflict(err error) error {
	var vc *dynrepo.VersionConflictError
	if !errors.As(err, &vc) {
		return err
	}
	ve := &ValidationError{
		Code:    "RECORD_VERSION_CONFLICT",
		Msg:     "bản ghi đã bị sửa bởi người khác (version không khớp); tải lại và thử lại",
		Details: map[string]any{"currentVersion": vc.Current},
	}
	return ve
}

// validateRelations enforces the 1-N relation FK rule (M3-T2): for every `relation` field
// whose value is present in the cleaned payload, the value must be a live record id in the
// field's target entity (options.relatedEntity) under the SAME tenant. A value that points
// nowhere — a non-existent id or one owned by another tenant (BatchRecordExists is
// tenant-scoped, so cross-tenant ids report absent) — is rejected RECORD_RELATION_INVALID
// (422). Omitted relation values are skipped (validated only when present).
//
// RM-11: instead of N individual RecordExists probes (one per relation field), this method
// groups the ids to check BY target entity and issues ONE BatchRecordExists query per unique
// target entity, then checks membership client-side. For a typical record with 8 relation
// fields pointing to different target entities, this reduces 8 sequential roundtrips to ≤8
// parallel-able queries (one per unique target entity, often fewer). Relation values are
// strings (enforced by checkType).
// relationExister is the minimal store slice for relation-FK existence probing (one batched query
// per target entity). Satisfied by the record service's RecordStore and *dynrepo.Repository.
// Declared consumer-side so BOTH the HTTP write-path (validateRelations) and the workflow
// field_update mutator share ONE relation validator — an automation must not merge a dangling or
// cross-tenant FK that a manual PUT would reject (review 2026-07-03).
type relationExister interface {
	BatchRecordExists(ctx context.Context, entity, tenantID string, ids []string) (map[string]bool, error)
}

// scopedRelationExister is the row-scope-aware probe: exists AND visible to `scope` (queue #331).
// Satisfied by *dynrepo.Repository. Optional on purpose (type-asserted, like userExister) so the
// many RecordStore test fakes need not grow a method; production is pinned by a compile-time
// assertion in record_relation_view_scope_test.go.
type scopedRelationExister interface {
	BatchRecordExistsInScope(ctx context.Context, entity, tenantID string, ids []string, scope dynrepo.RowScope) (map[string]bool, error)
}

// checkRelationTargets verifies every relation-FK present in `clean` points to a live record in the
// SAME tenant, returning RECORD_RELATION_INVALID on the first dangling/cross-tenant reference. One
// BatchRecordExists per unique target entity (RM-11); tenant-scoped so a foreign id never resolves.
func checkRelationTargets(ctx context.Context, store relationExister, tenantID string, specs []FieldSpec, clean map[string]any, scopeFor relationScopeFn) error {
	// Step 1: collect (fieldName, targetEntityName, id) triples from the payload.
	type relCheck struct {
		fieldName  string
		targetName string
		id         string
	}
	var checks []relCheck

	for _, f := range specs {
		if f.FieldType != "relation" {
			continue
		}
		raw, present := clean[f.Name]
		if !present {
			continue
		}
		id, ok := raw.(string)
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		target, ok := relatedEntityName(f.Options)
		if !ok {
			// A relation field with no target should never have been persisted (validation
			// guards it), but fail closed rather than silently accept an unverifiable FK.
			return invalid("RECORD_RELATION_INVALID",
				"field relation %q thiếu entity đích để kiểm tra FK", f.Name)
		}
		targetName, err := ddl.SanitizeIdentifier(target)
		if err != nil {
			return invalid("RECORD_RELATION_INVALID",
				"field relation %q có entity đích không hợp lệ", f.Name)
		}
		checks = append(checks, relCheck{fieldName: f.Name, targetName: targetName, id: id})
	}
	if len(checks) == 0 {
		return nil
	}

	// Step 2: group ids by target entity for batch lookup (RM-11: 1 query/target, not 1/field).
	byTarget := make(map[string][]string) // targetEntityName → []id
	for _, c := range checks {
		byTarget[c.targetName] = append(byTarget[c.targetName], c.id)
	}

	// Step 3: one probe per unique target entity.
	exists, err := probeRelationTargets(ctx, store, tenantID, byTarget, scopeFor)
	if err != nil {
		return fmt.Errorf("services.checkRelationTargets: %w", err)
	}

	// Step 4: check membership client-side; return first violation. An id the writer may not view
	// fails with the SAME sentence as an id that does not exist — no oracle on hidden records.
	for _, c := range checks {
		if !exists[c.targetName][c.id] {
			return invalid("RECORD_RELATION_INVALID",
				"field relation %q trỏ tới record không tồn tại trong entity %q", c.fieldName, c.targetName)
		}
	}
	return nil
}

// relationScopeFn resolves the row scope a user's relation-FK probe on `target` runs under (the
// writer's VIEW access to the TARGET entity). nil = the system probe: tenant-wide existence only.
type relationScopeFn func(ctx context.Context, tenantID, target string) (dynrepo.RowScope, error)

// probeRelationTargets answers, per target entity, which of the ids are live — and, when scopeFor
// is set, visible to the writer. ONE query per unique target (RM-11). Shared by the single-record
// guard, the batch guard and the workflow mutator so the three cannot drift.
func probeRelationTargets(ctx context.Context, store relationExister, tenantID string, byTarget map[string][]string, scopeFor relationScopeFn) (map[string]map[string]bool, error) {
	exists := make(map[string]map[string]bool, len(byTarget)) // targetName → (id → true)
	for target, ids := range byTarget {
		var found map[string]bool
		var err error
		if scopeFor == nil {
			found, err = store.BatchRecordExists(ctx, target, tenantID, ids)
		} else {
			sx, ok := store.(scopedRelationExister)
			if !ok { // unreachable via relationScopeFor; fail closed rather than widen
				return nil, fmt.Errorf("probe %q: store cannot probe in a row scope", target)
			}
			var scope dynrepo.RowScope
			if scope, err = scopeFor(ctx, tenantID, target); err != nil {
				return nil, fmt.Errorf("probe %q: %w", target, err)
			}
			found, err = sx.BatchRecordExistsInScope(ctx, target, tenantID, ids, scope)
		}
		if err != nil {
			return nil, fmt.Errorf("batch probe %q: %w", target, err)
		}
		exists[target] = found
	}
	return exists, nil
}

// relationScopeFor is the scope resolver for a USER's write (POST/PUT/bulk): a relation FK may only
// point at a record the writer may VIEW (queue #331). nil — the tenant-wide system probe — for an
// admin (bypasses ACL everywhere), when no authorizer is wired (unit tests; production always wires
// one), or when the store cannot probe in a scope. The scope is the writer's VIEW grant on the
// TARGET entity, never the create/edit level carried on `scope`; no view grant at all ⇒ AccessNone,
// so every id reads as absent.
func (s *RecordService) relationScopeFor(scope RecordScope) relationScopeFn {
	if scope.IsAdmin || s.authz == nil {
		return nil
	}
	if _, ok := s.store.(scopedRelationExister); !ok {
		return nil
	}
	return func(ctx context.Context, tenantID, target string) (dynrepo.RowScope, error) {
		allowed, level, err := s.authz.Authorize(ctx, tenantID, scope.RoleID, target, "view")
		if err != nil {
			// Fail closed: an ACL fault must never widen what the writer may point at.
			return dynrepo.RowScope{}, fmt.Errorf("services.RecordService.relationScopeFor: authorize %q: %w", target, err)
		}
		if !allowed {
			return dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessNone}, nil
		}
		return dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessLevel(level), IncludeShares: true}, nil
	}
}

// validateRelations is the RecordService write-path relation guard (POST). It delegates to the
// shared checkRelationTargets so the workflow field_update mutator enforces the IDENTICAL contract
// — except that a user's write is probed under their VIEW scope (relationScopeFor).
func (s *RecordService) validateRelations(ctx context.Context, scope RecordScope, tenantID string, specs []FieldSpec, clean map[string]any) error {
	return checkRelationTargets(ctx, s.store, tenantID, specs, clean, s.relationScopeFor(scope))
}

// validateRelationsAgainst is validateRelations for the UPDATE path: it skips any relation field
// whose submitted value is IDENTICAL to what the record already holds. See the call site for why —
// in short, the form resends every field, so re-probing an untouched FK let a deleted target make
// the whole record permanently unsavable. `current` nil (read failed) falls back to the strict
// check, so the degraded path is the safe one.
func (s *RecordService) validateRelationsAgainst(ctx context.Context, scope RecordScope, tenantID string, specs []FieldSpec, clean, current map[string]any) error {
	scopeFor := s.relationScopeFor(scope)
	if len(current) == 0 {
		return checkRelationTargets(ctx, s.store, tenantID, specs, clean, scopeFor)
	}
	changed := make(map[string]any, len(clean))
	for k, v := range clean {
		if cur, ok := current[k]; ok && fmt.Sprint(cur) == fmt.Sprint(v) {
			continue // untouched: not this caller's edit, and not this caller's to repair
		}
		changed[k] = v
	}
	return checkRelationTargets(ctx, s.store, tenantID, specs, changed, scopeFor)
}

// keepLegacyCurrencyValues drops from clean every currency value that would silently RELABEL a
// legacy row (queue #63): the record stores a code different from the field's configured
// options.currencyCode (a row written before the Q7 guard, queue #7), and the payload carries the
// SAME amount under the configured code (the Q7 guard already pinned the payload's code to the
// configured one). That is the edit form's stamp, not the caller's edit — writing it would store
// a different amount of money under the same number (USD 100 → VND 100) with nobody told, which
// Q7 forbids. Dropping the key leaves the stored value verbatim (partial-PUT: an absent key is
// untouched) while the rest of the save goes through, a warning names the row, and the Doctor's
// D2 check (doctor_currency_check.go) reports every such row for a human to fix.
//
// A DIFFERENT amount is the caller's own edit — a number typed into a field labelled with the
// configured code — and is written as sent. `current` nil (the best-effort read failed) means
// nothing to compare against: the payload is written as sent, the behaviour before this rule.
func (s *RecordService) keepLegacyCurrencyValues(tenantID, entity, id string, specs []FieldSpec, clean, current map[string]any) {
	if len(current) == 0 {
		return
	}
	for _, f := range specs {
		if f.FieldType != "currency" {
			continue
		}
		want, _ := f.Options["currencyCode"].(string)
		in, ok := clean[f.Name].(map[string]any)
		if want == "" || !ok {
			continue
		}
		stored, ok := current[f.Name].(map[string]any)
		if !ok {
			continue
		}
		storedCode, _ := stored["currencyCode"].(string)
		storedCode = strings.TrimSpace(storedCode)
		if storedCode == "" || storedCode == want {
			continue
		}
		inAmount, okIn := in["amount"].(float64)
		storedAmount, okStored := stored["amount"].(float64)
		if !okIn || !okStored || inAmount != storedAmount {
			continue
		}
		delete(clean, f.Name)
		if s.logger != nil {
			s.logger.Warn("services.RecordService.Update: kept a legacy currency value instead of relabelling it",
				zap.String("tenant_id", tenantID),
				zap.String("entity_name", entity),
				zap.String("record_id", id),
				zap.String("field", f.Name),
				zap.String("stored_currency_code", storedCode),
				zap.String("configured_currency_code", want),
			)
		}
	}
}

// checkPolymorphicTargets is the ACTIVITY-CORE #3a analogue of checkRelationTargets: for every
// polymorphic_relation field present in `clean` (value already shape-validated to {type,id} by
// checkPolymorphic), it verifies the target record `id` exists as a LIVE row in obj_<type> for
// the SAME tenant, returning RECORD_POLYMORPHIC_TARGET_INVALID on the first dangling/cross-tenant
// reference. It REUSES the tenant-scoped, parameterized relation FK existence probe
// (store.BatchRecordExists → one query per unique target entity, RM-11) — so an id/type owned by
// ANOTHER tenant reports absent (BatchRecordExists filters tenant_id) and is rejected. `type` is
// resolved through ddl.SanitizeIdentifier (never concatenated into SQL); membership of `type` in
// options.targetEntities was already enforced by checkPolymorphic before this runs.
func checkPolymorphicTargets(ctx context.Context, store relationExister, tenantID string, specs []FieldSpec, clean map[string]any, scopeFor relationScopeFn) error {
	type polyCheck struct {
		fieldName  string
		targetName string
		id         string
	}
	var checks []polyCheck

	for _, f := range specs {
		if f.FieldType != "polymorphic_relation" {
			continue
		}
		raw, present := clean[f.Name]
		if !present {
			continue
		}
		m, ok := raw.(map[string]any)
		if !ok {
			continue // shape validated upstream by checkPolymorphic; skip defensively
		}
		typ, _ := m["type"].(string)
		id, _ := m["id"].(string)
		if strings.TrimSpace(typ) == "" || strings.TrimSpace(id) == "" {
			continue
		}
		// Sanitize the entity name before it becomes a table identifier in BatchRecordExists —
		// never trust a client-supplied type into SQL text (defense-in-depth: BatchRecordExists →
		// tableFor also sanitizes). Fail closed on a malformed name.
		targetName, err := ddl.SanitizeIdentifier(typ)
		if err != nil {
			return invalidField("RECORD_POLYMORPHIC_TARGET_INVALID", f.Name+".type",
				"field %q có type %q không hợp lệ", f.Name, typ)
		}
		checks = append(checks, polyCheck{fieldName: f.Name, targetName: targetName, id: id})
	}
	if len(checks) == 0 {
		return nil
	}

	// Group ids by target entity for batch lookup (RM-11: 1 query/target, not 1/field).
	byTarget := make(map[string][]string)
	for _, c := range checks {
		byTarget[c.targetName] = append(byTarget[c.targetName], c.id)
	}
	// A user's write is probed under their VIEW scope on the target (#331 sibling): a record they may
	// not see answers like one that does not exist. nil scopeFor = the system probe (workflow).
	exists, err := probeRelationTargets(ctx, store, tenantID, byTarget, scopeFor)
	if err != nil {
		return fmt.Errorf("services.checkPolymorphicTargets: %w", err)
	}
	for _, c := range checks {
		if !exists[c.targetName][c.id] {
			return invalidField("RECORD_POLYMORPHIC_TARGET_INVALID", c.fieldName+".id",
				"field %q trỏ tới record không tồn tại trong entity %q (cùng tenant)", c.fieldName, c.targetName)
		}
	}
	return nil
}

// validatePolymorphicRelations is the RecordService write-path polymorphic guard (POST/PUT). It
// delegates to the shared checkPolymorphicTargets so the workflow field_update mutator enforces
// the IDENTICAL contract (same as validateRelations for `relation`).
// `current` (PUT only; nil on create) lets an untouched value skip the probe, like
// validateRelationsAgainst: the form resends every field, and a parent the editor can no longer
// see must not make the record unsavable.
func (s *RecordService) validatePolymorphicRelations(ctx context.Context, scope RecordScope, tenantID string, specs []FieldSpec, clean, current map[string]any) error {
	probed := clean
	if len(current) > 0 {
		probed = make(map[string]any, len(clean))
		for k, v := range clean {
			if cur, ok := current[k]; ok && fmt.Sprint(cur) == fmt.Sprint(v) {
				continue
			}
			probed[k] = v
		}
	}
	return checkPolymorphicTargets(ctx, s.store, tenantID, specs, probed, s.relationScopeFor(scope))
}

// userExister is the minimal store slice for `user` field FK existence probing (USER-FIELD
// slice) — the fixed-`users`-table analogue of relationExister (which targets obj_* tables).
// Declared consumer-side (mirrors relationExister) so unit tests can inject a fake without
// implementing every RecordStore method, and so NewRecordService can auto-detect
// *dynrepo.Repository via a type-assertion (it satisfies this the moment
// BatchActiveUserExists exists — see the compile-time guard just below this interface).
type userExister interface {
	// BatchActiveUserExists reports, for a set of ids, which are LIVE ACTIVE users in the given
	// tenant (RM-11-style batching — one query for the whole set). Absent ids (unknown, another
	// tenant's, or disabled) are simply missing from the result map — collapsed into ONE
	// "doesn't count" outcome by the caller, mirroring BatchRecordExists.
	BatchActiveUserExists(ctx context.Context, tenantID string, ids []string) (map[string]bool, error)
}

// Compile-time guarantees that production's *dynrepo.Repository satisfies BOTH new USER-FIELD
// seams (userExister here, userLabelFetcher in record_expand.go). Without these, a typo'd method
// signature on either side would silently fail the runtime type-assertion in NewRecordService /
// applyExpand — no compile error, no test failure — and production would quietly fall back to
// the fail-closed / no-op path (review: advisor-flagged highest risk for this slice).
var (
	_ userExister      = (*dynrepo.Repository)(nil)
	_ userLabelFetcher = (*dynrepo.Repository)(nil)
)

// checkUserTargets verifies every `user`-typed value present in `clean` is the id of an ACTIVE
// user in the SAME tenant (USER-FIELD slice) — the users-table analogue of checkRelationTargets.
// A dangling id, a cross-tenant id, and a disabled user's id are ALL rejected with the SAME
// RECORD_USER_INVALID (422, details.field = the field name) — one code for every "doesn't count"
// reason, so the response never leaks WHICH reason applied (anti-enumeration; mirrors relation's/
// polymorphic's tenant-scoped existence probes).
//
// A nil/empty value (field omitted from the payload, or an explicit null from a partial-PUT
// clear — `raw.(string)` fails on nil, so it is skipped exactly like checkRelationTargets skips
// it) is not checked — "validated only when present", same contract as relation. store==nil
// (userStore not wired — a hand-rolled test fake, never production) FAILS CLOSED with
// RECORD_USER_UNAVAILABLE, but ONLY when there is at least one `user` field value to check: an
// entity with no `user` field, or one whose `user` field is simply omitted from THIS payload, is
// entirely unaffected (mirrors SequenceClaimer's nil-disables-only-when-relevant contract, above).
func checkUserTargets(ctx context.Context, store userExister, tenantID string, specs []FieldSpec, clean map[string]any) error {
	type userCheck struct {
		fieldName string
		id        string
	}
	var checks []userCheck
	for _, f := range specs {
		if f.FieldType != "user" {
			continue
		}
		raw, present := clean[f.Name]
		if !present {
			continue
		}
		id, ok := raw.(string)
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		checks = append(checks, userCheck{fieldName: f.Name, id: id})
	}
	if len(checks) == 0 {
		return nil
	}
	if store == nil {
		return invalid("RECORD_USER_UNAVAILABLE",
			"kiểm tra field kiểu user hiện không khả dụng — vui lòng thử lại sau")
	}

	ids := make([]string, len(checks))
	for i, c := range checks {
		ids[i] = c.id
	}
	found, err := store.BatchActiveUserExists(ctx, tenantID, ids)
	if err != nil {
		return fmt.Errorf("services.checkUserTargets: batch probe: %w", err)
	}
	for _, c := range checks {
		if !found[c.id] {
			return invalidField("RECORD_USER_INVALID", c.fieldName,
				"field %q phải là id của một người dùng đang hoạt động trong tổ chức", c.fieldName)
		}
	}
	return nil
}

// validateUserFields is the RecordService write-path `user` field guard (POST/PUT). It delegates
// to the shared checkUserTargets so the workflow field_update mutator enforces the IDENTICAL
// contract (same pattern as validateRelations/validatePolymorphicRelations above).
func (s *RecordService) validateUserFields(ctx context.Context, tenantID string, specs []FieldSpec, clean map[string]any) error {
	return checkUserTargets(ctx, s.userStore, tenantID, specs, clean)
}

// asDataConstraint maps the repo's column-constraint sentinel (W5-B4: MySQL 1264/1406/
// 1292/1366 on a generated column) to a coded 422 ValidationError so the handler does not
// return an opaque 500. A too-long value → RECORD_TOO_LONG; an out-of-range / wrong value →
// RECORD_INVALID_RANGE. The physical column name parsed from the MySQL message equals the
// field name for unique/indexed fields (or `<field>_id` for a relation), so it is surfaced as
// details.field when present. Other errors pass through unchanged.
func asDataConstraint(err error) error {
	var dce *dynrepo.DataConstraintError
	if !errors.As(err, &dce) {
		return err
	}
	code := "RECORD_INVALID_RANGE"
	msg := "giá trị field vượt giới hạn cho phép của cột"
	if dce.Kind == dynrepo.DataConstraintTooLong {
		code = "RECORD_TOO_LONG"
		msg = "giá trị field vượt độ dài tối đa của cột"
	}
	if dce.Field != "" {
		return invalidField(code, dce.Field, "field %q: %s", dce.Field, msg)
	}
	return invalid(code, "%s", msg)
}

// asDuplicate maps the repo's duplicate-unique sentinel to a coded ValidationError so the
// handler returns 409. entityName is the sanitized entity name (e.g. "accounts") used to
// strip the `uq_obj_<entity>_` prefix from the index name and extract the field name.
// When the index name does not match the Nexus uq_ convention (e.g. PRIMARY or a custom
// constraint), the error is still returned as RECORD_DUPLICATE but without a details.field.
// Other errors pass through unchanged.
//
// ENG-REL2 #5: a 1-1 `relation` field materializes `uq_obj_<entity>_<field>_id` (see
// ddl.BuildAddOneToOneRelationColumn) — the SAME naming convention as an ordinary unique
// field, just on the "<field>_id" column. The two cases are disambiguated by METADATA
// (review MED-1 fix): the extracted column maps to 409 RECORD_RELATION_ALREADY_BOUND
// (details.field = the relation FIELD name, i.e. the column minus "_id") ONLY when `specs`
// actually contains a relation field with cardinality "1-1" owning that column. An ordinary
// unique field — even one literally named "*_id", e.g. "customer_id" — keeps the pre-#5
// RECORD_DUPLICATE contract with its full, unstripped name. Zero extra queries: both call
// sites (Create/Update) already hold the entity's resolved specs.
func asDuplicate(err error, entityName string, specs []FieldSpec) error {
	var dup *dynrepo.DuplicateUniqueError
	if !errors.As(err, &dup) {
		return err
	}
	// RM-28: a pivot (sys_unique_index) 1062 knows the field DIRECTLY (the INSERT is field-granular)
	// and sets dup.Field — prefer it over parsing an index name (the pivot index is the constant
	// "uq_pivot", which carries no field). The obj_ unique index path (dup.Field == "") still parses
	// the uq_obj_<entity>_<field> convention as before.
	field := dup.Field
	if field == "" {
		field = extractDuplicateField(dup.IndexName, entityName)
	}
	if field == "" {
		return invalid("RECORD_DUPLICATE", "giá trị field unique đã tồn tại")
	}
	if relField, ok := oneToOneFieldForColumn(specs, field); ok {
		return invalidField("RECORD_RELATION_ALREADY_BOUND", relField,
			"quan hệ 1-1 %q đã được gán cho một bản ghi khác", relField)
	}
	return invalidField("RECORD_DUPLICATE", field, "field %q đã tồn tại với giá trị này", field)
}

// oneToOneFieldForColumn maps a duplicate-index column name back to the 1-1 relation field
// that owns it: column "<f>_id" ↔ a relation-type spec named <f> with options.cardinality
// "1-1". Reports false for anything else — including a 1-N relation's column (its index is
// non-unique, so a duplicate there is a same-named unique field, not a cardinality conflict)
// and an ordinary unique field whose own name happens to end in "_id".
func oneToOneFieldForColumn(specs []FieldSpec, column string) (string, bool) {
	if !strings.HasSuffix(column, "_id") {
		return "", false
	}
	name := strings.TrimSuffix(column, "_id")
	for _, sp := range specs {
		if sp.Name == name && sp.FieldType == "relation" && relationCardinality(sp.Options) == "1-1" {
			return name, true
		}
	}
	return "", false
}

// extractDuplicateField parses the Nexus unique index naming convention:
//
//	uq_obj_<entity>_<field>
//
// and returns the field name. Returns "" when the index name does not match the convention
// or when entity is empty (fallback-safe: the caller uses generic duplicate error).
func extractDuplicateField(indexName, entityName string) string {
	if entityName == "" || indexName == "" {
		return ""
	}
	// RM-28 backstop: the pivot's constant index name "uq_pivot" encodes no field. It should never
	// reach here (a pivot 1062 sets DuplicateUniqueError.Field, which asDuplicate prefers), but guard
	// it explicitly so a stray pivot error degrades to the generic RECORD_DUPLICATE rather than
	// mis-parsing "uq_pivot" as a field name.
	if indexName == "uq_pivot" {
		return ""
	}
	// Expected prefix: "uq_obj_<entity>_"
	prefix := "uq_obj_" + entityName + "_"
	if !strings.HasPrefix(indexName, prefix) {
		return ""
	}
	field := indexName[len(prefix):]
	if field == "" {
		return ""
	}
	return field
}

// Delete soft-deletes a record. scope enforces A5 row-level access: a record the caller may not
// see is not deleted and surfaces as ErrRecordNotFound.
func (s *RecordService) Delete(ctx context.Context, scope RecordScope, tenantID, entityName, id string) error {
	// specs (un-discarded): the audit diff below must mask every field flagged
	// options.sensitive before the before-image is copied into the append-only trail.
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return err
	}
	// Capture the before-image (best-effort, scoped) for a delete = before-only audit diff.
	var before map[string]any
	var ownerUserID string
	rec, readErr := s.store.GetRecord(ctx, e.Name, tenantID, id, scope.toRowScope())
	found := readErr == nil && rec != nil
	if found {
		before = rec.Data
		// Record's immutable owner — same "$owner" resolution source as Update (M7 MED-1 fix).
		// EFFECTIVE owner (owner_id when set, else created_by) — the same value ACL enforces on.
		// Was rec.CreatedBy: after "Đổi chủ" a "$owner" notification still went to the CREATOR,
		// and a create_record action still stamped the creator as the new row's owner.
		ownerUserID = rec.OwnerID
	}
	// specs/048 S3: the before_delete rules judge the record as stored, before the one store call —
	// a refused delete removes nothing, audits nothing and fires no after_delete.
	if err := s.checkBeforeDelete(ctx, scope, tenantID, e.Name, id, before, found, readErr); err != nil {
		return err
	}
	if derr := s.store.SoftDeleteRecord(ctx, e.Name, tenantID, id, scope.toRowScope()); derr != nil {
		return s.sharedReadOnlyOr(ctx, scope, tenantID, e.Name, id, derr) // specs/051: shared-only → 403
	}
	s.refreshStoredRollups(ctx, tenantID, e.Name, before) // specs/083: the parent total no longer counts it
	// RM-19: audit the committed delete (before-only diff). Async + fail-open.
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Action:      audit.ActionDelete,
		Diff:        redactSensitiveDiff(s.redactionSpecs(ctx, tenantID, e.ID, specs), audit.ComputeDiff(before, nil)),
		At:          time.Now().UTC(),
	})
	// EPIC 4: fire after_delete workflows. The event carries the before-image (the record as it
	// was just before deletion) so a rule's condition can inspect the deleted record's fields.
	// field_update is a no-op on a delete (the record is gone); in_app_notification still fires.
	// Async + fail-open.
	s.engine.Emit(ctx, workflow.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Trigger:     workflow.TriggerAfterDelete,
		Data:        cloneData(before),
		OwnerUserID: ownerUserID,
		// Same reason as the update emit: the recursion guard must be correct on every hop, not
		// only the one that happens to recurse today.
		Depth: workflow.ChainDepthFromContext(ctx),
	})
	return nil
}

// Restore un-soft-deletes a record (W9b recycle bin), the inverse of Delete. scope enforces A5
// row-level access IDENTICALLY to Delete: a record the caller may not see is left untouched and
// surfaces as ErrRecordNotFound. It is authorized with the SAME "delete" grant as Delete (the
// route sits behind RequireACLAction("delete"), so the caller has proven delete access on the
// entity before this runs).
//
// Traps handled here (W9b):
//   - a record that is NOT soft-deleted / does not exist / is cross-tenant / out-of-scope →
//     ErrRecordNotFound (404), because RestoreRecord's WHERE requires deleted_at IS NOT NULL.
//   - a unique value that now collides with a LIVE row created after the delete → the store
//     returns *DuplicateUniqueError, mapped here (via asDuplicate) to 409 RECORD_DUPLICATE with
//     details.field — exactly like a create/update collision.
//
// Scope note (documented, not a bug): restore is record-level, NOT cascade. A restored record's
// relation fields may point at parents that are themselves still soft-deleted; that dangling
// reference is accepted — restoring a whole object graph is out of scope for a single restore.
func (s *RecordService) Restore(ctx context.Context, scope RecordScope, tenantID, entityName, id string) error {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return err
	}
	if rerr := s.store.RestoreRecord(ctx, e.Name, tenantID, id, scope.UserID, scope.toRowScope(), uniqueFieldSpecs(specs)); rerr != nil {
		// asDuplicate maps a *DuplicateUniqueError to 409 RECORD_DUPLICATE (details.field); any
		// other error (incl. ErrRecordNotFound) passes through unchanged for the handler to map.
		return asDuplicate(rerr, e.Name, specs)
	}
	// RM-19: audit the committed restore. The record is now live, so read it back (best-effort,
	// scoped) to record an after-only diff — mirroring create's ComputeDiff(nil, data). A read
	// failure is fail-open (the restore already committed): we still emit a diff-less restore
	// event so the action is attributable. Async + fail-open.
	var restored map[string]any
	if rec, gerr := s.store.GetRecord(ctx, e.Name, tenantID, id, scope.toRowScope()); gerr == nil && rec != nil {
		restored = rec.Data
	}
	s.refreshStoredRollups(ctx, tenantID, e.Name, restored) // specs/083: the parent total counts it again
	s.auditor.Record(ctx, audit.Event{
		TenantID:    tenantID,
		ActorUserID: scope.UserID,
		EntityName:  e.Name,
		RecordID:    id,
		Action:      audit.ActionRestore,
		Diff:        redactSensitiveDiff(s.redactionSpecs(ctx, tenantID, e.ID, specs), audit.ComputeDiff(nil, restored)),
		At:          time.Now().UTC(),
	})
	return nil
}

// cloneData returns a shallow copy of a record data map so the async workflow engine reads a
// stable snapshot even if the caller reuses/mutates the original after Emit returns. Values are
// only ever read (condition comparison / field_update operand), so a shallow copy suffices; a nil
// input yields a nil map (the engine treats it as "no data").
func cloneData(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// mergeAfterImage builds the post-update record image for workflow evaluation: the prior record
// (before) overlaid with the changed fields (clean), matching the store's JSON_MERGE_PATCH. An
// explicit nil in clean (a field cleared via partial-PUT) overwrites the prior value with nil so
// a condition sees the field as absent/null. When before is nil (the best-effort read failed), it
// returns a clone of clean alone.
func mergeAfterImage(before, clean map[string]any) map[string]any {
	out := cloneData(before)
	if out == nil {
		out = make(map[string]any, len(clean))
	}
	for k, v := range clean {
		out[k] = v
	}
	return out
}

// FieldSpecsOf is the exported wrapper around toFieldSpecs — the SAME conversion the record
// engine's resolve() runs before every ValidateRecord — for callers outside the package that
// need record-payload validation with parity to POST /:entity (the AI Builder's
// propose_records, EASE Nước 7). No second conversion body exists to drift.
func FieldSpecsOf(fields []metadata.FieldDefinition) ([]FieldSpec, error) {
	return toFieldSpecs(fields)
}

// toFieldSpecs converts persisted field definitions into validation specs, parsing the
// JSON options column (picklist values, etc.) back into a map.
func toFieldSpecs(fields []metadata.FieldDefinition) ([]FieldSpec, error) {
	specs := make([]FieldSpec, len(fields))
	for i, f := range fields {
		var opts map[string]any
		if f.Options != nil && *f.Options != "" {
			if err := json.Unmarshal([]byte(*f.Options), &opts); err != nil {
				return nil, fmt.Errorf("services.toFieldSpecs: field %q options: %w", f.Name, err)
			}
		}
		specs[i] = FieldSpec{
			Name:       f.Name,
			Label:      f.Label,
			FieldType:  f.FieldType,
			IsRequired: f.IsRequired,
			IsUnique:   f.IsUnique,
			IsIndexed:  boolOpt(opts, "indexed"),
			DefaultVal: f.DefaultVal,
			Options:    opts,
		}
	}
	return specs, nil
}
