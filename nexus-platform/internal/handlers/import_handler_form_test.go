package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// capturingImportSvc records the ImportInput the handler built from the form.
type capturingImportSvc struct {
	in *services.ImportInput
}

func (s *capturingImportSvc) Import(_ context.Context, in services.ImportInput) (*services.ImportResult, error) {
	s.in = &in
	return &services.ImportResult{DryRun: in.DryRun}, nil
}

func (s *capturingImportSvc) ImportIdempotent(ctx context.Context, in services.ImportInput, _ string) (*services.ImportResult, json.RawMessage, error) {
	r, err := s.Import(ctx, in)
	return r, nil, err
}

// postImportForm posts a minimal import form plus extra fields and returns the recorder.
func postImportForm(t *testing.T, svc importServiceI, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "data.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write([]byte("name\nAn\n")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	fields := map[string]string{"mapping": `{"name":"name"}`}
	for k, v := range extra {
		fields[k] = v
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("tenantID", "t1"); c.Next() })
	NewImportHandler(svc, zap.NewNop()).RegisterRoutes(r.Group("/api/v1"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/opps/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// specs/061 FR-010: the "Xem trước" step posts dryRun=true and the service is told so.
func TestImportHandler_DryRunForm(t *testing.T) {
	tests := []struct {
		name  string
		extra map[string]string
		want  bool
	}{
		{"absent", nil, false},
		{"true", map[string]string{"dryRun": "true"}, true},
		{"false", map[string]string{"dryRun": "false"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &capturingImportSvc{}
			rec := postImportForm(t, svc, tt.extra)
			if rec.Code != http.StatusOK || svc.in == nil {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			if svc.in.DryRun != tt.want {
				t.Fatalf("DryRun = %v; want %v", svc.in.DryRun, tt.want)
			}
		})
	}
}

// specs/061 FR-012: "Khớp theo" arrives as matchBy JSON; a malformed one is a 400, not a guess.
func TestImportHandler_MatchByForm(t *testing.T) {
	svc := &capturingImportSvc{}
	rec := postImportForm(t, svc, map[string]string{"matchBy": `{"project":"ma_du_an"}`})
	if rec.Code != http.StatusOK || svc.in == nil || svc.in.MatchBy["project"] != "ma_du_an" {
		t.Fatalf("status = %d, input = %+v; want matchBy forwarded", rec.Code, svc.in)
	}
	svc = &capturingImportSvc{}
	rec = postImportForm(t, svc, map[string]string{"matchBy": `{"project":`})
	if rec.Code != http.StatusBadRequest || svc.in != nil {
		t.Fatalf("status = %d, called = %v; want 400 before the service", rec.Code, svc.in != nil)
	}
}

// errImportSvc fails every import with a fixed error.
type errImportSvc struct{ err error }

func (s errImportSvc) Import(context.Context, services.ImportInput) (*services.ImportResult, error) {
	return nil, s.err
}

func (s errImportSvc) ImportIdempotent(context.Context, services.ImportInput, string) (*services.ImportResult, json.RawMessage, error) {
	return nil, nil, s.err
}

// queue #318: FIELD_FORBIDDEN is an authorization refusal — 403 like POST /:entity, not 422.
func TestImportHandler_FieldForbiddenIs403(t *testing.T) {
	tests := []struct {
		name string
		code string
		want int
	}{
		{"field forbidden", "FIELD_FORBIDDEN", http.StatusForbidden},
		{"other coded error stays 422", "RECORD_REQUIRED_LOCKED", http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := postImportForm(t, errImportSvc{&services.ValidationError{Code: tt.code, Msg: "x"}}, nil)
			if rec.Code != tt.want {
				t.Fatalf("status = %d; want %d (%s)", rec.Code, tt.want, rec.Body.String())
			}
		})
	}
}
