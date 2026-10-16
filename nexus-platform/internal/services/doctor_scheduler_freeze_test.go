package services

// specs/068-tenant-backup-restore T037 — the periodic Doctor sweep skips a write-frozen tenant (it
// would otherwise scan mid-restore data and notify admins about drift that is only the restore in
// flight). Other tenants are scanned as usual.

import (
	"context"
	"testing"
	"time"

	"nexus/platform/internal/doctor"
)

func TestDoctorScheduler_SweepSkipsFrozenTenant(t *testing.T) {
	scanner := &fakeSchedulerScanner{results: map[string][]doctor.CheckResult{
		"t-frozen": findingCount(4), "t-free": findingCount(1)}}
	store := &fakeScheduleStore{
		tenants: []string{"t-frozen", "t-free"},
		admins:  map[string][]string{"t-frozen": {"a1"}, "t-free": {"a2"}},
	}
	sink := &fakeNotifSink{}
	s := NewDoctorScheduler(DoctorSchedulerConfig{Interval: time.Hour}, scanner, store, sink, nil).
		WithTenantGate(func(_ context.Context, tenantID string) bool { return tenantID == "t-frozen" })

	s.Sweep(context.Background())

	if len(scanner.scans) != 1 || scanner.scans[0] != "t-free" {
		t.Fatalf("scans = %v, want only t-free", scanner.scans)
	}
	if len(sink.inserted) != 1 || sink.inserted[0].TenantID != "t-free" {
		t.Fatalf("notifications = %+v, want one for t-free", sink.inserted)
	}
}
