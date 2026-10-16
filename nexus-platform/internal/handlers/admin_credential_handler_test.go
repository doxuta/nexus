package handlers

// httptest — AdminCredentialHandler (../specs/050-named-credentials S1b, tasks T020). Pins the
// wire contract of the vault: a Nexus-generated secret appears in the create response ONLY; no
// read ever carries a secret, an envelope or a master-key id (asserted on the raw bytes); an
// inert vault answers 503; every CREDENTIAL_* code maps to its documented status.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// hFakeCredentialStore is the handlers-side in-memory store (structurally satisfies the
// service's store interface): tenant-pinned, live rows only, unique live name.
type hFakeCredentialStore struct {
	rows   map[string]*credential.Credential
	usages map[string][]credential.Usage
}

func newHFakeCredentialStore() *hFakeCredentialStore {
	return &hFakeCredentialStore{rows: map[string]*credential.Credential{}, usages: map[string][]credential.Usage{}}
}

func (f *hFakeCredentialStore) live(tenantID, id string) (*credential.Credential, bool) {
	c, ok := f.rows[id]
	return c, ok && c.TenantID == tenantID && c.DeletedAt == nil
}

func (f *hFakeCredentialStore) Create(_ context.Context, c *credential.Credential) error {
	for _, r := range f.rows {
		if r.TenantID == c.TenantID && r.Name == c.Name && r.DeletedAt == nil {
			return credential.ErrNameTaken
		}
	}
	c.CreatedAt, c.UpdatedAt = time.Now(), time.Now()
	cp := *c
	f.rows[c.ID] = &cp
	return nil
}

func (f *hFakeCredentialStore) Get(_ context.Context, tenantID, id string) (*credential.Credential, error) {
	c, ok := f.live(tenantID, id)
	if !ok {
		return nil, credential.ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (f *hFakeCredentialStore) List(_ context.Context, tenantID string) ([]credential.Credential, error) {
	out := []credential.Credential{}
	for _, c := range f.rows {
		if c.TenantID == tenantID && c.DeletedAt == nil {
			out = append(out, *c)
		}
	}
	return out, nil
}

func (f *hFakeCredentialStore) UpdateMetadata(_ context.Context, tenantID string, c *credential.Credential) error {
	cur, ok := f.live(tenantID, c.ID)
	if !ok {
		return credential.ErrNotFound
	}
	cur.Label, cur.BaseURL, cur.HeaderName, cur.HeaderPrefix, cur.Username = c.Label, c.BaseURL, c.HeaderName, c.HeaderPrefix, c.Username
	return nil
}

func (f *hFakeCredentialStore) WithTxForUpdate(_ context.Context, tenantID, id string, fn func(*credential.Credential) error) error {
	cur, ok := f.live(tenantID, id)
	if !ok {
		return credential.ErrNotFound
	}
	cp := *cur
	if err := fn(&cp); err != nil {
		return err
	}
	cur.SecretEnc, cur.SecretHint, cur.PrevSecretEnc, cur.PrevExpiresAt = cp.SecretEnc, cp.SecretHint, cp.PrevSecretEnc, cp.PrevExpiresAt
	cur.ExtraHeadersEnc, cur.ExtraHeaderNames, cur.KeyID = cp.ExtraHeadersEnc, cp.ExtraHeaderNames, cp.KeyID
	cur.BaseURL, cur.Username, cur.RotatedAt = cp.BaseURL, cp.Username, cp.RotatedAt
	return nil
}

func (f *hFakeCredentialStore) SoftDeleteShred(_ context.Context, tenantID, id, _ string) error {
	c, ok := f.live(tenantID, id)
	if !ok {
		return credential.ErrNotFound
	}
	now := time.Now()
	c.DeletedAt, c.SecretEnc, c.ExtraHeadersEnc, c.KeyID, c.SecretHint = &now, nil, nil, nil, ""
	return nil
}

func (f *hFakeCredentialStore) Usages(_ context.Context, _, id string) ([]credential.Usage, error) {
	return f.usages[id], nil
}

func (f *hFakeCredentialStore) CountUsagesByTenant(context.Context, string) (map[string]int, error) {
	return map[string]int{}, nil
}

func hTestKeyring(t *testing.T) *crypto.Keyring {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	k, err := crypto.NewKeyring("v1", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// newCredentialTestRouter mounts claims-injection → the REAL RequireAdmin → the handler, mirroring
// the production adminGated chain. keys nil = the vault is not configured.
func newCredentialTestRouter(t *testing.T, keys *crypto.Keyring) (*gin.Engine, *hFakeCredentialStore) {
	t.Helper()
	store := newHFakeCredentialStore()
	h := NewAdminCredentialHandler(services.NewCredentialService(store, keys, false), zap.NewNop())
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("authClaims", &auth.Claims{
			Subject:   auth.Subject{UserID: "actor-1", TenantID: "tenant-1", IsAdmin: true},
			TokenType: auth.TokenTypeAccess,
		})
		c.Set("tenantID", "tenant-1")
		c.Next()
	})
	gated := r.Group("")
	gated.Use(middleware.RequireAdmin(zap.NewNop()))
	h.RegisterAdminRoutes(gated)
	return r, store
}

func doCredReq(r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
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

type credCreateResp struct {
	Data struct {
		Credential map[string]any `json:"credential"`
		Secret     string         `json:"secret"`
	} `json:"data"`
}

func TestAdminCredentialHandler_CreateReturnsSecretOnce(t *testing.T) {
	r, store := newCredentialTestRouter(t, hTestKeyring(t))
	rec := doCredReq(r, http.MethodPost, "/admin/credentials", map[string]any{
		"name": "signed", "label": "Đối tác", "baseUrl": "https://hook.example.vn", "authType": "hmac", "generateSecret": true,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var cr credCreateResp
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cr.Data.Secret, "nxw_") {
		t.Fatalf("create response secret = %q; want the generated nxw_ key", cr.Data.Secret)
	}
	id, _ := cr.Data.Credential["id"].(string)
	if cr.Data.Credential["status"] != "ready" || id == "" {
		t.Fatalf("credential view = %v", cr.Data.Credential)
	}
	if env := *store.rows[id].SecretEnc; strings.Contains(rec.Body.String(), env) {
		t.Fatal("create response carries the sealed envelope")
	}

	get := doCredReq(r, http.MethodGet, "/admin/credentials/"+id, nil)
	if get.Code != http.StatusOK || strings.Contains(get.Body.String(), cr.Data.Secret) || strings.Contains(get.Body.String(), `"secret"`) {
		t.Fatalf("GET after create (status %d) must not carry the secret again: %s", get.Code, get.Body.String())
	}

	// A typed-in secret is the caller's own: it is never echoed, not even on create.
	rec = doCredReq(r, http.MethodPost, "/admin/credentials", map[string]any{
		"name": "typed", "label": "T", "baseUrl": "https://t.example.vn", "authType": "header", "secret": "typed-token-value",
	})
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "typed-token-value") || strings.Contains(rec.Body.String(), `"secret"`) {
		t.Fatalf("typed secret echoed (status %d): %s", rec.Code, rec.Body.String())
	}
}

func TestAdminCredentialHandler_ListNeverLeaksSecret(t *testing.T) {
	r, store := newCredentialTestRouter(t, hTestKeyring(t))
	const secret, extra = "partner-signing-key-0123456789-abcdef", "Bearer extra-value-never-read-back"
	rec := doCredReq(r, http.MethodPost, "/admin/credentials", map[string]any{
		"name": "pay", "label": "Pay", "baseUrl": "https://pay.example.vn", "authType": "hmac", "secret": secret,
		"extraHeaders": map[string]string{"Authorization": extra},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var cr credCreateResp
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	id, _ := cr.Data.Credential["id"].(string)
	keyID := *store.rows[id].KeyID

	for _, path := range []string{"/admin/credentials", "/admin/credentials/" + id} {
		got := doCredReq(r, http.MethodGet, path, nil)
		if got.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, got.Code)
		}
		body := got.Body.String()
		for label, v := range map[string]string{
			"secret": secret, "extra header value": extra, "envelope": "v1:", "master key id": keyID,
			"secret field": `"secret"`, "secretEnc field": "secretEnc", "keyId field": "keyId",
		} {
			if strings.Contains(body, v) {
				t.Errorf("GET %s leaks the %s: %s", path, label, body)
			}
		}
		if !strings.Contains(body, `"extraHeaderNames":["Authorization"]`) {
			t.Errorf("GET %s lost the extra header NAMES: %s", path, body)
		}
	}
}

func TestAdminCredentialHandler_NotConfigured503(t *testing.T) {
	r, store := newCredentialTestRouter(t, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/credentials"},
		{http.MethodGet, "/admin/credentials/x"},
		{http.MethodPost, "/admin/credentials"},
		{http.MethodPut, "/admin/credentials/x"},
		{http.MethodDelete, "/admin/credentials/x"},
	} {
		rec := doCredReq(r, tc.method, tc.path, map[string]any{"name": "pay", "label": "Pay", "baseUrl": "https://pay.example.vn", "authType": "header"})
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"CREDENTIALS_NOT_CONFIGURED"`) {
			t.Errorf("%s %s = %d %s; want 503 CREDENTIALS_NOT_CONFIGURED", tc.method, tc.path, rec.Code, rec.Body.String())
		}
	}
	if len(store.rows) != 0 {
		t.Fatal("inert vault persisted a row")
	}
}

func TestAdminCredentialHandler_ErrorCodesMapped(t *testing.T) {
	r, store := newCredentialTestRouter(t, hTestKeyring(t))
	ok := doCredReq(r, http.MethodPost, "/admin/credentials", map[string]any{"name": "pay", "label": "Pay", "baseUrl": "https://pay.example.vn", "authType": "header", "secret": "t"})
	var cr credCreateResp
	_ = json.Unmarshal(ok.Body.Bytes(), &cr)
	id, _ := cr.Data.Credential["id"].(string)
	store.usages[id] = []credential.Usage{{WorkflowID: "wf-1", WorkflowName: "Báo đơn", ActionIndex: 0}}

	for _, tc := range []struct {
		name, method, path string
		body               any
		status             int
		code, field        string
	}{
		{"bad name", http.MethodPost, "/admin/credentials", map[string]any{"name": "Pay VN", "label": "x", "baseUrl": "https://a.vn", "authType": "header"}, 422, "CREDENTIAL_NAME_INVALID", "name"},
		{"duplicate name", http.MethodPost, "/admin/credentials", map[string]any{"name": "pay", "label": "x", "baseUrl": "https://a.vn", "authType": "header"}, 409, "CREDENTIAL_NAME_TAKEN", "name"},
		{"weak hmac", http.MethodPost, "/admin/credentials", map[string]any{"name": "h", "label": "x", "baseUrl": "https://a.vn", "authType": "hmac", "secret": "short"}, 422, "CREDENTIAL_SECRET_WEAK", "secret"},
		{"unknown id", http.MethodGet, "/admin/credentials/nope", nil, 404, "CREDENTIAL_NOT_FOUND", ""},
		{"origin change", http.MethodPut, "/admin/credentials/" + id, map[string]any{"baseUrl": "https://evil.vn"}, 422, "CREDENTIAL_ORIGIN_CHANGE_REQUIRES_ROTATE", "baseUrl"},
		{"immutable", http.MethodPut, "/admin/credentials/" + id, map[string]any{"authType": "basic"}, 422, "CREDENTIAL_FIELD_IMMUTABLE", "authType"},
		{"in use", http.MethodDelete, "/admin/credentials/" + id, nil, 409, "CREDENTIAL_IN_USE", ""},
		{"bad json", http.MethodPost, "/admin/credentials", "not-an-object", 400, "INVALID_BODY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doCredReq(r, tc.method, tc.path, tc.body)
			var resp struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != tc.status || resp.Code != tc.code {
				t.Fatalf("status/code = %d %s; want %d %s (%s)", rec.Code, resp.Code, tc.status, tc.code, rec.Body.String())
			}
			if tc.field != "" && resp.Details["field"] != tc.field {
				t.Fatalf("details.field = %v; want %s", resp.Details["field"], tc.field)
			}
			if tc.code == "CREDENTIAL_IN_USE" {
				if us, _ := resp.Details["usages"].([]any); len(us) != 1 {
					t.Fatalf("409 must list the rules using the key: %s", rec.Body.String())
				}
			}
		})
	}

	delete(store.usages, id)
	if rec := doCredReq(r, http.MethodDelete, "/admin/credentials/"+id, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete unused = %d, want 204", rec.Code)
	}
}
