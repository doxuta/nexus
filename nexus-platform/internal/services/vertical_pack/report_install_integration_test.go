//go:build integration

// Integration test for PACK-REPORTS: a built-in vertical pack seeds saved reports through the SAME
// validated CreateReport path as POST /admin/reports, and those reports RUN correctly on the seeded
// records. It reuses the harness in vertical_pack_integration_test.go (vpSetup / vpTenantA /
// vpTenantB / vpSeedUserID) and wires the PRODUCTION composition against REAL MySQL — plus a real
// ReportService (report.SQLRepository over sys_reports + RecordService for the run/aggregate path),
// exactly as cmd/server/main.go wires it into vertical_pack.Deps.ReportWriter.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/vertical_pack/ -race -run Report -v
package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// vpBuildReports mirrors vpBuild but ALSO wires a real ReportService as the ReportWriter so the
// pack's reports install through the SAME validated admin path (CreateReport → ValidateReportConfig)
// and can be RUN back against the seeded records. It returns the vertical-pack service and the
// report service (to list + run the installed reports). The report service is wired
// WithEntityChecker(metaRepo) — *metadata.Repository satisfies GetEntityByName — so a report
// targeting an unknown entity 422s (REPORT_ENTITY_NOT_FOUND), parity with production.
func vpBuildReports(gdb *gorm.DB, sdb *sqlx.DB) (*Service, *services.ReportService) {
	metaRepo := metadata.NewRepository(gdb)
	layoutRepo := metadata.NewLayoutRepository(gdb)
	roleRepo := auth.NewRoleRepository(gdb)
	dyn := dynrepo.New(sdb)

	mc := services.NewMetaCache(metaRepo, services.DefaultMetaTTL) // as cmd/server: a field added at (re)install is visible to the validators at once
	entitySvc := services.NewEntityService(dyn, metaRepo).WithMetaCache(mc)
	fieldSvc := services.NewFieldService(dyn, metaRepo).WithMetaCache(mc)
	layoutSvc := services.NewLayoutService(metaRepo, layoutRepo, zap.NewNop())
	roleSvc := services.NewRoleService(roleRepo)
	relationSvc := services.NewRelationService(dyn, metaRepo)
	// Mirror cmd/server/main.go's production RecordService wiring so the CRM pack's auto_number
	// samples install (WithSequenceClaimer) and the report run's aggregate path resolves fields.
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	dashboardRepo := dashboard.NewSQLRepository(sdb)
	dashboardSvc := services.NewDashboardService(dashboardRepo, recordSvc, zap.NewNop())

	reportRepo := report.NewSQLRepository(sdb)
	reportSvc := services.NewReportService(reportRepo, recordSvc).WithEntityChecker(metaRepo)

	svc := NewService(Deps{
		EntityReader:       metaRepo,
		LayoutReader:       layoutRepo,
		RoleReader:         roleRepo,
		EntityWriter:       entitySvc,
		EntityExists:       metaRepo,
		FieldWriter:        fieldSvc,
		LayoutWriter:       layoutSvc,
		RoleWriter:         roleSvc,
		RelationWriter:     relationSvc,
		RecordWriter:       recordSvc,
		Linker:             relationSvc,
		RecordExists:       dyn,
		RecordFetcher:      dyn,
		DashboardWriter:    dashboardSvc,
		ReportWriter:       reportSvc, // PACK-REPORTS: seed pack reports through the validated path
		RelationshipReader: metaRepo,
		RecordLister:       dyn,
		ObjProber:          dyn,
	})
	return svc, reportSvc
}

// vpFindReport returns the tenant-scoped report of the given name, or fails the test.
func vpFindReport(t *testing.T, ctx context.Context, reportSvc *services.ReportService, tenantID, name string) *report.Record {
	t.Helper()
	reports, err := reportSvc.ListReports(ctx, tenantID)
	if err != nil {
		t.Fatalf("list reports: %v", err)
	}
	for i := range reports {
		if reports[i].Name == name {
			return &reports[i]
		}
	}
	t.Fatalf("report %q not found among %d reports", name, len(reports))
	return nil
}

// TestVerticalPack_ReportsCRM installs the built-in CRM pack and asserts the canned
// "Pipeline theo giai đoạn" report is created, carries the right entity/type/config, RUNS to the
// correct SUM(amount) by stage on the seeded opportunities, is idempotent on re-install, and is
// strictly tenant-scoped (tenant B can neither see nor run tenant A's report).
func TestVerticalPack_ReportsCRM(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, reportSvc := vpBuildReports(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install crm into A: %v", err)
	}
	// v1.6.0 ships the summary pipeline report; v1.7.0 (PACK-REPORTS-WORKLIST) adds the tabular
	// active-contracts work-list. Both are created on install.
	if !vpReportSetEqual(res.ReportsCreated, "Pipeline theo giai đoạn", "Danh sách hợp đồng hiệu lực") {
		t.Fatalf("ReportsCreated = %v, want [Pipeline theo giai đoạn, Danh sách hợp đồng hiệu lực]", res.ReportsCreated)
	}

	// The persisted report carries the right entity/type (config shape is proven by the RUN below).
	rec := vpFindReport(t, ctx, reportSvc, vpTenantA, "Pipeline theo giai đoạn")
	if rec.EntityName != "crm_opportunities" || rec.Type != "summary" || !rec.IsActive {
		t.Errorf("report = {entity:%q type:%q active:%v}, want {crm_opportunities summary true}", rec.EntityName, rec.Type, rec.IsActive)
	}

	// --- RUN the report: SUM(amount) GROUP BY stage over the 5 seeded opportunities. ---
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	run, err := reportSvc.RunReport(ctx, scope, vpTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("run report: %v", err)
	}
	if run.Aggregate == nil || !run.Aggregate.Grouped {
		t.Fatalf("run result = %+v, want a grouped aggregate", run)
	}
	// Build stage → parsed sum. Each Value is DECIMAL text; parse to be format-agnostic.
	sums := map[string]float64{}
	for _, g := range run.Aggregate.Groups {
		if g.Key == nil {
			t.Errorf("report group with nil stage key: %+v", g)
			continue
		}
		if g.Value == nil {
			t.Errorf("report group %q: SUM value is nil — currency-sum+groupby path broken", *g.Key)
			continue
		}
		v, perr := strconv.ParseFloat(*g.Value, 64)
		if perr != nil {
			t.Errorf("report group %q: SUM value %q not numeric: %v", *g.Key, *g.Value, perr)
			continue
		}
		sums[*g.Key] = v
	}
	// Seed opportunities by stage (amount VND): Prospecting 40000, Qualification 15000,
	// Proposal 75000, Negotiation 250000, Closed Won 120000.
	wantSums := map[string]float64{
		"Prospecting":   40000,
		"Qualification": 15000,
		"Proposal":      75000,
		"Negotiation":   250000,
		"Closed Won":    120000,
	}
	if len(sums) != len(wantSums) {
		t.Errorf("report groups = %v, want %d stages %v", sums, len(wantSums), wantSums)
	}
	for stage, want := range wantSums {
		if got := sums[stage]; got != want {
			t.Errorf("report SUM(amount) for stage %q = %v, want %v", stage, got, want)
		}
	}

	// --- PACK-REPORTS-WORKLIST: RUN the tabular active-contracts report. The seed has 3 contracts
	// (1 "Hiệu lực", 1 "Nháp", 1 "Đã gia hạn"); the status eq "Hiệu lực" filter must narrow to the
	// ONE active contract — a broken filter would return all 3, failing this assertion. ---
	wl := vpFindReport(t, ctx, reportSvc, vpTenantA, "Danh sách hợp đồng hiệu lực")
	if wl.EntityName != "crm_contracts" || wl.Type != "tabular" || !wl.IsActive {
		t.Errorf("worklist report = {entity:%q type:%q active:%v}, want {crm_contracts tabular true}", wl.EntityName, wl.Type, wl.IsActive)
	}
	wlRun, err := reportSvc.RunReport(ctx, scope, vpTenantA, wl.ID, nil)
	if err != nil {
		t.Fatalf("run worklist report: %v", err)
	}
	if wlRun.Tabular == nil {
		t.Fatalf("worklist run result = %+v, want a tabular result", wlRun)
	}
	// Columns round-trip through persist→load unchanged (proves the projection list is stored).
	wantCols := []string{"contract_number", "account", "end_date", "value", "status"}
	if !vpStrSliceEqual(wlRun.Tabular.Columns, wantCols) {
		t.Errorf("worklist columns = %v, want %v", wlRun.Tabular.Columns, wantCols)
	}
	if wlRun.Tabular.Total != 1 || len(wlRun.Tabular.Records) != 1 {
		t.Fatalf("worklist rows = total %d / %d records, want 1 (only the 'Hiệu lực' contract; filter broken if 3)", wlRun.Tabular.Total, len(wlRun.Tabular.Records))
	}
	row := wlRun.Tabular.Records[0]
	if s, _ := row.Data["status"].(string); s != "Hiệu lực" {
		t.Errorf("worklist row status = %q, want %q", s, "Hiệu lực")
	}
	// The relation column `account` must project through the tabular run AND come back named:
	// since #15/#16 a report resolves relation columns to {id,label} the same way the record grid
	// does, so the customer reads "Acme Corp" instead of a 36-character UUID. Asserting the label
	// (not just a non-empty cell) is what pins that promise — a regression to the bare id fails here.
	if id, label := vpRelationCell(t, row.Data["account"]); id == "" || label == "" {
		t.Errorf("worklist row account = {id:%q label:%q}, want both set (relation label, #15/#16); data=%v", id, label, row.Data)
	}
	if ed, _ := row.Data["end_date"].(string); ed == "" {
		t.Errorf("worklist row end_date is empty, want the seeded date; data=%v", row.Data)
	}

	// --- Re-install: both reports already exist by name → skipped, no duplicate. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("re-install crm: %v", err)
	}
	if len(res2.ReportsCreated) != 0 || len(res2.ReportsSkipped) != 2 {
		t.Errorf("re-install reports: created %v / skipped %v, want 0 / 2 (idempotent by name)", res2.ReportsCreated, res2.ReportsSkipped)
	}
	after, err := reportSvc.ListReports(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list reports after re-install: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("reports after re-install = %d, want 2 (no duplicate)", len(after))
	}

	// --- Tenant isolation: tenant B (fresh, no install) can neither see nor run A's report. ---
	bReports, err := reportSvc.ListReports(ctx, vpTenantB)
	if err != nil {
		t.Fatalf("list reports B: %v", err)
	}
	if len(bReports) != 0 {
		t.Errorf("tenant B reports = %d, want 0 (A's report must not leak)", len(bReports))
	}
	if _, err := reportSvc.RunReport(ctx, scope, vpTenantB, rec.ID, nil); err == nil {
		t.Error("running A's report under tenant B succeeded, want REPORT_NOT_FOUND (tenant-scoped)")
	} else {
		var ve *services.ValidationError
		if !errors.As(err, &ve) || ve.Code != "REPORT_NOT_FOUND" {
			t.Errorf("cross-tenant run error = %v, want REPORT_NOT_FOUND", err)
		}
	}
}

// TestVerticalPack_ReportsTicketing installs the built-in Ticketing pack and asserts the canned
// "Ticket theo trạng thái" report is created and RUNS to the correct COUNT by status distribution
// over the 6 seeded tickets (Open 3, Pending 1, Resolved 1, Closed 1).
func TestVerticalPack_ReportsTicketing(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, reportSvc := vpBuildReports(gdb, sdb)
	ctx := context.Background()

	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install ticketing into A: %v", err)
	}
	// v1.2.0 ships the summary status breakdown; v1.3.0 (PACK-REPORTS-WORKLIST) adds the tabular
	// open-tickets work-list. Both are created on install.
	if !vpReportSetEqual(res.ReportsCreated, "Ticket theo trạng thái", "Danh sách ticket đang mở") {
		t.Fatalf("ReportsCreated = %v, want [Ticket theo trạng thái, Danh sách ticket đang mở]", res.ReportsCreated)
	}

	rec := vpFindReport(t, ctx, reportSvc, vpTenantA, "Ticket theo trạng thái")
	if rec.EntityName != "tk_tickets" || rec.Type != "summary" {
		t.Errorf("report = {entity:%q type:%q}, want {tk_tickets summary}", rec.EntityName, rec.Type)
	}

	// --- RUN the report: COUNT GROUP BY status over the 6 seeded tickets. ---
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	run, err := reportSvc.RunReport(ctx, scope, vpTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("run report: %v", err)
	}
	if run.Aggregate == nil || !run.Aggregate.Grouped {
		t.Fatalf("run result = %+v, want a grouped aggregate", run)
	}
	counts := map[string]int64{}
	for _, g := range run.Aggregate.Groups {
		if g.Key == nil {
			t.Errorf("report group with nil status key: %+v", g)
			continue
		}
		counts[*g.Key] = g.Count
	}
	wantCounts := map[string]int64{"Open": 3, "Pending": 1, "Resolved": 1, "Closed": 1}
	if len(counts) != len(wantCounts) {
		t.Errorf("report groups = %v, want %v", counts, wantCounts)
	}
	for status, want := range wantCounts {
		if got := counts[status]; got != want {
			t.Errorf("report COUNT for status %q = %d, want %d", status, got, want)
		}
	}

	// --- PACK-REPORTS-WORKLIST: RUN the tabular open-tickets report. Of the 6 seeded tickets
	// (Open 3, Pending 1, Resolved 1, Closed 1), the status IN {Open, Pending} filter must narrow
	// to the 4 open-work tickets — a broken filter would return all 6, failing this assertion.
	// This exercises the `in` operator (two values) on a picklist, not just eq. ---
	wl := vpFindReport(t, ctx, reportSvc, vpTenantA, "Danh sách ticket đang mở")
	if wl.EntityName != "tk_tickets" || wl.Type != "tabular" || !wl.IsActive {
		t.Errorf("worklist report = {entity:%q type:%q active:%v}, want {tk_tickets tabular true}", wl.EntityName, wl.Type, wl.IsActive)
	}
	wlRun, err := reportSvc.RunReport(ctx, scope, vpTenantA, wl.ID, nil)
	if err != nil {
		t.Fatalf("run worklist report: %v", err)
	}
	if wlRun.Tabular == nil {
		t.Fatalf("worklist run result = %+v, want a tabular result", wlRun)
	}
	wantCols := []string{"subject", "priority", "status", "due_date", "assigned_to"}
	if !vpStrSliceEqual(wlRun.Tabular.Columns, wantCols) {
		t.Errorf("worklist columns = %v, want %v", wlRun.Tabular.Columns, wantCols)
	}
	if wlRun.Tabular.Total != 4 || len(wlRun.Tabular.Records) != 4 {
		t.Fatalf("worklist rows = total %d / %d records, want 4 (Open+Pending; filter broken if 6)", wlRun.Tabular.Total, len(wlRun.Tabular.Records))
	}
	// Every returned row is in the filtered status set, and its projected scalar columns are populated.
	for _, r := range wlRun.Tabular.Records {
		s, _ := r.Data["status"].(string)
		if s != "Open" && s != "Pending" {
			t.Errorf("worklist row status = %q, want Open or Pending; data=%v", s, r.Data)
		}
		if subj, _ := r.Data["subject"].(string); subj == "" {
			t.Errorf("worklist row subject is empty, want the seeded subject; data=%v", r.Data)
		}
		if pri, _ := r.Data["priority"].(string); pri == "" {
			t.Errorf("worklist row priority is empty, want the seeded priority; data=%v", r.Data)
		}
	}
}

// TestVerticalPack_ReportBrokenConfigFailsInstall proves a malformed pack report fails install with
// the REAL report validation code (no second, weaker write path): an invalid report Type ("pie") is
// rejected by ValidateReportConfig — the SAME body POST /admin/reports runs — so the install aborts
// with REPORT_TYPE_INVALID rather than persisting an unrunnable report. (A non-indexed/missing
// groupBy FIELD is NOT caught here by design — CreateReport uses shape-only validation, deferring the
// deep field checks to RUN time, exactly like the admin apply path; the two RUN tests above prove
// the canned reports actually execute.)
func TestVerticalPack_ReportBrokenConfigFailsInstall(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildReports(gdb, sdb)
	ctx := context.Background()

	badType := "pie" // not tabular/summary/matrix → REPORT_TYPE_INVALID in ValidateReportConfig
	pack := &Pack{
		Manifest: &Manifest{Name: "Broken", Slug: "broken-report", Version: "1.0.0"},
		Entities: []PackEntity{
			{Name: "br_tickets", Label: "Tickets", Fields: []PackField{
				{Name: "status", Label: "Status", FieldType: "text"},
			}},
		},
		Reports: []PackReport{
			{
				Name:       "Bad report",
				EntityName: "br_tickets", // entity IS valid → we exercise CONFIG validation, not entity
				Type:       badType,
				Config:     PackReportConfig{Metric: "count", GroupBy: "status"},
			},
		},
	}

	_, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err == nil {
		t.Fatal("install with a bad report type succeeded, want REPORT_TYPE_INVALID")
	}
	var ve *services.ValidationError
	if !errors.As(err, &ve) || ve.Code != "REPORT_TYPE_INVALID" {
		t.Fatalf("install error = %v, want REPORT_TYPE_INVALID (real report validator ran)", err)
	}
}

// TestVerticalPack_ReportExportIncludesReports proves PACK-EXPORT-FULL carries saved reports:
// installing the CRM pack (which ships reports) then EXPORTING that tenant yields a pack whose
// Reports slice is populated, each report structurally intact (name/entity/type) — so an exported
// vertical re-installs with its reports, not just its entity metadata. (Flipped an earlier
// assertion that reports were export-omitted; export now derives them from the tenant's saved
// reports, parity with dashboards/workflows/teams.)
func TestVerticalPack_ReportExportIncludesReports(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildReports(gdb, sdb)
	ctx := context.Background()

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm); err != nil {
		t.Fatalf("install crm: %v", err)
	}
	exported, _, err := svc.Export(ctx, vpTenantA, nil, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported.Reports) == 0 {
		t.Fatalf("exported pack Reports is empty, want the CRM pack's saved reports carried through (PACK-EXPORT-FULL)")
	}
	for i, r := range exported.Reports {
		if r.Name == "" || r.EntityName == "" || r.Type == "" {
			t.Errorf("exported report[%d] = %+v, want non-empty Name/EntityName/Type (structural round-trip)", i, r)
		}
	}
}

// TestVerticalPack_ReportsLMS installs the built-in LMS pack (v1.2.0) and asserts the canned tabular
// "Danh sách ghi danh quá hạn" work-list report is created and RUNS to EXACTLY the overdue enrolment.
// The seed has 6 enrolments, only ONE with status "Quá hạn"; the status eq "Quá hạn" filter must
// narrow to that single row — a broken filter would return all 6, failing the assertion. The report
// projects the two FK relation columns student/course, which come back populated (STORED generated
// columns off data.$.student / data.$.course → target UUIDs), proving relation columns project.
func TestVerticalPack_ReportsLMS(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, reportSvc := vpBuildReports(gdb, sdb)
	ctx := context.Background()

	pack, ok := BuiltinPack("lms")
	if !ok {
		t.Fatal("builtin lms pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install lms into A: %v", err)
	}
	if len(res.ReportsCreated) != 1 || res.ReportsCreated[0] != "Danh sách ghi danh quá hạn" {
		t.Fatalf("ReportsCreated = %v, want [Danh sách ghi danh quá hạn]", res.ReportsCreated)
	}

	rec := vpFindReport(t, ctx, reportSvc, vpTenantA, "Danh sách ghi danh quá hạn")
	if rec.EntityName != "lms_enrollments" || rec.Type != "tabular" || !rec.IsActive {
		t.Errorf("report = {entity:%q type:%q active:%v}, want {lms_enrollments tabular true}", rec.EntityName, rec.Type, rec.IsActive)
	}

	// --- RUN the tabular report: status eq "Quá hạn" over the 6 seeded enrolments → exactly 1 row. ---
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	run, err := reportSvc.RunReport(ctx, scope, vpTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("run report: %v", err)
	}
	if run.Tabular == nil {
		t.Fatalf("run result = %+v, want a tabular result", run)
	}
	wantCols := []string{"student", "course", "tuition_due", "status"}
	if !vpStrSliceEqual(run.Tabular.Columns, wantCols) {
		t.Errorf("worklist columns = %v, want %v", run.Tabular.Columns, wantCols)
	}
	if run.Tabular.Total != 1 || len(run.Tabular.Records) != 1 {
		t.Fatalf("worklist rows = total %d / %d records, want 1 (only the 'Quá hạn' enrolment; filter broken if 6)", run.Tabular.Total, len(run.Tabular.Records))
	}
	row := run.Tabular.Records[0]
	if s, _ := row.Data["status"].(string); s != "Quá hạn" {
		t.Errorf("worklist row status = %q, want %q", s, "Quá hạn")
	}
	// Relation columns student/course must project AND resolve to names (#15/#16) — a tuition
	// worklist that prints two UUIDs per row is unusable to the person chasing the payment.
	if id, label := vpRelationCell(t, row.Data["student"]); id == "" || label == "" {
		t.Errorf("worklist row student = {id:%q label:%q}, want both set (relation label, #15/#16); data=%v", id, label, row.Data)
	}
	if id, label := vpRelationCell(t, row.Data["course"]); id == "" || label == "" {
		t.Errorf("worklist row course = {id:%q label:%q}, want both set (relation label, #15/#16); data=%v", id, label, row.Data)
	}
	if td, _ := row.Data["tuition_due"].(string); td == "" {
		t.Errorf("worklist row tuition_due is empty, want the seeded date; data=%v", row.Data)
	}

	// --- Re-install: the report already exists by name → skipped, no duplicate. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install lms: %v", err)
	}
	if len(res2.ReportsCreated) != 0 || len(res2.ReportsSkipped) != 1 {
		t.Errorf("re-install reports: created %v / skipped %v, want 0 / 1 (idempotent by name)", res2.ReportsCreated, res2.ReportsSkipped)
	}
}

// vpReportSetEqual reports whether got contains exactly the wanted report names (order-independent).
func vpReportSetEqual(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(got))
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}

// vpStrSliceEqual reports whether two string slices are identical in order and content.
func vpStrSliceEqual(a, b []string) bool {
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

// vpRelationCell reads a relation/user column out of a tabular report row.
//
// REPORT-RELATION-LABELS (#15/#16, merged 2026-09-11) changed what such a cell CONTAINS: a report
// used to project the stored FK and hand back a bare 36-char UUID string, which is what a customer
// then saw on screen and in their CSV. Reports now go through the same expand the record grid uses,
// so the cell is an object — {id, label}, or {id} alone when field-level ACL withholds the target's
// label (EXP-01). These pack tests asserted the old string shape and went red on merge; they are the
// only place in the repo that still spoke the pre-#15 contract.
//
// Returns (id, label). label is "" for the ACL-withheld shape. Goes through JSON rather than a type
// assertion on purpose: services.expandedValue is unexported, and JSON is the contract this row
// actually travels over to the frontend — asserting on the wire shape is the point.
func vpRelationCell(t *testing.T, v any) (id, label string) {
	t.Helper()
	if s, ok := v.(string); ok {
		// Pre-#15 shape. Kept reachable rather than rejected so a REGRESSION back to the bare id
		// surfaces as a missing label at the call site, with the column named, instead of a panic.
		return s, ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("relation cell is not serialisable: %T %v", v, v)
	}
	var out struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("relation cell is neither a bare id nor {id,label}: %s", raw)
	}
	return out.ID, out.Label
}
