package services

// #331 relation-fk-view-scope: a relation FK may only point at a record the WRITER may VIEW.
//
// Before: checkRelationTargets probed existence tenant-wide, so an owner-scoped rep could put the
// id of a colleague's customer into `khach_hang` — and 422 vs 200 told them the id was real, and
// a system rule keyed on that relation (specs/058, run under AdminScope) could then act on a
// record they were never allowed to see. After: the probe runs under the writer's VIEW scope on
// the TARGET entity, and "not visible" is answered exactly like "does not exist".

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func danglingRelField() metadata.FieldDefinition {
	return metadata.FieldDefinition{Name: "khach_hang", FieldType: "relation", Options: &relOpts}
}

// scopedRelStore answers the tenant-wide probe "everything exists" (the old, leaky behaviour) and
// the scope-aware probe from `visible`, recording the scope it was handed.
type scopedRelStore struct {
	fakeRecordStore
	visible    map[string]bool // relation id → visible to the writer
	current    map[string]any  // what the stored record holds today (Update)
	scopedCall []dynrepo.RowScope
	scopedEnt  []string
	updated    bool
	inserted   int
}

func (s *scopedRelStore) InsertRecord(_ context.Context, _, _, _ string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	s.inserted++
	return "new-id", nil
}

func (s *scopedRelStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: "rec-1", Data: s.current}, nil
}

func (s *scopedRelStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	s.updated = true
	return nil
}

func (s *scopedRelStore) BatchRecordExistsInScope(_ context.Context, entity, _ string, ids []string, scope dynrepo.RowScope) (map[string]bool, error) {
	s.scopedCall = append(s.scopedCall, scope)
	s.scopedEnt = append(s.scopedEnt, entity)
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		// Like the real rowFilter: AccessNone matches no row at all.
		out[id] = s.visible[id] && scope.Level != dynrepo.AccessNone
	}
	return out, nil
}

// viewAuthz grants "view" on the target at a level (or denies it), records what it was asked.
type viewAuthz struct {
	level string
	deny  bool
	asked []string // "entity:action"
}

func (a *viewAuthz) Authorize(_ context.Context, _, _, entity, action string) (bool, string, error) {
	a.asked = append(a.asked, entity+":"+action)
	return !a.deny, a.level, nil
}

func repScope() RecordScope {
	// create/edit level here is deliberately NOT the view level — the probe must re-resolve "view".
	return RecordScope{UserID: "rep-1", AccessLevel: "all", RoleID: "role-rep"}
}

func newScopedRelSvc(store *scopedRelStore, authz *viewAuthz) *RecordService {
	return newDanglingServiceFor(store).WithACLAuthorizer(authz)
}

func newDanglingServiceFor(store RecordStore) *RecordService {
	return NewRecordService(&fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: append(courseFieldDefs(), danglingRelField()),
	}, store)
}

func TestCreate_RelationToARecordTheWriterCannotViewIsRefusedLikeAMissingOne(t *testing.T) {
	store := &scopedRelStore{visible: map[string]bool{"acc-mine": true}}
	authz := &viewAuthz{level: "owner"}
	svc := newScopedRelSvc(store, authz)

	// A colleague's customer: it EXISTS in the tenant (tenant-wide probe says yes) but is not visible.
	_, err := svc.Create(context.Background(), repScope(), "tenant-1", "rep-1", "courses",
		map[string]any{"title": "x", "khach_hang": "acc-colleague"})
	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("err = %v; want RECORD_RELATION_INVALID", err)
	}
	if store.inserted != 0 {
		t.Error("a record was inserted pointing at a customer the writer cannot see")
	}
	// Same sentence as a genuinely missing id: the answer must not reveal the record exists.
	_, ghost := svc.Create(context.Background(), repScope(), "tenant-1", "rep-1", "courses",
		map[string]any{"title": "x", "khach_hang": "acc-ghost"})
	if err.Error() != ghost.Error() {
		t.Errorf("hidden-vs-missing oracle: %q != %q", err.Error(), ghost.Error())
	}
	// The probe ran on the TARGET entity under the writer's VIEW level, not the write level.
	if len(store.scopedCall) == 0 || store.scopedEnt[0] != "crm_accounts" {
		t.Fatalf("scoped probe not used / wrong entity: %v", store.scopedEnt)
	}
	sc := store.scopedCall[0]
	if sc.Level != dynrepo.AccessOwner || sc.UserID != "rep-1" || sc.IsAdmin || !sc.IncludeShares {
		t.Errorf("probe scope = %+v; want owner-level, user rep-1, not admin, shares included (view)", sc)
	}
	if len(authz.asked) == 0 || authz.asked[0] != "crm_accounts:view" {
		t.Errorf("authz asked %v; want crm_accounts:view", authz.asked)
	}
}

func TestCreate_RelationToAVisibleRecordStillWrites(t *testing.T) {
	store := &scopedRelStore{visible: map[string]bool{"acc-mine": true}}
	svc := newScopedRelSvc(store, &viewAuthz{level: "owner"})
	if _, err := svc.Create(context.Background(), repScope(), "tenant-1", "rep-1", "courses",
		map[string]any{"title": "x", "khach_hang": "acc-mine"}); err != nil {
		t.Fatalf("visible target refused: %v", err)
	}
	if store.inserted != 1 {
		t.Errorf("inserted = %d; want 1", store.inserted)
	}
}

func TestCreate_NoViewGrantOnTheTargetEntityRefusesTheRelation(t *testing.T) {
	store := &scopedRelStore{visible: map[string]bool{"acc-mine": true}}
	svc := newScopedRelSvc(store, &viewAuthz{level: "all", deny: true})
	_, err := svc.Create(context.Background(), repScope(), "tenant-1", "rep-1", "courses",
		map[string]any{"title": "x", "khach_hang": "acc-mine"})
	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("err = %v; want RECORD_RELATION_INVALID when the role cannot view the target entity at all", err)
	}
	if store.inserted != 0 {
		t.Error("insert ran despite no view grant on the target entity")
	}
}

func TestCreate_AdminRelationProbeStaysTenantWide(t *testing.T) {
	store := &scopedRelStore{visible: map[string]bool{}} // scoped probe would answer "nothing visible"
	authz := &viewAuthz{level: "owner"}
	svc := newScopedRelSvc(store, authz)
	if _, err := svc.Create(context.Background(), RecordScope{UserID: "adm", AccessLevel: "all", IsAdmin: true},
		"tenant-1", "adm", "courses", map[string]any{"title": "x", "khach_hang": "acc-any"}); err != nil {
		t.Fatalf("admin refused: %v", err)
	}
	if len(store.scopedCall) != 0 || len(authz.asked) != 0 {
		t.Errorf("admin must not be narrowed (scoped calls=%d, authz asked=%v)", len(store.scopedCall), authz.asked)
	}
}

func TestUpdate_ChangingARelationToAHiddenRecordIsRefused_ResendingAnUntouchedOneIsNot(t *testing.T) {
	store := &scopedRelStore{
		visible: map[string]bool{"acc-mine": true},
		current: map[string]any{"khach_hang": "acc-legacy-hidden", "title": "cũ"},
	}
	svc := newScopedRelSvc(store, &viewAuthz{level: "owner"})

	// The form resends every field: an untouched FK (even one the writer can no longer see) must
	// not make the record unsavable — same unchanged-value rule as the dangling-target fix.
	if err := svc.Update(context.Background(), repScope(), "tenant-1", "courses", "rec-1",
		map[string]any{"khach_hang": "acc-legacy-hidden", "title": "mới"}, nil); err != nil {
		t.Fatalf("untouched relation blocked the save: %v", err)
	}
	if !store.updated {
		t.Fatal("update never reached the store")
	}

	store.updated = false
	err := svc.Update(context.Background(), repScope(), "tenant-1", "courses", "rec-1",
		map[string]any{"khach_hang": "acc-colleague"}, nil)
	if codeOf(err) != "RECORD_RELATION_INVALID" {
		t.Fatalf("err = %v; want RECORD_RELATION_INVALID", err)
	}
	if store.updated {
		t.Error("record re-pointed at a customer the writer cannot see")
	}
}

func TestUpdate_ChangingARelationToAVisibleRecordWrites(t *testing.T) {
	store := &scopedRelStore{
		visible: map[string]bool{"acc-mine": true},
		current: map[string]any{"khach_hang": "acc-other", "title": "cũ"},
	}
	svc := newScopedRelSvc(store, &viewAuthz{level: "team"})
	if err := svc.Update(context.Background(), repScope(), "tenant-1", "courses", "rec-1",
		map[string]any{"khach_hang": "acc-mine"}, nil); err != nil {
		t.Fatalf("visible target refused: %v", err)
	}
	if got := store.scopedCall[0].Level; got != dynrepo.AccessTeam {
		t.Errorf("probe level = %q; want team (the writer's view level)", got)
	}
}

// The batch path is the same hole through a different door.
func TestCreateBulk_RelationToAHiddenRecordFailsThatRowOnly(t *testing.T) {
	inner := &bulkTestStore{live: map[string]bool{
		"accounts|tenant-1|acct-mine": true, "accounts|tenant-1|acct-colleague": true,
	}}
	store := &bulkScopedStore{bulkTestStore: inner, visible: map[string]bool{"acct-mine": true}}
	svc := NewRecordService(bulkCatalogFixture(t), store).
		WithSequenceClaimer(&fakeSequenceClaimer{}).WithBulkInserter(&fakeBulkInserter{}).
		WithACLAuthorizer(&viewAuthz{level: "owner"})

	res, err := svc.CreateBulk(context.Background(), bulkRoleScope(), "tenant-1", "u1", "widgets",
		[]map[string]any{
			{"name": "Row0", "owner": "acct-mine"},
			{"name": "Row1", "owner": "acct-colleague"},
		}, BulkModePartial)
	if err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}
	if res.Results[0].Status != "created" {
		t.Errorf("Results[0] = %+v; want created", res.Results[0])
	}
	if res.Results[1].Status != "failed" || res.Results[1].Code != "RECORD_RELATION_INVALID" {
		t.Errorf("Results[1] = %+v; want failed RECORD_RELATION_INVALID", res.Results[1])
	}
}

type bulkScopedStore struct {
	*bulkTestStore
	visible map[string]bool
}

func (s *bulkScopedStore) BatchRecordExistsInScope(_ context.Context, _, _ string, ids []string, _ dynrepo.RowScope) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = s.visible[id]
	}
	return out, nil
}

// Compile-time: the production repository must satisfy the scoped probe, or the guard would
// silently fall back to the tenant-wide probe in production.
var _ scopedRelationExister = (*dynrepo.Repository)(nil)

// ---- polymorphic_relation: the same bug-class, same rule (#331 sibling) -------------------------

func polyTargetField() metadata.FieldDefinition {
	opts := `{"targetEntities":["crm_accounts"]}`
	return metadata.FieldDefinition{Name: "related_to", FieldType: "polymorphic_relation", Options: &opts}
}

func newScopedPolySvc(store *scopedRelStore, authz *viewAuthz) *RecordService {
	return NewRecordService(&fakeRecordCatalog{
		entity: courseEntityFixture(),
		fields: append(courseFieldDefs(), polyTargetField()),
	}, store).WithACLAuthorizer(authz)
}

func polyVal(id string) map[string]any {
	return map[string]any{"related_to": map[string]any{"type": "crm_accounts", "id": id}}
}

func TestPolymorphic_PointingAtARecordTheWriterCannotViewIsRefusedLikeAMissingOne(t *testing.T) {
	store := &scopedRelStore{visible: map[string]bool{"acc-mine": true}}
	svc := newScopedPolySvc(store, &viewAuthz{level: "owner"})
	ctx := context.Background()

	mk := func(id string) error {
		p := polyVal(id)
		p["title"] = "x"
		_, err := svc.Create(ctx, repScope(), "tenant-1", "rep-1", "courses", p)
		return err
	}
	if err := mk("acc-mine"); err != nil {
		t.Fatalf("visible target refused: %v", err)
	}
	hidden, ghost := mk("acc-colleague"), mk("acc-ghost")
	if codeOf(hidden) != "RECORD_POLYMORPHIC_TARGET_INVALID" {
		t.Fatalf("err = %v; want RECORD_POLYMORPHIC_TARGET_INVALID", hidden)
	}
	if hidden.Error() != ghost.Error() && codeOf(ghost) != codeOf(hidden) {
		t.Errorf("hidden-vs-missing oracle: %q vs %q", hidden, ghost)
	}
	sc := store.scopedCall[len(store.scopedCall)-1]
	if sc.Level != dynrepo.AccessOwner || sc.UserID != "rep-1" || !sc.IncludeShares {
		t.Errorf("probe scope = %+v; want the writer's owner-level VIEW with shares", sc)
	}

	// PUT: re-pointing at a hidden record is refused; resending an untouched hidden one is not.
	store.current = map[string]any{"related_to": map[string]any{"type": "crm_accounts", "id": "acc-legacy-hidden"}}
	if err := svc.Update(ctx, repScope(), "tenant-1", "courses", "rec-1", polyVal("acc-legacy-hidden"), nil); err != nil {
		t.Fatalf("untouched polymorphic value blocked the save: %v", err)
	}
	if err := svc.Update(ctx, repScope(), "tenant-1", "courses", "rec-1", polyVal("acc-colleague"), nil); codeOf(err) != "RECORD_POLYMORPHIC_TARGET_INVALID" {
		t.Fatalf("update err = %v; want RECORD_POLYMORPHIC_TARGET_INVALID", err)
	}
}

func TestPolymorphic_BulkHonoursTheWritersViewScope(t *testing.T) {
	store := &scopedRelStore{visible: map[string]bool{"acc-mine": true}}
	svc := newScopedPolySvc(store, &viewAuthz{level: "owner"})
	p := polyVal("acc-colleague")
	p["title"] = "x"
	res, err := svc.CreateBulk(context.Background(), repScope(), "tenant-1", "rep-1", "courses", []map[string]any{p}, BulkModePartial)
	code := codeOf(err)
	if err == nil && len(res.Results) > 0 {
		code = res.Results[0].Code
	}
	if code != "RECORD_POLYMORPHIC_TARGET_INVALID" || store.inserted != 0 {
		t.Fatalf("res=%+v err=%v inserted=%d; want RECORD_POLYMORPHIC_TARGET_INVALID, nothing written", res, err, store.inserted)
	}
}
