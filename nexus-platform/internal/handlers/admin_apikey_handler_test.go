package handlers

// httptest — AdminAPIKeyHandler (APIKEY-1, docs/api-conventions.md §3e). Pins the wire contract:
//   * every route sits behind RequireAdmin (non-admin → 403, nothing executed);
//   * the raw secret appears in the POST 201 response ONLY — the list response must contain
//     neither the secret nor the hash (asserted on the raw JSON bytes, not the parsed shape);
//   * DELETE revokes (204) and the revoked key shows revokedAt in the next list;
//   * validation codes map to the right statuses (422 name/role, 404 unknown id).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// hFakeAPIKeyRepo is the handlers-side in-memory repo (structurally satisfies the service's
// apiKeyRepo interface).
type hFakeAPIKeyRepo struct {
	rows map[string]*auth.APIKey
}

func newHFakeAPIKeyRepo() *hFakeAPIKeyRepo { return &hFakeAPIKeyRepo{rows: map[string]*auth.APIKey{}} }

func (f *hFakeAPIKeyRepo) Create(_ context.Context, k *auth.APIKey) error {
	cp := *k
	f.rows[k.ID] = &cp
	return nil
}

func (f *hFakeAPIKeyRepo) FindByHash(_ context.Context, keyHash string) (*auth.APIKey, error) {
	for _, k := range f.rows {
		if k.KeyHash == keyHash {
			cp := *k
			return &cp, nil
		}
	}
	return nil, auth.ErrAPIKeyInvalid
}

func (f *hFakeAPIKeyRepo) List(_ context.Context, tenantID string) ([]auth.APIKey, error) {
	out := make([]auth.APIKey, 0)
	for _, k := range f.rows {
		if k.TenantID == tenantID {
			out = append(out, *k)
		}
	}
	return out, nil
}

func (f *hFakeAPIKeyRepo) GetByID(_ context.Context, tenantID, id string) (*auth.APIKey, error) {
	k, ok := f.rows[id]
	if !ok || k.TenantID != tenantID {
		return nil, auth.ErrAPIKeyNotFound
	}
	cp := *k
	return &cp, nil
}

func (f *hFakeAPIKeyRepo) Revoke(_ context.Context, tenantID, id string, at time.Time) (bool, error) {
	k, ok := f.rows[id]
	if !ok || k.TenantID != tenantID || k.RevokedAt != nil {
		return false, nil
	}
	k.RevokedAt = &at
	return true, nil
}

func (f *hFakeAPIKeyRepo) TouchLastUsed(_ context.Context, _, id string, at time.Time) error {
	if k, ok := f.rows[id]; ok {
		k.LastUsedAt = &at
	}
	return nil
}

// hFakeRoleValidator accepts exactly one (tenant, role) pair.
type hFakeRoleValidator struct{ tenant, role string }

func (f hFakeRoleValidator) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if tenantID == f.tenant && id == f.role {
		return &auth.Role{ID: id, TenantID: tenantID}, nil
	}
	return nil, auth.ErrRoleNotFound
}

// newAPIKeyTestRouter mounts claims-injection (simulating AuthMiddleware output) → the REAL
// middleware.RequireAdmin → the handler, mirroring the production adminGated chain.
func newAPIKeyTestRouter(isAdmin bool) (*gin.Engine, *hFakeAPIKeyRepo) {
	repo := newHFakeAPIKeyRepo()
	svc := services.NewAPIKeyService(repo, hFakeRoleValidator{tenant: "tenant-1", role: "role-1"})
	h := NewAdminAPIKeyHandler(svc, zap.NewNop())

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "actor-1", TenantID: "tenant-1", IsAdmin: isAdmin},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "tenant-1")
		c.Next()
	})
	gated := r.Group("")
	gated.Use(middleware.RequireAdmin(zap.NewNop()))
	h.RegisterRoutes(gated)
	return r, repo
}

func doAPIKeyReq(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
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

// 🔒 The whole surface is admin-gated: a non-admin gets 403 on all three routes and nothing runs.
func TestAdminAPIKeyHandler_AdminGate(t *testing.T) {
	r, repo := newAPIKeyTestRouter(false)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/admin/api-keys"},
		{http.MethodGet, "/admin/api-keys"},
		{http.MethodDelete, "/admin/api-keys/some-id"},
	} {
		rec := doAPIKeyReq(r, tc.method, tc.path, map[string]any{"name": "x", "roleId": "role-1"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", tc.method, tc.path, rec.Code)
		}
	}
	if len(repo.rows) != 0 {
		t.Errorf("non-admin request persisted %d rows; want 0", len(repo.rows))
	}
}

// The raw secret appears ONLY in the create response; the stored row keeps just the hash.
func TestAdminAPIKeyHandler_CreateReturnsSecretOnce(t *testing.T) {
	r, repo := newAPIKeyTestRouter(true)
	rec := doAPIKeyReq(r, http.MethodPost, "/admin/api-keys", map[string]any{"name": "CI importer", "roleId": "role-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			APIKey struct {
				ID        string `json:"id"`
				Name      string `json:"name"`
				KeyPrefix string `json:"keyPrefix"`
				RoleID    string `json:"roleId"`
			} `json:"apiKey"`
			Secret string `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(resp.Data.Secret, "nxk_") {
		t.Errorf("secret: got %q, want nxk_ prefix", resp.Data.Secret)
	}
	if resp.Data.APIKey.KeyPrefix == "" || strings.Contains(resp.Data.Secret, resp.Data.APIKey.KeyPrefix) == false {
		t.Errorf("keyPrefix %q must be a slice of the secret", resp.Data.APIKey.KeyPrefix)
	}
	// The create response must not carry the HASH either (secret once, hash never).
	if strings.Contains(rec.Body.String(), repo.rows[resp.Data.APIKey.ID].KeyHash) {
		t.Error("create response leaked the key hash")
	}
	if repo.rows[resp.Data.APIKey.ID].KeyHash == resp.Data.Secret {
		t.Error("raw secret stored instead of hash")
	}
}

// The list response never contains the secret or the hash — asserted on the raw bytes.
func TestAdminAPIKeyHandler_ListNeverLeaksSecret(t *testing.T) {
	r, repo := newAPIKeyTestRouter(true)
	created := doAPIKeyReq(r, http.MethodPost, "/admin/api-keys", map[string]any{"name": "leak probe", "roleId": "role-1"})
	var cr struct {
		Data struct {
			Secret string `json:"secret"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	rec := doAPIKeyReq(r, http.MethodGet, "/admin/api-keys", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, cr.Data.Secret) {
		t.Error("list leaked the raw secret")
	}
	for _, k := range repo.rows {
		if strings.Contains(body, k.KeyHash) {
			t.Error("list leaked the key hash")
		}
	}
	if !strings.Contains(body, "leak probe") || !strings.Contains(body, "keyPrefix") {
		t.Error("list must still carry the metadata (name, keyPrefix)")
	}
}

// DELETE revokes (204, idempotent) and the row shows revokedAt afterwards; unknown id → 404.
func TestAdminAPIKeyHandler_Revoke(t *testing.T) {
	r, repo := newAPIKeyTestRouter(true)
	created := doAPIKeyReq(r, http.MethodPost, "/admin/api-keys", map[string]any{"name": "to revoke", "roleId": "role-1"})
	var cr struct {
		Data struct {
			APIKey struct {
				ID string `json:"id"`
			} `json:"apiKey"`
		} `json:"data"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	if rec := doAPIKeyReq(r, http.MethodDelete, "/admin/api-keys/"+cr.Data.APIKey.ID, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if repo.rows[cr.Data.APIKey.ID].RevokedAt == nil {
		t.Error("revoked_at not set")
	}
	// Idempotent second revoke.
	if rec := doAPIKeyReq(r, http.MethodDelete, "/admin/api-keys/"+cr.Data.APIKey.ID, nil); rec.Code != http.StatusNoContent {
		t.Errorf("second revoke status = %d, want 204", rec.Code)
	}
	// Unknown id → 404 APIKEY_NOT_FOUND.
	rec := doAPIKeyReq(r, http.MethodDelete, "/admin/api-keys/nope", nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "APIKEY_NOT_FOUND") {
		t.Errorf("unknown id: status = %d body=%s, want 404 APIKEY_NOT_FOUND", rec.Code, rec.Body.String())
	}
	// List shows the revoked row (kept for audit).
	list := doAPIKeyReq(r, http.MethodGet, "/admin/api-keys", nil)
	if !strings.Contains(list.Body.String(), "revokedAt") {
		t.Error("list must show revokedAt on the revoked row")
	}
}

// Validation mapping: empty name → 422 APIKEY_NAME_REQUIRED; bad role → 422 APIKEY_ROLE_INVALID.
func TestAdminAPIKeyHandler_ValidationCodes(t *testing.T) {
	r, _ := newAPIKeyTestRouter(true)
	tests := []struct {
		name     string
		body     map[string]any
		wantCode string
	}{
		{"empty name", map[string]any{"name": "  ", "roleId": "role-1"}, "APIKEY_NAME_REQUIRED"},
		{"missing role", map[string]any{"name": "x"}, "APIKEY_ROLE_INVALID"},
		{"foreign role", map[string]any{"name": "x", "roleId": "role-of-other-tenant"}, "APIKEY_ROLE_INVALID"},
		{"past expiry", map[string]any{"name": "x", "roleId": "role-1", "expiresAt": "2020-01-01T00:00:00Z"}, "APIKEY_EXPIRY_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doAPIKeyReq(r, http.MethodPost, "/admin/api-keys", tt.body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tt.wantCode) {
				t.Errorf("status=%d body=%s, want 422 %s", rec.Code, rec.Body.String(), tt.wantCode)
			}
		})
	}
}
