package handlers

// specs/062-customer-workspace T013 — the two workspace-session routes at HANDLER level (Subject
// injected, service faked): the status/code table, 201 vs 200, and the 204 of a duplicate close. The
// gates in FRONT of the handler (builderGated, RefuseActingSession) are pinned by
// router_workspace_gate_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

type fakeWorkspaceSvc struct {
	openRes   *services.WorkspaceOpenResult
	openErr   error
	closeErr  error
	gotOpen   services.WorkspaceOpenInput
	gotHome   auth.Subject
	openCalls int
	closeSubs []auth.Subject
}

func (f *fakeWorkspaceSvc) Open(_ context.Context, home auth.Subject, in services.WorkspaceOpenInput) (*services.WorkspaceOpenResult, error) {
	f.openCalls++
	f.gotOpen, f.gotHome = in, home
	return f.openRes, f.openErr
}

func (f *fakeWorkspaceSvc) Close(_ context.Context, sub auth.Subject) error {
	f.closeSubs = append(f.closeSubs, sub)
	return f.closeErr
}

func wsHandlerRouter(svc workspaceSessionService, sub auth.Subject, tokenType auth.TokenType) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", sub.TenantID)
		c.Set("authClaims", &auth.Claims{Subject: sub, TokenType: tokenType})
		c.Next()
	})
	NewAdminWorkspaceSessionHandler(svc, zap.NewNop()).RegisterRoutes(r.Group("/api/v1"))
	return r
}

func wsCall(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

var (
	wsHomeSub    = auth.Subject{UserID: "builder-1", TenantID: "home-1", IsAdmin: true, SessionID: "sid", IsPlatformOperator: true}
	wsSessionSub = auth.Subject{
		UserID: "builder-1", TenantID: "customer-1", IsAdmin: true, SessionID: "sid", IsPlatformOperator: true,
		ActorUserID: "builder-1", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
	}
)

func TestAdminWorkspaceSessionHandler_Open(t *testing.T) {
	exp := time.Date(2026, 10, 7, 17, 0, 0, 0, time.UTC)
	res := func(created bool) *services.WorkspaceOpenResult {
		return &services.WorkspaceOpenResult{
			AccessToken: "ws.token", ExpiresAt: exp, Created: created,
			Session: services.WorkspaceSessionView{ID: "ws-1", TenantID: "customer-1", TenantSlug: "khach", TenantName: "Khách", OpenedAt: exp, ExpiresAt: exp, Resumed: !created},
		}
	}
	cases := []struct {
		name     string
		body     string
		svc      *fakeWorkspaceSvc
		tokenTyp auth.TokenType
		want     int
		wantCode string
	}{
		{"new session is 201", `{"tenantSlug":"khach"}`, &fakeWorkspaceSvc{openRes: res(true)}, auth.TokenTypeAccess, http.StatusCreated, ""},
		{"reused / resumed session is 200", `{"tenantSlug":"khach","resumeSessionId":"ws-1"}`, &fakeWorkspaceSvc{openRes: res(false)}, auth.TokenTypeAccess, http.StatusOK, ""},
		{"empty slug is 422", `{"tenantSlug":""}`, &fakeWorkspaceSvc{openErr: services.ErrWorkspaceSlugRequired}, auth.TokenTypeAccess, http.StatusUnprocessableEntity, "WORKSPACE_SLUG_REQUIRED"},
		{"home tenant is 422", `{"tenantSlug":"home"}`, &fakeWorkspaceSvc{openErr: services.ErrWorkspaceIsHome}, auth.TokenTypeAccess, http.StatusUnprocessableEntity, "WORKSPACE_IS_HOME"},
		{"unknown tenant is 404", `{"tenantSlug":"x"}`, &fakeWorkspaceSvc{openErr: services.ErrWorkspaceTenantNotFound}, auth.TokenTypeAccess, http.StatusNotFound, "TENANT_NOT_FOUND"},
		{"dead resume id is 404", `{"tenantSlug":"khach","resumeSessionId":"gone"}`, &fakeWorkspaceSvc{openErr: services.ErrWorkspaceSessionEnded}, auth.TokenTypeAccess, http.StatusNotFound, "WORKSPACE_SESSION_ENDED"},
		{"dead home login is 401", `{"tenantSlug":"khach"}`, &fakeWorkspaceSvc{openErr: services.ErrWorkspaceUnauthorized}, auth.TokenTypeAccess, http.StatusUnauthorized, "UNAUTHORIZED"},
		{"nested open is 403", `{"tenantSlug":"khach"}`, &fakeWorkspaceSvc{openErr: services.ErrWorkspaceNotAllowed}, auth.TokenTypeAccess, http.StatusForbidden, "WORKSPACE_SESSION_NOT_ALLOWED"},
		{"unexpected error is 500", `{"tenantSlug":"khach"}`, &fakeWorkspaceSvc{openErr: errors.New("db down")}, auth.TokenTypeAccess, http.StatusInternalServerError, "WORKSPACE_SESSION_OPEN_FAILED"},
		{"malformed body is 400", `{`, &fakeWorkspaceSvc{}, auth.TokenTypeAccess, http.StatusBadRequest, ""},
		{"an API key principal is refused before the service", `{"tenantSlug":"khach"}`, &fakeWorkspaceSvc{}, auth.TokenTypeAPIKey, http.StatusForbidden, "FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := wsCall(wsHandlerRouter(tc.svc, wsHomeSub, tc.tokenTyp), http.MethodPost, "/api/v1/admin/workspace-sessions", tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), tc.want)
			}
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if tc.wantCode != "" && out["code"] != tc.wantCode {
				t.Errorf("code = %v, want %s", out["code"], tc.wantCode)
			}
			if tc.want == http.StatusForbidden && tc.tokenTyp == auth.TokenTypeAPIKey && tc.svc.openCalls != 0 {
				t.Error("service called for an API-key principal")
			}
		})
	}

	t.Run("the 2xx body and the home subject passed to the service", func(t *testing.T) {
		svc := &fakeWorkspaceSvc{openRes: res(true)}
		w := wsCall(wsHandlerRouter(svc, wsHomeSub, auth.TokenTypeAccess), http.MethodPost, "/api/v1/admin/workspace-sessions", `{"tenantSlug":"khach","resumeSessionId":"ws-1"}`)
		var out struct {
			Data struct {
				AccessToken string `json:"accessToken"`
				ExpiresAt   string `json:"expiresAt"`
				Session     struct {
					ID, TenantID, TenantSlug, TenantName string
					Resumed                              bool
				} `json:"session"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Data.AccessToken != "ws.token" || out.Data.ExpiresAt != "2026-10-07T17:00:00Z" || out.Data.Session.ID != "ws-1" || out.Data.Session.TenantSlug != "khach" {
			t.Errorf("body = %+v", out.Data)
		}
		if svc.gotHome != wsHomeSub || svc.gotOpen.TenantSlug != "khach" || svc.gotOpen.ResumeSessionID != "ws-1" {
			t.Errorf("service got home=%+v in=%+v", svc.gotHome, svc.gotOpen)
		}
	})
}

func TestAdminWorkspaceSessionHandler_Close(t *testing.T) {
	t.Run("session token closes (204)", func(t *testing.T) {
		svc := &fakeWorkspaceSvc{}
		w := wsCall(wsHandlerRouter(svc, wsSessionSub, auth.TokenTypeAccess), http.MethodDelete, "/api/v1/admin/workspace-sessions/current", "")
		if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
			t.Fatalf("status = %d body=%q, want an empty 204", w.Code, w.Body.String())
		}
		if len(svc.closeSubs) != 1 || svc.closeSubs[0] != wsSessionSub {
			t.Errorf("service got %+v", svc.closeSubs)
		}
	})
	t.Run("a second close that already passed the Auth stage is 204 too", func(t *testing.T) {
		svc := &fakeWorkspaceSvc{} // service reports nil when RowsAffected = 0
		r := wsHandlerRouter(svc, wsSessionSub, auth.TokenTypeAccess)
		for i := 0; i < 2; i++ {
			if w := wsCall(r, http.MethodDelete, "/api/v1/admin/workspace-sessions/current", ""); w.Code != http.StatusNoContent {
				t.Fatalf("close #%d: %d, want 204", i+1, w.Code)
			}
		}
	})
	t.Run("a home token holds no session: 404", func(t *testing.T) {
		svc := &fakeWorkspaceSvc{closeErr: services.ErrWorkspaceSessionNotFound}
		w := wsCall(wsHandlerRouter(svc, wsHomeSub, auth.TokenTypeAccess), http.MethodDelete, "/api/v1/admin/workspace-sessions/current", "")
		if w.Code != http.StatusNotFound || !bytes.Contains(w.Body.Bytes(), []byte("WORKSPACE_SESSION_NOT_FOUND")) {
			t.Fatalf("status = %d %s, want 404 WORKSPACE_SESSION_NOT_FOUND", w.Code, w.Body.String())
		}
	})
	t.Run("unexpected error is 500", func(t *testing.T) {
		svc := &fakeWorkspaceSvc{closeErr: errors.New("db down")}
		w := wsCall(wsHandlerRouter(svc, wsSessionSub, auth.TokenTypeAccess), http.MethodDelete, "/api/v1/admin/workspace-sessions/current", "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}
