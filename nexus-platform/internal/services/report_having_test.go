package services

// Unit tests for the threshold of a saved Summary / Matrix report (specs/049-report-grouping-pivot,
// slice S5 report-having): the config key `having` is validated by the ONE shape body every write
// path shares (T059 — admin API, vertical pack, AI Builder via ValidateReportConfig), a report that
// carries it runs through the grid engine with the threshold handed down as dynrepo.GridHaving (a
// one-level summary included — that is the only way its totals can add the kept groups only), the
// run echoes the threshold it applied, and a malformed threshold reaching RecordService.AggregateGrid
// directly is refused before the store. The SQL (CTE `_kept` joined back NULL-safely) is pinned by
// dynrepo TestAggregateGrid_ArgOrder / TestAggregateGrid_SQLShape and proven on MySQL by
// handlers/report_grid_integration_test.go (TestReportHaving_*).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// TestValidateReportConfig_Having is T059: `on` ∈ {value,count}; `op` ∈ {gt,gte,lt,lte,eq,neq,
// between}; exactly 1 value (2 for between); each value a decimal of ≤ 28 integer + ≤ 10 fraction
// digits (DECIMAL(38,10)); between's bounds ordered by exact decimal comparison; a tabular report
// cannot carry it. Every shape error is 422 REPORT_HAVING_INVALID {field:"having", reason} — the
// same with and without live fields (apply vs the AI Builder's deep mode).
func TestValidateReportConfig_Having(t *testing.T) {
	live := []metadata.FieldDefinition{
		{Name: "stage", FieldType: "picklist", Options: strPtr(`{"indexed":true}`)},
		{Name: "close_date", FieldType: "date", Options: strPtr(`{"indexed":true}`)},
		{Name: "amount", FieldType: "number"},
	}
	h := func(on, op string, vals ...string) *ReportHavingInput {
		return &ReportHavingInput{On: on, Op: op, Values: vals}
	}
	summary := func(hv *ReportHavingInput) ReportConfig {
		return ReportConfig{GroupBy: "stage", Metric: "sum", Field: "amount", Having: hv}
	}
	cases := []struct {
		name       string
		typ        string
		cfg        ReportConfig
		wantCode   string // "" = valid
		wantReason string // details.reason of a REPORT_HAVING_INVALID
	}{
		{name: "summary value > 500", typ: "summary", cfg: summary(h("value", "gt", "500"))},
		{name: "matrix count between 2 and 5", typ: "matrix",
			cfg: ReportConfig{GroupBy: "stage", Metric: "count", Having: h("count", "between", "2", "5"),
				ColumnGroup: &ReportGroupInput{Field: "close_date", Bucket: "month"}}},
		{name: "matrix without a column axis may carry it", typ: "matrix", cfg: summary(h("value", "lte", "-5"))},
		{name: "summary with sub-levels", typ: "summary",
			cfg: ReportConfig{GroupBy: "stage", Metric: "avg", Field: "amount", Having: h("value", "neq", "0"),
				SubGroups: []ReportGroupInput{{Field: "close_date", Bucket: "month"}}}},
		{name: "every operator token", typ: "summary", cfg: summary(h("value", "eq", "0.5"))},
		{name: "between with equal bounds", typ: "summary", cfg: summary(h("value", "between", "10", "10.0"))},
		{name: "widest decimal (28 + 10 digits)", typ: "summary",
			cfg: summary(h("value", "gte", "-"+strings.Repeat("9", 28)+"."+strings.Repeat("9", 10)))},

		{name: "tabular cannot carry a threshold", typ: "tabular",
			cfg: ReportConfig{Columns: []string{"stage"}, Having: h("value", "gt", "1")}, wantCode: "REPORT_CONFIG_INVALID"},
		{name: "unknown subject", typ: "summary", cfg: summary(h("sum", "gt", "1")), wantCode: "REPORT_HAVING_INVALID", wantReason: "on"},
		{name: "empty subject", typ: "summary", cfg: summary(h("", "gt", "1")), wantCode: "REPORT_HAVING_INVALID", wantReason: "on"},
		{name: "raw SQL operator", typ: "summary", cfg: summary(h("value", "> 0 OR 1=1 --", "1")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "op"},
		{name: "list operator", typ: "summary", cfg: summary(h("value", "in", "1")), wantCode: "REPORT_HAVING_INVALID", wantReason: "op"},
		{name: "no value", typ: "summary", cfg: summary(h("value", "gt")), wantCode: "REPORT_HAVING_INVALID", wantReason: "values"},
		{name: "two values for gt", typ: "summary", cfg: summary(h("value", "gt", "1", "2")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "values"},
		{name: "one value for between", typ: "summary", cfg: summary(h("count", "between", "1")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "values"},
		{name: "not a number", typ: "summary", cfg: summary(h("value", "gt", "abc")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "empty value", typ: "summary", cfg: summary(h("value", "gt", " ")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "exponent", typ: "summary", cfg: summary(h("value", "gt", "1e5")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "leading plus", typ: "summary", cfg: summary(h("value", "gt", "+1")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "bare fraction", typ: "summary", cfg: summary(h("value", "gt", ".5")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "trailing dot", typ: "summary", cfg: summary(h("value", "gt", "1.")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "thousands separator", typ: "summary", cfg: summary(h("value", "gt", "1,000")), wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "29 integer digits", typ: "summary", cfg: summary(h("value", "gt", strings.Repeat("9", 29))),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "11 fraction digits", typ: "summary", cfg: summary(h("value", "gt", "0."+strings.Repeat("1", 11))),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		{name: "second bound not a number", typ: "summary", cfg: summary(h("value", "between", "1", "x")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "value"},
		// Exact decimal order: a float or text comparison would call these ordered.
		{name: "between lower > upper", typ: "summary", cfg: summary(h("value", "between", "10", "9.5")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "range"},
		{name: "between negative bounds reversed", typ: "summary", cfg: summary(h("value", "between", "-1", "-2")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "range"},
		{name: "between past float precision", typ: "summary",
			cfg:      summary(h("value", "between", "1234567890123456789.0000000002", "1234567890123456789.0000000001")),
			wantCode: "REPORT_HAVING_INVALID", wantReason: "range"},
	}
	for _, tt := range cases {
		for _, mode := range []struct {
			name string
			live []metadata.FieldDefinition
		}{{"apply", nil}, {"deep (AI Builder)", live}} {
			t.Run(tt.name+"/"+mode.name, func(t *testing.T) {
				_, stored, err := ValidateReportConfig(tt.typ, tt.cfg, mode.live)
				if tt.wantCode == "" {
					if err != nil {
						t.Fatalf("want valid, got %v", err)
					}
					var back ReportConfig
					if err := json.Unmarshal(stored, &back); err != nil {
						t.Fatalf("stored config: %v", err)
					}
					if back.Having == nil || len(back.Having.Values) != len(tt.cfg.Having.Values) {
						t.Fatalf("stored config %s dropped the threshold", stored)
					}
					return
				}
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v, want %s", err, tt.wantCode)
				}
				if tt.wantReason != "" {
					if want := map[string]any{"field": "having", "reason": tt.wantReason}; !reflect.DeepEqual(ve.Details, want) {
						t.Fatalf("details = %v, want %v", ve.Details, want)
					}
					// The message is for people: no config token, no raw SQL echoed back.
					if strings.Contains(ve.Msg, "OR 1=1") {
						t.Fatalf("message echoes the raw operator: %q", ve.Msg)
					}
				}
			})
		}
	}

	t.Run("normalized on save: lower-case subject and operator, trimmed values", func(t *testing.T) {
		_, stored, err := ValidateReportConfig("summary", summary(h(" Value ", " BETWEEN ", " 1.50 ", "2 ")), nil)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var back ReportConfig
		_ = json.Unmarshal(stored, &back)
		if want := h("value", "between", "1.50", "2"); !reflect.DeepEqual(back.Having, want) {
			t.Fatalf("stored threshold = %+v, want %+v", back.Having, want)
		}
	})

	t.Run("a report without a threshold stores no having key", func(t *testing.T) {
		_, stored, err := ValidateReportConfig("summary", summary(nil), nil)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var raw map[string]any
		_ = json.Unmarshal(stored, &raw)
		if _, ok := raw["having"]; ok {
			t.Fatalf("stored %s carries having — a report without one must stay byte-identical", stored)
		}
	})
}

// TestReportRun_HavingRunsTheGrid is the unit half of T066: a saved threshold makes the run take the
// grid engine — a one-level summary (and a matrix without a column axis) as a one-level tree, so its
// total adds only the kept groups; a matrix with a column axis keeps its column totals — with the
// threshold handed to the store as GridHaving, and echoed on the result.
func TestReportRun_HavingRunsTheGrid(t *testing.T) {
	rows := []dynrepo.GridRow{gr(sp("900"), 3, "*"), gr(sp("900"), 3, "Alpha")}
	cases := []struct {
		name, typ, cfg string
		wantAxes       []string
		wantCross      bool
		wantType       report.Type
		wantHaving     dynrepo.GridHaving
		wantEcho       ReportHavingInput
	}{
		{name: "one-level summary, value threshold", typ: "summary",
			cfg:      `{"groupBy":"stage","metric":"sum","field":"amount","having":{"on":"value","op":"gt","values":["500"]}}`,
			wantAxes: []string{"stage"}, wantType: report.TypeSummary,
			wantHaving: dynrepo.GridHaving{Op: "gt", Values: []string{"500"}},
			wantEcho:   ReportHavingInput{On: "value", Op: "gt", Values: []string{"500"}}},
		{name: "matrix without a column axis runs as a one-level tree", typ: "matrix",
			cfg:      `{"groupBy":"stage","metric":"count","having":{"on":"count","op":"between","values":["2","5"]}}`,
			wantAxes: []string{"stage"}, wantType: report.TypeSummary,
			wantHaving: dynrepo.GridHaving{OnCount: true, Op: "between", Values: []string{"2", "5"}},
			wantEcho:   ReportHavingInput{On: "count", Op: "between", Values: []string{"2", "5"}}},
		{name: "summary with a sub-level", typ: "summary",
			cfg:      `{"groupBy":"stage","metric":"sum","field":"amount","subGroups":[{"field":"region"}],"having":{"on":"value","op":"lte","values":["-5"]}}`,
			wantAxes: []string{"stage", "region"}, wantType: report.TypeSummary,
			wantHaving: dynrepo.GridHaving{Op: "lte", Values: []string{"-5"}},
			wantEcho:   ReportHavingInput{On: "value", Op: "lte", Values: []string{"-5"}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store := &rjStore{gridRows: rows}
			res, err := matrixReportService(tt.typ, tt.cfg, store, nil).RunReport(context.Background(), gridRep, "t1", "rep1", nil)
			if err != nil {
				t.Fatalf("RunReport: %v", err)
			}
			if store.groupedCalled || !store.gridCalled {
				t.Fatalf("grouped=%v grid=%v, want the grid engine only", store.groupedCalled, store.gridCalled)
			}
			var got []string
			for _, ax := range store.gridParams.Axes {
				got = append(got, ax.Physical)
			}
			if !reflect.DeepEqual(got, tt.wantAxes) || store.gridParams.CrossTotals != tt.wantCross {
				t.Fatalf("axes = %v cross = %v, want %v cross = %v", got, store.gridParams.CrossTotals, tt.wantAxes, tt.wantCross)
			}
			if store.gridParams.Having == nil || !reflect.DeepEqual(*store.gridParams.Having, tt.wantHaving) {
				t.Fatalf("store having = %+v, want %+v", store.gridParams.Having, tt.wantHaving)
			}
			if res.Type != tt.wantType || res.Grid == nil || res.Grid.Kind != GridKindTree {
				t.Fatalf("result = %+v, want a %s tree", res, tt.wantType)
			}
			if res.Having == nil || !reflect.DeepEqual(*res.Having, tt.wantEcho) {
				t.Fatalf("echoed having = %+v, want %+v", res.Having, tt.wantEcho)
			}
		})
	}

	t.Run("a one-level summary with a threshold exports the one-level tree, total included", func(t *testing.T) {
		store := &rjStore{gridRows: rows}
		var buf bytes.Buffer
		err := matrixReportService("summary", cases[0].cfg, store, nil).
			ExportReportCSV(context.Background(), gridRep, "t1", "rep1", nil, &buf)
		if err != nil {
			t.Fatalf("ExportReportCSV: %v", err)
		}
		if got, want := buf.String(), utf8BOM+"stage,Giá trị,Số lượng\nAlpha,900,3\nTổng cộng,900,3\n"; got != want {
			t.Fatalf("export =\n%q\nwant\n%q (not the key,value,count file of the whole set)", got, want)
		}
	})

	t.Run("matrix with a column axis keeps its column totals", func(t *testing.T) {
		store := &rjStore{
			gridRows: []dynrepo.GridRow{gr(sp("20"), 1, "*", "*"), gr(sp("20"), 1, "Won", "2026-02"), gr(sp("20"), 1, "Won", "*")},
			gridCols: []dynrepo.GridRow{gr(sp("20"), 1, "2026-02")},
		}
		res, err := matrixReportService("matrix",
			`{"groupBy":"stage","metric":"sum","field":"amount","columnGroup":{"field":"close_date","bucket":"month"},"having":{"on":"value","op":"gt","values":["15"]}}`,
			store, nil).RunReport(context.Background(), gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if !store.gridParams.CrossTotals || store.gridParams.Having == nil || store.gridParams.Having.Op != "gt" ||
			res.Type != report.TypeMatrix || res.Grid.Kind != GridKindMatrix || res.Having == nil || res.Having.Values[0] != "15" {
			t.Fatalf("params = %+v result = %+v", store.gridParams, res)
		}
	})

	t.Run("no threshold: the grid gets none and the result echoes none", func(t *testing.T) {
		store := &rjStore{gridRows: rows}
		res, err := matrixReportService("summary",
			`{"groupBy":"stage","metric":"count","subGroups":[{"field":"region"}]}`, store, nil).
			RunReport(context.Background(), gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if store.gridParams.Having != nil || res.Having != nil {
			t.Fatalf("store having = %+v echo = %+v, want none", store.gridParams.Having, res.Having)
		}
	})
}

// TestAggregateGrid_MalformedHavingRefusedBeforeStore: RecordService.AggregateGrid is a service entry
// point of its own (the assistant path reaches it too), so a threshold that never went through the
// report validator — a config written before a validator change, a caller building GridQuery by
// hand — is judged by the SAME shape body before any query. A non-decimal bound would otherwise be
// CAST to 0 by MySQL and silently filter on the wrong number.
func TestAggregateGrid_MalformedHavingRefusedBeforeStore(t *testing.T) {
	for name, hv := range map[string]*ReportHavingInput{
		"not a number":   {On: "value", Op: "gt", Values: []string{"abc"}},
		"unknown op":     {On: "value", Op: "like", Values: []string{"1"}},
		"reversed range": {On: "count", Op: "between", Values: []string{"5", "2"}},
	} {
		t.Run(name, func(t *testing.T) {
			store := &rjStore{}
			_, err := buildGridService(store, nil).AggregateGrid(context.Background(), gridRep, "t1", "tickets",
				GridQuery{Metric: "count", Axes: axes("region"), Having: hv})
			if codeOf(err) != "REPORT_HAVING_INVALID" || store.gridCalled {
				t.Fatalf("err = %v gridCalled = %v, want REPORT_HAVING_INVALID before the store", err, store.gridCalled)
			}
		})
	}

	t.Run("a valid threshold is normalized on the way down", func(t *testing.T) {
		store := &rjStore{gridRows: []dynrepo.GridRow{}}
		_, err := buildGridService(store, nil).AggregateGrid(context.Background(), gridRep, "t1", "tickets",
			GridQuery{Metric: "count", Axes: axes("region"), Having: &ReportHavingInput{On: " COUNT", Op: "Gte ", Values: []string{" 2 "}}})
		if err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if want := (dynrepo.GridHaving{OnCount: true, Op: "gte", Values: []string{"2"}}); store.gridParams.Having == nil ||
			!reflect.DeepEqual(*store.gridParams.Having, want) {
			t.Fatalf("store having = %+v, want %+v", store.gridParams.Having, want)
		}
	})
}
