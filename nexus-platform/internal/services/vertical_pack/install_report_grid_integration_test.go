//go:build integration

// specs/049 S7b T078a: a pack carrying the grid-report keys (columnGroup + having on a matrix,
// subGroups on a summary) must survive the whole chain on REAL MySQL — export from one tenant, pack
// JSON, install into a second tenant, and RUN there as a non-admin: no key is dropped, the numbers
// are the rep's own, and a second install diffs 100% unchanged (spec 011 gate).
package vertical_pack

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

func TestPackReport_GridKeysInstallThenRunAsNonAdmin(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, reportSvc := vpBuildReports(gdb, sdb)
	store := newFakeItemStore()
	svc.WithItemStore(store)
	ctx := context.Background()

	const repID = "00000000-0000-0000-0000-0000000000b2"
	const instBID = "00000000-0000-0000-0000-0000000000b1"
	for _, u := range []struct{ id, tenant, email string }{
		{vpSeedUserID, vpTenantA, "gpa@it.example.com"},
		{instBID, vpTenantB, "gpb@it.example.com"},
		{repID, vpTenantB, "gprep@it.example.com"},
	} {
		if err := gdb.Exec("INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
			u.id, u.tenant, u.email, "x").Error; err != nil {
			t.Fatalf("seed user %s: %v", u.email, err)
		}
	}

	idx := map[string]any{"indexed": true}
	src := &Pack{
		Manifest: &Manifest{Name: "Grid Pack", Slug: "gridpack", Version: "1.0.0"},
		Entities: []PackEntity{{Name: "gp_opps", Label: "Cơ hội", Fields: []PackField{
			{Name: "stage", Label: "Giai đoạn", FieldType: "text", Options: idx},
			{Name: "close_date", Label: "Ngày chốt", FieldType: "date", Options: idx},
			{Name: "amount", Label: "Giá trị", FieldType: "number"},
		}}},
		Reports: []PackReport{
			{Name: "Giai đoạn × tháng", EntityName: "gp_opps", Type: "matrix", Config: PackReportConfig{
				GroupBy: "stage", Metric: "sum", Field: "amount",
				ColumnGroup: &PackReportGroup{Field: "close_date", Bucket: "month"},
				Having:      &PackReportHaving{On: "value", Op: "gt", Values: []string{"15"}}}},
			{Name: "Giai đoạn → tháng", EntityName: "gp_opps", Type: "summary", Config: PackReportConfig{
				GroupBy: "stage", Metric: "sum", Field: "amount",
				SubGroups: []PackReportGroup{{Field: "close_date", Bucket: "month"}}}},
		},
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, src); err != nil {
		t.Fatalf("install source pack into A: %v", err)
	}

	// Export from A -> wire JSON: every grid key must be on the wire.
	exported, _, err := svc.Export(ctx, vpTenantA, []string{"gp_opps"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	wire := mustJSON(t, exported)
	var pack Pack
	if err := json.Unmarshal(wire, &pack); err != nil {
		t.Fatalf("pack JSON: %v", err)
	}
	got := map[string]PackReportConfig{}
	for _, r := range pack.Reports {
		got[r.Name] = r.Config
	}
	if c := got["Giai đoạn × tháng"]; c.ColumnGroup == nil || c.ColumnGroup.Field != "close_date" || c.Having == nil || c.Having.Op != "gt" {
		t.Fatalf("exported matrix lost columnGroup/having: %+v", c)
	}
	if c := got["Giai đoạn → tháng"]; len(c.SubGroups) != 1 || c.SubGroups[0].Bucket != "month" {
		t.Fatalf("exported summary lost subGroups: %+v", c)
	}
	pack.Manifest = &Manifest{Name: "Grid Pack", Slug: "gridpack", Version: "1.0.0"}

	// Install into the SECOND tenant.
	if _, err := svc.Install(ctx, vpTenantB, instBID, &pack); err != nil {
		t.Fatalf("install into B: %v", err)
	}

	// Records: the rep owns two, the installer one (a third person's row the rep must never see).
	metaRepo := metadata.NewRepository(gdb)
	recordSvc := services.NewRecordService(metaRepo, dynrepo.New(sdb))
	admin := services.RecordScope{UserID: instBID, IsAdmin: true, AccessLevel: "all"}
	for _, r := range []struct {
		owner, stage, date string
		amount             float64
	}{
		{repID, "Proposal", "2026-01-15", 10.5},
		{repID, "Proposal", "2026-02-10", 20.25},
		{instBID, "Proposal", "2026-01-05", 999},
	} {
		if _, err := recordSvc.Create(ctx, admin, vpTenantB, r.owner, "gp_opps",
			map[string]any{"stage": r.stage, "close_date": r.date, "amount": r.amount}); err != nil {
			t.Fatalf("seed record: %v", err)
		}
	}
	rep := services.RecordScope{UserID: repID, RoleID: "crm_sales_rep", AccessLevel: "owner"}

	// Matrix: having > 15 keeps only the Feb cell (20.25); the January 10.5 cell is cut, and 999 is not the rep's.
	m := vpFindReport(t, ctx, reportSvc, vpTenantB, "Giai đoạn × tháng")
	run, err := reportSvc.RunReport(ctx, rep, vpTenantB, m.ID, nil)
	if err != nil {
		t.Fatalf("rep runs installed matrix: %v", err)
	}
	if run.Grid == nil || run.Grid.Kind != services.GridKindMatrix || len(run.Axes) != 2 || run.Having == nil {
		t.Fatalf("installed matrix ran as %+v (axes %v, having %v), want a cross-tab with a threshold", run.Grid, run.Axes, run.Having)
	}
	if len(run.Grid.Cells) != 1 || !vpDecEq(run.Grid.Cells[0].Value, "20.25") {
		t.Fatalf("matrix cells = %+v, want exactly the 20.25 cell (having kept, 999 not the rep's)", run.Grid.Cells)
	}

	// Tree: stage -> month, the rep's own 30.75 over 2 rows.
	tr := vpFindReport(t, ctx, reportSvc, vpTenantB, "Giai đoạn → tháng")
	trun, err := reportSvc.RunReport(ctx, rep, vpTenantB, tr.ID, nil)
	if err != nil {
		t.Fatalf("rep runs installed tree: %v", err)
	}
	if trun.Grid == nil || trun.Grid.Kind != services.GridKindTree || len(trun.Grid.Groups) != 1 || len(trun.Grid.Groups[0].Children) != 2 {
		t.Fatalf("installed tree ran as %+v, want 1 stage with 2 month children", trun.Grid)
	}
	if v := trun.Grid.Total.Value; !vpDecEq(v, "30.75") || trun.Grid.Total.Count != 2 {
		t.Fatalf("tree total = %v/%d, want 30.75/2 (rep's own rows)", v, trun.Grid.Total.Count)
	}

	// Spec 011 gate: install again -> every item, grid reports included, diffs unchanged.
	if _, err := svc.Install(ctx, vpTenantB, instBID, &pack); err != nil {
		t.Fatalf("re-install into B: %v", err)
	}
	diff, err := svc.DiffInstalled(ctx, vpTenantB, "gridpack")
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	seen := 0
	for _, it := range diff.Items {
		if it.Status != "unchanged" {
			t.Errorf("%s/%s status=%s changed=%v, want unchanged", it.ItemType, it.ItemKey, it.Status, it.ChangedPaths)
		}
		if it.ItemType == ItemReport {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("diff lists %d reports, want 2", seen)
	}
}

// vpDecEq compares MySQL decimal text as exact rationals ("20.250000" == "20.25").
func vpDecEq(got *string, want string) bool {
	if got == nil {
		return false
	}
	g, ok1 := new(big.Rat).SetString(*got)
	w, ok2 := new(big.Rat).SetString(want)
	return ok1 && ok2 && g.Cmp(w) == 0
}
