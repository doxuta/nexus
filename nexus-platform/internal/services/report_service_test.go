package services

// Unit tests for the matrix column axis of a saved report (specs/049-report-grouping-pivot, slice
// S2b report-matrix-run): the config key `columnGroup` is validated by the ONE shape body every
// write path shares (T027), a matrix that carries it runs through RecordService.AggregateGrid and
// comes back as a cross-tab, a matrix without it runs the single-axis path exactly as before, every
// hidden-axis route is refused before the store (T029, service half — the base-field half runs on
// MySQL through the production router in handlers/report_grid_integration_test.go), and — slice S4
// report-grid-csv — a grid exports as the grid on screen (T054), any error still before its first
// byte.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// TestValidateReportConfig_ColumnGroup is T027: the column axis belongs to a matrix only, must not
// repeat the row axis (same field AND same bucket), takes the run-time bucket set, must name the
// configured join when namespaced, and — in deep mode (the AI Builder's propose_report) — is
// resolved by the same axis body the run executes, the error naming the broken axis.
func TestValidateReportConfig_ColumnGroup(t *testing.T) {
	live := []metadata.FieldDefinition{
		{Name: "stage", FieldType: "picklist", Options: strPtr(`{"indexed":true}`)},
		{Name: "close_date", FieldType: "date", Options: strPtr(`{"indexed":true}`)},
		{Name: "region", FieldType: "text", Options: strPtr(`{"indexed":true}`)},
		{Name: "note", FieldType: "text"},
		{Name: "amount", FieldType: "number"},
	}
	cg := func(field, bucket string) *ReportGroupInput { return &ReportGroupInput{Field: field, Bucket: bucket} }
	matrix := func(groupBy, bucket string, col *ReportGroupInput) ReportConfig {
		return ReportConfig{GroupBy: groupBy, Bucket: bucket, Metric: "sum", Field: "amount", ColumnGroup: col}
	}
	cases := []struct {
		name     string
		typ      string
		cfg      ReportConfig
		live     []metadata.FieldDefinition
		wantCode string // "" = valid
		wantAxis string // details.axis the error must carry ("" = not checked)
	}{
		{name: "matrix stage × close_date(month)", typ: "matrix", cfg: matrix("stage", "", cg("close_date", "month"))},
		{name: "deep: same, against live fields", typ: "matrix", cfg: matrix("stage", "", cg("close_date", "month")), live: live},
		{name: "same field, other bucket is two axes", typ: "matrix", cfg: matrix("close_date", "year", cg("close_date", "month"))},
		{name: "same field, same bucket repeats the row axis", typ: "matrix",
			cfg: matrix("close_date", "month", cg("close_date", "MONTH")), wantCode: "REPORT_CONFIG_INVALID"},
		{name: "same field, no bucket repeats the row axis", typ: "matrix",
			cfg: matrix("stage", "", cg(" stage ", "")), wantCode: "REPORT_CONFIG_INVALID"},
		{name: "unknown bucket", typ: "matrix", cfg: matrix("stage", "", cg("close_date", "fortnight")), wantCode: "REPORT_CONFIG_INVALID"},
		{name: "empty field", typ: "matrix", cfg: matrix("stage", "", cg("  ", "")), wantCode: "REPORT_CONFIG_INVALID"},
		{name: "namespaced axis on another relationship", typ: "matrix",
			cfg: ReportConfig{GroupBy: "stage", Metric: "count", Joins: []ReportJoinInput{{Relationship: "account"}},
				ColumnGroup: cg("owner_rel.industry", "")},
			wantCode: "REPORT_JOIN_ENTITY_MISMATCH"},
		{name: "namespaced axis on the configured relationship", typ: "matrix",
			cfg: ReportConfig{GroupBy: "stage", Metric: "count", Joins: []ReportJoinInput{{Relationship: "account"}},
				ColumnGroup: cg("account.industry", "")}},
		{name: "summary cannot carry a column axis", typ: "summary", cfg: matrix("stage", "", cg("close_date", "month")),
			wantCode: "REPORT_CONFIG_INVALID"},
		{name: "tabular cannot carry a column axis", typ: "tabular",
			cfg: ReportConfig{Columns: []string{"stage"}, ColumnGroup: cg("close_date", "month")}, wantCode: "REPORT_CONFIG_INVALID"},
		{name: "deep: column axis not indexed", typ: "matrix", cfg: matrix("stage", "", cg("note", "")), live: live,
			wantCode: "AGGREGATE_REQUIRES_INDEXED", wantAxis: "columnGroup"},
		{name: "deep: column bucket over a non-date axis", typ: "matrix", cfg: matrix("stage", "", cg("region", "month")), live: live,
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE", wantAxis: "columnGroup"},
		{name: "deep: row axis not indexed names groupBy", typ: "matrix", cfg: matrix("note", "", cg("stage", "")), live: live,
			wantCode: "AGGREGATE_REQUIRES_INDEXED", wantAxis: "groupBy"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, stored, err := ValidateReportConfig(tt.typ, tt.cfg, tt.live)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				var back ReportConfig
				if err := json.Unmarshal(stored, &back); err != nil {
					t.Fatalf("stored config: %v", err)
				}
				if back.ColumnGroup == nil || back.ColumnGroup.Field != tt.cfg.ColumnGroup.Field {
					t.Fatalf("stored config %s dropped the column axis", stored)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
			if tt.wantAxis != "" && ve.Details["axis"] != tt.wantAxis {
				t.Fatalf("details = %v, want axis %q", ve.Details, tt.wantAxis)
			}
		})
	}

	t.Run("normalized on save: trimmed field, lower-case bucket", func(t *testing.T) {
		_, stored, err := ValidateReportConfig("matrix", matrix("stage", "", cg(" close_date ", " Month ")), nil)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var back ReportConfig
		_ = json.Unmarshal(stored, &back)
		if *back.ColumnGroup != (ReportGroupInput{Field: "close_date", Bucket: "month"}) {
			t.Fatalf("stored column axis = %+v, want close_date/month", *back.ColumnGroup)
		}
	})

	t.Run("a matrix without a column axis stores no columnGroup key", func(t *testing.T) {
		_, stored, err := ValidateReportConfig("matrix", matrix("stage", "", nil), nil)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var raw map[string]any
		_ = json.Unmarshal(stored, &raw)
		if _, ok := raw["columnGroup"]; ok {
			t.Fatalf("stored %s carries columnGroup — legacy matrices must stay byte-identical", stored)
		}
	})
}

// matrixReportService runs one saved report of the grid fixtures (tickets; FK hop "assignee" to
// employees; polymorphic "about") for an employee whose role may view all three entities.
func matrixReportService(typ, cfg string, store *rjStore, fACL *rjFieldACL) *ReportService {
	records := buildGridService(store, fACL).
		WithACLAuthorizer(&rjAuthz{allow: map[string]bool{"tickets": true, "employees": true, "tags": true}})
	return NewReportService(&fakeReportRepo{rec: report.Record{
		ID: "rep1", TenantID: "t1", Name: "M", EntityName: "tickets", Type: typ,
		Config: json.RawMessage(cfg), IsActive: true,
	}}, records)
}

// TestReportRun_MatrixColumnGroupRunsTheGrid: the saved column axis reaches the grid engine as the
// second axis with column totals asked for, and the run comes back as a matrix with both axes.
func TestReportRun_MatrixColumnGroupRunsTheGrid(t *testing.T) {
	store := &rjStore{
		gridRows: []dynrepo.GridRow{
			gr(sp("30"), 2, "*", "*"),
			gr(sp("10"), 1, "Proposal", "2026-01"),
			gr(sp("10"), 1, "Proposal", "*"),
			gr(sp("20"), 1, "Won", "2026-02"),
			gr(sp("20"), 1, "Won", "*"),
		},
		gridCols: []dynrepo.GridRow{gr(sp("10"), 1, "2026-01"), gr(sp("20"), 1, "2026-02")},
	}
	svc := matrixReportService("matrix",
		`{"groupBy":"stage","metric":"sum","field":"amount","columnGroup":{"field":"close_date","bucket":"month"}}`, store, nil)
	res, err := svc.RunReport(context.Background(), gridRep, "t1", "rep1", nil)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if store.groupedCalled || !store.gridCalled {
		t.Fatalf("grouped=%v grid=%v, want the grid engine only", store.groupedCalled, store.gridCalled)
	}
	if !store.gridParams.CrossTotals || len(store.gridParams.Axes) != 2 ||
		store.gridParams.Axes[0].Physical != "stage" || store.gridParams.Axes[1].Physical != "close_date" ||
		store.gridParams.Axes[1].Bucket != dynrepo.BucketMonth {
		t.Fatalf("grid params = %+v, want stage × close_date(month) with column totals", store.gridParams)
	}
	if res.Type != report.TypeMatrix || res.Aggregate != nil || res.Grid == nil || res.Grid.Kind != GridKindMatrix {
		t.Fatalf("result = %+v, want a matrix grid", res)
	}
	want := []ReportGroupInput{{Field: "stage"}, {Field: "close_date", Bucket: "month"}}
	if !reflect.DeepEqual(res.Axes, want) {
		t.Fatalf("axes = %+v, want %+v", res.Axes, want)
	}
	if len(res.Grid.Rows) != 2 || len(res.Grid.Columns) != 2 || len(res.Grid.Cells) != 2 || valText(res.Grid.Total.Value) != "30" {
		t.Fatalf("grid = %+v", res.Grid)
	}
}

// TestReportRun_LegacyMatrixStaysSingleAxis is the unit half of T030: a matrix saved before the
// column axis existed runs the single-axis aggregate and reports itself as a summary, as before.
func TestReportRun_LegacyMatrixStaysSingleAxis(t *testing.T) {
	store := &rjStore{}
	svc := matrixReportService("matrix", `{"groupBy":"stage","metric":"count"}`, store, nil)
	res, err := svc.RunReport(context.Background(), gridRep, "t1", "rep1", nil)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if !store.groupedCalled || store.gridCalled {
		t.Fatalf("grouped=%v grid=%v, want the single-axis path only", store.groupedCalled, store.gridCalled)
	}
	if res.Type != report.TypeSummary || res.Aggregate == nil || res.Grid != nil || res.Axes != nil {
		t.Fatalf("result = %+v, want the unchanged summary shape", res)
	}
}

// TestReportRun_MatrixHiddenAxisForbidden is the service half of T029: a hidden field at either
// axis — base, joined (asked on the RELATED entity), or a polymorphic <f>_type — is refused 403
// before the store, by the report run path (not only by AggregateGrid called directly).
func TestReportRun_MatrixHiddenAxisForbidden(t *testing.T) {
	cases := []struct {
		name, hidden, cfg, wantAsked string
	}{
		{name: "hidden column axis", hidden: "tickets.secret",
			cfg: `{"groupBy":"stage","metric":"count","columnGroup":{"field":"secret"}}`},
		{name: "hidden row axis", hidden: "tickets.secret",
			cfg: `{"groupBy":"secret","metric":"count","columnGroup":{"field":"region"}}`},
		{name: "hidden joined column axis", hidden: "employees.level", wantAsked: "employees.level",
			cfg: `{"groupBy":"stage","metric":"count","joins":[{"relationship":"assignee"}],"columnGroup":{"field":"assignee.level"}}`},
		{name: "hidden polymorphic column axis", hidden: "tickets.about", wantAsked: "tickets.about",
			cfg: `{"groupBy":"stage","metric":"count","columnGroup":{"field":"about_type"}}`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store := &rjStore{}
			fACL := &rjFieldACL{hidden: map[string]bool{tt.hidden: true}}
			_, err := matrixReportService("matrix", tt.cfg, store, fACL).RunReport(context.Background(), gridRep, "t1", "rep1", nil)
			if codeOf(err) != "FIELD_FORBIDDEN" {
				t.Fatalf("code = %q, want FIELD_FORBIDDEN (err=%v)", codeOf(err), err)
			}
			if store.gridCalled || store.groupedCalled || store.scalarCalled {
				t.Fatalf("store reached although an axis is hidden")
			}
			if tt.wantAsked != "" {
				asked := false
				for _, k := range fACL.asked {
					asked = asked || k == tt.wantAsked
				}
				if !asked {
					t.Fatalf("field-ACL never asked about %s; asked=%v", tt.wantAsked, fACL.asked)
				}
			}
		})
	}
}

// TestReportExport_GridWritesTheGridCSV replaces the unit half of T032 (specs/049 S4): exporting a
// matrix that runs as a grid writes the cross-tab file — headings from the axis fields (no label in
// this fixture, so the field name), each row with its cells and row total, the "Tổng" line — never
// the three-column file of the single-axis path (#51). A grid error still leaves nothing written,
// and a legacy matrix still exports as before.
func TestReportExport_GridWritesTheGridCSV(t *testing.T) {
	cfg := `{"groupBy":"stage","metric":"count","columnGroup":{"field":"region"}}`
	store := &rjStore{gridRows: []dynrepo.GridRow{gr(sp("1"), 1, "*", "*"), gr(sp("1"), 1, "A", "*"), gr(sp("1"), 1, "A", "B")},
		gridCols: []dynrepo.GridRow{gr(sp("1"), 1, "B")}}
	var buf bytes.Buffer
	if err := matrixReportService("matrix", cfg, store, nil).ExportReportCSV(context.Background(), gridRep, "t1", "rep1", nil, &buf); err != nil {
		t.Fatalf("grid export: %v", err)
	}
	if got, want := buf.String(), utf8BOM+"stage \\ region,B,Tổng\nA,1,1\nTổng,1,1\n"; got != want {
		t.Fatalf("grid export =\n%q\nwant\n%q", got, want)
	}

	overCap := &rjStore{gridErr: &dynrepo.ErrGridTooLarge{Leaves: 1001}}
	buf.Reset()
	err := matrixReportService("matrix", cfg, overCap, nil).ExportReportCSV(context.Background(), gridRep, "t1", "rep1", nil, &buf)
	if codeOf(err) != "REPORT_MATRIX_TOO_MANY_CELLS" || buf.Len() != 0 {
		t.Fatalf("over-cap export: code %q, %d bytes; want REPORT_MATRIX_TOO_MANY_CELLS and nothing written", codeOf(err), buf.Len())
	}

	legacy := matrixReportService("matrix", `{"groupBy":"stage","metric":"count"}`, &rjStore{}, nil)
	buf.Reset()
	if err := legacy.ExportReportCSV(context.Background(), gridRep, "t1", "rep1", nil, &buf); err != nil {
		t.Fatalf("legacy matrix export: %v", err)
	}
	if got := buf.String(); got != utf8BOM+"key,value,count\n" {
		t.Fatalf("legacy matrix export = %q, want the unchanged key,value,count file", got)
	}
}

// gk builds a grid header / tree node group (nil key = the NULL group).
func gk(key, value *string, count int64) AggregateGroupResult {
	return AggregateGroupResult{Key: key, Value: value, Count: count}
}

// leaf / branch build tree nodes for the CSV shapes.
func leaf(key, value *string, count int64) GridNode {
	return GridNode{AggregateGroupResult: gk(key, value, count)}
}

func branch(key, value *string, count int64, kids ...GridNode) GridNode {
	return GridNode{AggregateGroupResult: gk(key, value, count), Children: kids}
}

// TestWriteGridCSV_Shapes is T054: the grid file is the grid on screen (FR-020). A matrix: the
// "<row axis> \ <column axis>" heading, the column keys and "Tổng"; one line per row with its cells
// (an empty cell where no data) and the row total; a last "Tổng" line with the column totals and
// the grand total. A tree: one column per level then "Giá trị,Số lượng", depth first, every leaf
// line repeating its ancestors' keys, a "Tổng <key>" line closing each branch in that branch's own
// column, and "Tổng cộng" last. A NULL key is a real group and prints "(trống)"; a NULL value is an
// empty cell. Every file opens with the UTF-8 BOM.
func TestWriteGridCSV_Shapes(t *testing.T) {
	two := []string{"Giai đoạn", "Ngày chốt"}
	cases := []struct {
		name   string
		grid   GridRunResult
		titles []string
		want   string
	}{
		{name: "matrix: rows, sparse cells, row totals, the Tổng line", titles: two,
			grid: GridRunResult{Kind: GridKindMatrix,
				Rows:    []AggregateGroupResult{gk(sp("Won"), sp("20.2"), 1), gk(sp("Proposal"), sp("10.1"), 1)},
				Columns: []AggregateGroupResult{gk(sp("2026-01"), sp("10.1"), 1), gk(sp("2026-02"), sp("20.2"), 1)},
				Cells:   []GridCell{{R: 0, C: 1, Value: sp("20.2"), Count: 1}, {R: 1, C: 0, Value: sp("10.1"), Count: 1}},
				Total:   AggregateScalarResult{Value: sp("30.3"), Count: 2}},
			want: "Giai đoạn \\ Ngày chốt,2026-01,2026-02,Tổng\n" +
				"Won,,20.2,20.2\n" +
				"Proposal,10.1,,10.1\n" +
				"Tổng,10.1,20.2,30.3\n"},
		{name: "matrix: a NULL key is a group, a NULL value an empty cell", titles: two,
			grid: GridRunResult{Kind: GridKindMatrix,
				Rows:    []AggregateGroupResult{gk(sp("Won"), sp("5"), 2), gk(nil, nil, 1)},
				Columns: []AggregateGroupResult{gk(nil, sp("5"), 3)},
				Cells:   []GridCell{{R: 0, C: 0, Value: sp("5"), Count: 2}, {R: 1, C: 0, Value: nil, Count: 1}},
				Total:   AggregateScalarResult{Value: sp("5"), Count: 3}},
			want: "Giai đoạn \\ Ngày chốt,(trống),Tổng\n" +
				"Won,5,5\n" +
				"(trống),,\n" +
				"Tổng,5,5\n"},
		{name: "matrix: nothing matched", titles: two,
			grid: GridRunResult{Kind: GridKindMatrix, Rows: []AggregateGroupResult{}, Columns: []AggregateGroupResult{}, Cells: []GridCell{}},
			want: "Giai đoạn \\ Ngày chốt,Tổng\nTổng,\n"},
		{name: "tree, 2 levels: leaves repeat the parent, Tổng <key> closes the branch", titles: two,
			grid: GridRunResult{Kind: GridKindTree, Groups: []GridNode{
				branch(sp("Won"), sp("320.2"), 2, leaf(sp("2026-01"), sp("300"), 1), leaf(sp("2026-02"), sp("20.2"), 1)),
				branch(nil, sp("7"), 1, leaf(nil, sp("7"), 1)),
			}, Total: AggregateScalarResult{Value: sp("327.2"), Count: 3}},
			want: "Giai đoạn,Ngày chốt,Giá trị,Số lượng\n" +
				"Won,2026-01,300,1\n" +
				"Won,2026-02,20.2,1\n" +
				"Tổng Won,,320.2,2\n" +
				"(trống),(trống),7,1\n" +
				"Tổng (trống),,7,1\n" +
				"Tổng cộng,,327.2,3\n"},
		{name: "tree, 3 levels: a level-2 sub-total sits in the level-2 column", titles: []string{"Vùng", "Giai đoạn", "Tháng"},
			grid: GridRunResult{Kind: GridKindTree, Groups: []GridNode{
				branch(sp("Bắc"), sp("6"), 3,
					branch(sp("Won"), sp("5"), 2, leaf(sp("2026-01"), sp("2"), 1), leaf(sp("2026-02"), sp("3"), 1)),
					branch(sp("Lost"), sp("1"), 1, leaf(sp("2026-01"), sp("1"), 1))),
			}, Total: AggregateScalarResult{Value: sp("6"), Count: 3}},
			want: "Vùng,Giai đoạn,Tháng,Giá trị,Số lượng\n" +
				"Bắc,Won,2026-01,2,1\n" +
				"Bắc,Won,2026-02,3,1\n" +
				"Bắc,Tổng Won,,5,2\n" +
				"Bắc,Lost,2026-01,1,1\n" +
				"Bắc,Tổng Lost,,1,1\n" +
				"Tổng Bắc,,,6,3\n" +
				"Tổng cộng,,,6,3\n"},
		{name: "tree, 1 level (a thresholded summary): leaves then Tổng cộng", titles: []string{"Giai đoạn"},
			grid: GridRunResult{Kind: GridKindTree, Groups: []GridNode{leaf(sp("Won"), sp("9"), 2), leaf(sp("Lost"), nil, 1)},
				Total: AggregateScalarResult{Value: sp("9"), Count: 3}},
			want: "Giai đoạn,Giá trị,Số lượng\nWon,9,2\nLost,,1\nTổng cộng,9,3\n"},
		{name: "tree: nothing matched", titles: two,
			grid: GridRunResult{Kind: GridKindTree, Groups: []GridNode{}},
			want: "Giai đoạn,Ngày chốt,Giá trị,Số lượng\nTổng cộng,,,0\n"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeGridCSV(&tt.grid, tt.titles, &buf); err != nil {
				t.Fatalf("writeGridCSV: %v", err)
			}
			if got := buf.String(); got != utf8BOM+tt.want {
				t.Fatalf("file =\n%q\nwant\n%q", got, utf8BOM+tt.want)
			}
		})
	}

	t.Run("a heading count that does not fit the grid is an error before any byte", func(t *testing.T) {
		for _, g := range []GridRunResult{{Kind: GridKindMatrix}, {Kind: GridKindTree}} {
			var buf bytes.Buffer
			if err := writeGridCSV(&g, nil, &buf); err == nil || buf.Len() != 0 {
				t.Fatalf("%s: err = %v, %d bytes; want an error and nothing written", g.Kind, err, buf.Len())
			}
		}
	})
}

// TestWriteGridCSV_FormulaGuard is T054 (guard half): a key, an axis heading or a composed heading
// that Excel would read as a formula (=, +, -, @) is neutralized with a leading quote; a number,
// negative ones included, is left as it is.
func TestWriteGridCSV_FormulaGuard(t *testing.T) {
	evil := `=HYPERLINK("http://x","y")`
	var m bytes.Buffer
	if err := writeGridCSV(&GridRunResult{Kind: GridKindMatrix,
		Rows:    []AggregateGroupResult{gk(sp(evil), sp("-5"), 1), gk(sp("+cmd"), sp("1"), 1)},
		Columns: []AggregateGroupResult{gk(sp("@SUM(A1)"), sp("-4"), 2)},
		Cells:   []GridCell{{R: 0, C: 0, Value: sp("-5"), Count: 1}, {R: 1, C: 0, Value: sp("1"), Count: 1}},
		Total:   AggregateScalarResult{Value: sp("-4"), Count: 2},
	}, []string{"-rows", "Ngày"}, &m); err != nil {
		t.Fatalf("matrix: %v", err)
	}
	if got, want := m.String(), utf8BOM+`'-rows \ Ngày,'@SUM(A1),Tổng`+"\n"+
		`"'=HYPERLINK(""http://x"",""y"")",-5,-5`+"\n"+
		"'+cmd,1,1\nTổng,-4,-4\n"; got != want {
		t.Fatalf("matrix file =\n%q\nwant\n%q", got, want)
	}

	var tr bytes.Buffer
	if err := writeGridCSV(&GridRunResult{Kind: GridKindTree,
		Groups: []GridNode{branch(sp("-cmd"), sp("3"), 1, leaf(sp(evil), sp("3"), 1))},
		Total:  AggregateScalarResult{Value: sp("3"), Count: 1},
	}, []string{"=A", "@B"}, &tr); err != nil {
		t.Fatalf("tree: %v", err)
	}
	if got, want := tr.String(), utf8BOM+"'=A,'@B,Giá trị,Số lượng\n"+
		`'-cmd,"'=HYPERLINK(""http://x"",""y"")",3,1`+"\n"+
		"Tổng -cmd,,3,1\nTổng cộng,,3,1\n"; got != want {
		t.Fatalf("tree file =\n%q\nwant\n%q", got, want)
	}
}
