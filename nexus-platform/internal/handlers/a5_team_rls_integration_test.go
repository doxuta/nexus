//go:build integration

// A5-qa — Team row-level-security (RLS) integration tests driving the FULL production HTTP chain
// Auth → Tenant → ACL → DynamicRecordHandler → dynrepo against a REAL MySQL 8 brought up with the
// hand-written golang-migrate migrations 000001..000006 (the A5 schema teams / team_memberships /
// team_sets / team_sets_teams lives in 000006, NOT AutoMigrate — see a4_rbac_harness_integration_test.go).
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// WHY A HANDLER-LEVEL SUITE (distinct from internal/dynrepo/rls_a5_integration_test.go):
// the dynrepo suite proves the rowFilter SQL in isolation by handing the repo a hand-built RowScope.
// THIS suite proves the wiring that turns a real login into that scope end-to-end:
//
//   - the access_level the row filter uses is the one the ACL stage resolved from the principal's
//     acl_role_actions grant (none/owner/team/all) — never a hand-forged value;
//   - the row's created_by is the REAL authenticated user id minted by login (task requirement:
//     "created_by là user THẬT, không placeholder") — asserted directly against obj_* in the DB;
//   - the row's team_set_id is derived from THAT real creator's team memberships at POST time, so an
//     access_level=team viewer who shares a team can later read it over HTTP.
//
// The four access levels are each proven through the HTTP surface (GET list + GET by id):
//
//	team  → caller sees own rows + rows whose team_set shares a team with the caller
//	        (team_memberships → team_sets_teams), and NOT a different team's user's rows.
//	all   → caller sees every row in the tenant.
//	owner → caller sees ONLY the rows they created.
//	none  → caller sees NOTHING (empty list, 404 on direct get) — the row filter fails closed even
//	        though the ACL grant itself permits the action.
//
// Tenant isolation is layered on top: an access_level=all caller in tenant A never sees tenant B's
// rows, and a tenant-A caller hitting the route never reaches tenant-B data (the tenant comes from
// the signed claim, never a client value).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
)

// A5-qa fixture identity (own tenants/slugs so this suite is independent of the A4 ACL fixtures).
const (
	rlsTenantAID   = "a5a50000-0000-0000-0000-0000000000a1"
	rlsTenantBID   = "a5a50000-0000-0000-0000-0000000000b2"
	rlsTenantASlug = "a5-acme"
	rlsTenantBSlug = "a5-globex"
	rlsEntity      = "it_a5_deals"
)

// grantLevel builds an acl_role_actions grant with an EXPLICIT row-level access_level, unlike the
// harness grant() helper which defaults to "all". This is what lets each role drive a different A5
// row scope (team/all/owner/none) for the SAME entity+action.
func grantLevel(entity, action, level string) auth.RoleAction {
	return auth.RoleAction{EntityName: entity, Action: action, AccessLevel: level}
}

// a5SeedMembership inserts a team_memberships row (user ↔ team) directly. Memberships MUST exist
// BEFORE a user POSTs a record, because InsertRecord derives the record's team_set_id from the
// creator's memberships at insert time (A5 §B4). team_memberships has no tenant_id column; tenant
// scoping is via the teams/users rows it references.
func a5SeedMembership(t *testing.T, gdb *gorm.DB, userID, teamID string) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO team_memberships (id, user_id, team_id) VALUES (?, ?, ?)",
		uuid.NewString(), userID, teamID,
	).Error; err != nil {
		t.Fatalf("a5SeedMembership(user=%s,team=%s): %v", userID, teamID, err)
	}
}

// a5SeedTeam inserts a teams row in a tenant.
func a5SeedTeam(t *testing.T, gdb *gorm.DB, id, tenantID, name string) {
	t.Helper()
	if err := gdb.Exec(
		"INSERT INTO teams (id, tenant_id, name) VALUES (?, ?, ?)", id, tenantID, name,
	).Error; err != nil {
		t.Fatalf("a5SeedTeam(%s): %v", name, err)
	}
}

// a5CreatedBy reads the created_by column of a record straight from its obj_* table — the proof
// that the stored owner is the REAL authenticated user id (not a placeholder / not the admin who
// happened to register the entity).
func a5CreatedBy(t *testing.T, sx *sqlx.DB, recID string) string {
	t.Helper()
	var createdBy string
	if err := sx.Get(&createdBy, "SELECT created_by FROM `obj_"+rlsEntity+"` WHERE id = ?", recID); err != nil {
		t.Fatalf("a5CreatedBy(%s): %v", recID, err)
	}
	return createdBy
}

// a5TeamSetNull reports whether a record's team_set_id is NULL (creator was on no team).
func a5TeamSetNull(t *testing.T, sx *sqlx.DB, recID string) bool {
	t.Helper()
	var ts *string
	if err := sx.Get(&ts, "SELECT team_set_id FROM `obj_"+rlsEntity+"` WHERE id = ?", recID); err != nil {
		t.Fatalf("a5TeamSetNull(%s): %v", recID, err)
	}
	return ts == nil
}

// postRecordAs creates a record over HTTP as the given token and returns its id, failing the test on
// any non-201. The record's created_by/team_set_id are therefore decided by the AUTHENTICATED
// principal behind the token — exactly the production path.
func postRecordAs(t *testing.T, rt *a4Router, token, title string) string {
	t.Helper()
	w := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+rlsEntity, token, map[string]any{"title": title}, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST %s as token: got %d, want 201 (body=%s)", rlsEntity, w.Code, w.Body.String())
	}
	var env recordCreateEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode create envelope: %v (body=%s)", err, w.Body.String())
	}
	if env.Data.ID == "" {
		t.Fatalf("POST returned empty id (body=%s)", w.Body.String())
	}
	return env.Data.ID
}

// listIDsAs performs GET /:entity as the token and returns the set of returned record ids plus the
// reported pagination total. It fails on any non-200 (the ACL grant in these tests always permits
// view; row visibility is decided by the A5 filter, returning fewer/zero rows, NOT a 403).
func listIDsAs(t *testing.T, rt *a4Router, token string) (map[string]bool, int) {
	t.Helper()
	w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+rlsEntity, token, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s list: got %d, want 200 (body=%s)", rlsEntity, w.Code, w.Body.String())
	}
	var env struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Pagination struct {
			Total int `json:"total"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode list envelope: %v (body=%s)", err, w.Body.String())
	}
	ids := make(map[string]bool, len(env.Data))
	for _, r := range env.Data {
		ids[r.ID] = true
	}
	if len(ids) != env.Pagination.Total {
		t.Fatalf("list total %d != distinct ids %d (count/page scope mismatch; body=%s)",
			env.Pagination.Total, len(ids), w.Body.String())
	}
	return ids, env.Pagination.Total
}

// getStatusAs performs GET /:entity/:id as the token and returns just the HTTP status — used to
// assert a row OUTSIDE the caller's scope is 404 (RECORD_NOT_FOUND), indistinguishable from a
// non-existent id (no cross-team/cross-tenant leak), while an in-scope row is 200.
func getStatusAs(t *testing.T, rt *a4Router, token, id string) int {
	t.Helper()
	w := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+rlsEntity+"/"+id, token, nil, nil)
	return w.Code
}

// TestA5_TeamRLS_OverHTTP is the headline A5-qa case. ONE migrated DB, real logins, real
// memberships, records POSTed by the real users — then each access level is proven through the
// HTTP GET surface, plus tenant isolation and the created_by=real-user invariant.
//
// Tenant A layout:
//
//	teams: Alpha, Beta
//	users (all real logins):
//	  uAlpha   ∈ {Alpha}            role: create=all,  view=team   (creates recAlpha)
//	  uBeta    ∈ {Beta}             role: create=all,  view=team   (creates recBeta)
//	  uMulti   ∈ {Alpha, Beta}      role: create=all,  view=team
//	  uNoTeam  ∈ {}                 role: create=all,  view=team   (creates recNoTeam)
//	  uAll     ∈ {Alpha}            role: create=all,  view=all
//	  uOwner   ∈ {Alpha}            role: create=all,  view=owner  (creates recOwner)
//	  uNone    ∈ {Alpha}            role: create=all,  view=none
//	  admin                          is_admin (registers the entity, view=all bypass)
//
// Tenant B gets its own admin + record to prove cross-tenant isolation under access_level=all.
func TestA5_TeamRLS_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, rlsTenantAID, rlsTenantASlug, "Acme A5")
	a4SeedTenant(t, gdb, rlsTenantBID, rlsTenantBSlug, "Globex A5")

	teamAlpha := uuid.NewString()
	teamBeta := uuid.NewString()
	a5SeedTeam(t, gdb, teamAlpha, rlsTenantAID, "Alpha")
	a5SeedTeam(t, gdb, teamBeta, rlsTenantAID, "Beta")

	// Roles: all grant create=all (so every user can POST a record), differing only in their view
	// access_level — which is exactly what the A5 row filter consumes for reads.
	roleViewTeam := a4SeedRole(t, gdb, rlsTenantAID, "view-team", []auth.RoleAction{
		grantLevel(rlsEntity, "create", "all"),
		grantLevel(rlsEntity, "view", "team"),
		grantLevel(rlsEntity, "edit", "team"),
		grantLevel(rlsEntity, "delete", "team"),
	})
	roleViewAll := a4SeedRole(t, gdb, rlsTenantAID, "view-all", []auth.RoleAction{
		grantLevel(rlsEntity, "create", "all"),
		grantLevel(rlsEntity, "view", "all"),
	})
	roleViewOwner := a4SeedRole(t, gdb, rlsTenantAID, "view-owner", []auth.RoleAction{
		grantLevel(rlsEntity, "create", "all"),
		grantLevel(rlsEntity, "view", "owner"),
	})
	roleViewNone := a4SeedRole(t, gdb, rlsTenantAID, "view-none", []auth.RoleAction{
		grantLevel(rlsEntity, "create", "all"),
		grantLevel(rlsEntity, "view", "none"),
	})

	// Users (real logins). a4SeedUser returns the verified user id we later assert created_by against.
	uAlpha := a4SeedUser(t, gdb, rlsTenantAID, "alpha@"+rlsTenantASlug, false, &roleViewTeam)
	uBeta := a4SeedUser(t, gdb, rlsTenantAID, "beta@"+rlsTenantASlug, false, &roleViewTeam)
	uMulti := a4SeedUser(t, gdb, rlsTenantAID, "multi@"+rlsTenantASlug, false, &roleViewTeam)
	uNoTeam := a4SeedUser(t, gdb, rlsTenantAID, "noteam@"+rlsTenantASlug, false, &roleViewTeam)
	uAll := a4SeedUser(t, gdb, rlsTenantAID, "all@"+rlsTenantASlug, false, &roleViewAll)
	uOwner := a4SeedUser(t, gdb, rlsTenantAID, "owner@"+rlsTenantASlug, false, &roleViewOwner)
	uNone := a4SeedUser(t, gdb, rlsTenantAID, "none@"+rlsTenantASlug, false, &roleViewNone)
	a4SeedUser(t, gdb, rlsTenantAID, "admin@"+rlsTenantASlug, true, nil)
	a4SeedUser(t, gdb, rlsTenantBID, "admin@"+rlsTenantBSlug, true, nil)

	// Memberships BEFORE any record is created (team_set_id is derived at POST time).
	a5SeedMembership(t, gdb, uAlpha, teamAlpha)
	a5SeedMembership(t, gdb, uBeta, teamBeta)
	a5SeedMembership(t, gdb, uMulti, teamAlpha)
	a5SeedMembership(t, gdb, uMulti, teamBeta)
	a5SeedMembership(t, gdb, uAll, teamAlpha)
	a5SeedMembership(t, gdb, uOwner, teamAlpha)
	a5SeedMembership(t, gdb, uNone, teamAlpha)
	// uNoTeam intentionally has NO membership.

	a4DropEntity(sx, []string{rlsTenantAID, rlsTenantBID}, rlsEntity)
	t.Cleanup(func() {
		a4DropEntity(sx, []string{rlsTenantAID, rlsTenantBID}, rlsEntity)
		// Clean A5 join rows in FK-safe order (team_sets_teams cascades from teams, but be explicit).
		sx.Exec("DELETE FROM team_memberships WHERE team_id IN (?, ?)", teamAlpha, teamBeta)
		sx.Exec("DELETE st FROM team_sets_teams st JOIN team_sets s ON s.id = st.team_set_id WHERE s.tenant_id = ?", rlsTenantAID)
		sx.Exec("DELETE FROM team_sets WHERE tenant_id = ?", rlsTenantAID)
		sx.Exec("DELETE FROM teams WHERE tenant_id = ?", rlsTenantAID)
	})

	// Admins register the entity in each tenant so /:entity dispatches for every principal.
	adminATok := rt.login(t, rlsTenantASlug, "admin@"+rlsTenantASlug).Data.AccessToken
	adminBTok := rt.login(t, rlsTenantBSlug, "admin@"+rlsTenantBSlug).Data.AccessToken
	rt.createEntity(t, adminATok, rlsEntity)
	rt.createEntity(t, adminBTok, rlsEntity)

	// Real logins for the row-scoped principals.
	alphaTok := rt.login(t, rlsTenantASlug, "alpha@"+rlsTenantASlug).Data.AccessToken
	betaTok := rt.login(t, rlsTenantASlug, "beta@"+rlsTenantASlug).Data.AccessToken
	multiTok := rt.login(t, rlsTenantASlug, "multi@"+rlsTenantASlug).Data.AccessToken
	noTeamTok := rt.login(t, rlsTenantASlug, "noteam@"+rlsTenantASlug).Data.AccessToken
	allTok := rt.login(t, rlsTenantASlug, "all@"+rlsTenantASlug).Data.AccessToken
	ownerTok := rt.login(t, rlsTenantASlug, "owner@"+rlsTenantASlug).Data.AccessToken
	noneTok := rt.login(t, rlsTenantASlug, "none@"+rlsTenantASlug).Data.AccessToken

	// Records POSTed by the REAL users (created_by = their verified id; team_set_id from memberships).
	recAlpha := postRecordAs(t, rt, alphaTok, "alpha-deal")    // team_set {Alpha}
	recBeta := postRecordAs(t, rt, betaTok, "beta-deal")       // team_set {Beta}
	recNoTeam := postRecordAs(t, rt, noTeamTok, "noteam-deal") // team_set NULL
	recOwner := postRecordAs(t, rt, ownerTok, "owner-deal")    // team_set {Alpha} (uOwner ∈ Alpha)

	// Tenant B record (created by tenant-B admin) — must never be visible to a tenant-A caller.
	recTenantB := postRecordAs(t, rt, adminBTok, "tenantB-deal")

	// --- created_by is the REAL authenticated user, never a placeholder (task invariant). --------
	t.Run("created_by is the real authenticated user id", func(t *testing.T) {
		cases := []struct {
			rec  string
			want string
			who  string
		}{
			{recAlpha, uAlpha, "alpha"},
			{recBeta, uBeta, "beta"},
			{recNoTeam, uNoTeam, "noteam"},
			{recOwner, uOwner, "owner"},
		}
		for _, c := range cases {
			if got := a5CreatedBy(t, sx, c.rec); got != c.want {
				t.Errorf("created_by(%s) = %q; want the real %s user id %q", c.who, got, c.who, c.want)
			}
		}
		// And the no-team creator's record has a NULL team_set_id (owner-only visibility), proving
		// the team derivation, not a blanket assignment.
		if !a5TeamSetNull(t, sx, recNoTeam) {
			t.Errorf("recNoTeam team_set_id should be NULL (creator on no team)")
		}
		if a5TeamSetNull(t, sx, recAlpha) {
			t.Errorf("recAlpha team_set_id should be non-NULL (creator on team Alpha)")
		}
	})

	// --- access_level = team: own + same-team rows, NOT other-team users' rows. ------------------
	t.Run("team: single-team user sees own + same-team, not cross-team", func(t *testing.T) {
		// uAlpha ∈ Alpha. Sees recAlpha (own, Alpha set) and recOwner (Alpha set, created by uOwner).
		// Does NOT see recBeta (Beta) or recNoTeam (NULL set, different owner) or tenant B.
		ids, _ := listIDsAs(t, rt, alphaTok)
		if !ids[recAlpha] {
			t.Errorf("team(alpha): missing own record recAlpha")
		}
		if !ids[recOwner] {
			t.Errorf("team(alpha): missing same-team record recOwner (Alpha set, other owner)")
		}
		if ids[recBeta] {
			t.Errorf("team(alpha): leaked cross-team record recBeta")
		}
		if ids[recNoTeam] {
			t.Errorf("team(alpha): leaked other-user no-team record recNoTeam")
		}
		if ids[recTenantB] {
			t.Errorf("team(alpha): leaked tenant-B record")
		}
		// Direct GET mirrors the list scope: own/same-team 200, cross-team 404.
		if s := getStatusAs(t, rt, alphaTok, recOwner); s != http.StatusOK {
			t.Errorf("team(alpha) GET same-team recOwner: %d; want 200", s)
		}
		if s := getStatusAs(t, rt, alphaTok, recBeta); s != http.StatusNotFound {
			t.Errorf("team(alpha) GET cross-team recBeta: %d; want 404 (no cross-team leak)", s)
		}
	})

	t.Run("team: multi-team user sees both teams' rows", func(t *testing.T) {
		// uMulti ∈ {Alpha, Beta}: sees recAlpha + recOwner (Alpha set) AND recBeta (Beta set).
		ids, _ := listIDsAs(t, rt, multiTok)
		for _, id := range []string{recAlpha, recOwner, recBeta} {
			if !ids[id] {
				t.Errorf("team(multi): missing %s (multi-team union)", id)
			}
		}
		if ids[recNoTeam] {
			t.Errorf("team(multi): leaked no-team record recNoTeam")
		}
		if s := getStatusAs(t, rt, multiTok, recBeta); s != http.StatusOK {
			t.Errorf("team(multi) GET recBeta: %d; want 200 (member of Beta)", s)
		}
	})

	t.Run("team: beta user sees Beta not Alpha", func(t *testing.T) {
		ids, _ := listIDsAs(t, rt, betaTok)
		if !ids[recBeta] {
			t.Errorf("team(beta): missing own recBeta")
		}
		if ids[recAlpha] || ids[recOwner] {
			t.Errorf("team(beta): leaked Alpha-team records %v", ids)
		}
	})

	t.Run("team: no-team user sees only own rows", func(t *testing.T) {
		// uNoTeam has no membership → team branch matches nothing; only its own (NULL-set) record.
		ids, total := listIDsAs(t, rt, noTeamTok)
		if !ids[recNoTeam] || total != 1 {
			t.Errorf("team(noteam) got %v (total %d); want only recNoTeam", ids, total)
		}
	})

	// --- access_level = all: whole tenant, never another tenant. ---------------------------------
	t.Run("all: sees every tenant-A row, never tenant B", func(t *testing.T) {
		ids, _ := listIDsAs(t, rt, allTok)
		for _, id := range []string{recAlpha, recBeta, recNoTeam, recOwner} {
			if !ids[id] {
				t.Errorf("all: missing tenant-A record %s", id)
			}
		}
		if ids[recTenantB] {
			t.Errorf("all: leaked tenant-B record (tenant isolation broken)")
		}
		// Cross-team GET that team scope would 404 succeeds under all.
		if s := getStatusAs(t, rt, allTok, recBeta); s != http.StatusOK {
			t.Errorf("all GET recBeta: %d; want 200", s)
		}
		// But a tenant-B id is still 404 (tenant filter leads the query).
		if s := getStatusAs(t, rt, allTok, recTenantB); s != http.StatusNotFound {
			t.Errorf("all GET tenant-B id: %d; want 404 (cross-tenant invisible)", s)
		}
	})

	// --- access_level = owner: only the caller's own rows. ---------------------------------------
	t.Run("owner: sees only own rows", func(t *testing.T) {
		// uOwner created recOwner; though on team Alpha, owner scope ignores team — only own row.
		ids, total := listIDsAs(t, rt, ownerTok)
		if !ids[recOwner] || total != 1 {
			t.Errorf("owner got %v (total %d); want only recOwner", ids, total)
		}
		if s := getStatusAs(t, rt, ownerTok, recAlpha); s != http.StatusNotFound {
			t.Errorf("owner GET another-user same-team recAlpha: %d; want 404 (owner ignores team)", s)
		}
	})

	// --- access_level = none: sees NOTHING even though the ACL grant allows the action. ----------
	t.Run("none: sees nothing (fail-closed row filter)", func(t *testing.T) {
		ids, total := listIDsAs(t, rt, noneTok)
		if total != 0 || len(ids) != 0 {
			t.Errorf("none got %v (total %d); want empty (row filter fails closed)", ids, total)
		}
		// Even the caller's tenant-mates' rows are invisible by direct id.
		if s := getStatusAs(t, rt, noneTok, recAlpha); s != http.StatusNotFound {
			t.Errorf("none GET recAlpha: %d; want 404 (none sees no row)", s)
		}
	})
}

// TestA5_TeamHierarchy_OverHTTP (queue #20) proves the acceptance through real logins: a tree
// Vùng Bắc > Chi nhánh Hà Nội > PGD Cầu Giấy; a NON-admin access_level=team user who is a member
// of Vùng Bắc ONLY lists, reads and edits a record created by a PGD Cầu Giấy member; the creator
// still sees it afterwards; a user in a different branch never does.
func TestA5_TeamHierarchy_OverHTTP(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	const tenantID = "a5a50000-0000-0000-0000-0000000000c3"
	const slug = "a5-tree"
	a4SeedTenant(t, gdb, tenantID, slug, "Ngân hàng A5")

	region, branch, office, branch2 := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	a5SeedTeam(t, gdb, region, tenantID, "Vùng Bắc")
	a5SeedTeam(t, gdb, branch, tenantID, "Chi nhánh Hà Nội")
	a5SeedTeam(t, gdb, office, tenantID, "PGD Cầu Giấy")
	a5SeedTeam(t, gdb, branch2, tenantID, "Chi nhánh Hải Phòng")
	for child, parent := range map[string]string{branch: region, office: branch, branch2: region} {
		if err := gdb.Exec("UPDATE teams SET parent_id = ? WHERE id = ?", parent, child).Error; err != nil {
			t.Fatalf("set parent: %v", err)
		}
	}

	roleTeam := a4SeedRole(t, gdb, tenantID, "branch-rep", []auth.RoleAction{
		grantLevel(rlsEntity, "create", "all"),
		grantLevel(rlsEntity, "view", "team"),
		grantLevel(rlsEntity, "edit", "team"),
	})
	uRegion := a4SeedUser(t, gdb, tenantID, "region@"+slug, false, &roleTeam)
	uOffice := a4SeedUser(t, gdb, tenantID, "office@"+slug, false, &roleTeam)
	uBranch2 := a4SeedUser(t, gdb, tenantID, "branch2@"+slug, false, &roleTeam)
	a4SeedUser(t, gdb, tenantID, "admin@"+slug, true, nil)
	a5SeedMembership(t, gdb, uRegion, region)
	a5SeedMembership(t, gdb, uOffice, office)
	a5SeedMembership(t, gdb, uBranch2, branch2)

	a4DropEntity(sx, []string{tenantID}, rlsEntity)
	t.Cleanup(func() {
		a4DropEntity(sx, []string{tenantID}, rlsEntity)
		sx.Exec("DELETE FROM team_memberships WHERE team_id IN (?, ?, ?, ?)", region, branch, office, branch2)
		sx.Exec("DELETE st FROM team_sets_teams st JOIN team_sets s ON s.id = st.team_set_id WHERE s.tenant_id = ?", tenantID)
		sx.Exec("DELETE FROM team_sets WHERE tenant_id = ?", tenantID)
		sx.Exec("DELETE FROM teams WHERE tenant_id = ?", tenantID)
	})

	adminTok := rt.login(t, slug, "admin@"+slug).Data.AccessToken
	rt.createEntity(t, adminTok, rlsEntity)
	regionTok := rt.login(t, slug, "region@"+slug).Data.AccessToken
	officeTok := rt.login(t, slug, "office@"+slug).Data.AccessToken
	branch2Tok := rt.login(t, slug, "branch2@"+slug).Data.AccessToken

	recOffice := postRecordAs(t, rt, officeTok, "khách của PGD Cầu Giấy")

	// READ: the regional member (member of Vùng Bắc only) sees the office's record.
	ids, _ := listIDsAs(t, rt, regionTok)
	if !ids[recOffice] {
		t.Fatalf("region member does not see the office record in the list: %v", ids)
	}
	if s := getStatusAs(t, rt, regionTok, recOffice); s != http.StatusOK {
		t.Fatalf("region GET office record: %d; want 200", s)
	}
	// WRITE: same filter on PUT — the edit lands.
	w := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/"+rlsEntity+"/"+recOffice, regionTok,
		map[string]any{"title": "đã sửa bởi trưởng vùng"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("region PUT office record: %d %s", w.Code, w.Body.String())
	}
	// The creator still sees its own record afterwards.
	if s := getStatusAs(t, rt, officeTok, recOffice); s != http.StatusOK {
		t.Fatalf("office creator lost its record after the region edit: %d", s)
	}
	// A sibling branch never sees it — the closure only flows DOWN from the caller's teams.
	if ids2, _ := listIDsAs(t, rt, branch2Tok); ids2[recOffice] {
		t.Fatalf("sibling branch leaked the office record")
	}
	if s := getStatusAs(t, rt, branch2Tok, recOffice); s != http.StatusNotFound {
		t.Fatalf("sibling branch GET office record: %d; want 404", s)
	}
}
