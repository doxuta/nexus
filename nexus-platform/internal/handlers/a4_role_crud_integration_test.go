//go:build integration

// A4 RBAC — Role CRUD integration tests (coverage point T5) against a REAL MySQL 8 brought up with
// golang-migrate (see a4_rbac_harness_integration_test.go). Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// Covers /admin/roles end to end through the production chain:
//   - all four methods (GET list, GET/:id, POST, PUT, DELETE) round-trip against the migrated DB;
//   - tenant-scoped: a role created under tenant A is invisible / unmodifiable from tenant B
//     (list omits it, get/update/delete by its id → 404 under B);
//   - admin-gated: a non-admin principal hitting any /admin/roles method → 403 (RequireAdmin);
//   - delete-with-user (guard R3): deleting a role currently held by a user succeeds and the FK
//     ON DELETE SET NULL cleanly nulls users.role_id (the orphaned user keeps its row, no 500).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

const (
	roleTenantAID   = "a4r00000-0000-0000-0000-0000000000a1"
	roleTenantBID   = "a4r00000-0000-0000-0000-0000000000b2"
	roleTenantASlug = "a4r-acme"
	roleTenantBSlug = "a4r-globex"
)

// roleEnvelope decodes {"data":{"role":{id,name,label,actions:[...]}}}.
type roleEnvelope struct {
	Data struct {
		Role struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Label   string `json:"label"`
			Actions []struct {
				EntityName  string `json:"entityName"`
				Action      string `json:"action"`
				AccessLevel string `json:"accessLevel"`
			} `json:"actions"`
		} `json:"role"`
	} `json:"data"`
	Code string `json:"code"`
}

// roleListEnvelope decodes {"data":{"roles":[{id,name,...}]}}.
type roleListEnvelope struct {
	Data struct {
		Roles []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"roles"`
	} `json:"data"`
}

// TestA4_RoleCRUDFullCycle is T5 happy path: create → get → list → update (replace grants) →
// delete, all four methods against the migrated DB as a tenant-A admin.
func TestA4_RoleCRUDFullCycle(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, roleTenantAID, roleTenantASlug, "Acme Roles")
	a4SeedUser(t, gdb, roleTenantAID, "admin@"+roleTenantASlug, true, nil)
	adminTok := rt.login(t, roleTenantASlug, "admin@"+roleTenantASlug).Data.AccessToken

	// CREATE (POST) — a role with two grants.
	createBody := map[string]any{
		"name":  "sales",
		"label": "Sales",
		"actions": []map[string]any{
			{"entityName": "it_a4_contacts", "action": "view"},
			{"entityName": "it_a4_contacts", "action": "create"},
		},
	}
	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/roles", adminTok, createBody, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", wc.Code, wc.Body.String())
	}
	var created roleEnvelope
	if err := json.Unmarshal(wc.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	roleID := created.Data.Role.ID
	if roleID == "" || created.Data.Role.Name != "sales" || len(created.Data.Role.Actions) != 2 {
		t.Fatalf("create returned unexpected role: %+v", created.Data.Role)
	}

	// GET /:id — the role with its grants.
	wg := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/roles/"+roleID, adminTok, nil, nil)
	if wg.Code != http.StatusOK {
		t.Fatalf("get role: %d %s", wg.Code, wg.Body.String())
	}
	var got roleEnvelope
	_ = json.Unmarshal(wg.Body.Bytes(), &got)
	if got.Data.Role.ID != roleID || len(got.Data.Role.Actions) != 2 {
		t.Fatalf("get returned %+v; want id=%s with 2 actions", got.Data.Role, roleID)
	}

	// GET list — the new role appears.
	wl := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/roles", adminTok, nil, nil)
	if wl.Code != http.StatusOK {
		t.Fatalf("list roles: %d %s", wl.Code, wl.Body.String())
	}
	var list roleListEnvelope
	_ = json.Unmarshal(wl.Body.Bytes(), &list)
	if !roleListContains(list, roleID) {
		t.Fatalf("list did not contain created role %s: %+v", roleID, list.Data.Roles)
	}

	// PUT /:id — rename + REPLACE grants (now a single wildcard view grant).
	updateBody := map[string]any{
		"label": "Sales Team",
		"actions": []map[string]any{
			{"entityName": "*", "action": "view"},
		},
	}
	wu := doAuthed(t, rt.engine, http.MethodPut, "/api/v1/admin/roles/"+roleID, adminTok, updateBody, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("update role: %d %s", wu.Code, wu.Body.String())
	}
	var updated roleEnvelope
	_ = json.Unmarshal(wu.Body.Bytes(), &updated)
	if updated.Data.Role.Label != "Sales Team" || len(updated.Data.Role.Actions) != 1 ||
		updated.Data.Role.Actions[0].EntityName != "*" {
		t.Fatalf("update did not replace grants/label: %+v", updated.Data.Role)
	}

	// DELETE /:id — removed; a subsequent GET is 404.
	wd := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/roles/"+roleID, adminTok, nil, nil)
	if wd.Code != http.StatusNoContent {
		t.Fatalf("delete role: %d %s; want 204", wd.Code, wd.Body.String())
	}
	wg2 := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/roles/"+roleID, adminTok, nil, nil)
	if wg2.Code != http.StatusNotFound {
		t.Fatalf("get after delete: %d; want 404", wg2.Code)
	}
}

// TestA4_RoleTenantScoped is T5 isolation: a role created by tenant A is invisible and immutable
// from tenant B. B's list omits it, and get/update/delete by its id under B all return 404 — the
// id is never honored across the tenant boundary.
func TestA4_RoleTenantScoped(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, roleTenantAID, roleTenantASlug, "Acme Roles")
	a4SeedTenant(t, gdb, roleTenantBID, roleTenantBSlug, "Globex Roles")
	a4SeedUser(t, gdb, roleTenantAID, "admin@"+roleTenantASlug, true, nil)
	a4SeedUser(t, gdb, roleTenantBID, "admin@"+roleTenantBSlug, true, nil)

	aTok := rt.login(t, roleTenantASlug, "admin@"+roleTenantASlug).Data.AccessToken
	bTok := rt.login(t, roleTenantBSlug, "admin@"+roleTenantBSlug).Data.AccessToken

	// A creates a role.
	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/roles", aTok,
		map[string]any{"name": "a-only", "label": "A Only"}, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("A create role: %d %s", wc.Code, wc.Body.String())
	}
	var created roleEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &created)
	aRoleID := created.Data.Role.ID

	// B's list must NOT contain A's role.
	wl := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/roles", bTok, nil, nil)
	var bList roleListEnvelope
	_ = json.Unmarshal(wl.Body.Bytes(), &bList)
	if roleListContains(bList, aRoleID) {
		t.Fatalf("tenant B sees tenant A's role %s in its list: %+v", aRoleID, bList.Data.Roles)
	}

	// B get/update/delete by A's id → 404 (cross-tenant id not honored).
	for _, tc := range []struct {
		name   string
		method string
		body   any
	}{
		{"get", http.MethodGet, nil},
		{"update", http.MethodPut, map[string]any{"label": "hijack"}},
		{"delete", http.MethodDelete, nil},
	} {
		w := doAuthed(t, rt.engine, tc.method, "/api/v1/admin/roles/"+aRoleID, bTok, tc.body, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("B %s A's role: got %d; want 404 (cross-tenant isolation)", tc.name, w.Code)
		}
	}

	// Sanity: A can still get its own role (not collateral-damaged by B's probes).
	wa := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/admin/roles/"+aRoleID, aTok, nil, nil)
	if wa.Code != http.StatusOK {
		t.Errorf("A get own role after B probes: %d; want 200", wa.Code)
	}
}

// TestA4_RoleAdminGated is T5 admin-gate: a NON-admin principal is refused (403) on every
// /admin/roles method by RequireAdmin, before the role handler runs.
func TestA4_RoleAdminGated(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, roleTenantAID, roleTenantASlug, "Acme Roles")
	// A non-admin member (even with a role) must not reach /admin/roles. The role's grants are
	// irrelevant here — the gate refuses before the handler — so seed it with a single grant.
	memberRole := a4SeedRole(t, gdb, roleTenantAID, "member", []auth.RoleAction{grant("anything", "view")})
	a4SeedUser(t, gdb, roleTenantAID, "member@"+roleTenantASlug, false, &memberRole)
	memberTok := rt.login(t, roleTenantASlug, "member@"+roleTenantASlug).Data.AccessToken

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"list", http.MethodGet, "/api/v1/admin/roles", nil},
		{"get", http.MethodGet, "/api/v1/admin/roles/some-id", nil},
		{"create", http.MethodPost, "/api/v1/admin/roles", map[string]any{"name": "x"}},
		{"update", http.MethodPut, "/api/v1/admin/roles/some-id", map[string]any{"label": "y"}},
		{"delete", http.MethodDelete, "/api/v1/admin/roles/some-id", nil},
	} {
		w := doAuthed(t, rt.engine, tc.method, tc.path, memberTok, tc.body, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("non-admin %s /admin/roles: got %d; want 403 (admin-gate)", tc.name, w.Code)
		}
	}
}

// TestA4_DeleteRoleHeldByUserNullsRoleID is T5 + R3 guard: deleting a role that a user currently
// holds succeeds (200), and the FK users.role_id ON DELETE SET NULL cleanly detaches the user —
// users.role_id becomes NULL, the user row survives, and no 500 is raised. This proves the
// migration's ON DELETE SET NULL is real (it is exercised through the live DELETE, not AutoMigrate).
func TestA4_DeleteRoleHeldByUserNullsRoleID(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, roleTenantAID, roleTenantASlug, "Acme Roles")
	a4SeedUser(t, gdb, roleTenantAID, "admin@"+roleTenantASlug, true, nil)
	adminTok := rt.login(t, roleTenantASlug, "admin@"+roleTenantASlug).Data.AccessToken

	// Create a role and a member who holds it.
	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/admin/roles", adminTok,
		map[string]any{"name": "to-delete", "label": "Doomed"}, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("create role: %d %s", wc.Code, wc.Body.String())
	}
	var created roleEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &created)
	roleID := created.Data.Role.ID

	heldUserID := a4SeedUser(t, gdb, roleTenantAID, "holder@"+roleTenantASlug, false, &roleID)

	// Confirm the user actually references the role pre-delete.
	var before struct{ RoleID *string }
	if err := sx.Get(&before, "SELECT role_id AS roleid FROM users WHERE id = ?", heldUserID); err != nil {
		t.Fatalf("read user role_id pre-delete: %v", err)
	}
	if before.RoleID == nil || *before.RoleID != roleID {
		t.Fatalf("pre-delete user.role_id = %v; want %s", before.RoleID, roleID)
	}

	// DELETE the role (held by a user) → 200, NOT a 500.
	wd := doAuthed(t, rt.engine, http.MethodDelete, "/api/v1/admin/roles/"+roleID, adminTok, nil, nil)
	if wd.Code != http.StatusNoContent {
		t.Fatalf("delete role held by user: %d %s; want 204 (FK SET NULL, no 500)", wd.Code, wd.Body.String())
	}

	// The user row survives and its role_id is now NULL (orphan cleanly detached).
	var after struct {
		ID     string  `db:"id"`
		RoleID *string `db:"role_id"`
	}
	if err := sx.Get(&after, "SELECT id, role_id FROM users WHERE id = ?", heldUserID); err != nil {
		t.Fatalf("read user post-delete (row must survive): %v", err)
	}
	if after.RoleID != nil {
		t.Errorf("user.role_id after role delete = %v; want NULL (ON DELETE SET NULL)", *after.RoleID)
	}

	// The role's grant rows are gone too (acl_role_actions ON DELETE CASCADE) — no orphans.
	var orphanGrants int
	if err := sx.Get(&orphanGrants, "SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?", roleID); err != nil {
		t.Fatalf("count grants post-delete: %v", err)
	}
	if orphanGrants != 0 {
		t.Errorf("acl_role_actions left %d orphan grants for deleted role; want 0 (CASCADE)", orphanGrants)
	}
}

// roleListContains reports whether the list envelope holds a role with the given id.
func roleListContains(l roleListEnvelope, id string) bool {
	for _, r := range l.Data.Roles {
		if r.ID == id {
			return true
		}
	}
	return false
}
