package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// --- Fakes for RelationService unit tests ---

type fakeJoinRepo struct {
	created   []string // joined "a:b" strings recorded by CreateJoinTable
	createErr error
	existed   bool // JoinTableExists return value
	dropped   []string
	links     map[string]bool // key = "tenantID:srcID:tgtID" — used to track links
	linkErr   error
	unlinkErr error
	listIDs   []string
	listTotal int
	listErr   error
	// lastListScope records the RowScope passed to ListLinked so NN-01 tests can assert
	// the TARGET entity's scope is used, not the source entity's scope.
	lastListScope dynrepo.RowScope
	listCalled    bool
}

func newFakeJoinRepo() *fakeJoinRepo {
	return &fakeJoinRepo{links: make(map[string]bool)}
}

func (f *fakeJoinRepo) CreateJoinTable(_ context.Context, entityA, entityB string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, entityA+":"+entityB)
	return nil
}

func (f *fakeJoinRepo) JoinTableExists(_ context.Context, _, _ string) (bool, error) {
	return f.existed, nil
}

func (f *fakeJoinRepo) DropJoinTable(_ context.Context, _, _ string) error {
	f.dropped = append(f.dropped, "dropped")
	return nil
}

func (f *fakeJoinRepo) LinkRecords(_ context.Context, tenantID, _ string, _, sourceID, _, targetID string) (bool, error) {
	if f.linkErr != nil {
		return false, f.linkErr
	}
	key := tenantID + ":" + sourceID + ":" + targetID
	if f.links[key] {
		return false, nil // already linked
	}
	f.links[key] = true
	return true, nil
}

func (f *fakeJoinRepo) UnlinkRecords(_ context.Context, _ string, _, sourceID, _, targetID string) error {
	if f.unlinkErr != nil {
		return f.unlinkErr
	}
	return nil
}

func (f *fakeJoinRepo) ListLinked(_ context.Context, _ string, _, _ string, _ string, scope dynrepo.RowScope, _, _ int) ([]string, int, error) {
	f.lastListScope = scope
	f.listCalled = true
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listIDs, f.listTotal, nil
}

type fakeRelMeta struct {
	// knownEntities mirrors entity lookup behavior for the relation service.
	knownEntities map[string]bool
	existingRels  map[string]*metadata.RelationshipDefinition // keyed by name
	createRelErr  error
	createdRels   []*metadata.RelationshipDefinition
}

func newFakeRelMeta(entities ...string) *fakeRelMeta {
	m := &fakeRelMeta{
		knownEntities: make(map[string]bool),
		existingRels:  make(map[string]*metadata.RelationshipDefinition),
	}
	for _, e := range entities {
		m.knownEntities[e] = true
	}
	return m
}

func (f *fakeRelMeta) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if f.knownEntities[name] {
		return &metadata.EntityDefinition{Name: name, ID: "eid-" + name}, nil
	}
	return nil, metadata.ErrEntityNotFound
}

func (f *fakeRelMeta) CreateRelationship(_ context.Context, rel *metadata.RelationshipDefinition) error {
	if f.createRelErr != nil {
		return f.createRelErr
	}
	f.createdRels = append(f.createdRels, rel)
	f.existingRels[rel.Name] = rel
	return nil
}

func (f *fakeRelMeta) GetRelationshipByName(_ context.Context, _, name string) (*metadata.RelationshipDefinition, error) {
	if rel, ok := f.existingRels[name]; ok {
		return rel, nil
	}
	return nil, metadata.ErrRelationshipNotFound
}

// fakeRecordChecker allows unit-testing the source/target existence checks in Link.
// It is keyed by "entity:id" so source-not-found and target-not-found can be tested
// independently (fixes the advisor concern: a single bool would pass for the wrong reason).
type fakeRecordChecker struct {
	// existsByKey: "entity:id" → true means the record exists. When the key is absent the
	// fallback is existsDefault. Use existsDefault=true to make all IDs exist unless overridden.
	existsByKey   map[string]bool
	existsDefault bool
	existsErr     error
}

// newCheckerAllExist returns a checker where every (entity, id) pair exists — the common case
// for tests that focus on something other than the existence check.
func newCheckerAllExist() *fakeRecordChecker {
	return &fakeRecordChecker{existsByKey: map[string]bool{}, existsDefault: true}
}

// withMissing marks a specific (entity, id) pair as not existing.
func (f *fakeRecordChecker) withMissing(entity, id string) *fakeRecordChecker {
	f.existsByKey[entity+":"+id] = false
	return f
}

func (f *fakeRecordChecker) RecordExists(_ context.Context, entity, _, id string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	if v, ok := f.existsByKey[entity+":"+id]; ok {
		return v, nil
	}
	return f.existsDefault, nil
}

// fakeACLChecker implements TargetACLChecker for unit tests.
// allowedEntities is the set of entities the caller can view; nil means allow all.
type fakeACLChecker struct {
	allowedEntities map[string]bool // nil = allow everything
	checkErr        error
}

func newACLCheckerAllowAll() *fakeACLChecker {
	return &fakeACLChecker{allowedEntities: nil}
}

func newACLCheckerDenyEntity(entity string) *fakeACLChecker {
	return &fakeACLChecker{allowedEntities: map[string]bool{}} // empty = deny all
}

func (f *fakeACLChecker) CanViewEntity(_ context.Context, _, _, entityName string) (bool, error) {
	if f.checkErr != nil {
		return false, f.checkErr
	}
	if f.allowedEntities == nil {
		return true, nil // allow all
	}
	return f.allowedEntities[entityName], nil
}

// fakeRelationACLAuthorizer implements entityAuthorizer for RelationService unit tests.
// It returns a configurable (allowed, level) for a given entity. This is distinct from
// fakeExpandAuthz (wave5_a2_expand_acl_test.go) which always returns "all" as the level
// and is scoped to the applyExpand path; here we need per-entity level control to test
// that ListLinked uses the TARGET entity's resolved level (not the source entity's level).
type fakeRelationACLAuthorizer struct {
	// grants maps entity name → (allowed, level). Missing entries → denied.
	grants map[string]struct {
		allowed bool
		level   string
	}
	authzErr error
}

func newRelAuthz(entity string, allowed bool, level string) *fakeRelationACLAuthorizer {
	a := &fakeRelationACLAuthorizer{
		grants: make(map[string]struct {
			allowed bool
			level   string
		}),
	}
	a.grants[entity] = struct {
		allowed bool
		level   string
	}{allowed, level}
	return a
}

func (f *fakeRelationACLAuthorizer) Authorize(_ context.Context, _, _, entity, _ string) (bool, string, error) {
	if f.authzErr != nil {
		return false, "", f.authzErr
	}
	if g, ok := f.grants[entity]; ok {
		return g.allowed, g.level, nil
	}
	return false, "", nil // deny unknown entities
}

// fakeRecordFetcher satisfies RecordFetcher for unit-testing expand paths.
// It is scope-aware: AccessNone scope is honored by returning ErrRecordNotFound for every
// record, which lets tests verify that scope threading actually gates visibility.
type fakeRecordFetcher struct {
	// records keyed by id → returned by GetRecord when scope permits.
	records   map[string]dynrepo.Record
	fetchErr  error
	lastScope dynrepo.RowScope // most recent scope passed to GetRecord
}

func newFakeRecordFetcher(recs ...dynrepo.Record) *fakeRecordFetcher {
	f := &fakeRecordFetcher{records: make(map[string]dynrepo.Record)}
	for _, r := range recs {
		f.records[r.ID] = r
	}
	return f
}

func (f *fakeRecordFetcher) GetRecord(_ context.Context, _, _, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	f.lastScope = scope
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	// Mirror dynrepo's fail-closed access: only known positive levels (owner/team/all) or
	// IsAdmin bypass are allowed. Empty string and "none" both deny — same as real GetRecord.
	switch scope.Level {
	case dynrepo.AccessOwner, dynrepo.AccessTeam, dynrepo.AccessAll:
		// permit — fall through
	default:
		if !scope.IsAdmin {
			return nil, dynrepo.ErrRecordNotFound
		}
	}
	if r, ok := f.records[id]; ok {
		return &r, nil
	}
	return nil, dynrepo.ErrRecordNotFound
}

// --- Tests ---

func TestRegisterNNRelationship_HappyPath(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	svc := NewRelationService(joinRepo, relMeta)

	rel, err := svc.RegisterRelationship(context.Background(), "tenant-1", RegisterRelationshipInput{
		Name:    "accounts_contacts",
		EntityA: "accounts",
		EntityB: "contacts",
	})
	if err != nil {
		t.Fatalf("RegisterRelationship: %v", err)
	}
	if rel.RelationshipType != "N-N" {
		t.Errorf("type = %q; want N-N", rel.RelationshipType)
	}
	if rel.JoinTableName == nil || *rel.JoinTableName != "rel_accounts_contacts" {
		t.Errorf("joinTableName = %v; want rel_accounts_contacts", rel.JoinTableName)
	}
	if len(joinRepo.created) != 1 {
		t.Errorf("CreateJoinTable called %d times; want 1", len(joinRepo.created))
	}
	if len(relMeta.createdRels) != 1 {
		t.Errorf("CreateRelationship called %d times; want 1", len(relMeta.createdRels))
	}
	if relMeta.createdRels[0].TenantID != "tenant-1" {
		t.Errorf("TenantID = %q; want tenant-1", relMeta.createdRels[0].TenantID)
	}
}

func TestRegisterNNRelationship_EntityNotFound(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts") // "contacts" not registered
	svc := NewRelationService(joinRepo, relMeta)

	_, err := svc.RegisterRelationship(context.Background(), "tenant-1", RegisterRelationshipInput{
		Name:    "accounts_contacts",
		EntityA: "accounts",
		EntityB: "contacts",
	})
	if err == nil {
		t.Fatal("expected error for missing entity B, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "ENTITY_NOT_FOUND" {
		t.Errorf("code = %q; want ENTITY_NOT_FOUND", ve.Code)
	}
	// No DDL or metadata writes should have happened.
	if len(joinRepo.created) != 0 {
		t.Errorf("CreateJoinTable should not have been called, but was called %d times", len(joinRepo.created))
	}
}

func TestRegisterNNRelationship_MetadataFailCompensates(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.createRelErr = errors.New("db error")
	svc := NewRelationService(joinRepo, relMeta)

	_, err := svc.RegisterRelationship(context.Background(), "tenant-1", RegisterRelationshipInput{
		Name:    "accounts_contacts",
		EntityA: "accounts",
		EntityB: "contacts",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// DDL was executed (join table created) but metadata failed → should compensate.
	if len(joinRepo.dropped) == 0 {
		t.Error("expected DropJoinTable called as compensation, but it was not")
	}
}

func TestRegisterNNRelationship_SelfReferentialRejected(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("contacts")
	svc := NewRelationService(joinRepo, relMeta)

	_, err := svc.RegisterRelationship(context.Background(), "tenant-1", RegisterRelationshipInput{
		Name:    "contacts_self",
		EntityA: "contacts",
		EntityB: "contacts",
	})
	if err == nil {
		t.Fatal("expected error for self-referential N-N, got nil")
	}
}

func TestLinkRecords_HappyPath_Created(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	checker := newCheckerAllExist()
	svc := NewRelationService(joinRepo, relMeta)

	// Provide a fetcher that can see the source record (src-1) under AccessAll scope.
	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	created, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"accounts_contacts", "tgt-1",
		checker, nil, // nil aclChecker disables ACL-on-target in unit tests
		fetcher, sourceScope)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !created {
		t.Error("expected created=true for first link, got false")
	}
}

func TestLinkRecords_Idempotent(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	// Pre-populate link so second call returns already-linked.
	joinRepo.links["tenant-1:src-1:tgt-1"] = true
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	checker := newCheckerAllExist()
	svc := NewRelationService(joinRepo, relMeta)

	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	created, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"accounts_contacts", "tgt-1",
		checker, nil,
		fetcher, sourceScope)
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if created {
		t.Error("expected created=false for idempotent link (already exists), got true")
	}
}

func TestLinkRecords_TargetNotFound_422(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	// Target record does not exist (cross-tenant or simply missing); source DOES exist
	// so the failure is unambiguously about the target, not the source.
	checker := newCheckerAllExist().withMissing("contacts", "bad-target")
	svc := NewRelationService(joinRepo, relMeta)

	// Source record src-1 exists and is visible to the caller.
	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	_, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"accounts_contacts", "bad-target",
		checker, nil,
		fetcher, sourceScope)
	if err == nil {
		t.Fatal("expected error for non-existent target record, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "RECORD_RELATION_INVALID" {
		t.Errorf("code = %q; want RECORD_RELATION_INVALID", ve.Code)
	}
}

func TestLinkRecords_RelationNameNotNN_404(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	// No relationship registered under this name.
	checker := newCheckerAllExist()
	svc := NewRelationService(joinRepo, relMeta)

	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	_, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"nonexistent_rel", "tgt-1",
		checker, nil,
		fetcher, sourceScope)
	if err == nil {
		t.Fatal("expected error for unknown relation name, got nil")
	}
	if !errors.Is(err, ErrRelationshipNotFound) {
		t.Errorf("expected ErrRelationshipNotFound, got %T: %v", err, err)
	}
}

func TestUnlinkRecords_HappyPath(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
	}
	svc := NewRelationService(joinRepo, relMeta)

	// Provide a fetcher that can see the source record under AccessAll scope.
	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	err := svc.Unlink(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"accounts_contacts", "tgt-1",
		fetcher, sourceScope)
	if err != nil {
		t.Fatalf("Unlink: %v", err)
	}
}

// --- ListLinked tests ---

func makeAccountsContactsRel() *metadata.RelationshipDefinition {
	return &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
}

func TestListLinked_IDs_Only(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"id-1", "id-2"}
	joinRepo.listTotal = 2
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: false},
		nil) // fetcher nil — not needed without expand
	if err != nil {
		t.Fatalf("ListLinked: %v", err)
	}
	if len(result.IDs) != 2 {
		t.Errorf("len(IDs) = %d; want 2", len(result.IDs))
	}
	if result.Records != nil {
		t.Errorf("Records should be nil without expand, got %v", result.Records)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d; want 2", result.Total)
	}
}

func TestListLinked_Expand_ReturnsFullRecords(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1", "rec-2"}
	joinRepo.listTotal = 2
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)

	fetcher := newFakeRecordFetcher(
		// NN-02 read-side: ListLinked now probes the SOURCE record under the caller's row scope
		// (Link/Unlink always did). A caller listing the links of a record they cannot see is
		// exactly what that gate refuses, so the source must be visible for these expand tests.
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1", Data: map[string]any{"name": "Source"}},
		dynrepo.Record{ID: "rec-1", TenantID: "tenant-1", Data: map[string]any{"name": "Alice"}},
		dynrepo.Record{ID: "rec-2", TenantID: "tenant-1", Data: map[string]any{"name": "Bob"}},
	)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: true, Scope: RecordScope{AccessLevel: "all"}},
		fetcher)
	if err != nil {
		t.Fatalf("ListLinked expand: %v", err)
	}
	if len(result.Records) != 2 {
		t.Errorf("len(Records) = %d; want 2", len(result.Records))
	}
	if result.Records[0].ID != "rec-1" {
		t.Errorf("Records[0].ID = %q; want rec-1", result.Records[0].ID)
	}
	// Verify scope was actually threaded through to the fetcher.
	if fetcher.lastScope.Level != dynrepo.AccessAll {
		t.Errorf("fetcher.lastScope.Level = %q; want %q (scope not threaded)", fetcher.lastScope.Level, dynrepo.AccessAll)
	}
}

func TestListLinked_Expand_SkipsMissingRecord(t *testing.T) {
	// Race: one record was soft-deleted between list and fetch → skip, not error.
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1", "gone"}
	joinRepo.listTotal = 2
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)

	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"}, // the SOURCE, gated by NN-02 read-side
		dynrepo.Record{ID: "rec-1", TenantID: "tenant-1", Data: map[string]any{}},
		// "gone" is not in the fetcher — GetRecord will return ErrRecordNotFound.
	)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: true, Scope: RecordScope{AccessLevel: "all"}},
		fetcher)
	if err != nil {
		t.Fatalf("ListLinked expand with missing record: %v", err)
	}
	// Only rec-1 should be in Records; "gone" is silently skipped.
	if len(result.Records) != 1 {
		t.Errorf("len(Records) = %d; want 1 (gone skipped)", len(result.Records))
	}
}

// TestListLinked_Expand_ScopeNone_ReturnsEmpty verifies that the caller's scope is
// actually threaded into GetRecord calls. A user with AccessNone must learn NOTHING from this
// endpoint. This guards against regressions that reintroduce AdminScope().
//
// The answer CHANGED with the NN-02 read-side gate, and the new one is stricter: it used to return
// an empty Records list (the expand loop skipped every target it could not read), which quietly
// asserted "this source record exists and simply has no links you can see". Now the SOURCE itself
// is probed first, so a caller with no access gets the same not-found a direct GET would give —
// no existence oracle on the source id, and no association graph.
func TestListLinked_Expand_ScopeNone_IsNotFound(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1", "rec-2"}
	joinRepo.listTotal = 2
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)

	fetcher := newFakeRecordFetcher(
		// NN-02 read-side: ListLinked now probes the SOURCE record under the caller's row scope
		// (Link/Unlink always did). A caller listing the links of a record they cannot see is
		// exactly what that gate refuses, so the source must be visible for these expand tests.
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1", Data: map[string]any{"name": "Source"}},
		dynrepo.Record{ID: "rec-1", TenantID: "tenant-1", Data: map[string]any{"name": "Alice"}},
		dynrepo.Record{ID: "rec-2", TenantID: "tenant-1", Data: map[string]any{"name": "Bob"}},
	)

	// Zero-value Scope → AccessLevel "" → normalizeAccessLevel → AccessNone. The fake honours that
	// the way real GetRecord does: not-found for everything, source included.
	_, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: true, Scope: RecordScope{}},
		fetcher)
	if !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("err = %v; want ErrRecordNotFound — a source the caller cannot see must answer exactly like a missing one", err)
	}
	// Confirm the scope reached the fetcher and was the caller's zero-value scope
	// (empty AccessLevel → fail-closed deny, not silently widened to admin).
	if fetcher.lastScope.Level != dynrepo.AccessLevel("") {
		t.Errorf("fetcher.lastScope.Level = %q; want empty (zero RecordScope should not be promoted)", fetcher.lastScope.Level)
	}
	if fetcher.lastScope.IsAdmin {
		t.Error("fetcher.lastScope.IsAdmin = true; want false (zero RecordScope must not carry admin flag)")
	}
}

func TestListLinked_WrongEntityName_404(t *testing.T) {
	// sourceEntity is not one of the two sides of the relationship → clean 404.
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)

	_, err := svc.ListLinked(context.Background(), "tenant-1",
		"tickets", "src-1", // "tickets" is not accounts or contacts
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20},
		nil)
	if err == nil {
		t.Fatal("expected error for wrong entity name, got nil")
	}
	if !errors.Is(err, ErrRelationshipNotFound) {
		t.Errorf("expected ErrRelationshipNotFound, got %T: %v", err, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NN-01 regression tests: ListLinked must use the TARGET entity's row-scope,
// not the SOURCE entity's scope from q.Scope.
// ─────────────────────────────────────────────────────────────────────────────

// TestNN01_ListLinked_UsesTargetEntityScope verifies that when s.authz is wired,
// ListLinked resolves the TARGET entity's ACL level and passes IT to dynRepo.ListLinked —
// not q.Scope (the source entity's scope). A caller with view:all on the source but
// view:owner on the target must see the target records under "owner" scope, not "all".
func TestNN01_ListLinked_UsesTargetEntityScope(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1"}
	joinRepo.listTotal = 1
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()

	// Authz returns "owner" level for the TARGET entity "contacts".
	authz := newRelAuthz("contacts", true, "owner")
	svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

	// q.Scope has AccessLevel "all" — this is the SOURCE entity's scope.
	// After the fix, "contacts" (target) resolves to "owner" via authz, and
	// that "owner" scope must reach the join repo — not "all".
	_, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{
			Page:  1,
			Limit: 20,
			Scope: RecordScope{UserID: "user-1", AccessLevel: "all", RoleID: "role-1"},
		},
		nil)
	if err != nil {
		t.Fatalf("ListLinked: %v", err)
	}
	// The scope passed to the join repo must be "owner" (target entity's level), not "all" (source's).
	if joinRepo.lastListScope.Level != dynrepo.AccessOwner {
		t.Errorf("lastListScope.Level = %q; want %q (NN-01: target scope must be used, not source scope)",
			joinRepo.lastListScope.Level, dynrepo.AccessOwner)
	}
}

// TestNN01_ListLinked_DeniedTargetEntity_ReturnsEmpty verifies that when the caller has NO view
// grant on the TARGET entity, ListLinked returns an empty result — not the full list. The join
// repo must NOT be called (no point querying if the caller can't view the target at all).
func TestNN01_ListLinked_DeniedTargetEntity_ReturnsEmpty(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1", "rec-2"}
	joinRepo.listTotal = 2
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()

	// Authz denies view on "contacts" (the target entity).
	authz := newRelAuthz("contacts", false, "")
	svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{
			Page:  1,
			Limit: 20,
			Scope: RecordScope{UserID: "user-1", AccessLevel: "all", RoleID: "role-1"},
		},
		nil)
	if err != nil {
		t.Fatalf("ListLinked with denied target: %v", err)
	}
	// Must return empty — caller cannot see any linked target records.
	if len(result.IDs) != 0 {
		t.Errorf("IDs = %v; want empty (denied target entity must yield empty list, not full list)", result.IDs)
	}
	if result.Total != 0 {
		t.Errorf("Total = %d; want 0 (denied target entity must not over-report count)", result.Total)
	}
	// The join repo must NOT have been called — no point querying when target is denied.
	if joinRepo.listCalled {
		t.Error("dynRepo.ListLinked must not be called when target entity is denied (NN-01)")
	}
}

// TestNN01_ListLinked_AdminBypassesTargetACL verifies that an admin caller (IsAdmin=true)
// sees all linked records regardless of the target-entity ACL (admin short-circuits to AccessAll).
func TestNN01_ListLinked_AdminBypassesTargetACL(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1"}
	joinRepo.listTotal = 1
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()

	// Authz would deny contacts for a non-admin — but admin bypasses it.
	authz := newRelAuthz("contacts", false, "")
	svc := NewRelationService(joinRepo, relMeta).WithRelationACLAuthorizer(authz)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{
			Page:  1,
			Limit: 20,
			Scope: RecordScope{UserID: "admin-1", IsAdmin: true}, // admin flag
		},
		nil)
	if err != nil {
		t.Fatalf("ListLinked as admin: %v", err)
	}
	// Admin must see the IDs — scope bypasses target ACL.
	if len(result.IDs) != 1 {
		t.Errorf("IDs = %v; want [rec-1] (admin must bypass target ACL)", result.IDs)
	}
	// Scope passed to join repo must be AccessAll + IsAdmin.
	if joinRepo.lastListScope.Level != dynrepo.AccessAll {
		t.Errorf("lastListScope.Level = %q; want %q (admin must use AccessAll)", joinRepo.lastListScope.Level, dynrepo.AccessAll)
	}
	if !joinRepo.lastListScope.IsAdmin {
		t.Error("lastListScope.IsAdmin = false; want true (admin flag must propagate to join repo)")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// NN-02 regression tests: Link and Unlink must gate the SOURCE record by the
// caller's row-scope (IDOR fix).
// ─────────────────────────────────────────────────────────────────────────────

// TestNN02_Link_SourceOutsideScope_Fails verifies that Link returns RECORD_RELATION_INVALID
// when the source record exists in the tenant but is outside the caller's row-scope
// (i.e. GetRecord returns ErrRecordNotFound for it under that scope). This closes the IDOR
// where a caller could link records they cannot see.
func TestNN02_Link_SourceOutsideScope_Fails(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	checker := newCheckerAllExist() // target and source both "exist" at tenant level
	svc := NewRelationService(joinRepo, relMeta)

	// Source record does NOT appear in the fetcher — simulating ErrRecordNotFound under the
	// caller's narrow scope (e.g. the record exists but was created by someone else and the
	// caller only has view:owner). The fetcher returns ErrRecordNotFound for any unknown id.
	fetcherEmpty := newFakeRecordFetcher() // no records loaded → GetRecord always 404
	ownerScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessOwner}

	_, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-outside-scope",
		"accounts_contacts", "tgt-1",
		checker, nil,
		fetcherEmpty, ownerScope)
	if err == nil {
		t.Fatal("expected error when source is outside caller's scope, got nil (NN-02 IDOR)")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "RECORD_RELATION_INVALID" {
		t.Errorf("code = %q; want RECORD_RELATION_INVALID (source outside scope must be indistinguishable from missing)", ve.Code)
	}
	// The join-table must NOT have been written — no dangling link row.
	if len(joinRepo.links) != 0 {
		t.Errorf("links = %v; join-table must not be written when source is outside scope", joinRepo.links)
	}
}

// TestNN02_Link_SourceInScope_Succeeds verifies that Link succeeds when the source record IS
// within the caller's row-scope (sanity check paired with the failure case above).
func TestNN02_Link_SourceInScope_Succeeds(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	checker := newCheckerAllExist()
	svc := NewRelationService(joinRepo, relMeta)

	// Source record IS in the fetcher — caller can see it under owner scope.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-owned", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"}, // the target is probed under row scope too
	)
	ownerScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessOwner}

	created, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-owned",
		"accounts_contacts", "tgt-1",
		checker, nil,
		fetcher, ownerScope)
	if err != nil {
		t.Fatalf("Link with in-scope source: %v", err)
	}
	if !created {
		t.Error("expected created=true for in-scope source, got false")
	}
}

// TestNN02_Unlink_SourceOutsideScope_Fails verifies that Unlink returns RECORD_RELATION_INVALID
// when the source record is outside the caller's row-scope (IDOR fix). Previously Unlink had
// no source existence check at all — this proves the gate is now in place.
func TestNN02_Unlink_SourceOutsideScope_Fails(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
	}
	svc := NewRelationService(joinRepo, relMeta)

	// Fetcher has no records — source is outside the caller's scope.
	fetcherEmpty := newFakeRecordFetcher()
	ownerScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessOwner}

	err := svc.Unlink(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-outside-scope",
		"accounts_contacts", "tgt-1",
		fetcherEmpty, ownerScope)
	if err == nil {
		t.Fatal("expected error when source is outside caller's scope, got nil (NN-02 IDOR)")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "RECORD_RELATION_INVALID" {
		t.Errorf("code = %q; want RECORD_RELATION_INVALID", ve.Code)
	}
}

// TestNN02_Unlink_SourceInScope_Succeeds verifies that Unlink succeeds when the source
// record IS within the caller's row-scope (sanity check).
func TestNN02_Unlink_SourceInScope_Succeeds(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
	}
	svc := NewRelationService(joinRepo, relMeta)

	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-owned", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"}, // the target is probed under row scope too
	)
	ownerScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessOwner}

	err := svc.Unlink(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-owned",
		"accounts_contacts", "tgt-1",
		fetcher, ownerScope)
	if err != nil {
		t.Fatalf("Unlink with in-scope source: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FU-1 regression tests: ListLinked ?expand must strip A6-hidden fields.
// ─────────────────────────────────────────────────────────────────────────────

// fakeExpandFieldACL is a test-double for fieldACLEnforcer used in the FU-1 expand tests.
// hiddenField is deleted from the record's Data map in FilterReadFields; empty = no-op.
// When isAdmin is true the strip is skipped (mirrors real FieldACLService.FilterReadFields).
type fakeExpandFieldACL struct {
	hiddenField string
}

func (f *fakeExpandFieldACL) FilterReadFields(_ context.Context, _, _, _ string, isAdmin bool, data map[string]any) error {
	if f.hiddenField != "" && !isAdmin {
		delete(data, f.hiddenField)
	}
	return nil
}

func (f *fakeExpandFieldACL) FilterReadFieldsBatch(_ context.Context, _, _, _ string, isAdmin bool, datas []map[string]any) error {
	for _, d := range datas {
		if f.hiddenField != "" && !isAdmin {
			delete(d, f.hiddenField)
		}
	}
	return nil
}

func (f *fakeExpandFieldACL) CheckWriteFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: these fakes have no notion of a
// record's current values, and the update-path rule (an unchanged locked field is a no-op)
// is exercised in the field-ACL tests that DO model them.
func (f *fakeExpandFieldACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return f.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}

func (f *fakeExpandFieldACL) AssertFieldVisible(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}

func (f *fakeExpandFieldACL) AssertFieldsVisible(_ context.Context, _, _, _ string, _ bool, _ []string) error {
	return nil
}

func (f *fakeExpandFieldACL) HiddenFields(_ context.Context, _, _, _ string, _ bool) (map[string]struct{}, error) {
	return nil, nil
}

// TestListLinked_Expand_FieldACL_HidesField verifies that when a fieldACL enforcer is wired
// via WithRelationFieldACL, expanded records have A6-hidden fields stripped before being
// returned. This closes the FU-1 gap where ?expand bypassed field-level security.
func TestListLinked_Expand_FieldACL_HidesField(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1"}
	joinRepo.listTotal = 1
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)
	svc.WithRelationFieldACL(&fakeExpandFieldACL{hiddenField: "secret"})

	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"}, // the SOURCE, gated by NN-02 read-side
		dynrepo.Record{ID: "rec-1", TenantID: "tenant-1", Data: map[string]any{"name": "Alice", "secret": "topsecret"}},
	)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: true, Scope: RecordScope{AccessLevel: "all", RoleID: "role-restricted"}},
		fetcher)
	if err != nil {
		t.Fatalf("ListLinked expand with fieldACL: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("len(Records) = %d; want 1", len(result.Records))
	}
	if _, present := result.Records[0].Data["secret"]; present {
		t.Error("A6-hidden field \"secret\" must be stripped from expand result, but it is present (FU-1 gap)")
	}
	if _, present := result.Records[0].Data["name"]; !present {
		t.Error("non-hidden field \"name\" must remain in expand result")
	}
}

// TestListLinked_Expand_FieldACL_AdminSeesAll verifies that an admin caller (IsAdmin=true)
// receives all fields even when a fieldACL enforcer is wired — admin bypasses A6 strip.
func TestListLinked_Expand_FieldACL_AdminSeesAll(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1"}
	joinRepo.listTotal = 1
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta)
	svc.WithRelationFieldACL(&fakeExpandFieldACL{hiddenField: "secret"})

	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"}, // the SOURCE, gated by NN-02 read-side
		dynrepo.Record{ID: "rec-1", TenantID: "tenant-1", Data: map[string]any{"name": "Alice", "secret": "topsecret"}},
	)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: true, Scope: RecordScope{AccessLevel: "all", IsAdmin: true}},
		fetcher)
	if err != nil {
		t.Fatalf("ListLinked expand admin: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("len(Records) = %d; want 1", len(result.Records))
	}
	if _, present := result.Records[0].Data["secret"]; !present {
		t.Error("admin must see A6-hidden field \"secret\" in expand result")
	}
}

// TestListLinked_Expand_NilFieldACL_FieldsUnchanged verifies that when no fieldACL enforcer
// is wired (nil — test/backward-compat default), expanded records are returned as-is.
func TestListLinked_Expand_NilFieldACL_FieldsUnchanged(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	joinRepo.listIDs = []string{"rec-1"}
	joinRepo.listTotal = 1
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
	svc := NewRelationService(joinRepo, relMeta) // no WithRelationFieldACL → nil enforcer

	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"}, // the SOURCE, gated by NN-02 read-side
		dynrepo.Record{ID: "rec-1", TenantID: "tenant-1", Data: map[string]any{"name": "Alice", "secret": "topsecret"}},
	)

	result, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"accounts_contacts",
		RelListQuery{Page: 1, Limit: 20, Expand: true, Scope: RecordScope{AccessLevel: "all"}},
		fetcher)
	if err != nil {
		t.Fatalf("ListLinked expand nil fieldACL: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("len(Records) = %d; want 1", len(result.Records))
	}
	// nil enforcer = no strip; both fields must be present.
	if _, present := result.Records[0].Data["secret"]; !present {
		t.Error("nil fieldACL must leave all fields intact (backward-compat)")
	}
}

// --- ENG-REL2 #4: GetRelationshipByName was broadened (migration 000028) to also resolve
// 1-N registry rows. Link/Unlink/ListLinked implement N-N join-table semantics ONLY, so a
// relationship name that resolves to a 1-N row must behave EXACTLY as it did before the
// broadening — indistinguishable from an unknown relationship name (404 ErrRelationshipNotFound
// upstream), never silently misapplied as a join-table link/unlink.

func makeOneToNRel() *metadata.RelationshipDefinition {
	return &metadata.RelationshipDefinition{
		Name:             "orders_customer",
		ParentEntityName: "accounts", // "accounts" plays the parent (customer) role here
		ChildEntityName:  "contacts", // "contacts" plays the child (order-owning) role here
		RelationshipType: "1-N",
		InverseField:     "contacts",
		ChildFKColumn:    "customer_id",
	}
}

func TestLinkRecords_OneToNRelationshipName_NotFound(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["orders_customer"] = makeOneToNRel()
	checker := newCheckerAllExist()
	svc := NewRelationService(joinRepo, relMeta)

	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	_, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"orders_customer", "tgt-1",
		checker, nil,
		fetcher, sourceScope)
	if !errors.Is(err, ErrRelationshipNotFound) {
		t.Fatalf("Link on a 1-N relationship name: err = %v; want ErrRelationshipNotFound", err)
	}
	if len(joinRepo.created) != 0 {
		t.Errorf("Link must not touch the join-table repo for a 1-N name; created=%v", joinRepo.created)
	}
}

func TestUnlinkRecords_OneToNRelationshipName_NotFound(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["orders_customer"] = makeOneToNRel()
	svc := NewRelationService(joinRepo, relMeta)

	// NN-02 now probes the TARGET under the caller's row scope too (entity-level "view" is not
	// row-level permission), so a Link test must make both endpoints visible.
	fetcher := newFakeRecordFetcher(
		dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
		dynrepo.Record{ID: "tgt-1", TenantID: "tenant-1"},
	)
	sourceScope := dynrepo.RowScope{Level: dynrepo.AccessAll}

	err := svc.Unlink(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-1",
		"orders_customer", "tgt-1",
		fetcher, sourceScope)
	if !errors.Is(err, ErrRelationshipNotFound) {
		t.Fatalf("Unlink on a 1-N relationship name: err = %v; want ErrRelationshipNotFound", err)
	}
}

func TestListLinked_OneToNRelationshipName_NotFound(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["orders_customer"] = makeOneToNRel()
	svc := NewRelationService(joinRepo, relMeta)

	_, err := svc.ListLinked(context.Background(), "tenant-1",
		"accounts", "src-1",
		"orders_customer",
		RelListQuery{Page: 1, Limit: 20},
		nil)
	if !errors.Is(err, ErrRelationshipNotFound) {
		t.Fatalf("ListLinked on a 1-N relationship name: err = %v; want ErrRelationshipNotFound", err)
	}
	if joinRepo.listCalled {
		t.Error("ListLinked must not reach the join-table repo for a 1-N name")
	}
}

// TestListLinked_GatesTheSourceRecord pins the NN-02 gap on the READ side.
//
// Link and Unlink always probed the SOURCE record under the caller's row scope; ListLinked did not,
// and the SQL it runs never touches the source table — it joins only the TARGET — so the target
// scope could not stand in for it. What leaked was the ASSOCIATION GRAPH ("these rows I can already
// read are attached to source X") for an X the caller is 404'd out of, plus an existence oracle on
// the source id. The asymmetry is what made it a bug: the reverse direction was already filtered.
func TestListLinked_GatesTheSourceRecord(t *testing.T) {
	newSvc := func() *RelationService {
		joinRepo := newFakeJoinRepo()
		joinRepo.listIDs = []string{"rec-1"}
		joinRepo.listTotal = 1
		relMeta := newFakeRelMeta("accounts", "contacts")
		relMeta.existingRels["accounts_contacts"] = makeAccountsContactsRel()
		return NewRelationService(joinRepo, relMeta)
	}
	query := RelListQuery{Page: 1, Limit: 20, Scope: RecordScope{UserID: "u1", AccessLevel: "owner"}}

	t.Run("source NOT visible to this caller → not-found, no ids", func(t *testing.T) {
		// Only the TARGET exists in the fetcher: the caller can read rec-1 on its own list, but the
		// source row belongs to a colleague. That is exactly the shape that used to leak.
		fetcher := newFakeRecordFetcher(dynrepo.Record{ID: "rec-1", TenantID: "tenant-1"})
		_, err := newSvc().ListLinked(context.Background(), "tenant-1",
			"accounts", "src-hidden", "accounts_contacts", query, fetcher)
		if !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("err = %v; want ErrRecordNotFound — the association graph of an invisible record must not be listable", err)
		}
	})

	t.Run("source visible → the linked ids come back", func(t *testing.T) {
		fetcher := newFakeRecordFetcher(
			dynrepo.Record{ID: "src-1", TenantID: "tenant-1"},
			dynrepo.Record{ID: "rec-1", TenantID: "tenant-1"},
		)
		res, err := newSvc().ListLinked(context.Background(), "tenant-1",
			"accounts", "src-1", "accounts_contacts", query, fetcher)
		if err != nil {
			t.Fatalf("visible source refused: %v", err)
		}
		if len(res.IDs) != 1 || res.IDs[0] != "rec-1" {
			t.Errorf("IDs = %v; want [rec-1]", res.IDs)
		}
	})

	t.Run("admin is not gated", func(t *testing.T) {
		fetcher := newFakeRecordFetcher() // nothing seeded at all
		res, err := newSvc().ListLinked(context.Background(), "tenant-1",
			"accounts", "src-1", "accounts_contacts",
			RelListQuery{Page: 1, Limit: 20, Scope: RecordScope{IsAdmin: true, AccessLevel: "all"}}, fetcher)
		if err != nil {
			t.Fatalf("admin refused: %v", err)
		}
		if len(res.IDs) != 1 {
			t.Errorf("admin got %d ids; want 1", len(res.IDs))
		}
	})
}

// TestNN02_Link_TargetOutsideScope_Fails is the other half of NN-02, which shipped guarding only
// the SOURCE. The target was validated with a tenant-wide existence check, under a comment claiming
// "target scope is already enforced by the CanViewEntity ACL-on-target check above" — but that
// check is ENTITY-level: it answers "may you view this kind of record", never "may you view THIS
// one". So a user with view:own on the target could link a colleague's row to their own record, and
// ListLinked then returns it to them forever after: a durable read of a row they were never allowed
// to see, created by a write they were allowed to make.
func TestNN02_Link_TargetOutsideScope_Fails(t *testing.T) {
	joinRepo := newFakeJoinRepo()
	relMeta := newFakeRelMeta("accounts", "contacts")
	relMeta.existingRels["accounts_contacts"] = &metadata.RelationshipDefinition{
		Name:             "accounts_contacts",
		ParentEntityName: "accounts",
		ChildEntityName:  "contacts",
		RelationshipType: "N-N",
		JoinTableName:    strPtr("rel_accounts_contacts"),
	}
	svc := NewRelationService(joinRepo, relMeta)

	// The source is visible; the TARGET is not in the fetcher — a colleague's row.
	fetcher := newFakeRecordFetcher(dynrepo.Record{ID: "src-owned", TenantID: "tenant-1"})
	ownerScope := dynrepo.RowScope{UserID: "user-1", Level: dynrepo.AccessOwner}

	created, err := svc.Link(context.Background(), "tenant-1", "user-1", "role-1", false,
		"accounts", "src-owned",
		"accounts_contacts", "tgt-hidden",
		newCheckerAllExist(), nil, // the tenant-wide checker WOULD say this exists — that was the bug
		fetcher, ownerScope)

	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("err = %v; want RECORD_RELATION_INVALID — an unreachable target must refuse exactly like a missing one", err)
	}
	if created {
		t.Error("a link to an out-of-scope target was created")
	}
	if len(joinRepo.links) != 0 {
		t.Errorf("join rows written despite the refusal: %v", joinRepo.links)
	}
}
