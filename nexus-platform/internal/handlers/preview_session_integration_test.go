//go:build integration

// specs/064-preview-as-role-sample-data T011 (integration half) — a preview-as-role token over the
// REAL chain (login → POST /admin/preview-sessions → composite verifier → PreviewService.Verify →
// AuthMiddlewareLive → PreviewReadOnly) against MySQL 8, killed by every real revocation path on
// the very next request. The point of the uncached ActorLive check is the paths that do NOT mark a
// session "ended": a password change and the E5 reuse alarm (RevokeAllForUser, revoke_reason NULL),
// a demotion / disable / operator flag removed by SQL, a suspended home tenant.
//
//	go test -tags=integration ./internal/handlers/ -run TestPreviewSession
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
	pvITTenantID = "a1a1a1a1-0000-0000-0000-000000006401"
	pvITSlug     = "it-preview"
	pvITRoleID   = "a1a1a1a1-0000-0000-0000-000000006402"
	pvITEmail    = "builder@it-preview.test"
	pvITPassword = "Pr3view-Builder-Pw!"
)

type previewIT struct {
	r      *gin.Engine
	gdb    *gorm.DB
	tokens *auth.TokenService
	userID string
}

func newPreviewIT(t *testing.T) *previewIT {
	t.Helper()
	gdb := authGormDB(t)
	p := &previewIT{gdb: gdb, userID: uuid.NewString()}

	clean := func() {
		gdb.Exec("DELETE FROM sys_preview_sessions WHERE tenant_id = ?", pvITTenantID)
		gdb.Where("tenant_id = ?", pvITTenantID).Delete(&auth.RefreshToken{})
		gdb.Unscoped().Where("tenant_id = ?", pvITTenantID).Delete(&auth.User{})
		gdb.Where("tenant_id = ?", pvITTenantID).Delete(&auth.Role{})
		gdb.Unscoped().Where("id = ?", pvITTenantID).Delete(&auth.Tenant{})
	}
	clean()
	t.Cleanup(clean)

	now := time.Now()
	if err := gdb.Create(&auth.Tenant{ID: pvITTenantID, Slug: pvITSlug, Name: "Preview (IT)", Status: "active", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pvITPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := gdb.Create(&auth.User{ID: p.userID, TenantID: pvITTenantID, Email: pvITEmail, PasswordHash: string(hash),
		FullName: "Builder IT", Status: "active", IsAdmin: true, IsPlatformOperator: true, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed builder: %v", err)
	}
	if err := gdb.Create(&auth.Role{ID: pvITRoleID, TenantID: pvITTenantID, Name: "it_sales_rep", Label: "Nhân viên IT", CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed role: %v", err)
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
	previewSvc := services.NewPreviewService(auth.NewPreviewSessionRepository(gdb), auth.NewWorkspaceSessionRepository(gdb),
		auth.NewRoleRepository(gdb), tokens, 30*time.Minute)

	gin.SetMode(gin.TestMode)
	p.r = gin.New()
	RegisterAPIRoutes(p.r.Group("/api/v1"), services.NewCompositeVerifier(tokens, nil).WithPreview(previewSvc), nil, APIHandlers{
		Auth:     NewAuthHandler(authSvc, zap.NewNop()),
		Metadata: NewMetadataHandler(gdb, zap.NewNop()),
		Preview:  NewPreviewHandler(previewSvc, zap.NewNop()),
	}, zap.NewNop(),
		WithOperatorTier(middleware.RequirePlatformOperator(users, zap.NewNop())),
		WithLiveSubject(users),
		WithSessionGuard(guard),
	)
	return p
}

func (p *previewIT) do(t *testing.T, method, path, token string, body any) (int, map[string]any) {
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

// login returns (access, refresh, sid) of a fresh login session of the builder.
func (p *previewIT) login(t *testing.T, password string) (string, string, string) {
	t.Helper()
	code, out := p.do(t, http.MethodPost, "/api/v1/auth/login", "", map[string]string{"tenant": pvITSlug, "email": pvITEmail, "password": password})
	if code != http.StatusOK {
		t.Fatalf("login: %d %v", code, out)
	}
	data := out["data"].(map[string]any)
	access, refresh := data["accessToken"].(string), data["refreshToken"].(string)
	claims, err := p.tokens.VerifyAccessToken(access)
	if err != nil || claims.SessionID == "" {
		t.Fatalf("login access token: %+v %v", claims, err)
	}
	return access, refresh, claims.SessionID
}

// start opens a preview session from the given builder access token and returns its token.
func (p *previewIT) start(t *testing.T, access string) string {
	t.Helper()
	code, out := p.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", access, map[string]string{"roleId": pvITRoleID})
	if code != http.StatusCreated {
		t.Fatalf("start preview: %d %v", code, out)
	}
	return out["data"].(map[string]any)["token"].(string)
}

// alive reports whether the preview token still authenticates a read.
func (p *previewIT) alive(t *testing.T, previewTok string) bool {
	t.Helper()
	code, out := p.do(t, http.MethodGet, "/api/v1/metadata/entities", previewTok, nil)
	switch code {
	case http.StatusOK:
		return true
	case http.StatusUnauthorized:
		return false
	}
	t.Fatalf("probe: unexpected %d %v", code, out)
	return false
}

func (p *previewIT) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if err := p.gdb.Exec(sql, args...).Error; err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestPreviewSession_RevocationPaths(t *testing.T) {
	p := newPreviewIT(t)

	t.Run("read-only and not admin", func(t *testing.T) {
		access, _, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		if !p.alive(t, tok) {
			t.Fatal("fresh preview token refused")
		}
		if code, out := p.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", tok, map[string]string{"roleId": pvITRoleID}); code != http.StatusForbidden || out["code"] != "PREVIEW_READ_ONLY" {
			t.Fatalf("preview opening a preview: %d %v, want 403 PREVIEW_READ_ONLY", code, out)
		}
		if code, out := p.do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refreshToken": tok}); code != http.StatusUnauthorized {
			t.Fatalf("preview token as a refresh token: %d %v, want 401", code, out)
		}
		if code, _ := p.do(t, http.MethodGet, "/api/v1/auth/me", tok, nil); code != http.StatusUnauthorized {
			t.Fatalf("GET /auth/me with a preview token before S9-2: %d, want 401 (door not opened yet)", code)
		}
	})

	t.Run("survives refresh rotation, dies on refresh-token reuse (E5 RevokeAllForUser)", func(t *testing.T) {
		access, refresh1, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		code, out := p.do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refreshToken": refresh1})
		if code != http.StatusOK {
			t.Fatalf("rotate: %d %v", code, out)
		}
		if !p.alive(t, tok) {
			t.Fatal("rotation killed the preview (the successor row keeps the same sid)")
		}
		if code, _ := p.do(t, http.MethodPost, "/api/v1/auth/refresh", "", map[string]string{"refreshToken": refresh1}); code != http.StatusUnauthorized {
			t.Fatalf("replay of a rotated refresh token: %d, want 401", code)
		}
		if p.alive(t, tok) {
			t.Fatal("preview survived the E5 family revoke")
		}
	})

	t.Run("logout", func(t *testing.T) {
		access, refresh, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		if code, _ := p.do(t, http.MethodPost, "/api/v1/auth/logout", "", map[string]string{"refreshToken": refresh}); code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("logout: %d", code)
		}
		if p.alive(t, tok) {
			t.Fatal("preview survived the builder's logout")
		}
	})

	t.Run("end session", func(t *testing.T) {
		access, _, sid := p.login(t, pvITPassword)
		other, _, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		if code, out := p.do(t, http.MethodDelete, "/api/v1/auth/sessions/"+sid, other, nil); code != http.StatusNoContent {
			t.Fatalf("end session: %d %v", code, out)
		}
		if p.alive(t, tok) {
			t.Fatal("preview survived its login session being ended")
		}
	})

	t.Run("logout everywhere", func(t *testing.T) {
		access, _, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		if code, out := p.do(t, http.MethodPost, "/api/v1/auth/logout-all", access, nil); code != http.StatusNoContent && code != http.StatusOK {
			t.Fatalf("logout-all: %d %v", code, out)
		}
		if p.alive(t, tok) {
			t.Fatal("preview survived logout-all")
		}
	})

	for _, tc := range []struct{ name, kill, restore string }{
		{"demoted (is_admin=0)", "UPDATE users SET is_admin = 0 WHERE id = ?", "UPDATE users SET is_admin = 1 WHERE id = ?"},
		{"operator flag removed by SQL/CLI", "UPDATE users SET is_platform_operator = 0 WHERE id = ?", "UPDATE users SET is_platform_operator = 1 WHERE id = ?"},
		{"account disabled", "UPDATE users SET status = 'disabled' WHERE id = ?", "UPDATE users SET status = 'active' WHERE id = ?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access, _, _ := p.login(t, pvITPassword)
			tok := p.start(t, access)
			p.exec(t, tc.kill, p.userID)
			defer p.exec(t, tc.restore, p.userID)
			if p.alive(t, tok) {
				t.Fatalf("preview survived: %s (the builder check must not be cached)", tc.name)
			}
		})
	}

	t.Run("home tenant suspended", func(t *testing.T) {
		access, _, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		p.exec(t, "UPDATE tenants SET status = 'suspended' WHERE id = ?", pvITTenantID)
		defer p.exec(t, "UPDATE tenants SET status = 'active' WHERE id = ?", pvITTenantID)
		if p.alive(t, tok) {
			t.Fatal("preview survived a suspended home tenant")
		}
	})

	t.Run("admin without the operator flag cannot start", func(t *testing.T) {
		p.exec(t, "UPDATE users SET is_platform_operator = 0 WHERE id = ?", p.userID)
		defer p.exec(t, "UPDATE users SET is_platform_operator = 1 WHERE id = ?", p.userID)
		access, _, _ := p.login(t, pvITPassword)
		if code, _ := p.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", access, map[string]string{"roleId": pvITRoleID}); code != http.StatusForbidden {
			t.Fatalf("legacy customer admin started a preview: %d, want 403", code)
		}
	})

	t.Run("exit ends the session", func(t *testing.T) {
		access, _, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		if code, out := p.do(t, http.MethodDelete, "/api/v1/preview-sessions/current?reason=exit", tok, nil); code != http.StatusNoContent {
			t.Fatalf("exit: %d %v", code, out)
		}
		if p.alive(t, tok) {
			t.Fatal("preview token alive after exit")
		}
	})

	// Last: it changes the password.
	t.Run("password changed from another session (RevokeAllForUser, revoke_reason NULL)", func(t *testing.T) {
		access, _, _ := p.login(t, pvITPassword)
		other, _, _ := p.login(t, pvITPassword)
		tok := p.start(t, access)
		code, out := p.do(t, http.MethodPut, "/api/v1/auth/me/password", other,
			map[string]string{"currentPassword": pvITPassword, "newPassword": "An0ther-Builder-Pw!"})
		if code != http.StatusOK && code != http.StatusNoContent {
			t.Fatalf("change password: %d %v", code, out)
		}
		if p.alive(t, tok) {
			t.Fatal("preview survived a password change — a leaked 15-minute access token would buy a 30–60-minute preview")
		}
	})
}
