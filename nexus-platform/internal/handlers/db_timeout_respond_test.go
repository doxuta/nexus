package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// netTimeoutErr is a net.Error whose Timeout() is true — the shape the go-sql-driver surfaces
// when the DSN readTimeout/writeTimeout fires (W5-E2).
type netTimeoutErr struct{}

func (netTimeoutErr) Error() string   { return "read tcp 127.0.0.1:3306: i/o timeout" }
func (netTimeoutErr) Timeout() bool   { return true }
func (netTimeoutErr) Temporary() bool { return true }

// callRespondError invokes the unexported respondError with a fresh gin context and returns the
// recorder so a test can assert the mapped status/code without a real service or DB.
func callRespondError(err error) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/widgets", nil)
	h := &DynamicRecordHandler{svc: nil, logger: zap.NewNop()}
	h.respondError(c, err, "List")
	return w
}

// TestRespondError_DBTimeout_503 verifies a DB statement/connection timeout maps to 503
// DB_TIMEOUT (not a generic 500), carries a Retry-After header, and is detected through the
// standard fmt.Errorf("%w") wrap chain — the W5-E2 fail-fast-not-hang contract.
func TestRespondError_DBTimeout_503(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"context deadline", fmt.Errorf("dynrepo.List: %w", context.DeadlineExceeded)},
		{"net i/o timeout", fmt.Errorf("dynrepo.List: query: %w", error(netTimeoutErr{}))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := callRespondError(tc.err)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", w.Code, w.Body.String())
			}
			if ra := w.Header().Get("Retry-After"); ra == "" {
				t.Errorf("missing Retry-After header on 503")
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["code"] != "DB_TIMEOUT" {
				t.Errorf("code = %v, want DB_TIMEOUT", body["code"])
			}
		})
	}
}

// TestRespondError_NonTimeoutStillMaps confirms the timeout branch does NOT shadow the existing
// mappings: not-found stays 404, validation stays its code's status, and a plain error stays 500.
func TestRespondError_NonTimeoutStillMaps(t *testing.T) {
	t.Run("entity not found -> 404", func(t *testing.T) {
		w := callRespondError(services.ErrEntityNotFound)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})
	t.Run("validation -> 422", func(t *testing.T) {
		w := callRespondError(&services.ValidationError{Code: "RECORD_REQUIRED_MISSING", Msg: "missing"})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", w.Code)
		}
	})
	t.Run("plain error -> 500", func(t *testing.T) {
		w := callRespondError(errors.New("boom"))
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}
