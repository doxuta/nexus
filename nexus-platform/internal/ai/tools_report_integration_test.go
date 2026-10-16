//go:build integration

// Integration tests for the M8 report tool against the REAL *metadata.Repository and the REAL
// services.ReportService. They prove what the in-memory fake cannot: (a) propose_report writes
// NOTHING to sys_reports, and (b) VALIDATOR PARITY — the exact payload a propose buffered is
// accepted by the real CreateReport apply path (accept parity), an invalid payload is rejected by
// BOTH sides with the SAME ValidationError code (reject parity), and a DEEP-invalid payload (the
// tool's stricter surface) is rejected by the tool while CreateReport still accepts it — the
// documented deferred-to-run-time design — all through one exported body:
// services.ValidateReportConfig.
//
// Run with (throwaway DB migrated per the gate recipe — single-source TEST_DATABASE_URL, B7):
//
//	mysql -h127.0.0.1 -uroot -e "CREATE DATABASE IF NOT EXISTS nexus_it"
//	/opt/homebrew/bin/migrate -path migrations -database "mysql://root@tcp(127.0.0.1:3306)/nexus_it" up
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_it?parseTime=true&multiStatements=true&charset=utf8mb4" \
//	  go test -tags=integration ./internal/ai/ -run TestToolsReport_IT -count=1
package ai

import (
	"context"
	"errors"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

const (
	repITTenantA = "aiaiaiai-m8m8-4444-8888-tenanta00001"
	repITTenantB = "aiaiaiai-m8m8-4444-8888-tenantb00002"
	repITEntity  = "it_ai_m8_orders" // 15 chars — within SanitizeIdentifier's 26-char cap
)

// cleanupReportIT hard-deletes every row the suite seeds, keyed by its throwaway tenant IDs, so
// reruns are idempotent and nothing leaks into other suites sharing the DB.
func cleanupReportIT(gdb *gorm.DB) {
	gdb.Exec(`DELETE FROM sys_reports WHERE tenant_id IN (?, ?)`, repITTenantA, repITTenantB)
	gdb.Exec(`DELETE FROM sys_fields WHERE tenant_id IN (?, ?)`, repITTenantA, repITTenantB)
	gdb.Exec(`DELETE FROM sys_entities WHERE tenant_id IN (?, ?)`, repITTenantA, repITTenantB)
}

// seedReportIT registers the entity under both tenants; tenant A's copy carries an indexed
// picklist (status — a groupable/filterable column) plus a number field (price — the sum target).
// Tenant B's copy has NO fields, so any field reference valid for A is a deep-reject for B.
func seedReportIT(t *testing.T, gdb *gorm.DB) {
	t.Helper()
	cleanupReportIT(gdb)
	t.Cleanup(func() { cleanupReportIT(gdb) })

	seed := []struct {
		entID, tenant string
	}{
		{"itai-m8e1-0000-0000-000000000001", repITTenantA},
		{"itai-m8e2-0000-0000-000000000002", repITTenantB},
	}
	for _, s := range seed {
		ent := metadata.EntityDefinition{
			ID:        s.entID,
			TenantID:  s.tenant,
			Name:      repITEntity,
			Label:     "M8 IT orders",
			DataTable: "obj_" + repITEntity,
		}
		if err := gdb.Create(&ent).Error; err != nil {
			t.Fatalf("seed entity for %s: %v", s.tenant, err)
		}
	}

	status := `{"values":["open","closed"],"indexed":true}`
	fields := []metadata.FieldDefinition{
		{ID: "itai-m8f1-0000-0000-000000000001", EntityID: seed[0].entID, TenantID: repITTenantA,
			Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: &status, FieldOrder: 10},
		{ID: "itai-m8f2-0000-0000-000000000002", EntityID: seed[0].entID, TenantID: repITTenantA,
			Name: "price", Label: "Giá", FieldType: "number", FieldOrder: 20},
	}
	for i := range fields {
		if err := gdb.Create(&fields[i]).Error; err != nil {
			t.Fatalf("seed field %s: %v", fields[i].Name, err)
		}
	}
}

// draftToCreateReportInput maps a buffered DraftReport onto the apply path's CreateReportInput
// EXACTLY the way the FE Apply button maps it onto the POST /admin/reports body (1:1 fields) — so
// the parity assertion exercises the same payload on both sides.
func draftToCreateReportInput(d *DraftReport) services.CreateReportInput {
	cfg := services.ReportConfig{
		Columns: d.Config.Columns,
		GroupBy: d.Config.GroupBy,
		Metric:  d.Config.Metric,
		Field:   d.Config.Field,
		Bucket:  d.Config.Bucket,
	}
	for _, f := range d.Config.Filters {
		cfg.Filters = append(cfg.Filters, services.ReportFilterInput{Field: f.Field, Op: f.Op, Values: f.Values})
	}
	return services.CreateReportInput{
		Name:       d.Name,
		EntityName: d.EntityName,
		Type:       d.Type,
		Config:     cfg,
		IsActive:   &d.IsActive,
	}
}

func TestToolsReport_IT_ProposeParity_CreateReportRoundTrip(t *testing.T) {
	repo, gdb := newRelITReader(t) // same single-source DSN helper as the M5/M6/M7 suites
	sqlxDB := connectAI(t)         // sqlx over the SAME DSN for the real report repository
	seedReportIT(t, gdb)
	ctx := context.Background()

	execA := &toolExecutor{reader: repo, tenantID: repITTenantA,
		docs: &DocReaders{Reports: services.NewReportService(report.NewSQLRepository(sqlxDB), nil).WithEntityChecker(repo)}}

	// Happy path: a bucketed count over created_at, narrowed by an indexed-picklist filter.
	goodArgs := `{
		"name": "Đơn chốt theo tháng",
		"entity": "` + repITEntity + `",
		"reportType": "summary",
		"groupBy": "created_at",
		"metric": "count",
		"bucket": "month",
		"filters": [{"field": "status", "op": "eq", "values": ["closed"]}]
	}`
	res := execA.execute(ctx, "propose_report", goodArgs)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_report should succeed against real metadata: %s", res)
	}
	if len(execA.draft) != 1 || execA.draft[0].Kind != "report" || execA.draft[0].Report == nil {
		t.Fatalf("expected 1 buffered report draft, got %+v", execA.draft)
	}

	// HITL: nothing was written — the draft is a proposal, not a sys_reports row.
	var n int64
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ?`, repITTenantA).
		Scan(&n).Error; err != nil {
		t.Fatalf("count check: %v", err)
	}
	if n != 0 {
		t.Fatalf("propose_report must NOT write sys_reports; found %d rows", n)
	}

	// PARITY (accept side): applying the buffered draft VERBATIM through the real CreateReport —
	// the apply path behind POST /admin/reports, entity-existence armed via the metadata repo —
	// must succeed, because the tool validated with the SAME exported body
	// (services.ValidateReportConfig) CreateReport runs.
	svc := services.NewReportService(report.NewSQLRepository(sqlxDB), nil).WithEntityChecker(repo)
	rec, err := svc.CreateReport(ctx, repITTenantA, draftToCreateReportInput(execA.draft[0].Report))
	if err != nil {
		t.Fatalf("parity broken: CreateReport rejected a report propose_report accepted: %v", err)
	}
	if rec == nil || rec.ID == "" {
		t.Fatalf("CreateReport returned no persisted row: %+v", rec)
	}

	// PARITY (reject side): an unknown metric is a SHAPE violation — rejected by the TOOL with
	// REPORT_CONFIG_INVALID...
	badArgs := `{"name":"Sai metric","entity":"` + repITEntity + `","reportType":"summary",
		"groupBy":"created_at","metric":"median"}`
	resBad := execA.execute(ctx, "propose_report", badArgs)
	okBad, mBad := parseToolResult(t, resBad)
	if okBad {
		t.Fatalf("propose_report must reject an unknown metric: %s", resBad)
	}
	if errStr, _ := mBad["error"].(string); !strings.Contains(errStr, "REPORT_CONFIG_INVALID") {
		t.Fatalf("expected REPORT_CONFIG_INVALID from the tool, got %q", errStr)
	}
	if len(execA.draft) != 1 { // still only the good draft
		t.Fatalf("rejected proposal must not buffer, got %+v", execA.draft)
	}
	// ...and by the REAL CreateReport with the SAME code — both sides run one validator body.
	_, errCreate := svc.CreateReport(ctx, repITTenantA, services.CreateReportInput{
		Name: "Sai metric", EntityName: repITEntity, Type: "summary",
		Config: services.ReportConfig{GroupBy: "created_at", Metric: "median"},
	})
	var ve *services.ValidationError
	if !errors.As(errCreate, &ve) || ve.Code != "REPORT_CONFIG_INVALID" {
		t.Fatalf("parity broken: CreateReport should reject with REPORT_CONFIG_INVALID, got %v", errCreate)
	}

	// DEEP divergence (pinned by design): a bucket on the non-date id column is rejected by the
	// TOOL with the RUN-TIME code (AGGREGATE_BUCKET_FIELD_NOT_DATE — deep mode armed by
	// liveFields), while CreateReport ACCEPTS it because the apply path defers deep semantics to
	// run time (a metadata change after save must surface honestly at run). If this assertion
	// ever flips, the apply path started deep-validating — update the tool contract deliberately.
	deepArgs := `{"name":"Bucket sai","entity":"` + repITEntity + `","reportType":"summary",
		"groupBy":"id","metric":"count","bucket":"month"}`
	okDeep, mDeep := parseToolResult(t, execA.execute(ctx, "propose_report", deepArgs))
	if okDeep {
		t.Fatal("propose_report must deep-reject a bucket on a non-date groupBy")
	}
	if errStr, _ := mDeep["error"].(string); !strings.Contains(errStr, "AGGREGATE_BUCKET_FIELD_NOT_DATE") {
		t.Fatalf("expected AGGREGATE_BUCKET_FIELD_NOT_DATE from the tool, got %q", errStr)
	}
	recDeep, errDeep := svc.CreateReport(ctx, repITTenantA, services.CreateReportInput{
		Name: "Bucket sai", EntityName: repITEntity, Type: "summary",
		Config: services.ReportConfig{GroupBy: "id", Metric: "count", Bucket: "month"},
	})
	if errDeep != nil || recDeep == nil {
		t.Fatalf("apply path must still accept (deep checks deferred to run time), got %v", errDeep)
	}

	// Final ledger: exactly the TWO applied rows exist (the propose calls added nothing).
	if err := gdb.Raw(`SELECT COUNT(*) FROM sys_reports WHERE tenant_id = ?`, repITTenantA).
		Scan(&n).Error; err != nil {
		t.Fatalf("final count check: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected exactly 2 sys_reports rows (the applied ones), found %d", n)
	}
}

func TestToolsReport_IT_TenantIsolation(t *testing.T) {
	repo, gdb := newRelITReader(t)
	seedReportIT(t, gdb)
	ctx := context.Background()

	// Tenant B owns an entity with the SAME name but NONE of tenant A's fields — a report valid
	// for tenant A must deep-fail for tenant B on the unknown-field check, proving the tool
	// resolved liveFields through tenant B's rows only (never tenant A's).
	execB := &toolExecutor{reader: repo, tenantID: repITTenantB}
	args := `{"name":"r","entity":"` + repITEntity + `","reportType":"summary",
		"groupBy":"created_at","metric":"sum","field":"price"}`
	ok, m := parseToolResult(t, execB.execute(ctx, "propose_report", args))
	if ok {
		t.Fatal("tenant B must not validate against tenant A's fields")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "AGGREGATE_FIELD_UNKNOWN") {
		t.Fatalf("expected AGGREGATE_FIELD_UNKNOWN (tenant B has no 'price'), got %q", errStr)
	}

	// And an entity that exists ONLY under other tenants resolves as missing.
	execA := &toolExecutor{reader: repo, tenantID: repITTenantA}
	resMissing := execA.execute(ctx, "propose_report",
		`{"name":"r","entity":"never_seeded_entity","reportType":"summary","groupBy":"created_at","metric":"count"}`)
	if okM, mM := parseToolResult(t, resMissing); okM {
		t.Fatal("propose_report on a missing entity must fail")
	} else if errStr, _ := mM["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
}
