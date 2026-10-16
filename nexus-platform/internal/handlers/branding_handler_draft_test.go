package handlers

// branding_handler_draft_test.go — specs/063-draft-publish T067: GET /auth/branding/:tenant NEVER
// overlays a draft (even with a builder in the ctx and the header), GET /admin/tenant/branding does
// for the builder asking and says draftState.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/services"
)

type bhLive struct{ t auth.Tenant }

func (f *bhLive) ResolveTenantBySlug(context.Context, string) (*auth.Tenant, error) {
	t := f.t
	return &t, nil
}
func (f *bhLive) GetTenantByID(context.Context, string) (*auth.Tenant, error) {
	t := f.t
	return &t, nil
}
func (f *bhLive) UpdateTenantBranding(_ context.Context, _ string, raw json.RawMessage) error {
	f.t.Branding = raw
	return nil
}

type bhDrafts struct {
	rows map[string]metaversion.DraftRow
}

func (f *bhDrafts) Upsert(_ context.Context, _ string, in metaversion.UpsertInput) error {
	f.rows[in.DocType+"|"+in.DocKey] = metaversion.DraftRow{DocType: in.DocType, DocKey: in.DocKey, Op: in.Op, DraftJSON: in.Doc}
	return nil
}
func (f *bhDrafts) GetByKey(_ context.Context, _ string, typ, key string) (*metaversion.DraftRow, error) {
	if r, ok := f.rows[typ+"|"+key]; ok {
		return &r, nil
	}
	return nil, nil
}
func (f *bhDrafts) ListByType(context.Context, string, string) ([]metaversion.DraftRow, error) {
	return nil, nil
}
func (f *bhDrafts) DeleteByKey(context.Context, string, string, string) (bool, error) {
	return false, nil
}

type bhDelivered struct{}

func (bhDelivered) HasNumbered(context.Context, string) (bool, error) { return true, nil }

const bhTenant = "t1"

func bhRouter(caller metaversion.Caller) (*gin.Engine, *bhLive) {
	gin.SetMode(gin.TestMode)
	live := &bhLive{t: auth.Tenant{ID: bhTenant, Slug: "acme", Branding: json.RawMessage(`{"accentColor":"#111111"}`)}}
	store := services.NewDraftBrandingStore(live, &bhDrafts{rows: map[string]metaversion.DraftRow{}}, services.NewDraftGate(bhDelivered{}))
	h := NewBrandingHandler(services.NewTenantBrandingService(store), zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", bhTenant)
		c.Request = c.Request.WithContext(metaversion.WithCaller(c.Request.Context(), caller))
		c.Next()
	})
	api := r.Group("/api/v1")
	h.RegisterPublicRoute(api)
	h.RegisterAdminRoutes(api)
	return r, live
}

func bhDo(r *gin.Engine, method, path, body string) (int, map[string]any) {
	req := httptest.NewRequest(method, path, nil)
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Nexus-Draft", "1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func bhColour(out map[string]any) (colour, state string) {
	d, _ := out["data"].(map[string]any)
	b, _ := d["branding"].(map[string]any)
	colour, _ = b["accentColor"].(string)
	state, _ = d["draftState"].(string)
	return
}

func TestBrandingHandler_PublicReadNeverOverlaysAndAdminReadDoes(t *testing.T) {
	builder := metaversion.Caller{UserID: "b", TenantID: bhTenant, HomeTenantID: bhTenant, IsBuilder: true, ReadDraft: true}
	r, live := bhRouter(builder)

	code, out := bhDo(r, http.MethodPut, "/api/v1/admin/tenant/branding", `{"accentColor":"#222222"}`)
	if c, st := bhColour(out); code != http.StatusOK || c != "#222222" || st != "edited" {
		t.Fatalf("builder PUT = %d %v, want 200 with the draft colour and draftState edited", code, out)
	}
	if string(live.t.Branding) != `{"accentColor":"#111111"}` {
		t.Fatalf("PUT reached the live document: %s", live.t.Branding)
	}
	if code, out = bhDo(r, http.MethodGet, "/api/v1/auth/branding/acme", ""); code != http.StatusOK {
		t.Fatalf("public GET = %d", code)
	}
	if c, _ := bhColour(out); c != "#111111" {
		t.Fatalf("PUBLIC branding with a builder in ctx + header = %q, want the live colour", c)
	}
	_, out = bhDo(r, http.MethodGet, "/api/v1/admin/tenant/branding", "")
	if c, st := bhColour(out); c != "#222222" || st != "edited" {
		t.Fatalf("admin GET = %q state %q, want the draft", c, st)
	}
}

func TestBrandingHandler_NonBuilderNeverSeesTheDraftOrAState(t *testing.T) {
	legacy := metaversion.Caller{UserID: "u", TenantID: bhTenant, HomeTenantID: bhTenant}
	r, live := bhRouter(legacy)
	if code, out := bhDo(r, http.MethodPut, "/api/v1/admin/tenant/branding", `{"accentColor":"#333333"}`); code != http.StatusOK {
		t.Fatalf("legacy admin PUT = %d %v", code, out)
	} else if _, st := bhColour(out); st != "" {
		t.Errorf("draftState %q leaked to a non-builder", st)
	}
	if string(live.t.Branding) != `{"accentColor":"#333333"}` {
		t.Fatalf("a legacy admin's save must land live (direct), got %s", live.t.Branding)
	}
	if _, out := bhDo(r, http.MethodGet, "/api/v1/admin/tenant/branding", ""); out == nil {
		t.Fatal("no body")
	} else if _, st := bhColour(out); st != "" {
		t.Errorf("draftState %q leaked to a non-builder on GET", st)
	}
}
