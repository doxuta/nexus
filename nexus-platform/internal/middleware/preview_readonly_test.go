package middleware

// preview_readonly_test.go — specs/064-preview-as-role-sample-data T013 (plan A5). One middleware,
// one code: a preview token may read, may leave (DELETE /preview-sessions/current), and nothing
// else; account routes (/auth/*) and the ⌘K assistant are closed even for GET.
// specs/063-draft-publish S7-3 adds its X-Nexus-Draft cases to THIS file.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/auth"
)

// previewRouter mounts PreviewReadOnly behind a stub auth stage that publishes claims, on the
// same /api/v1 base the production router uses, with one route per shape under test.
func previewRouter(claims *auth.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	api.Use(func(c *gin.Context) {
		c.Set(ctxKeyClaims, claims)
		c.Set(ctxKeyTenantID, claims.TenantID)
		c.Next()
	})
	api.Use(PreviewReadOnly(api.BasePath(), nil))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		api.Handle(m, "/:entity", ok)
		api.Handle(m, "/:entity/:id", ok)
	}
	api.DELETE("/preview-sessions/current", ok)
	api.GET("/auth/sessions", ok)
	api.PUT("/auth/me/password", ok)
	api.POST("/assistant/ask", ok)
	api.GET("/assistant/sessions", ok)
	return r
}

type previewCase struct {
	method, path string
	wantStatus   int
	wantCode     string
}

func runPreviewCase(t *testing.T, r *gin.Engine, tc previewCase) {
	t.Helper()
	req := httptest.NewRequest(tc.method, tc.path, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != tc.wantStatus {
		t.Fatalf("%s %s: status %d, want %d (body %s)", tc.method, tc.path, w.Code, tc.wantStatus, w.Body.String())
	}
	if tc.wantCode == "" {
		return
	}
	var body struct {
		Code    string            `json:"code"`
		Error   string            `json:"error"`
		Details map[string]string `json:"details"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if body.Code != tc.wantCode || body.Error == "" {
		t.Fatalf("%s %s: code %q error %q, want %s", tc.method, tc.path, body.Code, body.Error, tc.wantCode)
	}
	if tc.wantCode == "PREVIEW_READ_ONLY" && (body.Details["method"] != tc.method || body.Details["path"] == "") {
		t.Errorf("%s %s: details %v, want {method, path}", tc.method, tc.path, body.Details)
	}
}

func TestPreviewReadOnly_PreviewToken(t *testing.T) {
	r := previewRouter(previewClaims())
	cases := []previewCase{
		{http.MethodGet, "/api/v1/crm_accounts", http.StatusOK, ""},
		{http.MethodGet, "/api/v1/crm_accounts/abc", http.StatusOK, ""},
		{http.MethodHead, "/api/v1/crm_accounts", http.StatusOK, ""},
		{http.MethodOptions, "/api/v1/crm_accounts", http.StatusOK, ""},
		{http.MethodPost, "/api/v1/crm_accounts", http.StatusForbidden, "PREVIEW_READ_ONLY"},
		{http.MethodPut, "/api/v1/crm_accounts/abc", http.StatusForbidden, "PREVIEW_READ_ONLY"},
		{http.MethodPatch, "/api/v1/crm_accounts/abc", http.StatusForbidden, "PREVIEW_READ_ONLY"},
		{http.MethodDelete, "/api/v1/crm_accounts/abc", http.StatusForbidden, "PREVIEW_READ_ONLY"},
		// The single write a preview token may make: leaving.
		{http.MethodDelete, "/api/v1/preview-sessions/current", http.StatusOK, ""},
		// Account + assistant surfaces are closed even for reads.
		{http.MethodGet, "/api/v1/auth/sessions", http.StatusForbidden, "PREVIEW_FORBIDDEN"},
		{http.MethodPut, "/api/v1/auth/me/password", http.StatusForbidden, "PREVIEW_FORBIDDEN"},
		{http.MethodPost, "/api/v1/assistant/ask", http.StatusForbidden, "PREVIEW_FORBIDDEN"},
		{http.MethodGet, "/api/v1/assistant/sessions", http.StatusForbidden, "PREVIEW_FORBIDDEN"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) { runPreviewCase(t, r, tc) })
	}
}

// An ordinary token is never touched: writes and account routes pass through.
func TestPreviewReadOnly_OrdinaryTokenUntouched(t *testing.T) {
	r := previewRouter(sampleClaims())
	for _, tc := range []previewCase{
		{http.MethodPost, "/api/v1/crm_accounts", http.StatusOK, ""},
		{http.MethodDelete, "/api/v1/crm_accounts/abc", http.StatusOK, ""},
		{http.MethodPut, "/api/v1/auth/me/password", http.StatusOK, ""},
		{http.MethodPost, "/api/v1/assistant/ask", http.StatusOK, ""},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) { runPreviewCase(t, r, tc) })
	}
}

// A path that merely CONTAINS "auth" as an entity name is not the account surface.
func TestPreviewReadOnly_AuthPrefixIsAnchored(t *testing.T) {
	r := previewRouter(previewClaims())
	runPreviewCase(t, r, previewCase{http.MethodGet, "/api/v1/auth_logs", http.StatusOK, ""})
}

// ---- specs/063-draft-publish S7-3 (T068): the X-Nexus-Draft branch of the SAME middleware ----

func builderClaims() *auth.Claims {
	c := sampleClaims()
	c.IsPlatformOperator = true
	return c
}

func draftCall(r *gin.Engine, method, path string, header bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if header {
		req.Header.Set(DraftHeader, "1")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func draftBranchRouter(claims *auth.Claims) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/v1")
	api.Use(func(c *gin.Context) {
		c.Set(ctxKeyClaims, claims)
		c.Set(ctxKeyTenantID, claims.TenantID)
		c.Next()
	})
	api.Use(PreviewReadOnly(api.BasePath(), nil))
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		api.Handle(m, "/:entity", ok)
		api.Handle(m, "/:entity/:id", ok)
	}
	api.POST("/:entity/import", ok)
	api.POST("/:entity/bulk", ok)
	api.POST("/:entity/:id/files", ok)
	api.POST("/admin/users", ok)
	api.PUT("/admin/roles/:id", ok)
	api.PUT("/admin/workflows/:id", ok)
	api.POST("/auth/logout", ok)
	api.POST("/auth/refresh", ok)
	return r
}

func TestPreviewReadOnly_DraftHeaderBranch(t *testing.T) {
	t.Run("builder + header: every write is refused, one code one status", func(t *testing.T) {
		r := draftBranchRouter(builderClaims())
		for _, tc := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/crm_accounts"},
			{http.MethodPut, "/api/v1/crm_accounts/abc"},
			{http.MethodPatch, "/api/v1/crm_accounts/abc"},
			{http.MethodDelete, "/api/v1/crm_accounts/abc"},
			{http.MethodPost, "/api/v1/crm_accounts/import"},
			{http.MethodPost, "/api/v1/crm_accounts/bulk"},
			{http.MethodPost, "/api/v1/crm_accounts/abc/files"},
			// the preview tab never writes for real, the admin area included
			{http.MethodPost, "/api/v1/admin/users"},
			{http.MethodPut, "/api/v1/admin/roles/r1"},
			{http.MethodPut, "/api/v1/admin/workflows/w1"},
		} {
			w := draftCall(r, tc.method, tc.path, true)
			if w.Code != http.StatusForbidden {
				t.Errorf("%s %s: status %d, want 403", tc.method, tc.path, w.Code)
				continue
			}
			var body struct {
				Code    string            `json:"code"`
				Error   string            `json:"error"`
				Details map[string]string `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != "PREVIEW_READ_ONLY" || body.Error == "" ||
				body.Details["method"] != tc.method || body.Details["path"] == "" {
				t.Errorf("%s %s: body %s, want PREVIEW_READ_ONLY {error, details.method, details.path}", tc.method, tc.path, w.Body.String())
			}
		}
	})

	t.Run("builder + header: reads and /auth/* pass", func(t *testing.T) {
		r := draftBranchRouter(builderClaims())
		for _, tc := range []struct{ method, path string }{
			{http.MethodGet, "/api/v1/crm_accounts"},
			{http.MethodHead, "/api/v1/crm_accounts"},
			{http.MethodOptions, "/api/v1/crm_accounts"},
			{http.MethodPost, "/api/v1/auth/logout"},
			{http.MethodPost, "/api/v1/auth/refresh"},
		} {
			if w := draftCall(r, tc.method, tc.path, true); w.Code != http.StatusOK {
				t.Errorf("%s %s: status %d, want 200", tc.method, tc.path, w.Code)
			}
		}
	})

	t.Run("no header: the builder writes as before (Studio)", func(t *testing.T) {
		r := draftBranchRouter(builderClaims())
		for _, p := range []string{"/api/v1/crm_accounts", "/api/v1/admin/users"} {
			if w := draftCall(r, http.MethodPost, p, false); w.Code != http.StatusOK {
				t.Errorf("POST %s without the header: status %d, want 200", p, w.Code)
			}
		}
	})

	t.Run("not the builder: the header is ignored silently", func(t *testing.T) {
		legacy := sampleClaims() // IsAdmin but not IsPlatformOperator = legacy customer admin
		staff := sampleClaims()
		staff.IsAdmin = false
		apiKey := builderClaims()
		apiKey.TokenType = auth.TokenTypeAPIKey
		for name, claims := range map[string]*auth.Claims{"legacy customer admin": legacy, "staff": staff, "api key": apiKey} {
			r := draftBranchRouter(claims)
			for _, p := range []string{"/api/v1/crm_accounts", "/api/v1/admin/users"} {
				if w := draftCall(r, http.MethodPost, p, true); w.Code != http.StatusOK {
					t.Errorf("%s POST %s with the header: status %d, want 200 (header must be ignored)", name, p, w.Code)
				}
			}
		}
	})

	t.Run("the match is on the route, not the raw url", func(t *testing.T) {
		r := draftBranchRouter(builderClaims())
		// "/auth/" in a query string, or an entity that merely starts with auth, must not open the gate
		if w := draftCall(r, http.MethodPost, "/api/v1/auth_logs?x=/auth/", true); w.Code != http.StatusForbidden {
			t.Errorf("POST /auth_logs?x=/auth/: status %d, want 403", w.Code)
		}
	})
}
