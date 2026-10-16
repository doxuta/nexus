package handlers

// specs/048 S6 (T111): the dry-run body carries the simulated write — op + changes. A body key the
// handler does not map is dropped before the service sees it, so a "Sẽ chặn" preview would quietly
// become a preview of the stored record: the round trip below goes through the real handler.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// dryRunCatalog is the deals entity the preview validates changes against.
type dryRunCatalog struct{}

func (dryRunCatalog) GetEntityByName(_ context.Context, _, name string) (*metadata.EntityDefinition, error) {
	if name != "deals" {
		return nil, metadata.ErrEntityNotFound
	}
	return &metadata.EntityDefinition{ID: "ent-d", TenantID: "t1", Name: "deals"}, nil
}

func (dryRunCatalog) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	stages := `{"values":["Proposal","Closed Won"]}`
	return []metadata.FieldDefinition{
		{ID: "f1", EntityID: "ent-d", Name: "stage", FieldType: "picklist", Options: &stages},
		{ID: "f2", EntityID: "ent-d", Name: "close_date", FieldType: "date"},
	}, nil
}

type dryRunReader struct{}

func (dryRunReader) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	if tenantID != "t1" || id != "r1" {
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: "r1", TenantID: "t1", Data: map[string]any{"stage": "Proposal"}}, nil
}

func TestWorkflowAPI_DryRunOpChanges(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := services.NewWorkflowService(&memWorkflowRepo{rows: map[string]*workflow.WorkflowRecord{}}).
		WithEntityChecker(dryRunCatalog{}).WithRecordReader(dryRunReader{})
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("tenantID", "t1"); c.Next() })
	NewAdminWorkflowHandler(svc, zap.NewNop()).RegisterRoutes(&r.RouterGroup)

	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s body: %v", path, err)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(raw)))
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s = %d, body not JSON: %v (%s)", method, path, w.Code, err, w.Body.String())
		}
		return w.Code, out
	}
	code, out := call(http.MethodPost, "/admin/workflows", map[string]any{
		"name": "veto", "entityName": "deals", "trigger": "before_save", "isActive": false,
		"condition": map[string]any{"rules": []any{
			map[string]any{"field": "stage", "op": "eq", "value": "Closed Won"},
			map[string]any{"field": "close_date", "op": "isnull"},
		}},
		"actions": []any{map[string]any{"type": "veto", "field": "close_date", "message": "Chốt Thắng phải có Ngày chốt"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create rule = %d %v", code, out)
	}
	id := out["data"].(map[string]any)["workflow"].(map[string]any)["id"].(string)
	path := fmt.Sprintf("/admin/workflows/%s/test", id)

	code, out = call(http.MethodPost, path, map[string]any{"op": "create", "changes": map[string]any{"stage": "Closed Won"}})
	if code != http.StatusOK {
		t.Fatalf("dry-run op=create = %d %v", code, out)
	}
	test := out["data"].(map[string]any)["test"].(map[string]any)
	trace := test["trace"].([]any)
	last := trace[len(trace)-1].(map[string]any)["detail"].(map[string]any)
	if test["op"] != "create" || test["recordId"] != "" || test["matched"] != true || last["wouldBlock"] != true {
		t.Fatalf("op/changes did not reach the service: op=%v recordId=%v matched=%v last=%v", test["op"], test["recordId"], test["matched"], last)
	}

	code, out = call(http.MethodPost, path, map[string]any{"recordId": "r1", "changes": map[string]any{"ghost": 1}})
	details, _ := out["details"].(map[string]any)
	if code != http.StatusUnprocessableEntity || out["code"] != "WORKFLOW_TEST_CHANGES_INVALID" || details["field"] != "ghost" {
		t.Fatalf("invalid changes = %d %v, want 422 WORKFLOW_TEST_CHANGES_INVALID on ghost", code, out)
	}
}
