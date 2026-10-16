package ai

// AI-MODULES tool tests: propose_approval / propose_dashboard / propose_workqueue. They prove
// (a) validator-parity — the tools surface the REAL apply-path codes (services.
// ValidateApprovalProcess / ValidateDashboardWidgets / ValidateWorkQueueConfig, the same bodies
// CreateProcess/CreateDashboard/WorkQueueService.validate run); (b) tenant isolation — another
// tenant's entity is ENTITY_NOT_FOUND; (c) draft-buffer awareness — an entity proposed THIS turn
// is a valid target; (d) the exact FE draft wire shape (the contract the draft cards build on).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// modulesReader is tenant-A "tickets" (subject text required, status indexed picklist, due_date
// date, cost number) + tenant-B "invoices" — rich enough for filters/sorts/metrics.
func modulesReader() *fakeMetaReader {
	status := `{"values":["Mới","Đang xử lý"],"indexed":true}`
	cost := `{"min":0}`
	return &fakeMetaReader{
		entities: map[string][]metadata.EntityDefinition{
			"tenant-A": {{ID: "eT", TenantID: "tenant-A", Name: "tickets", Label: "Phiếu hỗ trợ"}},
			"tenant-B": {{ID: "eB", TenantID: "tenant-B", Name: "invoices", Label: "Hóa đơn"}},
		},
		fields: map[string]map[string][]metadata.FieldDefinition{
			"tenant-A": {"eT": {
				{ID: "f1", Name: "subject", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
				{ID: "f2", Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: &status},
				{ID: "f3", Name: "due_date", Label: "Hạn xử lý", FieldType: "date"},
				{ID: "f4", Name: "cost", Label: "Chi phí", FieldType: "number", Options: &cost},
			}},
			"tenant-B": {"eB": {{ID: "fB1", Name: "total", Label: "Tổng", FieldType: "currency"}}},
		},
	}
}

func mustBuffer(t *testing.T, e *toolExecutor, tool, args string) map[string]any {
	t.Helper()
	res := e.execute(context.Background(), tool, args)
	ok, out := parseToolResult(t, res)
	if !ok {
		t.Fatalf("%s expected ok, got %s", tool, res)
	}
	return out
}

func mustReject(t *testing.T, e *toolExecutor, tool, args, wantCode string) {
	t.Helper()
	before := len(e.draft)
	res := e.execute(context.Background(), tool, args)
	ok, out := parseToolResult(t, res)
	if ok {
		t.Fatalf("%s expected reject %s, got ok: %s", tool, wantCode, res)
	}
	errStr, _ := out["error"].(string)
	if !strings.HasPrefix(errStr, wantCode+":") && !strings.HasPrefix(errStr, wantCode+" ") && !strings.HasPrefix(errStr, wantCode) {
		t.Fatalf("%s expected code %s, got %q", tool, wantCode, errStr)
	}
	if len(e.draft) != before {
		t.Fatalf("%s rejected call must not grow the draft (before %d, after %d)", tool, before, len(e.draft))
	}
}

// ---- propose_approval -------------------------------------------------------------------------

func TestProposeApproval_BuffersValidProcess(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_approval", `{
		"name": "Duyệt chi phí lớn", "entity": "tickets", "trigger": "after_update",
		"criteria": {"match": "and", "rules": [{"field": "cost", "op": "gt", "value": 5000000}]},
		"steps": [
			{"name": "Trưởng phòng duyệt", "approverType": "role", "approverRef": "manager", "slaSeconds": 86400},
			{"approverType": "user", "approverRef": "boss@example.com"}
		]
	}`)

	if len(e.draft) != 1 || e.draft[0].Kind != "approval" || e.draft[0].Approval == nil {
		t.Fatalf("expected one approval draft, got %+v", e.draft)
	}
	ap := e.draft[0].Approval
	if ap.EntityName != "tickets" || ap.Trigger != "after_update" || !ap.IsActive {
		t.Fatalf("bad draft header: %+v", ap)
	}
	if len(ap.Steps) != 2 || ap.Steps[0].ApproverType != "role" || ap.Steps[0].ApproverRef != "manager" ||
		ap.Steps[0].SLASeconds != 86400 || ap.Steps[1].ApproverRef != "boss@example.com" {
		t.Fatalf("bad steps: %+v", ap.Steps)
	}
	if ap.Criteria == nil || len(ap.Criteria.Rules) != 1 || ap.Criteria.Rules[0].Field != "cost" {
		t.Fatalf("bad criteria: %+v", ap.Criteria)
	}

	// FE wire-shape contract: the step ref key is approverRef (resolved to approverId at Apply).
	b, _ := json.Marshal(e.draft[0])
	for _, want := range []string{`"kind":"approval"`, `"entityName":"tickets"`, `"approverRef":"manager"`, `"isActive":true`, `"trigger":"after_update"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("approval draft JSON missing %s: %s", want, b)
		}
	}
}

// TestProposeApproval_ParityRejects proves the tool surfaces the REAL apply-path codes — the
// same services.ValidateApprovalProcess body CreateProcess runs.
func TestProposeApproval_ParityRejects(t *testing.T) {
	step := `{"approverType": "role", "approverRef": "manager"}`
	longName := strings.Repeat("a", 151)
	cases := []struct {
		name, args, wantCode string
	}{
		{"empty name", `{"name": " ", "entity": "tickets", "trigger": "after_create", "steps": [` + step + `]}`, "APPROVAL_PROCESS_NAME_REQUIRED"},
		{"name over column budget", `{"name": "` + longName + `", "entity": "tickets", "trigger": "after_create", "steps": [` + step + `]}`, "APPROVAL_PROCESS_NAME_TOO_LONG"},
		{"trigger not approvable", `{"name": "x", "entity": "tickets", "trigger": "after_delete", "steps": [` + step + `]}`, "APPROVAL_TRIGGER_INVALID"},
		{"no steps", `{"name": "x", "entity": "tickets", "trigger": "after_create", "steps": []}`, "APPROVAL_STEPS_REQUIRED"},
		{"bad approver type", `{"name": "x", "entity": "tickets", "trigger": "after_create", "steps": [{"approverType": "team", "approverRef": "a"}]}`, "APPROVAL_STEP_INVALID"},
		{"empty approver ref", `{"name": "x", "entity": "tickets", "trigger": "after_create", "steps": [{"approverType": "role", "approverRef": "  "}]}`, "APPROVAL_STEP_INVALID"},
		{"negative sla", `{"name": "x", "entity": "tickets", "trigger": "after_create", "steps": [{"approverType": "role", "approverRef": "m", "slaSeconds": -1}]}`, "APPROVAL_STEP_INVALID"},
		{"bad criteria op", `{"name": "x", "entity": "tickets", "trigger": "after_create", "criteria": {"rules": [{"field": "cost", "op": "like", "value": 1}]}, "steps": [` + step + `]}`, "APPROVAL_CRITERIA_INVALID"},
		{"isnull with value", `{"name": "x", "entity": "tickets", "trigger": "after_create", "criteria": {"rules": [{"field": "cost", "op": "isnull", "value": 1}]}, "steps": [` + step + `]}`, "APPROVAL_CRITERIA_INVALID"},
		{"cross-tenant entity", `{"name": "x", "entity": "invoices", "trigger": "after_create", "steps": [` + step + `]}`, "ENTITY_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
			mustReject(t, e, "propose_approval", tc.args, tc.wantCode)
		})
	}
}

func TestProposeApproval_AcceptsEntityDraftedThisTurn(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_entity", `{"name": "orders", "label": "Đơn hàng", "fields": [
		{"name": "title", "label": "Tiêu đề", "fieldType": "text", "required": true}
	]}`)
	mustBuffer(t, e, "propose_approval", `{"name": "Duyệt đơn", "entity": "Orders", "trigger": "after_create",
		"steps": [{"approverType": "role", "approverRef": "manager"}]}`)

	if len(e.draft) != 2 || e.draft[1].Kind != "approval" {
		t.Fatalf("expected entity+approval drafts, got %+v", e.draft)
	}
	if got := e.draft[1].Approval.EntityName; got != "orders" {
		t.Fatalf("approval must carry the canonical draft name, got %q", got)
	}
}

// ---- propose_dashboard ------------------------------------------------------------------------

func TestProposeDashboard_BuffersAndNormalizes(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_dashboard", `{
		"name": "Tổng quan hỗ trợ", "description": "Nhịp vận hành",
		"widgets": [
			{"title": "Tổng ticket", "type": "kpi", "entity": "tickets", "metric": "count"},
			{"title": "Chi phí theo trạng thái", "type": "bar", "entity": "tickets", "metric": "sum", "field": "cost", "groupBy": "status"},
			{"title": "Ticket theo tháng", "type": "line", "entity": "tickets", "metric": "count", "groupBy": "due_date", "bucket": "MONTH"}
		]
	}`)

	if len(e.draft) != 1 || e.draft[0].Kind != "dashboard" || e.draft[0].Dashboard == nil {
		t.Fatalf("expected one dashboard draft, got %+v", e.draft)
	}
	d := e.draft[0].Dashboard
	if d.Name != "Tổng quan hỗ trợ" || len(d.Widgets) != 3 {
		t.Fatalf("bad dashboard draft: %+v", d)
	}
	// Pre-assigned ids, auto-layout sentinel, and the SAME bucket normalization the apply path runs.
	if d.Widgets[0].ID != "w1" || d.Widgets[2].ID != "w3" {
		t.Fatalf("widget ids must be pre-assigned w1..wN: %+v", d.Widgets)
	}
	if d.Widgets[2].Bucket != "month" {
		t.Fatalf("bucket must be normalized lowercase like the apply path, got %q", d.Widgets[2].Bucket)
	}
	for _, w := range d.Widgets {
		if w.X != 0 || w.Y != 0 || w.W != 0 || w.H != 0 {
			t.Fatalf("model must never invent grid coords (0,0,0,0 sentinel): %+v", w)
		}
	}
	// FE wire-shape contract: widgets marshal as the exact POST /admin/dashboards body slice.
	b, _ := json.Marshal(e.draft[0])
	for _, want := range []string{`"kind":"dashboard"`, `"id":"w1"`, `"groupBy":"status"`, `"bucket":"month"`, `"metric":"sum"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("dashboard draft JSON missing %s: %s", want, b)
		}
	}
}

func TestProposeDashboard_Rejects(t *testing.T) {
	kpi := `{"title": "Tổng", "type": "kpi", "entity": "tickets", "metric": "count"}`
	many := kpi
	for i := 0; i < 12; i++ {
		many += "," + kpi
	}
	cases := []struct {
		name, args, wantCode string
	}{
		{"no name", `{"name": " ", "widgets": [` + kpi + `]}`, "DASHBOARD_NAME_REQUIRED"},
		{"name over column budget", `{"name": "` + strings.Repeat("d", 151) + `", "widgets": [` + kpi + `]}`, "DASHBOARD_NAME_TOO_LONG"},
		{"no widgets", `{"name": "x", "widgets": []}`, "DASHBOARD_WIDGETS_REQUIRED"},
		{"too many widgets", `{"name": "x", "widgets": [` + many + `]}`, "DASHBOARD_TOO_MANY_WIDGETS"},
		{"bad type", `{"name": "x", "widgets": [{"title": "t", "type": "pie", "entity": "tickets", "metric": "count"}]}`, "WIDGET_TYPE_INVALID"},
		{"bad metric", `{"name": "x", "widgets": [{"title": "t", "type": "kpi", "entity": "tickets", "metric": "median"}]}`, "WIDGET_METRIC_INVALID"},
		{"bucket without groupBy", `{"name": "x", "widgets": [{"title": "t", "type": "line", "entity": "tickets", "metric": "count", "bucket": "month"}]}`, "WIDGET_BUCKET_REQUIRES_GROUPBY"},
		{"bad bucket", `{"name": "x", "widgets": [{"title": "t", "type": "line", "entity": "tickets", "metric": "count", "groupBy": "due_date", "bucket": "decade"}]}`, "WIDGET_BUCKET_INVALID"},
		{"cross-tenant entity", `{"name": "x", "widgets": [{"title": "t", "type": "kpi", "entity": "invoices", "metric": "count"}]}`, "ENTITY_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
			mustReject(t, e, "propose_dashboard", tc.args, tc.wantCode)
		})
	}
}

func TestProposeDashboard_AcceptsEntityDraftedThisTurn(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_entity", `{"name": "orders", "label": "Đơn hàng", "fields": [
		{"name": "title", "label": "Tiêu đề", "fieldType": "text", "required": true}
	]}`)
	mustBuffer(t, e, "propose_dashboard", `{"name": "Tổng quan đơn", "widgets": [
		{"title": "Tổng đơn", "type": "kpi", "entity": "orders", "metric": "count"}
	]}`)
	if len(e.draft) != 2 || e.draft[1].Dashboard.Widgets[0].Entity != "orders" {
		t.Fatalf("dashboard must accept + canonicalize a drafted entity, got %+v", e.draft)
	}
}

// ---- propose_workqueue ------------------------------------------------------------------------

func TestProposeWorkQueue_BuffersValidQueue(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_workqueue", `{
		"name": "ticket_toi_han", "label": "Ticket tới hạn hôm nay", "entity": "tickets",
		"roleName": "agent",
		"filters": [
			{"field": "due_date", "op": "lte", "value": "today"},
			{"field": "status", "op": "in", "value": "Mới,Đang xử lý"}
		],
		"sortField": "status", "sortOrder": "desc"
	}`)

	if len(e.draft) != 1 || e.draft[0].Kind != "workqueue" || e.draft[0].WorkQueue == nil {
		t.Fatalf("expected one workqueue draft, got %+v", e.draft)
	}
	q := e.draft[0].WorkQueue
	if q.EntityName != "tickets" || q.RoleName != "agent" || !q.IsActive {
		t.Fatalf("bad queue header: %+v", q)
	}
	if q.ActionVerb != "open" || q.SortOrder != "desc" || q.SortField != "status" {
		t.Fatalf("normalization drifted from the apply path: %+v", q)
	}
	if len(q.Filters) != 2 || q.Filters[0].Field != "due_date" || q.Filters[0].Operator != "lte" {
		t.Fatalf("bad filters: %+v", q.Filters)
	}
	// FE wire-shape contract: filters carry the stored {field, op, value} keys; role rides as
	// roleName (resolved to roleId at Apply); the payload key is the one-word "workqueue".
	b, _ := json.Marshal(e.draft[0])
	for _, want := range []string{`"kind":"workqueue"`, `"workqueue":{`, `"roleName":"agent"`, `"op":"lte"`, `"value":"today"`, `"actionVerb":"open"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("workqueue draft JSON missing %s: %s", want, b)
		}
	}
}

// TestProposeWorkQueue_ParityRejects proves the tool surfaces the REAL apply-path codes — the
// same services.ValidateWorkQueueConfig body WorkQueueService.validate runs (incl. the runtime's
// own date-token + RM-09 sort-allowlist checks).
func TestProposeWorkQueue_ParityRejects(t *testing.T) {
	cases := []struct {
		name, args, wantCode string
	}{
		{"missing label", `{"name": "q", "label": " ", "entity": "tickets"}`, "WORKQUEUE_INVALID"},
		{"bad action", `{"name": "q", "label": "Q", "entity": "tickets", "actionVerb": "delete"}`, "WORKQUEUE_ACTION_INVALID"},
		{"unknown filter field", `{"name": "q", "label": "Q", "entity": "tickets", "filters": [{"field": "ghost", "op": "eq", "value": "1"}]}`, "WORKQUEUE_FILTER_INVALID"},
		{"bogus date token", `{"name": "q", "label": "Q", "entity": "tickets", "filters": [{"field": "due_date", "op": "eq", "value": "overdue"}]}`, "WORKQUEUE_FILTER_INVALID"},
		{"unsortable field", `{"name": "q", "label": "Q", "entity": "tickets", "sortField": "subject"}`, "WORKQUEUE_SORT_NOT_SORTABLE"},
		{"cross-tenant entity", `{"name": "q", "label": "Q", "entity": "invoices"}`, "ENTITY_NOT_FOUND"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
			mustReject(t, e, "propose_workqueue", tc.args, tc.wantCode)
		})
	}
}

func TestProposeWorkQueue_AcceptsEntityDraftedThisTurn(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	mustBuffer(t, e, "propose_entity", `{"name": "tasks", "label": "Việc", "fields": [
		{"name": "title", "label": "Tiêu đề", "fieldType": "text", "required": true},
		{"name": "due_date", "label": "Hạn", "fieldType": "date"},
		{"name": "status", "label": "Trạng thái", "fieldType": "picklist", "options": {"values": ["Mới", "Xong"], "indexed": true}}
	]}`)
	// Filters + sort validate against the DRAFTED shape-only specs: the date token resolves on
	// the drafted date field, and the indexed drafted picklist is sortable (IsIndexed mapping).
	mustBuffer(t, e, "propose_workqueue", `{
		"name": "viec_qua_han", "label": "Việc quá hạn", "entity": "tasks",
		"filters": [{"field": "due_date", "op": "lt", "value": "today"}],
		"sortField": "status"
	}`)
	q := e.draft[1].WorkQueue
	if q == nil || q.EntityName != "tasks" || q.SortField != "status" {
		t.Fatalf("workqueue must validate against drafted specs, got %+v", e.draft[1])
	}
}

func TestProposeDashboard_JoinsStrippedWithNote(t *testing.T) {
	e := &toolExecutor{reader: modulesReader(), tenantID: "tenant-A"}
	out := mustBuffer(t, e, "propose_dashboard", `{"name":"x","widgets":[{"title":"t","type":"bar","entity":"tickets","metric":"count","groupBy":"status","joins":[{"relationship":"assignee"}]}]}`)
	res, _ := out["result"].(map[string]any)
	if n, _ := res["note"].(string); !strings.Contains(n, "joins đã bị bỏ qua") {
		t.Fatalf("want a stripped-joins note, got %v", out)
	}
	if b, _ := json.Marshal(e.draft[0]); strings.Contains(string(b), `"joins"`) {
		t.Fatalf("joins must not reach the draft: %s", b)
	}
}
