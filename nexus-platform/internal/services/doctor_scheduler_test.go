package services

// Unit tests for the DOCTOR-OPS scheduled scan (doctor_scheduler.go): the sweep notifies a
// tenant's admins exactly when findings exist, per-tenant errors don't stop the sweep, and
// the no-auto-fix rule holds by construction (the scanner dependency has no Fix method — see
// doctorScanner; these tests additionally pin that only Scan is ever invoked).

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/config"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/workflow"
)

// fakeSchedulerScanner returns canned per-tenant scan results and counts Scan calls.
type fakeSchedulerScanner struct {
	results map[string][]doctor.CheckResult
	errs    map[string]error
	scans   []string // tenant ids in call order
}

func (f *fakeSchedulerScanner) Scan(_ context.Context, tenantID string) ([]doctor.CheckResult, error) {
	f.scans = append(f.scans, tenantID)
	if err := f.errs[tenantID]; err != nil {
		return nil, err
	}
	return f.results[tenantID], nil
}

type fakeScheduleStore struct {
	tenants    []string
	tenantsErr error
	admins     map[string][]string
	adminsErr  map[string]error
}

func (f *fakeScheduleStore) ListActiveTenants(context.Context, int) ([]string, error) {
	return f.tenants, f.tenantsErr
}
func (f *fakeScheduleStore) ListTenantAdmins(_ context.Context, tenantID string, _ int) ([]string, error) {
	if f.adminsErr != nil && f.adminsErr[tenantID] != nil {
		return nil, f.adminsErr[tenantID]
	}
	return f.admins[tenantID], nil
}

type fakeNotifSink struct {
	inserted []workflow.Notification
	err      error
}

func (f *fakeNotifSink) InsertNotification(_ context.Context, n workflow.Notification) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.inserted = append(f.inserted, n)
	return "notif-1", nil
}

func findingCount(n int) []doctor.CheckResult {
	return []doctor.CheckResult{{CheckID: "B1_PHANTOM_RELATIONSHIPS", Count: n}}
}

func TestDoctorScheduler_SweepNotifiesAdminsOnFindingsOnly(t *testing.T) {
	scanner := &fakeSchedulerScanner{
		results: map[string][]doctor.CheckResult{
			"t-clean": findingCount(0),
			"t-drift": {
				{CheckID: "B1_PHANTOM_RELATIONSHIPS", Count: 2},
				{CheckID: "C4_ZOMBIE_WORKFLOWS", Count: 1},
			},
		},
	}
	store := &fakeScheduleStore{
		tenants: []string{"t-clean", "t-drift"},
		admins:  map[string][]string{"t-drift": {"admin-1", "admin-2"}, "t-clean": {"admin-x"}},
	}
	sink := &fakeNotifSink{}
	s := NewDoctorScheduler(DoctorSchedulerConfig{Interval: time.Hour}, scanner, store, sink, nil)

	s.Sweep(context.Background())

	// Both tenants scanned (read-only), in order.
	if len(scanner.scans) != 2 || scanner.scans[0] != "t-clean" || scanner.scans[1] != "t-drift" {
		t.Fatalf("scans = %v; want cả 2 tenant", scanner.scans)
	}
	// ONLY the drifting tenant's admins get notified — one row per admin, total in the title.
	if len(sink.inserted) != 2 {
		t.Fatalf("notifications = %d (%+v); want 2 (mỗi admin của t-drift một row)", len(sink.inserted), sink.inserted)
	}
	for i, want := range []string{"admin-1", "admin-2"} {
		n := sink.inserted[i]
		if n.TenantID != "t-drift" || n.UserID != want {
			t.Fatalf("notification[%d] = %+v; want tenant t-drift user %s", i, n, want)
		}
		if n.Title != "Bác sĩ hệ thống: 3 khoản lệch" {
			t.Fatalf("title = %q; want tổng findings 3 (2+1)", n.Title)
		}
		if !strings.Contains(n.Message, "Khám hệ thống") || !strings.Contains(n.Message, "KHÔNG tự sửa") {
			t.Fatalf("message phải trỏ Bảo trì → Khám hệ thống + nói rõ không auto-fix: %q", n.Message)
		}
	}
}

func TestDoctorScheduler_TenantScanErrorContinuesSweep(t *testing.T) {
	scanner := &fakeSchedulerScanner{
		results: map[string][]doctor.CheckResult{"t-b": findingCount(1)},
		errs:    map[string]error{"t-a": errors.New("db down")},
	}
	store := &fakeScheduleStore{
		tenants: []string{"t-a", "t-b"},
		admins:  map[string][]string{"t-b": {"admin-b"}},
	}
	sink := &fakeNotifSink{}
	s := NewDoctorScheduler(DoctorSchedulerConfig{Interval: time.Hour}, scanner, store, sink, nil)

	s.Sweep(context.Background())

	if len(scanner.scans) != 2 {
		t.Fatalf("scans = %v; tenant lỗi phải CONTINUE, không dừng sweep", scanner.scans)
	}
	if len(sink.inserted) != 1 || sink.inserted[0].TenantID != "t-b" {
		t.Fatalf("notifications = %+v; want 1 cho t-b", sink.inserted)
	}
}

func TestDoctorScheduler_StartStopLifecycle(t *testing.T) {
	// A started scheduler stops cleanly within the deadline (janitor lifecycle contract).
	scanner := &fakeSchedulerScanner{}
	s := NewDoctorScheduler(DoctorSchedulerConfig{Interval: time.Hour}, scanner, &fakeScheduleStore{}, &fakeNotifSink{}, nil)
	s.Start()
	s.Start() // idempotent
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// First tick fires only after a full interval — nothing scanned in this short window.
	if len(scanner.scans) != 0 {
		t.Fatalf("scans = %v; tick đầu phải chờ trọn một interval", scanner.scans)
	}
}

func TestLoadDoctorConfig_DegradeToDefaultOff(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		want    time.Duration
		enabled bool
	}{
		{"unset → OFF (opt-in)", "", 0, false},
		{"0 → OFF", "0", 0, false},
		{"âm → degrade về OFF, không fail boot", "-5", 0, false},
		{"rác → degrade về OFF", "banana", 0, false},
		{"hợp lệ → bật đúng cadence", "86400", 86400 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.env == "" {
				t.Setenv("DOCTOR_SCAN_INTERVAL_SEC", "")
			} else {
				t.Setenv("DOCTOR_SCAN_INTERVAL_SEC", tt.env)
			}
			cfg := config.LoadDoctorConfig()
			if cfg.ScanInterval != tt.want || cfg.Enabled() != tt.enabled {
				t.Fatalf("cfg = %+v (enabled %v); want interval %v enabled %v", cfg, cfg.Enabled(), tt.want, tt.enabled)
			}
		})
	}
}
