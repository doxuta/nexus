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

func init() { gin.SetMode(gin.TestMode) }

// fakeVerifier is an in-memory tokenVerifier: it returns the configured claims/error for
// any token, and records the token string it was asked to verify so tests can assert the
// middleware extracted the bare credential (no "Bearer " prefix, trimmed).
type fakeVerifier struct {
	claims    *auth.Claims
	err       error
	gotToken  string
	callCount int
}

func (f *fakeVerifier) VerifyAccessToken(tok string) (*auth.Claims, error) {
	f.callCount++
	f.gotToken = tok
	return f.claims, f.err
}

func sampleClaims() *auth.Claims {
	return &auth.Claims{
		Subject: auth.Subject{
			UserID:   "user-123",
			TenantID: "tenant-abc",
			RoleID:   "role-xyz",
			IsAdmin:  true,
		},
		TokenType: auth.TokenTypeAccess,
	}
}

// newRequest builds a *gin.Context wired to a recorder, with the given Authorization header.
func newRequest(authHeader string) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/whatever", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	c.Request = req
	return c, rec
}

// runMiddleware executes AuthMiddleware on a context with the given header, recording
// whether the next handler ran and what it saw in context.
func runMiddleware(t *testing.T, v tokenVerifier, authHeader string) (*gin.Context, *httptest.ResponseRecorder, bool) {
	t.Helper()
	c, rec := newRequest(authHeader)
	nextRan := false
	mw := AuthMiddleware(v, nil)
	// Emulate the engine running the chain: invoke the middleware, then a terminal handler
	// only if the middleware did not abort.
	mw(c)
	if !c.IsAborted() {
		nextRan = true
	}
	return c, rec, nextRan
}

// --- valid token --------------------------------------------------------------

func TestAuthMiddleware_ValidToken_PopulatesContext(t *testing.T) {
	v := &fakeVerifier{claims: sampleClaims()}
	c, rec, nextRan := runMiddleware(t, v, "Bearer good.token.here")

	if !nextRan {
		t.Fatalf("valid token: request was aborted, want it to proceed (status=%d, body=%s)", rec.Code, rec.Body.String())
	}
	if v.callCount != 1 {
		t.Errorf("verifier call count: got %d, want 1", v.callCount)
	}
	if v.gotToken != "good.token.here" {
		t.Errorf("token passed to verifier: got %q, want bare token without scheme", v.gotToken)
	}

	// tenant_id available in context via accessor.
	tenant, ok := TenantFromContext(c)
	if !ok || tenant != "tenant-abc" {
		t.Errorf("TenantFromContext: got (%q, %v), want (\"tenant-abc\", true)", tenant, ok)
	}
	// full claims available, roles/admin accessible.
	claims, ok := ClaimsFromContext(c)
	if !ok {
		t.Fatal("ClaimsFromContext: want claims present after valid token")
	}
	if claims.UserID != "user-123" || claims.RoleID != "role-xyz" {
		t.Errorf("claims subject: got userID=%q roleID=%q, want user-123/role-xyz", claims.UserID, claims.RoleID)
	}
	if !IsAdminFromContext(c) {
		t.Error("IsAdminFromContext: got false, want true for admin claim")
	}
	sub, ok := SubjectFromContext(c)
	if !ok || sub.TenantID != "tenant-abc" {
		t.Errorf("SubjectFromContext: got (%+v, %v), want tenant-abc/true", sub, ok)
	}
}

func TestAuthMiddleware_ValidToken_LowercaseScheme(t *testing.T) {
	v := &fakeVerifier{claims: sampleClaims()}
	_, _, nextRan := runMiddleware(t, v, "bearer good.token")
	if !nextRan {
		t.Fatal("lowercase 'bearer' scheme should be accepted (RFC 7235 case-insensitive)")
	}
	if v.gotToken != "good.token" {
		t.Errorf("token: got %q, want good.token", v.gotToken)
	}
}

// --- expired token ------------------------------------------------------------

func TestAuthMiddleware_ExpiredToken_401(t *testing.T) {
	v := &fakeVerifier{err: auth.ErrTokenExpired}
	c, rec, nextRan := runMiddleware(t, v, "Bearer expired.token")

	if nextRan {
		t.Fatal("expired token: request proceeded, want abort")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	assertUnauthorizedBody(t, rec.Body.String())
	if _, ok := TenantFromContext(c); ok {
		t.Error("expired token must not populate tenant in context")
	}
}

// --- bad signature ------------------------------------------------------------

func TestAuthMiddleware_BadSignature_401(t *testing.T) {
	v := &fakeVerifier{err: auth.ErrTokenInvalid}
	_, rec, nextRan := runMiddleware(t, v, "Bearer tampered.token.sig")

	if nextRan {
		t.Fatal("bad signature: request proceeded, want abort")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	assertUnauthorizedBody(t, rec.Body.String())
}

// --- wrong token type (refresh presented as access) ---------------------------

func TestAuthMiddleware_WrongTokenType_401(t *testing.T) {
	v := &fakeVerifier{err: auth.ErrWrongTokenType}
	_, rec, nextRan := runMiddleware(t, v, "Bearer refresh.as.access")
	if nextRan {
		t.Fatal("wrong token type: request proceeded, want abort")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

// --- missing / malformed header ----------------------------------------------

func TestAuthMiddleware_MissingHeader_401_NoVerifierCall(t *testing.T) {
	v := &fakeVerifier{claims: sampleClaims()} // would succeed IF called
	_, rec, nextRan := runMiddleware(t, v, "")

	if nextRan {
		t.Fatal("missing header: request proceeded, want abort")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	if v.callCount != 0 {
		t.Errorf("verifier should not be called for a missing header; got %d calls", v.callCount)
	}
}

func TestAuthMiddleware_MalformedHeader_401(t *testing.T) {
	cases := []struct {
		name   string
		header string
	}{
		{"no scheme", "good.token.here"},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"bearer no token", "Bearer "},
		{"bearer whitespace token", "Bearer    "},
		{"just bearer word", "Bearer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &fakeVerifier{claims: sampleClaims()}
			_, rec, nextRan := runMiddleware(t, v, tc.header)
			if nextRan {
				t.Fatalf("%s: request proceeded, want abort", tc.name)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s: status got %d, want 401", tc.name, rec.Code)
			}
			if v.callCount != 0 {
				t.Errorf("%s: verifier called %d times, want 0 (no extractable token)", tc.name, v.callCount)
			}
		})
	}
}

// --- RequireTenant guard ------------------------------------------------------

// runChain executes AuthMiddleware followed by RequireTenant on a context with the given
// header, reporting whether the terminal (business) handler would run.
func runChain(t *testing.T, v tokenVerifier, authHeader string) (*gin.Context, *httptest.ResponseRecorder, bool) {
	t.Helper()
	c, rec := newRequest(authHeader)
	AuthMiddleware(v, nil)(c)
	if !c.IsAborted() {
		RequireTenant(nil)(c)
	}
	return c, rec, !c.IsAborted()
}

func TestRequireTenant_PassesWithTenantClaim(t *testing.T) {
	v := &fakeVerifier{claims: sampleClaims()}
	_, _, reached := runChain(t, v, "Bearer good.token")
	if !reached {
		t.Fatal("RequireTenant aborted a request carrying a non-empty tenant claim")
	}
}

func TestRequireTenant_RejectsEmptyTenantClaim(t *testing.T) {
	// A verified token whose claim carries NO tenant id must be refused before any handler runs
	// (defense-in-depth: a tenant-less request must never reach a tenant-scoped query).
	claims := sampleClaims()
	claims.TenantID = ""
	v := &fakeVerifier{claims: claims}
	c, rec, reached := runChain(t, v, "Bearer good.but.tenantless")
	if reached {
		t.Fatal("RequireTenant let a tenant-less verified token through")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
	assertUnauthorizedBody(t, rec.Body.String())
	if _, ok := TenantFromContext(c); ok {
		t.Error("tenant-less claim must not surface a tenant in context")
	}
}

func TestRequireTenant_RejectsWhenAuthBypassed(t *testing.T) {
	// Mounted in isolation (simulating a wiring bug where AuthMiddleware never ran), RequireTenant
	// fails closed: with no tenant in context it aborts with 401 rather than passing through.
	c, rec := newRequest("")
	RequireTenant(nil)(c)
	if !c.IsAborted() {
		t.Fatal("RequireTenant should abort when no tenant is in context")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", rec.Code)
	}
}

// --- testable core: verifyBearer ---------------------------------------------

func TestVerifyBearer_TypedErrors(t *testing.T) {
	t.Run("missing header → ErrTokenMalformed", func(t *testing.T) {
		v := &fakeVerifier{claims: sampleClaims()}
		_, err := verifyBearer(context.Background(), v, "")
		if !errors.Is(err, auth.ErrTokenMalformed) {
			t.Errorf("got %v, want ErrTokenMalformed", err)
		}
	})
	t.Run("propagates verifier sentinel", func(t *testing.T) {
		v := &fakeVerifier{err: auth.ErrTokenExpired}
		_, err := verifyBearer(context.Background(), v, "Bearer x.y.z")
		if !errors.Is(err, auth.ErrTokenExpired) {
			t.Errorf("got %v, want ErrTokenExpired", err)
		}
	})
	t.Run("valid returns claims", func(t *testing.T) {
		v := &fakeVerifier{claims: sampleClaims()}
		claims, err := verifyBearer(context.Background(), v, "Bearer x.y.z")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if claims.TenantID != "tenant-abc" {
			t.Errorf("tenant: got %q, want tenant-abc", claims.TenantID)
		}
	})
}

// fakeContextVerifier implements BOTH tokenVerifier and the contextTokenVerifier upgrade
// (APIKEY-1). It records which path was invoked so the preference is provable.
type fakeContextVerifier struct {
	fakeVerifier
	ctxCalls int
	gotCtx   context.Context
}

func (f *fakeContextVerifier) VerifyAccessTokenContext(ctx context.Context, tok string) (*auth.Claims, error) {
	f.ctxCalls++
	f.gotCtx = ctx
	f.gotToken = tok
	return f.claims, f.err
}

func TestVerifyBearer_PrefersContextVerifier(t *testing.T) {
	// A verifier implementing the contextTokenVerifier upgrade must be called through it —
	// with the caller's ctx — and the plain VerifyAccessToken must NOT run (the composite
	// JWT+API-key verifier relies on this to thread the request context into its DB lookup).
	v := &fakeContextVerifier{fakeVerifier: fakeVerifier{claims: sampleClaims()}}
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	claims, err := verifyBearer(ctx, v, "Bearer nxk_something")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.ctxCalls != 1 || v.callCount != 0 {
		t.Errorf("path: ctxCalls=%d plainCalls=%d, want 1/0 (context path preferred)", v.ctxCalls, v.callCount)
	}
	if v.gotCtx == nil || v.gotCtx.Value(ctxKey{}) != "marker" {
		t.Error("caller ctx was not threaded through to the context verifier")
	}
	if v.gotToken != "nxk_something" {
		t.Errorf("token: got %q, want the bare credential", v.gotToken)
	}
	if claims.TenantID != "tenant-abc" {
		t.Errorf("tenant: got %q, want tenant-abc", claims.TenantID)
	}
}

// --- accessors fail closed when middleware was bypassed -----------------------

func TestAccessors_FailClosed_WhenNoMiddleware(t *testing.T) {
	c, _ := newRequest("")
	if _, ok := TenantFromContext(c); ok {
		t.Error("TenantFromContext should be (\"\", false) without middleware")
	}
	if _, ok := ClaimsFromContext(c); ok {
		t.Error("ClaimsFromContext should be (nil, false) without middleware")
	}
	if _, ok := SubjectFromContext(c); ok {
		t.Error("SubjectFromContext should be false without middleware")
	}
	if IsAdminFromContext(c) {
		t.Error("IsAdminFromContext should be false without middleware")
	}
}

func TestAccessors_FailClosed_WrongType(t *testing.T) {
	c, _ := newRequest("")
	c.Set(ctxKeyClaims, "not-claims")
	c.Set(ctxKeyTenantID, 42) // wrong type
	if _, ok := ClaimsFromContext(c); ok {
		t.Error("ClaimsFromContext should reject wrong-typed value")
	}
	if _, ok := TenantFromContext(c); ok {
		t.Error("TenantFromContext should reject wrong-typed value")
	}
}

// assertUnauthorizedBody checks the structured 401 body shape and that it leaks no internals.
func assertUnauthorizedBody(t *testing.T, body string) {
	t.Helper()
	if !contains(body, "UNAUTHORIZED") {
		t.Errorf("401 body should carry a structured code; got %s", body)
	}
	// No internal detail (sentinel text / secret / claim) should appear in the response.
	for _, leak := range []string{"expired", "signature", "malformed", "secret", "tenant-abc"} {
		if contains(body, leak) {
			t.Errorf("401 body leaked internal detail %q: %s", leak, body)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
