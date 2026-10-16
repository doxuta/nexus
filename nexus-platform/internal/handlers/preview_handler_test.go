package handlers

// preview_handler_test.go — specs/064-preview-as-role-sample-data T015. The real PreviewService over
// in-memory fakes, mounted through RegisterAPIRoutes with the operator tier, so every gate the
// production chain applies (RequireAdmin → operator tier → PreviewReadOnly) is in the path.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

const (
	phTenant    = "t1"
	phRole      = "11111111-aaaa-4aaa-8aaa-111111111111"
	phOtherRole = "22222222-bbbb-4bbb-8bbb-222222222222"
	phSecret    = "preview-handler-test-secret-0123456789"
)

type phStore struct {
	rows map[string]*auth.PreviewSession
}

func (s *phStore) Create(_ context.Context, r *auth.PreviewSession) error {
	cp := *r
	s.rows[r.TenantID+"|"+r.ID] = &cp
	return nil
}

func (s *phStore) GetByID(_ context.Context, tenantID, id string) (*auth.PreviewSession, error) {
	if r, ok := s.rows[tenantID+"|"+id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, auth.ErrPreviewSessionNotFound
}

func (s *phStore) End(_ context.Context, tenantID, id, reason string, now time.Time) (bool, error) {
	r, ok := s.rows[tenantID+"|"+id]
	if !ok || r.EndedAt != nil {
		return false, nil
	}
	r.EndedAt, r.EndReason = &now, &reason
	return true, nil
}

type phActors struct{ dead bool }

func (a *phActors) ActorLive(_ context.Context, _, act, _ string, _ time.Time) (*auth.LiveActor, error) {
	if a.dead {
		return nil, auth.ErrActorNotLive
	}
	return &auth.LiveActor{ID: act}, nil
}

type phRoles struct{}

func (phRoles) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if tenantID == phTenant && id == phRole {
		return &auth.Role{ID: phRole, TenantID: phTenant, Name: "crm_sales_rep", Label: "Nhân viên kinh doanh"}, nil
	}
	return nil, auth.ErrRoleNotFound
}

type previewHarness struct {
	r       *gin.Engine
	store   *phStore
	actors  *phActors
	preview *auth.Claims // claims of a live session seeded in store
}

func newPreviewHarness(t *testing.T) *previewHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	tokens, err := auth.NewTokenService(phSecret, 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	h := &previewHarness{store: &phStore{rows: map[string]*auth.PreviewSession{}}, actors: &phActors{}}
	svc := services.NewPreviewService(h.store, h.actors, phRoles{}, tokens, 30*time.Minute)

	// A live session row the "tok-preview" claims point at.
	row := &auth.PreviewSession{ID: "ps-1", TenantID: phTenant, RoleID: phRole, PrincipalID: services.PreviewPrincipal(phTenant, phRole),
		ActorUserID: "u-op", ActorTenantID: phTenant, ActorSessionID: "sid-op", ExpiresAt: time.Now().Add(time.Hour), CreatedBy: "u-op"}
	h.store.rows[phTenant+"|"+row.ID] = row
	h.preview = &auth.Claims{Subject: auth.Subject{UserID: row.PrincipalID, TenantID: phTenant, RoleID: phRole, SessionID: row.ID,
		ActorUserID: "u-op", ActorTenantID: phTenant}, TokenType: auth.TokenTypePreview}

	access := func(uid string, admin bool, sid string) *auth.Claims {
		return &auth.Claims{Subject: auth.Subject{UserID: uid, TenantID: phTenant, IsAdmin: admin, SessionID: sid}, TokenType: auth.TokenTypeAccess}
	}
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"tok-op":       access("u-op", true, "sid-op"),
		"tok-op-nosid": access("u-op", true, ""),
		"tok-admin":    access("u-adm", true, "sid-adm"), // legacy customer admin: not an operator
		"tok-staff":    access("u-staff", false, "sid-staff"),
		"tok-preview":  h.preview,
	}}
	h.r = gin.New()
	RegisterAPIRoutes(h.r.Group("/api/v1"), ver, nil, APIHandlers{Preview: NewPreviewHandler(svc, zap.NewNop())}, zap.NewNop(),
		WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())))
	return h
}

type phResp struct {
	Code  string `json:"code"`
	Error string `json:"error"`
	Data  struct {
		SessionID string    `json:"sessionId"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expiresAt"`
		Role      struct {
			ID, Name, Label string
		} `json:"role"`
		AppID *string `json:"appId"`
		Draft *bool   `json:"draft"`
	} `json:"data"`
}

func (h *previewHarness) do(t *testing.T, method, path, token, body string) (int, phResp) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.r.ServeHTTP(w, req)
	var out phResp
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, w.Body.String(), err)
		}
	}
	return w.Code, out
}

func TestPreviewHandler_Start201(t *testing.T) {
	h := newPreviewHarness(t)
	code, out := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", "tok-op", `{"roleId":"`+phRole+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("status %d (%+v), want 201", code, out)
	}
	d := out.Data
	if d.SessionID == "" || d.Token == "" || d.ExpiresAt.IsZero() || d.Role.ID != phRole || d.Role.Name != "crm_sales_rep" ||
		d.Role.Label != "Nhân viên kinh doanh" || d.AppID != nil || d.Draft == nil || *d.Draft {
		t.Fatalf("201 body = %+v", d)
	}
	if _, ok := h.store.rows[phTenant+"|"+d.SessionID]; !ok {
		t.Error("201 without a sys_preview_sessions row")
	}
}

func TestPreviewHandler_StartRefusals(t *testing.T) {
	cases := []struct {
		name, token, body string
		dead              bool
		wantStatus        int
		wantCode          string
	}{
		{"not json", "tok-op", `nope`, false, http.StatusBadRequest, "INVALID_BODY"},
		{"roleId missing", "tok-op", `{}`, false, http.StatusBadRequest, "INVALID_BODY"},
		{"appId not a uuid", "tok-op", `{"roleId":"` + phRole + `","appId":"x"}`, false, http.StatusBadRequest, "INVALID_BODY"},
		{"non-admin staff", "tok-staff", `{"roleId":"` + phRole + `"}`, false, http.StatusForbidden, "FORBIDDEN"},
		{"legacy customer admin (not operator)", "tok-admin", `{"roleId":"` + phRole + `"}`, false, http.StatusForbidden, ""},
		{"builder token without sid", "tok-op-nosid", `{"roleId":"` + phRole + `"}`, false, http.StatusForbidden, "FORBIDDEN"},
		{"builder login session dead", "tok-op", `{"roleId":"` + phRole + `"}`, true, http.StatusForbidden, "FORBIDDEN"},
		{"role of another tenant", "tok-op", `{"roleId":"` + phOtherRole + `"}`, false, http.StatusUnprocessableEntity, "PREVIEW_ROLE_NOT_FOUND"},
		{"draft before S9-9", "tok-op", `{"roleId":"` + phRole + `","draft":true}`, false, http.StatusUnprocessableEntity, "PREVIEW_DRAFT_UNAVAILABLE"},
		{"a preview token cannot open another preview", "tok-preview", `{"roleId":"` + phRole + `"}`, false, http.StatusForbidden, "PREVIEW_READ_ONLY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newPreviewHarness(t)
			h.actors.dead = tc.dead
			before := len(h.store.rows)
			code, out := h.do(t, http.MethodPost, "/api/v1/admin/preview-sessions", tc.token, tc.body)
			if code != tc.wantStatus || (tc.wantCode != "" && out.Code != tc.wantCode) {
				t.Fatalf("status %d code %q, want %d %q", code, out.Code, tc.wantStatus, tc.wantCode)
			}
			if len(h.store.rows) != before {
				t.Error("a refused start wrote a session row")
			}
		})
	}
}

func TestPreviewHandler_End(t *testing.T) {
	h := newPreviewHarness(t)
	if code, out := h.do(t, http.MethodDelete, "/api/v1/preview-sessions/current?reason=bogus", "tok-preview", ""); code != http.StatusBadRequest || out.Code != "INVALID_QUERY" {
		t.Fatalf("bad reason: %d %q, want 400 INVALID_QUERY", code, out.Code)
	}
	if code, out := h.do(t, http.MethodDelete, "/api/v1/preview-sessions/current?reason=exit", "tok-op", ""); code != http.StatusConflict || out.Code != "PREVIEW_NOT_ACTIVE" {
		t.Fatalf("ordinary token: %d %q, want 409 PREVIEW_NOT_ACTIVE", code, out.Code)
	}
	if code, _ := h.do(t, http.MethodDelete, "/api/v1/preview-sessions/current?reason=exit", "tok-preview", ""); code != http.StatusNoContent {
		t.Fatalf("exit: %d, want 204", code)
	}
	row := h.store.rows[phTenant+"|ps-1"]
	if row.EndedAt == nil || row.EndReason == nil || *row.EndReason != "exit" {
		t.Fatalf("row not ended: %+v", row)
	}
	// Idempotent at the service: a second exit (the stub verifier still accepts the claims) is 204.
	if code, _ := h.do(t, http.MethodDelete, "/api/v1/preview-sessions/current?reason=left", "tok-preview", ""); code != http.StatusNoContent {
		t.Fatalf("second exit: %d, want 204", code)
	}
}
