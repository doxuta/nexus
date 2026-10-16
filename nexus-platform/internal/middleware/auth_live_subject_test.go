package middleware

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeLiveUsers is an in-memory LiveUserReader: it returns the configured row/error for every
// lookup and records the (tenant, user) pairs it was asked for, so a test can assert the
// middleware reads the row pinned to the TOKEN's tenant + user (never a client value) and that
// the API-key path never reads at all.
type fakeLiveUsers struct {
	user  *auth.User
	err   error
	calls [][2]string
}

func (f *fakeLiveUsers) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	f.calls = append(f.calls, [2]string{tenantID, id})
	return f.user, f.err
}

// runLive executes AuthMiddlewareLive on a request carrying a valid Bearer whose verifier
// returns claims (the STALE snapshot minted at login) while users serves the LIVE row.
func runLive(t *testing.T, claims *auth.Claims, users *fakeLiveUsers) (*fakeVerifier, int, string, bool, auth.Subject) {
	t.Helper()
	v := &fakeVerifier{claims: claims}
	c, rec := newRequest("Bearer good.token.here")
	AuthMiddlewareLive(v, users, nil, nil)(c)
	sub, _ := SubjectFromContext(c)
	return v, rec.Code, rec.Body.String(), !c.IsAborted(), sub
}

// Queue #14 (claim-freshness): the token still says admin + role-old (minted 15 minutes ago),
// the row says demoted + role-new — the request must run under the ROW.
func TestAuthMiddlewareLive_RowOverridesStaleRoleAndAdmin(t *testing.T) {
	newRole := "role-new"
	users := &fakeLiveUsers{user: &auth.User{Status: "active", IsAdmin: false, RoleID: &newRole}}
	_, code, body, nextRan, sub := runLive(t, sampleClaims(), users) // sampleClaims: IsAdmin=true, RoleID=role-xyz

	if !nextRan {
		t.Fatalf("active user was refused (status=%d body=%s); want pass-through", code, body)
	}
	if sub.IsAdmin {
		t.Error("IsAdmin: got true (stale token claim), want false (live row)")
	}
	if sub.RoleID != "role-new" {
		t.Errorf("RoleID: got %q (stale token claim), want role-new (live row)", sub.RoleID)
	}
	// Identity claims are the cache key, not something the row may rewrite.
	if sub.UserID != "user-123" || sub.TenantID != "tenant-abc" {
		t.Errorf("identity drifted: got user=%q tenant=%q, want user-123/tenant-abc", sub.UserID, sub.TenantID)
	}
	if want := [2]string{"tenant-abc", "user-123"}; len(users.calls) != 1 || users.calls[0] != want {
		t.Errorf("row lookup calls = %v, want exactly one for %v (tenant+user from the verified token)", users.calls, want)
	}
}

// A role cleared to NULL on the row must clear the claim too (empty RoleID → default-deny in ACL).
func TestAuthMiddlewareLive_RowRoleClearedYieldsEmptyRoleID(t *testing.T) {
	users := &fakeLiveUsers{user: &auth.User{Status: "active", IsAdmin: false, RoleID: nil}}
	_, _, _, nextRan, sub := runLive(t, sampleClaims(), users)
	if !nextRan {
		t.Fatal("active user was refused; want pass-through")
	}
	if sub.RoleID != "" {
		t.Errorf("RoleID: got %q, want empty when the row's role_id is NULL", sub.RoleID)
	}
}

// Disable: status left "active" → the very next request is refused with the SAME 401 body every
// auth failure gets (anti-enumeration), so the FE's refresh-then-/login path kicks in.
func TestAuthMiddlewareLive_DisabledUser_401(t *testing.T) {
	users := &fakeLiveUsers{user: &auth.User{Status: "disabled", IsAdmin: true}}
	_, code, body, nextRan, _ := runLive(t, sampleClaims(), users)
	if nextRan {
		t.Fatal("disabled user proceeded past the auth stage; want abort")
	}
	if code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", code)
	}
	assertUnauthorizedBody(t, body)
}

// Delete: a soft-deleted row is ErrUserNotFound to the reader → 401, not 500 (expected sentinel).
func TestAuthMiddlewareLive_DeletedUser_401(t *testing.T) {
	users := &fakeLiveUsers{err: auth.ErrUserNotFound}
	_, code, body, nextRan, _ := runLive(t, sampleClaims(), users)
	if nextRan {
		t.Fatal("deleted user proceeded past the auth stage; want abort")
	}
	if code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", code)
	}
	assertUnauthorizedBody(t, body)
}

// Infrastructure failure on the row read FAILS CLOSED (500) — the stale token claims must never
// be used as a fallback authority.
func TestAuthMiddlewareLive_ReaderError_FailsClosed500(t *testing.T) {
	users := &fakeLiveUsers{err: errors.New("db down")}
	_, code, _, nextRan, _ := runLive(t, sampleClaims(), users)
	if nextRan {
		t.Fatal("request proceeded on a row-read error; want fail-closed abort")
	}
	if code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500 (fail closed, not a stale-claim pass)", code)
	}
}

// API keys (`nxk_`) are machine identities with NO user row: the verifier already reads their
// row per request. The live re-read must NOT run for them — reading would 401 every key.
func TestAuthMiddlewareLive_APIKeyClaimsAreNotReRead(t *testing.T) {
	keyClaims := &auth.Claims{
		Subject:   auth.Subject{UserID: "key-1", TenantID: "tenant-abc", RoleID: "role-key", IsAdmin: false},
		TokenType: auth.TokenTypeAPIKey,
	}
	users := &fakeLiveUsers{err: auth.ErrUserNotFound} // would 401 IF consulted
	_, code, body, nextRan, sub := runLive(t, keyClaims, users)
	if !nextRan {
		t.Fatalf("api-key request was refused (status=%d body=%s); want pass-through", code, body)
	}
	if len(users.calls) != 0 {
		t.Errorf("user row was read %d times for an api-key principal; want 0", len(users.calls))
	}
	if sub.RoleID != "role-key" || sub.UserID != "key-1" || sub.IsAdmin {
		t.Errorf("api-key subject changed: %+v; want verbatim key claims", sub)
	}
}

// The verifier's claims object is never mutated in place: a verifier that hands out a shared
// pointer (the test fake here) must see its own snapshot untouched after the request.
func TestAuthMiddlewareLive_DoesNotMutateVerifierClaims(t *testing.T) {
	stale := sampleClaims()
	users := &fakeLiveUsers{user: &auth.User{Status: "active", IsAdmin: false}}
	v, _, _, _, sub := runLive(t, stale, users)
	if sub.IsAdmin {
		t.Fatal("context subject still admin; live row should have demoted it")
	}
	if !v.claims.IsAdmin || v.claims.RoleID != "role-xyz" {
		t.Errorf("verifier claims were mutated in place: %+v", v.claims.Subject)
	}
}

// Plain AuthMiddleware (no reader wired) keeps the pre-#14 behaviour: token claims verbatim.
func TestAuthMiddleware_WithoutReader_KeepsTokenClaims(t *testing.T) {
	v := &fakeVerifier{claims: sampleClaims()}
	c, _, nextRan := runMiddleware(t, v, "Bearer good.token.here")
	if !nextRan {
		t.Fatal("valid token refused")
	}
	sub, _ := SubjectFromContext(c)
	if !sub.IsAdmin || sub.RoleID != "role-xyz" {
		t.Errorf("subject = %+v; want the token claims verbatim when no live reader is wired", sub)
	}
}
