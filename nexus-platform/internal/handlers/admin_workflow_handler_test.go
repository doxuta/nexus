package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// TestWorkflowRespondServiceError_ForwardsDetails asserts the workflow error envelope carries
// details.field for a field-level WF-01 reject (RECORD_UNKNOWN_FIELD / RECORD_*_READONLY built via
// invalidField), per api-conventions §9.1 — the FE reads details.field instead of regex-parsing the
// message. A non-field ValidationError (invalid()) emits an empty details object, not a missing key.
func TestWorkflowRespondServiceError_ForwardsDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &AdminWorkflowHandler{logger: zap.NewNop()}

	tests := []struct {
		name       string
		err        *services.ValidationError
		wantStatus int
		wantField  any // string when a field is expected, nil when details must be an empty object
	}{
		{
			name: "field-level reject carries details.field",
			err: &services.ValidationError{
				Code:    "RECORD_UNKNOWN_FIELD",
				Msg:     `field "status" không tồn tại trong metadata`,
				Field:   "status",
				Details: map[string]any{"field": "status"},
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "status",
		},
		{
			name: "readonly target carries details.field",
			err: &services.ValidationError{
				Code:    "RECORD_AUTONUMBER_READONLY",
				Msg:     `field "code" là computed/assigned field`,
				Field:   "code",
				Details: map[string]any{"field": "code"},
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  "code",
		},
		{
			// specs/050 S2a: a builder save that would drop a key set by API is a state
			// conflict with the stored rule (409), naming the action it would have stripped.
			name: "legacy secret pending is a 409 naming the action",
			err: &services.ValidationError{
				Code:    "WEBHOOK_LEGACY_SECRET_PENDING",
				Msg:     "rule này còn khoá ký/header đặt bằng API",
				Field:   "actions[0].url",
				Details: map[string]any{"field": "actions[0].url"},
			},
			wantStatus: http.StatusConflict,
			wantField:  "actions[0].url",
		},
		{
			name:       "non-field error emits empty details object",
			err:        &services.ValidationError{Code: "WORKFLOW_ACTIONS_REQUIRED", Msg: "cần ít nhất 1 action", Details: map[string]any{}},
			wantStatus: http.StatusUnprocessableEntity,
			wantField:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			h.respondServiceError(c, tt.err, "Create", "t-1")

			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			var env struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v (body %s)", err, w.Body.String())
			}
			if env.Code != tt.err.Code {
				t.Fatalf("code = %q, want %q", env.Code, tt.err.Code)
			}
			if env.Details == nil {
				t.Fatalf("details key missing from envelope: %s", w.Body.String())
			}
			if tt.wantField == nil {
				if len(env.Details) != 0 {
					t.Fatalf("expected empty details object, got %+v", env.Details)
				}
				return
			}
			if env.Details["field"] != tt.wantField {
				t.Fatalf("details.field = %v, want %v", env.Details["field"], tt.wantField)
			}
		})
	}
}
