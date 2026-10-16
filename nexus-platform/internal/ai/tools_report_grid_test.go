package ai

import (
	"context"
	"strings"
	"testing"
)

// specs/049 S7a (T077): propose_report carries the three grid keys and deep-validates them with
// the SAME body the save/run path uses.
func TestProposeReport_GridKeysDeepValidated(t *testing.T) {
	const base = `"name":"Doanh thu","entity":"treatments","metric":"sum","field":"price","groupBy":"created_at","bucket":"month"`
	tests := []struct {
		name    string
		args    string
		wantErr []string // all must appear in the error; nil = must buffer
		wantCfg func(c DraftReportConfig) bool
	}{
		{
			name: "valid matrix carries columnGroup+having",
			args: `{` + base + `,"reportType":"matrix","columnGroup":{"field":"visit_date","bucket":"MONTH"},"having":{"on":"VALUE","op":"GT","values":["10000"]}}`,
			wantCfg: func(c DraftReportConfig) bool {
				return c.ColumnGroup != nil && c.ColumnGroup.Field == "visit_date" && c.ColumnGroup.Bucket == "month" &&
					c.Having != nil && c.Having.On == "value" && c.Having.Op == "gt" && len(c.Having.Values) == 1
			},
		},
		{
			name: "valid summary carries subGroups",
			args: `{` + base + `,"reportType":"summary","subGroups":[{"field":"visit_date","bucket":"month"}]}`,
			wantCfg: func(c DraftReportConfig) bool {
				return len(c.SubGroups) == 1 && c.SubGroups[0].Field == "visit_date"
			},
		},
		{
			name:    "column axis not indexed names the axis",
			args:    `{` + base + `,"reportType":"matrix","columnGroup":{"field":"status"}}`,
			wantErr: []string{"AGGREGATE_REQUIRES_INDEXED", "columnGroup"},
		},
		{
			name:    "sub axis not indexed names the axis",
			args:    `{` + base + `,"reportType":"summary","subGroups":[{"field":"status"}]}`,
			wantErr: []string{"AGGREGATE_REQUIRES_INDEXED", "subGroups[0]"},
		},
		{
			name:    "bad having",
			args:    `{` + base + `,"reportType":"matrix","columnGroup":{"field":"visit_date"},"having":{"on":"value","op":"gt","values":["abc"]}}`,
			wantErr: []string{"REPORT_HAVING_INVALID"},
		},
		{
			name:    "columnGroup on summary rejected",
			args:    `{` + base + `,"reportType":"summary","columnGroup":{"field":"visit_date"}}`,
			wantErr: []string{"REPORT_CONFIG_INVALID"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantReportReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_report", tc.args))
			if tc.wantErr != nil {
				if ok || len(e.draft) != 0 {
					t.Fatalf("want rejection, got ok=%v draft=%d", ok, len(e.draft))
				}
				msg, _ := m["error"].(string)
				for _, w := range tc.wantErr {
					if !strings.Contains(msg, w) {
						t.Fatalf("error %q must contain %q", msg, w)
					}
				}
				return
			}
			if !ok || len(e.draft) != 1 {
				t.Fatalf("want buffered draft, got ok=%v %+v", ok, m)
			}
			if !tc.wantCfg(e.draft[0].Report.Config) {
				t.Fatalf("draft config lost a grid key: %+v", e.draft[0].Report.Config)
			}
		})
	}
}

// FR-012 pin: without this sentence the model translates "keep only stages above X" into a
// per-cell threshold.
func TestProposeReport_SchemaGridKeys(t *testing.T) {
	for _, tool := range ToolDefinitions() {
		if tool.Function.Name != "propose_report" {
			continue
		}
		props := tool.Function.Parameters["properties"].(map[string]any)
		for _, k := range []string{"subGroups", "columnGroup", "having"} {
			if _, ok := props[k]; !ok {
				t.Fatalf("propose_report schema missing %q", k)
			}
		}
		d := props["having"].(map[string]any)["description"].(string)
		if !strings.Contains(d, "lọc từng ô / nhóm cấp sâu nhất, không lọc tổng hàng") {
			t.Fatalf("having description must pin FR-012, got %q", d)
		}
		rt := props["reportType"].(map[string]any)["description"].(string)
		if strings.Contains(rt, "hiện chạy như summary một trục") {
			t.Fatal("stale 'matrix chạy như summary một trục' text still in schema")
		}
		return
	}
	t.Fatal("propose_report not found")
}
