package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// fakeDecider is an in-memory ACL decider for the middleware tests. It records the arguments of
// the last Authorize call (so a test can assert the method→action mapping and tenant/role pass-
// through) and returns a programmed decision + access level. When err is set it is returned
// (fail-closed path).
type fakeDecider struct {
	allow bool
	level string
	err   error

	called    bool
	gotTenant string
	gotRole   string
	gotEntity string
	gotAction string
}

func (f *fakeDecider) Authorize(_ context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	f.called = true
	f.gotTenant, f.gotRole, f.gotEntity, f.gotAction = tenantID, roleID, entity, action
	return f.allow, f.level, f.err
}

// setACLClaims puts verified claims into context the way AuthMiddleware would, with an explicit
// role id and is_admin flag so the ACL middleware sees a realistic principal.
func setACLClaims(c *gin.Context, tenantID, roleID string, isAdmin bool) {
	c.Set(ctxKeyClaims, &auth.Claims{
		Subject: auth.Subject{
			UserID:   "user-1",
			TenantID: tenantID,
			RoleID:   roleID,
			IsAdmin:  isAdmin,
		},
		TokenType: auth.TokenTypeAccess,
	})
	c.Set(ctxKeyTenantID, tenantID)
}

// runACL drives the ACL middleware against a request for the given HTTP method on /:entity,
// with claims (tenant/role/admin) seeded as configured. It returns the recorder, whether the
// downstream handler ran, and the decider (to inspect what it was asked).
func runACL(method, entity string, dec *fakeDecider, tenantID, roleID string, isAdmin, setClaims bool) (*httptest.ResponseRecorder, bool, *fakeDecider) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(method, "/api/v1/"+entity, nil)
	// Bind the :entity route param the way gin's router would for /:entity.
	c.Params = gin.Params{{Key: "entity", Value: entity}}
	if setClaims {
		setACLClaims(c, tenantID, roleID, isAdmin)
	}

	RequireACL(dec, nil)(c)

	nextRan := !c.IsAborted()
	return rec, nextRan, dec
}

// D2: GET → view, and an allowed decision passes through to the handler.
func TestACL_AllowedRequestPasses(t *testing.T) {
	dec := &fakeDecider{allow: true}
	rec, nextRan, got := runACL(http.MethodGet, "contacts", dec, "t1", "role-1", false, true)
	if !nextRan {
		t.Fatalf("allowed request was aborted; want pass-through (body=%s)", rec.Body.String())
	}
	if !got.called {
		t.Fatalf("decider was not consulted")
	}
	if got.gotAction != "view" {
		t.Fatalf("action = %q, want view (GET→view)", got.gotAction)
	}
	if got.gotTenant != "t1" || got.gotRole != "role-1" || got.gotEntity != "contacts" {
		t.Fatalf("decider got (%q,%q,%q), want (t1,role-1,contacts)", got.gotTenant, got.gotRole, got.gotEntity)
	}
}

// D2: a denied decision is refused with 403, and the handler never runs.
func TestACL_DeniedRequestIs403(t *testing.T) {
	dec := &fakeDecider{allow: false}
	rec, nextRan, _ := runACL(http.MethodDelete, "contacts", dec, "t1", "role-1", false, true)
	if nextRan {
		t.Fatalf("denied request reached the handler; want it refused")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// D2: the full method→action mapping (GET→view, POST→create, PUT→edit, DELETE→delete).
func TestACL_MethodActionMapping(t *testing.T) {
	cases := map[string]string{
		http.MethodGet:    "view",
		http.MethodPost:   "create",
		http.MethodPut:    "edit",
		http.MethodDelete: "delete",
	}
	for method, wantAction := range cases {
		dec := &fakeDecider{allow: true}
		_, _, got := runACL(method, "deals", dec, "t1", "role-1", false, true)
		if got.gotAction != wantAction {
			t.Fatalf("%s → action %q, want %q", method, got.gotAction, wantAction)
		}
	}
}

// D3: is_admin BYPASS — an admin passes for EVERY action without the decider being consulted.
func TestACL_AdminBypassesEntirely(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		dec := &fakeDecider{allow: false} // would DENY if consulted
		rec, nextRan, got := runACL(method, "contacts", dec, "t1", "", true, true)
		if !nextRan {
			t.Fatalf("admin %s was aborted; want bypass (body=%s)", method, rec.Body.String())
		}
		if got.called {
			t.Fatalf("admin %s consulted the decider; want full bypass", method)
		}
	}
}

// A repository/decider error must FAIL CLOSED: the request is refused (403), handler never runs.
func TestACL_DeciderErrorFailsClosed(t *testing.T) {
	dec := &fakeDecider{allow: true, err: errors.New("db down")}
	rec, nextRan, _ := runACL(http.MethodGet, "contacts", dec, "t1", "role-1", false, true)
	if nextRan {
		t.Fatalf("decider error let the request through; want fail-closed refuse")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on decider error", rec.Code)
	}
}

// A request that never passed AuthMiddleware (no claims) must be refused — fail closed, not
// treated as an anonymous allow.
func TestACL_MissingClaimsRefused(t *testing.T) {
	dec := &fakeDecider{allow: true}
	_, nextRan, got := runACL(http.MethodGet, "contacts", dec, "t1", "role-1", false, false)
	if nextRan {
		t.Fatalf("request with no claims reached the handler; want refused")
	}
	if got.called {
		t.Fatalf("decider consulted despite missing claims; want early refuse")
	}
}

// A5: on an allowed (non-admin) decision RequireACL stashes the resolved access_level AND the
// verified user id into context so the record handler can build the row-level scope without a
// second ACL query.
func TestACL_StashesAccessLevelAndUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil)
	c.Params = gin.Params{{Key: "entity", Value: "contacts"}}
	setACLClaims(c, "t1", "role-1", false)

	dec := &fakeDecider{allow: true, level: "team"}
	RequireACL(dec, nil)(c)

	if c.IsAborted() {
		t.Fatalf("allowed request aborted; want pass-through")
	}
	if lvl, ok := AccessLevelFromContext(c); !ok || lvl != "team" {
		t.Fatalf("access level in context = (%q,%v), want (team,true)", lvl, ok)
	}
	if uid, ok := ACLUserIDFromContext(c); !ok || uid != "user-1" {
		t.Fatalf("acl user id in context = (%q,%v), want (user-1,true)", uid, ok)
	}
}

// specs/051: the action the level was resolved FOR is stashed too — the manual-share clause is a
// read-only widening, so the record handler turns it on only when that action is "view". A fixed
// action (RequireACLAction) wins over the method, and the admin bypass stashes nothing (admins see
// every row; there is no share to add).
func TestACL_StashesResolvedAction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name, method, fixed, want string
		admin, wantOK             bool
	}{
		{name: "GET maps to view", method: http.MethodGet, want: "view", wantOK: true},
		{name: "PUT maps to edit", method: http.MethodPut, want: "edit", wantOK: true},
		{name: "fixed edit on a GET stays edit", method: http.MethodGet, fixed: "edit", want: "edit", wantOK: true},
		{name: "fixed view on a DELETE stays view", method: http.MethodDelete, fixed: "view", want: "view", wantOK: true},
		{name: "admin bypass stashes no action", method: http.MethodGet, admin: true, wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(tc.method, "/api/v1/contacts/x", nil)
			c.Params = gin.Params{{Key: "entity", Value: "contacts"}}
			setACLClaims(c, "t1", "role-1", tc.admin)
			dec := &fakeDecider{allow: true, level: "owner"}
			if tc.fixed != "" {
				RequireACLAction(dec, nil, tc.fixed)(c)
			} else {
				RequireACL(dec, nil)(c)
			}
			if c.IsAborted() {
				t.Fatalf("request aborted; want pass-through")
			}
			got, ok := ACLActionFromContext(c)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("action in context = (%q,%v), want (%q,%v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// A5: the is_admin bypass stashes access_level=all + the verified user id (admins read every row).
func TestACL_AdminBypassStashesAll(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/contacts", nil)
	c.Params = gin.Params{{Key: "entity", Value: "contacts"}}
	setACLClaims(c, "t1", "", true)

	dec := &fakeDecider{allow: false} // would deny if consulted
	RequireACL(dec, nil)(c)

	if c.IsAborted() {
		t.Fatalf("admin request aborted; want bypass")
	}
	if dec.called {
		t.Fatalf("admin consulted decider; want full bypass")
	}
	if lvl, ok := AccessLevelFromContext(c); !ok || lvl != "all" {
		t.Fatalf("admin access level = (%q,%v), want (all,true)", lvl, ok)
	}
	if uid, ok := ACLUserIDFromContext(c); !ok || uid != "user-1" {
		t.Fatalf("admin acl user id = (%q,%v), want (user-1,true)", uid, ok)
	}
}

func TestACL_UnmappedMethodRefused(t *testing.T) {
	dec := &fakeDecider{allow: true}
	rec, nextRan, got := runACL(http.MethodPatch, "contacts", dec, "t1", "role-1", false, true)
	if nextRan {
		t.Fatalf("unmapped PATCH reached the handler; want refused")
	}
	if got.called {
		t.Fatalf("decider consulted for unmapped method; want early refuse")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for unmapped method", rec.Code)
	}
}

// W9b (security 🔵): RequireACLAction with an EMPTY action must fail closed on every request —
// a mis-wired route group (empty literal, refactor slip) must never become default-allow.
func TestACLAction_EmptyActionFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/contacts/x/restore", nil)
	c.Params = gin.Params{{Key: "entity", Value: "contacts"}}
	setACLClaims(c, "t1", "role-1", false)

	dec := &fakeDecider{allow: true} // even an allow-everything decider must not be consulted
	RequireACLAction(dec, nil, "")(c)

	if !c.IsAborted() {
		t.Fatal("empty-action RequireACLAction let the request through; want fail-closed 403")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if dec.called {
		t.Fatal("decider was consulted despite empty action; want short-circuit deny")
	}
}

// W9b: the fixed-action variant consults the decider with EXACTLY the wired action, regardless
// of HTTP method (POST here would map to "create" under RequireACL — must stay "delete").
func TestACLAction_FixedActionOverridesMethod(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/contacts/x/restore", nil)
	c.Params = gin.Params{{Key: "entity", Value: "contacts"}}
	setACLClaims(c, "t1", "role-1", false)

	dec := &fakeDecider{allow: true}
	RequireACLAction(dec, nil, "delete")(c)

	if c.IsAborted() {
		t.Fatalf("allowed fixed-action request was aborted (body=%s)", rec.Body.String())
	}
	if dec.gotAction != "delete" {
		t.Fatalf("decider action = %q, want the wired \"delete\" (not POST→create)", dec.gotAction)
	}
}
