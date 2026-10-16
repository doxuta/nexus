package middleware

// auth_preview_test.go — specs/064-preview-as-role-sample-data T012 (plan A4). Pins the ORDER of
// the branches in authMiddleware: a typ=preview token is published right after verifyBearer,
// BEFORE refreshSubject and the `sid` guard. If it went through refreshSubject it would be rebuilt
// from a users row (an admin reader here) and become admin; its `sid` is the preview-session id,
// not a login session, so the session guard must not see it either.
//
// specs/062-customer-workspace L1a adds a delegation branch (act/att/wsid, TokenType == access
// only) AFTER the preview branch; when it lands it wires middleware.WorkspaceResolver into this
// test and asserts the resolver is never called for typ=preview (062 T021).

import (
	"context"
	"testing"

	"nexus/platform/internal/auth"
)

// adminForEveryone is a LiveUserReader that answers "active admin" for ANY uid — the worst case a
// preview token could be rebuilt from if it ever reached refreshSubject.
type adminForEveryone struct{ calls int }

func (a *adminForEveryone) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	a.calls++
	role := "role-admin"
	return &auth.User{ID: id, TenantID: tenantID, Status: "active", IsAdmin: true, IsPlatformOperator: true, RoleID: &role}, nil
}

func previewClaims() *auth.Claims {
	return &auth.Claims{
		Subject: auth.Subject{
			UserID:        "5b1c0e39-2f7b-5d2e-9a57-4c1f0f6d9a10", // synthetic principal ≠ act
			TenantID:      "tenant-abc",
			RoleID:        "role-sales",
			IsAdmin:       false,
			SessionID:     "preview-session-1",
			ActorUserID:   "builder-1",
			ActorTenantID: "tenant-abc", // att = tid before a customer workspace (S9-8)
		},
		TokenType: auth.TokenTypePreview,
	}
}

func TestAuthMiddlewareLive_PreviewTokenSkipsRefreshSubjectAndSessionGuard(t *testing.T) {
	users := &adminForEveryone{}
	sessions := &fakeSessions{ended: true} // would 401 if it were consulted
	c, rec := newRequest("Bearer preview.token.here")
	AuthMiddlewareLive(&fakeVerifier{claims: previewClaims()}, users, sessions, nil)(c)

	if c.IsAborted() {
		t.Fatalf("preview token refused (status %d body %s); want pass-through", rec.Code, rec.Body.String())
	}
	sub, ok := SubjectFromContext(c)
	if !ok {
		t.Fatal("no subject in context")
	}
	if sub.IsAdmin || sub.IsPlatformOperator || sub.WorkspaceSessionID != "" {
		t.Errorf("preview subject gained privilege: admin=%v operator=%v wsid=%q", sub.IsAdmin, sub.IsPlatformOperator, sub.WorkspaceSessionID)
	}
	if sub.RoleID != "role-sales" || sub.UserID != previewClaims().UserID {
		t.Errorf("preview subject rewritten: %+v", sub)
	}
	if sub.ActorUserID == sub.UserID || sub.ActorTenantID != sub.TenantID {
		t.Errorf("act/att not preserved: %+v", sub)
	}
	if users.calls != 0 {
		t.Errorf("refreshSubject read a users row %d time(s) for a preview token", users.calls)
	}
	if len(sessions.calls) != 0 {
		t.Errorf("session guard consulted for a preview token: %v", sessions.calls)
	}
	if tid, _ := TenantFromContext(c); tid != "tenant-abc" {
		t.Errorf("tenant = %q, want tenant-abc", tid)
	}
}

// The claims-only harness (AuthMiddleware, no reader) publishes the preview claims unchanged too.
func TestAuthMiddleware_PreviewTokenPublishedAsIs(t *testing.T) {
	c, _ := newRequest("Bearer preview.token.here")
	AuthMiddleware(&fakeVerifier{claims: previewClaims()}, nil)(c)
	claims, ok := ClaimsFromContext(c)
	if !ok || claims.TokenType != auth.TokenTypePreview || claims.IsAdmin {
		t.Fatalf("claims = %+v ok=%v", claims, ok)
	}
}

// Control: an access token still goes through refreshSubject (the preview branch is narrow).
func TestAuthMiddlewareLive_AccessTokenStillRefreshed(t *testing.T) {
	users := &adminForEveryone{}
	c, _ := newRequest("Bearer access.token.here")
	claims := sampleClaims()
	claims.IsAdmin = false
	AuthMiddlewareLive(&fakeVerifier{claims: claims}, users, nil, nil)(c)
	if users.calls != 1 {
		t.Fatalf("access token: users row reads = %d, want 1", users.calls)
	}
}
