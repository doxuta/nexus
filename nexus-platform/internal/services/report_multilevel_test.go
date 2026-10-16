package services

// Unit tests for the sub-levels of a saved Summary report (specs/049-report-grouping-pivot, slice
// S3 report-multilevel-run): the config key `subGroups` is validated by the ONE shape body every
// write path shares (T043), a summary that carries it runs through RecordService.AggregateGrid as
// a tree, a summary without it runs the single-axis path exactly as before, and the CSV export
// writes the same tree (slice S4 report-grid-csv). The run shape, the owner
// scope and the hidden sub-level run on MySQL through the production router in
// handlers/report_grid_integration_test.go.

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

// TestValidateReportConfig_SubGroups is T043: sub-levels belong to a summary only, at most 2, each
// a field with a run-time bucket, never repeating an earlier level (same field AND same bucket),
// namespaced only on the configured join, and — in deep mode (the AI Builder's propose_report) —
// resolved by the same axis body the run executes, the error naming the broken level.
func TestValidateReportConfig_SubGroups(t *testing.T) {
	live := []metadata.FieldDefinition{
		{Name: "stage", FieldType: "picklist", Options: strPtr(`{"indexed":true}`)},
		{Name: "close_date", FieldType: "date", Options: strPtr(`{"indexed":true}`)},
		{Name: "region", FieldType: "text", Options: strPtr(`{"indexed":true}`)},
		{Name: "note", FieldType: "text"},
		{Name: "amount", FieldType: "number"},
	}
	sg := func(pairs ...string) []ReportGroupInput {
		out := make([]ReportGroupInput, 0, len(pairs)/2)
		for i := 0; i+1 < len(pairs); i += 2 {
			out = append(out, ReportGroupInput{Field: pairs[i], Bucket: pairs[i+1]})
		}
		return out
	}
	summary := func(groupBy, bucket string, subs []ReportGroupInput) ReportConfig {
		return ReportConfig{GroupBy: groupBy, Bucket: bucket, Metric: "sum", Field: "amount", SubGroups: subs}
	}
	joined := func(subs []ReportGroupInput) ReportConfig {
		return ReportConfig{GroupBy: "stage", Metric: "count", Joins: []ReportJoinInput{{Relationship: "account"}}, SubGroups: subs}
	}
	cases := []struct {
		name     string
		typ      string
		cfg      ReportConfig
		live     []metadata.FieldDefinition
		wantCode string // "" = valid
		wantAxis string // details.axis the error must carry ("" = not checked)
	}{
		{name: "stage → close_date(month)", typ: "summary", cfg: summary("stage", "", sg("close_date", "month"))},
		{name: "deep: same, against live fields", typ: "summary", cfg: summary("stage", "", sg("close_date", "month")), live: live},
		{name: "three levels", typ: "summary", cfg: summary("stage", "", sg("close_date", "month", "region", "")), live: live},
		{name: "same field, other bucket is two levels", typ: "summary", cfg: summary("close_date", "year", sg("close_date", "month"))},
		{name: "more than 2 sub-levels", typ: "summary",
			cfg: summary("stage", "", sg("close_date", "month", "region", "", "close_date", "year")), wantCode: "REPORT_CONFIG_INVALID"},
		{name: "matrix cannot carry sub-levels", typ: "matrix", cfg: summary("stage", "", sg("close_date", "month")),
			wantCode: "REPORT_CONFIG_INVALID"},
		{name: "tabular cannot carry sub-levels", typ: "tabular",
			cfg: ReportConfig{Columns: []string{"stage"}, SubGroups: sg("close_date", "month")}, wantCode: "REPORT_CONFIG_INVALID"},
		{name: "empty field", typ: "summary", cfg: summary("stage", "", sg("close_date", "month", "  ", "")),
			wantCode: "REPORT_CONFIG_INVALID", wantAxis: "subGroups[1]"},
		{name: "unknown bucket", typ: "summary", cfg: summary("stage", "", sg("close_date", "fortnight")),
			wantCode: "REPORT_CONFIG_INVALID", wantAxis: "subGroups[0]"},
		{name: "repeats groupBy (same field, same bucket)", typ: "summary", cfg: summary("close_date", "month", sg(" close_date ", "MONTH")),
			wantCode: "REPORT_CONFIG_INVALID", wantAxis: "subGroups[0]"},
		{name: "repeats groupBy (same field, no bucket)", typ: "summary", cfg: summary("stage", "", sg("stage", "")),
			wantCode: "REPORT_CONFIG_INVALID", wantAxis: "subGroups[0]"},
		{name: "second sub-level repeats the first", typ: "summary", cfg: summary("stage", "", sg("region", "", "region", "")),
			wantCode: "REPORT_CONFIG_INVALID", wantAxis: "subGroups[1]"},
		{name: "namespaced sub-level on another relationship", typ: "summary", cfg: joined(sg("owner_rel.industry", "")),
			wantCode: "REPORT_JOIN_ENTITY_MISMATCH"},
		{name: "namespaced sub-level on the configured relationship", typ: "summary", cfg: joined(sg("account.industry", ""))},
		// A joined token is resolved at run time (the related entity's live fields + the caller's
		// hop ACL), whatever level it sits at — the middle one included.
		{name: "deep: joined middle level is left to the run", typ: "summary", cfg: joined(sg("account.industry", "", "region", "")), live: live},
		{name: "deep: second sub-level not indexed", typ: "summary", cfg: summary("stage", "", sg("close_date", "month", "note", "")), live: live,
			wantCode: "AGGREGATE_REQUIRES_INDEXED", wantAxis: "subGroups[1]"},
		{name: "deep: bucket over a non-date sub-level", typ: "summary", cfg: summary("stage", "", sg("region", "month")), live: live,
			wantCode: "AGGREGATE_BUCKET_FIELD_NOT_DATE", wantAxis: "subGroups[0]"},
		// #261: two tokens, one physical column (ownerId / owner_id). Shape-only cannot see it (it
		// compares the token text); deep mode must, exactly as the grid run does.
		{name: "deep: owner aliases are one column", typ: "summary", cfg: summary("ownerId", "", sg("owner_id", "")), live: live,
			wantCode: "REPORT_CONFIG_INVALID", wantAxis: "subGroups[0]"},
		{name: "deep: owner alias vs created_at stays two levels", typ: "summary", cfg: summary("ownerId", "", sg("created_at", "month")), live: live},
		{name: "deep: groupBy not indexed names groupBy", typ: "summary", cfg: summary("note", "", sg("stage", "")), live: live,
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
				if len(back.SubGroups) != len(tt.cfg.SubGroups) {
					t.Fatalf("stored config %s dropped a sub-level", stored)
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
		_, stored, err := ValidateReportConfig("summary", summary("stage", "", sg(" close_date ", " Month ", " region ", "")), nil)
		if err != nil {
			t.Fatalf("validate: %v", err)
		}
		var back ReportConfig
		_ = json.Unmarshal(stored, &back)
		if want := sg("close_date", "month", "region", ""); !reflect.DeepEqual(back.SubGroups, want) {
			t.Fatalf("stored sub-levels = %+v, want %+v", back.SubGroups, want)
		}
	})

	t.Run("a summary without sub-levels stores no subGroups key", func(t *testing.T) {
		for _, subs := range [][]ReportGroupInput{nil, {}} {
			_, stored, err := ValidateReportConfig("summary", summary("stage", "", subs), nil)
			if err != nil {
				t.Fatalf("validate: %v", err)
			}
			var raw map[string]any
			_ = json.Unmarshal(stored, &raw)
			if _, ok := raw["subGroups"]; ok {
				t.Fatalf("stored %s carries subGroups — a one-level summary must stay byte-identical", stored)
			}
		}
	})
}

// TestReportRun_SummarySubGroupsRunsTheTree: the saved sub-levels reach the grid engine after the
// groupBy, WITHOUT column totals, and the run comes back as a summary tree with its levels; the
// CSV export writes that same tree (S4).
func TestReportRun_SummarySubGroupsRunsTheTree(t *testing.T) {
	store := &rjStore{gridRows: []dynrepo.GridRow{
		gr(sp("30"), 3, "*", "*"),
		gr(sp("10"), 1, "Proposal", "2026-01"),
		gr(sp("10"), 1, "Proposal", "*"),
		gr(sp("5"), 1, "Won", "2026-01"),
		gr(sp("15"), 1, "Won", "2026-02"),
		gr(sp("20"), 2, "Won", "*"),
	}}
	cfg := `{"groupBy":"stage","metric":"sum","field":"amount","subGroups":[{"field":"close_date","bucket":"month"}]}`
	svc := matrixReportService("summary", cfg, store, nil)
	res, err := svc.RunReport(context.Background(), gridRep, "t1", "rep1", nil)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	if store.groupedCalled || !store.gridCalled {
		t.Fatalf("grouped=%v grid=%v, want the grid engine only", store.groupedCalled, store.gridCalled)
	}
	if store.gridParams.CrossTotals || len(store.gridParams.Axes) != 2 ||
		store.gridParams.Axes[0].Physical != "stage" || store.gridParams.Axes[1].Physical != "close_date" ||
		store.gridParams.Axes[1].Bucket != dynrepo.BucketMonth {
		t.Fatalf("grid params = %+v, want stage → close_date(month), no column totals", store.gridParams)
	}
	if res.Type != report.TypeSummary || res.Aggregate != nil || res.Grid == nil || res.Grid.Kind != GridKindTree {
		t.Fatalf("result = %+v, want a summary tree", res)
	}
	if want := []ReportGroupInput{{Field: "stage"}, {Field: "close_date", Bucket: "month"}}; !reflect.DeepEqual(res.Axes, want) {
		t.Fatalf("levels = %+v, want %+v", res.Axes, want)
	}
	g := res.Grid.Groups
	if len(g) != 2 || keyText(g[0].Key) != "Won" || len(g[0].Children) != 2 || keyText(g[1].Key) != "Proposal" ||
		valText(res.Grid.Total.Value) != "30" || res.Grid.Total.Count != 3 {
		t.Fatalf("tree = %+v total = %+v", g, res.Grid.Total)
	}

	// S4: the export writes the same tree, one column per level (no label in this fixture, so the
	// field name), each branch closed by its sub-total line, the grand total last.
	var buf bytes.Buffer
	if err := svc.ExportReportCSV(context.Background(), gridRep, "t1", "rep1", nil, &buf); err != nil {
		t.Fatalf("tree export: %v", err)
	}
	if got, want := buf.String(), utf8BOM+"stage,close_date,Giá trị,Số lượng\n"+
		"Won,2026-01,5,1\nWon,2026-02,15,1\nTổng Won,,20,2\n"+
		"Proposal,2026-01,10,1\nTổng Proposal,,10,1\n"+
		"Tổng cộng,,30,3\n"; got != want {
		t.Fatalf("tree export =\n%q\nwant\n%q", got, want)
	}
}

// TestReportRun_SummaryWithoutSubGroupsStaysSingleAxis: a summary with no (or an empty list of)
// sub-levels runs the single-axis aggregate exactly as before (FR-016).
func TestReportRun_SummaryWithoutSubGroupsStaysSingleAxis(t *testing.T) {
	for _, cfg := range []string{`{"groupBy":"stage","metric":"count"}`, `{"groupBy":"stage","metric":"count","subGroups":[]}`} {
		store := &rjStore{}
		res, err := matrixReportService("summary", cfg, store, nil).RunReport(context.Background(), gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport %s: %v", cfg, err)
		}
		if !store.groupedCalled || store.gridCalled || res.Grid != nil || res.Axes != nil || res.Type != report.TypeSummary {
			t.Fatalf("%s: grouped=%v grid=%v result=%+v, want the single-axis path only", cfg, store.groupedCalled, store.gridCalled, res)
		}
	}
}

// TestReportSave_ResolvedDuplicateAxis is #261 on the admin save path: shape-only validation
// compares tokens, so ownerId + owner_id (one column) used to save and then 422 on every run. Now
// the save itself is rejected, naming the offending level.
func TestReportSave_ResolvedDuplicateAxis(t *testing.T) {
	base, _ := axisLabelService("summary", `{}`, nil, viewDealsAndAccounts, nil)
	svc := NewReportService(emptyListReportRepo{&fakeReportRepo{}}, base.records)
	cfg := ReportConfig{GroupBy: "ownerId", Metric: "count", SubGroups: []ReportGroupInput{{Field: "owner_id"}}}
	for _, save := range []struct {
		name string
		fn   func() error
	}{
		{"create", func() error {
			_, err := svc.CreateReport(context.Background(), "t1", CreateReportInput{Name: "x", EntityName: "deals", Type: "summary", Config: cfg})
			return err
		}},
		{"update", func() error {
			_, err := svc.UpdateReport(context.Background(), "t1", "rep1", UpdateReportInput{Name: "x", EntityName: "deals", Type: "summary", Config: cfg})
			return err
		}},
	} {
		t.Run(save.name, func(t *testing.T) {
			var ve *ValidationError
			err := save.fn()
			if !errors.As(err, &ve) || ve.Code != "REPORT_CONFIG_INVALID" || ve.Details["axis"] != "subGroups[0]" {
				t.Fatalf("err = %v, want REPORT_CONFIG_INVALID on subGroups[0]", err)
			}
		})
	}
}

// emptyListReportRepo lets CreateReport's duplicate-name scan run over a fakeReportRepo that panics on List.
type emptyListReportRepo struct{ *fakeReportRepo }

func (emptyListReportRepo) List(context.Context, string) ([]report.Record, error) { return nil, nil }

// listReportRepo returns canned rows to CreateReport's duplicate-name scan.
type listReportRepo struct {
	*fakeReportRepo
	rows []report.Record
}

func (l listReportRepo) List(context.Context, string) ([]report.Record, error) { return l.rows, nil }

// TestReportCreate_DuplicateNameIgnoresDraftDeleted: a report the builder marked "to delete" in the draft
// overlay does not block re-creating the same name; a live or edited one still does (specs/063 S7-2).
func TestReportCreate_DuplicateNameIgnoresDraftDeleted(t *testing.T) {
	base, _ := axisLabelService("summary", `{}`, nil, viewDealsAndAccounts, nil)
	// An invalid config makes a name that passes the dup scan stop at REPORT_CONFIG_INVALID — no repo Create needed.
	bad := ReportConfig{GroupBy: "ownerId", Metric: "count", SubGroups: []ReportGroupInput{{Field: "owner_id"}}}
	for _, tt := range []struct{ state, wantCode string }{
		{"", "REPORT_ALREADY_EXISTS"},
		{"edited", "REPORT_ALREADY_EXISTS"},
		{draftStateDeleted, "REPORT_CONFIG_INVALID"},
	} {
		t.Run("state="+tt.state, func(t *testing.T) {
			repo := listReportRepo{&fakeReportRepo{}, []report.Record{{ID: "r1", Name: "Pipeline", DraftState: tt.state}}}
			svc := NewReportService(repo, base.records)
			_, err := svc.CreateReport(context.Background(), "t1", CreateReportInput{Name: "Pipeline", EntityName: "deals", Type: "summary", Config: bad})
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want %s", err, tt.wantCode)
			}
		})
	}
}
