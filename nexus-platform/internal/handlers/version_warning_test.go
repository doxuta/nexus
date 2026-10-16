package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/metaversion"
)

// FR-033: a direct version entry that failed to write must not fail the request; the 2xx body says so.
func TestVersionWarningResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		warn       bool
		call       func(c *gin.Context)
		wantStatus int
		wantWarn   bool
	}{
		{"json, no warning", false, func(c *gin.Context) { jsonWV(c, http.StatusOK, gin.H{"data": 1}) }, 200, false},
		{"json, warning", true, func(c *gin.Context) { jsonWV(c, http.StatusCreated, gin.H{"data": 1}) }, 201, true},
		{"delete, no warning stays 204", false, noContentWV, 204, false},
		{"delete, warning becomes 200 with the notice", true, noContentWV, 200, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			ctx := metaversion.WithWarnings(httptest.NewRequest(http.MethodGet, "/", nil).Context())
			if tc.warn {
				metaversion.AddWarning(ctx, "boom")
			}
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
			tc.call(c)
			c.Writer.WriteHeaderNow()
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			var body map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &body) // empty 204 body leaves body nil
			if _, got := body["versionWarning"]; got != tc.wantWarn {
				t.Errorf("versionWarning present = %v, want %v (body %s)", got, tc.wantWarn, w.Body.String())
			}
		})
	}
}
