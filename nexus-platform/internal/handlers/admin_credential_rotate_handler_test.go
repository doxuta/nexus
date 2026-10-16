package handlers

// specs/050 S4 (tasks T077) — POST /admin/credentials/:id/rotate over httptest.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestAdminCredentialHandler_RotateReturnsGeneratedOnce: 200 with the Nexus-generated hmac key ONLY in
// that response (and a no-store header), never the typed-in secret, never an envelope; the view
// reports the grace; the error codes map to their statuses; an inert vault answers 503.
func TestAdminCredentialHandler_RotateReturnsGeneratedOnce(t *testing.T) {
	r, store := newCredentialTestRouter(t, hTestKeyring(t))
	const old = "0123456789abcdef0123456789abcdef-old"
	rec := doCredReq(r, http.MethodPost, "/admin/credentials", map[string]any{
		"name": "signed", "label": "Đối tác", "baseUrl": "https://hook.example.vn", "authType": "hmac", "secret": old,
	})
	var cr credCreateResp
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	id, _ := cr.Data.Credential["id"].(string)
	if rec.Code != http.StatusCreated || id == "" {
		t.Fatalf("fixture create = %d %s", rec.Code, rec.Body.String())
	}

	rec = doCredReq(r, http.MethodPost, "/admin/credentials/"+id+"/rotate", map[string]any{"generateSecret": true, "graceHours": 6})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out credCreateResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Data.Secret, "nxw_") || out.Data.Credential["status"] != "grace" || out.Data.Credential["previousExpiresAt"] == nil {
		t.Fatalf("rotate response = %s; want the generated key once and status grace", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q; a response carrying a key must not be cached", rec.Header().Get("Cache-Control"))
	}
	for _, leak := range []string{old, *store.rows[id].SecretEnc, *store.rows[id].PrevSecretEnc, `"keyId"`} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("rotate response leaks %q", leak)
		}
	}
	if get := doCredReq(r, http.MethodGet, "/admin/credentials/"+id, nil); strings.Contains(get.Body.String(), out.Data.Secret) {
		t.Fatalf("GET after rotate carries the generated key again: %s", get.Body.String())
	}

	// A typed-in secret is the caller's own: never echoed.
	rec = doCredReq(r, http.MethodPost, "/admin/credentials/"+id+"/rotate", map[string]any{"secret": "typed-key-0123456789abcdef0123456789-x"})
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "typed-key") || strings.Contains(rec.Body.String(), `"secret"`) {
		t.Fatalf("typed rotate (status %d) echoed the secret: %s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		name, path string
		body       any
		status     int
		code       string
		field      string
	}{
		{"nothing to change", "/admin/credentials/" + id + "/rotate", map[string]any{}, 422, "CREDENTIAL_SECRET_REQUIRED", "secret"},
		{"grace out of range", "/admin/credentials/" + id + "/rotate", map[string]any{"generateSecret": true, "graceHours": 500}, 422, "CREDENTIAL_GRACE_INVALID", "graceHours"},
		{"unknown id", "/admin/credentials/nope/rotate", map[string]any{"generateSecret": true}, 404, "CREDENTIAL_NOT_FOUND", ""},
		{"a field only PUT knows is refused, not dropped", "/admin/credentials/" + id + "/rotate", map[string]any{"generateSecret": true, "headerName": "X-Evil"}, 400, "INVALID_BODY", ""},
		{"bad json", "/admin/credentials/" + id + "/rotate", "not-an-object", 400, "INVALID_BODY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doCredReq(r, http.MethodPost, tc.path, tc.body)
			var resp struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			if rec.Code != tc.status || resp.Code != tc.code || (tc.field != "" && resp.Details["field"] != tc.field) {
				t.Fatalf("= %d %s %v; want %d %s field=%s (%s)", rec.Code, resp.Code, resp.Details, tc.status, tc.code, tc.field, rec.Body.String())
			}
		})
	}

	inert, _ := newCredentialTestRouter(t, nil)
	if rec := doCredReq(inert, http.MethodPost, "/admin/credentials/x/rotate", map[string]any{"generateSecret": true}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("inert vault rotate = %d, want 503", rec.Code)
	}
}
