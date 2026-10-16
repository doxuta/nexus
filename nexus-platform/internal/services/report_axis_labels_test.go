package services

// Unit tests for the axis labels of a summary/matrix report (specs/049-report-grouping-pivot, slice
// S6 report-axis-labels): a group key that is a stored code (picklist), a record id (relation) or a
// person id (a `user` field, the owner) carries the name the screen shows — through the SAME gate
// the tabular report's relation columns go through (applyExpand: entity-view on the target,
// field-ACL on its labelField). A caller who may not see the target gets the raw key and nothing
// is fetched. The label is additive: keys, values, counts and order stay as they were, the dashboard
// / work-queue / assistant aggregate never carries one, and the one-level CSV keeps its bytes. The
// same labels reach the grid CSV (the file == the screen), proven on MySQL through the production
// router in handlers/report_grid_integration_test.go (TestReportGridCSV_UsesSameAxisLabelsAsRun).

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// axisLabelsStore answers the single-axis grouped aggregate with canned groups (rjStore answers
// nil) and the grid with rjStore's canned rows.
type axisLabelsStore struct {
	rjStore
	groups []dynrepo.AggregateGroup
}

func (s *axisLabelsStore) AggregateGrouped(context.Context, string, string, dynrepo.RowScope, dynrepo.AggregateParams) ([]dynrepo.AggregateGroup, error) {
	s.groupedCalled = true
	return s.groups, nil
}

// axisFieldACL is rjFieldACL whose read-side hidden set is configurable per entity (rjFieldACL's is
// always empty) — the set applyExpand consults for the target's labelField.
type axisFieldACL struct {
	rjFieldACL
	hiddenOn map[string]map[string]struct{}
}

func (f *axisFieldACL) HiddenFields(_ context.Context, _, _, entity string, isAdmin bool) (map[string]struct{}, error) {
	if isAdmin {
		return nil, nil
	}
	return f.hiddenOn[entity], nil
}

// axisLabelService runs one saved report over deals{stage picklist with Vietnamese valueLabels
// (Lost has none), account relation → accounts, assignee user, close_date date, amount number} for
// the rep; allow is the rep's entity-view grant set.
func axisLabelService(typ, cfg string, store RecordStore, allow map[string]bool, fACL fieldACLEnforcer) (*ReportService, *fakeLabelAndUserFetcher) {
	idx := gridOpt(`{"indexed":true}`)
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{"deals": ent("deals"), "accounts": ent("accounts")},
		fields: map[string][]metadata.FieldDefinition{
			"deals": {
				{Name: "stage", Label: "Giai đoạn", FieldType: "picklist",
					Options: gridOpt(`{"indexed":true,"values":["Proposal","Won","Lost"],"valueLabels":{"Proposal":"Báo giá","Won":"Thắng"}}`)},
				{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: gridOpt(`{"relatedEntity":"accounts"}`)},
				{Name: "assignee", Label: "Người xử lý", FieldType: "user"},
				{Name: "close_date", Label: "Ngày chốt", FieldType: "date", Options: idx},
				{Name: "amount", Label: "Giá trị", FieldType: "number"},
			},
			"accounts": {{Name: "name", Label: "Tên công ty", FieldType: "text"}},
		},
	}
	fetcher := newFakeLabelAndUserFetcher(
		map[string]string{"acc-1": "Công ty ABC", "acc-2": "Công ty XYZ"},
		map[string]dynrepo.UserLabelResult{
			"u-1": {ID: "u-1", Name: "Nguyễn Văn A", Email: "a@example.com"},
			"u-2": {ID: "u-2", Name: "Trần Thị B", Email: "b@example.com"},
		},
	)
	records := NewRecordService(cat, store).WithLabelFetcher(fetcher).WithACLAuthorizer(&rjAuthz{allow: allow})
	if fACL != nil {
		records = records.WithFieldACL(fACL)
	}
	return NewReportService(&fakeReportRepo{rec: report.Record{
		ID: "rep1", TenantID: "t1", Name: "L", EntityName: "deals", Type: typ,
		Config: json.RawMessage(cfg), IsActive: true,
	}}, records), fetcher
}

var viewDealsAndAccounts = map[string]bool{"deals": true, "accounts": true}

// joinedAxisReport is a tree over tickets joined through "assignee" (tickets.assignee_id →
// employees): stage (base picklist) → assignee.level (a picklist on employees) → assignee.dept (a
// relation on employees → depts). The two joined axes only have a label if they resolve on the
// RELATED entity's specs — tickets has neither field.
const joinedAxisReport = `{"groupBy":"stage","metric":"sum","field":"amount","joins":[{"relationship":"assignee"}],` +
	`"subGroups":[{"field":"assignee.level"},{"field":"assignee.dept"}]}`

// joinedAxisLabelService runs joinedAxisReport for the rep; allow is the rep's entity-view grant set.
func joinedAxisLabelService(allow map[string]bool) (*ReportService, *fakeLabelAndUserFetcher) {
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{"tickets": ent("tickets"), "employees": ent("employees"), "depts": ent("depts")},
		fields: map[string][]metadata.FieldDefinition{
			"tickets": {
				{Name: "stage", FieldType: "picklist", Options: gridOpt(`{"indexed":true,"values":["Won"],"valueLabels":{"Won":"Thắng"}}`)},
				{Name: "amount", FieldType: "number"},
			},
			"employees": {
				{Name: "level", FieldType: "picklist",
					Options: gridOpt(`{"indexed":true,"values":["senior","junior"],"valueLabels":{"senior":"Cao cấp","junior":"Mới"}}`)},
				{Name: "dept", FieldType: "relation", Options: gridOpt(`{"relatedEntity":"depts"}`)},
			},
			"depts": {{Name: "name", FieldType: "text"}},
		},
	}
	rollup := &rjRollup{rels: map[string]*metadata.RelationshipDefinition{
		"assignee": rel("1-N", "employees", "tickets", "assignee_id", nil),
	}}
	store := &axisLabelsStore{rjStore: rjStore{gridRows: []dynrepo.GridRow{
		gr(sp("30"), 3, "*", "*", "*"),
		gr(sp("30"), 3, "Won", "*", "*"),
		gr(sp("20"), 2, "Won", "senior", "*"),
		gr(sp("20"), 2, "Won", "senior", "d-1"),
		gr(sp("10"), 1, "Won", "junior", "*"),
		gr(sp("10"), 1, "Won", "junior", "d-2"),
	}}}
	fetcher := newFakeLabelAndUserFetcher(map[string]string{"d-1": "Phòng Kinh doanh", "d-2": "Phòng Kỹ thuật"}, nil)
	records := NewRecordService(cat, store).WithRollupMeta(rollup).WithLabelFetcher(fetcher).WithACLAuthorizer(&rjAuthz{allow: allow})
	return NewReportService(&fakeReportRepo{rec: report.Record{
		ID: "rep1", TenantID: "t1", Name: "J", EntityName: "tickets", Type: "summary",
		Config: json.RawMessage(joinedAxisReport), IsActive: true,
	}}, records), fetcher
}

// joinedTreeLabels flattens joinedAxisReport's tree into one "key=label" list per level.
func joinedTreeLabels(t *testing.T, res ReportRunResult) (lvl1, lvl2, lvl3 []string) {
	t.Helper()
	if res.Grid == nil || len(res.Grid.Groups) != 1 {
		t.Fatalf("result = %+v, want a tree with one stage", res)
	}
	root := res.Grid.Groups[0]
	lvl1 = groupLabels([]AggregateGroupResult{root.AggregateGroupResult})
	for _, c := range root.Children {
		lvl2 = append(lvl2, groupLabels([]AggregateGroupResult{c.AggregateGroupResult})...)
		for _, leaf := range c.Children {
			lvl3 = append(lvl3, groupLabels([]AggregateGroupResult{leaf.AggregateGroupResult})...)
		}
	}
	return lvl1, lvl2, lvl3
}

// labelOf renders a group's label for comparison: "∅" when it has none.
func labelOf(g AggregateGroupResult) string {
	if g.Label == nil {
		return "∅"
	}
	return *g.Label
}

func groupLabels(gs []AggregateGroupResult) []string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = keyText(g.Key) + "=" + labelOf(g)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestReportAxisLabels_OwnerRelationPicklist is T071: picklist ⇒ its valueLabels; relation ⇒ the
// target record's name; a `user` field and the owner ⇒ the person's name (never the email); a
// time bucket, a code without a Vietnamese label, a NULL key and an id that does not resolve ⇒ no
// label. One label fetch per target, whatever the number of groups. (A NULL key sorts last.)
func TestReportAxisLabels_OwnerRelationPicklist(t *testing.T) {
	ctx := context.Background()

	t.Run("one-level summary by stage: the picklist's valueLabels", func(t *testing.T) {
		store := &axisLabelsStore{groups: []dynrepo.AggregateGroup{
			{Key: sp("Proposal"), Value: sp("10"), Count: 2}, {Key: sp("Lost"), Value: sp("5"), Count: 1}, {Key: nil, Value: sp("1"), Count: 1},
		}}
		svc, _ := axisLabelService("summary", `{"groupBy":"stage","metric":"sum","field":"amount"}`, store, viewDealsAndAccounts, nil)
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if got, want := groupLabels(res.Aggregate.Groups), []string{"Proposal=Báo giá", "Lost=∅", "∅=∅"}; !equalStrings(got, want) {
			t.Fatalf("labels = %v, want %v", got, want)
		}
	})

	t.Run("one-level summary by a user field: the person's name, not the email", func(t *testing.T) {
		store := &axisLabelsStore{groups: []dynrepo.AggregateGroup{
			{Key: sp("u-1"), Value: sp("10"), Count: 2}, {Key: sp("u-9"), Value: sp("3"), Count: 1},
		}}
		svc, fetcher := axisLabelService("summary", `{"groupBy":"assignee","metric":"count"}`, store, viewDealsAndAccounts, nil)
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		// u-9 does not resolve (deleted / other tenant) ⇒ the raw id, no label.
		if got, want := groupLabels(res.Aggregate.Groups), []string{"u-1=Nguyễn Văn A", "u-9=∅"}; !equalStrings(got, want) {
			t.Fatalf("labels = %v, want %v", got, want)
		}
		if fetcher.calls["users"] != 1 {
			t.Fatalf("user label fetches = %d, want 1", fetcher.calls["users"])
		}
	})

	matrixRows := []dynrepo.GridRow{
		gr(sp("30"), 3, "*", "*"),
		gr(sp("10"), 1, "Proposal", "acc-1"),
		gr(sp("10"), 1, "Proposal", "*"),
		gr(sp("15"), 1, "Won", "acc-2"),
		gr(sp("5"), 1, "Won", "acc-1"),
		gr(sp("20"), 2, "Won", "*"),
	}
	matrixCols := []dynrepo.GridRow{gr(sp("15"), 2, "acc-1"), gr(sp("15"), 1, "acc-2")}

	t.Run("matrix stage × account: row and column labels, one fetch; the CSV prints them", func(t *testing.T) {
		store := &axisLabelsStore{rjStore: rjStore{gridRows: matrixRows, gridCols: matrixCols}}
		svc, fetcher := axisLabelService("matrix",
			`{"groupBy":"stage","metric":"sum","field":"amount","columnGroup":{"field":"account"}}`, store, viewDealsAndAccounts, nil)
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if res.Grid == nil || res.Grid.Kind != GridKindMatrix {
			t.Fatalf("result = %+v, want a matrix", res)
		}
		if got, want := groupLabels(res.Grid.Rows), []string{"Won=Thắng", "Proposal=Báo giá"}; !equalStrings(got, want) {
			t.Fatalf("row labels = %v, want %v", got, want)
		}
		// Column totals tie at 15 ⇒ key ascending: acc-1, acc-2.
		if got, want := groupLabels(res.Grid.Columns), []string{"acc-1=Công ty ABC", "acc-2=Công ty XYZ"}; !equalStrings(got, want) {
			t.Fatalf("column labels = %v, want %v", got, want)
		}
		if fetcher.calls["accounts"] != 1 {
			t.Fatalf("account label fetches = %d, want 1 (batched)", fetcher.calls["accounts"])
		}

		fetcher.calls = map[string]int{}
		var buf bytes.Buffer
		if err := svc.ExportReportCSV(ctx, gridRep, "t1", "rep1", nil, &buf); err != nil {
			t.Fatalf("ExportReportCSV: %v", err)
		}
		want := utf8BOM + "Giai đoạn \\ Khách hàng,Công ty ABC,Công ty XYZ,Tổng\n" +
			"Thắng,5,15,20\n" +
			"Báo giá,10,,10\n" +
			"Tổng,15,15,30\n"
		if got := buf.String(); got != want {
			t.Fatalf("matrix CSV =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("tree owner → user field → month: names on the first two levels, none on the bucket", func(t *testing.T) {
		store := &axisLabelsStore{rjStore: rjStore{gridRows: []dynrepo.GridRow{
			gr(sp("30"), 3, "*", "*", "*"),
			gr(sp("30"), 3, "u-1", "*", "*"),
			gr(sp("10"), 1, "u-1", "u-2", "*"),
			gr(sp("10"), 1, "u-1", "u-2", "2026-01"),
			gr(sp("20"), 2, "u-1", "∅", "*"),
			gr(sp("20"), 2, "u-1", "∅", "2026-02"),
		}}}
		svc, fetcher := axisLabelService("summary",
			`{"groupBy":"ownerId","metric":"sum","field":"amount","subGroups":[{"field":"assignee"},{"field":"close_date","bucket":"month"}]}`,
			store, viewDealsAndAccounts, nil)
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		g := res.Grid.Groups
		if len(g) != 1 || labelOf(g[0].AggregateGroupResult) != "Nguyễn Văn A" {
			t.Fatalf("level 1 = %+v, want the owner u-1 named Nguyễn Văn A", g)
		}
		lvl2 := make([]AggregateGroupResult, len(g[0].Children))
		for i, c := range g[0].Children {
			lvl2[i] = c.AggregateGroupResult
		}
		if got, want := groupLabels(lvl2), []string{"u-2=Trần Thị B", "∅=∅"}; !equalStrings(got, want) {
			t.Fatalf("level 2 labels = %v, want %v", got, want)
		}
		for _, c := range g[0].Children {
			for _, leaf := range c.Children {
				if leaf.Label != nil {
					t.Fatalf("bucket leaf %s carries label %q, want none (the screen formats dates)", keyText(leaf.Key), *leaf.Label)
				}
			}
		}
		// Owner and assignee both read the users table: one fetch per axis, not per group.
		if fetcher.calls["users"] != 2 {
			t.Fatalf("user label fetches = %d, want 2 (one per axis)", fetcher.calls["users"])
		}

		var buf bytes.Buffer
		if err := svc.ExportReportCSV(ctx, gridRep, "t1", "rep1", nil, &buf); err != nil {
			t.Fatalf("ExportReportCSV: %v", err)
		}
		want := utf8BOM + "ownerId,Người xử lý,Ngày chốt,Giá trị,Số lượng\n" +
			"Nguyễn Văn A,Trần Thị B,2026-01,10,1\n" +
			"Nguyễn Văn A,Tổng Trần Thị B,,10,1\n" +
			"Nguyễn Văn A,(trống),2026-02,20,2\n" +
			"Nguyễn Văn A,Tổng (trống),,20,2\n" +
			"Tổng Nguyễn Văn A,,,30,3\n" +
			"Tổng cộng,,,30,3\n"
		if got := buf.String(); got != want {
			t.Fatalf("tree CSV =\n%q\nwant\n%q", got, want)
		}
	})

	t.Run("joined axes resolve on the related entity: its picklist's valueLabels and its relation's name", func(t *testing.T) {
		svc, fetcher := joinedAxisLabelService(map[string]bool{"tickets": true, "employees": true, "depts": true})
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		lvl1, lvl2, lvl3 := joinedTreeLabels(t, res)
		if want := []string{"Won=Thắng"}; !equalStrings(lvl1, want) {
			t.Fatalf("base axis labels = %v, want %v (a joined report still names its base axis)", lvl1, want)
		}
		if want := []string{"senior=Cao cấp", "junior=Mới"}; !equalStrings(lvl2, want) {
			t.Fatalf("assignee.level labels = %v, want %v (employees.level's valueLabels)", lvl2, want)
		}
		if want := []string{"d-1=Phòng Kinh doanh", "d-2=Phòng Kỹ thuật"}; !equalStrings(lvl3, want) {
			t.Fatalf("assignee.dept labels = %v, want %v (the depts record's name)", lvl3, want)
		}
		if fetcher.calls["depts"] != 1 {
			t.Fatalf("dept label fetches = %d, want 1 (batched)", fetcher.calls["depts"])
		}
	})
}

// TestReportAxisLabels_NoLeakWithoutTargetView is T072: a rep who may not view the relation's
// target entity, or whose role hides the target's labelField, gets the raw key — and the name is
// never fetched, so it cannot leak one bug away from the response. The file falls back the same way.
func TestReportAxisLabels_NoLeakWithoutTargetView(t *testing.T) {
	ctx := context.Background()
	groups := []dynrepo.AggregateGroup{{Key: sp("acc-1"), Value: sp("10"), Count: 2}}
	cfg := `{"groupBy":"account","metric":"sum","field":"amount"}`

	t.Run("no view on the target entity", func(t *testing.T) {
		svc, fetcher := axisLabelService("summary", cfg, &axisLabelsStore{groups: groups}, map[string]bool{"deals": true}, nil)
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if got := groupLabels(res.Aggregate.Groups); !equalStrings(got, []string{"acc-1=∅"}) {
			t.Fatalf("labels = %v, want the raw key only", got)
		}
		if fetcher.calls["accounts"] != 0 {
			t.Fatalf("the name was fetched (%d) for a rep who may not view accounts", fetcher.calls["accounts"])
		}
	})

	t.Run("labelField hidden by field-ACL", func(t *testing.T) {
		fACL := &axisFieldACL{hiddenOn: map[string]map[string]struct{}{"accounts": {"name": {}}}}
		svc, fetcher := axisLabelService("summary", cfg, &axisLabelsStore{groups: groups}, viewDealsAndAccounts, fACL)
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if got := groupLabels(res.Aggregate.Groups); !equalStrings(got, []string{"acc-1=∅"}) {
			t.Fatalf("labels = %v, want the raw key only", got)
		}
		if fetcher.calls["accounts"] != 0 {
			t.Fatalf("the hidden name was fetched (%d)", fetcher.calls["accounts"])
		}
	})

	t.Run("the grid file falls back to the raw key the screen shows", func(t *testing.T) {
		store := &axisLabelsStore{rjStore: rjStore{
			gridRows: []dynrepo.GridRow{gr(sp("10"), 2, "*", "*"), gr(sp("10"), 2, "acc-1", "Won"), gr(sp("10"), 2, "acc-1", "*")},
		}}
		svc, fetcher := axisLabelService("summary",
			`{"groupBy":"account","metric":"sum","field":"amount","subGroups":[{"field":"stage"}]}`, store, map[string]bool{"deals": true}, nil)
		var buf bytes.Buffer
		if err := svc.ExportReportCSV(ctx, gridRep, "t1", "rep1", nil, &buf); err != nil {
			t.Fatalf("ExportReportCSV: %v", err)
		}
		want := utf8BOM + "Khách hàng,Giai đoạn,Giá trị,Số lượng\n" +
			"acc-1,Thắng,10,2\n" +
			"Tổng acc-1,,10,2\n" +
			"Tổng cộng,,10,2\n"
		if got := buf.String(); got != want || fetcher.calls["accounts"] != 0 {
			t.Fatalf("tree CSV =\n%q\nwant\n%q (account fetches %d, want 0)", got, want, fetcher.calls["accounts"])
		}
	})

	t.Run("joined relation axis without view on its target (depts)", func(t *testing.T) {
		// The rep may view the joined entity (employees) — the join runs — but not depts.
		svc, fetcher := joinedAxisLabelService(map[string]bool{"tickets": true, "employees": true})
		res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		_, lvl2, lvl3 := joinedTreeLabels(t, res)
		if want := []string{"senior=Cao cấp", "junior=Mới"}; !equalStrings(lvl2, want) {
			t.Fatalf("assignee.level labels = %v, want %v (a picklist has no target to gate)", lvl2, want)
		}
		if want := []string{"d-1=∅", "d-2=∅"}; !equalStrings(lvl3, want) {
			t.Fatalf("assignee.dept labels = %v, want the raw keys only", lvl3)
		}
		if fetcher.calls["depts"] != 0 {
			t.Fatalf("the dept name was fetched (%d) for a rep who may not view depts", fetcher.calls["depts"])
		}
	})
}

// TestReportAxisLabels_LegacySummaryAdditiveOnly is T073: the one-level summary only GAINS a label —
// the keys, values, counts and order are the aggregate engine's; the aggregate itself (dashboard,
// work queue, ⌘K) never carries one; and the one-level CSV keeps its key,value,count bytes with the
// stored code (FR-016).
func TestReportAxisLabels_LegacySummaryAdditiveOnly(t *testing.T) {
	ctx := context.Background()
	groups := []dynrepo.AggregateGroup{
		{Key: sp("Won"), Value: sp("20"), Count: 1}, {Key: sp("Proposal"), Value: sp("10"), Count: 2}, {Key: nil, Value: sp("1"), Count: 1},
	}
	store := &axisLabelsStore{groups: groups}
	svc, _ := axisLabelService("summary", `{"groupBy":"stage","metric":"sum","field":"amount"}`, store, viewDealsAndAccounts, nil)

	res, err := svc.RunReport(ctx, gridRep, "t1", "rep1", nil)
	if err != nil {
		t.Fatalf("RunReport: %v", err)
	}
	agg, err := svc.records.Aggregate(ctx, RecordScope{UserID: "u-rep", RoleID: "rep", AccessLevel: "all"}, "t1", "deals",
		AggregateQuery{Metric: "sum", Field: "amount", GroupBy: "stage"})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(res.Aggregate.Groups) != len(agg.Groups) {
		t.Fatalf("report groups = %d, aggregate groups = %d", len(res.Aggregate.Groups), len(agg.Groups))
	}
	for i, g := range agg.Groups {
		if g.Label != nil {
			t.Fatalf("RecordService.Aggregate group %d carries label %q — dashboards/work queues/⌘K must stay unlabelled", i, *g.Label)
		}
		r := res.Aggregate.Groups[i]
		if keyText(r.Key) != keyText(g.Key) || valText(r.Value) != valText(g.Value) || r.Count != g.Count {
			t.Fatalf("report group %d = %s/%s/%d, aggregate = %s/%s/%d — the label must be additive only",
				i, keyText(r.Key), valText(r.Value), r.Count, keyText(g.Key), valText(g.Value), g.Count)
		}
	}
	if got, want := groupLabels(res.Aggregate.Groups), []string{"Won=Thắng", "Proposal=Báo giá", "∅=∅"}; !equalStrings(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}

	var buf bytes.Buffer
	if err := svc.ExportReportCSV(ctx, gridRep, "t1", "rep1", nil, &buf); err != nil {
		t.Fatalf("ExportReportCSV: %v", err)
	}
	if got, want := buf.String(), utf8BOM+"key,value,count\nWon,20,1\nProposal,10,2\n,1,1\n"; got != want {
		t.Fatalf("one-level CSV = %q, want the frozen bytes %q", got, want)
	}
}
