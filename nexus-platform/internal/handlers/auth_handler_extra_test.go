package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// doLogin logs the seeded user in and returns the issued (access, refresh) token pair.
func doLogin(t *testing.T, r *gin.Engine, password string) (string, string) {
	t.Helper()
	w := postJSON(r, "/api/v1/auth/login", gin.H{
		"tenant": "acme", "email": "alice@acme.test", "password": password,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("login: got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return resp.Data.AccessToken, resp.Data.RefreshToken
}

func getWithAuth(r *gin.Engine, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRefreshHandler_Success(t *testing.T) {
	r, password, _, _ := newAuthTestRouter(t)
	_, refresh := doLogin(t, r, password)

	w := postJSON(r, "/api/v1/auth/refresh", gin.H{"refreshToken": refresh})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.AccessToken == "" || resp.Data.RefreshToken == "" {
		t.Errorf("expected new token pair, got %+v", resp.Data)
	}
	if resp.Data.RefreshToken == refresh {
		t.Error("refresh token was not rotated")
	}
}

func TestRefreshHandler_RevokedTokenIs401(t *testing.T) {
	r, password, _, _ := newAuthTestRouter(t)
	_, refresh := doLogin(t, r, password)

	// First refresh rotates (and revokes) the original token.
	if w := postJSON(r, "/api/v1/auth/refresh", gin.H{"refreshToken": refresh}); w.Code != http.StatusOK {
		t.Fatalf("first refresh: got %d (%s)", w.Code, w.Body.String())
	}
	// Replaying the original token must now be 401.
	w := postJSON(r, "/api/v1/auth/refresh", gin.H{"refreshToken": refresh})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on replayed refresh, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestRefreshHandler_GarbageIs401(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	w := postJSON(r, "/api/v1/auth/refresh", gin.H{"refreshToken": "not-a-jwt"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestRefreshHandler_BadBodyIs400(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/refresh", bytes.NewReader([]byte("{bad")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestLogoutHandler_RevokesAndIs204(t *testing.T) {
	r, password, _, refresh := newAuthTestRouter(t)
	_ = refresh
	_, refreshTok := doLogin(t, r, password)

	w := postJSON(r, "/api/v1/auth/logout", gin.H{"refreshToken": refreshTok})
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", w.Code, w.Body.String())
	}
	// The logged-out token can no longer be refreshed.
	if w2 := postJSON(r, "/api/v1/auth/refresh", gin.H{"refreshToken": refreshTok}); w2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 refreshing a logged-out token, got %d (%s)", w2.Code, w2.Body.String())
	}
}

func TestLogoutHandler_IdempotentOnGarbage(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	// Logout of an unknown/garbage token is a no-op success (no probing signal).
	w := postJSON(r, "/api/v1/auth/logout", gin.H{"refreshToken": "garbage"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for garbage logout, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestMeHandler_Success(t *testing.T) {
	r, password, _, _ := newAuthTestRouter(t)
	access, _ := doLogin(t, r, password)

	w := getWithAuth(r, "/api/v1/auth/me", access)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.ID != "u-1" || resp.Data.Email != "alice@acme.test" {
		t.Errorf("unexpected user: %+v", resp.Data)
	}
	// Never leak the password hash.
	if bytes.Contains(w.Body.Bytes(), []byte("passwordHash")) || bytes.Contains(w.Body.Bytes(), []byte("$2a$")) {
		t.Error("me response leaked the password hash")
	}
}

func TestMeHandler_NoToken_401(t *testing.T) {
	r, _, _, _ := newAuthTestRouter(t)
	w := getWithAuth(r, "/api/v1/auth/me", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestMeHandler_RefreshTokenRejected_401(t *testing.T) {
	r, password, _, _ := newAuthTestRouter(t)
	_, refresh := doLogin(t, r, password)
	// A refresh token presented as a Bearer access token must be rejected (type pinning).
	w := getWithAuth(r, "/api/v1/auth/me", refresh)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for refresh-as-access, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestMeHandler_MalformedAuthHeader_401(t *testing.T) {
	r, password, _, _ := newAuthTestRouter(t)
	access, _ := doLogin(t, r, password)

	cases := []string{access, "Token " + access, "Bearer", "Bearer "}
	for _, hdr := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.Header.Set("Authorization", hdr)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("header %q: expected 401, got %d (%s)", hdr, w.Code, w.Body.String())
		}
	}
	// Sanity: a correct lowercase-scheme header is accepted (RFC 7235 case-insensitive).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.Header.Set("Authorization", "bearer "+access)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("lowercase bearer scheme: expected 200, got %d (%s)", w.Code, w.Body.String())
	}
}
