//go:build integration

// A4 RBAC — ACL enforcement integration tests (coverage points T1–T4) against a REAL MySQL 8
// brought up with the golang-migrate migration 000005 (see a4_rbac_harness_integration_test.go).
// Run with:
//
//	go test -tags=integration ./internal/handlers/
//
// These drive the genuine production chain Auth → Tenant → ACL → handler (RegisterAPIRoutes with a
// real services.ACLService over auth.RoleRepository). A real login mints each principal's token, so
// the role_id/is_admin the ACL stage reads is exactly what a production login would carry — never a
// hand-forged claim.
//
//	T1 — table-driven decision: a role that LACKS a grant for the entity → 403; a role WITH the
//	     grant (exact entity name OR '*') → OK. All four actions (view/create/edit/delete ↔
//	     GET/POST/PUT/DELETE) are covered for both the exact-grant and wildcard-grant roles.
//	T2 — default-deny: a role with NO rule matching the (entity, action) is refused (the absence of
//	     a rule denies; ACL never falls open).
//	T3 — is_admin bypass: an is_admin=true principal passes every action on every entity regardless
//	     of any role grant.
//	T4 — tenant-scoped + forge-tenant: a tenant-B principal cannot ride a tenant-A role. A grant
//	     that exists only under tenant A does NOT authorize the same role_id presented from tenant B
//	     (the ACL query is tenant-pinned → cross-tenant role yields zero grants → deny).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

// A4 fixture identity (isolated from the auth-suite fixtures; this suite owns its migrated DB).
const (
	aclTenantAID   = "a4a40000-0000-0000-0000-0000000000a1"
	aclTenantBID   = "a4a40000-0000-0000-0000-0000000000b2"
	aclTenantASlug = "a4-acme"
	aclTenantBSlug = "a4-globex"
)

// aclEntity is the dynamic entity the ACL tests grant/deny access to.
const aclEntity = "it_a4_contacts"

// aclResult decodes the dynamic-record List envelope enough to confirm a granted GET reached the
// handler (200 with a data/pagination shape) rather than being short-circuited by the ACL stage.
type aclListEnvelope struct {
	Data       json.RawMessage `json:"data"`
	Pagination struct {
		Total int `json:"total"`
	} `json:"pagination"`
	Code string `json:"code"`
}

// recordCreateEnvelope decodes the {"data":{"id":...}} returned by a successful POST /:entity.
type recordCreateEnvelope struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

// TestA4_ACLDecisionTableDriven is T1+T2: a single migrated DB, three non-admin principals —
//   - "none"     role has NO grant on aclEntity            → every action 403 (T2 default-deny);
//   - "exact"    role is granted all 4 actions on aclEntity → every action OK (T1 exact match);
//   - "wildcard" role is granted all 4 actions on '*'       → every action OK (T1 wildcard match).
//
// The four HTTP methods map to the four RBAC actions (GET→view, POST→create, PUT→edit,
// DELETE→delete), so the matrix proves enforce-and-allow across the whole action set in one table.
func TestA4_ACLDecisionTableDriven(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, aclTenantAID, aclTenantASlug, "Acme A4")

	// Admin (to create the entity) + three role-scoped members in tenant A.
	a4SeedUser(t, gdb, aclTenantAID, "admin@"+aclTenantASlug, true, nil)

	noneRole := a4SeedRole(t, gdb, aclTenantAID, "none-role", nil) // no grants at all
	exactRole := a4SeedRole(t, gdb, aclTenantAID, "exact-role", []auth.RoleAction{
		grant(aclEntity, "view"), grant(aclEntity, "create"), grant(aclEntity, "edit"), grant(aclEntity, "delete"),
	})
	wildRole := a4SeedRole(t, gdb, aclTenantAID, "wild-role", []auth.RoleAction{
		grant("*", "view"), grant("*", "create"), grant("*", "edit"), grant("*", "delete"),
	})

	a4SeedUser(t, gdb, aclTenantAID, "none@"+aclTenantASlug, false, &noneRole)
	a4SeedUser(t, gdb, aclTenantAID, "exact@"+aclTenantASlug, false, &exactRole)
	a4SeedUser(t, gdb, aclTenantAID, "wild@"+aclTenantASlug, false, &wildRole)

	a4DropEntity(sx, []string{aclTenantAID}, aclEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{aclTenantAID}, aclEntity) })

	// Create the entity (+obj_ table) as the admin so /:entity dispatches for the member tokens.
	adminTok := rt.login(t, aclTenantASlug, "admin@"+aclTenantASlug).Data.AccessToken
	rt.createEntity(t, adminTok, aclEntity)

	noneTok := rt.login(t, aclTenantASlug, "none@"+aclTenantASlug).Data.AccessToken
	exactTok := rt.login(t, aclTenantASlug, "exact@"+aclTenantASlug).Data.AccessToken
	wildTok := rt.login(t, aclTenantASlug, "wild@"+aclTenantASlug).Data.AccessToken

	// Seed one record (as admin, bypassing ACL) so the granted GET/PUT/DELETE have a target id and
	// the allow path returns a real 2xx rather than a 404 we'd have to disambiguate from a 403.
	wSeed := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+aclEntity, adminTok, map[string]any{"title": "seed"}, nil)
	if wSeed.Code != http.StatusCreated {
		t.Fatalf("seed record: %d %s", wSeed.Code, wSeed.Body.String())
	}
	var seeded recordCreateEnvelope
	_ = json.Unmarshal(wSeed.Body.Bytes(), &seeded)
	recID := seeded.Data.ID

	// Each row is one (method, path) probe. wantDeniedForNone is always true (T2). The granted
	// roles (exact/wildcard) must be allowed (not 403); we assert a non-403 success-ish status.
	type probe struct {
		name   string
		method string
		path   string
		body   any
	}
	probes := []probe{
		{"view (GET list)", http.MethodGet, "/api/v1/" + aclEntity, nil},
		{"create (POST)", http.MethodPost, "/api/v1/" + aclEntity, map[string]any{"title": "x"}},
		{"edit (PUT)", http.MethodPut, "/api/v1/" + aclEntity + "/" + recID, map[string]any{"title": "y"}},
		{"delete (DELETE)", http.MethodDelete, "/api/v1/" + aclEntity + "/" + recID, nil},
	}

	for _, p := range probes {
		// T2 — the no-grant role is DENIED on every action with the ACL 403.
		t.Run("none-denied/"+p.name, func(t *testing.T) {
			w := doAuthed(t, rt.engine, p.method, p.path, noneTok, p.body, nil)
			if w.Code != http.StatusForbidden {
				t.Fatalf("no-grant role got %d on %s; want 403 (default-deny)", w.Code, p.name)
			}
			var env aclListEnvelope
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != "FORBIDDEN" {
				t.Errorf("no-grant 403 body code = %q; want FORBIDDEN", env.Code)
			}
		})
	}

	// T1 — exact-grant and wildcard-grant roles are ALLOWED on every action (never the ACL 403).
	// Re-seed the record before the edit/delete probes for each granted role so the ids exist.
	for _, gr := range []struct {
		who string
		tok string
	}{{"exact", exactTok}, {"wildcard", wildTok}} {
		gr := gr
		// fresh record for this role's edit/delete
		wr := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+aclEntity, adminTok, map[string]any{"title": "for-" + gr.who}, nil)
		var rec recordCreateEnvelope
		_ = json.Unmarshal(wr.Body.Bytes(), &rec)
		id := rec.Data.ID

		allow := []struct {
			name   string
			method string
			path   string
			body   any
			want   int
		}{
			{"view", http.MethodGet, "/api/v1/" + aclEntity, nil, http.StatusOK},
			{"create", http.MethodPost, "/api/v1/" + aclEntity, map[string]any{"title": "ok"}, http.StatusCreated},
			{"edit", http.MethodPut, "/api/v1/" + aclEntity + "/" + id, map[string]any{"title": "ok2"}, http.StatusOK},
			{"delete", http.MethodDelete, "/api/v1/" + aclEntity + "/" + id, nil, http.StatusNoContent},
		}
		for _, a := range allow {
			t.Run(gr.who+"-allowed/"+a.name, func(t *testing.T) {
				w := doAuthed(t, rt.engine, a.method, a.path, gr.tok, a.body, nil)
				if w.Code == http.StatusForbidden {
					t.Fatalf("%s role got 403 on %s; ACL must ALLOW a granted action (body=%s)", gr.who, a.name, w.Body.String())
				}
				if w.Code != a.want {
					t.Fatalf("%s role %s: got %d, want %d (body=%s)", gr.who, a.name, w.Code, a.want, w.Body.String())
				}
			})
		}
	}
}

// TestA4_AdminBypassesACL is T3: an is_admin=true principal with NO role at all passes every
// (method→action) on the entity. Admin is the super-gate — the ACL stage is skipped entirely.
func TestA4_AdminBypassesACL(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, aclTenantAID, aclTenantASlug, "Acme A4")
	a4SeedUser(t, gdb, aclTenantAID, "admin@"+aclTenantASlug, true, nil) // admin, role_id NULL

	a4DropEntity(sx, []string{aclTenantAID}, aclEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{aclTenantAID}, aclEntity) })

	adminTok := rt.login(t, aclTenantASlug, "admin@"+aclTenantASlug).Data.AccessToken
	rt.createEntity(t, adminTok, aclEntity)

	// Create → list → edit → delete, all as the admin: none of these may be 403, despite the admin
	// holding no acl_roles grant whatsoever (D3 bypass).
	wc := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+aclEntity, adminTok, map[string]any{"title": "a"}, nil)
	if wc.Code != http.StatusCreated {
		t.Fatalf("admin create: %d %s (want 201, ACL must be bypassed)", wc.Code, wc.Body.String())
	}
	var rec recordCreateEnvelope
	_ = json.Unmarshal(wc.Body.Bytes(), &rec)

	for _, p := range []struct {
		name   string
		method string
		path   string
		body   any
		want   int
	}{
		{"list", http.MethodGet, "/api/v1/" + aclEntity, nil, http.StatusOK},
		{"edit", http.MethodPut, "/api/v1/" + aclEntity + "/" + rec.Data.ID, map[string]any{"title": "b"}, http.StatusOK},
		{"delete", http.MethodDelete, "/api/v1/" + aclEntity + "/" + rec.Data.ID, nil, http.StatusNoContent},
	} {
		w := doAuthed(t, rt.engine, p.method, p.path, adminTok, p.body, nil)
		if w.Code == http.StatusForbidden {
			t.Fatalf("admin %s got 403; is_admin must bypass ACL", p.name)
		}
		if w.Code != p.want {
			t.Errorf("admin %s: got %d, want %d (body=%s)", p.name, w.Code, p.want, w.Body.String())
		}
	}
}

// TestA4_ForgeTenantCannotRideForeignRole is T4: the ACL decision is tenant-pinned. We seed a role
// with a granting rule under tenant A, then present a tenant-B principal whose users.role_id points
// at THAT SAME role id. Because the ACL query joins acl_roles ON tenant_id, the tenant-B request
// loads ZERO grants for the foreign role → default-deny (403). A tenant cannot escalate by borrowing
// another tenant's role id.
func TestA4_ForgeTenantCannotRideForeignRole(t *testing.T) {
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, aclTenantAID, aclTenantASlug, "Acme A4")
	a4SeedTenant(t, gdb, aclTenantBID, aclTenantBSlug, "Globex A4")

	// Role lives in tenant A and grants view+create on the entity.
	roleA := a4SeedRole(t, gdb, aclTenantAID, "shared-name", []auth.RoleAction{
		grant(aclEntity, "view"), grant(aclEntity, "create"),
	})

	// Tenant A member legitimately holds roleA. Tenant B member is given the SAME role id (the
	// forge: a B user pointing at an A role) — the ACL query must refuse to honor it.
	a4SeedUser(t, gdb, aclTenantAID, "amember@"+aclTenantASlug, false, &roleA)
	a4SeedUser(t, gdb, aclTenantBID, "bforger@"+aclTenantBSlug, false, &roleA)
	a4SeedUser(t, gdb, aclTenantAID, "aadmin@"+aclTenantASlug, true, nil)
	a4SeedUser(t, gdb, aclTenantBID, "badmin@"+aclTenantBSlug, true, nil)

	a4DropEntity(sx, []string{aclTenantAID, aclTenantBID}, aclEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{aclTenantAID, aclTenantBID}, aclEntity) })

	// Register the entity in BOTH tenants (each admin registers it under its own tenant) so the
	// /:entity route dispatches for either principal and the difference is purely the ACL decision.
	aAdminTok := rt.login(t, aclTenantASlug, "aadmin@"+aclTenantASlug).Data.AccessToken
	bAdminTok := rt.login(t, aclTenantBSlug, "badmin@"+aclTenantBSlug).Data.AccessToken
	rt.createEntity(t, aAdminTok, aclEntity)
	rt.createEntity(t, bAdminTok, aclEntity)

	aMemberTok := rt.login(t, aclTenantASlug, "amember@"+aclTenantASlug).Data.AccessToken
	bForgerTok := rt.login(t, aclTenantBSlug, "bforger@"+aclTenantBSlug).Data.AccessToken

	// Tenant A member (legitimate grant) → ALLOWED to create under tenant A.
	wA := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+aclEntity, aMemberTok, map[string]any{"title": "A-ok"}, nil)
	if wA.Code != http.StatusCreated {
		t.Fatalf("tenant-A member with legit grant got %d; want 201 (body=%s)", wA.Code, wA.Body.String())
	}

	// Tenant B forger (same role id, wrong tenant) → DENIED. The grant under tenant A must NOT apply
	// to a tenant-B request: the tenant-pinned ACL query returns no grants → 403.
	wB := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+aclEntity, bForgerTok, map[string]any{"title": "B-forge"}, nil)
	if wB.Code != http.StatusForbidden {
		t.Fatalf("tenant-B forger riding tenant-A role got %d; want 403 (forge-tenant must NOT cross the ACL boundary; body=%s)", wB.Code, wB.Body.String())
	}
	var env aclListEnvelope
	_ = json.Unmarshal(wB.Body.Bytes(), &env)
	if env.Code != "FORBIDDEN" {
		t.Errorf("forge-tenant deny body code = %q; want FORBIDDEN", env.Code)
	}

	// Also verify the read side: the B forger cannot view via the borrowed grant either.
	wBList := doAuthed(t, rt.engine, http.MethodGet, "/api/v1/"+aclEntity, bForgerTok, nil, nil)
	if wBList.Code != http.StatusForbidden {
		t.Errorf("tenant-B forger GET got %d; want 403 (no cross-tenant view via borrowed role)", wBList.Code)
	}
}
