package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// fakeRecordReader records every scope/entity/query it receives so tests can prove the caller's
// RecordScope is threaded VERBATIM from the executor (trap #1) — never widened, narrowed, or
// rebuilt from the thin ai.Scope.
type fakeRecordReader struct {
	gotScopes   []services.RecordScope
	gotEntities []string
	lastListQ   services.ListQuery
	lastAggQ    services.AggregateQuery

	listRes services.ListResult
	aggRes  services.AggregateResult
	ents    []metadata.EntityDefinition
	fields  []metadata.FieldDefinition
	reports []report.Record
	runRes  services.ReportRunResult
	err     error

	gotReportID string

	// GetRecord fixtures (propose_record_update existence probe) — a SEPARATE error slot so a
	// test can have Fields succeed while the probe fails (record absent).
	getRec    *dynrepo.Record
	getRecErr error
	gotGetIDs []string
}

func (f *fakeRecordReader) GetRecord(_ context.Context, scope services.RecordScope, _, entity, id string) (*dynrepo.Record, error) {
	f.gotScopes = append(f.gotScopes, scope)
	f.gotEntities = append(f.gotEntities, entity)
	f.gotGetIDs = append(f.gotGetIDs, id)
	if f.getRecErr != nil {
		return nil, f.getRecErr
	}
	return f.getRec, nil
}

func (f *fakeRecordReader) QueryRecords(_ context.Context, scope services.RecordScope, _, entity string, q services.ListQuery) (services.ListResult, error) {
	f.gotScopes = append(f.gotScopes, scope)
	f.gotEntities = append(f.gotEntities, entity)
	f.lastListQ = q
	return f.listRes, f.err
}

func (f *fakeRecordReader) AggregateRecords(_ context.Context, scope services.RecordScope, _, entity string, q services.AggregateQuery) (services.AggregateResult, error) {
	f.gotScopes = append(f.gotScopes, scope)
	f.gotEntities = append(f.gotEntities, entity)
	f.lastAggQ = q
	return f.aggRes, f.err
}

func (f *fakeRecordReader) ViewableEntities(_ context.Context, scope services.RecordScope, _ string) ([]metadata.EntityDefinition, error) {
	f.gotScopes = append(f.gotScopes, scope)
	return f.ents, f.err
}

func (f *fakeRecordReader) Fields(_ context.Context, scope services.RecordScope, _, entity string) ([]metadata.FieldDefinition, error) {
	f.gotScopes = append(f.gotScopes, scope)
	f.gotEntities = append(f.gotEntities, entity)
	return f.fields, f.err
}

func (f *fakeRecordReader) ListReports(_ context.Context, scope services.RecordScope, _ string) ([]report.Record, error) {
	f.gotScopes = append(f.gotScopes, scope)
	return f.reports, f.err
}

func (f *fakeRecordReader) RunReport(_ context.Context, scope services.RecordScope, _, id string) (services.ReportRunResult, error) {
	f.gotScopes = append(f.gotScopes, scope)
	f.gotReportID = id
	return f.runRes, f.err
}

// callerScope is a distinctive non-admin scope: every field non-zero so a rebuild-from-ai.Scope
// (which would lose AccessLevel) or any mutation is caught by the == comparison.
func callerScope() services.RecordScope {
	return services.RecordScope{UserID: "u-77", AccessLevel: "team", IsAdmin: false, RoleID: "r-9"}
}

// TestAssistantTools_QueryRecords_ScopeVerbatim is the trap-#1 pin at the executor→reader
// boundary: query_records must hand the reader EXACTLY the RecordScope the handler built.
func TestAssistantTools_QueryRecords_ScopeVerbatim(t *testing.T) {
	rd := &fakeRecordReader{}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}

	res := e.execute(context.Background(), "query_records",
		`{"entity":"opportunities","filters":[{"field":"status","op":"eq","values":["open"]}],"limit":5,"sort":"created_at","order":"asc"}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("query_records failed: %s", res)
	}
	if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
		t.Fatalf("reader scope = %+v; want the caller scope threaded VERBATIM %+v", rd.gotScopes, callerScope())
	}
	if rd.gotEntities[0] != "opportunities" {
		t.Fatalf("entity = %q; want opportunities", rd.gotEntities[0])
	}
	q := rd.lastListQ
	if q.Page != 1 || q.Limit != 5 || q.Sort != "created_at" || q.Order != "asc" {
		t.Fatalf("list query = %+v; want page=1 limit=5 sort=created_at order=asc", q)
	}
	if len(q.Filters) != 1 || q.Filters[0].Field != "status" || string(q.Filters[0].Operator) != "eq" || q.Filters[0].Values[0] != "open" {
		t.Fatalf("filters = %+v; want the status=open predicate", q.Filters)
	}
}

// TestAssistantTools_QueryRecords_LimitClamped: no limit → default 10; over-cap → 50.
func TestAssistantTools_QueryRecords_LimitClamped(t *testing.T) {
	rd := &fakeRecordReader{}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}

	e.execute(context.Background(), "query_records", `{"entity":"accounts"}`)
	if rd.lastListQ.Limit != assistantDefaultRows {
		t.Fatalf("default limit = %d; want %d", rd.lastListQ.Limit, assistantDefaultRows)
	}
	e.execute(context.Background(), "query_records", `{"entity":"accounts","limit":5000}`)
	if rd.lastListQ.Limit != assistantMaxRows {
		t.Fatalf("clamped limit = %d; want %d", rd.lastListQ.Limit, assistantMaxRows)
	}
}

// TestAssistantTools_AggregateRecords_ScopeVerbatim mirrors the trap-#1 pin for aggregates and
// checks the query mapping (metric/field/groupBy/bucket/filters).
func TestAssistantTools_AggregateRecords_ScopeVerbatim(t *testing.T) {
	rd := &fakeRecordReader{}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}

	res := e.execute(context.Background(), "aggregate_records",
		`{"entity":"opportunities","metric":"sum","field":"amount","groupBy":"stage","filters":[{"field":"status","values":["open"]}]}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("aggregate_records failed: %s", res)
	}
	if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
		t.Fatalf("reader scope = %+v; want the caller scope threaded VERBATIM", rd.gotScopes)
	}
	q := rd.lastAggQ
	if q.Metric != services.AggMetricSum || q.Field != "amount" || q.GroupBy != "stage" {
		t.Fatalf("aggregate query = %+v; want sum(amount) group by stage", q)
	}
	if len(q.Filters) != 1 || q.Filters[0].Field != "status" || q.Filters[0].Operator != "" {
		t.Fatalf("filters = %+v; want status filter with empty (eq) operator", q.Filters)
	}
}

// TestAssistantTools_AggregateResult_Shapes: scalar vs grouped envelopes, currencyCode surfaced.
func TestAssistantTools_AggregateResult_Shapes(t *testing.T) {
	v := "1500000"
	rd := &fakeRecordReader{aggRes: services.AggregateResult{
		Scalar: services.AggregateScalarResult{Value: &v, Count: 12}, CurrencyCode: "VND",
	}}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	ok, m := parseToolResult(t, e.execute(context.Background(), "aggregate_records", `{"entity":"opps","metric":"sum","field":"amount"}`))
	if !ok {
		t.Fatal("scalar aggregate should succeed")
	}
	result := m["result"].(map[string]any)
	if result["value"] != "1500000" || result["count"] != float64(12) || result["currencyCode"] != "VND" {
		t.Fatalf("scalar envelope = %+v; want value/count/currencyCode", result)
	}

	key := "open"
	rd.aggRes = services.AggregateResult{Grouped: true, Groups: []services.AggregateGroupResult{{Key: &key, Value: nil, Count: 3}}}
	ok, m = parseToolResult(t, e.execute(context.Background(), "aggregate_records", `{"entity":"opps","metric":"count","groupBy":"status"}`))
	if !ok {
		t.Fatal("grouped aggregate should succeed")
	}
	groups := m["result"].(map[string]any)["groups"].([]any)
	g0 := groups[0].(map[string]any)
	if g0["key"] != "open" || g0["value"] != nil || g0["count"] != float64(3) {
		t.Fatalf("group envelope = %+v; want key=open value=null count=3", g0)
	}
}

// TestAssistantTools_ErrorMapping: the reader's sentinels/validation errors surface as machine
// codes the model can self-correct on — and a forbidden entity is indistinguishable from a
// nonexistent one (ENTITY_NOT_FOUND, no existence leak).
func TestAssistantTools_ErrorMapping(t *testing.T) {
	rd := &fakeRecordReader{err: services.ErrEntityNotFound}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	ok, m := parseToolResult(t, e.execute(context.Background(), "query_records", `{"entity":"secrets"}`))
	if ok {
		t.Fatal("a not-found/forbidden entity must not succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}

	rd.err = &services.ValidationError{Code: "LIST_UNKNOWN_FILTER", Msg: "không thể lọc theo field \"bogus\""}
	ok, m = parseToolResult(t, e.execute(context.Background(), "query_records", `{"entity":"accounts","filters":[{"field":"bogus"}]}`))
	if ok {
		t.Fatal("a validation error must not succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "LIST_UNKNOWN_FILTER") {
		t.Fatalf("expected the real validation code, got %q", errStr)
	}
}

// TestAssistantTools_RegistryReadPlusHITLDraft is the trap-#3 pin, deliberately AMENDED 2026-07-26
// (⌘K-can-act, EASE Nước 7): the assistant registry is no longer a read-only cul-de-sac — it is
// READ + HITL-DRAFT, still ZERO direct writes. Exactly eight tools: the six reads plus
// propose_record_create / propose_record_update, which only validate-and-buffer a record draft
// the USER applies later (FE → POST /:entity / PUT /:entity/:id under their own token). The two
// record proposals are the ONLY propose_* names allowed; every builder metadata/admin surface
// (propose_entity/…/snapshot_model — checked against the LIVE builder registry so a future
// builder tool can never leak in silently) stays structurally unreachable. A separate explicit
// list (not a filtered builder registry) is what makes that guarantee hold as the builder grows.
// (Earlier amendment 2026-07-25 A3: + list_reports + run_report.)
func TestAssistantTools_RegistryReadPlusHITLDraft(t *testing.T) {
	defs := AssistantToolDefinitions()
	if len(defs) != 8 {
		t.Fatalf("expected exactly 8 assistant tools, got %d — extend this invariant deliberately", len(defs))
	}
	want := map[string]bool{
		"list_entities": true, "list_fields": true, "query_records": true, "aggregate_records": true,
		"list_reports": true, "run_report": true,
		"propose_record_create": true, "propose_record_update": true,
	}
	allowedPropose := map[string]bool{"propose_record_create": true, "propose_record_update": true}

	// Builder metadata/admin surfaces, from the LIVE builder registry: any name the builder
	// treats as a propose/snapshot surface is banned here (the two registries may share only
	// read-tool names like list_entities/list_fields).
	builderSurfaces := map[string]bool{}
	for _, bt := range ToolDefinitions() {
		if n := bt.Function.Name; strings.HasPrefix(n, "propose_") || strings.HasPrefix(n, "snapshot") {
			builderSurfaces[n] = true
		}
	}

	for _, tool := range defs {
		name := tool.Function.Name
		if !want[name] {
			t.Fatalf("unexpected assistant tool %q", name)
		}
		delete(want, name)
		if strings.HasPrefix(name, "propose_") && !allowedPropose[name] {
			t.Fatalf("assistant tool %q is a propose surface beyond the two HITL record drafts", name)
		}
		if builderSurfaces[name] {
			t.Fatalf("assistant tool %q is a builder metadata surface — must stay builder-only", name)
		}
		// Direct-write / admin vocabulary stays banned outright (a draft proposes; it never writes).
		for _, banned := range []string{"snapshot", "delete", "write", "admin", "apply", "execute_"} {
			if strings.Contains(strings.ToLower(name), banned) {
				t.Fatalf("assistant tool %q matches banned surface %q", name, banned)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing assistant tools: %v", want)
	}
}

// TestAssistantTools_NoTenantParamInSchemas mirrors the builder's by-construction isolation pin:
// no assistant tool schema exposes a tenant field the model could fill in — the tenant flows
// one-way from the verified JWT into the executor.
func TestAssistantTools_NoTenantParamInSchemas(t *testing.T) {
	for _, tool := range AssistantToolDefinitions() {
		blob, _ := json.Marshal(tool.Function.Parameters)
		if strings.Contains(strings.ToLower(string(blob)), "tenant") {
			t.Fatalf("assistant tool %q schema exposes a tenant parameter — isolation must be server-injected: %s",
				tool.Function.Name, blob)
		}
	}
}

// TestAssistantTools_UnknownTool: the executor rejects anything outside its registry — including
// builder tool names, which must NOT be executable through the assistant.
func TestAssistantTools_UnknownTool(t *testing.T) {
	e := &assistantExecutor{reader: &fakeRecordReader{}, tenantID: "t1", scope: callerScope()}
	for _, name := range []string{"propose_entity", "propose_workflow", "snapshot_model", "delete_everything"} {
		ok, m := parseToolResult(t, e.execute(context.Background(), name, "{}"))
		if ok {
			t.Fatalf("tool %q must not be executable by the assistant", name)
		}
		if errStr, _ := m["error"].(string); !strings.Contains(errStr, "UNKNOWN_TOOL") {
			t.Fatalf("expected UNKNOWN_TOOL for %q, got %q", name, errStr)
		}
	}
}

// TestAssistantTools_ListEntities_UsesReader: the ACL-filtered reader result is what the model
// sees — name+label only.
func TestAssistantTools_ListEntities_UsesReader(t *testing.T) {
	rd := &fakeRecordReader{ents: []metadata.EntityDefinition{{Name: "accounts", Label: "Khách hàng"}}}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	res := e.execute(context.Background(), "list_entities", "{}")
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("list_entities failed: %s", res)
	}
	if !strings.Contains(res, "accounts") || !strings.Contains(res, "Khách hàng") {
		t.Fatalf("list_entities must surface the reader's entities: %s", res)
	}
	if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
		t.Fatalf("reader scope = %+v; want the caller scope threaded VERBATIM", rd.gotScopes)
	}
}

// TestAssistantExecute_ListReports — A3 semantic-layer catalog: id/name/entity/type/isActive
// surface for the model, caller scope threaded verbatim.
func TestAssistantExecute_ListReports(t *testing.T) {
	rd := &fakeRecordReader{reports: []report.Record{
		{ID: "rp-1", Name: "Doanh thu theo tháng", EntityName: "crm_opportunities", Type: "summary", IsActive: true},
		{ID: "rp-2", Name: "Bản nháp", EntityName: "crm_leads", Type: "tabular", IsActive: false},
	}}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	res := e.execute(context.Background(), "list_reports", "{}")
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("list_reports failed: %s", res)
	}
	for _, want := range []string{"rp-1", "Doanh thu theo tháng", "crm_opportunities", "summary", "rp-2"} {
		if !strings.Contains(res, want) {
			t.Fatalf("list_reports missing %q: %s", want, res)
		}
	}
	if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
		t.Fatalf("reader scope = %+v; want caller scope verbatim", rd.gotScopes)
	}
}

// TestAssistantExecute_RunReport_Tabular — saved-definition projection: rows projected to the
// report's columns only, id + total kept, scope verbatim, report id forwarded.
func TestAssistantExecute_RunReport_Tabular(t *testing.T) {
	rd := &fakeRecordReader{runRes: services.ReportRunResult{
		Tabular: &services.TabularRunResult{
			Columns: []string{"name", "amount"},
			Records: []services.ReportRow{
				{ID: "r1", Data: map[string]any{"name": "Acme", "amount": 5, "secret_extra": "drop-me"}},
			},
			Total: 1,
		},
	}}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	res := e.execute(context.Background(), "run_report", `{"id":"rp-1"}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("run_report failed: %s", res)
	}
	if rd.gotReportID != "rp-1" {
		t.Fatalf("report id = %q; want rp-1", rd.gotReportID)
	}
	if !strings.Contains(res, "Acme") || !strings.Contains(res, `"total":1`) {
		t.Fatalf("run_report must carry projected rows + total: %s", res)
	}
	if strings.Contains(res, "secret_extra") {
		t.Fatalf("run_report must project data to the report's columns ONLY: %s", res)
	}
	if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
		t.Fatalf("reader scope = %+v; want caller scope verbatim", rd.gotScopes)
	}
}

// TestRunReportTool_TreeAndGridWithLabels (specs/049 S7b T078; replaces the S2b honest-error pin):
// a tree / cross-tab is read back with its display labels, the caller's own numbers (scope
// threaded verbatim) and, over the budget, cut to <=50 level-1 groups / <=30 rows x 20 columns
// with truncated:true.
func TestRunReportTool_TreeAndGridWithLabels(t *testing.T) {
	s := func(v string) *string { return &v }
	grp := func(k, v, l string, c int64) services.AggregateGroupResult {
		return services.AggregateGroupResult{Key: s(k), Value: s(v), Count: c, Label: s(l)}
	}
	result := func(t *testing.T, raw string) map[string]any {
		t.Helper()
		ok, m := parseToolResult(t, raw)
		if !ok {
			t.Fatalf("run_report failed: %s", raw)
		}
		r, _ := m["result"].(map[string]any)
		return r
	}
	t.Run("tree keeps labels and children", func(t *testing.T) {
		rd := &fakeRecordReader{runRes: services.ReportRunResult{
			Type: "summary", Axes: []services.ReportGroupInput{{Field: "stage"}, {Field: "close_date", Bucket: "month"}},
			Grid: &services.GridRunResult{Kind: services.GridKindTree, CurrencyCode: "VND",
				Groups: []services.GridNode{{AggregateGroupResult: grp("won", "30", "Thắng", 2),
					Children: []services.GridNode{{AggregateGroupResult: grp("2026-09", "30", "09/2026", 2)}}}},
				Total: services.AggregateScalarResult{Value: s("30"), Count: 2}},
			Having: &services.ReportHavingInput{On: "value", Op: "gt", Values: []string{"10"}},
		}}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		raw := e.execute(context.Background(), "run_report", `{"id":"rp-tree"}`)
		tr := result(t, raw)
		if hv, _ := tr["having"].(map[string]any); hv == nil || hv["op"] != "gt" {
			t.Fatalf("having must be passed through: %v", tr["having"])
		}
		if tot, _ := tr["total"].(map[string]any); tot == nil || tot["count"] != float64(2) || tot["value"] != "30" {
			t.Fatalf("total must be passed through: %v", tr["total"])
		}
		for _, w := range []string{`"Thắng"`, `"09/2026"`, `"children"`, `"levels"`, `"VND"`} {
			if !strings.Contains(raw, w) {
				t.Fatalf("tree result missing %s: %s", w, raw)
			}
		}
		if strings.Contains(raw, "truncated") {
			t.Fatalf("small tree must not be truncated: %s", raw)
		}
		if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
			t.Fatalf("scope not threaded verbatim: %+v", rd.gotScopes)
		}
	})
	t.Run("tree over budget is cut", func(t *testing.T) {
		var gs []services.GridNode
		for i := 0; i < 60; i++ {
			gs = append(gs, services.GridNode{AggregateGroupResult: grp(fmt.Sprint("k", i), "1", "L", 1)})
		}
		rd := &fakeRecordReader{runRes: services.ReportRunResult{Type: "summary",
			Grid: &services.GridRunResult{Kind: services.GridKindTree, Groups: gs}}}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		m := result(t, e.execute(context.Background(), "run_report", `{"id":"x"}`))
		if got := len(m["groups"].([]any)); got != 50 {
			t.Fatalf("level-1 groups = %d; want 50", got)
		}
		if m["truncated"] != true {
			t.Fatalf("truncated flag missing: %v", m)
		}
	})
	t.Run("matrix over budget is cut and cells follow", func(t *testing.T) {
		g := &services.GridRunResult{Kind: services.GridKindMatrix}
		for i := 0; i < 40; i++ {
			g.Rows = append(g.Rows, grp(fmt.Sprint("r", i), "1", "R", 1))
		}
		for i := 0; i < 25; i++ {
			g.Columns = append(g.Columns, grp(fmt.Sprint("c", i), "1", "C", 1))
		}
		g.Cells = []services.GridCell{{R: 0, C: 0, Value: s("1"), Count: 1}, {R: 35, C: 0, Value: s("1"), Count: 1}, {R: 0, C: 22, Value: s("1"), Count: 1}}
		rd := &fakeRecordReader{runRes: services.ReportRunResult{Type: "matrix",
			Axes: []services.ReportGroupInput{{Field: "a"}, {Field: "b"}}, Grid: g}}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		m := result(t, e.execute(context.Background(), "run_report", `{"id":"x"}`))
		if ra, _ := m["rowAxis"].(map[string]any); ra == nil || ra["field"] != "a" {
			t.Fatalf("rowAxis missing: %v", m["rowAxis"])
		}
		if ca, _ := m["columnAxis"].(map[string]any); ca == nil || ca["field"] != "b" {
			t.Fatalf("columnAxis missing: %v", m["columnAxis"])
		}
		if len(m["rows"].([]any)) != 30 || len(m["columns"].([]any)) != 20 || len(m["cells"].([]any)) != 1 || m["truncated"] != true {
			t.Fatalf("matrix cut wrong: %v", m)
		}
	})
}

// TestAssistantExecute_RunReport_ValidationCode — REPORT_NOT_FOUND (and siblings REPORT_INACTIVE
// / FORBIDDEN) must surface their machine code so the model can self-correct.
func TestAssistantExecute_RunReport_ValidationCode(t *testing.T) {
	rd := &fakeRecordReader{err: &services.ValidationError{Code: "REPORT_NOT_FOUND", Msg: "report không tồn tại"}}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	res := e.execute(context.Background(), "run_report", `{"id":"nope"}`)
	if ok, _ := parseToolResult(t, res); ok {
		t.Fatalf("run_report with unknown id must fail: %s", res)
	}
	if !strings.Contains(res, "REPORT_NOT_FOUND") {
		t.Fatalf("run_report must surface the validation code: %s", res)
	}
}

// ---- ⌘K-can-act (HITL record drafts) ------------------------------------------------------------

// ticketFields is a small realistic entity: one required text + one picklist with pinned values —
// enough to exercise required/unknown/picklist validation parity.
func ticketFields() []metadata.FieldDefinition {
	opts := `{"values":["low","high"]}`
	return []metadata.FieldDefinition{
		{Name: "title", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
		{Name: "priority", Label: "Ưu tiên", FieldType: "picklist", Options: &opts},
	}
}

// TestAssistantTools_ProposeRecordCreate_BuffersDraft: a valid payload validates through the SAME
// services.ValidateRecord body the POST /:entity apply path runs and lands in the executor's
// draft buffer as Kind="record_create" — NOTHING written, scope threaded verbatim to the
// ACL-gated Fields read.
func TestAssistantTools_ProposeRecordCreate_BuffersDraft(t *testing.T) {
	rd := &fakeRecordReader{fields: ticketFields()}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}

	res := e.execute(context.Background(), "propose_record_create",
		`{"entity":"tickets","data":{"title":"Khách A lỗi đăng nhập","priority":"high"}}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_record_create failed: %s", res)
	}
	if len(rd.gotScopes) != 1 || rd.gotScopes[0] != callerScope() {
		t.Fatalf("reader scope = %+v; want the caller scope threaded VERBATIM", rd.gotScopes)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "record_create" || e.draft[0].RecordCreate == nil {
		t.Fatalf("draft = %+v; want one record_create item", e.draft)
	}
	d := e.draft[0].RecordCreate
	if d.Entity != "tickets" || d.Data["title"] != "Khách A lỗi đăng nhập" || d.Data["priority"] != "high" {
		t.Fatalf("draft payload = %+v; want the RAW proposed data", d)
	}
}

// TestAssistantTools_ProposeRecordCreate_ValidationParity: invalid payloads surface the REAL
// record-engine codes (required missing / unknown field / bad picklist) and buffer NOTHING —
// the model self-corrects in-chat, exactly like the apply path would refuse.
func TestAssistantTools_ProposeRecordCreate_ValidationParity(t *testing.T) {
	cases := []struct {
		name, args, wantCode string
	}{
		{"required missing", `{"entity":"tickets","data":{"priority":"high"}}`, "RECORD_REQUIRED_MISSING"},
		{"unknown field", `{"entity":"tickets","data":{"title":"x","bogus":"y"}}`, "RECORD_UNKNOWN_FIELD"},
		{"picklist outside values", `{"entity":"tickets","data":{"title":"x","priority":"urgent"}}`, "RECORD_INVALID_PICKLIST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rd := &fakeRecordReader{fields: ticketFields()}
			e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
			res := e.execute(context.Background(), "propose_record_create", tc.args)
			if ok, _ := parseToolResult(t, res); ok {
				t.Fatalf("invalid payload must fail: %s", res)
			}
			if !strings.Contains(res, tc.wantCode) {
				t.Fatalf("error = %s; want code %s", res, tc.wantCode)
			}
			if len(e.draft) != 0 {
				t.Fatalf("an invalid proposal must buffer nothing; draft = %+v", e.draft)
			}
		})
	}
}

// TestAssistantTools_ProposeRecord_EntityNoLeak: a forbidden entity is indistinguishable from a
// nonexistent one on BOTH propose tools (the reader's scopeFor semantics ride in via Fields).
func TestAssistantTools_ProposeRecord_EntityNoLeak(t *testing.T) {
	rd := &fakeRecordReader{err: services.ErrEntityNotFound}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
	for _, call := range []struct{ tool, args string }{
		{"propose_record_create", `{"entity":"secrets","data":{"x":"1"}}`},
		{"propose_record_update", `{"entity":"secrets","recordId":"r-1","data":{"x":"1"}}`},
	} {
		res := e.execute(context.Background(), call.tool, call.args)
		if ok, _ := parseToolResult(t, res); ok {
			t.Fatalf("%s on a forbidden entity must fail: %s", call.tool, res)
		}
		if !strings.Contains(res, "ENTITY_NOT_FOUND") {
			t.Fatalf("%s error = %s; want ENTITY_NOT_FOUND (no existence leak)", call.tool, res)
		}
	}
	if len(e.draft) != 0 {
		t.Fatalf("no draft may be buffered; got %+v", e.draft)
	}
}

// TestAssistantTools_ProposeRecordUpdate_BuffersDraft: partial data validates through the SAME
// services.ValidatePartialRecord body the PUT apply path runs, the record's existence is probed
// through the reader's GetRecord (same engine as GET /:entity/:id), and the draft echoes Before =
// the CURRENT values of exactly the touched fields for an honest old→new confirm card.
func TestAssistantTools_ProposeRecordUpdate_BuffersDraft(t *testing.T) {
	rd := &fakeRecordReader{
		fields: ticketFields(),
		getRec: &dynrepo.Record{ID: "r-1", Data: map[string]any{"title": "Khách A lỗi đăng nhập", "priority": "low"}},
	}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}

	res := e.execute(context.Background(), "propose_record_update",
		`{"entity":"tickets","recordId":"r-1","data":{"priority":"high"}}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_record_update failed: %s", res)
	}
	if len(rd.gotGetIDs) != 1 || rd.gotGetIDs[0] != "r-1" {
		t.Fatalf("probe ids = %v; want the proposed recordId", rd.gotGetIDs)
	}
	for i, got := range rd.gotScopes { // Fields + GetRecord both ride the caller scope verbatim
		if got != callerScope() {
			t.Fatalf("reader call %d scope = %+v; want caller scope verbatim", i, got)
		}
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "record_update" || e.draft[0].RecordUpdate == nil {
		t.Fatalf("draft = %+v; want one record_update item", e.draft)
	}
	d := e.draft[0].RecordUpdate
	if d.Entity != "tickets" || d.RecordID != "r-1" || d.Data["priority"] != "high" {
		t.Fatalf("draft payload = %+v; want entity/recordId/data", d)
	}
	if len(d.Before) != 1 || d.Before["priority"] != "low" {
		t.Fatalf("draft Before = %+v; want ONLY the touched field's current value", d.Before)
	}
}

// TestAssistantTools_ProposeRecordUpdate_Refusals: missing recordId / empty data are refused with
// honest codes BEFORE any reader call; an absent (or row-scope-hidden) record is RECORD_NOT_FOUND;
// blanking a required field via explicit null hits the partial-validator's RECORD_REQUIRED_MISSING
// (ValidatePartialRecord parity — not the full-record validator). Nothing ever buffers.
func TestAssistantTools_ProposeRecordUpdate_Refusals(t *testing.T) {
	t.Run("missing recordId", func(t *testing.T) {
		rd := &fakeRecordReader{fields: ticketFields()}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		res := e.execute(context.Background(), "propose_record_update", `{"entity":"tickets","data":{"priority":"high"}}`)
		if !strings.Contains(res, "RECORD_ID_REQUIRED") {
			t.Fatalf("error = %s; want RECORD_ID_REQUIRED", res)
		}
		if len(rd.gotScopes) != 0 {
			t.Fatal("no reader call may happen before the args are sane")
		}
	})
	t.Run("empty data", func(t *testing.T) {
		rd := &fakeRecordReader{fields: ticketFields()}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		res := e.execute(context.Background(), "propose_record_update", `{"entity":"tickets","recordId":"r-1","data":{}}`)
		if !strings.Contains(res, "RECORD_UPDATE_EMPTY") {
			t.Fatalf("error = %s; want RECORD_UPDATE_EMPTY", res)
		}
		if len(rd.gotScopes) != 0 {
			t.Fatal("no reader call may happen for an empty change set")
		}
	})
	t.Run("record absent", func(t *testing.T) {
		rd := &fakeRecordReader{fields: ticketFields(), getRecErr: services.ErrRecordNotFound}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		res := e.execute(context.Background(), "propose_record_update", `{"entity":"tickets","recordId":"ghost","data":{"priority":"high"}}`)
		if !strings.Contains(res, "RECORD_NOT_FOUND") {
			t.Fatalf("error = %s; want RECORD_NOT_FOUND", res)
		}
		if len(e.draft) != 0 {
			t.Fatalf("draft must stay empty; got %+v", e.draft)
		}
	})
	t.Run("null on required field", func(t *testing.T) {
		rd := &fakeRecordReader{fields: ticketFields(), getRec: &dynrepo.Record{ID: "r-1", Data: map[string]any{}}}
		e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}
		res := e.execute(context.Background(), "propose_record_update", `{"entity":"tickets","recordId":"r-1","data":{"title":null}}`)
		if !strings.Contains(res, "RECORD_REQUIRED_MISSING") {
			t.Fatalf("error = %s; want RECORD_REQUIRED_MISSING (partial-validator parity)", res)
		}
		if len(e.draft) != 0 {
			t.Fatalf("draft must stay empty; got %+v", e.draft)
		}
	})
}
