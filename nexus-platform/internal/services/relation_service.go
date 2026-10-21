package services

import (
	"context"
	"errors"
	"fmt"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// mysqlDuplicateErrNo is MySQL ER_DUP_ENTRY (unique-key violation).
const mysqlRelDuplicateErrNo = uint16(1062)

// isRelDuplicate returns true when err (possibly wrapped several layers deep) is a MySQL
// 1062 unique-key violation. Mirrors the pattern in dynrepo/record.go isDuplicate.
func isRelDuplicate(err error) bool {
	var me *gomysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlRelDuplicateErrNo
}

// ErrRelationshipNotFound is the sentinel for an unknown N-N relationship name.
var ErrRelationshipNotFound = errors.New("relationship not found")

// JoinRepo handles DDL and DML for N-N join tables (rel_<a>_<b>).
// Implemented by *dynrepo.Repository; declared here (consumer-side) for testability.
type JoinRepo interface {
	CreateJoinTable(ctx context.Context, entityA, entityB string) error
	JoinTableExists(ctx context.Context, entityA, entityB string) (bool, error)
	DropJoinTable(ctx context.Context, entityA, entityB string) error
	LinkRecords(ctx context.Context, tenantID, userID, sourceEntity, sourceID, targetEntity, targetID string) (bool, error)
	UnlinkRecords(ctx context.Context, tenantID, sourceEntity, sourceID, targetEntity, targetID string) error
	// ListLinked returns target IDs and total count visible to scope. scope is the caller's
	// resolved A5 row-level access (owner/team/all/none) applied to the TARGET entity's obj_*
	// table so the list never exposes IDs the caller cannot see via a direct GET.
	ListLinked(ctx context.Context, tenantID, sourceEntity, sourceID, targetEntity string, scope dynrepo.RowScope, page, limit int) ([]string, int, error)
}

// RelationMetaRepo persists and reads N-N relationship definitions from sys_relationships.
// Implemented by *metadata.Repository; declared here for testability.
type RelationMetaRepo interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
	CreateRelationship(ctx context.Context, rel *metadata.RelationshipDefinition) error
	GetRelationshipByName(ctx context.Context, tenantID, name string) (*metadata.RelationshipDefinition, error)
}

// TargetChecker validates that a record exists and belongs to the same tenant.
// Used for both source and target existence checks in Link.
// Implemented by *dynrepo.Repository; declared here for testability.
type TargetChecker interface {
	RecordExists(ctx context.Context, entity, tenantID, id string) (bool, error)
}

// TargetACLChecker reports whether a principal (identified by tenantID and roleID) may
// perform "view" on a given entity. Used in Link to gate existence-oracle attacks:
// if the caller cannot view the target entity at all, we must not reveal whether a target
// record exists by returning RECORD_RELATION_INVALID vs FORBIDDEN.
// The real implementation is *services.ACLService (Authorize with action="view").
// A nil checker disables the ACL-on-target check (for tests that don't wire ACL).
type TargetACLChecker interface {
	// CanViewEntity reports whether the principal identified by (tenantID, roleID) may view
	// the named entity. IsAdmin=true principals always return (true, nil).
	CanViewEntity(ctx context.Context, tenantID, roleID, entityName string) (bool, error)
}

// RecordFetcher fetches a single full record by id for the expand path.
// Implemented by *dynrepo.Repository; declared here for testability.
// The caller supplies its resolved RowScope so row-level security (owner/team/all) is
// enforced the same way it is on a direct GET — the expand path is not a privilege escalation.
type RecordFetcher interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// RelationService orchestrates N-N relationship registration, link, unlink, and listing.
type RelationService struct {
	joinRepo RelationMetaRepo
	dynRepo  JoinRepo
	// authz resolves the target-entity view permission for the caller (NN-01). Optional: nil
	// preserves prior behaviour (fall back to q.Scope.toRowScope()) so existing tests that do
	// not wire an authorizer keep working — matching the nil-authz convention in RecordService.
	authz entityAuthorizer
	// fieldACL strips A6-hidden fields from expanded records in ListLinked (FU-1 fix).
	// Optional: nil leaves records unstripped (backward-compat for unit tests that do not wire
	// field-ACL). Production wires *FieldACLService via WithRelationFieldACL.
	fieldACL fieldACLEnforcer
	logger   *zap.Logger
}

// NewRelationService wires the relation service.
func NewRelationService(dynRepo JoinRepo, joinRepo RelationMetaRepo, opts ...func(*RelationService)) *RelationService {
	s := &RelationService{
		dynRepo:  dynRepo,
		joinRepo: joinRepo,
		logger:   zap.NewNop(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// WithRelationLogger injects a real logger.
func WithRelationLogger(l *zap.Logger) func(*RelationService) {
	return func(s *RelationService) { s.logger = l }
}

// WithRelationACLAuthorizer injects the entity-ACL authorizer used to derive the TARGET
// entity's row-scope in ListLinked (NN-01 fix). Production wires *ACLService; nil leaves
// ListLinked falling back to the caller's source-entity scope (prior behaviour — kept only
// for unit tests that do not wire ACL, matching the nil-authz convention in RecordService).
func (s *RelationService) WithRelationACLAuthorizer(a entityAuthorizer) *RelationService {
	s.authz = a
	return s
}

// WithRelationFieldACL injects the field-level-security enforcer (A6) so the ?expand path
// in ListLinked strips hidden fields from each expanded record before returning — closing the
// FU-1 gap where the expand shortcut bypassed A6 while a direct GET honoured it.
// Chainable; nil is ignored (nil = no strip, backward-compat for tests). Production wires
// *FieldACLService after WithRelationACLAuthorizer.
func (s *RelationService) WithRelationFieldACL(f fieldACLEnforcer) *RelationService {
	if f != nil {
		s.fieldACL = f
	}
	return s
}

// sourceEditScope resolves the row scope the SOURCE record of a link/unlink must be read under:
// the caller's own EDIT grant on the source entity — a link is a write to that record, so the
// question is "may this person change THIS row", never the scope the route stage happened to hand
// down (specs/060 #351: a create-mapped route handed create:all to a role holding edit:own, and
// links landed on a colleague's record). Same two-tier shape as files.Service.Upload: the route is
// pinned to "edit" AND the service re-resolves it, so the service stays correct wherever it is
// mounted. ok=false = no edit grant at all; the caller refuses it exactly like a missing record, so
// the answer never separates "absent" from "not yours" (no existence oracle).
// Admins and unwired-authz test harnesses keep the scope they arrived with.
func (s *RelationService) sourceEditScope(ctx context.Context, tenantID, userID, roleID string, isAdmin bool, sourceEntity string, handed dynrepo.RowScope) (scope dynrepo.RowScope, ok bool, err error) {
	if isAdmin || s.authz == nil {
		return handed, true, nil
	}
	allowed, level, err := s.authz.Authorize(ctx, tenantID, roleID, sourceEntity, "edit")
	if err != nil {
		return dynrepo.RowScope{}, false, fmt.Errorf("services.sourceEditScope: authorize %q: %w", sourceEntity, err)
	}
	if !allowed {
		return dynrepo.RowScope{}, false, nil
	}
	return dynrepo.RowScope{UserID: userID, Level: dynrepo.AccessLevel(level)}, true, nil
}

// gateSource is the source-record gate of Link and Unlink: both change THAT record, so it is read under
// the caller's EDIT scope (sourceEditScope, #351). A miss gets the answer every record write gets
// (explainWriteMiss, #43): 403 when the caller can read the record (RECORD_SHARED_READ_ONLY through a
// share, FORBIDDEN through their view scope); otherwise the RECORD_RELATION_INVALID an absent record
// gets, so a record they cannot see stays indistinguishable from one that does not exist (NN-02).
func (s *RelationService) gateSource(ctx context.Context, tenantID, userID, roleID string, isAdmin bool, sourceEntity, sourceID string, fetcher RecordFetcher, handed dynrepo.RowScope) error {
	absent := invalid("RECORD_RELATION_INVALID", "bản ghi nguồn %q không tồn tại hoặc thuộc tenant khác", sourceID)
	srcScope, ok, err := s.sourceEditScope(ctx, tenantID, userID, roleID, isAdmin, sourceEntity, handed)
	if err != nil {
		return err
	}
	if !ok {
		return absent
	}
	_, err = fetcher.GetRecord(ctx, sourceEntity, tenantID, sourceID, srcScope)
	if err == nil {
		return nil
	}
	write := RecordScope{UserID: srcScope.UserID, AccessLevel: string(srcScope.Level), IsAdmin: srcScope.IsAdmin, RoleID: roleID}
	switch err = explainWriteMiss(ctx, fetcher, s.authz, write, tenantID, sourceEntity, sourceID, err); {
	case errors.Is(err, ErrRecordNotFound):
		return absent
	case errors.Is(err, ErrRecordSharedReadOnly), errors.Is(err, ErrRecordEditForbidden):
		return err
	}
	return fmt.Errorf("services.RelationService.gateSource: %w", err)
}

// RegisterRelationshipInput is the payload for registering a new N-N relationship.
type RegisterRelationshipInput struct {
	Name    string // unique identifier for this relationship (e.g. "accounts_contacts")
	EntityA string // one side
	EntityB string // other side
}

// RegisterRelationship creates the join table (DDL) then persists the metadata row.
// If metadata persistence fails after DDL auto-commits, the join table is dropped
// (compensation pattern, same as entity_orchestration.go).
//
// Multi-tenancy: tenant_id is derived from the resolved entities, never from client input.
// Self-referential pairs and invalid entity names are rejected before any DDL.
func (s *RelationService) RegisterRelationship(
	ctx context.Context,
	tenantID string,
	input RegisterRelationshipInput,
) (*metadata.RelationshipDefinition, error) {
	// Validate: both entities must exist in this tenant.
	if _, err := s.joinRepo.GetEntityByName(ctx, tenantID, input.EntityA); err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, &ValidationError{Code: "ENTITY_NOT_FOUND",
				Msg: fmt.Sprintf("entity %q không tồn tại", input.EntityA)}
		}
		return nil, fmt.Errorf("services.RegisterRelationship: lookup entityA: %w", err)
	}
	if _, err := s.joinRepo.GetEntityByName(ctx, tenantID, input.EntityB); err != nil {
		if errors.Is(err, metadata.ErrEntityNotFound) {
			return nil, &ValidationError{Code: "ENTITY_NOT_FOUND",
				Msg: fmt.Sprintf("entity %q không tồn tại", input.EntityB)}
		}
		return nil, fmt.Errorf("services.RegisterRelationship: lookup entityB: %w", err)
	}

	// Derive canonical join-table name (validates names + rejects self-referential).
	joinTableName, _, _, err := ddl.JoinParts(input.EntityA, input.EntityB)
	if err != nil {
		return nil, &ValidationError{Code: "RELATION_INVALID_ENTITIES", Msg: err.Error()}
	}

	// Check whether the join table already exists so compensation only drops what WE created.
	existed, err := s.dynRepo.JoinTableExists(ctx, input.EntityA, input.EntityB)
	if err != nil {
		return nil, fmt.Errorf("services.RegisterRelationship: JoinTableExists: %w", err)
	}

	// DDL: create join table (idempotent IF NOT EXISTS). DDL auto-commits in MySQL.
	if err := s.dynRepo.CreateJoinTable(ctx, input.EntityA, input.EntityB); err != nil {
		return nil, fmt.Errorf("services.RegisterRelationship: CreateJoinTable: %w", err)
	}

	// Metadata write. If it fails, compensate by dropping the table we just created
	// (only if it did NOT pre-exist, mirroring entity_orchestration compensation).
	rel := &metadata.RelationshipDefinition{
		ID:               uuid.New().String(),
		TenantID:         tenantID,
		Name:             input.Name,
		ParentEntityName: input.EntityA,
		ChildEntityName:  input.EntityB,
		RelationshipType: "N-N",
		JoinTableName:    &joinTableName,
	}
	if err := s.joinRepo.CreateRelationship(ctx, rel); err != nil {
		// Fix 2+3: UNIQUE(tenant_id, join_table_name) and UNIQUE(tenant_id, name) from
		// migration 000008 surface here as MySQL 1062. Translate to a clean validation error
		// so the handler can return 409 instead of a generic 500. We don't compensate-drop
		// on a dup because the join table already existed (or we just created it for a
		// different relationship row — the caller can reuse it under the same name).
		if isRelDuplicate(err) {
			return nil, &ValidationError{
				Code: "RELATIONSHIP_ALREADY_EXISTS",
				Msg:  fmt.Sprintf("quan hệ %q hoặc cặp entity này đã được đăng ký cho tenant này", input.Name),
			}
		}
		if !existed {
			if dropErr := s.dynRepo.DropJoinTable(ctx, input.EntityA, input.EntityB); dropErr != nil {
				s.logger.Error("services.RegisterRelationship: compensation drop failed",
					zap.String("joinTable", joinTableName),
					zap.Error(dropErr))
			}
		}
		return nil, fmt.Errorf("services.RegisterRelationship: CreateRelationship: %w", err)
	}

	s.logger.Info("N-N relationship registered",
		zap.String("tenant_id", tenantID),
		zap.String("name", rel.Name),
		zap.String("joinTable", joinTableName))
	return rel, nil
}

// Link creates a join-table row linking sourceID (in sourceEntity) to targetID.
// Returns (true, nil) on a fresh link, (false, nil) if already linked (idempotent).
// Returns RECORD_RELATION_INVALID (422) if sourceID or targetID does not exist in the
// same tenant (spec §4.2: validate BOTH records), or if sourceID is outside the caller's
// row-scope (NN-02: IDOR fix — source outside scope is indistinguishable from missing).
// A source the caller can read but not edit is 403 instead (gateSource, #43).
// Returns ErrRelationshipNotFound (→ 404) if relName is unknown or not an N-N relation.
//
// aclChecker is optional (nil disables the ACL-on-target check). When provided it must
// report whether the caller may view the target entity; if not, the link is refused with
// FORBIDDEN (fix 5: defence against existence-oracle via link probe).
// roleID is the caller's role (from JWT claim); isAdmin skips the ACL-on-target check.
//
// sourceScope is the caller's resolved A5 row-level scope for the source entity (from the
// RequireACL gate on :entity). The scoped fetch gates IDOR: a source record outside the
// caller's scope is treated as not found — identical to a missing record, so no information
// is leaked about records outside the caller's visibility (NN-02).
// fetcher must be non-nil (it is the same dynRepo used for the target checks).
func (s *RelationService) Link(
	ctx context.Context,
	tenantID, userID, roleID string,
	isAdmin bool,
	sourceEntity, sourceID string,
	relName, targetID string,
	checker TargetChecker,
	aclChecker TargetACLChecker,
	fetcher RecordFetcher,
	sourceScope dynrepo.RowScope,
) (created bool, err error) {
	rel, err := s.joinRepo.GetRelationshipByName(ctx, tenantID, relName)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return false, ErrRelationshipNotFound
		}
		return false, fmt.Errorf("services.Link: GetRelationshipByName: %w", err)
	}
	// ENG-REL2 #4: GetRelationshipByName was broadened (migration 000028) to also resolve 1-N
	// registry rows, which did not exist before this slice. Link/Unlink/ListLinked implement
	// N-N join-table semantics only; a 1-N name must be treated exactly as it was before the
	// broadening (indistinguishable from "unknown relationship") — otherwise otherSide below
	// would silently misapply join-table link/unlink logic to a FK-on-child relation.
	if rel.RelationshipType != "N-N" {
		return false, ErrRelationshipNotFound
	}

	// Derive target entity name from the relationship definition.
	targetEntity, err := otherSide(rel, sourceEntity)
	if err != nil {
		return false, ErrRelationshipNotFound
	}

	// Fix 5: ACL check on target entity BEFORE any record probes, to avoid an
	// existence-oracle (a caller who cannot view the target entity must not be able to infer
	// whether a target record exists by observing RECORD_RELATION_INVALID vs FORBIDDEN).
	// Admins bypass: isAdmin skips the checker (already allowed everything).
	if aclChecker != nil && !isAdmin {
		canView, aclErr := aclChecker.CanViewEntity(ctx, tenantID, roleID, targetEntity)
		if aclErr != nil {
			return false, fmt.Errorf("services.Link: ACL check target: %w", aclErr)
		}
		if !canView {
			return false, &ValidationError{Code: "FORBIDDEN",
				Msg: fmt.Sprintf("không có quyền xem entity %q", targetEntity)}
		}
	}

	// NN-02: Gate the SOURCE record via the caller's resolved row-scope (A5). A source record
	// outside the caller's scope is indistinguishable from a missing record (both return
	// RECORD_RELATION_INVALID) so no information is leaked about records the caller cannot see.
	// This replaces the former tenant-only RecordExists check for the source.
	// Admins carry IsAdmin=true in sourceScope → GetRecord short-circuits to AccessAll.
	// #351: the scope is the caller's EDIT scope on the source (see sourceEditScope), not the one
	// handed down by the route.
	if err := s.gateSource(ctx, tenantID, userID, roleID, isAdmin, sourceEntity, sourceID, fetcher, sourceScope); err != nil {
		return false, fmt.Errorf("services.Link: %w", err)
	}

	// Validate the target record under the caller's ROW scope on the target entity — not merely
	// tenant-wide. The comment this replaces claimed "target scope is already enforced by the
	// CanViewEntity check above", but that check is ENTITY-level: it answers "may you view this
	// kind of record", never "may you view THIS one". A user with view:own on the target could
	// therefore link to a colleague's row — and the link is then visible to them forever after,
	// since ListLinked returns rows through the relationship. Same probe the source already uses
	// (:294), same refusal shape, so no new oracle: unreachable and absent are one answer.
	targetProbeScope := dynrepo.RowScope{UserID: userID, Level: dynrepo.AccessAll, IsAdmin: true}
	if !isAdmin && s.authz != nil {
		allowed, level, authzErr := s.authz.Authorize(ctx, tenantID, roleID, targetEntity, "view")
		if authzErr != nil {
			return false, fmt.Errorf("services.Link: authorize target %q: %w", targetEntity, authzErr)
		}
		if !allowed {
			return false, invalid("RECORD_RELATION_INVALID",
				"bản ghi đích %q không tồn tại hoặc thuộc tenant khác", targetID)
		}
		targetProbeScope = dynrepo.RowScope{UserID: userID, Level: dynrepo.AccessLevel(level)}
	}
	if fetcher != nil {
		if _, tErr := fetcher.GetRecord(ctx, targetEntity, tenantID, targetID, targetProbeScope); tErr != nil {
			if errors.Is(tErr, dynrepo.ErrRecordNotFound) {
				return false, invalid("RECORD_RELATION_INVALID",
					"bản ghi đích %q không tồn tại hoặc thuộc tenant khác", targetID)
			}
			return false, fmt.Errorf("services.Link: GetRecord target: %w", tErr)
		}
	} else {
		// No fetcher (unit-test path): fall back to the tenant-only existence check rather than
		// skipping validation entirely.
		tgtOk, err := checker.RecordExists(ctx, targetEntity, tenantID, targetID)
		if err != nil {
			return false, fmt.Errorf("services.Link: RecordExists target: %w", err)
		}
		if !tgtOk {
			return false, invalid("RECORD_RELATION_INVALID",
				"bản ghi đích %q không tồn tại hoặc thuộc tenant khác", targetID)
		}
	}

	created, err = s.dynRepo.LinkRecords(ctx, tenantID, userID, sourceEntity, sourceID, targetEntity, targetID)
	if err != nil {
		return false, fmt.Errorf("services.Link: LinkRecords: %w", err)
	}
	return created, nil
}

// Unlink removes the join-table row linking sourceID to targetID.
// Returns ErrRelationshipNotFound (→ 404) if relName is unknown.
// Returns ErrRecordNotFound (→ 404) if the specific link does not exist.
// Returns RECORD_RELATION_INVALID (422) if sourceID is outside the caller's EDIT row-scope
// (NN-02: IDOR fix — a caller with edit-on-entity but owner/team scope cannot unlink
// relationships on source records they cannot see, same as the Link gate). A source they can
// read but not edit is 403 instead (gateSource, #43).
//
// sourceScope is the caller's resolved A5 row-level scope for the source entity.
// fetcher must be non-nil (it is the same dynRepo used for DML).
func (s *RelationService) Unlink(
	ctx context.Context,
	tenantID, userID, roleID string,
	isAdmin bool,
	sourceEntity, sourceID string,
	relName, targetID string,
	fetcher RecordFetcher,
	sourceScope dynrepo.RowScope,
) error {
	rel, err := s.joinRepo.GetRelationshipByName(ctx, tenantID, relName)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return ErrRelationshipNotFound
		}
		return fmt.Errorf("services.Unlink: GetRelationshipByName: %w", err)
	}
	// ENG-REL2 #4: see the identical guard in Link — a 1-N registry row must resolve as
	// "not found" here, preserving pre-000028 behaviour byte-for-byte.
	if rel.RelationshipType != "N-N" {
		return ErrRelationshipNotFound
	}

	targetEntity, err := otherSide(rel, sourceEntity)
	if err != nil {
		return ErrRelationshipNotFound
	}

	// NN-02: Gate the SOURCE record via the caller's resolved row-scope (A5). A source record
	// outside the caller's scope is indistinguishable from a missing record so no information
	// is leaked. Previously Unlink had no source existence check at all — this adds one.
	// #351: unlinking is editing the source record, so it is gated by the EDIT scope too.
	if err := s.gateSource(ctx, tenantID, userID, roleID, isAdmin, sourceEntity, sourceID, fetcher, sourceScope); err != nil {
		return fmt.Errorf("services.Unlink: %w", err)
	}

	if err := s.dynRepo.UnlinkRecords(ctx, tenantID, sourceEntity, sourceID, targetEntity, targetID); err != nil {
		return fmt.Errorf("services.Unlink: UnlinkRecords: %w", err)
	}
	return nil
}

// RelListQuery pagination params for listing linked records.
// When Expand is true the service fetches full records for each linked ID and
// populates ListLinkedResult.Records instead of (or in addition to) IDs.
// Scope must carry the caller's resolved A5 row-level access context so that
// the expand GetRecord calls enforce the same row visibility as a direct GET — no
// privilege escalation via the expand path.
type RelListQuery struct {
	Page   int
	Limit  int
	Expand bool        // ?expand=true → include full record payloads
	Scope  RecordScope // caller's resolved row-level scope; used for expand GetRecord calls
}

// ListLinkedResult is the result of listing linked records for a given relationship.
// Records is non-nil only when RelListQuery.Expand is true; IDs is always populated.
type ListLinkedResult struct {
	IDs     []string
	Total   int
	Records []dynrepo.Record // non-nil when Expand=true
}

// ListLinked returns linked record IDs (and optionally full records) for sourceID via relName.
// Returns ErrRelationshipNotFound (→ 404) if relName is unknown or sourceEntity is not one
// of the two sides of the relationship.
//
// NN-01 fix: the scope applied to the TARGET obj_* table is derived from the TARGET entity's
// ACL (not the SOURCE entity's scope carried in q.Scope). Without this, a caller with
// view:all on the source but view:owner on the target would see every linked target record.
// When s.authz is nil (unit tests without ACL wiring) the behaviour falls back to
// q.Scope.toRowScope() — matching the nil-authz convention in RecordService/applyExpand.
//
// When q.Expand is true, fetcher must be non-nil; the service fetches full records for every
// returned ID using targetScope so the expand path enforces the same row visibility as a
// direct GET — no privilege escalation. Missing records (race-deleted between list and fetch,
// or invisible to the caller's scope) are silently skipped.
func (s *RelationService) ListLinked(
	ctx context.Context,
	tenantID string,
	sourceEntity, sourceID string,
	relName string,
	q RelListQuery,
	fetcher RecordFetcher,
) (*ListLinkedResult, error) {
	rel, err := s.joinRepo.GetRelationshipByName(ctx, tenantID, relName)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return nil, ErrRelationshipNotFound
		}
		return nil, fmt.Errorf("services.ListLinked: GetRelationshipByName: %w", err)
	}
	// ENG-REL2 #4: see the identical guard in Link — a 1-N registry row must resolve as
	// "not found" here, preserving pre-000028 behaviour byte-for-byte.
	if rel.RelationshipType != "N-N" {
		return nil, ErrRelationshipNotFound
	}

	targetEntity, err := otherSide(rel, sourceEntity)
	if err != nil {
		return nil, ErrRelationshipNotFound
	}

	// NN-01: Derive the TARGET entity's row-scope from the target's ACL grant, not from the
	// source-entity scope the RequireACL gate resolved (q.Scope). The source entity's scope
	// must not be used to authorise reads on the target's obj_* table.
	//
	// Precedence:
	//   1. Admin caller → target scope = {UserID, AccessAll, IsAdmin:true} (bypass).
	//   2. authz wired  → Authorize(target, "view"):
	//        • denied    → return empty result (not a 403 — the caller's lack of target-entity
	//                       view is not an error, just an empty set from their perspective).
	//        • allowed   → target scope = {UserID, resolved level}.
	//   3. authz nil (unit tests without ACL wiring) → fall back to q.Scope.toRowScope() so
	//      existing tests that rely on q.Scope continue to work unchanged.
	var targetScope dynrepo.RowScope
	switch {
	case q.Scope.IsAdmin:
		// Admin bypasses target ACL; all rows in the tenant are visible.
		targetScope = dynrepo.RowScope{
			UserID:  q.Scope.UserID,
			Level:   dynrepo.AccessAll,
			IsAdmin: true,
		}
	case s.authz != nil:
		allowed, level, authzErr := s.authz.Authorize(ctx, tenantID, q.Scope.RoleID, targetEntity, "view")
		if authzErr != nil {
			return nil, fmt.Errorf("services.ListLinked: authorize target %q: %w", targetEntity, authzErr)
		}
		if !allowed {
			// Caller cannot view the target entity at all → empty result (no rows, no error).
			// Do NOT call dynRepo (which might scan under the wrong scope), do NOT 500.
			return &ListLinkedResult{IDs: []string{}, Total: 0}, nil
		}
		targetScope = dynrepo.RowScope{
			UserID: q.Scope.UserID,
			Level:  dynrepo.AccessLevel(level),
			// specs/051: a VIEW of the related list also shows target rows shared with the caller,
			// the same rows their own list of the target entity shows.
			IncludeShares: q.Scope.IncludeShares,
		}
	default:
		// nil authz (unit-test path) — preserve prior behaviour so existing tests pass.
		// The comment below is intentionally explicit: this matches the nil-authz fallback in
		// RecordService.WithACLAuthorizer (record_orchestration.go:162-165) and applyExpand.
		targetScope = q.Scope.toRowScope()
	}

	// NN-02 on the READ side. Link (:294) and Unlink (:359) both gate the SOURCE record through the
	// caller's row scope; this read did not, and the SQL never touches the source table — it only
	// joins the TARGET (dynrepo/relation.go:293-300), so the target scope cannot stand in for it.
	// The consequence is not new record contents but the ASSOCIATION GRAPH: "these rows I can
	// already read are linked to source X", for an X the caller is 404'd out of, plus an existence
	// oracle on the source id. The reverse direction was already filtered, which is what made the
	// asymmetry a bug rather than a design. Same refusal shape as a plain record read: not-found.
	if fetcher != nil && !q.Scope.IsAdmin {
		if _, srcErr := fetcher.GetRecord(ctx, sourceEntity, tenantID, sourceID, q.Scope.toRowScope()); srcErr != nil {
			if errors.Is(srcErr, dynrepo.ErrRecordNotFound) {
				return nil, ErrRecordNotFound
			}
			return nil, fmt.Errorf("services.ListLinked: GetRecord source: %w", srcErr)
		}
	}

	// Pass the TARGET entity's resolved scope (not q.Scope) to the join query so that IDs
	// returned are only those the caller can see via a direct GET on the target entity — no
	// privilege escalation, and the total count never over-reports.
	ids, total, err := s.dynRepo.ListLinked(ctx, tenantID, sourceEntity, sourceID, targetEntity, targetScope, q.Page, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("services.ListLinked: ListLinked: %w", err)
	}

	result := &ListLinkedResult{IDs: ids, Total: total}

	if q.Expand && fetcher != nil && len(ids) > 0 {
		// Fetch full records for each ID using the TARGET entity's resolved row-scope (same
		// scope used for the list) so the expand path enforces the same visibility as a direct
		// GET — no privilege escalation via the expand shortcut.
		// Records invisible to targetScope return ErrRecordNotFound and are silently skipped
		// (same path as race-deleted records).
		records := make([]dynrepo.Record, 0, len(ids))
		for _, id := range ids {
			rec, ferr := fetcher.GetRecord(ctx, targetEntity, tenantID, id, targetScope)
			if ferr != nil {
				if errors.Is(ferr, dynrepo.ErrRecordNotFound) {
					// Race: record was soft-deleted between list and fetch — skip.
					continue
				}
				return nil, fmt.Errorf("services.ListLinked: expand GetRecord %s: %w", id, ferr)
			}
			// FU-1 fix: strip A6-hidden fields from the expanded record, mirroring what
			// RecordService.Get does via fieldACL.FilterReadFields. Without this strip, a
			// caller with a sys_field_acl hide on a target field could read it via ?expand
			// even though a direct GET would redact it. Row-scope (A5) is already enforced
			// above; this closes the field axis.
			if s.fieldACL != nil {
				if ferr = s.fieldACL.FilterReadFields(ctx, tenantID, q.Scope.RoleID, targetEntity, q.Scope.IsAdmin, rec.Data); ferr != nil {
					return nil, fmt.Errorf("services.ListLinked: expand field-acl %s: %w", id, ferr)
				}
			}
			records = append(records, *rec)
		}
		result.Records = records
	}

	return result, nil
}

// otherSide returns the entity name on the "other" side of the relationship.
// Returns ErrRelationshipNotFound if sourceEntity is neither side of the relationship
// (caller used wrong entity name for the given relName → clean 404 instead of a
// confusing SQL error from a mismatched join-table name).
func otherSide(rel *metadata.RelationshipDefinition, sourceEntity string) (string, error) {
	if rel.ParentEntityName == sourceEntity {
		return rel.ChildEntityName, nil
	}
	if rel.ChildEntityName == sourceEntity {
		return rel.ParentEntityName, nil
	}
	return "", ErrRelationshipNotFound
}
