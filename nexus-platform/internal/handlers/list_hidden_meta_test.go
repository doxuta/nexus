package handlers

// HIDDEN-COL-SIGNAL — envelope lock for GET /:entity. When the caller's field-ACL hides ≥1
// declared field of the entity, the list envelope gains an ADDITIVE `meta.hiddenColumns` key;
// when nothing is hidden (member with no restrictions, admin bypass) the key is ABSENT — so the
// FE chip driven by it can never be a false positive. data/pagination shapes stay unchanged.
//
// The request runs the REAL production chain (AuthMiddleware → RequireTenant → RequireACL →
// DynamicRecordHandler.List → services.RecordService) with handler-local fakes only at the
// leaves (catalog / store / field-ACL enforcer), reusing the stubVerifier / claimsFor /
// aclWiringChecker fixtures from acl_wiring_test.go (same package).

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

// hiddenMetaCatalog serves one fixed entity + its declared fields.
type hiddenMetaCatalog struct {
	entity *metadata.EntityDefinition
	fields []metadata.FieldDefinition
}

func (c *hiddenMetaCatalog) GetEntityByName(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
	return c.entity, nil
}
func (c *hiddenMetaCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return c.fields, nil
}

// hiddenMetaStore returns one canned row so the envelope has real data alongside meta.
type hiddenMetaStore struct{}

func (s *hiddenMetaStore) InsertRecord(_ context.Context, _, _, _ string, _ map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	return "", nil
}
func (s *hiddenMetaStore) GetRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return nil, dynrepo.ErrRecordNotFound
}
func (s *hiddenMetaStore) ListRecords(_ context.Context, _, tenantID string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return []dynrepo.Record{
		{ID: "rec-1", TenantID: tenantID, Data: map[string]any{"name": "Acme"}, Version: 1},
	}, 1, nil
}
func (s *hiddenMetaStore) ListRecordsCursor(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	return nil, "", nil
}
func (s *hiddenMetaStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (s *hiddenMetaStore) SoftDeleteRecord(_ context.Context, _, _, _ string, _ dynrepo.RowScope) error {
	return nil
}
func (s *hiddenMetaStore) RestoreRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ []dynrepo.UniqueFieldSpec) error {
	return nil
}
func (s *hiddenMetaStore) RecordExists(_ context.Context, _, _, _ string) (bool, error) {
	return true, nil
}
func (s *hiddenMetaStore) BatchRecordExists(_ context.Context, _, _ string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
func (s *hiddenMetaStore) AggregateScalar(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	return dynrepo.AggregateScalar{}, nil
}
func (s *hiddenMetaStore) AggregateGrouped(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	return nil, nil
}
func (s *hiddenMetaStore) AggregateGrid(_ context.Context, _, _ string, _ dynrepo.RowScope, _ dynrepo.GridParams) ([]dynrepo.GridRow, []dynrepo.GridRow, error) {
	return nil, nil, nil
}

// SetOwner is a no-op stub — this fixture only exercises List's HIDDEN-COL-SIGNAL envelope, never
// reassignment (services.RecordStore interface compliance only; unrelated to record-notes).
func (s *hiddenMetaStore) SetOwner(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, _ string) error {
	return nil
}

// hiddenMetaACL hides a per-role field set on every entity; it satisfies the record service's
// consumer-side field-ACL seam (all-exported methods) via WithFieldACL. FilterReadFieldsBatch
// actually strips so the test's rows mirror production (hidden values never in the body).
type hiddenMetaACL struct {
	byRole map[string]map[string]struct{} // roleID → hidden field names
}

func (f *hiddenMetaACL) set(roleID string) map[string]struct{} {
	if f.byRole == nil {
		return nil
	}
	return f.byRole[roleID]
}
func (f *hiddenMetaACL) FilterReadFields(_ context.Context, _, roleID, _ string, isAdmin bool, data map[string]any) error {
	if isAdmin {
		return nil
	}
	for name := range f.set(roleID) {
		delete(data, name)
	}
	return nil
}
func (f *hiddenMetaACL) FilterReadFieldsBatch(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, datas []map[string]any) error {
	for _, d := range datas {
		if err := f.FilterReadFields(ctx, tenantID, roleID, entity, isAdmin, d); err != nil {
			return err
		}
	}
	return nil
}
func (f *hiddenMetaACL) CheckWriteFields(_ context.Context, _, _, _ string, _ bool, _ map[string]any) error {
	return nil
}

// CheckWriteFieldsAgainst delegates to CheckWriteFields: these fakes have no notion of a
// record's current values, and the update-path rule (an unchanged locked field is a no-op)
// is exercised in the field-ACL tests that DO model them.
func (f *hiddenMetaACL) CheckWriteFieldsAgainst(ctx context.Context, t, r, e string, isAdmin bool, payload, _ map[string]any) error {
	return f.CheckWriteFields(ctx, t, r, e, isAdmin, payload)
}
func (f *hiddenMetaACL) AssertFieldVisible(_ context.Context, _, _, _ string, _ bool, _ string) error {
	return nil
}
func (f *hiddenMetaACL) AssertFieldsVisible(_ context.Context, _, _, _ string, _ bool, _ []string) error {
	return nil
}
func (f *hiddenMetaACL) HiddenFields(_ context.Context, _, roleID, _ string, isAdmin bool) (map[string]struct{}, error) {
	if isAdmin {
		return map[string]struct{}{}, nil
	}
	return f.set(roleID), nil
}

// buildHiddenMetaEngine mounts the production chain onto GET /:entity with a real RecordService.
// role-1 has salary+phone hidden; role-2 has nothing hidden; admin bypasses ACL entirely.
func buildHiddenMetaEngine(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "phone", FieldType: "text"},
			{Name: "salary", FieldType: "number"},
		},
	}
	facl := &hiddenMetaACL{byRole: map[string]map[string]struct{}{
		"role-1": {"salary": {}, "phone": {}},
	}}
	svc := services.NewRecordService(cat, &hiddenMetaStore{}).WithFieldACL(facl)

	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"member":   claimsFor("t1", "role-1", false),
		"member2":  claimsFor("t1", "role-2", false),
		"adminTok": claimsFor("t1", "", true),
	}}
	// Grant view on contacts to both member roles; the admin bypasses inside RequireACL.
	checker := &multiRoleViewChecker{tenant: "t1", entity: "contacts", roles: map[string]bool{"role-1": true, "role-2": true}}

	r := gin.New()
	api := r.Group("/api/v1")
	protected := api.Group("")
	protected.Use(middleware.AuthMiddleware(ver, zap.NewNop()))
	protected.Use(middleware.RequireTenant(zap.NewNop()))
	recordGroup := protected.Group("")
	recordGroup.Use(middleware.RequireACL(checker, zap.NewNop()))
	NewDynamicRecordHandler(svc, zap.NewNop()).RegisterRoutes(recordGroup)
	return r
}

// multiRoleViewChecker authorizes "view" on one entity for a set of roles (row level "all").
type multiRoleViewChecker struct {
	tenant, entity string
	roles          map[string]bool
}

func (c *multiRoleViewChecker) Authorize(_ context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	if tenantID == c.tenant && entity == c.entity && action == "view" && c.roles[roleID] {
		return true, "all", nil
	}
	return false, "", nil
}

// listEnvelope is the decoded response body under test.
type listEnvelope struct {
	Data       []map[string]any `json:"data"`
	Pagination map[string]any   `json:"pagination"`
	Meta       *struct {
		HiddenColumns int `json:"hiddenColumns"`
	} `json:"meta"`
}

func doHiddenMetaList(t *testing.T, r *gin.Engine, token string) listEnvelope {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/contacts?page=1&limit=20", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var env listEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v (%s)", err, rec.Body.String())
	}
	return env
}

// A member whose role hides 2 declared fields gets meta.hiddenColumns=2 — and the offset
// pagination block is untouched (additive key only, backward-compatible).
func TestListEnvelope_HiddenColumns_MemberSeesCount(t *testing.T) {
	r := buildHiddenMetaEngine(t)
	env := doHiddenMetaList(t, r, "member")

	if env.Meta == nil {
		t.Fatalf("meta absent; want meta.hiddenColumns=2")
	}
	if env.Meta.HiddenColumns != 2 {
		t.Errorf("meta.hiddenColumns = %d; want 2", env.Meta.HiddenColumns)
	}
	// Backward compat: the offset pagination block keeps its four keys.
	for _, k := range []string{"page", "limit", "total", "totalPages"} {
		if _, ok := env.Pagination[k]; !ok {
			t.Errorf("pagination missing %q — envelope shape must not change", k)
		}
	}
	// The hidden values themselves must still be stripped from rows (RM-42 unchanged).
	if len(env.Data) != 1 {
		t.Fatalf("data len = %d; want 1", len(env.Data))
	}
	rowData, _ := env.Data[0]["data"].(map[string]any)
	if _, leaked := rowData["salary"]; leaked {
		t.Errorf("hidden field 'salary' leaked into row data: %v", rowData)
	}
}

// A member whose role hides NOTHING gets no meta key at all (absent, not 0).
func TestListEnvelope_HiddenColumns_NoRestrictionsOmitsMeta(t *testing.T) {
	r := buildHiddenMetaEngine(t)
	env := doHiddenMetaList(t, r, "member2")
	if env.Meta != nil {
		t.Errorf("meta present (%+v); want ABSENT when nothing is hidden", env.Meta)
	}
}

// An admin bypasses field-ACL (sees every column) → meta must be absent, or the chip would be a
// 100%-false-positive for every admin screen.
func TestListEnvelope_HiddenColumns_AdminOmitsMeta(t *testing.T) {
	r := buildHiddenMetaEngine(t)
	env := doHiddenMetaList(t, r, "adminTok")
	if env.Meta != nil {
		t.Errorf("meta present for admin (%+v); want ABSENT (admin sees all columns)", env.Meta)
	}
}
