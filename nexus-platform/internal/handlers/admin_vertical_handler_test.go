package handlers

// FE-CATCHUP-2 — GET /admin/verticals/builtin must return the REAL manifest per shipped pack
// ({slug, manifest:{name, version, ...}, entityCount}) instead of bare slugs, so the FE
// vertical page can show a pack's true version pre-install without a static fallback catalog.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services/vertical_pack"
)

// verticalTestRouter mounts the vertical handler behind a context-injecting shim that sets the
// tenant id the way AuthMiddleware would (same pattern as roleTestRouter). RequireAdmin is
// assumed satisfied upstream. The service is nil on purpose: ListBuiltin is a pure in-code
// registry read and must never touch tenant state — a nil-pointer panic here would itself be a
// regression signal that the endpoint started doing writes/reads it should not.
func verticalTestRouter(tenantID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	h := NewAdminVerticalHandler(nil, zap.NewNop())
	r := gin.New()
	if tenantID != "" {
		r.Use(func(c *gin.Context) {
			c.Set("tenantID", tenantID)
			c.Next()
		})
	}
	h.RegisterRoutes(r.Group("/api/v1"))
	return r
}

// builtinListResponse mirrors the {data:{builtins:[{slug,manifest,entityCount}]}} envelope.
type builtinListResponse struct {
	Data struct {
		Builtins []struct {
			Slug     string `json:"slug"`
			Manifest *struct {
				Name        string `json:"name"`
				Slug        string `json:"slug"`
				Version     string `json:"version"`
				Description string `json:"description"`
				Author      string `json:"author"`
			} `json:"manifest"`
			EntityCount int `json:"entityCount"`
		} `json:"builtins"`
	} `json:"data"`
}

// TestListBuiltin_ReturnsManifests pins the new shape against the real in-code registry:
// every shipped slug appears exactly once (sorted), carries its pack's true manifest
// version/name, and reports how many entities installing it would create.
func TestListBuiltin_ReturnsManifests(t *testing.T) {
	r := verticalTestRouter("t-1")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/verticals/builtin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}

	var resp builtinListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := len(resp.Data.Builtins), len(vertical_pack.BuiltinSlugs()); got != want {
		t.Fatalf("builtins count = %d; want %d (%s)", got, want, w.Body.String())
	}

	byBuilt := map[string]int{}
	for i, b := range resp.Data.Builtins {
		byBuilt[b.Slug] = i
	}

	tests := []struct {
		slug            string
		wantName        string
		wantVersion     string
		wantEntityCount int
	}{
		{slug: "clinic", wantName: "Clinic", wantVersion: "1.0.1", wantEntityCount: 2},
		// 1.10.0: `assigned_to` đổi từ `text` sang kiểu `user` — "Phân công cho" hiện TÊN người
		// thay vì UUID trần. (1.9.0: field `file` cho crm_quotes/crm_contracts.)
		// Pin này bắt đúng lần bump — giữ nguyên kiểu pin, chỉ đổi giá trị.
		{slug: "crm", wantName: "CRM", wantVersion: "1.16.0", wantEntityCount: 10},
	}
	for _, tt := range tests {
		t.Run(tt.slug, func(t *testing.T) {
			idx, ok := byBuilt[tt.slug]
			if !ok {
				t.Fatalf("slug %q missing from response: %s", tt.slug, w.Body.String())
			}
			b := resp.Data.Builtins[idx]
			if b.Manifest == nil {
				t.Fatalf("slug %q has no manifest: %s", tt.slug, w.Body.String())
			}
			if b.Manifest.Name != tt.wantName {
				t.Errorf("manifest.name = %q; want %q", b.Manifest.Name, tt.wantName)
			}
			if b.Manifest.Version != tt.wantVersion {
				t.Errorf("manifest.version = %q; want %q", b.Manifest.Version, tt.wantVersion)
			}
			if b.Manifest.Slug != tt.slug {
				t.Errorf("manifest.slug = %q; want %q", b.Manifest.Slug, tt.slug)
			}
			if b.EntityCount != tt.wantEntityCount {
				t.Errorf("entityCount = %d; want %d", b.EntityCount, tt.wantEntityCount)
			}

			// Cross-check against the registry itself so the endpoint can never drift from the
			// packs it claims to describe (no hardcoded mirror on the server side either).
			pack, found := vertical_pack.BuiltinPack(tt.slug)
			if !found {
				t.Fatalf("registry lost slug %q", tt.slug)
			}
			if pack.Manifest.Version != b.Manifest.Version {
				t.Errorf("response version %q diverged from registry %q", b.Manifest.Version, pack.Manifest.Version)
			}
			if len(pack.Entities) != b.EntityCount {
				t.Errorf("response entityCount %d diverged from registry %d", b.EntityCount, len(pack.Entities))
			}
		})
	}

	// Sorted-by-slug order (BuiltinSlugs contract): clinic before crm.
	if resp.Data.Builtins[0].Slug != "clinic" || resp.Data.Builtins[1].Slug != "crm" {
		t.Errorf("builtins not sorted by slug: %s", w.Body.String())
	}
}

// TestListBuiltin_NoTenantClaim_401 pins the auth gate: without a tenant claim in context
// (mounting bug / unauthenticated), the endpoint refuses with the generic 401.
func TestListBuiltin_NoTenantClaim_401(t *testing.T) {
	r := verticalTestRouter("")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/verticals/builtin", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without tenant claim, got %d (%s)", w.Code, w.Body.String())
	}
}
