//go:build integration

// A CSV a customer downloads is opened by double-clicking it in Excel, and Excel only reads a
// bare .csv as UTF-8 when the file starts with a BOM — without it every Vietnamese value arrives
// mojibake ("Nguyá»…n VÄƒn A"). The same file's header row must read like the screen it came
// from (the admin-authored labels), not like the machine field names.
//
// This file pins that contract end to end on a freshly-migrated throwaway MySQL DB with the REAL
// FieldACLService wired (flsWire — the same wiring main.go uses), because the header labels are
// under the SAME field-ACL policy the on-screen run is: a hidden column and a deleted column must
// both degrade to the bare field name, or `label != name` becomes an oracle separating them.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestExportCSVExcelReady
package services

import (
	"bytes"
	"context"
	"encoding/csv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/report"
)

// parseExportCSV asserts the stream opens with a UTF-8 BOM and returns the rows behind it.
func parseExportCSV(t *testing.T, body string) [][]string {
	t.Helper()
	if !strings.HasPrefix(body, "\xEF\xBB\xBF") {
		t.Fatalf("export does not start with a UTF-8 BOM (first bytes %q) — Excel will mojibake every Vietnamese value", body[:min(6, len(body))])
	}
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(body, "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid RFC-4180: %v\n%s", err, body)
	}
	return rows
}

// seedExcelReadyEntity builds quotes{title, revenue, sum_label} where 'revenue' carries a label
// that says what its name hides, and 'sum_label' carries a label an admin typed starting with
// '=' — a formula the header row must neutralize just like a data cell.
func seedExcelReadyEntity(t *testing.T, entitySvc *EntityService, ctx context.Context, tenant, entity string) {
	t.Helper()
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  entity,
		Label: "Báo giá",
		Fields: []FieldInput{
			{Name: "title", Label: "Tiêu đề báo giá", FieldType: "text", IsRequired: true},
			{Name: "revenue", Label: "Doanh thu ròng chưa công bố", FieldType: "number"},
			{Name: "sum_label", Label: "=Tổng cộng", FieldType: "text"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity %s: %v", entity, err)
	}
}

func TestExportCSVExcelReady_ReportHeaderIsLabelsWithBOM(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	reportSvc := NewReportService(report.NewSQLRepository(sdb), recordSvc)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000e1"
	restrictedRole := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-expexcel")
	flsSeedRole(t, sdb, restrictedRole, tenant, "sales-restricted-rep")
	seedExcelReadyEntity(t, entitySvc, ctx, tenant, "expquotes")
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "expquotes", "revenue", false, false)

	admin := flsAdmin(user)
	if _, err := recordSvc.Create(ctx, admin, tenant, user, "expquotes", map[string]any{
		"title": "Nguyễn Văn A — báo giá", "revenue": float64(900), "sum_label": "x",
	}); err != nil {
		t.Fatalf("admin Create: %v", err)
	}

	// A saved tabular report projecting the visible field, the hidden field, a column whose
	// field was deleted, and the '='-labelled field.
	rec, err := reportSvc.CreateReport(ctx, tenant, CreateReportInput{
		Name:       "Đơn hàng tháng này",
		EntityName: "expquotes",
		Type:       "tabular",
		Config:     ReportConfig{Columns: []string{"title", "revenue", "ghost", "sum_label"}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}

	// (1) Restricted (non-admin) role — the acceptance persona.
	var buf bytes.Buffer
	if err := reportSvc.ExportReportCSV(ctx, flsRoleScope(user, restrictedRole), tenant, rec.ID, nil, &buf); err != nil {
		t.Fatalf("restricted ExportReportCSV: %v", err)
	}
	rows := parseExportCSV(t, buf.String())
	if len(rows) < 2 {
		t.Fatalf("export rows = %d; want ≥2 (header + 1 data)", len(rows))
	}
	header := rows[0]
	// The report export projects exactly the on-screen columns — no leading raw UUID column.
	want := []string{"Tiêu đề báo giá", "revenue", "ghost", "'=Tổng cộng"}
	if len(header) != len(want) {
		t.Fatalf("header = %v (%d cols); want %v (%d cols) — the report export must carry the projected columns only, with no leading id",
			header, len(header), want, len(want))
	}
	for i := range want {
		if header[i] != want[i] {
			t.Errorf("header[%d] = %q; want %q", i, header[i], want[i])
		}
	}
	// Hidden and deleted must be INDISTINGUISHABLE from the header alone.
	if header[1] != "revenue" {
		t.Errorf("hidden column leaked its label in the CSV header: %q", header[1])
	}
	// The value strip is unchanged: the hidden column exports as an empty cell.
	if rows[1][1] != "" {
		t.Errorf("hidden field value leaked into the exported cell: %q", rows[1][1])
	}
	if rows[1][0] != "Nguyễn Văn A — báo giá" {
		t.Errorf("data cell = %q; want the Vietnamese value verbatim", rows[1][0])
	}

	// (2) Admin bypasses field-ACL → the real label appears, proving the suppression above is
	// the ACL acting and not the label plumbing being broken for everyone.
	var abuf bytes.Buffer
	if err := reportSvc.ExportReportCSV(ctx, admin, tenant, rec.ID, nil, &abuf); err != nil {
		t.Fatalf("admin ExportReportCSV: %v", err)
	}
	aheader := parseExportCSV(t, abuf.String())[0]
	if aheader[1] != "Doanh thu ròng chưa công bố" {
		t.Errorf("admin header[1] = %q; want the real label — the label plumbing is broken", aheader[1])
	}
}

func TestExportCSVExcelReady_EntityExportKeepsIDColumn(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000e2"

	flsSeedTenant(t, sdb, tenant, "tenant-a-expwhole")
	seedExcelReadyEntity(t, entitySvc, ctx, tenant, "expwhole")

	admin := flsAdmin(user)
	if _, err := recordSvc.Create(ctx, admin, tenant, user, "expwhole", map[string]any{
		"title": "Trần Thị B", "revenue": float64(10), "sum_label": "y",
	}); err != nil {
		t.Fatalf("admin Create: %v", err)
	}

	var buf bytes.Buffer
	if err := recordSvc.ExportCSV(ctx, admin, tenant, "expwhole", ExportCSVOptions{}, &buf); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	header := parseExportCSV(t, buf.String())[0]
	// The whole-entity export keeps "id" as the stable row key (it is not a declared field, so
	// it has no label), and labels every declared field.
	want := []string{"id", "Tiêu đề báo giá", "Doanh thu ròng chưa công bố", "'=Tổng cộng"}
	if len(header) != len(want) {
		t.Fatalf("header = %v; want %v", header, want)
	}
	for i := range want {
		if header[i] != want[i] {
			t.Errorf("header[%d] = %q; want %q", i, header[i], want[i])
		}
	}
}

// A pre-stream error must stay a clean JSON envelope: the handler resets to JSON only while
// nothing has been written, so the BOM must NOT be emitted before the RM-42 filter guard runs.
func TestExportCSVExcelReady_HiddenFieldFilterWritesNothing(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000e3"
	restrictedRole := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-expguard")
	flsSeedRole(t, sdb, restrictedRole, tenant, "sales-restricted-rep")
	seedExcelReadyEntity(t, entitySvc, ctx, tenant, "expguard")
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "expguard", "revenue", false, false)

	var buf bytes.Buffer
	err := recordSvc.ExportCSV(ctx, flsRoleScope(user, restrictedRole), tenant, "expguard", ExportCSVOptions{
		Filters: []FilterInput{{Field: "revenue", Operator: dynrepo.FilterEq, Values: []string{"900"}}},
	}, &buf)
	if codeOf(err) != "LIST_UNKNOWN_FILTER" { // a hidden name reads like a missing one (queue #317)
		t.Fatalf("filtering on a hidden field: err = %v; want LIST_UNKNOWN_FILTER", err)
	}
	if buf.Len() != 0 {
		t.Errorf("export wrote %d bytes (%q) before the ACL refusal — the handler can no longer reset to a JSON error envelope", buf.Len(), buf.String())
	}
}
