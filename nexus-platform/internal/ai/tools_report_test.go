package ai

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
)

// twoTenantReportReader extends the M5/M6 fixture with the field surface the M8 report tool needs
// on tenant-A's treatments (eA2, which already carries title text + patient relation[indexed]):
//   - price      number   (numeric, NOT indexed → valid sum target, invalid groupBy)
//   - status     picklist (NOT indexed → AGGREGATE_REQUIRES_INDEXED groupBy probe; filterable)
//   - visit_date date     (indexed → valid date groupBy + bucket target)
//
// tenant-B: invoices (total currency) — the cross-tenant probe.
func twoTenantReportReader() *fakeMetaReader {
	r := twoTenantRelReader()
	r.fields["tenant-A"]["eA2"] = append(r.fields["tenant-A"]["eA2"],
		metadata.FieldDefinition{Name: "price", Label: "Giá", FieldType: "number"},
		metadata.FieldDefinition{Name: "status", Label: "Trạng thái", FieldType: "picklist",
			Options: strPtr(`{"values":["open","done"]}`)},
		metadata.FieldDefinition{Name: "visit_date", Label: "Ngày khám", FieldType: "date",
			Options: strPtr(`{"indexed":true}`)},
	)
	return r
}

func TestTools_ProposeReport_ValidSummary_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{
		"name": "Ca điều trị theo tháng",
		"entity": "treatments",
		"reportType": "summary",
		"groupBy": "created_at",
		"metric": "count",
		"bucket": "month",
		"filters": [{"field": "status", "op": "eq", "values": ["done"]}]
	}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", args))
	if !ok {
		t.Fatalf("valid propose_report should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "report" || e.draft[0].Report == nil {
		t.Fatalf("expected 1 buffered report draft, got %+v", e.draft)
	}
	r := e.draft[0].Report
	if r.Name != "Ca điều trị theo tháng" || r.EntityName != "treatments" || r.Type != "summary" {
		t.Fatalf("buffered report wrong shape: %+v", r)
	}
	if r.Config.GroupBy != "created_at" || r.Config.Metric != "count" || r.Config.Bucket != "month" {
		t.Fatalf("buffered report config wrong: %+v", r.Config)
	}
	if len(r.Config.Filters) != 1 || r.Config.Filters[0].Field != "status" {
		t.Fatalf("buffered report must carry the filters verbatim: %+v", r.Config.Filters)
	}
	if !r.IsActive {
		t.Fatalf("proposed report must default IsActive=true: %+v", r)
	}
	if e.draft[0].Entity != nil || e.draft[0].Field != nil || e.draft[0].Relationship != nil ||
		e.draft[0].Layout != nil || e.draft[0].Workflow != nil {
		t.Fatalf("report draft must not carry other payloads: %+v", e.draft[0])
	}
}

// TestTools_ProposeReport_ValidVariants proves the other proposable shapes buffer: a SUM over a
// numeric field grouped by an indexed (relation) field, a bucket over a declared date field (not
// just created_at), and a tabular projection.
func TestTools_ProposeReport_ValidVariants(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantType string
	}{
		{
			name: "sum(price) by relation groupBy",
			args: `{"name":"Doanh thu theo bệnh nhân","entity":"treatments","reportType":"summary",
				"groupBy":"patient","metric":"sum","field":"price"}`,
			wantType: "summary",
		},
		{
			name: "count by declared date field with bucket",
			args: `{"name":"Ca khám theo tuần","entity":"treatments","reportType":"matrix",
				"groupBy":"visit_date","metric":"count","bucket":"week"}`,
			wantType: "matrix",
		},
		{
			name:     "tabular projection",
			args:     `{"name":"Danh sách điều trị","entity":"treatments","reportType":"tabular","columns":["title","status"]}`,
			wantType: "tabular",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", tt.args))
			if !ok {
				t.Fatalf("valid propose_report should succeed: %+v", m)
			}
			if len(e.draft) != 1 || e.draft[0].Kind != "report" || e.draft[0].Report == nil {
				t.Fatalf("expected 1 buffered report draft, got %+v", e.draft)
			}
			if e.draft[0].Report.Type != tt.wantType {
				t.Fatalf("expected type %q, got %+v", tt.wantType, e.draft[0].Report)
			}
		})
	}
}

// TestTools_ProposeReport_Invalid drives the real validator (services.ValidateReportConfig — the
// SAME body CreateReport runs, with the deep aggregate/filter checks armed by liveFields) through
// the tool: each violation surfaces its actual code for model self-correction and buffers nothing.
func TestTools_ProposeReport_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{
			name:     "empty name",
			args:     `{"name":"  ","entity":"treatments","reportType":"summary","groupBy":"created_at","metric":"count"}`,
			wantCode: "REPORT_NAME_REQUIRED",
		},
		{
			name:     "unknown reportType",
			args:     `{"name":"r","entity":"treatments","reportType":"pivot","groupBy":"created_at","metric":"count"}`,
			wantCode: "REPORT_TYPE_INVALID",
		},
		{
			name:     "summary without groupBy",
			args:     `{"name":"r","entity":"treatments","reportType":"summary","metric":"count"}`,
			wantCode: "REPORT_CONFIG_INVALID",
		},
		{
			name:     "unknown metric",
			args:     `{"name":"r","entity":"treatments","reportType":"summary","groupBy":"created_at","metric":"median"}`,
			wantCode: "REPORT_CONFIG_INVALID",
		},
		{
			// Deep check (run-time code at propose time): id IS a groupable system column but
			// carries no date — the bucket must be rejected exactly like a run would reject it.
			name:     "bucket on a non-date groupBy",
			args:     `{"name":"r","entity":"treatments","reportType":"summary","groupBy":"id","metric":"count","bucket":"month"}`,
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE",
		},
		{
			name:     "bucket on tabular",
			args:     `{"name":"r","entity":"treatments","reportType":"tabular","columns":["title"],"bucket":"month"}`,
			wantCode: "REPORT_CONFIG_INVALID",
		},
		{
			name:     "tabular without columns",
			args:     `{"name":"r","entity":"treatments","reportType":"tabular"}`,
			wantCode: "REPORT_CONFIG_INVALID",
		},
		{
			name:     "sum over a nonexistent field",
			args:     `{"name":"r","entity":"treatments","reportType":"summary","groupBy":"created_at","metric":"sum","field":"ghost"}`,
			wantCode: "AGGREGATE_FIELD_UNKNOWN",
		},
		{
			name:     "sum over a non-numeric field",
			args:     `{"name":"r","entity":"treatments","reportType":"summary","groupBy":"created_at","metric":"sum","field":"title"}`,
			wantCode: "AGGREGATE_FIELD_NOT_NUMERIC",
		},
		{
			name:     "groupBy on a non-indexed field",
			args:     `{"name":"r","entity":"treatments","reportType":"summary","groupBy":"status","metric":"count"}`,
			wantCode: "AGGREGATE_REQUIRES_INDEXED",
		},
		{
			name: "filter on a nonexistent field",
			args: `{"name":"r","entity":"treatments","reportType":"summary","groupBy":"created_at","metric":"count",
				"filters":[{"field":"ghost","op":"eq","values":["x"]}]}`,
			wantCode: "LIST_UNKNOWN_FILTER",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", tt.args))
			if ok {
				t.Fatalf("invalid report must NOT succeed: %+v", m)
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}
}

// TestTools_ProposeReport_BadName_Rejected pins the REPORT_NAME_TOO_LONG guard (MED-1 fix): the
// sys_reports.name column is VARCHAR(150) — a name over that byte budget must never buffer, or the
// draft would 500 at Apply (MySQL 1406 "Data too long"), breaking proposeReport's own
// "no DB-dependent reject left for Apply" contract. Mirrors M5's maxRelationshipNameBytes test.
func TestTools_ProposeReport_BadName_Rejected(t *testing.T) {
	base := `"entity":"treatments","reportType":"summary","groupBy":"created_at","metric":"count"`

	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"empty name", `{"name":"",` + base + `}`, "REPORT_NAME_REQUIRED"},
		{"whitespace name", `{"name":"   ",` + base + `}`, "REPORT_NAME_REQUIRED"},
		{"over 150 bytes", `{"name":"` + strings.Repeat("x", 151) + `",` + base + `}`, "REPORT_NAME_TOO_LONG"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", tt.args))
			if ok {
				t.Fatal("bad report name must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}

	// Boundary: the check is `len(name) > maxReportNameBytes`, so a name of EXACTLY 150 bytes must
	// be ACCEPTED (off-by-one regression guard: a `>=` typo here would silently reject the longest
	// legal name).
	t.Run("exactly 150 bytes accepted", func(t *testing.T) {
		e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
		name150 := strings.Repeat("x", maxReportNameBytes)
		if len(name150) != maxReportNameBytes {
			t.Fatalf("fixture bug: name150 is %d bytes, want %d", len(name150), maxReportNameBytes)
		}
		args := `{"name":"` + name150 + `",` + base + `}`
		ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", args))
		if !ok {
			t.Fatalf("a name of exactly %d bytes must be accepted, got %+v", maxReportNameBytes, m)
		}
		if len(e.draft) != 1 {
			t.Fatalf("expected 1 buffered report draft, got %+v", e.draft)
		}
	})
}

func TestTools_ProposeReport_TenantScope(t *testing.T) {
	// A perfectly valid report aimed at ANOTHER tenant's entity must be indistinguishable from
	// aiming at a missing entity — never validated against the other tenant's fields.
	e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"name":"r","entity":"invoices","reportType":"summary","groupBy":"created_at","metric":"count"}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", args))
	if ok {
		t.Fatal("propose_report on a cross-tenant entity must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected cross-tenant proposal must not buffer, got %+v", e.draft)
	}
}
