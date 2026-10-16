package handlers

// specs/048 S1b (T033) — the wire shape of a before_save veto and of "rules unreadable", through the
// production route chain (Auth → Tenant → RequireACL → handler) over a REAL RecordService: the 422
// envelope the form anchors under a field, and the 503 + Retry-After a client can back off on.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// vetoHTTPStore serves one stored contact to Update's reads.
type vetoHTTPStore struct{ hiddenMetaStore }

func (s *vetoHTTPStore) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	return &dynrepo.Record{ID: id, TenantID: tenantID, Data: map[string]any{"name": "Acme", "stage": "Proposal"}}, nil
}

type vetoHTTPLoader struct {
	rules []workflow.Workflow
	err   error
}

func (l vetoHTTPLoader) ListActive(context.Context, string, string, workflow.Trigger) ([]workflow.Workflow, error) {
	return l.rules, l.err
}

func buildVetoEngine(loader workflow.WorkflowLoader) *gin.Engine {
	gin.SetMode(gin.TestMode)
	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "stage", FieldType: "text"},
			{Name: "close_date", FieldType: "date"},
		},
	}
	svc := services.NewRecordService(cat, &vetoHTTPStore{}).WithBeforeHooks(loader, nil)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"member": claimsFor("t1", "role-1", false)}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, &massopBothChecker{}, APIHandlers{
		Record: NewDynamicRecordHandler(svc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

func putContact(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/contacts/c1", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer member")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestDynamicRecordHandler_HookVetoEnvelope(t *testing.T) {
	rule := workflow.Workflow{ID: "rule-won", TenantID: "t1", EntityName: "contacts", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{SaveOn: workflow.SaveOnUpdate, Rules: []workflow.Rule{
			{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"},
			{Field: "close_date", Op: workflow.OpIsNull},
		}},
		Actions: []workflow.Action{{Type: workflow.ActionVeto, Field: "close_date", Message: "Chốt Thắng phải có Ngày chốt"}}}
	r := buildVetoEngine(vetoHTTPLoader{rules: []workflow.Workflow{rule}})

	rec := putContact(t, r, `{"stage":"Closed Won"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Details struct {
			Field      string `json:"field"`
			Violations []struct {
				RuleID  *string `json:"ruleId"`
				Field   string  `json:"field"`
				Message string  `json:"message"`
			} `json:"violations"`
		} `json:"details"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if env.Code != "RECORD_HOOK_VETO" || env.Error != "Chốt Thắng phải có Ngày chốt" || env.Details.Field != "close_date" {
		t.Fatalf("envelope = %+v", env)
	}
	if len(env.Details.Violations) != 1 || env.Details.Violations[0].Field != "close_date" ||
		env.Details.Violations[0].Message != "Chốt Thắng phải có Ngày chốt" || env.Details.Violations[0].RuleID != nil {
		t.Fatalf("violations = %+v (no ruleId for a non-admin)", env.Details.Violations)
	}

	if rec := putContact(t, r, `{"stage":"Closed Won","close_date":"2026-10-01"}`); rec.Code != http.StatusOK {
		t.Fatalf("compliant save status = %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestDynamicRecordHandler_HooksUnavailable503(t *testing.T) {
	r := buildVetoEngine(vetoHTTPLoader{err: errors.New("sys_workflows: connection refused")})
	rec := putContact(t, r, `{"name":"Acme 2"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var env struct{ Code, Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != "RECORD_HOOKS_UNAVAILABLE" || env.Error == "" {
		t.Fatalf("envelope = %+v", env)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("connection refused")) {
		t.Fatalf("infrastructure error leaked to the client: %s", rec.Body.String())
	}
}

// noImportStore fails loudly if the import ever reaches the store — a refused import writes nothing.
type noImportStore struct{}

func (noImportStore) InsertRecords(context.Context, string, string, string, []map[string]any, []dynrepo.UniqueFieldSpec) ([]string, error) {
	return nil, errors.New("noImportStore: insert reached the store")
}

func (noImportStore) BatchExistingValuesForField(context.Context, string, string, string, dynrepo.DedupFieldSpec, []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

// specs/048 S2 (T052): unreadable rules refuse a CSV import with the same 503 + Retry-After the
// record routes answer, through the production route chain over a REAL ImportService.
func TestImportHandler_HooksUnavailable503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cat := &hiddenMetaCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-1", TenantID: "t1", Name: "contacts", DataTable: "obj_contacts"},
		fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}, {Name: "stage", FieldType: "text"}},
	}
	svc := services.NewImportService(cat, noImportStore{}, zap.NewNop()).
		WithBeforeHooks(vetoHTTPLoader{err: errors.New("sys_workflows: connection refused")}, nil)
	ver := &stubVerifier{byToken: map[string]*auth.Claims{"member": claimsFor("t1", "role-1", false)}}
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), ver, nil, APIHandlers{
		Import: NewImportHandler(svc, zap.NewNop()),
		Record: NewDynamicRecordHandler(nil, zap.NewNop()),
	}, zap.NewNop())

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "contacts.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("name,stage\nAcme,Proposal\n")); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("mapping", `{"name":"name","stage":"stage"}`); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer member")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	var env struct{ Code, Error string }
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != "RECORD_HOOKS_UNAVAILABLE" || env.Error == "" {
		t.Fatalf("envelope = %+v", env)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("connection refused")) {
		t.Fatalf("infrastructure error leaked to the client: %s", rec.Body.String())
	}
}
