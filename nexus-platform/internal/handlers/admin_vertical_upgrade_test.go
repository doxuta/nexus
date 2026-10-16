package handlers

// specs/069-pack-apply-upgrade T016 — POST /admin/verticals/installs/:slug/upgrade-plan over fakes.
// The gate (non-operator / staff / API key ⇒ same 403 as install) is proven at router level with a
// nil-service handler (403 = gate blocked, anything else = gate admitted); the request shape and
// the cross-tenant order (authorize + resolve BEFORE reading the body) at handler level.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
	"nexus/platform/internal/services/vertical_pack"
)

// planSvc records PlanUpgrade calls and returns a canned result/error.
type planSvc struct {
	fakeVerticalSvc
	mu      sync.Mutex
	tenants []string
	slugs   []string
	sources []string
	pack    *vertical_pack.Pack
	err     error
}

func (f *planSvc) PlanUpgrade(_ context.Context, tenantID, slug string, pack *vertical_pack.Pack, source string) (*vertical_pack.UpgradePlan, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants, f.slugs, f.sources, f.pack = append(f.tenants, tenantID), append(f.slugs, slug), append(f.sources, source), pack
	if f.err != nil {
		return nil, f.err
	}
	return &vertical_pack.UpgradePlan{Slug: slug, ToVersion: "1.0.0", Items: []vertical_pack.UpgradeItem{}}, nil
}

// planRouter mounts the handler with the verified subject the way AuthMiddleware would.
func planRouter(svc verticalPackService, gate operatorGate, resolver tenantSlugResolver, sub auth.Subject, mws ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewAdminVerticalHandler(svc, zap.NewNop())
	if gate != nil || resolver != nil {
		h.WithCrossTenantInstall(gate, resolver, audit.NewNoopAuditor())
	}
	r := gin.New()
	r.Use(mws...)
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", sub.TenantID)
		c.Set("authClaims", &auth.Claims{Subject: sub})
		c.Next()
	})
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

func opSubject() auth.Subject {
	return auth.Subject{UserID: mscCallerUser, TenantID: mscCallerTenant, IsAdmin: true}
}

func postPlan(t *testing.T, r *gin.Engine, slug, query, rawBody string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/verticals/installs/"+slug+"/upgrade-plan"+query, strings.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func upgradeErrCode(t *testing.T, w *httptest.ResponseRecorder) (string, map[string]any) {
	t.Helper()
	var resp struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return resp.Code, resp.Details
}

func TestUpgradePlan_RequestValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		svcErr   error
		wantHTTP int
		wantCode string
		field    string
	}{
		{"neither builtin nor pack", `{}`, nil, 422, "VERTICAL_PACK_REQUIRED", ""},
		{"both builtin and pack", `{"builtin":"crm","pack":{"entities":[]}}`, nil, 422, "VERTICAL_PACK_REQUIRED", ""},
		{"unknown builtin", `{"builtin":"khong-co"}`, nil, 404, "VERTICAL_BUILTIN_NOT_FOUND", ""},
		{"unknown field in uploaded pack", `{"pack":{"manifest":{"slug":"p","version":"1.0.0"},"entities":[],"bonusSection":[1]}}`, nil, 422, "PACK_UPGRADE_UNKNOWN_FIELD", "bonusSection"},
		{"unknown nested field", `{"pack":{"manifest":{"slug":"p","version":"1.0.0","flavour":"9"},"entities":[]}}`, nil, 422, "PACK_UPGRADE_UNKNOWN_FIELD", "flavour"},
		{"pack of the wrong shape", `{"pack":{"entities":"nope"}}`, nil, 400, "INVALID_BODY", ""},
		{"not installed", `{"builtin":"crm"}`, vertical_pack.ErrInstallNotFound, 404, "VERTICAL_INSTALL_NOT_FOUND", ""},
		{"service validation error maps to 422", `{"builtin":"crm"}`,
			&services.ValidationError{Code: "PACK_UPGRADE_DOWNGRADE", Msg: "thấp hơn", Details: map[string]any{}}, 422, "PACK_UPGRADE_DOWNGRADE", ""},
		{"unexpected error is a 500", `{"builtin":"crm"}`, errors.New("boom"), 500, "VERTICAL_UPGRADE_PLAN_FAILED", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &planSvc{err: tt.svcErr}
			r := planRouter(svc, nil, nil, opSubject())
			w := postPlan(t, r, "crm", "", tt.body)
			if w.Code != tt.wantHTTP {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tt.wantHTTP, w.Body.String())
			}
			code, details := upgradeErrCode(t, w)
			if code != tt.wantCode {
				t.Fatalf("code = %q, want %q", code, tt.wantCode)
			}
			if tt.field != "" && details["field"] != tt.field {
				t.Fatalf("details.field = %v, want %s", details["field"], tt.field)
			}
		})
	}
}

func TestUpgradePlan_HappyPathBuiltinAndUpload(t *testing.T) {
	svc := &planSvc{}
	r := planRouter(svc, nil, nil, opSubject())
	w := postPlan(t, r, "crm", "", `{"builtin":"CRM"}`)
	if w.Code != 200 || svc.sources[0] != "builtin" || svc.tenants[0] != mscCallerTenant || svc.slugs[0] != "crm" {
		t.Fatalf("builtin: %d %s tenants=%v sources=%v", w.Code, w.Body.String(), svc.tenants, svc.sources)
	}
	if svc.pack == nil || svc.pack.Manifest == nil || svc.pack.Manifest.Slug != "crm" {
		t.Fatalf("builtin pack not resolved: %+v", svc.pack)
	}
	w = postPlan(t, r, "p", "", `{"pack":{"manifest":{"slug":"p","version":"1.2.0"},"entities":[{"name":"e","label":"E","fields":[]}]}}`)
	if w.Code != 200 || svc.sources[1] != "upload" || svc.pack.Manifest.Version != "1.2.0" {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Plan vertical_pack.UpgradePlan `json:"plan"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Data.Plan.Slug != "p" {
		t.Fatalf("envelope {data:{plan}} broken: %v %s", err, w.Body.String())
	}
}

func TestUpgradePlan_BodyTooLarge413(t *testing.T) {
	r := planRouter(&planSvc{}, nil, nil, opSubject(), middleware.MaxBodySize(64))
	w := postPlan(t, r, "crm", "", `{"builtin":"crm","pack":null,"pad":"`+strings.Repeat("x", 4096)+`"}`)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413, got %d %s", w.Code, w.Body.String())
	}
}

// Cross-tenant: authorization + target resolution happen BEFORE the body is read.
func TestUpgradePlan_TargetTenantOrder(t *testing.T) {
	t.Run("non-operator with a corrupt body still gets only the uniform 403", func(t *testing.T) {
		svc := &planSvc{}
		nonOp := operatorUser()
		nonOp.IsPlatformOperator = false
		resolver := &fakeTenantResolver{t: t, failIfCalled: true}
		r := planRouter(svc, &fakeOperatorGate{t: t, user: nonOp}, resolver, opSubject())
		w := postPlan(t, r, "crm", "?targetTenant=khach-b", `{not json`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("want 403 before the body is read, got %d %s", w.Code, w.Body.String())
		}
		if len(svc.tenants) != 0 {
			t.Fatalf("service reached: %v", svc.tenants)
		}
	})
	t.Run("unknown and inactive target are one 404", func(t *testing.T) {
		svc := &planSvc{}
		r := planRouter(svc, &fakeOperatorGate{t: t, user: operatorUser()}, &fakeTenantResolver{t: t}, opSubject())
		w1 := postPlan(t, r, "crm", "?targetTenant=khong-ton-tai", `{"builtin":"crm"}`)
		w2 := postPlan(t, r, "crm", "?targetTenant=da-ngung", `{"builtin":"crm"}`)
		if w1.Code != 404 || w1.Body.String() != w2.Body.String() {
			t.Fatalf("want identical 404s, got %d %s / %d %s", w1.Code, w1.Body.String(), w2.Code, w2.Body.String())
		}
		if c, _ := upgradeErrCode(t, w1); c != "TENANT_NOT_FOUND" {
			t.Fatalf("code = %s", c)
		}
	})
	t.Run("operator plans against the TARGET tenant", func(t *testing.T) {
		svc := &planSvc{}
		resolver := &fakeTenantResolver{t: t, bySlug: map[string]*auth.Tenant{"khach-b": {ID: mscTargetTenant}}}
		r := planRouter(svc, &fakeOperatorGate{t: t, user: operatorUser()}, resolver, opSubject())
		w := postPlan(t, r, "crm", "?targetTenant=khach-b", `{"builtin":"crm"}`)
		if w.Code != 200 || len(svc.tenants) != 1 || svc.tenants[0] != mscTargetTenant {
			t.Fatalf("want plan on %s, got %d %v", mscTargetTenant, w.Code, svc.tenants)
		}
	})
	t.Run("workspace-session token with targetTenant is refused before anything is read", func(t *testing.T) {
		svc := &planSvc{}
		gate := &fakeOperatorGate{t: t, user: operatorUser(), failIfCalled: true}
		resolver := &fakeTenantResolver{t: t, failIfCalled: true}
		sub := opSubject()
		sub.WorkspaceSessionID = "wsid-1"
		r := planRouter(svc, gate, resolver, sub)
		w := postPlan(t, r, "crm", "?targetTenant=khach-b", `{"builtin":"crm"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d %s", w.Code, w.Body.String())
		}
		if c, _ := upgradeErrCode(t, w); c != "FORBIDDEN" || len(svc.tenants) != 0 {
			t.Fatalf("code=%s service calls=%v", c, svc.tenants)
		}
	})
	t.Run("workspace-session token WITHOUT targetTenant plans its own tenant", func(t *testing.T) {
		svc := &planSvc{}
		sub := opSubject()
		sub.WorkspaceSessionID = "wsid-1"
		r := planRouter(svc, nil, nil, sub)
		if w := postPlan(t, r, "crm", "", `{"builtin":"crm"}`); w.Code != 200 || svc.tenants[0] != mscCallerTenant {
			t.Fatalf("got %d %v", w.Code, svc.tenants)
		}
	})
}

// Router-level gate: the new route sits on builderGated, so a plain tenant admin, a non-admin and
// an API key get the SAME 403 body as POST /admin/verticals/install; the operator is admitted.
func TestUpgradePlan_RouteGatedLikeInstall(t *testing.T) {
	gin.SetMode(gin.TestMode)
	claims := map[string]*auth.Claims{
		"tok-admin": {Subject: auth.Subject{UserID: "u-adm", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
		"tok-op":    {Subject: auth.Subject{UserID: "u-op", TenantID: "t1", RoleID: "r1", IsAdmin: true}, TokenType: auth.TokenTypeAccess},
		"tok-staff": {Subject: auth.Subject{UserID: "u-st", TenantID: "t1", RoleID: "r1", IsAdmin: false}, TokenType: auth.TokenTypeAccess},
		"tok-key":   {Subject: auth.Subject{UserID: "u-k", TenantID: "t1", RoleID: "r1", IsAdmin: false}, TokenType: auth.TokenTypeAPIKey},
	}
	r := gin.New()
	r.Use(gin.Recovery())
	RegisterAPIRoutes(r.Group("/api/v1"), &stubVerifier{byToken: claims}, nil, APIHandlers{
		Vertical: NewAdminVerticalHandler(nil, zap.NewNop()),
	}, zap.NewNop(), WithOperatorTier(middleware.RequirePlatformOperator(&operatorRepoFake{operatorID: "u-op"}, zap.NewNop())))

	call := func(path, tok string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{"builtin":"crm"}`)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}
	for _, tok := range []string{"tok-admin", "tok-staff", "tok-key"} {
		ic, ib := call("/api/v1/admin/verticals/install", tok)
		uc, ub := call("/api/v1/admin/verticals/installs/crm/upgrade-plan", tok)
		if ic != http.StatusForbidden || uc != http.StatusForbidden || ib != ub {
			t.Errorf("%s: install %d %q vs upgrade-plan %d %q — must be the same 403", tok, ic, ib, uc, ub)
		}
	}
	if c, b := call("/api/v1/admin/verticals/installs/crm/upgrade-plan", "tok-op"); c == http.StatusForbidden || c == http.StatusUnauthorized {
		t.Errorf("operator must be admitted by the gate, got %d %s", c, b)
	}
}
