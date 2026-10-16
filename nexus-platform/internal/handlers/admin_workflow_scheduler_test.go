package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// #291: Studio must be able to ask "is the scheduler on?" before a builder saves a "scheduled"
// rule that would never fire. The handler only reports what main wired; it never guesses.
func TestAdminWorkflowHandler_SchedulerStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name         string
		h            *AdminWorkflowHandler
		wantEnabled  bool
		wantInterval float64
	}{
		{"on", NewAdminWorkflowHandler(nil, nil).WithSchedulerStatus(60 * time.Second), true, 60},
		{"off", NewAdminWorkflowHandler(nil, nil).WithSchedulerStatus(0), false, 0},
		{"unwired is off", NewAdminWorkflowHandler(nil, nil), false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			tt.h.RegisterRoutes(r.Group("/"))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/workflows/scheduler", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Data["enabled"] != tt.wantEnabled || body.Data["intervalSeconds"] != tt.wantInterval {
				t.Fatalf("data = %v; want enabled=%v intervalSeconds=%v", body.Data, tt.wantEnabled, tt.wantInterval)
			}
		})
	}
}
