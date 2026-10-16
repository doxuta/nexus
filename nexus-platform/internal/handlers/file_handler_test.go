package handlers

// specs/048 Nợ 16 — the file routes answer a refused record write with the same envelope as the
// record routes: a before_save veto is 422 RECORD_HOOK_VETO, and "rules unreadable" is 503
// RECORD_HOOKS_UNAVAILABLE + Retry-After (the service has already undone the file change, so the
// retry is safe) — not a 500 FILE_OP_FAILED that tells the client nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"nexus/platform/internal/files"
	"nexus/platform/internal/services"
)

// fileErrStub answers every file operation with one error.
type fileErrStub struct{ err error }

func (f fileErrStub) Upload(context.Context, services.RecordScope, files.UploadInput) (*files.UploadResult, error) {
	return nil, f.err
}

func (f fileErrStub) Open(context.Context, services.RecordScope, string, string) (io.ReadCloser, *files.File, error) {
	return nil, nil, f.err
}

func (f fileErrStub) Delete(context.Context, services.RecordScope, string, string) error {
	return f.err
}

func TestFileHandler_Delete_RefusedRecordWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	veto := &services.ValidationError{Code: "RECORD_HOOK_VETO", Msg: "Hồ sơ đã khoá, không sửa được",
		Details: map[string]any{"violations": []map[string]any{{"message": "Hồ sơ đã khoá, không sửa được"}}}}
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		retryAfter string
	}{
		{"before_save veto", fmt.Errorf("files.Delete: %w", veto), http.StatusUnprocessableEntity, "RECORD_HOOK_VETO", ""},
		{"before rules unreadable", fmt.Errorf("files.Delete: %w: %w", services.ErrRecordHooksUnavailable, errors.New("sys_workflows: connection refused")),
			http.StatusServiceUnavailable, "RECORD_HOOKS_UNAVAILABLE", "1"},
		{"anything else", errors.New("disk on fire"), http.StatusInternalServerError, "FILE_OP_FAILED", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) { c.Set("tenantID", "t1") })
			NewFileHandler(fileErrStub{err: tt.err}, 0, nil).RegisterFileRoutes(r.Group("/api/v1"))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/files/f1", nil))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var env struct{ Code, Error string }
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.Code != tt.wantCode || env.Error == "" {
				t.Fatalf("envelope = %+v, want code %s", env, tt.wantCode)
			}
			if got := rec.Header().Get("Retry-After"); got != tt.retryAfter {
				t.Errorf("Retry-After = %q, want %q", got, tt.retryAfter)
			}
			for _, leak := range []string{"connection refused", "disk on fire"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("infrastructure error leaked to the client: %s", rec.Body.String())
				}
			}
		})
	}
}
