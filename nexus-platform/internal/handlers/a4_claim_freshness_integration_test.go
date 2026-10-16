//go:build integration

// Queue #14 claim-freshness — integration proof against a REAL MySQL 8 brought up with
// golang-migrate (the A4 harness, a4_rbac_harness_integration_test.go). Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -p 1 -run 'TestA4_ClaimFreshness' ./internal/handlers/
//
// WHAT IS PROVEN (end to end, production router with WithLiveSubject, real login, real tokens):
// a non-admin holding a LIVE access token (no logout, no expiry, no /auth/refresh) is affected by
// an admin's user mutation on their VERY NEXT request —
//
//   - role downgrade (editor → viewer): the next PUT is 403, the next GET still 200;
//   - disable: the next request is 401, and the FE's fallback /auth/refresh is 401 too (W1);
//   - delete: the next request is 401;
//   - admin demotion: the next /admin/* request is 403;
//
// plus the lock-out guard: a normal login → protected route → /admin route still works with the
// live stage mounted (the same router every other A4 suite now runs on).
package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

const (
	cfTenantID   = "a4cf0000-0000-0000-0000-0000000000c4"
	cfTenantSlug = "a4cf-acme"
	cfEntity     = "it_cf_deals"
)

// cfFixture is one migrated DB with: an admin, an EDITOR role (view+edit on cfEntity), a VIEWER
// role (view only), a member holding the editor role, the entity, and one seeded record.
type cfFixture struct {
	rt         *a4Router
	adminTok   string
	memberID   string
	viewerRole string
	recID      string
}

func cfSetup(t *testing.T) *cfFixture {
	t.Helper()
	gdb, sx := a4SetupMigratedDB(t)
	rt := newA4Router(t, gdb, sx)

	a4SeedTenant(t, gdb, cfTenantID, cfTenantSlug, "Acme CF")
	a4SeedUser(t, gdb, cfTenantID, "admin@"+cfTenantSlug, true, nil)
	editor := a4SeedRole(t, gdb, cfTenantID, "cf-editor", []auth.RoleAction{grant(cfEntity, "view"), grant(cfEntity, "edit")})
	viewer := a4SeedRole(t, gdb, cfTenantID, "cf-viewer", []auth.RoleAction{grant(cfEntity, "view")})
	memberID := a4SeedUser(t, gdb, cfTenantID, "member@"+cfTenantSlug, false, &editor)

	a4DropEntity(sx, []string{cfTenantID}, cfEntity)
	t.Cleanup(func() { a4DropEntity(sx, []string{cfTenantID}, cfEntity) })

	adminTok := rt.login(t, cfTenantSlug, "admin@"+cfTenantSlug).Data.AccessToken
	rt.createEntity(t, adminTok, cfEntity)
	wSeed := doAuthed(t, rt.engine, http.MethodPost, "/api/v1/"+cfEntity, adminTok, map[string]any{"title": "seed"}, nil)
	if wSeed.Code != http.StatusCreated {
		t.Fatalf("seed record: %d %s", wSeed.Code, wSeed.Body.String())
	}
	var seeded recordCreateEnvelope
	_ = json.Unmarshal(wSeed.Body.Bytes(), &seeded)

	return &cfFixture{rt: rt, adminTok: adminTok, memberID: memberID, viewerRole: viewer, recID: seeded.Data.ID}
}

// cfCode decodes the machine-readable `code` of an error envelope ("" on success bodies).
func cfCode(body []byte) string {
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Code
}

// Role downgrade lands on the very next click: PUT was 200 under editor, is 403 under viewer with
// the SAME access token, while GET (still granted) stays 200.
func TestA4_ClaimFreshness_RoleDowngradeIsNextRequest403(t *testing.T) {
	f := cfSetup(t)
	memberTok := f.rt.login(t, cfTenantSlug, "member@"+cfTenantSlug).Data.AccessToken

	before := doAuthed(t, f.rt.engine, http.MethodPut, "/api/v1/"+cfEntity+"/"+f.recID, memberTok, map[string]any{"title": "edited by editor"}, nil)
	if before.Code != http.StatusOK {
		t.Fatalf("editor PUT before downgrade = %d %s; want 200", before.Code, before.Body.String())
	}

	wu := doAuthed(t, f.rt.engine, http.MethodPut, "/api/v1/admin/users/"+f.memberID, f.adminTok, map[string]any{"roleId": f.viewerRole}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("admin downgrade role: %d %s", wu.Code, wu.Body.String())
	}

	// SAME token, no refresh, no re-login — the next edit must be refused.
	after := doAuthed(t, f.rt.engine, http.MethodPut, "/api/v1/"+cfEntity+"/"+f.recID, memberTok, map[string]any{"title": "edited after downgrade"}, nil)
	if after.Code != http.StatusForbidden || cfCode(after.Body.Bytes()) != "FORBIDDEN" {
		t.Fatalf("PUT after downgrade = %d %s; want 403 FORBIDDEN on the very next request", after.Code, after.Body.String())
	}
	view := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/"+cfEntity+"/"+f.recID, memberTok, nil, nil)
	if view.Code != http.StatusOK {
		t.Fatalf("GET after downgrade = %d %s; want 200 (viewer keeps view)", view.Code, view.Body.String())
	}
}

// Disable lands on the very next click: the live token is refused with the uniform 401, and the
// FE's automatic /auth/refresh fallback is refused too (W1 revoke) → the user lands on /login.
func TestA4_ClaimFreshness_DisableIsNextRequest401(t *testing.T) {
	f := cfSetup(t)
	member := f.rt.login(t, cfTenantSlug, "member@"+cfTenantSlug)

	before := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/"+cfEntity, member.Data.AccessToken, nil, nil)
	if before.Code != http.StatusOK {
		t.Fatalf("member GET before disable = %d %s; want 200", before.Code, before.Body.String())
	}

	wu := doAuthed(t, f.rt.engine, http.MethodPut, "/api/v1/admin/users/"+f.memberID, f.adminTok, map[string]any{"status": "disabled"}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("admin disable: %d %s", wu.Code, wu.Body.String())
	}

	after := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/"+cfEntity, member.Data.AccessToken, nil, nil)
	if after.Code != http.StatusUnauthorized || cfCode(after.Body.Bytes()) != "UNAUTHORIZED" {
		t.Fatalf("GET after disable = %d %s; want 401 UNAUTHORIZED on the very next request", after.Code, after.Body.String())
	}
	if ok, code := w1RefreshStillWorks(t, f.rt, member.Data.RefreshToken); ok {
		t.Fatalf("refresh after disable still worked (code %d); the FE fallback must fail so the user is sent to /login", code)
	}
}

// Delete lands on the very next click: the soft-deleted row is gone to the auth stage → 401.
func TestA4_ClaimFreshness_DeleteIsNextRequest401(t *testing.T) {
	f := cfSetup(t)
	memberTok := f.rt.login(t, cfTenantSlug, "member@"+cfTenantSlug).Data.AccessToken

	before := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/"+cfEntity, memberTok, nil, nil)
	if before.Code != http.StatusOK {
		t.Fatalf("member GET before delete = %d %s; want 200", before.Code, before.Body.String())
	}
	wd := doAuthed(t, f.rt.engine, http.MethodDelete, "/api/v1/admin/users/"+f.memberID, f.adminTok, nil, nil)
	if wd.Code != http.StatusNoContent {
		t.Fatalf("admin delete member: %d %s; want 204", wd.Code, wd.Body.String())
	}
	after := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/"+cfEntity, memberTok, nil, nil)
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("GET after delete = %d %s; want 401 on the very next request", after.Code, after.Body.String())
	}
}

// Admin demotion lands on the very next click on /admin/*: the demoted admin's live token is
// refused by RequireAdmin (403) because is_admin now comes from the row, not the token.
func TestA4_ClaimFreshness_DemoteAdminIsNextRequest403OnAdminRoute(t *testing.T) {
	f := cfSetup(t)
	gdb := f.rt.gdb
	demoteID := a4SeedUser(t, gdb, cfTenantID, "demote@"+cfTenantSlug, true, nil)
	demoteTok := f.rt.login(t, cfTenantSlug, "demote@"+cfTenantSlug).Data.AccessToken

	before := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/admin/users", demoteTok, nil, nil)
	if before.Code != http.StatusOK {
		t.Fatalf("second admin GET /admin/users before demotion = %d %s; want 200", before.Code, before.Body.String())
	}
	wu := doAuthed(t, f.rt.engine, http.MethodPut, "/api/v1/admin/users/"+demoteID, f.adminTok, map[string]any{"isAdmin": false}, nil)
	if wu.Code != http.StatusOK {
		t.Fatalf("demote: %d %s", wu.Code, wu.Body.String())
	}
	after := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/admin/users", demoteTok, nil, nil)
	if after.Code != http.StatusForbidden || cfCode(after.Body.Bytes()) != "FORBIDDEN" {
		t.Fatalf("GET /admin/users after demotion = %d %s; want 403 FORBIDDEN on the very next request", after.Code, after.Body.String())
	}
}

// Lock-out guard: with the live stage mounted, an ordinary login still reaches a protected
// route (member) and the admin surface (admin) — nobody is locked out by the re-read itself.
func TestA4_ClaimFreshness_NormalLoginStillWorks(t *testing.T) {
	f := cfSetup(t)
	memberTok := f.rt.login(t, cfTenantSlug, "member@"+cfTenantSlug).Data.AccessToken

	if w := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/"+cfEntity, memberTok, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("member protected GET = %d %s; want 200", w.Code, w.Body.String())
	}
	if w := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/auth/me", memberTok, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("member /auth/me = %d %s; want 200", w.Code, w.Body.String())
	}
	if w := doAuthed(t, f.rt.engine, http.MethodGet, "/api/v1/admin/users", f.adminTok, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("admin GET /admin/users = %d %s; want 200", w.Code, w.Body.String())
	}
}
