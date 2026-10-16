package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// fakeRoleRepoH is a tiny in-memory role repo to drive the handler through the real RoleService
// (handler→service→repo). It is keyed by (tenant,id) and grants by roleID.
type fakeRoleRepoH struct {
	roles   map[string]*auth.Role
	actions map[string][]auth.RoleAction
}

func newFakeRoleRepoH() *fakeRoleRepoH {
	return &fakeRoleRepoH{roles: map[string]*auth.Role{}, actions: map[string][]auth.RoleAction{}}
}
func (f *fakeRoleRepoH) k(t, id string) string { return t + "\x00" + id }
func (f *fakeRoleRepoH) ListRoles(_ context.Context, t string) ([]auth.Role, error) {
	var out []auth.Role
	for _, r := range f.roles {
		if r.TenantID == t {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (f *fakeRoleRepoH) GetRoleByID(_ context.Context, t, id string) (*auth.Role, error) {
	if r, ok := f.roles[f.k(t, id)]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, auth.ErrRoleNotFound
}
func (f *fakeRoleRepoH) CreateRole(_ context.Context, role *auth.Role, actions []auth.RoleAction) error {
	for _, ex := range f.roles {
		if ex.TenantID == role.TenantID && ex.Name == role.Name {
			return auth.ErrRoleAlreadyExists
		}
	}
	cp := *role
	f.roles[f.k(role.TenantID, role.ID)] = &cp
	for i := range actions {
		actions[i].RoleID = role.ID
	}
	f.actions[role.ID] = append([]auth.RoleAction(nil), actions...)
	return nil
}
func (f *fakeRoleRepoH) UpdateRole(_ context.Context, t, id string, fields map[string]any, actions []auth.RoleAction) (*auth.Role, error) {
	r, ok := f.roles[f.k(t, id)]
	if !ok {
		return nil, auth.ErrRoleNotFound
	}
	if v, ok := fields["name"].(string); ok {
		r.Name = v
	}
	if v, ok := fields["label"].(string); ok {
		r.Label = v
	}
	if actions != nil {
		f.actions[id] = append([]auth.RoleAction(nil), actions...)
	}
	cp := *r
	return &cp, nil
}
func (f *fakeRoleRepoH) DeleteRole(_ context.Context, t, id string) error {
	k := f.k(t, id)
	if _, ok := f.roles[k]; !ok {
		return auth.ErrRoleNotFound
	}
	delete(f.roles, k)
	delete(f.actions, id)
	return nil
}
func (f *fakeRoleRepoH) ListActionsForRole(_ context.Context, t, id string) ([]auth.RoleAction, error) {
	if _, ok := f.roles[f.k(t, id)]; !ok {
		return nil, auth.ErrRoleNotFound
	}
	return append([]auth.RoleAction(nil), f.actions[id]...), nil
}

// roleTestRouter mounts the role handler with a context-injecting shim that sets the tenant id
// (the way AuthMiddleware would). RequireAdmin is assumed satisfied upstream; these tests focus
// on the handler↔service contract and tenant scoping.
func roleTestRouter(tenantID string) (*gin.Engine, *fakeRoleRepoH) {
	gin.SetMode(gin.TestMode)
	repo := newFakeRoleRepoH()
	svc := services.NewRoleService(repo)
	h := NewAdminRoleHandler(svc, nil)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("tenantID", tenantID)
		c.Next()
	})
	grp := r.Group("/api/v1")
	h.RegisterRoutes(grp)
	return r, repo
}

func doJSON(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// R1: POST creates a role (201), GET lists/gets it, PUT updates it, DELETE removes it.
func TestRoleHandler_FullCRUD(t *testing.T) {
	r, _ := roleTestRouter("t1")

	// Create
	rec := doJSON(r, http.MethodPost, "/api/v1/admin/roles", map[string]any{
		"name": "sales", "label": "Sales",
		"actions": []map[string]any{{"entityName": "contacts", "action": "view"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body=%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Data struct {
			Role services.RoleView `json:"role"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id := created.Data.Role.ID
	if id == "" {
		t.Fatalf("created role has no id")
	}

	// List
	rec = doJSON(r, http.MethodGet, "/api/v1/admin/roles", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}

	// Get
	rec = doJSON(r, http.MethodGet, "/api/v1/admin/roles/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// Update
	rec = doJSON(r, http.MethodPut, "/api/v1/admin/roles/"+id, map[string]any{
		"label":   "Sales Team",
		"actions": []map[string]any{{"entityName": "*", "action": "view"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	// Delete -> 204 No Content, empty body (#8: DELETE /admin/* returns 204 on success).
	rec = doJSON(r, http.MethodDelete, "/api/v1/admin/roles/"+id, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("delete 204 must have empty body, got %q", rec.Body.String())
	}

	// Get after delete → 404
	rec = doJSON(r, http.MethodGet, "/api/v1/admin/roles/"+id, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", rec.Code)
	}
}

// R1: a duplicate name → 409.
func TestRoleHandler_DuplicateIs409(t *testing.T) {
	r, _ := roleTestRouter("t1")
	body := map[string]any{"name": "dup", "label": "Dup"}
	if rec := doJSON(r, http.MethodPost, "/api/v1/admin/roles", body); rec.Code != http.StatusCreated {
		t.Fatalf("first create = %d, want 201", rec.Code)
	}
	rec := doJSON(r, http.MethodPost, "/api/v1/admin/roles", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup create = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
}

// R1: an invalid action verb → 422.
func TestRoleHandler_InvalidActionIs422(t *testing.T) {
	r, _ := roleTestRouter("t1")
	rec := doJSON(r, http.MethodPost, "/api/v1/admin/roles", map[string]any{
		"name": "x", "label": "X",
		"actions": []map[string]any{{"entityName": "contacts", "action": "drop"}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid action = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
}

// R2: a role created in t1 is not gettable from t2 (404). Tenant comes from the verified claim.
func TestRoleHandler_TenantScoped(t *testing.T) {
	r1, repo := roleTestRouter("t1")
	rec := doJSON(r1, http.MethodPost, "/api/v1/admin/roles", map[string]any{"name": "r", "label": "R"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d", rec.Code)
	}
	var created struct {
		Data struct {
			Role services.RoleView `json:"role"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.Data.Role.ID

	// Build a router for t2 over the SAME repo so the role physically exists, just in another tenant.
	gin.SetMode(gin.TestMode)
	svc := services.NewRoleService(repo)
	h := NewAdminRoleHandler(svc, nil)
	r2 := gin.New()
	r2.Use(func(c *gin.Context) { c.Set("tenantID", "t2"); c.Next() })
	h.RegisterRoutes(r2.Group("/api/v1"))

	rec2 := doJSON(r2, http.MethodGet, "/api/v1/admin/roles/"+id, nil)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d, want 404", rec2.Code)
	}
}
