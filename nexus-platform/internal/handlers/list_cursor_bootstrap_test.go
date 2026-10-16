package handlers

// CURSOR-BOOTSTRAP (queue #42, specs/039-cursor-bootstrap) — GET /:entity?paginate=cursor opens a
// keyset chain from the FIRST request: the response carries pagination.nextCursor with no ?after=
// in hand. Before this, cursor mode only switched on for a non-empty ?after=, and page 1 never
// returned a cursor, so no client could ever enter it.
//
// Runs the REAL production chain (AuthMiddleware → RequireTenant → RequireACL →
// DynamicRecordHandler → services.RecordService) with fakes only at the leaves, reusing the
// hiddenMeta* fixtures from list_hidden_meta_test.go (same package).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// cursorCaptureStore records the CursorParams the service hands the keyset path and serves one
// row plus a next-page token, so the envelope can be checked end to end.
type cursorCaptureStore struct {
	hiddenMetaStore
	cursorCalls int
	got         dynrepo.CursorParams
}

func (s *cursorCaptureStore) ListRecordsCursor(_ context.Context, _, tenantID string, _ dynrepo.RowScope, p dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	s.cursorCalls++
	s.got = p
	return []dynrepo.Record{{ID: "rec-1", TenantID: tenantID, Data: map[string]any{"name": "Acme"}, Version: 1}}, "next-token", nil
}

func buildCursorBootstrapEngine(t *testing.T) (*gin.Engine, *cursorCaptureStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}},
	}
	store := &cursorCaptureStore{}
	svc := services.NewRecordService(cat, store).WithFieldACL(&hiddenMetaACL{})
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"member": claimsFor("t1", "role-1", false)}}
	checker := &multiRoleViewChecker{tenant: "t1", entity: "contacts", roles: map[string]bool{"role-1": true}}

	r := gin.New()
	protected := r.Group("/api/v1")
	protected.Use(middleware.AuthMiddleware(ver, zap.NewNop()))
	protected.Use(middleware.RequireTenant(zap.NewNop()))
	recordGroup := protected.Group("")
	recordGroup.Use(middleware.RequireACL(checker, zap.NewNop()))
	NewDynamicRecordHandler(svc, zap.NewNop()).RegisterRoutes(recordGroup)
	return r, store
}

func getAsMember(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer member")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The first request with ?paginate=cursor (no ?after=) is served by the keyset path and hands
// back pagination.nextCursor — the entrance that did not exist before.
func TestList_PaginateCursor_FirstPageReturnsNextCursor(t *testing.T) {
	r, store := buildCursorBootstrapEngine(t)
	rec := getAsMember(t, r, "/api/v1/contacts?paginate=cursor&sort=created_at&order=desc&limit=100")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if got := env.Pagination["nextCursor"]; got != "next-token" {
		t.Errorf("pagination.nextCursor = %v; want \"next-token\" on the FIRST request", got)
	}
	if _, hasTotal := env.Pagination["total"]; hasTotal {
		t.Errorf("cursor envelope carries total (%v) — keyset mode must not COUNT(*)", env.Pagination)
	}
	if store.cursorCalls != 1 {
		t.Fatalf("keyset path called %d times; want 1", store.cursorCalls)
	}
	if store.got.After != "" || store.got.Sort != "created_at" || store.got.Order != "DESC" || store.got.Limit != 100 {
		t.Errorf("CursorParams = %+v; want After=\"\" Sort=created_at Order=DESC Limit=100", store.got)
	}
	// paginate is a reserved key, never a record filter.
	if len(store.got.Filters) != 0 {
		t.Errorf("paginate leaked into filters: %+v", store.got.Filters)
	}
}

// Every request that cannot be served honestly is refused with a named code instead of being
// quietly turned into something else.
func TestList_PaginateMode_Rejections(t *testing.T) {
	cases := []struct {
		name string
		path string
		want string
	}{
		{"cursor with page", "/api/v1/contacts?paginate=cursor&page=5", "PAGINATION_MODE_CONFLICT"},
		{"after with page", "/api/v1/contacts?after=" + dynrepo.EncodeCursor("created_at", "DESC", false, "2026-09-01 00:00:00", "rec-9") + "&page=2", "PAGINATION_MODE_CONFLICT"},
		{"offset with after", "/api/v1/contacts?paginate=offset&after=" + dynrepo.EncodeCursor("created_at", "DESC", false, "2026-09-01 00:00:00", "rec-9"), "PAGINATION_MODE_CONFLICT"},
		{"unknown mode", "/api/v1/contacts?paginate=keyset", "INVALID_PAGINATION_MODE"},
		{"recycle bin is offset-only", "/api/v1/contacts/deleted?paginate=cursor", "PAGINATION_MODE_CONFLICT"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, store := buildCursorBootstrapEngine(t)
			rec := getAsMember(t, r, tc.path)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
			}
			var body struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Code != tc.want {
				t.Errorf("code = %q; want %q", body.Code, tc.want)
			}
			if store.cursorCalls != 0 {
				t.Errorf("keyset path ran %d times for a rejected request", store.cursorCalls)
			}
		})
	}
}

// paginate=offset (the explicit default) keeps the offset envelope byte-compatible.
func TestList_PaginateOffset_KeepsOffsetEnvelope(t *testing.T) {
	r, store := buildCursorBootstrapEngine(t)
	rec := getAsMember(t, r, "/api/v1/contacts?paginate=offset&page=2&limit=20")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"page", "limit", "total", "totalPages"} {
		if _, ok := env.Pagination[k]; !ok {
			t.Errorf("offset envelope missing %q", k)
		}
	}
	if _, ok := env.Pagination["nextCursor"]; ok {
		t.Errorf("offset envelope must not carry nextCursor (never bridge offset into keyset): %v", env.Pagination)
	}
	if store.cursorCalls != 0 {
		t.Errorf("offset request hit the keyset path")
	}
}
