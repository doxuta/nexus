//go:build integration

// REPORT-RELATION-LABELS (queue #15/#16) end to end on a freshly-migrated throwaway MySQL with
// the REAL FieldACLService and the REAL *dynrepo.Repository label fetcher (flsWire — the wiring
// main.go uses): a tabular report whose columns include a relation field must name the customer,
// on screen (RunReport) AND in the downloaded file (ExportReportCSV), for a NON-admin. A second
// non-admin whose role is denied can_view on the target's labelField (accounts.name) sees the
// stored id in both places — {id} on screen, the bare UUID in the file — and never an error,
// never the name (EXP-01 on the report surface). Admin bypasses field-ACL and sees the label.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestReportRelationLabels
package services

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/report"
)

func TestReportRelationLabels_RunAndExport_NonAdmin(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	reportSvc := NewReportService(report.NewSQLRepository(sdb), recordSvc)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000f1"
	repRole := uuid.NewString()        // a sales rep with no field restriction
	restrictedRole := uuid.NewString() // denied can_view on the target's labelField

	flsSeedTenant(t, sdb, tenant, "tenant-a-rellabels")
	flsSeedRole(t, sdb, repRole, tenant, "sales-rep")
	flsSeedRole(t, sdb, restrictedRole, tenant, "sales-restricted-rep")

	// The relation TARGET first (its labelField is the default "name"), then the quotes entity
	// whose `account` column is what the report projects.
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "relaccounts",
		Label: "Khách hàng",
		Fields: []FieldInput{
			{Name: "name", Label: "Tên công ty", FieldType: "text", IsRequired: true},
		},
	}); err != nil {
		t.Fatalf("CreateEntity relaccounts: %v", err)
	}
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "relquotes",
		Label: "Báo giá",
		Fields: []FieldInput{
			{Name: "title", Label: "Tiêu đề báo giá", FieldType: "text", IsRequired: true},
			{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{"relatedEntity": "relaccounts"}},
		},
	}); err != nil {
		t.Fatalf("CreateEntity relquotes: %v", err)
	}
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "relaccounts", "name", false, false)

	admin := flsAdmin(user)
	accID, err := recordSvc.Create(ctx, admin, tenant, user, "relaccounts", map[string]any{"name": "Công ty ABC"})
	if err != nil {
		t.Fatalf("admin Create relaccounts: %v", err)
	}
	if _, err := recordSvc.Create(ctx, admin, tenant, user, "relquotes", map[string]any{
		"title": "Đơn hàng số 1", "account": accID,
	}); err != nil {
		t.Fatalf("admin Create relquotes: %v", err)
	}
	rec, err := reportSvc.CreateReport(ctx, tenant, CreateReportInput{
		Name:       "Đơn hàng tháng này",
		EntityName: "relquotes",
		Type:       "tabular",
		Config:     ReportConfig{Columns: []string{"title", "account"}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	runAccount := func(scope RecordScope) any {
		t.Helper()
		res, err := reportSvc.RunReport(ctx, scope, tenant, rec.ID, nil)
		if err != nil {
			t.Fatalf("RunReport: %v", err)
		}
		if res.Tabular == nil || len(res.Tabular.Records) != 1 {
			t.Fatalf("Tabular = %+v; want 1 row", res.Tabular)
		}
		return res.Tabular.Records[0].Data["account"]
	}
	exportRow := func(scope RecordScope) []string {
		t.Helper()
		var buf bytes.Buffer
		if err := reportSvc.ExportReportCSV(ctx, scope, tenant, rec.ID, nil, &buf); err != nil {
			t.Fatalf("ExportReportCSV: %v", err)
		}
		rows := parseExportCSV(t, buf.String())
		if len(rows) != 2 {
			t.Fatalf("export rows = %d; want 2 (header + 1 data)\n%s", len(rows), buf.String())
		}
		if got, want := strings.Join(rows[0], ","), "Tiêu đề báo giá,Khách hàng"; got != want {
			t.Errorf("export header = %q; want %q", got, want)
		}
		return rows[1]
	}

	// (1) Sales rep (non-admin, field-ACL clean): the customer's NAME, on screen and in the file.
	rep := flsRoleScope(user, repRole)
	if got, ok := runAccount(rep).(expandedValue); !ok || got.Label != "Công ty ABC" || got.ID != accID {
		t.Errorf("rep run account = %#v; want expandedValue{%s, Công ty ABC} — the screen still shows the UUID", runAccount(rep), accID)
	}
	row := exportRow(rep)
	if row[1] != "Công ty ABC" {
		t.Errorf("rep export account cell = %q; want %q (not the UUID, not a JSON blob)", row[1], "Công ty ABC")
	}
	if strings.Contains(row[1], accID) || strings.Contains(row[1], "{") {
		t.Errorf("rep export account cell = %q leaks the id / JSON shape", row[1])
	}

	// (2) Restricted rep: role may view relaccounts but NOT its `name` → {id} on screen, the bare
	// UUID in the file, no error — the label is never fetched (EXP-01 on the report surface).
	restricted := flsRoleScope(user, restrictedRole)
	if got, ok := runAccount(restricted).(expandedIDOnly); !ok || got.ID != accID {
		t.Errorf("restricted run account = %#v; want expandedIDOnly{%s} (no label leak)", runAccount(restricted), accID)
	}
	rrow := exportRow(restricted)
	if rrow[1] != accID {
		t.Errorf("restricted export account cell = %q; want the stored id %q (label withheld by field-ACL)", rrow[1], accID)
	}
	if strings.Contains(rrow[1], "Công ty ABC") {
		t.Errorf("restricted export leaked the denied label: %q", rrow[1])
	}

	// (3) Admin bypasses field-ACL → the real label, proving (2) is the ACL acting.
	if got, ok := runAccount(admin).(expandedValue); !ok || got.Label != "Công ty ABC" {
		t.Errorf("admin run account = %#v; want the label — the expand plumbing is broken", runAccount(admin))
	}
}
