package handlers

// relation_acl_wiring_test.go — specs/060 (DAILY-QUEUE #351) regression lock, no DB, no build tag.
// Linking / unlinking an N-N record acts on an EXISTING source record, so both routes must
// authorize the "edit" grant and hand the handler the EDIT row scope — not the method-mapped
// POST→"create" / DELETE→"delete". The bug it locks: a role holding create:all + edit:own linked
// records onto a colleague's row, because the gate stashed the create scope (all) and
// RelationService.Link trusted it. Wires the REAL RegisterAPIRoutes, so moving a route back onto
// recordGroup turns these probes red.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// relCall is everything the handler handed the service: the verified identity (the service
// re-resolves the edit grant from tenantID/roleID/userID/isAdmin, so a swapped argument would
// silently authorize the wrong subject) plus the addressed record and the source row scope.
type relCall struct {
	tenantID, userID, roleID           string
	isAdmin                            bool
	sourceEntity, sourceID, rel, other string
	scope                              dynrepo.RowScope
}

// relationSpy records the call the handler made and answers success.
type relationSpy struct{ got *relCall }

func (s relationSpy) Link(_ context.Context, tenantID, userID, roleID string, isAdmin bool, sourceEntity, sourceID, relName, targetID string, _ services.TargetChecker, _ services.TargetACLChecker, _ services.RecordFetcher, sourceScope dynrepo.RowScope) (bool, error) {
	*s.got = relCall{tenantID, userID, roleID, isAdmin, sourceEntity, sourceID, relName, targetID, sourceScope}
	return true, nil
}

func (s relationSpy) Unlink(_ context.Context, tenantID, userID, roleID string, isAdmin bool, sourceEntity, sourceID, relName, targetID string, _ services.RecordFetcher, sourceScope dynrepo.RowScope) error {
	*s.got = relCall{tenantID, userID, roleID, isAdmin, sourceEntity, sourceID, relName, targetID, sourceScope}
	return nil
}

func (s relationSpy) ListLinked(context.Context, string, string, string, string, services.RelListQuery, services.RecordFetcher) (*services.ListLinkedResult, error) {
	return &services.ListLinkedResult{}, nil
}

func TestRelationACLWiring_LinkUnlinkRequireEditScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const linkPath = "/api/v1/accounts/r1/relationships/accounts_contacts"
	tests := []struct {
		name      string
		method    string
		path      string
		body      string
		levels    map[string]string
		wantCode  int
		wantLevel string // row scope level the handler must receive when allowed
		admin     bool
	}{
		{"liên kết: chỉ Sửa (của mình), không Tạo ⇒ 201", http.MethodPost, linkPath, `{"targetId":"tg1"}`,
			map[string]string{"view": "owner", "edit": "owner"}, http.StatusCreated, "owner", false},
		{"liên kết: Tạo toàn bộ + Sửa của mình ⇒ phạm vi là của Sửa", http.MethodPost, linkPath, `{"targetId":"tg1"}`,
			map[string]string{"view": "all", "create": "all", "edit": "owner"}, http.StatusCreated, "owner", false},
		{"liên kết: chỉ Tạo, không Sửa ⇒ 403", http.MethodPost, linkPath, `{"targetId":"tg1"}`,
			map[string]string{"view": "all", "create": "all"}, http.StatusForbidden, "", false},
		{"liên kết: chỉ Xem ⇒ 403", http.MethodPost, linkPath, `{"targetId":"tg1"}`,
			map[string]string{"view": "all"}, http.StatusForbidden, "", false},
		{"bỏ liên kết: chỉ Sửa (của mình), không Xoá ⇒ 204", http.MethodDelete, linkPath + "/tg1", "",
			map[string]string{"view": "owner", "edit": "owner"}, http.StatusNoContent, "owner", false},
		{"bỏ liên kết: Xoá toàn bộ + Sửa của mình ⇒ phạm vi là của Sửa", http.MethodDelete, linkPath + "/tg1", "",
			map[string]string{"view": "all", "delete": "all", "edit": "owner"}, http.StatusNoContent, "owner", false},
		{"bỏ liên kết: chỉ Xoá, không Sửa ⇒ 403", http.MethodDelete, linkPath + "/tg1", "",
			map[string]string{"view": "all", "delete": "all"}, http.StatusForbidden, "", false},
		{"quản trị: liên kết ⇒ isAdmin tới service, phạm vi toàn bộ", http.MethodPost, linkPath, `{"targetId":"tg1"}`,
			map[string]string{}, http.StatusCreated, "all", true},
		{"xem danh sách liên kết: chỉ Xem vẫn đọc được ⇒ 200", http.MethodGet, linkPath, "",
			map[string]string{"view": "owner"}, http.StatusOK, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got relCall
			ver := &stubVerifier{byToken: map[string]*auth.Claims{"u": claimsFor("t1", "role-x", tt.admin)}}
			r := gin.New()
			r.Use(gin.Recovery())
			RegisterAPIRoutes(r.Group("/api/v1"), ver, levelPerActionChecker{entity: "accounts", levels: tt.levels}, APIHandlers{
				Record:   NewDynamicRecordHandler(nil, zap.NewNop()),
				Relation: NewRelationHandler(relationSpy{got: &got}, nil, nil, zap.NewNop()),
			}, zap.NewNop())

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer u")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantLevel == "" {
				return
			}
			if string(got.scope.Level) != tt.wantLevel {
				t.Errorf("service received row scope %q, want %q — the route stage consulted the wrong grant", got.scope.Level, tt.wantLevel)
			}
			// Identity + addressing must reach the service verbatim (claimsFor: userID "u", roleID "role-x").
			want := relCall{tenantID: "t1", userID: "u", roleID: "role-x", isAdmin: tt.admin,
				sourceEntity: "accounts", sourceID: "r1", rel: "accounts_contacts", other: "tg1", scope: got.scope}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("service received %+v, want %+v", got, want)
			}
		})
	}
}
