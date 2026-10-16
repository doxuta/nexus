package handlers

// massop_acl_wiring_test.go — ENG-MASSOP ACL-action regression lock (no DB, no build tag),
// mirroring bulk_acl_wiring_test.go: both mass-op routes are POST but must NOT ride the default
// POST→"create" mapping — bulk-update requires the "edit" grant (like PUT /:entity/:id) and
// bulk-delete requires the "delete" grant (like DELETE /:entity/:id). These tests wire the REAL
// RegisterAPIRoutes, so if either route is ever moved onto the method-mapped recordGroup (where
// POST would authorize "create"), the create-granted probes below flip from 403 and go red.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// buildMassOpACLEngine wires the REAL RegisterAPIRoutes with the given checker and a nil
// RecordService (only the middleware chain in front of the handler is exercised; gin.Recovery
// turns a legitimately-passed request into a 500 instead of crashing the process).
func buildMassOpACLEngine(checker ACLChecker) *gin.Engine {
	gin.SetMode(gin.TestMode)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{
		"member": claimsFor("t1", "role-1", false),
	}}
	r := gin.New()
	r.Use(gin.Recovery())
	api := r.Group("/api/v1")
	RegisterAPIRoutes(api, ver, checker, APIHandlers{
		Record: NewDynamicRecordHandler(nil, zap.NewNop()),
	}, zap.NewNop())
	return r
}

func doMassOpReq(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer member")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The fixed ACL action per mass-op route: a role holding ONLY the default-POST "create" grant is
// refused on both routes, while the semantically-matching grant (edit / delete) clears the stage.
func TestMassOpACLWiring_FixedActions(t *testing.T) {
	tests := []struct {
		name       string
		path, body string
		grant      string // action the checker allows on contacts
		wantDenied bool
	}{
		{"bulk-update từ chối grant create", "/api/v1/contacts/bulk-update", `{"ids":["x"],"values":{"name":"A"}}`, "create", true},
		{"bulk-update nhận grant edit", "/api/v1/contacts/bulk-update", `{"ids":["x"],"values":{"name":"A"}}`, "edit", false},
		{"bulk-delete từ chối grant create", "/api/v1/contacts/bulk-delete", `{"ids":["x"]}`, "create", true},
		{"bulk-delete nhận grant delete", "/api/v1/contacts/bulk-delete", `{"ids":["x"]}`, "delete", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &aclWiringChecker{allowTenant: "t1", allowRole: "role-1", allowEntity: "contacts", allowAction: tt.grant}
			r := buildMassOpACLEngine(checker)
			rec := doMassOpReq(r, tt.path, tt.body)
			if tt.wantDenied {
				if rec.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
				}
				return
			}
			// Granted: the ACL stage must NOT refuse — the request reaches the nil-service
			// handler (recovered ~500). Any 401/403 means the chain rejected it.
			if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
				t.Fatalf("granted request refused by middleware chain: status = %d (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}
