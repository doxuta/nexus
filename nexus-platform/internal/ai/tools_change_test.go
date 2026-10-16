package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

type wfList []workflow.WorkflowRecord

func (l wfList) ListWorkflows(context.Context, string) ([]workflow.WorkflowRecord, error) {
	return l, nil
}

type rpList []report.Record

func (l rpList) ListReports(context.Context, string) ([]report.Record, error) { return l, nil }

type failLayouts struct{}

func (failLayouts) GetLayout(context.Context, string, string, string) (*services.GetLayoutResult, error) {
	return nil, errors.New("boom")
}

func wfArgs(name string) string {
	return `{"name":"` + name + `","entity":"treatments","trigger":"after_update",` +
		`"actions":[{"type":"field_update","field":"title","value":"x"}]}`
}

func lastChange(t *testing.T, e *toolExecutor) *DraftChange {
	t.Helper()
	if len(e.draft) == 0 || e.draft[len(e.draft)-1].Change == nil {
		t.Fatalf("no change on last draft item: %+v", e.draft)
	}
	return e.draft[len(e.draft)-1].Change
}

func TestChange_WorkflowCreateVsUpdate(t *testing.T) {
	hook := json.RawMessage(`[{"type":"outbound_webhook","url":"https://api.partner.vn/x?k=SENTINEL-Q","secret":"SENTINEL-KEY"},{"type":"in_app_notification","title":"t","message":"m"}]`)
	docs := &DocReaders{Layouts: fakeLayouts{}, Reports: rpList{}, Workflows: wfList{
		{ID: "wf1", Name: "Nhắc lịch hẹn", EntityName: "treatments", Trigger: "after_update", Actions: hook, IsActive: true},
	}}
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: docs}

	ok, res := parseToolResult(t, e.execute(context.Background(), "propose_workflow", wfArgs(" Nhắc lịch hẹn ")))
	if !ok {
		t.Fatalf("refused: %v", res)
	}
	c := lastChange(t, e)
	if c.Op != "update" || c.DocType != "workflow" || c.DocKey != "wf1" {
		t.Fatalf("want update/workflow/wf1, got %+v", c)
	}
	if !strings.Contains(strings.Join(c.ChangedPaths, ","), "actions") {
		t.Fatalf("changedPaths must report actions, got %v", c.ChangedPaths)
	}
	if inner, _ := res["result"].(map[string]any); inner["op"] != "update" {
		t.Fatalf("tool result must carry op, got %v", res)
	}
	raw, _ := json.Marshal(e.draft)
	if strings.Contains(string(raw), "SENTINEL") {
		t.Fatalf("secret leaked into draft: %s", raw)
	}

	e2 := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: docs}
	if ok, res := parseToolResult(t, e2.execute(context.Background(), "propose_workflow", wfArgs("Tên mới"))); !ok {
		t.Fatalf("refused: %v", res)
	}
	if c := lastChange(t, e2); c.Op != "create" || c.DocKey != "" || len(c.Before) != 0 {
		t.Fatalf("want create without before, got %+v", c)
	}
}

func TestChange_BeforeSaveNameTaken_NoBuffer(t *testing.T) {
	docs := &DocReaders{Layouts: fakeLayouts{}, Reports: rpList{}, Workflows: wfList{
		{ID: "wf9", Name: "Chặn", EntityName: "treatments", Trigger: "before_save"},
	}}
	e := &toolExecutor{reader: twoTenantWorkflowReader(), tenantID: "tenant-A", docs: docs}
	mustReject(t, e, "propose_workflow", wfArgs("Chặn"), "WORKFLOW_NAME_TAKEN")
	if len(e.draft) != 0 {
		t.Fatalf("must not buffer, got %d", len(e.draft))
	}
}

func TestChange_PreviewReadFailed_NoBuffer(t *testing.T) {
	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: &DocReaders{Layouts: failLayouts{}}}
	mustReject(t, e, "propose_layout", `{"entity":"treatments","viewType":"edit","layoutJson":{"version":1,"sections":[{"title":"A","columns":1,"fields":["title","patient"]}]}}`, "PREVIEW_READ_FAILED")
	if len(e.draft) != 0 {
		t.Fatalf("must not buffer, got %d", len(e.draft))
	}
}

func TestChange_LayoutReadsServedBefore(t *testing.T) {
	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	ok, res := parseToolResult(t, e.execute(context.Background(), "propose_layout",
		`{"entity":"treatments","viewType":"edit","layoutJson":{"version":1,"sections":[{"title":"A","columns":1,"fields":["title","patient"]}]}}`))
	if !ok {
		t.Fatalf("refused: %v", res)
	}
	c := lastChange(t, e)
	if c.Op != "update" || c.DocType != "layout" || c.DocKey != "treatments:edit" || len(c.Before) == 0 {
		t.Fatalf("want custom update with before, got %+v", c)
	}
	// default view: label says so, no docKey
	e2 := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	if ok, res := parseToolResult(t, e2.execute(context.Background(), "propose_layout",
		`{"entity":"treatments","viewType":"detail","layoutJson":{"version":1,"sections":[{"title":"A","columns":1,"fields":["title","patient"]}]}}`)); !ok {
		t.Fatalf("refused: %v", res)
	}
	if c := lastChange(t, e2); !strings.Contains(c.Label, "(mặc định)") || c.DocKey != "" {
		t.Fatalf("default view must be labelled, got %+v", c)
	}
}

func TestChange_ReportSameNameUpdates(t *testing.T) {
	docs := &DocReaders{Layouts: fakeLayouts{}, Workflows: wfList{}, Reports: rpList{{ID: "r1", Name: "Doanh thu", EntityName: "treatments", Type: "tabular", IsActive: true}}}
	e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: docs}
	ok, res := parseToolResult(t, e.execute(context.Background(), "propose_report",
		`{"name":"Doanh thu","entity":"treatments","reportType":"tabular","columns":["title"]}`))
	if !ok {
		t.Fatalf("refused: %v", res)
	}
	if c := lastChange(t, e); c.Op != "update" || c.DocKey != "r1" {
		t.Fatalf("want update r1, got %+v", c)
	}
}

func TestChange_MoreChangedCapAndSimpleKinds(t *testing.T) {
	before, after := map[string]any{}, map[string]any{}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		before[k], after[k] = 1, 2
	}
	c := newChange("workflow", "x", "update", "l", DraftChangeTarget{}, before, after)
	if len(c.ChangedPaths) != 5 || c.MoreChanged != 2 {
		t.Fatalf("want 5 + 2 more, got %v %d", c.ChangedPaths, c.MoreChanged)
	}

	tests := []struct {
		name string
		item DraftItem
		op   string
		typ  string
	}{
		{"records", DraftItem{Kind: "records", Records: &DraftRecords{Entity: "p", Rows: []map[string]any{{"a": 1}, {"a": 2}}}}, "create", "records"},
		{"delete_field", DraftItem{Kind: "delete_field", DeleteField: &DraftDeleteField{Entity: "p", Field: "f", FieldID: "id1", Label: "F"}}, "delete", "field"},
		{"delete_entity", DraftItem{Kind: "delete_entity", DeleteEntity: &DraftDeleteEntity{Entity: "p", Label: "P", FieldCount: 3}}, "delete", "entity"},
		{"edit_field", DraftItem{Kind: "edit_field", EditField: &DraftEditField{Entity: "p", Field: "f", FieldID: "id1",
			Before: DraftFieldState{Label: "A"}, After: DraftFieldState{Label: "B"}}}, "update", "field"},
		{"entity", DraftItem{Kind: "entity", Entity: &DraftEntity{Name: "p", Label: "P"}}, "create", "entity"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := simpleChange(&tc.item)
			if got == nil || got.Op != tc.op || got.DocType != tc.typ {
				t.Fatalf("want %s/%s, got %+v", tc.op, tc.typ, got)
			}
			if tc.name == "records" && string(got.After) != `{"entity":"p","rowCount":2}` {
				t.Fatalf("records after = %s", got.After)
			}
			if tc.name == "edit_field" && (len(got.ChangedPaths) != 1 || got.ChangedPaths[0] != "label") {
				t.Fatalf("edit_field paths = %v", got.ChangedPaths)
			}
		})
	}
	if simpleChange(&DraftItem{Kind: "app", App: &DraftApp{Name: "a"}}) != nil {
		t.Fatal("app must have no change in L10-4a")
	}
}
