package handlers

// Queue #44 session-list-kill-a (../specs/055-session-list-kill/spec.md): GET /auth/sessions lists the
// CALLER's live login sessions with the device each one came from; GET /admin/users/:id/sessions does
// the same for a user of the admin's tenant. Identity comes from the verified claim only.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

func (s *stubRefreshRepo) ListLiveSessions(_ context.Context, tenantID, userID string, limit int) ([]auth.RefreshToken, error) {
	var out []auth.RefreshToken
	for _, rt := range s.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil && rt.ExpiresAt.After(time.Now()) {
			out = append(out, *rt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

const (
	uaMac    = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	uaIPhone = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"
)

// loginFrom logs the seeded user in from a given device (User-Agent + client address).
func loginFrom(t *testing.T, r *gin.Engine, password, ua, remoteAddr string) (string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"tenant":"acme","email":"alice@acme.test","password":"`+password+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return resp.Data.AccessToken, resp.Data.RefreshToken
}

type sessionRow struct {
	ID         string    `json:"id"`
	Browser    string    `json:"browser"`
	OS         string    `json:"os"`
	UserAgent  string    `json:"userAgent"`
	IP         string    `json:"ip"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Current    bool      `json:"current"`
}

func decodeSessions(t *testing.T, w *httptest.ResponseRecorder) []sessionRow {
	t.Helper()
	var env struct {
		Data []sessionRow `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode sessions: %v (%s)", err, w.Body.String())
	}
	return env.Data
}

func TestAuthSessions_ListsTheCallersDevices(t *testing.T) {
	r, password, refresh := logoutAllRouter(t)
	access, _ := loginFrom(t, r, password, uaMac, "113.161.42.87:51000")
	loginFrom(t, r, password, uaIPhone, "42.118.7.9:62000")
	// Another user's live session in the same tenant — must not leak into the caller's list.
	if err := refresh.Store(context.Background(), &auth.RefreshToken{ID: "zz-other", SessionID: "s-other", TenantID: "t-1", UserID: "u-2", TokenHash: "h-other", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := getWithAuth(r, "/api/v1/auth/sessions", access)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /auth/sessions = %d %s, want 200", w.Code, w.Body.String())
	}
	got := decodeSessions(t, w)
	if len(got) != 2 {
		t.Fatalf("sessions = %d (%s), want the caller's 2 logins", len(got), w.Body.String())
	}
	devices := map[string]sessionRow{}
	for _, s := range got {
		if s.ID == "" || s.ID == "s-other" {
			t.Errorf("bad session id %q", s.ID)
		}
		devices[s.Browser+"/"+s.OS] = s
	}
	mac, ok := devices["Chrome/macOS"]
	if !ok || mac.IP != "113.161.42.x" || mac.UserAgent != uaMac || mac.LastUsedAt.IsZero() || mac.ExpiresAt.IsZero() {
		t.Errorf("Mac row = %+v (all=%v), want Chrome/macOS from 113.161.42.x", mac, devices)
	}
	if phone, ok := devices["Safari/iOS"]; !ok || phone.IP != "42.118.7.x" {
		t.Errorf("iPhone row = %+v (all=%v), want Safari/iOS from 42.118.7.x", phone, devices)
	}
	if strings.Contains(w.Body.String(), "113.161.42.87") {
		t.Error("the full client IP leaked into the response")
	}
}

func TestAuthSessions_RequiresAVerifiedCaller(t *testing.T) {
	r, _, _ := logoutAllRouter(t)
	for _, tok := range []string{"", "not-a-jwt"} {
		if w := getWithAuth(r, "/api/v1/auth/sessions", tok); w.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, w.Code)
		}
	}
}

func TestAdminUserSessions_ListsTheTargetUsersDevices(t *testing.T) {
	r, password, tokens, _ := newAdminSessionsEngine(t)
	adminAccess, _ := loginFrom(t, r, password, uaMac, "113.161.42.87:51000")

	w := getWithAuth(r, "/api/v1/admin/users/u-1/sessions", adminAccess)
	if w.Code != http.StatusOK {
		t.Fatalf("admin GET sessions = %d %s, want 200", w.Code, w.Body.String())
	}
	if got := decodeSessions(t, w); len(got) != 1 || got[0].Browser != "Chrome" {
		t.Fatalf("admin sees %s, want the one Mac session", w.Body.String())
	}

	// Unknown / other-tenant user → 404 USER_NOT_FOUND (tenant-pinned lookup).
	w = getWithAuth(r, "/api/v1/admin/users/ghost/sessions", adminAccess)
	if w.Code != http.StatusNotFound || cfCodeOf(w) != "USER_NOT_FOUND" {
		t.Fatalf("unknown user = %d %s, want 404 USER_NOT_FOUND", w.Code, w.Body.String())
	}

	// A non-admin never reaches the handler (RequireAdmin → 403).
	nonAdmin, err := tokens.IssueAccessToken(auth.Subject{UserID: "u-1", TenantID: "t-1", IsAdmin: false})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if w := getWithAuth(r, "/api/v1/admin/users/u-1/sessions", nonAdmin); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", w.Code)
	}
}

func cfCodeOf(w *httptest.ResponseRecorder) string {
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Code
}

// newAdminSessionsEngine: one engine with the public auth routes + the admin user routes behind
// Auth → Tenant → RequireAdmin, sharing ONE AuthService (so the admin's own login is listable).
func newAdminSessionsEngine(t *testing.T) (*gin.Engine, string, *auth.TokenService, *stubRefreshRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc, password, tokens, refresh := newStubAuthService(t, true)

	r := gin.New()
	api := r.Group("/api/v1")
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(api)

	admin := r.Group("/api/v1")
	admin.Use(middleware.AuthMiddleware(tokens, zap.NewNop()))
	admin.Use(middleware.RequireTenant(zap.NewNop()))
	admin.Use(middleware.RequireAdmin(zap.NewNop()))
	// The sessions routes ask the user service the B2 question (may this admin act on that account?),
	// so it needs a user store holding the account these tests look at.
	users := newFakeUserCRUDRepo()
	users.rows[users.key("t-1", "u-1")] = &auth.User{ID: "u-1", TenantID: "t-1", Email: "alice@acme.test", Status: "active"}
	NewAdminUserHandler(services.NewUserService(users), zap.NewNop()).WithSessionManager(svc).RegisterRoutes(admin)
	return r, password, tokens, refresh
}

// newStubAuthService builds an AuthService over the in-memory stubs with one seeded user
// (t-1/u-1 alice@acme.test, admin flag as given) and returns it with its password, token service
// and refresh store.
func newStubAuthService(t *testing.T, isAdmin bool) (*services.AuthService, string, *auth.TokenService, *stubRefreshRepo) {
	t.Helper()
	const password = "s3cret-pw"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	repo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user: &auth.User{
			ID: "u-1", TenantID: "t-1", Email: "alice@acme.test",
			PasswordHash: string(hash), Status: "active", IsAdmin: isAdmin,
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	refresh := newStubRefreshRepo()
	return services.NewAuthService(repo, refresh, tokens), password, tokens, refresh
}

// --- #45 session-list-kill-b: "phiên này" + ending one session ----------------------------------

func (s *stubRefreshRepo) EndSession(_ context.Context, tenantID, userID, sessionID string) (bool, error) {
	ended := false
	for _, rt := range s.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.SessionID == sessionID && rt.RevokedAt == nil {
			now, reason := time.Now(), auth.RevokeReasonEnded
			rt.RevokedAt, rt.RevokeReason = &now, &reason
			ended = true
		}
	}
	return ended, nil
}

func (s *stubRefreshRepo) EndAllSessions(_ context.Context, tenantID, userID, except string) ([]string, error) {
	var sids []string
	for _, rt := range s.rows {
		if rt.TenantID == tenantID && rt.UserID == userID && rt.RevokedAt == nil && (except == "" || rt.SessionID != except) {
			now, reason := time.Now(), auth.RevokeReasonEnded
			rt.RevokedAt, rt.RevokeReason = &now, &reason
			sids = append(sids, rt.SessionID)
		}
	}
	return sids, nil
}

func deleteWithAuth(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAuthSessions_MarksCurrentAndEndsAnotherDevice(t *testing.T) {
	r, password, _ := logoutAllRouter(t)
	macAccess, _ := loginFrom(t, r, password, uaMac, "113.161.42.87:51000")
	_, phoneRefresh := loginFrom(t, r, password, uaIPhone, "42.118.7.9:62000")

	rows := decodeSessions(t, getWithAuth(r, "/api/v1/auth/sessions", macAccess))
	var mac, phone sessionRow
	for _, s := range rows {
		if s.OS == "macOS" {
			mac = s
		} else {
			phone = s
		}
	}
	if !mac.Current || phone.Current {
		t.Fatalf("current flags mac=%v phone=%v, want only the caller's (Mac) session current", mac.Current, phone.Current)
	}

	if w := deleteWithAuth(r, "/api/v1/auth/sessions/"+phone.ID, macAccess); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE phone session = %d %s, want 204", w.Code, w.Body.String())
	}
	left := decodeSessions(t, getWithAuth(r, "/api/v1/auth/sessions", macAccess))
	if len(left) != 1 || left[0].ID != mac.ID {
		t.Fatalf("after ending the phone: %+v, want only the Mac", left)
	}
	if w := postJSON(r, "/api/v1/auth/refresh", gin.H{"refreshToken": phoneRefresh}); w.Code != http.StatusUnauthorized {
		t.Fatalf("the ended phone still refreshes: %d", w.Code)
	}
}

func TestAuthSessions_EndingSomeoneElsesSessionIsNotFound(t *testing.T) {
	r, password, refresh := logoutAllRouter(t)
	access, _ := loginFrom(t, r, password, uaMac, "113.161.42.87:51000")
	other := &auth.RefreshToken{ID: "zz-other", SessionID: "s-other", TenantID: "t-1", UserID: "u-2", TokenHash: "h-other", ExpiresAt: time.Now().Add(time.Hour)}
	if err := refresh.Store(context.Background(), other); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, sid := range []string{"s-other", "does-not-exist"} {
		w := deleteWithAuth(r, "/api/v1/auth/sessions/"+sid, access)
		if w.Code != http.StatusNotFound || cfCodeOf(w) != "SESSION_NOT_FOUND" {
			t.Errorf("DELETE %s = %d %s, want 404 SESSION_NOT_FOUND", sid, w.Code, w.Body.String())
		}
	}
	if refresh.liveFor("t-1", "u-2") != 1 {
		t.Fatal("another user's session was ended through /auth/sessions")
	}
	if w := deleteWithAuth(r, "/api/v1/auth/sessions/s-other", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", w.Code)
	}
}

func TestAdminUserSessions_AdminEndsAUsersSession(t *testing.T) {
	r, password, tokens, refresh := newAdminSessionsEngine(t)
	adminAccess, _ := loginFrom(t, r, password, uaMac, "113.161.42.87:51000")
	loginFrom(t, r, password, uaIPhone, "42.118.7.9:62000")

	rows := decodeSessions(t, getWithAuth(r, "/api/v1/admin/users/u-1/sessions", adminAccess))
	var target string
	for _, s := range rows {
		if s.OS == "iOS" {
			target = s.ID
		}
		if s.OS == "macOS" && !s.Current {
			t.Error("the admin's own session (they are viewing themselves) is not marked current")
		}
	}

	nonAdmin, err := tokens.IssueAccessToken(auth.Subject{UserID: "u-1", TenantID: "t-1"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if w := deleteWithAuth(r, "/api/v1/admin/users/u-1/sessions/"+target, nonAdmin); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin DELETE = %d, want 403", w.Code)
	}
	if w := deleteWithAuth(r, "/api/v1/admin/users/u-1/sessions/"+target, adminAccess); w.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	if n := refresh.liveFor("t-1", "u-1"); n != 1 {
		t.Fatalf("live sessions after admin end = %d, want 1 (only the iPhone ended)", n)
	}
	if w := deleteWithAuth(r, "/api/v1/admin/users/u-1/sessions/"+target, adminAccess); w.Code != http.StatusNotFound {
		t.Errorf("ending an already-ended session = %d, want 404", w.Code)
	}
}
