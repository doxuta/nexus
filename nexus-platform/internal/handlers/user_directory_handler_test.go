package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// fakeUserDirectoryRepo satisfies the (unexported) services.directoryRepo interface structurally
// so this handler test can drive a REAL services.UserDirectoryService without a DB. The actual
// tenant/status/q WHERE-clause filtering lives in auth.UserRepository.ListDirectory and is proven
// by the DB-backed integration test (internal/handlers/user_directory_integration_test.go,
// -tags=integration); this file proves HTTP wiring + response shape — in particular that the JSON
// body NEVER carries a field beyond {id, fullName, email}.
type fakeUserDirectoryRepo struct {
	rows              []auth.DirectoryUser
	total             int64
	err               error
	gotTenantID       string
	gotQ              string
	gotLimit, gotOffs int
}

func (f *fakeUserDirectoryRepo) ListDirectory(_ context.Context, tenantID, q string, limit, offset int) ([]auth.DirectoryUser, int64, error) {
	f.gotTenantID, f.gotQ, f.gotLimit, f.gotOffs = tenantID, q, limit, offset
	if f.err != nil {
		return nil, 0, f.err
	}
	return f.rows, f.total, nil
}

// doDirectoryRequest wires UserDirectoryHandler behind a minimal stand-in for AuthMiddleware +
// RequireTenant (same technique as doUserRequest in admin_user_handler_test.go): it injects the
// verified claim + tenantID context keys tenantOf/scopeOf read, without exercising the real JWT
// verifier. isAdmin is threaded so a test can prove the route works for a NON-admin caller — the
// entire point of this endpoint (unlike /admin/users).
func doDirectoryRequest(h *UserDirectoryHandler, tenantID string, isAdmin bool, path string) *httptest.ResponseRecorder {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "caller-1", TenantID: tenantID, IsAdmin: isAdmin},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", tenantID)
		c.Next()
	})
	h.RegisterRoutes(r.Group(""))
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// TestUserDirectoryHandler_List_NonAdminGetsOK is the headline regression test for the FE-gap this
// endpoint closes: a NON-admin caller (isAdmin=false) must get 200 — /admin/users refuses this
// caller with 403, which is exactly the bug (owner-reassign dialog cannot load anyone to pick).
func TestUserDirectoryHandler_List_NonAdminGetsOK(t *testing.T) {
	repo := &fakeUserDirectoryRepo{
		rows:  []auth.DirectoryUser{{ID: "u1", FullName: "Nguyễn Văn A", Email: "a@co.vn"}},
		total: 1,
	}
	h := NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop())

	rec := doDirectoryRequest(h, "tenant-1", false, "/users/directory")

	if rec.Code != http.StatusOK {
		t.Fatalf("non-admin caller status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.gotTenantID != "tenant-1" {
		t.Errorf("repo saw tenantID=%q, want tenant-1", repo.gotTenantID)
	}
}

// TestUserDirectoryHandler_List_NoSensitiveField is the field-leak regression test the task
// demands: decode the RAW body (not a typed struct — a typed struct silently drops unknown keys
// and would pass even if the handler started leaking password_hash) and assert every item's key
// set is EXACTLY {id, fullName, email}, plus a raw-byte substring check for every field this
// endpoint must never expose.
func TestUserDirectoryHandler_List_NoSensitiveField(t *testing.T) {
	repo := &fakeUserDirectoryRepo{
		rows: []auth.DirectoryUser{
			{ID: "u1", FullName: "Nguyễn Văn A", Email: "a@co.vn"},
			{ID: "u2", FullName: "", Email: "b@co.vn"}, // exercises COALESCE-to-empty at the service boundary too
		},
		total: 2,
	}
	h := NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop())

	rec := doDirectoryRequest(h, "tenant-1", false, "/users/directory")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()

	// Forbidden substrings: password hash marker, admin/role/status/MFA fields, tenantId. None may
	// ever appear in this response, regardless of what a future DTO refactor does upstream.
	forbidden := []string{
		"password", "$2a$", "$2b$", "passwordHash",
		`"isAdmin"`, `"roleId"`, `"status"`, "mfa", "Mfa", "MFA",
		`"tenantId"`, `"failedLoginCount"`, `"lockedUntil"`,
	}
	for _, f := range forbidden {
		if strings.Contains(raw, f) {
			t.Errorf("response body contains forbidden substring %q; body=%s", f, raw)
		}
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v; body=%s", err, raw)
	}
	if len(envelope.Data) != 2 {
		t.Fatalf("data length = %d, want 2; body=%s", len(envelope.Data), raw)
	}
	wantKeys := map[string]bool{"id": true, "fullName": true, "email": true}
	for i, item := range envelope.Data {
		if len(item) != len(wantKeys) {
			t.Errorf("item[%d] has %d keys (%v), want exactly %v", i, len(item), keysOf(item), wantKeys)
			continue
		}
		for k := range item {
			if !wantKeys[k] {
				t.Errorf("item[%d] has unexpected key %q — leaked field beyond {id,fullName,email}: %v", i, k, item)
			}
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestUserDirectoryHandler_List_QueryParamsForwarded proves page/limit/q parsed from the query
// string reach the service (and therefore the repo) — the service-level clamping itself is
// covered by internal/services/user_directory_test.go.
func TestUserDirectoryHandler_List_QueryParamsForwarded(t *testing.T) {
	repo := &fakeUserDirectoryRepo{}
	h := NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop())

	rec := doDirectoryRequest(h, "tenant-1", false, "/users/directory?q=alice&page=2&limit=5")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.gotLimit != 5 {
		t.Errorf("repo limit = %d, want 5", repo.gotLimit)
	}
	if repo.gotOffs != 5 { // page=2, limit=5 -> offset=(2-1)*5
		t.Errorf("repo offset = %d, want 5", repo.gotOffs)
	}
	if repo.gotQ != "alice" {
		t.Errorf("repo q = %q, want alice", repo.gotQ)
	}
}

// TestUserDirectoryHandler_List_ServiceErrorMapsTo500 proves a repository-layer failure surfaces
// as a logged 500 USER_DIRECTORY_FAILED, never a silently-swallowed error / empty 200.
func TestUserDirectoryHandler_List_ServiceErrorMapsTo500(t *testing.T) {
	repo := &fakeUserDirectoryRepo{err: errors.New("db exploded")}
	h := NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop())

	rec := doDirectoryRequest(h, "tenant-1", false, "/users/directory")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "USER_DIRECTORY_FAILED") {
		t.Errorf("body = %s, want code USER_DIRECTORY_FAILED", rec.Body.String())
	}
}

// TestUserDirectoryHandler_List_EmptyResultIsEmptyArrayNotNull proves a zero-row page serializes
// as `"data":[]`, never `"data":null` (a common Go nil-slice JSON footgun that would break an FE
// `.map()`/`.filter()` over the response).
func TestUserDirectoryHandler_List_EmptyResultIsEmptyArrayNotNull(t *testing.T) {
	repo := &fakeUserDirectoryRepo{rows: nil, total: 0}
	h := NewUserDirectoryHandler(services.NewUserDirectoryService(repo), zap.NewNop())

	rec := doDirectoryRequest(h, "tenant-1", false, "/users/directory")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"data":null`) {
		t.Errorf("body = %s, want \"data\":[] not null", rec.Body.String())
	}
}
