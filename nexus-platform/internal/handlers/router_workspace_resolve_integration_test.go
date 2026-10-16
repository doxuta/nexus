//go:build integration

// specs/062-customer-workspace T015a — a builder workspace-session token over the REAL router and the
// REAL resolver against MySQL 8: the write lands in the CUSTOMER tenant (never the workshop), the
// operator-only cluster stays shut (403 byte-identical), and every revocation kills the very next
// request. The token is minted with IssueAccessTokenUntil and the session row inserted by hand —
// there is no route that mints one before L1b.
//
//	go test -tags=integration ./internal/handlers/ -run TestWorkspaceResolve
package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

const (
	wsITHomeID   = "a1a1a1a1-0000-0000-0000-000000006201"
	wsITCustID   = "a1a1a1a1-0000-0000-0000-000000006202"
	wsITHomeSlug = "it-ws-home"
	wsITCustSlug = "it-ws-cust"
	wsITEmail    = "builder@it-ws-home.test"
	wsITPassword = "Ws-Builder-Pw-1!"
	wsITEntity   = "ws_items"
)

type wsResolveIT struct {
	gdb      *gorm.DB
	tokens   *auth.TokenService
	userID   string
	loginSID string
	wsID     string
	build    func(withResolver bool) *gin.Engine
}

func newWSResolveIT(t *testing.T) *wsResolveIT {
	t.Helper()
	gdb := authGormDB(t)
	p := &wsResolveIT{gdb: gdb, userID: uuid.NewString(), loginSID: uuid.NewString(), wsID: uuid.NewString()}
	ids := []string{wsITHomeID, wsITCustID}

	clean := func() {
		gdb.Exec("DELETE FROM sys_workspace_sessions WHERE tenant_id IN ?", ids)
		gdb.Exec("DELETE FROM sys_layouts WHERE tenant_id IN ?", ids)
		gdb.Exec("DELETE FROM sys_entities WHERE tenant_id IN ?", ids)
		gdb.Where("tenant_id IN ?", ids).Delete(&auth.RefreshToken{})
		gdb.Unscoped().Where("tenant_id IN ?", ids).Delete(&auth.User{})
		gdb.Unscoped().Where("id IN ?", ids).Delete(&auth.Tenant{})
	}
	clean()
	t.Cleanup(clean)

	now := time.Now()
	for _, tn := range []auth.Tenant{
		{ID: wsITHomeID, Slug: wsITHomeSlug, Name: "Xưởng IT", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: wsITCustID, Slug: wsITCustSlug, Name: "Khách IT", Status: "active", CreatedAt: now, UpdatedAt: now},
	} {
		if err := gdb.Create(&tn).Error; err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(wsITPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := gdb.Create(&auth.User{ID: p.userID, TenantID: wsITHomeID, Email: wsITEmail, PasswordHash: string(hash),
		FullName: "Builder IT", Status: "active", IsAdmin: true, IsPlatformOperator: true, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed builder: %v", err)
	}
	if err := gdb.Create(&metadata.EntityDefinition{ID: uuid.NewString(), TenantID: wsITCustID, Name: wsITEntity,
		Label: "Mục IT", DataTable: "obj_" + wsITEntity, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	t.Setenv("JWT_SECRET", itJWTSecret)
	tokens, err := auth.NewTokenService(itJWTSecret, 15*time.Minute, 168*time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	p.tokens = tokens
	users := auth.NewUserRepository(gdb)
	refresh := auth.NewRefreshTokenRepository(gdb)
	guard := services.NewCachedSessionGuard(refresh, services.DefaultSessionGuardTTL)
	authSvc := services.NewAuthService(users, refresh, tokens).WithSessionGuard(guard)
	wsRepo := auth.NewWorkspaceSessionRepository(gdb)
	resolver := services.NewWorkspaceSessionService(wsRepo, users, tokens)
	metaRepo := metadata.NewRepository(gdb)
	layoutSvc := services.NewLayoutService(metaRepo, metadata.NewLayoutRepository(gdb), zap.NewNop())

	p.build = func(withResolver bool) *gin.Engine {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		opts := []RouteOption{
			WithOperatorTier(middleware.RequireBuilderTier(users, zap.NewNop())),
			WithLiveSubject(users),
			WithSessionGuard(guard),
		}
		if withResolver {
			opts = append(opts, WithWorkspaceResolver(resolver))
		}
		RegisterAPIRoutes(r.Group("/api/v1"), tokens, nil, APIHandlers{
			Auth:   NewAuthHandler(authSvc, zap.NewNop()),
			Layout: NewAdminLayoutHandler(layoutSvc, zap.NewNop()),
			Tenant: NewAdminTenantHandler(services.NewTenantProvisionService(gdb), users, zap.NewNop()),
		}, zap.NewNop(), opts...)
		return r
	}
	return p
}

func (p *wsResolveIT) do(t *testing.T, r *gin.Engine, method, path, token string, body any) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// homeLogin logs the builder in for real (live refresh row of a fresh login session) and returns
// the home access token and its sid.
func (p *wsResolveIT) homeLogin(t *testing.T, r *gin.Engine) (string, string) {
	t.Helper()
	code, body := p.do(t, r, http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"tenant": wsITHomeSlug, "email": wsITEmail, "password": wsITPassword})
	if code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}
	var out struct {
		Data struct {
			AccessToken string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	claims, err := p.tokens.VerifyAccessToken(out.Data.AccessToken)
	if err != nil || claims.SessionID == "" {
		t.Fatalf("login token: %+v %v", claims, err)
	}
	return out.Data.AccessToken, claims.SessionID
}

// openSession inserts the session row for (customer, builder, loginSID) and mints its token.
func (p *wsResolveIT) openSession(t *testing.T, loginSID string) string {
	t.Helper()
	exp := time.Now().Add(8 * time.Hour)
	if err := auth.NewWorkspaceSessionRepository(p.gdb).Create(t.Context(), &auth.WorkspaceSession{
		ID: p.wsID, TenantID: wsITCustID, ActorUserID: p.userID, ActorTenantID: wsITHomeID, ActorSessionID: loginSID,
		ActorLabel: "Builder IT (Xưởng IT)", ExpiresAt: exp, CreatedBy: p.userID,
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	tok, err := p.tokens.IssueAccessTokenUntil(auth.Subject{
		UserID: p.userID, TenantID: wsITCustID, IsAdmin: true, SessionID: loginSID,
		ActorUserID: p.userID, ActorTenantID: wsITHomeID, WorkspaceSessionID: p.wsID,
	}, exp)
	if err != nil {
		t.Fatalf("mint session token: %v", err)
	}
	return tok
}

func (p *wsResolveIT) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := p.gdb.Exec(sql, args...).Error; err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (p *wsResolveIT) layoutCount(t *testing.T, tenantID string) int64 {
	t.Helper()
	var n int64
	if err := p.gdb.Raw("SELECT COUNT(*) FROM sys_layouts WHERE tenant_id = ? AND entity_name = ?", tenantID, wsITEntity).Scan(&n).Error; err != nil {
		t.Fatalf("count layouts: %v", err)
	}
	return n
}

func TestWorkspaceResolve_WriteLandsInCustomerAndOperatorClusterStaysShut(t *testing.T) {
	p := newWSResolveIT(t)
	r := p.build(true)
	_, sid := p.homeLogin(t, r)
	tok := p.openSession(t, sid)

	// The write goes to the customer tenant — the workshop gets nothing.
	code, body := p.do(t, r, http.MethodPost, "/api/v1/admin/layouts", tok, map[string]any{
		"entityName": wsITEntity, "viewType": "edit", "layoutJson": map[string]any{"version": 1, "sections": []any{}},
	})
	if code != http.StatusOK {
		t.Fatalf("layout write with a session token: %d %s, want 200", code, body)
	}
	var out struct {
		Data struct {
			TenantID string `json:"tenantId"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Data.TenantID != wsITCustID {
		t.Errorf("layout tenantId = %q, want the customer %q", out.Data.TenantID, wsITCustID)
	}
	if got := p.layoutCount(t, wsITCustID); got != 1 {
		t.Errorf("customer layouts = %d, want 1", got)
	}
	if got := p.layoutCount(t, wsITHomeID); got != 0 {
		t.Errorf("workshop layouts = %d, want 0 — the write leaked into the home tenant", got)
	}

	// The operator-only cluster is shut: byte-identical 403, never the tenant list.
	const want = `{"code":"FORBIDDEN","error":"Yêu cầu quyền quản trị viên"}`
	if code, body := p.do(t, r, http.MethodGet, "/api/v1/admin/tenants", tok, nil); code != http.StatusForbidden || body != want {
		t.Errorf("GET /admin/tenants with a session token: %d %s, want 403 %s", code, body, want)
	}
}

// A home token is unchanged by the new tier: the builder still reaches /admin/tenants and a
// layout route on the workshop (RequireBuilderTier = the old live read).
func TestWorkspaceResolve_HomeTokenUnchanged(t *testing.T) {
	p := newWSResolveIT(t)
	r := p.build(true)
	home, _ := p.homeLogin(t, r)
	if code, body := p.do(t, r, http.MethodGet, "/api/v1/admin/tenants", home, nil); code != http.StatusOK {
		t.Fatalf("home builder GET /admin/tenants: %d %s, want 200", code, body)
	}
	// And a non-operator admin of the workshop is still refused by the tier.
	p.exec(t, "UPDATE users SET is_platform_operator = 0 WHERE tenant_id = ? AND id = ?", wsITHomeID, p.userID)
	if code, _ := p.do(t, r, http.MethodPost, "/api/v1/admin/layouts", home, map[string]any{
		"entityName": wsITEntity, "viewType": "edit", "layoutJson": map[string]any{"version": 1, "sections": []any{}},
	}); code != http.StatusForbidden {
		t.Fatalf("non-operator home admin on a builder route: %d, want 403", code)
	}
}

// Without the resolver wired, a delegation token is refused outright (fail closed).
func TestWorkspaceResolve_NoResolverFailsClosed(t *testing.T) {
	p := newWSResolveIT(t)
	r := p.build(true)
	_, sid := p.homeLogin(t, r)
	tok := p.openSession(t, sid)
	if code, body := p.do(t, p.build(false), http.MethodGet, "/api/v1/admin/tenants", tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("session token with no resolver: %d %s, want 401", code, body)
	}
}

// Every revocation path kills the NEXT request, through the real router.
func TestWorkspaceResolve_RevocationPaths(t *testing.T) {
	cases := []struct {
		name string
		kill func(t *testing.T, p *wsResolveIT, sid string)
	}{
		{"Về Nexus (session closed)", func(t *testing.T, p *wsResolveIT, _ string) {
			p.exec(t, "UPDATE sys_workspace_sessions SET closed_at = NOW(3), closed_reason = 'closed' WHERE tenant_id = ? AND id = ?", wsITCustID, p.wsID)
		}},
		{"ordinary logout of the home login", func(t *testing.T, p *wsResolveIT, sid string) {
			p.exec(t, "UPDATE refresh_tokens SET revoked_at = NOW() WHERE tenant_id = ? AND session_id = ?", wsITHomeID, sid)
		}},
		{"home session ended", func(t *testing.T, p *wsResolveIT, sid string) {
			p.exec(t, "UPDATE refresh_tokens SET revoked_at = NOW(), revoke_reason = 'ended' WHERE tenant_id = ? AND session_id = ?", wsITHomeID, sid)
		}},
		{"operator flag removed", func(t *testing.T, p *wsResolveIT, _ string) {
			p.exec(t, "UPDATE users SET is_platform_operator = 0 WHERE tenant_id = ? AND id = ?", wsITHomeID, p.userID)
		}},
		{"builder disabled", func(t *testing.T, p *wsResolveIT, _ string) {
			p.exec(t, "UPDATE users SET status = 'disabled' WHERE tenant_id = ? AND id = ?", wsITHomeID, p.userID)
		}},
		{"customer suspended", func(t *testing.T, p *wsResolveIT, _ string) {
			p.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", wsITCustID)
		}},
		{"workshop suspended", func(t *testing.T, p *wsResolveIT, _ string) {
			p.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", wsITHomeID)
		}},
		{"hard deadline passed", func(t *testing.T, p *wsResolveIT, _ string) {
			p.exec(t, "UPDATE sys_workspace_sessions SET expires_at = NOW(3) - INTERVAL 1 SECOND WHERE tenant_id = ? AND id = ?", wsITCustID, p.wsID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newWSResolveIT(t)
			r := p.build(true)
			_, sid := p.homeLogin(t, r)
			tok := p.openSession(t, sid)
			probe := func() int {
				code, _ := p.do(t, r, http.MethodGet, "/api/v1/metadata/layouts/"+wsITEntity, tok, nil)
				return code
			}
			if code := probe(); code == http.StatusUnauthorized {
				t.Fatalf("a fresh session token was refused (%d) before any revocation", code)
			}
			tc.kill(t, p, sid)
			if code := probe(); code != http.StatusUnauthorized {
				t.Fatalf("after %s: status %d, want 401 on the very next request", tc.name, code)
			}
		})
	}
}
