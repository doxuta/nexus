package handlers

// specs/062-customer-workspace T077 — the three project-registry routes through the REAL router,
// REAL tokens and the REAL operator gate (service faked): only the workshop's platform operator
// reaches them; a customer's own admin, a customer's staff / owner (non-admin) and a builder
// workspace-session token all get the same byte-identical 403 as every other operator route, and
// the service is never called for them. Plus request mapping and error statuses.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

type fakeProjectSvc struct {
	calls      int
	listCaller string
	gotActor   services.ProjectActor
	gotTenant  string
	gotPatch   services.ProjectPatch
	err        error

	gotHome        auth.Subject
	gotCreate      services.ProjectCreateInput
	createPassword string
}

func (f *fakeProjectSvc) List(_ context.Context, caller string) ([]services.ProjectView, error) {
	f.calls++
	f.listCaller = caller
	if f.err != nil {
		return nil, f.err
	}
	return []services.ProjectView{{TenantID: "t1", Slug: "a", Name: "Khách A", Stage: "building",
		Health: services.ProjectHealth{Status: "unknown"}}}, nil
}

func (f *fakeProjectSvc) Patch(_ context.Context, actor services.ProjectActor, tenantID string, in services.ProjectPatch) (*services.ProjectView, error) {
	f.calls++
	f.gotActor, f.gotTenant, f.gotPatch = actor, tenantID, in
	if f.err != nil {
		return nil, f.err
	}
	return &services.ProjectView{TenantID: tenantID, Stage: "trial"}, nil
}

func (f *fakeProjectSvc) HealthCheck(_ context.Context, actor services.ProjectActor, tenantID string) (*services.ProjectHealth, error) {
	f.calls++
	f.gotActor, f.gotTenant = actor, tenantID
	if f.err != nil {
		return nil, f.err
	}
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	return &services.ProjectHealth{Status: "ok", CheckedAt: &now}, nil
}

func (f *fakeProjectSvc) Create(_ context.Context, home auth.Subject, in services.ProjectCreateInput) (*services.ProjectCreated, error) {
	f.calls++
	f.gotHome, f.gotCreate = home, in
	if f.err != nil {
		return nil, f.err
	}
	return &services.ProjectCreated{
		Project:         &services.ProjectView{TenantID: "new-1", Slug: in.Slug, Stage: "building"},
		Admin:           services.UserView{ID: "owner-1", Email: in.AdminEmail},
		OneTimePassword: f.createPassword,
		Start:           services.ProjectStart{Mode: in.StartMode, Status: "skipped"},
		Workspace:       &services.WorkspaceOpenResult{AccessToken: "ws-token"},
	}, nil
}

type projectFixture struct {
	r      *gin.Engine
	tokens *auth.TokenService
	svc    *fakeProjectSvc
}

func newProjectFixture(t *testing.T) *projectFixture {
	t.Helper()
	tokens, err := auth.NewTokenService("a-sufficiently-long-real-secret-value-1234567890", 15*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	users := gateUsers{byID: map[string]*auth.User{
		"builder":     {ID: "builder", Status: "active", IsAdmin: true, IsPlatformOperator: true},
		"legacyadmin": {ID: "legacyadmin", Status: "active", IsAdmin: true},
		"owner":       {ID: "owner", Status: "active", IsAdmin: false},
	}}
	f := &projectFixture{tokens: tokens, svc: &fakeProjectSvc{}}
	gin.SetMode(gin.TestMode)
	f.r = gin.New()
	RegisterAPIRoutes(f.r.Group("/api/v1"), tokens, nil, APIHandlers{
		Auth:    NewAuthHandler(services.NewAuthService(nil, nil, nil), zap.NewNop()),
		Project: NewAdminProjectHandler(f.svc, users, zap.NewNop()),
	}, zap.NewNop(), WithLiveSubject(users), WithWorkspaceResolver(&statefulResolver{}))
	return f
}

func (f *projectFixture) token(t *testing.T, uid string, admin bool) string {
	t.Helper()
	tok, err := f.tokens.IssueAccessToken(auth.Subject{UserID: uid, TenantID: "home-1", IsAdmin: admin, SessionID: "sid"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *projectFixture) sessionToken(t *testing.T) string {
	t.Helper()
	tok, err := f.tokens.IssueAccessTokenUntil(auth.Subject{
		UserID: "builder", TenantID: "customer-1", IsAdmin: true, SessionID: "sid",
		ActorUserID: "builder", ActorTenantID: "home-1", WorkspaceSessionID: "ws-1",
	}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (f *projectFixture) do(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

const (
	pjList   = "/api/v1/admin/projects"
	pjTenant = "/api/v1/admin/projects/11111111-1111-4111-8111-111111111111"
)

func TestProjectRoutes_OperatorOnly(t *testing.T) {
	routes := []struct {
		name, method, path, body string
	}{
		{"list", http.MethodGet, pjList, ""},
		{"patch", http.MethodPatch, pjTenant, `{"trial":true}`},
		{"health-check", http.MethodPost, pjTenant + "/health-check", ""},
	}
	callers := []struct {
		name  string
		token func(f *projectFixture, t *testing.T) string
		want  int
	}{
		{"platform operator", func(f *projectFixture, t *testing.T) string { return f.token(t, "builder", true) }, http.StatusOK},
		{"customer admin without the operator flag", func(f *projectFixture, t *testing.T) string { return f.token(t, "legacyadmin", true) }, http.StatusForbidden},
		{"customer owner / staff (non-admin)", func(f *projectFixture, t *testing.T) string { return f.token(t, "owner", false) }, http.StatusForbidden},
		{"builder workspace-session token", func(f *projectFixture, t *testing.T) string { return f.sessionToken(t) }, http.StatusForbidden},
		{"no token", func(*projectFixture, *testing.T) string { return "" }, http.StatusUnauthorized},
	}
	for _, rt := range routes {
		for _, c := range callers {
			t.Run(rt.name+"/"+c.name, func(t *testing.T) {
				f := newProjectFixture(t)
				w := f.do(rt.method, rt.path, c.token(f, t), rt.body)
				if w.Code != c.want {
					t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), c.want)
				}
				if c.want == http.StatusForbidden && w.Body.String() != gateForbiddenBody {
					t.Errorf("403 body = %s, want byte-identical %s", w.Body.String(), gateForbiddenBody)
				}
				if called := f.svc.calls > 0; called != (c.want == http.StatusOK) {
					t.Errorf("service reached = %v for status %d", called, c.want)
				}
			})
		}
	}
}

func TestProjectList_PassesTheCallersOwnTenantSoItCanBeHidden(t *testing.T) {
	f := newProjectFixture(t)
	w := f.do(http.MethodGet, pjList, f.token(t, "builder", true), "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stage":"building"`) || !strings.Contains(w.Body.String(), `"tenantId":"t1"`) {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if f.svc.listCaller != "home-1" {
		t.Fatalf("service got caller tenant %q, want the token's tenant home-1", f.svc.listCaller)
	}
}

func TestProjectPatch_MapsTheBody(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		check      func(t *testing.T, p services.ProjectPatch)
	}{
		{"industry + trial", `{"industry":"Phòng khám","trial":true}`, 200, func(t *testing.T, p services.ProjectPatch) {
			if !p.IndustrySet || p.Industry == nil || *p.Industry != "Phòng khám" || p.Trial == nil || !*p.Trial {
				t.Fatalf("patch = %+v", p)
			}
		}},
		{"industry null clears", `{"industry":null}`, 200, func(t *testing.T, p services.ProjectPatch) {
			if !p.IndustrySet || p.Industry != nil || p.Trial != nil {
				t.Fatalf("patch = %+v", p)
			}
		}},
		{"absent industry is left alone", `{"trial":false}`, 200, func(t *testing.T, p services.ProjectPatch) {
			if p.IndustrySet || p.Trial == nil || *p.Trial {
				t.Fatalf("patch = %+v", p)
			}
		}},
		{"wrong type", `{"industry":5}`, 400, nil},
		{"stage is never settable", `{"stage":"live"}`, 400, nil},
		{"deliveredAt is never settable", `{"deliveredAt":"2026-10-01T00:00:00Z"}`, 400, nil},
		{"malformed", `{broken`, 400, nil},
		{"empty body", ``, 400, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectFixture(t)
			w := f.do(http.MethodPatch, pjTenant, f.token(t, "builder", true), tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", w.Code, w.Body.String(), tc.wantStatus)
			}
			if tc.check == nil {
				if f.svc.calls != 0 {
					t.Fatal("a bad body must not reach the service")
				}
				return
			}
			tc.check(t, f.svc.gotPatch)
			if f.svc.gotTenant != "11111111-1111-4111-8111-111111111111" || f.svc.gotActor.UserID != "builder" || f.svc.gotActor.TenantID != "home-1" {
				t.Fatalf("service saw tenant %q actor %+v", f.svc.gotTenant, f.svc.gotActor)
			}
		})
	}
}

func TestProjectRoutes_ErrorStatusMapping(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{&services.ValidationError{Code: "TENANT_NOT_FOUND", Msg: "x"}, http.StatusNotFound, "TENANT_NOT_FOUND"},
		{&services.ValidationError{Code: "PROJECT_INDUSTRY_TOO_LONG", Msg: "x"}, http.StatusUnprocessableEntity, "PROJECT_INDUSTRY_TOO_LONG"},
		{&services.ValidationError{Code: "PROJECT_TRIAL_AFTER_DELIVERY", Msg: "x"}, http.StatusUnprocessableEntity, "PROJECT_TRIAL_AFTER_DELIVERY"},
		{errors.New("boom"), http.StatusInternalServerError, "PROJECT_FAILED"},
	}
	routes := []struct{ method, path, body string }{
		{http.MethodGet, pjList, ""},
		{http.MethodPatch, pjTenant, `{"trial":true}`},
		{http.MethodPost, pjTenant + "/health-check", ""},
	}
	for _, tc := range tests {
		for _, rt := range routes {
			t.Run(tc.code+" "+rt.method, func(t *testing.T) {
				f := newProjectFixture(t)
				f.svc.err = tc.err
				w := f.do(rt.method, rt.path, f.token(t, "builder", true), rt.body)
				if w.Code != tc.status || !strings.Contains(w.Body.String(), `"code":"`+tc.code+`"`) {
					t.Fatalf("status = %d body = %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.code)
				}
				if tc.status == http.StatusInternalServerError && strings.Contains(w.Body.String(), "boom") {
					t.Fatalf("internal error text leaked to the client: %s", w.Body.String())
				}
			})
		}
	}
}

// The 201 carries the show-once password and a builder token: it must say so to proxies/browsers.
func TestProjectCreate_ResponseIsNoStore(t *testing.T) {
	f := newProjectFixture(t)
	f.svc.createPassword = "s3cret-once"
	w := f.do(http.MethodPost, pjList, f.token(t, "builder", true),
		`{"name":"Khách","slug":"khach","adminEmail":"chu@khach.test","generatePassword":true,"start":{"mode":"blank"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s), want 201", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}
