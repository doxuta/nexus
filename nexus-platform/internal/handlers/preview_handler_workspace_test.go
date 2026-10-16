package handlers

// preview_handler_workspace_test.go — specs/064-preview-as-role-sample-data T120 (S9-8): a builder
// working INSIDE a customer's workspace (workspace-session token: uid=act, tid=customer, att=home
// workshop, wsid) opens "view as role" through the real production gate chain — Auth (workspace
// resolver) → Tenant → PreviewReadOnly → RequireAdmin → RequireBuilderTier (D-6 of specs/062).

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

type phWorkspaces struct{ calls int }

func (w *phWorkspaces) GetLive(_ context.Context, tenantID, id, _, _, _ string, _ time.Time) (*auth.WorkspaceSessionLive, error) {
	w.calls++
	return &auth.WorkspaceSessionLive{ID: id, TenantID: tenantID}, nil
}

func newWorkspacePreviewHarness(t *testing.T) (*previewHarness, *statefulResolver) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tokens, err := auth.NewTokenService(phSecret, 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	h := &previewHarness{store: &phStore{rows: map[string]*auth.PreviewSession{}}, actors: &phActors{}}
	svc := services.NewPreviewService(h.store, h.actors, phRoles{}, tokens, 30*time.Minute).WithWorkspaceSessions(&phWorkspaces{})
	resolver := &statefulResolver{}
	ws := func(uid string) *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{
			UserID: uid, TenantID: phTenant, SessionID: "sid-op",
			ActorUserID: uid, ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
		}, TokenType: auth.TokenTypeAccess}
	}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-ws": ws("u-op"),
		// A token with a wsid but a missing att: the Auth stage refuses it before any handler.
		"tok-ws-partial": {Subject: auth.Subject{UserID: "u-op", TenantID: phTenant, SessionID: "sid-op", WorkspaceSessionID: "ws-1"}, TokenType: auth.TokenTypeAccess},
	}}
	h.r = gin.New()
	RegisterAPIRoutes(h.r.Group("/api/v1"), ver, nil, APIHandlers{Preview: NewPreviewHandler(svc, zap.NewNop())}, zap.NewNop(),
		WithOperatorTier(middleware.RequireBuilderTier(&operatorRepoFake{}, zap.NewNop())),
		WithWorkspaceResolver(resolver),
	)
	return h, resolver
}

func TestPreviewHandler_StartInsideCustomerWorkspace(t *testing.T) {
	h, _ := newWorkspacePreviewHarness(t)
	code, out := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", "tok-ws", `{"roleId":"`+phRole+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d (%+v), want 201 through builderGated for a workspace-session token", code, out)
	}
	row := h.store.rows[phTenant+"|"+out.Data.SessionID]
	if row == nil {
		t.Fatal("201 without a sys_preview_sessions row")
	}
	if row.TenantID != phTenant || row.ActorTenantID != "home-1" || row.ActorUserID != "u-op" ||
		row.WorkspaceSessionID == nil || *row.WorkspaceSessionID != "ws-1" {
		t.Errorf("row = %+v; want tenant=customer, actor_tenant=home-1 (att), workspace_session_id=ws-1", row)
	}
}

func TestPreviewHandler_StartInsideWorkspace_Refusals(t *testing.T) {
	t.Run("role of another customer", func(t *testing.T) {
		h, _ := newWorkspacePreviewHarness(t)
		code, out := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", "tok-ws", `{"roleId":"`+phOtherRole+`"}`)
		if code != http.StatusUnprocessableEntity || out.Code != "PREVIEW_ROLE_NOT_FOUND" {
			t.Fatalf("status %d code %q, want 422 PREVIEW_ROLE_NOT_FOUND", code, out.Code)
		}
		if len(h.store.rows) != 0 {
			t.Error("a refused start wrote a session row")
		}
	})
	t.Run("workspace session closed ⇒ Auth stage 401, no row", func(t *testing.T) {
		h, resolver := newWorkspacePreviewHarness(t)
		resolver.closed = true
		code, _ := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", "tok-ws", `{"roleId":"`+phRole+`"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", code)
		}
		if len(h.store.rows) != 0 {
			t.Error("a refused start wrote a session row")
		}
	})
	t.Run("partial delegation claims ⇒ 401", func(t *testing.T) {
		h, _ := newWorkspacePreviewHarness(t)
		code, _ := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", "tok-ws-partial", `{"roleId":"`+phRole+`"}`)
		if code != http.StatusUnauthorized {
			t.Fatalf("status %d, want 401", code)
		}
	})
}
