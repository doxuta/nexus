package handlers

// Unit tests for PUT /admin/relationships/:id/view-config (ENG-RELVIEW): the route sits
// behind the REAL middleware.RequireAdmin gate (403 path exercised through the actual
// middleware, mirroring aiTestRouter), and the handler maps the service's ValidationError
// codes to the canonical statuses. The service itself is faked — its validation contract is
// pinned by services/relation_viewconfig_test.go; the full DB round-trip by
// eng_relview_integration_test.go.

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
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// fakeRelLifecycle implements the RelationLifecycle consumer interface. Only SetViewConfig
// carries behavior; the W2 endpoints are not under test here.
type fakeRelLifecycle struct {
	setErr   error
	lastTen  string
	lastRel  string
	lastRaw  json.RawMessage
	echoBack map[string]any
}

func (f *fakeRelLifecycle) Impact(context.Context, string, string) (*services.RelationImpact, error) {
	return nil, nil
}
func (f *fakeRelLifecycle) Patch(context.Context, string, string, services.PatchRelationshipInput) (*metadata.RelationshipDefinition, error) {
	return nil, nil
}
func (f *fakeRelLifecycle) Delete(context.Context, string, string) error { return nil }
func (f *fakeRelLifecycle) SetViewConfig(_ context.Context, tenantID, relID string, raw json.RawMessage) (map[string]any, error) {
	f.lastTen, f.lastRel, f.lastRaw = tenantID, relID, raw
	if f.setErr != nil {
		return nil, f.setErr
	}
	return f.echoBack, nil
}

// relViewTestRouter mounts the admin relation handler behind the REAL RequireAdmin gate with
// a shim injecting verified claims (tenant t1) the way AuthMiddleware would.
func relViewTestRouter(lc *fakeRelLifecycle, isAdmin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", "t1")
		c.Set("authClaims", &auth.Claims{Subject: auth.Subject{
			UserID: "u1", TenantID: "t1", IsAdmin: isAdmin,
		}})
		c.Next()
	})
	grp := r.Group("/api/v1")
	grp.Use(middleware.RequireAdmin(nil))
	NewAdminRelationHandler(nil, lc, zap.NewNop()).RegisterRoutes(grp)
	return r
}

func TestRelViewConfigHTTP_AdminGate(t *testing.T) {
	lc := &fakeRelLifecycle{}
	r := relViewTestRouter(lc, false /* non-admin */)

	rec := doJSON(r, http.MethodPut, "/api/v1/admin/relationships/rel-1/view-config", map[string]any{"label": "X"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin PUT = %d; want 403 (body=%s)", rec.Code, rec.Body.String())
	}
	if lc.lastRel != "" {
		t.Errorf("service must not be reached by a non-admin; got relID %q", lc.lastRel)
	}
}

func TestRelViewConfigHTTP_HappyPath(t *testing.T) {
	lc := &fakeRelLifecycle{echoBack: map[string]any{"label": "Đơn hàng", "allowCreate": true}}
	r := relViewTestRouter(lc, true)

	rec := doJSON(r, http.MethodPut, "/api/v1/admin/relationships/rel-1/view-config",
		map[string]any{"label": "Đơn hàng", "allowCreate": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d; want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if lc.lastTen != "t1" || lc.lastRel != "rel-1" {
		t.Errorf("service call = (%q,%q); want (t1, rel-1)", lc.lastTen, lc.lastRel)
	}
	var resp struct {
		Data struct {
			ViewConfig map[string]any `json:"viewConfig"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.ViewConfig["label"] != "Đơn hàng" {
		t.Errorf("echo = %+v; want stored config echoed", resp.Data.ViewConfig)
	}
}

func TestRelViewConfigHTTP_ErrorMapping(t *testing.T) {
	tests := []struct {
		name     string
		svcErr   error
		wantCode int
		wantBody string
	}{
		{"validation 422", &services.ValidationError{Code: "RELVIEW_COLUMN_UNKNOWN", Msg: "field ghost không tồn tại"},
			http.StatusUnprocessableEntity, "RELVIEW_COLUMN_UNKNOWN"},
		{"not found 404", &services.ValidationError{Code: "RELATIONSHIP_NOT_FOUND", Msg: "không tồn tại"},
			http.StatusNotFound, "RELATIONSHIP_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := relViewTestRouter(&fakeRelLifecycle{setErr: tt.svcErr}, true)
			rec := doJSON(r, http.MethodPut, "/api/v1/admin/relationships/rel-1/view-config", map[string]any{"columns": []string{"ghost"}})
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d; want %d (body=%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			var resp struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Code != tt.wantBody {
				t.Errorf("code = %q; want %q", resp.Code, tt.wantBody)
			}
		})
	}
}

func TestRelViewConfigHTTP_MalformedBodyIs400(t *testing.T) {
	r := relViewTestRouter(&fakeRelLifecycle{}, true)
	// doJSON always encodes valid JSON — hand-roll a broken body instead.
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/relationships/rel-1/view-config", strings.NewReader(`{"label":`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d; want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestRelationshipResponse_ViewConfigParsed pins the GET /metadata/relationships DTO
// conversion: a stored JSON string becomes a real object (never an escaped string, RM-02),
// nil stays omitted, and corrupt JSON fails soft (row kept, viewConfig dropped).
func TestRelationshipResponse_ViewConfigParsed(t *testing.T) {
	stored := `{"label":"Đơn hàng","columns":["name"]}`
	rel := metadata.RelationshipDefinition{
		ID: "rel-1", Name: "orders_customer", ParentEntityName: "customers",
		ChildEntityName: "orders", RelationshipType: "1-N", ViewConfig: &stored,
	}
	resp, err := toRelationshipResponse(rel)
	if err != nil {
		t.Fatalf("toRelationshipResponse: %v", err)
	}
	if resp.ViewConfig["label"] != "Đơn hàng" {
		t.Errorf("viewConfig = %+v; want parsed object", resp.ViewConfig)
	}

	rel.ViewConfig = nil
	resp, err = toRelationshipResponse(rel)
	if err != nil || resp.ViewConfig != nil {
		t.Errorf("nil stored config → (%v, %v); want (nil viewConfig, nil err)", resp.ViewConfig, err)
	}

	corrupt := `{broken`
	rel.ViewConfig = &corrupt
	resp, err = toRelationshipResponse(rel)
	if err == nil {
		t.Errorf("corrupt config must surface an error for the caller to log")
	}
	if resp.ID != "rel-1" || resp.ViewConfig != nil {
		t.Errorf("corrupt config must fail soft: row kept, viewConfig dropped; got %+v", resp)
	}
}
