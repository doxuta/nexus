package vertical_pack

// Unit tests for the reports install pass (PACK-REPORTS) over in-memory fakes: a pack with N
// reports → CreateReport called N times (mapped from PackReport to CreateReportInput); a report
// whose name already exists in the tenant → skipped by name (idempotent); a re-install → every
// report skipped; a nil report writer → the pack's reports are silently skipped (optional dep
// absent, back-compat). Config-time validation itself (ValidateReportConfig) is covered by the
// integration test against a real ReportService — the fake here only records calls.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
)

// fakeReportWriter is an in-memory reportWriter: CreateReport records the mapped input and makes the
// created report visible to a later ListReports (so a re-install skips it by name). It does NOT
// validate — parity with the real service's validation is proven in the integration test.
type fakeReportWriter struct {
	mu       sync.Mutex
	seq      int
	existing []report.Record              // pre-seeded reports (for the idempotent-skip test)
	created  []services.CreateReportInput // every CreateReport input, in call order
}

func (f *fakeReportWriter) ListReports(_ context.Context, _ string) ([]report.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]report.Record(nil), f.existing...), nil
}

func (f *fakeReportWriter) CreateReport(_ context.Context, tenantID string, in services.CreateReportInput) (*report.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.created = append(f.created, in)
	rec := report.Record{ID: fmt.Sprintf("rep-%d", f.seq), TenantID: tenantID, Name: in.Name, EntityName: in.EntityName, Type: in.Type, Config: json.RawMessage("{}")}
	// Make the new report visible to a later ListReports so a re-install skips it by name.
	f.existing = append(f.existing, rec)
	return &rec, nil
}

// newReportTestService wires a Service over the shared entity/role fakes plus the report writer
// (nil = optional dep absent). Returns the service and the entity fake (for rollback assertions).
func newReportTestService(rep *fakeReportWriter) (*Service, *fakeMintMeta) {
	meta := &fakeMintMeta{}
	d := Deps{
		EntityReader: meta,
		RoleReader:   fakeMintRoles{},
		EntityWriter: meta,
	}
	if rep != nil {
		d.ReportWriter = rep // avoid a typed-nil interface tripping the s.reportWr != nil guard
	}
	return NewService(d), meta
}

// reportTestPack: one entity + two reports — a summary (count group-by status) and a tabular
// (columns projection). The fake writer does not validate the shapes; the shapes here only prove
// the pack→input mapping carries type/config through.
func reportTestPack() *Pack {
	active := true
	return &Pack{
		Entities: []PackEntity{
			{Name: "rep_tickets", Label: "Tickets", Fields: []PackField{
				{Name: "subject", Label: "Subject", FieldType: "text"},
				{Name: "status", Label: "Status", FieldType: "text"},
			}},
		},
		Reports: []PackReport{
			{
				Name:       "By status",
				EntityName: "rep_tickets",
				Type:       "summary",
				Config:     PackReportConfig{Metric: "count", GroupBy: "status"},
				IsActive:   &active,
			},
			{
				Name:       "All tickets",
				EntityName: "rep_tickets",
				Type:       "tabular",
				Config:     PackReportConfig{Columns: []string{"subject", "status"}},
			},
		},
	}
}

// TestInstall_ReportsCreated: a pack with 2 reports → CreateReport called twice with the mapped
// inputs (ActorID = install user, type/config carried through), and a re-install skips both by name.
func TestInstall_ReportsCreated(t *testing.T) {
	rep := &fakeReportWriter{}
	svc, _ := newReportTestService(rep)
	ctx := context.Background()

	res, err := svc.Install(ctx, "tenant-1", "user-1", reportTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.ReportsCreated) != 2 {
		t.Fatalf("ReportsCreated = %v, want 2", res.ReportsCreated)
	}
	if len(res.ReportsSkipped) != 0 {
		t.Errorf("ReportsSkipped = %v, want empty on first install", res.ReportsSkipped)
	}
	if len(rep.created) != 2 {
		t.Fatalf("CreateReport called %d times, want 2", len(rep.created))
	}
	// Mapping: names preserved in pack order, ActorID is the install user.
	if rep.created[0].Name != "By status" || rep.created[1].Name != "All tickets" {
		t.Errorf("created names = [%q %q], want [By status, All tickets]", rep.created[0].Name, rep.created[1].Name)
	}
	for i, in := range rep.created {
		if in.ActorID != "user-1" {
			t.Errorf("created[%d].ActorID = %q, want user-1", i, in.ActorID)
		}
	}
	// The summary config mapped through (metric + groupBy).
	if rep.created[0].Type != "summary" || rep.created[0].Config.Metric != "count" || rep.created[0].Config.GroupBy != "status" {
		t.Errorf("created[0] = {type:%q metric:%q groupBy:%q}, want {summary count status}",
			rep.created[0].Type, rep.created[0].Config.Metric, rep.created[0].Config.GroupBy)
	}
	// The tabular columns mapped through.
	if rep.created[1].Type != "tabular" || len(rep.created[1].Config.Columns) != 2 || rep.created[1].Config.Columns[0] != "subject" {
		t.Errorf("created[1] = {type:%q columns:%v}, want {tabular [subject status]}",
			rep.created[1].Type, rep.created[1].Config.Columns)
	}

	// --- Re-install: both reports now exist by name → all skipped, no new CreateReport. ---
	res2, err := svc.Install(ctx, "tenant-1", "user-1", reportTestPack())
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if len(res2.ReportsCreated) != 0 {
		t.Errorf("re-install ReportsCreated = %v, want empty (idempotent)", res2.ReportsCreated)
	}
	if len(res2.ReportsSkipped) != 2 {
		t.Errorf("re-install ReportsSkipped = %v, want 2", res2.ReportsSkipped)
	}
	if len(rep.created) != 2 {
		t.Errorf("CreateReport total calls = %d, want 2 (no new creates on re-install)", len(rep.created))
	}
}

// TestInstall_ReportsSkippedByName: a report whose name already exists in the tenant is skipped by
// name (buildPlan's reportSkip), the other is created.
func TestInstall_ReportsSkippedByName(t *testing.T) {
	rep := &fakeReportWriter{
		existing: []report.Record{{ID: "pre-1", TenantID: "tenant-1", Name: "All tickets"}},
	}
	svc, _ := newReportTestService(rep)

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", reportTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.ReportsCreated) != 1 || res.ReportsCreated[0] != "By status" {
		t.Errorf("ReportsCreated = %v, want [By status]", res.ReportsCreated)
	}
	if len(res.ReportsSkipped) != 1 || res.ReportsSkipped[0] != "All tickets" {
		t.Errorf("ReportsSkipped = %v, want [All tickets]", res.ReportsSkipped)
	}
	// Only the non-existing report went through CreateReport.
	if len(rep.created) != 1 || rep.created[0].Name != "By status" {
		t.Errorf("CreateReport calls = %+v, want one (By status)", rep.created)
	}
}

// TestInstall_NilReportWriter_NoReports: with the optional dep absent, a pack's reports are silently
// skipped (no create, no skip counters) — back-compat with pre-PACK-REPORTS behavior.
func TestInstall_NilReportWriter_NoReports(t *testing.T) {
	svc, _ := newReportTestService(nil) // optional dep absent

	res, err := svc.Install(context.Background(), "tenant-1", "user-1", reportTestPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(res.ReportsCreated) != 0 || len(res.ReportsSkipped) != 0 {
		t.Errorf("nil-writer result = created:%v skipped:%v, want both empty", res.ReportsCreated, res.ReportsSkipped)
	}
	// The entity still installed (reports are a late pass, absent here).
	if len(res.EntitiesCreated) != 1 {
		t.Errorf("EntitiesCreated = %v, want 1 (install proceeds without report writer)", res.EntitiesCreated)
	}
}
