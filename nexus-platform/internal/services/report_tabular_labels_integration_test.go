//go:build integration

// A tabular report's column LABELS must obey field-ACL. The run response carries the label of
// every projected column so the viewer can render a heading, and the label is admin-authored
// prose — it says what a bare machine name hides. Serving it for a field the caller's role may
// not view would also make `label != name` an ORACLE separating "this field exists but is hidden
// from you" from "this column's field was deleted", the exact distinction the FLS policy
// suppresses elsewhere (handlers/fls_hidden_metadata_integration_test.go). Both must look the
// same: the heading falls back to the field name.
//
// Rides the REAL FieldACLService over sys_field_acl on a freshly-migrated throwaway MySQL DB
// (flsWire — the same wiring main.go uses), so it proves the grant JOIN, not a fake.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run TestReportTabularLabels_FieldACL
package services

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/report"
)

func TestReportTabularLabels_FieldACL_HiddenColumnKeepsSlotWithoutLabel(t *testing.T) {
	entitySvc, recordSvc, sdb := flsWire(t)
	reportSvc := NewReportService(report.NewSQLRepository(sdb), recordSvc)
	ctx := context.Background()

	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000d1"
	restrictedRole := uuid.NewString()

	flsSeedTenant(t, sdb, tenant, "tenant-a-replabel")
	flsSeedRole(t, sdb, restrictedRole, tenant, "sales-restricted-rep")

	// quotes{title, revenue} — 'revenue' carries a label that leaks meaning its name does not.
	if _, _, err := entitySvc.CreateEntity(ctx, tenant, CreateEntityInput{
		Name:  "repquotes",
		Label: "Báo giá",
		Fields: []FieldInput{
			{Name: "title", Label: "Tiêu đề báo giá", FieldType: "text", IsRequired: true},
			{Name: "revenue", Label: "Doanh thu ròng chưa công bố", FieldType: "number"},
		},
	}); err != nil {
		t.Fatalf("CreateEntity repquotes: %v", err)
	}
	flsSeedFieldGrant(t, sdb, tenant, restrictedRole, "repquotes", "revenue", false, false)

	admin := flsAdmin(user)
	if _, err := recordSvc.Create(ctx, admin, tenant, user, "repquotes", map[string]any{
		"title": "Báo giá số một", "revenue": float64(900),
	}); err != nil {
		t.Fatalf("admin Create repquotes: %v", err)
	}

	// An admin saves a tabular report projecting BOTH the visible and the hidden field, plus a
	// column whose field does not exist at all — the two cases that must be indistinguishable.
	rec, err := reportSvc.CreateReport(ctx, tenant, CreateReportInput{
		Name:       "Báo giá theo doanh thu",
		EntityName: "repquotes",
		Type:       "tabular",
		Config:     ReportConfig{Columns: []string{"title", "revenue", "ghost"}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	reportID := rec.ID

	// (1) Restricted role: every column keeps its slot, but ONLY the visible field carries a label.
	res, err := reportSvc.RunReport(ctx, flsRoleScope(user, restrictedRole), tenant, reportID, nil)
	if err != nil {
		t.Fatalf("restricted RunReport: %v", err)
	}
	if res.Tabular == nil {
		t.Fatalf("restricted run: Tabular = nil")
	}
	if got, want := len(res.Tabular.Columns), 3; got != want {
		t.Fatalf("restricted columns = %d (%v); want %d — a hidden column must keep its slot",
			got, res.Tabular.Columns, want)
	}
	if got := res.Tabular.ColumnLabels["title"]; got != "Tiêu đề báo giá" {
		t.Errorf("visible column label = %q; want %q", got, "Tiêu đề báo giá")
	}
	if got, ok := res.Tabular.ColumnLabels["revenue"]; ok {
		t.Errorf("hidden column 'revenue' leaked its label %q — field-ACL bypassed on the label path", got)
	}
	if got, ok := res.Tabular.ColumnLabels["ghost"]; ok {
		t.Errorf("column with no field carries label %q; want none", got)
	}
	// The whole point: hidden and deleted must be INDISTINGUISHABLE downstream. The handler
	// falls back to name for both, so neither has an entry here.
	_, hiddenHasLabel := res.Tabular.ColumnLabels["revenue"]
	_, ghostHasLabel := res.Tabular.ColumnLabels["ghost"]
	if hiddenHasLabel != ghostHasLabel {
		t.Errorf("hidden-vs-deleted distinguishable from labels alone (hidden=%v ghost=%v)",
			hiddenHasLabel, ghostHasLabel)
	}
	// The VALUE strip is unchanged and still does its own job.
	if len(res.Tabular.Records) != 1 {
		t.Fatalf("restricted rows = %d; want 1", len(res.Tabular.Records))
	}
	if _, leaked := res.Tabular.Records[0].Data["revenue"]; leaked {
		t.Errorf("hidden field value leaked into row: %v", res.Tabular.Records[0].Data)
	}

	// (2) Admin bypasses field-ACL → the real label is served, proving the suppression above is
	// the ACL acting and not the label plumbing being broken for everyone.
	resAdmin, err := reportSvc.RunReport(ctx, admin, tenant, reportID, nil)
	if err != nil {
		t.Fatalf("admin RunReport: %v", err)
	}
	if got := resAdmin.Tabular.ColumnLabels["revenue"]; got != "Doanh thu ròng chưa công bố" {
		t.Errorf("admin label for 'revenue' = %q; want the real label — label plumbing is broken", got)
	}
}
