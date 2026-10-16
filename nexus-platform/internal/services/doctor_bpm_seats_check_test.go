package services

// specs/043 S3a2 T063 — D3: the Doctor counts the pending approvals whose inbox still rests on the
// projection fallback (no open seat, or open seats none of which agrees with the instance), so
// Nexus knows when the fallback of bpm.AwaitsActor can go. Report-only: the fix is S3b's
// seat-on-touch, or the next decision on the instance — never a Doctor write.

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/doctor"
)

type fakeApprovalSeats struct {
	rows   []bpm.SeatDriftRow
	tenant string
	limit  int
}

func (f *fakeApprovalSeats) ListSeatDrift(_ context.Context, tenantID string, limit int) ([]bpm.SeatDriftRow, error) {
	f.tenant, f.limit = tenantID, limit
	return f.rows, nil
}

func TestDoctor_D3_ApprovalSeatDriftReported(t *testing.T) {
	_, _, svc := newDoctorFixture()
	seats := &fakeApprovalSeats{rows: []bpm.SeatDriftRow{
		{InstanceID: "inst-a", EntityName: "crm_quotes", RecordID: "rec-a", Drift: bpm.SeatDriftMissing},
		{InstanceID: "inst-b", EntityName: "crm_quotes", RecordID: "rec-b", Drift: bpm.SeatDriftMismatch},
	}}
	svc.WithApprovalSeats(seats)

	got := scanByID(t, svc, "t-a")[DoctorCheckApprovalSeatDrift]
	if !got.ReportOnly || got.AutoFixSafe || got.Severity != doctor.SeverityInfo {
		t.Fatalf("D3 = %+v; want report-only, never auto-fix-safe, info (the inbox still shows these rows)", got)
	}
	if got.Count != 2 || seats.tenant != "t-a" || seats.limit != doctor.MaxFindingsPerCheck+1 {
		t.Fatalf("D3 count %d (tenant %q, limit %d); want 2 findings read for t-a with limit %d",
			got.Count, seats.tenant, seats.limit, doctor.MaxFindingsPerCheck+1)
	}
	details := map[string]string{}
	for _, it := range got.Items {
		details[it.Key] = it.Detail
	}
	for key, want := range map[string][]string{
		"inst-a": {"crm_quotes", "rec-a", "chưa có ghế"},
		"inst-b": {"crm_quotes", "rec-b", "lệch"},
	} {
		d, ok := details[key]
		if !ok {
			t.Fatalf("items %+v; want key %s", got.Items, key)
		}
		for _, w := range want {
			if !strings.Contains(d, w) {
				t.Errorf("detail %q does not say %q", d, w)
			}
		}
	}

	if _, err := svc.Fix(context.Background(), "t-a", []string{DoctorCheckApprovalSeatDrift}, true); codeOf(err) != "DOCTOR_REPORT_ONLY" {
		t.Fatalf("fix D3 err = %v; want 422 DOCTOR_REPORT_ONLY", err)
	}
}

// Unwired (a server built without the BPM repository) the check is absent, not a false "0".
func TestDoctor_D3_AbsentWhenUnwired(t *testing.T) {
	_, _, svc := newDoctorFixture()
	if _, ok := scanByID(t, svc, "t-a")[DoctorCheckApprovalSeatDrift]; ok {
		t.Fatalf("D3 reported without a seat reader — a clean count it never measured")
	}
}
