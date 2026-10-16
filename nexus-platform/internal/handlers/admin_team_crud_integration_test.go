//go:build integration

// A5-extra Team/Member CRUD integration tests against a REAL MySQL 8 brought up with golang-migrate
// (reusing the A4 harness: a4SetupMigratedDB applies migrations 000001..000006, so the teams /
// team_memberships / team_sets tables come from migration 000006_create_team_rls_a5, NOT AutoMigrate).
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Covers /admin/teams + /admin/teams/:id/members end to end through the production chain:
//   - team CRUD round-trip (POST create, GET/:id, GET list, PUT rename, DELETE) as a tenant-A admin;
//   - member add/remove round-trip (POST add → ListMembers shows the user → DELETE remove);
//   - tenant-scoped: a team created under tenant A is invisible/unmodifiable from tenant B (404);
//   - cross-tenant member guard: A cannot add a user that belongs to tenant B (404 MEMBER_USER_NOT_FOUND);
//   - admin-gated: a non-admin principal hitting any /admin/teams method → 403 (RequireAdmin);
//   - DB cascade: deleting a team that has members removes the team_memberships rows (FK ON DELETE CASCADE).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

const (
	teamTenantAID   = "a5t00000-0000-0000-0000-0000000000a1"
	teamTenantBID   = "a5t00000-0000-0000-0000-0000000000b2"
	teamTenantASlug = "a5t-acme"
	teamTenantBSlug = "a5t-globex"
)

// teamEnvelope decodes {"data":{"team":{id,name,parentId}}}.
type teamEnvelope struct {
	Data struct {
		Team struct {
			ID       string  `json:"id"`
			Name     string  `json:"name"`
			ParentID *string `json:"parentId"`
		} `json:"team"`
	} `json:"data"`
	Code string `json:"code"`
}

// teamListEnvelope decodes {"data":{"teams":[{id,name}]}}.
type teamListEnvelope struct {
	Data struct {
		Teams []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"teams"`
	} `json:"data"`
}

// memberListEnvelope decodes {"data":{"members":[{id,email}]}}.
type memberListEnvelope struct {
	Data struct {
		Members []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"members"`
	} `json:"data"`
}

// TestA5_TeamCRUDFullCycle: create → get → list → update (rename) → delete, all against the migrated DB.
func TestA5_TeamCRUDFullCycle(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	adminTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken

	// CREATE
	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", adminTok,
		map[string]any{"name": "Sales"}, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("create team: %d %s", wc.Code, wc.Body.String())
	}
	var created teamEnvelope
	if err := json.Unmarshal(wc.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	teamID := created.Data.Team.ID
	if teamID == "" || created.Data.Team.Name != "Sales" {
		t.Fatalf("create returned unexpected team: %+v", created.Data.Team)
	}

	// duplicate name → 409
	wd := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", adminTok,
		map[string]any{"name": "Sales"}, nil)
	if wd.Code != http.StatusConflict {
		t.Fatalf("duplicate name: %d; want 409", wd.Code)
	}

	// GET /:id
	wg := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams/"+teamID, adminTok, nil, nil)
	if wg.Code != http.StatusOK {
		t.Fatalf("get team: %d %s", wg.Code, wg.Body.String())
	}

	// GET list
	wl := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams", adminTok, nil, nil)
	var list teamListEnvelope
	_ = json.Unmarshal(wl.Body.Bytes(), &list)
	if !teamListContains(list, teamID) {
		t.Fatalf("list missing created team %s: %+v", teamID, list.Data.Teams)
	}

	// PUT rename
	wu := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/teams/"+teamID, adminTok,
		map[string]any{"name": "Sales Team"}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("update team: %d %s", wu.Code, wu.Body.String())
	}
	var updated teamEnvelope
	_ = json.Unmarshal(wu.Body.Bytes(), &updated)
	if updated.Data.Team.Name != "Sales Team" {
		t.Fatalf("rename failed: %+v", updated.Data.Team)
	}

	// DELETE → then GET 404
	wdel := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+teamID, adminTok, nil, nil)
	if wdel.Code != http.StatusNoContent {
		t.Fatalf("delete team: %d %s; want 204", wdel.Code, wdel.Body.String())
	}
	wg2 := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams/"+teamID, adminTok, nil, nil)
	if wg2.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d; want 404", wg2.Code)
	}
}

// TestA5_TeamMemberAddRemove: add a tenant user to a team, see them listed, remove them.
func TestA5_TeamMemberAddRemove(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	memberID := a4SeedUser(t, gdb, teamTenantAID, "member@"+teamTenantASlug, false, nil)
	adminTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken

	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", adminTok,
		map[string]any{"name": "Eng"}, nil)
	var created teamEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &created)
	teamID := created.Data.Team.ID

	// ADD member
	wa := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams/"+teamID+"/members", adminTok,
		map[string]any{"userId": memberID}, nil)
	if wa.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", wa.Code, wa.Body.String())
	}

	// idempotent re-add → still 200
	wa2 := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams/"+teamID+"/members", adminTok,
		map[string]any{"userId": memberID}, nil)
	if wa2.Code != http.StatusOK {
		t.Fatalf("re-add member: %d; want 200 (idempotent)", wa2.Code)
	}

	// LIST members
	wlm := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams/"+teamID+"/members", adminTok, nil, nil)
	var members memberListEnvelope
	_ = json.Unmarshal(wlm.Body.Bytes(), &members)
	if len(members.Data.Members) != 1 || members.Data.Members[0].ID != memberID {
		t.Fatalf("members = %+v; want exactly [%s]", members.Data.Members, memberID)
	}

	// DB sanity: exactly one membership row exists.
	var n int
	if err := sx.Get(&n, "SELECT COUNT(*) FROM team_memberships WHERE team_id = ? AND user_id = ?", teamID, memberID); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if n != 1 {
		t.Fatalf("team_memberships count = %d; want 1", n)
	}

	// REMOVE member → then list empty
	wr := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+teamID+"/members/"+memberID, adminTok, nil, nil)
	if wr.Code != http.StatusNoContent {
		t.Fatalf("remove member: %d %s; want 204", wr.Code, wr.Body.String())
	}
	// removing again → 404 (no longer a member)
	wr2 := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+teamID+"/members/"+memberID, adminTok, nil, nil)
	if wr2.Code != http.StatusNotFound {
		t.Fatalf("re-remove member: %d; want 404", wr2.Code)
	}
}

// TestA5_TeamMemberCascadeOnDelete: deleting a team with members removes its team_memberships rows
// (FK ON DELETE CASCADE), proving the migration's cascade is real (exercised via the live DELETE).
func TestA5_TeamMemberCascadeOnDelete(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	memberID := a4SeedUser(t, gdb, teamTenantAID, "m@"+teamTenantASlug, false, nil)
	adminTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken

	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", adminTok,
		map[string]any{"name": "Doomed"}, nil)
	var created teamEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &created)
	teamID := created.Data.Team.ID

	wa := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams/"+teamID+"/members", adminTok,
		map[string]any{"userId": memberID}, nil)
	if wa.Code != http.StatusOK {
		t.Fatalf("add member: %d %s", wa.Code, wa.Body.String())
	}

	wdel := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+teamID, adminTok, nil, nil)
	if wdel.Code != http.StatusNoContent {
		t.Fatalf("delete team with members: %d %s; want 204 (no 500)", wdel.Code, wdel.Body.String())
	}

	var orphans int
	if err := sx.Get(&orphans, "SELECT COUNT(*) FROM team_memberships WHERE team_id = ?", teamID); err != nil {
		t.Fatalf("count orphan memberships: %v", err)
	}
	if orphans != 0 {
		t.Errorf("team_memberships left %d orphans for deleted team; want 0 (ON DELETE CASCADE)", orphans)
	}

	// the user row survives.
	var userN int
	if err := sx.Get(&userN, "SELECT COUNT(*) FROM users WHERE id = ?", memberID); err != nil {
		t.Fatalf("count user: %v", err)
	}
	if userN != 1 {
		t.Errorf("member user row count = %d after team delete; want 1 (user survives)", userN)
	}
}

// TestA5_TeamTenantScoped: a team created by tenant A is invisible/immutable from tenant B.
func TestA5_TeamTenantScoped(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedTenant(t, gdb, teamTenantBID, teamTenantBSlug, "Globex Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	a4SeedUser(t, gdb, teamTenantBID, "admin@"+teamTenantBSlug, true, nil)
	aTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken
	bTok := rt.login(t, teamTenantBSlug, "admin@"+teamTenantBSlug).Data.AccessToken

	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", aTok,
		map[string]any{"name": "a-only"}, nil)
	var created teamEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &created)
	aTeamID := created.Data.Team.ID

	// B's list must NOT contain A's team.
	wl := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams", bTok, nil, nil)
	var bList teamListEnvelope
	_ = json.Unmarshal(wl.Body.Bytes(), &bList)
	if teamListContains(bList, aTeamID) {
		t.Fatalf("tenant B sees tenant A's team %s: %+v", aTeamID, bList.Data.Teams)
	}

	// B get/update/delete + member ops by A's id → 404 (cross-tenant id not honored).
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"get", http.MethodGet, "/api/v1/admin/teams/" + aTeamID, nil},
		{"update", http.MethodPut, "/api/v1/admin/teams/" + aTeamID, map[string]any{"name": "hijack"}},
		{"delete", http.MethodDelete, "/api/v1/admin/teams/" + aTeamID, nil},
		{"list-members", http.MethodGet, "/api/v1/admin/teams/" + aTeamID + "/members", nil},
		{"add-member", http.MethodPost, "/api/v1/admin/teams/" + aTeamID + "/members", map[string]any{"userId": "anyone"}},
	} {
		w := doAuthed(t, rt.engine, tc.method, tc.path, bTok, tc.body, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("B %s A's team: got %d; want 404 (cross-tenant isolation)", tc.name, w.Code)
		}
	}

	// Same name allowed across tenants (uniqueness is per-tenant).
	wb := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", bTok,
		map[string]any{"name": "a-only"}, nil)
	if wb.Code != http.StatusCreated {
		t.Errorf("B create same name as A: %d; want 201 (per-tenant uniqueness)", wb.Code)
	}
}

// TestA5_AddMemberCrossTenantUser404: A cannot add a user that belongs to tenant B to A's team.
func TestA5_AddMemberCrossTenantUser404(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedTenant(t, gdb, teamTenantBID, teamTenantBSlug, "Globex Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	bUserID := a4SeedUser(t, gdb, teamTenantBID, "buser@"+teamTenantBSlug, false, nil)
	aTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken

	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", aTok,
		map[string]any{"name": "A Team"}, nil)
	var created teamEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &created)
	aTeamID := created.Data.Team.ID

	// A tries to add B's user → 404 MEMBER_USER_NOT_FOUND (the cross-tenant member guard).
	wa := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams/"+aTeamID+"/members", aTok,
		map[string]any{"userId": bUserID}, nil)
	if wa.Code != http.StatusNotFound {
		t.Fatalf("A add B's user: %d %s; want 404 (cross-tenant member guard)", wa.Code, wa.Body.String())
	}

	// DB sanity: no membership row was created.
	var n int
	if err := sx.Get(&n, "SELECT COUNT(*) FROM team_memberships WHERE team_id = ?", aTeamID); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if n != 0 {
		t.Errorf("team_memberships count = %d after cross-tenant add; want 0", n)
	}
}

// TestA5_TeamAdminGated: a NON-admin principal is refused (403) on every /admin/teams method.
func TestA5_TeamAdminGated(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedUser(t, gdb, teamTenantAID, "member@"+teamTenantASlug, false, nil)
	memberTok := rt.login(t, teamTenantASlug, "member@"+teamTenantASlug).Data.AccessToken

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list", http.MethodGet, "/api/v1/admin/teams", nil},
		{"get", http.MethodGet, "/api/v1/admin/teams/some-id", nil},
		{"create", http.MethodPost, "/api/v1/admin/teams", map[string]any{"name": "x"}},
		{"update", http.MethodPut, "/api/v1/admin/teams/some-id", map[string]any{"name": "y"}},
		{"delete", http.MethodDelete, "/api/v1/admin/teams/some-id", nil},
		{"list-members", http.MethodGet, "/api/v1/admin/teams/some-id/members", nil},
		{"add-member", http.MethodPost, "/api/v1/admin/teams/some-id/members", map[string]any{"userId": "u"}},
		{"remove-member", http.MethodDelete, "/api/v1/admin/teams/some-id/members/u", nil},
	} {
		w := doAuthed(t, rt.engine, tc.method, tc.path, memberTok, tc.body, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("non-admin %s /admin/teams: got %d; want 403 (admin-gate)", tc.name, w.Code)
		}
	}
}

// TestA5_TeamParentCycleRejected (queue #21) drives PUT /admin/teams/:id through the production
// chain: with a 3-level tree Vùng Bắc > Chi nhánh Hà Nội > PGD Cầu Giấy, making the region a child
// of its own grandchild (or of itself) is refused 422 TEAM_PARENT_CYCLE with a readable reason and
// the stored parent is untouched; a legal reparent and an explicit detach still return 200.
func TestA5_TeamParentCycleRejected(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	adminTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken

	create := func(name string, parent *string) string {
		t.Helper()
		w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", adminTok,
			map[string]any{"name": name, "parentId": parent}, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, w.Code, w.Body.String())
		}
		var env teamEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return env.Data.Team.ID
	}
	region := create("Vùng Bắc", nil)
	branch := create("Chi nhánh Hà Nội", &region)
	office := create("PGD Cầu Giấy", &branch)

	put := func(id, parent string) (int, teamEnvelope) {
		t.Helper()
		w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/teams/"+id, adminTok,
			map[string]any{"parentId": parent}, nil)
		var env teamEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		return w.Code, env
	}

	for _, bad := range []struct{ who, parent string }{
		{"grandchild as parent", office},
		{"child as parent", branch},
		{"self as parent", region},
	} {
		code, env := put(region, bad.parent)
		if code != http.StatusUnprocessableEntity || env.Code != "TEAM_PARENT_CYCLE" {
			t.Fatalf("%s: got %d %s; want 422 TEAM_PARENT_CYCLE", bad.who, code, env.Code)
		}
		wg := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams/"+region, adminTok, nil, nil)
		var got teamEnvelope
		_ = json.Unmarshal(wg.Body.Bytes(), &got)
		if got.Data.Team.ParentID != nil {
			t.Fatalf("%s: region parent written despite refusal: %v", bad.who, *got.Data.Team.ParentID)
		}
	}

	// Legal reparent: office directly under region.
	if code, env := put(office, region); code != http.StatusOK || env.Data.Team.ParentID == nil || *env.Data.Team.ParentID != region {
		t.Fatalf("legal reparent: %d %+v", code, env.Data.Team)
	}
	// Explicit detach.
	if code, env := put(office, ""); code != http.StatusOK || env.Data.Team.ParentID != nil {
		t.Fatalf("detach: %d %+v", code, env.Data.Team)
	}
}

// TestA5_TeamDeleteRehomesChildren (review follow-up to queue #20): deleting the MIDDLE team of
// Vùng Bắc > Chi nhánh Hà Nội > PGD Cầu Giấy re-homes the office under the region — parent_id has
// no FK, so without the re-home the office would dangle and silently drop out of the region's
// downward-inherited row scope. Deleting a root team detaches its children (parent → NULL).
func TestA5_TeamDeleteRehomesChildren(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, teamTenantAID, teamTenantASlug, "Acme Teams")
	a4SeedUser(t, gdb, teamTenantAID, "admin@"+teamTenantASlug, true, nil)
	adminTok := rt.login(t, teamTenantASlug, "admin@"+teamTenantASlug).Data.AccessToken

	create := func(name string, parent *string) string {
		t.Helper()
		w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/teams", adminTok,
			map[string]any{"name": name, "parentId": parent}, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, w.Code, w.Body.String())
		}
		var env teamEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		return env.Data.Team.ID
	}
	region := create("Vùng Bắc", nil)
	branch := create("Chi nhánh Hà Nội", &region)
	office := create("PGD Cầu Giấy", &branch)

	parentOf := func(id string) *string {
		t.Helper()
		w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/teams/"+id, adminTok, nil, nil)
		var env teamEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || w.Code != http.StatusOK {
			t.Fatalf("get %s: %d %s", id, w.Code, w.Body.String())
		}
		return env.Data.Team.ParentID
	}

	if w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+branch, adminTok, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete branch: %d %s", w.Code, w.Body.String())
	}
	if p := parentOf(office); p == nil || *p != region {
		t.Fatalf("office parent after deleting branch = %v; want region %s (re-homed to grandparent)", p, region)
	}

	if w := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/teams/"+region, adminTok, nil, nil); w.Code != http.StatusNoContent {
		t.Fatalf("delete region: %d %s", w.Code, w.Body.String())
	}
	if p := parentOf(office); p != nil {
		t.Fatalf("office parent after deleting root = %s; want nil (detached)", *p)
	}
}

// teamListContains reports whether the list envelope holds a team with the given id.
func teamListContains(l teamListEnvelope, id string) bool {
	for _, tm := range l.Data.Teams {
		if tm.ID == id {
			return true
		}
	}
	return false
}
