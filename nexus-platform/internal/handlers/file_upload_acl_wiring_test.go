package handlers

// file_upload_acl_wiring_test.go — specs/060 (DAILY-QUEUE #269) regression lock, no DB, no build
// tag. POST /:entity/:id/files attaches a file to an EXISTING record, so the route must authorize
// the "edit" grant — like PUT /:entity/:id, bulk-update and the share routes — not the default
// POST→"create" of the method-mapped recordGroup. The bug it locks: a technician whose role held
// edit (own rows) but no create was refused 403 on every photo, and granting create to unblock the
// upload also let them open new work orders. Wires the REAL RegisterAPIRoutes, so moving the route
// back onto recordGroup turns these probes red.

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/files"
	"nexus/platform/internal/services"
)

// levelPerActionChecker grants each listed action on one entity at its own access level, so a test
// can tell WHICH grant the route stage consulted from the level it stashed for the handler.
type levelPerActionChecker struct {
	entity string
	levels map[string]string // action → access level
}

func (c levelPerActionChecker) Authorize(_ context.Context, _, _, entity, action string) (bool, string, error) {
	if entity != c.entity {
		return false, "", nil
	}
	lvl, ok := c.levels[action]
	return ok, lvl, nil
}

// uploadSpy is a FileUploader that records the row scope the handler handed it and answers 201.
type uploadSpy struct{ got *services.RecordScope }

func (s uploadSpy) Upload(_ context.Context, scope services.RecordScope, in files.UploadInput) (*files.UploadResult, error) {
	*s.got = scope
	return &files.UploadResult{ID: "f1", Name: in.Filename}, nil
}

func (s uploadSpy) Open(context.Context, services.RecordScope, string, string) (io.ReadCloser, *files.File, error) {
	return nil, nil, files.ErrFileNotFound
}

func (s uploadSpy) Delete(context.Context, services.RecordScope, string, string) error {
	return files.ErrFileNotFound
}

func TestFileUploadACLWiring_RequiresEditNotCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		levels    map[string]string
		wantCode  int
		wantLevel string // access level the upload handler must receive when allowed
	}{
		{"chỉ có Sửa (của mình), không có Tạo ⇒ tải được", map[string]string{"view": "owner", "edit": "owner"}, http.StatusCreated, "owner"},
		{"chỉ có Tạo, không có Sửa ⇒ 403", map[string]string{"view": "all", "create": "all"}, http.StatusForbidden, ""},
		{"chỉ có Xem ⇒ 403", map[string]string{"view": "all"}, http.StatusForbidden, ""},
		{"Tạo toàn bộ + Sửa của mình ⇒ phạm vi là của Sửa", map[string]string{"view": "all", "create": "all", "edit": "owner"}, http.StatusCreated, "owner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got services.RecordScope
			ver := &stubVerifier{byToken: map[string]*auth.Claims{"ktv": claimsFor("t1", "role-ktv", false)}}
			r := gin.New()
			r.Use(gin.Recovery())
			RegisterAPIRoutes(r.Group("/api/v1"), ver, levelPerActionChecker{entity: "phieu_sua_chua", levels: tt.levels}, APIHandlers{
				Record: NewDynamicRecordHandler(nil, zap.NewNop()),
				File:   NewFileHandler(uploadSpy{got: &got}, 0, zap.NewNop()),
			}, zap.NewNop())

			var body bytes.Buffer
			mw := multipart.NewWriter(&body)
			part, err := mw.CreateFormFile("file", "nghiem-thu.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write([]byte("png")); err != nil {
				t.Fatal(err)
			}
			if err := mw.Close(); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/phieu_sua_chua/r1/files?field=anh", &body)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			req.Header.Set("Authorization", "Bearer ktv")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantLevel != "" && got.AccessLevel != tt.wantLevel {
				t.Errorf("upload handler received access level %q, want %q — the route stage consulted the wrong grant", got.AccessLevel, tt.wantLevel)
			}
		})
	}
}
