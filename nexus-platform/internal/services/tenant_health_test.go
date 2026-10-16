package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/workflow"
)

func TestDeriveHealth(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Hour), now.Add(-3*24*time.Hour)
	cases := []struct {
		name string
		mut  func(*HealthSummary)
		want string
	}{
		{"nothing", func(*HealthSummary) {}, "ok"},
		{"stuck", func(s *HealthSummary) { s.Jobs.Stuck = 1 }, "critical"},
		{"doctor critical", func(s *HealthSummary) { s.Doctor.Status = "critical" }, "critical"},
		{"doctor warn", func(s *HealthSummary) { s.Doctor.Status = "warn" }, "warn"},
		{"doctor unknown", func(s *HealthSummary) { s.Doctor.Status = "unknown" }, "ok"},
		{"job failure 24h", func(s *HealthSummary) { s.Jobs.Failed7d = []JobFailureItem{{JobType: "x", Count: 1, LastAt: recent}} }, "warn"},
		{"job failure only 7d", func(s *HealthSummary) { s.Jobs.Failed7d = []JobFailureItem{{JobType: "x", Count: 1, LastAt: old}} }, "ok"},
		{"rule failure 24h", func(s *HealthSummary) { s.WorkflowRuns.Failed24h = 2 }, "warn"},
		{"rule failure 7d only", func(s *HealthSummary) { s.WorkflowRuns.Failed7d = 2 }, "ok"},
		{"email failed 24h", func(s *HealthSummary) { s.Emails.Failed24h = 1 }, "warn"},
		{"unresent without 24h", func(s *HealthSummary) { s.Emails.UnresentFailed = 3; s.Emails.Failed7d = 3 }, "ok"},
		{"inbound hook 24h", func(s *HealthSummary) { s.InboundHooks.Failed24h = 1 }, "warn"},
		{"stuck beats warn", func(s *HealthSummary) { s.Jobs.Stuck = 1; s.Emails.Failed24h = 1 }, "critical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := HealthSummary{Doctor: HealthDoctor{Status: "unknown"}}
			tc.mut(&s)
			if got := DeriveHealth(s, now); got != tc.want {
				t.Fatalf("DeriveHealth = %q, want %q", got, tc.want)
			}
		})
	}
}

type fakeHealthJobs struct {
	tenant string
	stuck  int
	err    error
}

func (f *fakeHealthJobs) FailedByTypeSince(_ context.Context, tenant string, _ time.Time) ([]jobs.JobTypeFailure, error) {
	f.tenant = tenant
	return []jobs.JobTypeFailure{{JobType: "outbound_webhook", Count: 1, LastAt: time.Now()}}, f.err
}
func (f *fakeHealthJobs) StuckQueuedForTenant(context.Context, string, time.Time) (int, error) {
	return f.stuck, nil
}

type fakeHealthRuns struct{}

func (fakeHealthRuns) FailedRunsSince(_ context.Context, _ string, _ time.Time, limit int) (int, []workflow.RuleFailure, error) {
	if limit == 0 {
		return 2, nil, nil
	}
	return 5, []workflow.RuleFailure{{WorkflowID: "w1", Name: "Rule", Count: 2, LastAt: time.Now()}}, nil
}
func (fakeHealthRuns) LastFailedRun(context.Context, string, string, time.Time) (string, error) {
	return strings.Repeat("x", 300), nil
}

type fakeHealthMail struct{}

func (fakeHealthMail) FailedSummarySince(context.Context, string, time.Time, time.Time) (emaillog.FailedSummary, error) {
	return emaillog.FailedSummary{Failed7d: 3, Failed24h: 1, UnresentFailed: 2}, nil
}

type fakeHealthHooks struct{}

func (fakeHealthHooks) FailedByHookSince(context.Context, string, time.Time) ([]inboundhook.HookFailure, error) {
	return []inboundhook.HookFailure{{HookID: "h1", Name: "Hook", Count: 4, LastAt: time.Now()}}, nil
}

func TestHealthService_Summary(t *testing.T) {
	j := &fakeHealthJobs{stuck: 7}
	svc := NewHealthService(j, fakeHealthRuns{}, fakeHealthMail{}, fakeHealthHooks{}, nil, true)
	s, err := svc.Summary(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if j.tenant != "t1" || s.Jobs.Stuck != 7 || len(s.Jobs.Failed7d) != 1 {
		t.Fatalf("jobs = %+v tenant %q", s.Jobs, j.tenant)
	}
	if s.WorkflowRuns.Failed24h != 2 || s.WorkflowRuns.Failed7d != 5 || len(s.WorkflowRuns.TopFailing) != 1 {
		t.Fatalf("runs = %+v", s.WorkflowRuns)
	}
	if n := len([]rune(s.WorkflowRuns.TopFailing[0].LastError)); n != 200 {
		t.Fatalf("lastError runes = %d, want 200", n)
	}
	if s.Emails.Failed24h != 1 || s.Emails.Failed7d != 3 || s.Emails.UnresentFailed != 2 {
		t.Fatalf("emails = %+v", s.Emails)
	}
	if s.InboundHooks.Failed24h != 4 || len(s.InboundHooks.ByHook) != 1 {
		t.Fatalf("hooks = %+v", s.InboundHooks)
	}
	if s.Doctor.Status != "unknown" || s.Doctor.CheckedAt != nil {
		t.Fatalf("doctor = %+v", s.Doctor)
	}
	if s.Status != "critical" || !s.QueueEnabled {
		t.Fatalf("status %q queue %v", s.Status, s.QueueEnabled)
	}
}

func TestHealthService_QueueOffIgnoresStuck(t *testing.T) {
	svc := NewHealthService(&fakeHealthJobs{stuck: 7}, fakeHealthRuns{}, fakeHealthMail{}, fakeHealthHooks{}, nil, false)
	s, err := svc.Summary(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Jobs.Stuck != 0 {
		t.Fatalf("stuck = %d with queue off, want 0", s.Jobs.Stuck)
	}
}

func TestHealthService_StoreError(t *testing.T) {
	svc := NewHealthService(&fakeHealthJobs{err: errors.New("db down")}, fakeHealthRuns{}, fakeHealthMail{}, fakeHealthHooks{}, nil, true)
	if _, err := svc.Summary(context.Background(), "t1"); err == nil {
		t.Fatal("want error")
	}
}
