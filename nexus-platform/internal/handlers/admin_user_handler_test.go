package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeUserCRUDRepo satisfies the (unexported) services.userCRUDRepo interface structurally so
// the handler tests can drive the REAL UserService — including its is_admin gate — without a DB.
type fakeUserCRUDRepo struct {
	rows map[string]*auth.User // keyed by tenantID + "\x00" + id
}

func newFakeUserCRUDRepo() *fakeUserCRUDRepo {
	return &fakeUserCRUDRepo{rows: map[string]*auth.User{}}
}

func (f *fakeUserCRUDRepo) key(tid, id string) string { return tid + "\x00" + id }

func (f *fakeUserCRUDRepo) ListUsers(_ context.Context, tenantID string) ([]auth.User, error) {
	var out []auth.User
	for _, u := range f.rows {
		if u.TenantID == tenantID {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (f *fakeUserCRUDRepo) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	if u, ok := f.rows[f.key(tenantID, id)]; ok {
		return u, nil
	}
	return nil, auth.ErrUserNotFound
}

func (f *fakeUserCRUDRepo) CreateUser(_ context.Context, u *auth.User) error {
	for _, ex := range f.rows {
		if ex.TenantID == u.TenantID && ex.Email == u.Email {
			return auth.ErrUserAlreadyExists
		}
	}
	cp := *u
	f.rows[f.key(u.TenantID, u.ID)] = &cp
	return nil
}

func (f *fakeUserCRUDRepo) UpdateUser(_ context.Context, tenantID, id string, fields map[string]any) (*auth.User, error) {
	u, ok := f.rows[f.key(tenantID, id)]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	if v, ok := fields["full_name"].(string); ok {
		u.FullName = v
	}
	if v, ok := fields["status"].(string); ok {
		u.Status = v
	}
	if v, ok := fields["can_manage_users"].(bool); ok {
		u.CanManageUsers = v
	}
	if rv, present := fields["role_id"]; present {
		if rv == nil {
			u.RoleID = nil
		} else if s, ok := rv.(string); ok {
			u.RoleID = &s
		}
	}
	cp := *u
	return &cp, nil
}

func (f *fakeUserCRUDRepo) SoftDeleteUser(_ context.Context, tenantID, id string) error {
	k := f.key(tenantID, id)
	if _, ok := f.rows[k]; !ok {
		return auth.ErrUserNotFound
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeUserCRUDRepo) CountActiveAdmins(_ context.Context, tenantID string) (int, error) {
	n := 0
	for _, u := range f.rows {
		if u.TenantID == tenantID && u.IsAdmin && u.Status == "active" {
			n++
		}
	}
	return n, nil
}

// UpdateUserGuardLastAdmin / SoftDeleteUserGuardLastAdmin mirror the real repo's atomic
// last-admin guard (auth.ErrLastAdmin when only one active admin remains) so the handler tests
// exercise the service's demote/delete-admin paths through the same interface as production.
func (f *fakeUserCRUDRepo) UpdateUserGuardLastAdmin(ctx context.Context, tenantID, id string, fields map[string]any) (*auth.User, error) {
	n, _ := f.CountActiveAdmins(ctx, tenantID)
	if n <= 1 {
		return nil, auth.ErrLastAdmin
	}
	return f.UpdateUser(ctx, tenantID, id, fields)
}

// UpdateUserIfManageable mirrors the real conditional write: only a manageable target is touched.
func (f *fakeUserCRUDRepo) UpdateUserIfManageable(ctx context.Context, tenantID, id, actorID string, fields map[string]any) (*auth.User, error) {
	u, ok := f.rows[f.key(tenantID, id)]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	if !u.ManageableBy(actorID) {
		return nil, auth.ErrUserNotManageable
	}
	return f.UpdateUser(ctx, tenantID, id, fields)
}

func (f *fakeUserCRUDRepo) SoftDeleteUserGuardLastAdmin(ctx context.Context, tenantID, id string) error {
	n, _ := f.CountActiveAdmins(ctx, tenantID)
	if n <= 1 {
		return auth.ErrLastAdmin
	}
	return f.SoftDeleteUser(ctx, tenantID, id)
}

// newUserHandler builds the handler over the real service + fake repo.
func newUserHandler() (*AdminUserHandler, *fakeUserCRUDRepo) {
	repo := newFakeUserCRUDRepo()
	svc := services.NewUserService(repo)
	return NewAdminUserHandler(svc, zap.NewNop()), repo
}

// doUserRequest drives a single request through a gin engine whose context carries verified
// claims (tenant + is_admin) the way AuthMiddleware would, then RequireAdmin/the handler read
// them. We mount the handler directly (no admin middleware) so these tests isolate the
// HANDLER's own is_admin gate (passed into the service) from the route-level RequireAdmin.
func doUserRequest(h *AdminUserHandler, method, path string, isAdmin bool, body any) *httptest.ResponseRecorder {
	r := gin.New()
	// Inject verified claims into context for every request (simulates AuthMiddleware output).
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject: auth.Subject{
				UserID:   "actor-1",
				TenantID: "tenant-1",
				IsAdmin:  isAdmin,
			},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "tenant-1")
		c.Next()
	})
	h.RegisterRoutes(r.Group(""))

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

// 🔒 A non-admin actor reaching the create handler must be refused with 403 (the service gate),
// and no user must be written.
func TestAdminUserHandler_CreateNonAdmin403(t *testing.T) {
	h, repo := newUserHandler()
	rec := doUserRequest(h, http.MethodPost, "/admin/users", false, map[string]any{
		"email": "x@acme.test", "password": "secret123",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.rows) != 0 {
		t.Fatalf("non-admin create wrote %d rows; want 0", len(repo.rows))
	}
}

// 🔒 A non-admin actor reaching the delete handler must be refused with 403, row preserved.
func TestAdminUserHandler_DeleteNonAdmin403(t *testing.T) {
	h, repo := newUserHandler()
	// Seed a row directly so there is something a delete could (wrongly) remove.
	repo.rows[repo.key("tenant-1", "u1")] = &auth.User{ID: "u1", TenantID: "tenant-1", Email: "v@acme.test"}
	rec := doUserRequest(h, http.MethodDelete, "/admin/users/u1", false, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(repo.rows) != 1 {
		t.Fatalf("non-admin delete removed the row; want it preserved")
	}
}

// An admin create succeeds with 201 and the response never leaks a password hash.
func TestAdminUserHandler_CreateAdmin201NoHashLeak(t *testing.T) {
	h, _ := newUserHandler()
	rec := doUserRequest(h, http.MethodPost, "/admin/users", true, map[string]any{
		"email": "ceo@acme.test", "password": "topsecret", "isAdmin": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if bytes.Contains([]byte(body), []byte("topsecret")) {
		t.Fatalf("response leaked the plaintext password: %s", body)
	}
	if bytes.Contains([]byte(body), []byte("$2a$")) || bytes.Contains([]byte(body), []byte("passwordHash")) {
		t.Fatalf("response leaked a password hash: %s", body)
	}
}

// An admin delete succeeds with 204 No Content and removes the row.
func TestAdminUserHandler_DeleteAdmin200(t *testing.T) {
	h, repo := newUserHandler()
	repo.rows[repo.key("tenant-1", "u1")] = &auth.User{ID: "u1", TenantID: "tenant-1", Email: "v@acme.test"}
	rec := doUserRequest(h, http.MethodDelete, "/admin/users/u1", true, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("delete 204 must have empty body, got %q", rec.Body.String())
	}
	if len(repo.rows) != 0 {
		t.Fatalf("admin delete did not remove the row")
	}
}

// List returns the tenant's users with 200 (admin).
func TestAdminUserHandler_List200(t *testing.T) {
	h, repo := newUserHandler()
	repo.rows[repo.key("tenant-1", "u1")] = &auth.User{ID: "u1", TenantID: "tenant-1", Email: "a@acme.test"}
	rec := doUserRequest(h, http.MethodGet, "/admin/users", true, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// Get an unknown id returns 404.
func TestAdminUserHandler_GetNotFound404(t *testing.T) {
	h, _ := newUserHandler()
	rec := doUserRequest(h, http.MethodGet, "/admin/users/nope", true, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// 🔒 An admin deleting its OWN account (actor-1 == :id) is refused with 422 SELF_DELETE_FORBIDDEN.
// doUserRequest injects actor-1 as the verified subject, so the handler must pass it through.
func TestAdminUserHandler_SelfDelete422(t *testing.T) {
	h, repo := newUserHandler()
	// actor-1 is the acting admin; seed a sibling so last-admin isn't the trip reason.
	repo.rows[repo.key("tenant-1", "actor-1")] = &auth.User{ID: "actor-1", TenantID: "tenant-1", Email: "me@acme.test", IsAdmin: true, Status: "active"}
	repo.rows[repo.key("tenant-1", "admin-2")] = &auth.User{ID: "admin-2", TenantID: "tenant-1", Email: "b@acme.test", IsAdmin: true, Status: "active"}
	rec := doUserRequest(h, http.MethodDelete, "/admin/users/actor-1", true, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := repo.rows[repo.key("tenant-1", "actor-1")]; !ok {
		t.Fatalf("self-delete removed the row; want preserved")
	}
}

// 🔒 Deleting the LAST active admin is refused with 422 LAST_ADMIN, row preserved.
func TestAdminUserHandler_LastAdminDelete422(t *testing.T) {
	h, repo := newUserHandler()
	repo.rows[repo.key("tenant-1", "admin-x")] = &auth.User{ID: "admin-x", TenantID: "tenant-1", Email: "x@acme.test", IsAdmin: true, Status: "active"}
	rec := doUserRequest(h, http.MethodDelete, "/admin/users/admin-x", true, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := repo.rows[repo.key("tenant-1", "admin-x")]; !ok {
		t.Fatalf("last-admin delete removed the row; want preserved")
	}
}
