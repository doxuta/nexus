package services

// specs/043 S9a T262 — D4: the Doctor lists the records that hold two or more PENDING approvals so
// Nexus can withdraw one of each pair BEFORE Migration B (uq_ai_open) is applied. Report-only.

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/doctor"
)

type fakeApprovalDuplicates struct {
	rows   []bpm.DuplicatePendingRow
	tenant string
	limit  int
}

func (f *fakeApprovalDuplicates) ListDuplicatePending(_ context.Context, tenantID string, limit int) ([]bpm.DuplicatePendingRow, error) {
	f.tenant, f.limit = tenantID, limit
	return f.rows, nil
}

func TestDoctor_DuplicatePendingPairsReported(t *testing.T) {
	_, _, svc := newDoctorFixture()
	dups := &fakeApprovalDuplicates{rows: []bpm.DuplicatePendingRow{
		{EntityName: "crm_quotes", RecordID: "rec-a", InstanceIDs: []string{"inst-1", "inst-2"}},
	}}
	svc.WithApprovalDuplicates(dups)

	got := scanByID(t, svc, "t-a")[DoctorCheckApprovalDuplicatePending]
	if !got.ReportOnly || got.AutoFixSafe || got.Severity != doctor.SeverityWarn {
		t.Fatalf("D4 = %+v; want report-only, never auto-fix-safe, warn", got)
	}
	if got.Count != 1 || dups.tenant != "t-a" || dups.limit != doctor.MaxFindingsPerCheck+1 {
		t.Fatalf("D4 count %d (tenant %q, limit %d); want 1 finding read for t-a with limit %d",
			got.Count, dups.tenant, dups.limit, doctor.MaxFindingsPerCheck+1)
	}
	if it := got.Items[0]; it.Key != "crm_quotes/rec-a" ||
		!strings.Contains(it.Detail, "inst-1") || !strings.Contains(it.Detail, "inst-2") || !strings.Contains(it.Detail, "Migration B") {
		t.Fatalf("item %+v; want the record key and both instance ids in the detail", it)
	}
	if _, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckApprovalDuplicatePending}, true); codeOf(err) != "DOCTOR_REPORT_ONLY" {
		t.Fatalf("fix D4 err = %v; want 422 DOCTOR_REPORT_ONLY (the system never picks which approval of a pair to cancel)", err)
	}
}

func TestDoctor_DuplicatePendingAbsentWhenUnwired(t *testing.T) {
	_, _, svc := newDoctorFixture()
	if _, ok := scanByID(t, svc, "t-a")[DoctorCheckApprovalDuplicatePending]; ok {
		t.Fatalf("D4 reported without a reader — a clean count it never measured")
	}
}
