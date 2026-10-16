//go:build integration

// specs/064-preview-as-role-sample-data T120/T124 (S9-8) — "view as role" opened from INSIDE a
// customer's workspace, over the REAL chain against MySQL 8: builder login → workspace-session token
// → POST /admin/preview-sessions (builderGated, workspace resolver) → composite verifier →
// PreviewService.Verify (ActorLive in the builder's workshop + GetLive on the workspace session) →
// AuthMiddlewareLive → PreviewReadOnly. Every way the workspace or the builder can die must kill
// the preview on the very next request.
//
//	go test -tags=integration ./internal/handlers/ -run TestPreviewInWorkspace
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
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

const (
	pwITHomeID   = "a1a1a1a1-0000-0000-0000-000000006411"
	pwITCustID   = "a1a1a1a1-0000-0000-0000-000000006412"
	pwITHomeSlug = "it-pw-home"
	pwITCustSlug = "it-pw-cust"
	pwITCustRole = "a1a1a1a1-0000-0000-0000-000000006413"
	pwITHomeRole = "a1a1a1a1-0000-0000-0000-000000006414" // a role of the WORKSHOP — "another customer" from K's view
	pwITEmail    = "builder@it-pw-home.test"
	pwITPassword = "Pw-Builder-Pw-1!"
)

type previewWSIT struct {
	r      *gin.Engine
	gdb    *gorm.DB
	tokens *auth.TokenService
	userID string
	wsID   string
}

func newPreviewWSIT(t *testing.T) *previewWSIT {
	t.Helper()
	gdb := authGormDB(t)
	p := &previewWSIT{gdb: gdb, userID: uuid.NewString(), wsID: uuid.NewString()}
	ids := []string{pwITHomeID, pwITCustID}

	clean := func() {
		gdb.Exec("DELETE FROM sys_preview_sessions WHERE tenant_id IN ?", ids)
		gdb.Exec("DELETE FROM sys_workspace_sessions WHERE tenant_id IN ?", ids)
		gdb.Where("tenant_id IN ?", ids).Delete(&auth.RefreshToken{})
		gdb.Unscoped().Where("tenant_id IN ?", ids).Delete(&auth.User{})
		gdb.Where("tenant_id IN ?", ids).Delete(&auth.Role{})
		gdb.Unscoped().Where("id IN ?", ids).Delete(&auth.Tenant{})
	}
	clean()
	t.Cleanup(clean)

	now := time.Now()
	for _, tn := range []auth.Tenant{
		{ID: pwITHomeID, Slug: pwITHomeSlug, Name: "Xưởng IT", Status: "active", CreatedAt: now, UpdatedAt: now},
		{ID: pwITCustID, Slug: pwITCustSlug, Name: "Khách IT", Status: "active", CreatedAt: now, UpdatedAt: now},
	} {
		if err := gdb.Create(&tn).Error; err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pwITPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := gdb.Create(&auth.User{ID: p.userID, TenantID: pwITHomeID, Email: pwITEmail, PasswordHash: string(hash),
		FullName: "Builder IT", Status: "active", IsAdmin: true, IsPlatformOperator: true, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed builder: %v", err)
	}
	for _, r := range []auth.Role{
		{ID: pwITCustRole, TenantID: pwITCustID, Name: "it_cust_rep", Label: "Nhân viên khách", CreatedAt: now, UpdatedAt: now},
		{ID: pwITHomeRole, TenantID: pwITHomeID, Name: "it_home_role", Label: "Vai xưởng", CreatedAt: now, UpdatedAt: now},
	} {
		if err := gdb.Create(&r).Error; err != nil {
			t.Fatalf("seed role: %v", err)
		}
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
	wsSvc := services.NewWorkspaceSessionService(wsRepo, users, tokens)
	previewSvc := services.NewPreviewService(auth.NewPreviewSessionRepository(gdb), wsRepo, auth.NewRoleRepository(gdb), tokens, 30*time.Minute).
		WithWorkspaceSessions(wsRepo)

	gin.SetMode(gin.TestMode)
	p.r = gin.New()
	RegisterAPIRoutes(p.r.Group("/api/v1"), services.NewCompositeVerifier(tokens, nil).WithPreview(previewSvc), nil, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Preview:  NewPreviewHandler(previewSvc, zap.NewNop()),
	}, zap.NewNop(),
		WithOperatorTier(middleware.RequireBuilderTier(users, zap.NewNop())),
		WithLiveSubject(users),
		WithSessionGuard(guard),
		WithWorkspaceResolver(wsSvc),
	)
	return p
}

func (p *previewWSIT) do(t *testing.T, method, path, token string, body any) (int, map[string]any) {
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
	p.r.ServeHTTP(w, req)
	out := map[string]any{}
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &out)
	}
	return w.Code, out
}

// openWorkspace logs the builder in for real and opens a workspace session in the customer; it returns
// the workspace-session token and the home login's refresh token.
func (p *previewWSIT) openWorkspace(t *testing.T) (wsToken, refresh string) {
	t.Helper()
	code, out := p.do(t, http.MethodPost, "/api/v1/auth/login", "",
		map[string]string{"tenant": pwITHomeSlug, "email": pwITEmail, "password": pwITPassword})
	if code != http.StatusOK {
		t.Fatalf("login: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	access, refresh := data["accessToken"].(string), data["refreshToken"].(string)
	claims, err := p.tokens.VerifyAccessToken(access)
	if err != nil || claims.SessionID == "" {
		t.Fatalf("login token: %+v %v", claims, err)
	}
	p.wsID = uuid.NewString()
	exp := time.Now().Add(8 * time.Hour)
	if err := auth.NewWorkspaceSessionRepository(p.gdb).Create(t.Context(), &auth.WorkspaceSession{
		ID: p.wsID, TenantID: pwITCustID, ActorUserID: p.userID, ActorTenantID: pwITHomeID, ActorSessionID: claims.SessionID,
		ActorLabel: "Builder IT (Xưởng IT)", ExpiresAt: exp, CreatedBy: p.userID,
	}); err != nil {
		t.Fatalf("create workspace session: %v", err)
	}
	wsToken, err = p.tokens.IssueAccessTokenUntil(auth.Subject{
		UserID: p.userID, TenantID: pwITCustID, IsAdmin: true, SessionID: claims.SessionID,
		ActorUserID: p.userID, ActorTenantID: pwITHomeID, WorkspaceSessionID: p.wsID,
	}, exp)
	if err != nil {
		t.Fatalf("mint workspace token: %v", err)
	}
	return wsToken, refresh
}

func (p *previewWSIT) startPreview(t *testing.T, wsToken string) string {
	t.Helper()
	code, out := p.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", wsToken, map[string]string{"roleId": pwITCustRole})
	if code != http.StatusCreated {
		t.Fatalf("start preview in workspace: %d %v", code, out)
	}
	return out["data"].(map[string]any)["token"].(string)
}

func (p *previewWSIT) alive(t *testing.T, tok string) bool {
	t.Helper()
	code, out := p.do(t, http.MethodGet, "/api/v1/metadata/entities", tok, nil)
	switch code {
	case http.StatusOK:
		return true
	case http.StatusUnauthorized:
		return false
	}
	t.Fatalf("probe: unexpected %d %v", code, out)
	return false
}

func (p *previewWSIT) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := p.gdb.Exec(sql, args...).Error; err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestPreviewInWorkspace_StartAndRow(t *testing.T) {
	p := newPreviewWSIT(t)
	ws, _ := p.openWorkspace(t)
	tok := p.startPreview(t, ws)
	if !p.alive(t, tok) {
		t.Fatal("fresh preview token refused")
	}

	var row struct {
		TenantID, ActorTenantID, ActorUserID string
		WorkspaceSessionID                   *string
	}
	if err := p.gdb.Raw("SELECT tenant_id, actor_tenant_id, actor_user_id, workspace_session_id FROM sys_preview_sessions WHERE tenant_id = ?",
		pwITCustID).Scan(&row).Error; err != nil {
		t.Fatalf("read row: %v", err)
	}
	if row.TenantID != pwITCustID || row.ActorTenantID != pwITHomeID || row.ActorUserID != p.userID ||
		row.WorkspaceSessionID == nil || *row.WorkspaceSessionID != p.wsID {
		t.Fatalf("row = %+v; want tenant=customer, actor_tenant=workshop, workspace_session_id=%s", row, p.wsID)
	}

	// Read-only and not admin, exactly like a home preview.
	if code, out := p.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", tok, map[string]string{"roleId": pwITCustRole}); code != http.StatusForbidden || out["code"] != "PREVIEW_READ_ONLY" {
		t.Fatalf("preview opening a preview: %d %v, want 403 PREVIEW_READ_ONLY", code, out)
	}
	// A role of ANOTHER tenant (here the workshop's own) is not previewable from the customer.
	if code, out := p.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", ws, map[string]string{"roleId": pwITHomeRole}); code != http.StatusUnprocessableEntity || out["code"] != "PREVIEW_ROLE_NOT_FOUND" {
		t.Fatalf("role of another tenant: %d %v, want 422 PREVIEW_ROLE_NOT_FOUND", code, out)
	}
	// Exit works and kills the token.
	if code, out := p.do(t, http.MethodDelete, "/api/v1/preview-sessions/current?reason=exit", tok, nil); code != http.StatusNoContent {
		t.Fatalf("exit: %d %v", code, out)
	}
	if p.alive(t, tok) {
		t.Fatal("preview token alive after exit")
	}
}

func TestPreviewInWorkspace_RevocationPaths(t *testing.T) {
	cases := []struct {
		name    string
		kill    func(t *testing.T, p *previewWSIT, refresh string)
		restore func(t *testing.T, p *previewWSIT)
	}{
		{"workspace session closed (Về Nexus)", func(t *testing.T, p *previewWSIT, _ string) {
			p.exec(t, "UPDATE sys_workspace_sessions SET closed_at = NOW(), closed_reason = 'builder' WHERE id = ?", p.wsID)
		}, nil},
		{"workspace session past its hard deadline", func(t *testing.T, p *previewWSIT, _ string) {
			p.exec(t, "UPDATE sys_workspace_sessions SET expires_at = DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE id = ?", p.wsID)
		}, nil},
		{"customer suspended", func(t *testing.T, p *previewWSIT, _ string) {
			p.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", pwITCustID)
		}, func(t *testing.T, p *previewWSIT) {
			p.exec(t, "UPDATE tenants SET status = 'active' WHERE id = ?", pwITCustID)
		}},
		{"builder demoted", func(t *testing.T, p *previewWSIT, _ string) {
			p.exec(t, "UPDATE users SET is_admin = 0 WHERE id = ?", p.userID)
		}, nil},
		{"operator flag removed by SQL/CLI", func(t *testing.T, p *previewWSIT, _ string) {
			p.exec(t, "UPDATE users SET is_platform_operator = 0 WHERE id = ?", p.userID)
		}, nil},
		{"builder logged out of the workshop", func(t *testing.T, p *previewWSIT, refresh string) {
			if code, _ := p.do(t, http.MethodPost, "/api/v1/auth/logout", "", map[string]string{"refreshToken": refresh}); code != http.StatusOK && code != http.StatusNoContent {
				t.Fatalf("logout: %d", code)
			}
		}, nil},
		{"workshop suspended", func(t *testing.T, p *previewWSIT, _ string) {
			p.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", pwITHomeID)
		}, func(t *testing.T, p *previewWSIT) {
			p.exec(t, "UPDATE tenants SET status = 'active' WHERE id = ?", pwITHomeID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPreviewWSIT(t)
			ws, refresh := p.openWorkspace(t)
			tok := p.startPreview(t, ws)
			if !p.alive(t, tok) {
				t.Fatal("fresh preview token refused")
			}
			tc.kill(t, p, refresh)
			if tc.restore != nil {
				defer tc.restore(t, p)
			}
			if p.alive(t, tok) {
				t.Fatalf("preview survived: %s (the workspace + builder checks must not be cached)", tc.name)
			}
		})
	}
}
