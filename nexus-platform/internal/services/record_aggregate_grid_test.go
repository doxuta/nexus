package services

// Unit tests for the report grid service (specs/049-report-grouping-pivot, slice S2a
// report-grid-service): every guard of the shared aggregatePreflight runs BEFORE the store is
// reached, whatever position the offending axis holds (T035a); shape/axis errors name the broken
// axis; the dynrepo cap maps to the two 422 codes; and the ROLLUP rows become a sorted, dense-capped
// matrix or tree. The SQL itself is proven on MySQL by the dynrepo + services integration tests.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

func gridOpt(s string) *string { return &s }

// buildGridService wires the grid fixtures: tickets (region/stage/close_date/secret indexed,
// amount number, about polymorphic), employees reached through the FK hop "assignee" (level
// indexed), tags reached through the N-N hop "tagged".
func buildGridService(store RecordStore, fACL *rjFieldACL) *RecordService {
	idx := gridOpt(`{"indexed":true}`)
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{
			"tickets": ent("tickets"), "employees": ent("employees"), "tags": ent("tags"),
		},
		fields: map[string][]metadata.FieldDefinition{
			"tickets": {
				{Name: "region", FieldType: "text", Options: idx},
				{Name: "stage", FieldType: "picklist", Options: idx},
				{Name: "close_date", FieldType: "date", Options: idx},
				{Name: "secret", FieldType: "text", Options: idx},
				{Name: "amount", FieldType: "number"},
				{Name: "about", FieldType: "polymorphic_relation"},
			},
			"employees": {{Name: "level", FieldType: "text", Options: idx}, {Name: "salary", FieldType: "number"}},
			"tags":      {{Name: "label", FieldType: "text", Options: idx}},
		},
	}
	rollup := &rjRollup{rels: map[string]*metadata.RelationshipDefinition{
		"assignee": rel("1-N", "employees", "tickets", "assignee_id", nil),
		"tagged":   rel("N-N", "tickets", "tags", "", &rjNNJoinTable),
	}}
	s := NewRecordService(cat, store).WithRollupMeta(rollup).
		WithACLAuthorizer(&rjAuthz{allow: map[string]bool{"employees": true, "tags": true}})
	if fACL != nil {
		s = s.WithFieldACL(fACL)
	}
	return s
}

func axes(tokens ...string) []GridAxisInput {
	out := make([]GridAxisInput, len(tokens))
	for i, t := range tokens {
		out[i] = GridAxisInput{Field: t}
		if t == "close_date" {
			out[i].Bucket = "month"
		}
	}
	return out
}

var gridRep = RecordScope{UserID: "u-rep", RoleID: "rep", AccessLevel: "owner"}

// TestAggregateGridService_GuardsBeforeStore is T035a: a hidden axis at EVERY position (row /
// level 1, sub level, column, joined on the related entity, polymorphic <f>_type), a hidden metric
// field, a hidden filter field and SUM over an N-N hop are rejected before store.AggregateGrid.
func TestAggregateGridService_GuardsBeforeStore(t *testing.T) {
	ctx := context.Background()
	assignee := []AggregateJoin{{Relationship: "assignee"}}
	tagged := []AggregateJoin{{Relationship: "tagged"}}
	cases := []struct {
		name      string
		hidden    string // "entity.field" hidden from the caller's role
		q         GridQuery
		wantCode  string
		wantAsked string // field-ACL must have been asked about this "entity.field"
		neverAsk  string // …and never about this one (routing)
	}{
		{name: "hidden row axis (groupBy)", hidden: "tickets.secret",
			q:        GridQuery{Metric: "count", Matrix: true, Axes: axes("secret", "region")},
			wantCode: "FIELD_FORBIDDEN"},
		{name: "hidden sub level (subGroups[1])", hidden: "tickets.secret",
			q:        GridQuery{Metric: "count", Axes: axes("region", "close_date", "secret")},
			wantCode: "FIELD_FORBIDDEN"},
		{name: "hidden column axis", hidden: "tickets.secret",
			q:        GridQuery{Metric: "count", Matrix: true, Axes: axes("region", "secret")},
			wantCode: "FIELD_FORBIDDEN"},
		{name: "hidden joined axis is asked on the RELATED entity", hidden: "employees.level",
			q:         GridQuery{Metric: "count", Matrix: true, Joins: assignee, Axes: axes("region", "assignee.level")},
			wantCode:  "FIELD_FORBIDDEN",
			wantAsked: "employees.level", neverAsk: "tickets.level"},
		{name: "hidden polymorphic field through its <f>_type axis", hidden: "tickets.about",
			q:         GridQuery{Metric: "count", Axes: axes("region", "about_type")},
			wantCode:  "FIELD_FORBIDDEN",
			wantAsked: "tickets.about"},
		{name: "hidden metric field", hidden: "tickets.amount",
			q:        GridQuery{Metric: "sum", Field: "amount", Matrix: true, Axes: axes("region", "stage")},
			wantCode: "FIELD_FORBIDDEN"},
		{name: "hidden filter field (anti-oracle)", hidden: "tickets.secret",
			q: GridQuery{Metric: "count", Matrix: true, Axes: axes("region", "stage"),
				Filters: []FilterInput{{Field: "secret", Values: []string{"x"}}}},
			wantCode: "FIELD_FORBIDDEN"},
		{name: "SUM over an N-N hop",
			q:        GridQuery{Metric: "sum", Field: "amount", Matrix: true, Joins: tagged, Axes: axes("region", "stage")},
			wantCode: "AGGREGATE_NN_FANOUT_UNSAFE"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store := &rjStore{}
			fACL := &rjFieldACL{hidden: map[string]bool{tt.hidden: true}}
			s := buildGridService(store, fACL)
			_, err := s.AggregateGrid(ctx, gridRep, "t1", "tickets", tt.q)
			if codeOf(err) != tt.wantCode {
				t.Fatalf("code = %q, want %q (err=%v)", codeOf(err), tt.wantCode, err)
			}
			if store.gridCalled || store.groupedCalled || store.scalarCalled {
				t.Fatalf("store reached although the preflight rejected")
			}
			asked := map[string]bool{}
			for _, k := range fACL.asked {
				asked[k] = true
			}
			if tt.wantAsked != "" && !asked[tt.wantAsked] {
				t.Fatalf("field-ACL never asked about %s; asked=%v", tt.wantAsked, fACL.asked)
			}
			if tt.neverAsk != "" && asked[tt.neverAsk] {
				t.Fatalf("field-ACL asked about %s — routed to the wrong entity; asked=%v", tt.neverAsk, fACL.asked)
			}
		})
	}

	t.Run("every axis visible → store reached with the resolved axes", func(t *testing.T) {
		store := &rjStore{}
		s := buildGridService(store, &rjFieldACL{hidden: map[string]bool{"tickets.secret": true}})
		q := GridQuery{Metric: "sum", Field: "amount", Matrix: true, Joins: assignee,
			Axes: []GridAxisInput{{Field: " close_date ", Bucket: "Month"}, {Field: "assignee.level"}}}
		if _, err := s.AggregateGrid(ctx, gridRep, "t1", "tickets", q); err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if !store.gridCalled {
			t.Fatalf("store.AggregateGrid not reached")
		}
		p := store.gridParams
		want := []dynrepo.GridAxis{{Physical: "close_date", Bucket: dynrepo.BucketMonth}, {Physical: "level", Alias: reportJoinAlias}}
		if !reflect.DeepEqual(p.Axes, want) {
			t.Fatalf("axes = %+v, want %+v", p.Axes, want)
		}
		if !p.CrossTotals || p.Metric != dynrepo.MetricSum || p.FieldColumn != "amount" || len(p.Joins) != 1 {
			t.Fatalf("params = %+v, want CrossTotals + sum(amount) + 1 join", p)
		}
		if store.gridScope.Level != dynrepo.AccessOwner || store.gridScope.UserID != "u-rep" || store.gridScope.IsAdmin {
			t.Fatalf("row scope = %+v, want owner/u-rep (never widened)", store.gridScope)
		}
	})

	t.Run("a tree does not ask for column totals", func(t *testing.T) {
		store := &rjStore{}
		s := buildGridService(store, nil)
		if _, err := s.AggregateGrid(ctx, gridRep, "t1", "tickets", GridQuery{Metric: "count", Axes: axes("region", "stage", "close_date")}); err != nil {
			t.Fatalf("AggregateGrid: %v", err)
		}
		if store.gridParams.CrossTotals || len(store.gridParams.Axes) != 3 {
			t.Fatalf("tree params = %+v, want 3 axes, no CrossTotals", store.gridParams)
		}
	})
}

// TestAggregateGridService_AxisErrorsNameTheAxis: shape errors and per-axis resolution errors
// never reach the store, and carry details.axis in the report-config vocabulary (FR-005).
func TestAggregateGridService_AxisErrorsNameTheAxis(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		q        GridQuery
		wantCode string
		wantAxis string // "" = no details.axis expected
	}{
		{"matrix with one axis", GridQuery{Metric: "count", Matrix: true, Axes: axes("region")}, "REPORT_CONFIG_INVALID", ""},
		{"matrix with three axes", GridQuery{Metric: "count", Matrix: true, Axes: axes("region", "stage", "close_date")}, "REPORT_CONFIG_INVALID", ""},
		{"tree without axis", GridQuery{Metric: "count"}, "REPORT_CONFIG_INVALID", ""},
		{"tree with four axes", GridQuery{Metric: "count", Axes: axes("region", "stage", "close_date", "secret")}, "REPORT_CONFIG_INVALID", ""},
		{"empty column axis", GridQuery{Metric: "count", Matrix: true, Axes: axes("region", "  ")}, "REPORT_CONFIG_INVALID", "columnGroup"},
		{"duplicate axis (same field, same bucket)", GridQuery{Metric: "count", Axes: axes("region", "region")}, "REPORT_CONFIG_INVALID", "subGroups[0]"},
		{"non-indexed column axis", GridQuery{Metric: "count", Matrix: true, Axes: axes("region", "amount")}, "AGGREGATE_REQUIRES_INDEXED", "columnGroup"},
		{"bad bucket on the third level", GridQuery{Metric: "count",
			Axes: []GridAxisInput{{Field: "region"}, {Field: "stage"}, {Field: "close_date", Bucket: "fortnight"}}}, "AGGREGATE_BUCKET_INVALID", "subGroups[1]"},
		{"bucket on a non-date row axis", GridQuery{Metric: "count", Matrix: true,
			Axes: []GridAxisInput{{Field: "region", Bucket: "month"}, {Field: "stage"}}}, "AGGREGATE_BUCKET_FIELD_NOT_DATE", "groupBy"},
		{"namespaced axis on the wrong relationship", GridQuery{Metric: "count", Matrix: true,
			Joins: []AggregateJoin{{Relationship: "assignee"}}, Axes: axes("region", "boss.level")}, "REPORT_JOIN_ENTITY_MISMATCH", "columnGroup"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			store := &rjStore{}
			_, err := buildGridService(store, nil).AggregateGrid(ctx, gridRep, "t1", "tickets", tt.q)
			if codeOf(err) != tt.wantCode {
				t.Fatalf("code = %q, want %q (err=%v)", codeOf(err), tt.wantCode, err)
			}
			if store.gridCalled {
				t.Fatalf("store reached on an invalid grid")
			}
			var ve *ValidationError
			errors.As(err, &ve)
			if got, _ := ve.Details["axis"].(string); got != tt.wantAxis {
				t.Fatalf("details.axis = %q, want %q", got, tt.wantAxis)
			}
		})
	}

	t.Run("same field with another bucket is not a duplicate", func(t *testing.T) {
		store := &rjStore{}
		q := GridQuery{Metric: "count", Matrix: true,
			Axes: []GridAxisInput{{Field: "close_date", Bucket: "year"}, {Field: "close_date", Bucket: "month"}}}
		if _, err := buildGridService(store, nil).AggregateGrid(ctx, gridRep, "t1", "tickets", q); err != nil {
			t.Fatalf("year × month of one field must run: %v", err)
		}
	})
}

// TestAggregateGridService_TooLargeMapsTo422 pins the cap mapping: a matrix only knows "at least
// this many cells" when the engine stops on leaves (dense cells ≥ leaves), a tree reports groups.
func TestAggregateGridService_TooLargeMapsTo422(t *testing.T) {
	ctx := context.Background()
	wrap := func(e *dynrepo.ErrGridTooLarge) error { return fmt.Errorf("dynrepo.AggregateGrid: %w", e) }
	cases := []struct {
		name        string
		matrix      bool
		err         error
		wantCode    string
		wantDetails map[string]any
	}{
		{"matrix, exact leaves", true, wrap(&dynrepo.ErrGridTooLarge{Leaves: 1001}),
			"REPORT_MATRIX_TOO_MANY_CELLS", map[string]any{"cap": 1000, "cells": 1001, "lowerBound": true}},
		{"tree, exact leaves", false, wrap(&dynrepo.ErrGridTooLarge{Leaves: 1500}),
			"REPORT_TOO_MANY_GROUPS", map[string]any{"cap": 1000, "groups": 1500, "lowerBound": false}},
		{"tree, LIMIT reached", false, wrap(&dynrepo.ErrGridTooLarge{Leaves: 1001, LowerBound: true}),
			"REPORT_TOO_MANY_GROUPS", map[string]any{"cap": 1000, "groups": 1001, "lowerBound": true}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			q := GridQuery{Metric: "count", Matrix: tt.matrix, Axes: axes("region", "stage")}
			_, err := buildGridService(&rjStore{gridErr: tt.err}, nil).AggregateGrid(ctx, gridRep, "t1", "tickets", q)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantCode {
				t.Fatalf("err = %v, want ValidationError %s", err, tt.wantCode)
			}
			if !reflect.DeepEqual(ve.Details, tt.wantDetails) {
				t.Fatalf("details = %v, want %v", ve.Details, tt.wantDetails)
			}
		})
	}

	t.Run("any other store error is wrapped, not a 422", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := buildGridService(&rjStore{gridErr: boom}, nil).AggregateGrid(ctx, gridRep, "t1", "tickets",
			GridQuery{Metric: "count", Axes: axes("region")})
		var ve *ValidationError
		if !errors.Is(err, boom) || errors.As(err, &ve) {
			t.Fatalf("err = %v, want the wrapped store error", err)
		}
	})
}

// ---- pure result builder ------------------------------------------------------------------------

func sp(s string) *string { return &s }

// gr builds a ROLLUP row: "*" = rolled-up level, "∅" = NULL data key, "text@id" = a key MySQL
// spells "text" for the group whose identity is "id" (plain "text" = identity "text").
func gr(value *string, count int64, keys ...string) dynrepo.GridRow {
	r := dynrepo.GridRow{Keys: make([]*string, len(keys)), IDs: make([]*string, len(keys)), Rolled: make([]bool, len(keys)), Value: value, Count: count}
	for i, k := range keys {
		switch k {
		case "*":
			r.Rolled[i] = true
		case "∅":
		default:
			text, id, spelled := strings.Cut(k, "@")
			if !spelled {
				id = text
			}
			r.Keys[i], r.IDs[i] = sp(text), sp(id)
		}
	}
	return r
}

func keyText(k *string) string {
	if k == nil {
		return "∅"
	}
	return *k
}

func valText(v *string) string {
	if v == nil {
		return "null"
	}
	return *v
}

func TestBuildGridResult_Matrix(t *testing.T) {
	// stage × month. The NULL stage has the largest value but a NULL key sorts last; the month
	// axis is bucketed, so its columns read chronologically with the NULL month last.
	rows := []dynrepo.GridRow{
		gr(sp("1750"), 5, "*", "*"),
		gr(sp("200"), 1, "Proposal", "2026-02"),
		gr(sp("300"), 2, "Proposal", "*"),
		gr(sp("100"), 1, "Proposal", "2026-01"),
		gr(sp("500"), 1, "Won", "2026-01"),
		gr(sp("50"), 1, "Won", "∅"),
		gr(sp("550"), 2, "Won", "*"),
		gr(sp("900"), 1, "∅", "2026-01"),
		gr(sp("900"), 1, "∅", "*"),
	}
	cols := []dynrepo.GridRow{gr(sp("50"), 1, "∅"), gr(sp("200"), 1, "2026-02"), gr(sp("1500"), 3, "2026-01")}
	res, err := buildGridResult(true, []bool{false, true}, rows, cols)
	if err != nil {
		t.Fatalf("buildGridResult: %v", err)
	}
	if res.Kind != GridKindMatrix || res.Groups != nil {
		t.Fatalf("kind = %q groups = %v, want matrix without groups", res.Kind, res.Groups)
	}
	var gotRows, gotCols, gotCells []string
	for _, h := range res.Rows {
		gotRows = append(gotRows, fmt.Sprintf("%s=%s/%d", keyText(h.Key), valText(h.Value), h.Count))
	}
	for _, h := range res.Columns {
		gotCols = append(gotCols, fmt.Sprintf("%s=%s/%d", keyText(h.Key), valText(h.Value), h.Count))
	}
	for _, c := range res.Cells {
		gotCells = append(gotCells, fmt.Sprintf("%d,%d=%s/%d", c.R, c.C, valText(c.Value), c.Count))
	}
	wantRows := []string{"Won=550/2", "Proposal=300/2", "∅=900/1"}
	wantCols := []string{"2026-01=1500/3", "2026-02=200/1", "∅=50/1"}
	wantCells := []string{"0,0=500/1", "0,2=50/1", "1,0=100/1", "1,1=200/1", "2,0=900/1"}
	if !reflect.DeepEqual(gotRows, wantRows) || !reflect.DeepEqual(gotCols, wantCols) || !reflect.DeepEqual(gotCells, wantCells) {
		t.Fatalf("rows=%v cols=%v cells=%v\nwant rows=%v cols=%v cells=%v", gotRows, gotCols, gotCells, wantRows, wantCols, wantCells)
	}
	if valText(res.Total.Value) != "1750" || res.Total.Count != 5 {
		t.Fatalf("total = %s/%d, want 1750/5", valText(res.Total.Value), res.Total.Count)
	}
}

func TestBuildGridResult_MatrixDenseCap(t *testing.T) {
	build := func(nRows, nCols int) ([]dynrepo.GridRow, []dynrepo.GridRow) {
		rows := []dynrepo.GridRow{gr(sp("1"), 1, "*", "*")}
		for i := 0; i < nRows; i++ {
			rows = append(rows, gr(sp("1"), 1, fmt.Sprintf("r%02d", i), "c000"), gr(sp("1"), 1, fmt.Sprintf("r%02d", i), "*"))
		}
		var cols []dynrepo.GridRow
		for j := 0; j < nCols; j++ {
			cols = append(cols, gr(sp("1"), 1, fmt.Sprintf("c%03d", j)))
		}
		return rows, cols
	}
	rows, cols := build(10, 100)
	if res, err := buildGridResult(true, []bool{false, false}, rows, cols); err != nil || len(res.Rows)*len(res.Columns) != 1000 {
		t.Fatalf("10×100 = 1000 cells must pass: err=%v", err)
	}
	rows, cols = build(11, 100)
	_, err := buildGridResult(true, []bool{false, false}, rows, cols)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "REPORT_MATRIX_TOO_MANY_CELLS" {
		t.Fatalf("11×100: err = %v, want REPORT_MATRIX_TOO_MANY_CELLS", err)
	}
	want := map[string]any{"cap": 1000, "cells": 1100, "rows": 11, "columns": 100, "lowerBound": false}
	if !reflect.DeepEqual(ve.Details, want) {
		t.Fatalf("details = %v, want %v", ve.Details, want)
	}
}

func TestBuildGridResult_Tree(t *testing.T) {
	// region (plain: value DESC, tie on key ASC, NULL value after every number, NULL key last) →
	// month (bucketed: key ASC). Rows arrive in no particular order.
	rows := []dynrepo.GridRow{
		gr(sp("25"), 2, "North", "*"),
		gr(sp("149"), 7, "*", "*"),
		gr(sp("20"), 1, "North", "2026-02"),
		gr(sp("5"), 1, "North", "2026-01"),
		gr(sp("12.50"), 1, "South", "2026-01"),
		gr(sp("12.50"), 1, "South", "*"),
		gr(sp("12.5"), 2, "East", "2026-03"),
		gr(sp("12.5"), 2, "East", "*"),
		gr(nil, 1, "West", "2026-01"),
		gr(nil, 1, "West", "*"),
		gr(sp("99"), 1, "∅", "2026-01"),
		gr(sp("99"), 1, "∅", "*"),
	}
	res, err := buildGridResult(false, []bool{false, true}, rows, nil)
	if err != nil {
		t.Fatalf("buildGridResult: %v", err)
	}
	if res.Kind != GridKindTree || res.Rows != nil || res.Columns != nil || res.Cells != nil {
		t.Fatalf("kind = %q, want a tree with no matrix parts", res.Kind)
	}
	var got []string
	var walk func(prefix string, nodes []GridNode)
	walk = func(prefix string, nodes []GridNode) {
		for _, n := range nodes {
			p := prefix + keyText(n.Key)
			got = append(got, fmt.Sprintf("%s=%s/%d", p, valText(n.Value), n.Count))
			walk(p+">", n.Children)
		}
	}
	walk("", res.Groups)
	want := []string{
		"North=25/2", "North>2026-01=5/1", "North>2026-02=20/1",
		"East=12.5/2", "East>2026-03=12.5/2",
		"South=12.50/1", "South>2026-01=12.50/1",
		"West=null/1", "West>2026-01=null/1",
		"∅=99/1", "∅>2026-01=99/1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree =\n%v\nwant\n%v", got, want)
	}
	if valText(res.Total.Value) != "149" || res.Total.Count != 7 {
		t.Fatalf("total = %s/%d, want 149/7", valText(res.Total.Value), res.Total.Count)
	}
	for _, leaf := range res.Groups[0].Children {
		if leaf.Children == nil || len(leaf.Children) != 0 {
			t.Fatalf("a leaf must carry an empty (non-nil) children list, got %v", leaf.Children)
		}
	}
}

// An empty set (no row matches, or a threshold kept nothing) yields NO ROLLUP row at all — MySQL
// emits no grand total over nothing — so the service builds the total itself (plan §4.2).
func TestBuildGridResult_EmptySet(t *testing.T) {
	m, err := buildGridResult(true, []bool{false, true}, nil, nil)
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}
	if m.Rows == nil || m.Columns == nil || m.Cells == nil || len(m.Rows)+len(m.Columns)+len(m.Cells) != 0 {
		t.Fatalf("empty matrix parts must be [] not null: %+v", m)
	}
	tr, err := buildGridResult(false, []bool{false}, nil, nil)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if tr.Groups == nil || len(tr.Groups) != 0 {
		t.Fatalf("empty tree groups must be [] not null: %+v", tr.Groups)
	}
	for _, r := range []GridRunResult{m, tr} {
		if r.Total.Value != nil || r.Total.Count != 0 {
			t.Fatalf("empty total = %+v, want {null 0}", r.Total)
		}
	}
}

// MySQL groups text by its collation and shows ONE member's spelling per output row — not always
// the same member. Rows must meet on the group identity; each header shows its own row's spelling.
func TestBuildGridResult_CollationEqualKeys(t *testing.T) {
	t.Run("matrix", func(t *testing.T) {
		rows := []dynrepo.GridRow{
			gr(sp("63"), 6, "*", "*"),
			gr(sp("13"), 3, "proposal@P", "North@N"),
			gr(sp("2"), 1, "PROPOSAL@P", "South@S"),
			gr(sp("15"), 4, "Proposal @P", "*"),
			gr(sp("16"), 1, "Won@W", "north @N"),
			gr(sp("32"), 1, "won@W", "SOUTH@S"),
			gr(sp("48"), 2, "WON@W", "*"),
		}
		cols := []dynrepo.GridRow{gr(sp("29"), 4, "NORTH@N"), gr(sp("34"), 2, "south@S")}
		res, err := buildGridResult(true, []bool{false, false}, rows, cols)
		if err != nil {
			t.Fatalf("buildGridResult: %v", err)
		}
		var gotRows, gotCols, gotCells []string
		for _, h := range res.Rows {
			gotRows = append(gotRows, fmt.Sprintf("%s=%s", keyText(h.Key), valText(h.Value)))
		}
		for _, h := range res.Columns {
			gotCols = append(gotCols, fmt.Sprintf("%s=%s", keyText(h.Key), valText(h.Value)))
		}
		for _, c := range res.Cells {
			gotCells = append(gotCells, fmt.Sprintf("%d,%d=%s", c.R, c.C, valText(c.Value)))
		}
		wantRows, wantCols := []string{"WON=48", "Proposal =15"}, []string{"south=34", "NORTH=29"}
		wantCells := []string{"0,0=32", "0,1=16", "1,0=2", "1,1=13"}
		if !reflect.DeepEqual(gotRows, wantRows) || !reflect.DeepEqual(gotCols, wantCols) || !reflect.DeepEqual(gotCells, wantCells) {
			t.Fatalf("rows=%v cols=%v cells=%v\nwant rows=%v cols=%v cells=%v", gotRows, gotCols, gotCells, wantRows, wantCols, wantCells)
		}
	})
	t.Run("tree", func(t *testing.T) {
		rows := []dynrepo.GridRow{
			gr(sp("13"), 3, "proposal@P", "North@N"),
			gr(sp("2"), 1, "PROPOSAL@P", "South@S"),
			gr(sp("15"), 4, "Proposal@P", "*"),
			gr(sp("15"), 4, "*", "*"),
		}
		res, err := buildGridResult(false, []bool{false, false}, rows, nil)
		if err != nil {
			t.Fatalf("buildGridResult: %v", err)
		}
		if len(res.Groups) != 1 || keyText(res.Groups[0].Key) != "Proposal" || len(res.Groups[0].Children) != 2 ||
			keyText(res.Groups[0].Children[0].Key) != "North" || keyText(res.Groups[0].Children[1].Key) != "South" {
			t.Fatalf("tree = %+v, want one Proposal=15 over North=13, South=2", res.Groups)
		}
	})
}

// Identities that disagree are an engine bug; the builder fails loudly instead of drawing a
// phantom group or merging two groups' numbers under one header.
func TestBuildGridResult_IdentityMismatchFailsLoudly(t *testing.T) {
	cases := map[string]struct {
		matrix     bool
		rows, cols []dynrepo.GridRow
	}{
		"tree: a child whose parent has no row": {rows: []dynrepo.GridRow{
			gr(sp("1"), 1, "a@A", "x"), gr(sp("1"), 1, "a@A2", "*"), gr(sp("1"), 1, "*", "*")}},
		"tree: two rows for one group": {rows: []dynrepo.GridRow{
			gr(sp("1"), 1, "a@A"), gr(sp("2"), 1, "b@A"), gr(sp("3"), 2, "*")}},
		"matrix: two row totals for one group": {matrix: true,
			rows: []dynrepo.GridRow{gr(sp("1"), 1, "a@A", "x"), gr(sp("1"), 1, "a@A", "*"), gr(sp("1"), 1, "b@A", "*")},
			cols: []dynrepo.GridRow{gr(sp("1"), 1, "x")}},
		"matrix: a cell without its column": {matrix: true,
			rows: []dynrepo.GridRow{gr(sp("1"), 1, "a", "x@X"), gr(sp("1"), 1, "a", "*")},
			cols: []dynrepo.GridRow{gr(sp("1"), 1, "x@X2")}},
	}
	for name, c := range cases {
		if _, err := buildGridResult(c.matrix, []bool{false, false}, c.rows, c.cols); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
