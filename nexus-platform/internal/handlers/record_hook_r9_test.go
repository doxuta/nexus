package handlers

// specs/048 plan R9 (queue #345) — a before_save rule may fill a field the saving role may not even
// SEE. The service-level test proves the write; this proves the other half through the production
// route chain: the response of POST/PUT /:entity must not carry that value back. The plan leans on
// Create/Update answering with an id only, so any future "return the saved record" change must
// filter through the field ACL first — this test goes red the moment it does not.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// r9Store records what the store was handed and serves one stored contact to Update's reads.
type r9Store struct {
	hiddenMetaStore
	mu      sync.Mutex
	written []map[string]any
}

func (s *r9Store) InsertRecord(_ context.Context, _, _, _ string, data map[string]any, _ []dynrepo.UniqueFieldSpec) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, data)
	return "new-1", nil
}
func (s *r9Store) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: id, TenantID: tenantID, Data: map[string]any{"name": "Acme"}}, nil
}
func (s *r9Store) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, data)
	return nil
}

// r9Checker lets role-1 create and edit contacts.
type r9Checker struct{}

func (r9Checker) Authorize(_ context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	return tenantID == "t1" && roleID == "role-1" && entity == "contacts" && (action == "create" || action == "edit"), "all", nil
}

const r9Secret = 7351.0 // a value only the rule knows; the saving user never sends it

func TestRecordWrite_ResponseDoesNotCarryRuleFilledHiddenField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "salary", FieldType: "number"},
		},
	}
	rule := workflow.Workflow{ID: "rule-fill", TenantID: "t1", EntityName: "contacts", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Actions: []workflow.Action{{Type: workflow.ActionFieldUpdate, Field: "salary", Value: r9Secret}}}
	store := &r9Store{}
	// role-1 has "salary" hidden (and the guard never sees it in the payload: the user sends only name).
	facl := &hiddenMetaACL{byRole: map[string]map[string]struct{}{"role-1": {"salary": {}}}}
	svc := services.NewRecordService(cat, store).WithFieldACL(facl).WithBeforeHooks(vetoHTTPLoader{rules: []workflow.Workflow{rule}}, nil)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"member": claimsFor("t1", "role-1", false)}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, r9Checker{}, APIHandlers{Record: NewDynamicRecordHandler(svc, zap.NewNop())}, zap.NewNop())

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer member")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct {
		name, method, path, body string
		wantStatus               int
	}{
		{"POST /:entity", http.MethodPost, "/api/v1/contacts", `{"name":"Acme"}`, http.StatusCreated},
		{"PUT /:entity/:id", http.MethodPut, "/api/v1/contacts/c1", `{"name":"Acme 2"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store.written = nil
			rec := do(tc.method, tc.path, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			// the rule really wrote the hidden field …
			if len(store.written) != 1 || store.written[0]["salary"] != r9Secret {
				t.Fatalf("store was handed %v, want salary %v from the rule", store.written, r9Secret)
			}
			// … and the response says nothing about it: an id, no record data, no field name, no value.
			var env struct {
				Data map[string]any `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if len(env.Data) != 1 || env.Data["id"] == nil {
				t.Fatalf("data = %v, want only the id", env.Data)
			}
			for _, leak := range []string{"salary", "7351"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("response leaks %q of a field the role may not see: %s", leak, rec.Body.String())
				}
			}
		})
	}
}
