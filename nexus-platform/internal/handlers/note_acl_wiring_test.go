package handlers

// note_acl_wiring_test.go — specs/060 (DAILY-QUEUE #349) regression lock, no DB, no build tag.
// POST /:entity/:id/notes adds a note to an EXISTING record, so the route floor is "view" ("whoever
// can read the record can comment on it", Founder 2026-10-09) — NOT the default POST→"create" of the
// method-mapped recordGroup. The bug it locks: a technician whose role held edit/view (own rows) but
// no create got 403 on every note on their own work order. notes.Service.Create re-resolves view +
// row scope itself (ResolveNoteTarget), so the floor never widens which rows are reachable. Wires the
// REAL RegisterAPIRoutes, so moving the route back onto recordGroup turns these probes red.

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notes"
	"nexus/platform/internal/services"
)

// noteSpy is a NoteService that records the row scope the handler handed Create and answers 201.
type noteSpy struct{ got *services.RecordScope }

func (s noteSpy) List(context.Context, services.RecordScope, string, string, string, int, int) ([]notes.Note, int, error) {
	return nil, 0, nil
}

func (s noteSpy) Create(_ context.Context, scope services.RecordScope, _, _, _, body string) (*notes.Note, error) {
	*s.got = scope
	return &notes.Note{ID: "n1", Body: body}, nil
}

func (s noteSpy) Delete(context.Context, services.RecordScope, string, string, string, string) error {
	return nil
}

func TestNoteACLWiring_CreateRequiresViewNotCreate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		levels    map[string]string
		wantCode  int
		wantLevel string // access level the Create handler must receive when allowed
	}{
		{"Xem + Sửa (của mình), không có Tạo ⇒ ghi chú được", map[string]string{"view": "owner", "edit": "owner"}, http.StatusCreated, "owner"},
		{"chỉ Xem (của mình) ⇒ ghi chú được", map[string]string{"view": "owner"}, http.StatusCreated, "owner"},
		{"chỉ có Tạo, không có Xem ⇒ 403", map[string]string{"create": "all"}, http.StatusForbidden, ""},
		{"không có grant nào ⇒ 403", map[string]string{}, http.StatusForbidden, ""},
		{"Tạo toàn bộ + Xem của mình ⇒ phạm vi là của Xem", map[string]string{"view": "owner", "create": "all"}, http.StatusCreated, "owner"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got services.RecordScope
			ver := &stubVerifier{byToken: map[string]*auth.Claims{"ktv": claimsFor("t1", "role-ktv", false)}}
			r := gin.New()
			r.Use(gin.Recovery())
			RegisterAPIRoutes(r.Group("/api/v1"), ver, levelPerActionChecker{entity: "phieu_sua_chua", levels: tt.levels}, APIHandlers{
				Record: NewDynamicRecordHandler(nil, zap.NewNop()),
				Note:   NewNoteHandler(noteSpy{got: &got}, zap.NewNop()),
			}, zap.NewNop())

			req := httptest.NewRequest(http.MethodPost, "/api/v1/phieu_sua_chua/r1/notes", bytes.NewBufferString(`{"body":"đã kiểm tra"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer ktv")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantLevel != "" && got.AccessLevel != tt.wantLevel {
				t.Errorf("note handler received access level %q, want %q — the route stage consulted the wrong grant", got.AccessLevel, tt.wantLevel)
			}
		})
	}
}
