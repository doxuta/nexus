package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRM06_LoginMissingField_422 verifies that a valid-JSON but semantically incomplete
// login body (missing password) returns 422, not 400. This is the RM-06 fix: the
// ValidationError path in Login now uses statusForCode(ve.Code) → 422.
func TestRM06_LoginMissingField_422(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	w := postJSON(r, "/api/v1/auth/login", gin.H{"tenant": "acme", "email": "x@x.com"})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 (RM-06), got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if body["code"] == nil {
		t.Error("response should have 'code' field")
	}
}

// TestRM06_LoginBadJSON_400 verifies that a truly malformed JSON body (not a JSON object)
// still returns 400 — only the semantic-missing-field case becomes 422 (RM-06).
func TestRM06_LoginBadJSON_400(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for nil body, got %d: %s", w.Code, w.Body.String())
	}
}
