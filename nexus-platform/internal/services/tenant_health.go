package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/project"
	"nexus/platform/internal/workflow"
)

// Health tuning (specs/066 L6).
const (
	healthStuckAfter    = 15 * time.Minute
	healthTopRules      = 5
	healthLastErrorRune = 200
	healthWindow24h     = 24 * time.Hour
	healthWindow7d      = 7 * 24 * time.Hour
)

// Consumer-side views of the repositories that own the tables (each query pins the tenant).
type healthJobsStore interface {
	FailedByTypeSince(ctx context.Context, tenantID string, since time.Time) ([]jobs.JobTypeFailure, error)
	StuckQueuedForTenant(ctx context.Context, tenantID string, dueBefore time.Time) (int, error)
}
type healthRunsStore interface {
	FailedRunsSince(ctx context.Context, tenantID string, since time.Time, limit int) (int, []workflow.RuleFailure, error)
	LastFailedRun(ctx context.Context, tenantID, workflowID string, since time.Time) (string, error)
}
type healthMailStore interface {
	FailedSummarySince(ctx context.Context, tenantID string, since7d, since24h time.Time) (emaillog.FailedSummary, error)
}
type healthHooksStore interface {
	FailedByHookSince(ctx context.Context, tenantID string, since time.Time) ([]inboundhook.HookFailure, error)
}

// DoctorSnapshot is the latest Doctor sweep of a tenant, as counts of findings.
type DoctorSnapshot struct {
	Critical  int
	Warn      int
	CheckedAt *time.Time
}

// DoctorSnapshotReader reads the latest Doctor sweep. A nil snapshot (or one never checked) is
// "unknown". noopDoctorSnapshot stands in until specs/062-customer-workspace L5 lands
// sys_project_profiles.health_* (FR-063).
type DoctorSnapshotReader interface {
	Latest(ctx context.Context, tenantID string) (*DoctorSnapshot, error)
}

type noopDoctorSnapshot struct{}

func (noopDoctorSnapshot) Latest(context.Context, string) (*DoctorSnapshot, error) { return nil, nil }

// JobFailureItem is one failed-job type of the last 7 days.
type JobFailureItem struct {
	JobType string    `json:"jobType"`
	Count   int       `json:"count"`
	LastAt  time.Time `json:"lastAt"`
}

// RuleFailureItem is one failing rule: names and a masked, clipped last error only.
type RuleFailureItem struct {
	WorkflowID string    `json:"workflowId"`
	Name       string    `json:"name"`
	Failed     int       `json:"failed"`
	LastAt     time.Time `json:"lastAt"`
	LastError  string    `json:"lastError"`
}

// HookFailureItem is one failing inbound hook. No payload, no raw error (FR-062).
type HookFailureItem struct {
	HookID string    `json:"hookId"`
	Name   string    `json:"name"`
	Failed int       `json:"failed"`
	LastAt time.Time `json:"lastAt"`
}

// HealthSummary is GET /admin/health's data.
type HealthSummary struct {
	Status       string    `json:"status"`
	GeneratedAt  time.Time `json:"generatedAt"`
	QueueEnabled bool      `json:"queueEnabled"`
	Jobs         struct {
		Failed7d []JobFailureItem `json:"failed7d"`
		Stuck    int              `json:"stuck"`
	} `json:"jobs"`
	WorkflowRuns struct {
		Failed24h  int               `json:"failed24h"`
		Failed7d   int               `json:"failed7d"`
		TopFailing []RuleFailureItem `json:"topFailing"`
	} `json:"workflowRuns"`
	Emails struct {
		Failed24h      int `json:"failed24h"`
		Failed7d       int `json:"failed7d"`
		UnresentFailed int `json:"unresentFailed"`
	} `json:"emails"`
	InboundHooks struct {
		Failed24h int               `json:"failed24h"`
		ByHook    []HookFailureItem `json:"byHook"`
	} `json:"inboundHooks"`
	Doctor HealthDoctor `json:"doctor"`
}

// HealthDoctor is the Doctor part of the summary; Status is ok|warn|critical|unknown.
type HealthDoctor struct {
	Status    string     `json:"status"`
	Critical  int        `json:"critical"`
	Warn      int        `json:"warn"`
	CheckedAt *time.Time `json:"checkedAt"`
}

// HealthService assembles the operating-health summary of one tenant.
type HealthService struct {
	jobs         healthJobsStore
	runs         healthRunsStore
	mail         healthMailStore
	hooks        healthHooksStore
	doctor       DoctorSnapshotReader
	queueEnabled bool
	now          func() time.Time
}

// NewHealthService wires the service; a nil doctor reader means noop (Doctor "unknown").
func NewHealthService(j healthJobsStore, r healthRunsStore, m healthMailStore, h healthHooksStore, d DoctorSnapshotReader, queueEnabled bool) *HealthService {
	if d == nil {
		d = noopDoctorSnapshot{}
	}
	return &HealthService{jobs: j, runs: r, mail: m, hooks: h, doctor: d, queueEnabled: queueEnabled, now: time.Now}
}

// Summary gathers the signals for tenantID (explicit, so the project ledger can call it per customer).
func (s *HealthService) Summary(ctx context.Context, tenantID string) (HealthSummary, error) {
	var out HealthSummary
	now := s.now().UTC()
	since24, since7 := now.Add(-healthWindow24h), now.Add(-healthWindow7d)
	out.GeneratedAt, out.QueueEnabled = now, s.queueEnabled
	wrap := func(op string, err error) error { return fmt.Errorf("services.HealthService.Summary: %s: %w", op, err) }

	failed, err := s.jobs.FailedByTypeSince(ctx, tenantID, since7)
	if err != nil {
		return out, wrap("jobs", err)
	}
	out.Jobs.Failed7d = make([]JobFailureItem, 0, len(failed))
	for _, f := range failed {
		out.Jobs.Failed7d = append(out.Jobs.Failed7d, JobFailureItem{JobType: f.JobType, Count: f.Count, LastAt: f.LastAt})
	}
	if s.queueEnabled {
		if out.Jobs.Stuck, err = s.jobs.StuckQueuedForTenant(ctx, tenantID, now.Add(-healthStuckAfter)); err != nil {
			return out, wrap("stuck", err)
		}
	}

	if out.WorkflowRuns.Failed24h, _, err = s.runs.FailedRunsSince(ctx, tenantID, since24, 0); err != nil {
		return out, wrap("runs24h", err)
	}
	total7, top, err := s.runs.FailedRunsSince(ctx, tenantID, since7, healthTopRules)
	if err != nil {
		return out, wrap("runs7d", err)
	}
	out.WorkflowRuns.Failed7d = total7
	out.WorkflowRuns.TopFailing = make([]RuleFailureItem, 0, len(top))
	for _, r := range top {
		msg, err := s.runs.LastFailedRun(ctx, tenantID, r.WorkflowID, since7)
		if err != nil {
			return out, wrap("lastRun", err)
		}
		out.WorkflowRuns.TopFailing = append(out.WorkflowRuns.TopFailing,
			RuleFailureItem{WorkflowID: r.WorkflowID, Name: r.Name, Failed: r.Count, LastAt: r.LastAt, LastError: clipRunes(msg, healthLastErrorRune)})
	}

	mail, err := s.mail.FailedSummarySince(ctx, tenantID, since7, since24)
	if err != nil {
		return out, wrap("emails", err)
	}
	out.Emails.Failed24h, out.Emails.Failed7d, out.Emails.UnresentFailed = mail.Failed24h, mail.Failed7d, mail.UnresentFailed

	hooks, err := s.hooks.FailedByHookSince(ctx, tenantID, since24)
	if err != nil {
		return out, wrap("hooks", err)
	}
	out.InboundHooks.ByHook = make([]HookFailureItem, 0, len(hooks))
	for _, h := range hooks {
		out.InboundHooks.Failed24h += h.Count
		out.InboundHooks.ByHook = append(out.InboundHooks.ByHook, HookFailureItem{HookID: h.HookID, Name: h.Name, Failed: h.Count, LastAt: h.LastAt})
	}

	snap, err := s.doctor.Latest(ctx, tenantID)
	if err != nil {
		return out, wrap("doctor", err)
	}
	out.Doctor = doctorOf(snap)
	out.Status = DeriveHealth(out, now)
	return out, nil
}

func doctorOf(snap *DoctorSnapshot) HealthDoctor {
	if snap == nil || snap.CheckedAt == nil {
		return HealthDoctor{Status: "unknown"}
	}
	d := HealthDoctor{Status: "ok", Critical: snap.Critical, Warn: snap.Warn, CheckedAt: snap.CheckedAt}
	switch {
	case snap.Critical > 0:
		d.Status = "critical"
	case snap.Warn > 0:
		d.Status = "warn"
	}
	return d
}

// DeriveHealth is the pure overall label (FR-061): critical on stuck jobs or a critical Doctor;
// warn on any failure within 24h or a warn Doctor; else ok. Doctor "unknown" and the
// unresent-mail count (a display number) never move it.
func DeriveHealth(s HealthSummary, now time.Time) string {
	if s.Jobs.Stuck > 0 || s.Doctor.Status == "critical" {
		return "critical"
	}
	warn := s.WorkflowRuns.Failed24h > 0 || s.InboundHooks.Failed24h > 0 || s.Emails.Failed24h > 0 || s.Doctor.Status == "warn"
	for _, f := range s.Jobs.Failed7d {
		if f.LastAt.After(now.Add(-healthWindow24h)) {
			warn = true
		}
	}
	if warn {
		return "warn"
	}
	return "ok"
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// projectProfileGetter is the one read the Doctor adapter needs (implemented by *project.Repository).
type projectProfileGetter interface {
	Get(ctx context.Context, tenantID string) (*project.Profile, error)
}

// ProjectDoctorSnapshot reads the latest Doctor sweep from the project registry (specs/066 T138a).
// It derives the level from the COUNTS $.critical/$.warn only: never health_status (it also folds in
// app readiness, and the ops label would loop back into itself) and never $.ops.
type ProjectDoctorSnapshot struct{ repo projectProfileGetter }

// NewProjectDoctorSnapshot wires the adapter over the project registry.
func NewProjectDoctorSnapshot(r projectProfileGetter) *ProjectDoctorSnapshot {
	return &ProjectDoctorSnapshot{repo: r}
}

// Latest returns nil (unknown) when the tenant has no profile or was never checked.
func (a *ProjectDoctorSnapshot) Latest(ctx context.Context, tenantID string) (*DoctorSnapshot, error) {
	p, err := a.repo.Get(ctx, tenantID)
	if errors.Is(err, project.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("services.ProjectDoctorSnapshot.Latest: %w", err)
	}
	if p.HealthCheckedAt == nil || len(p.HealthJSON) == 0 {
		return nil, nil
	}
	var doc struct {
		Critical int `json:"critical"`
		Warn     int `json:"warn"`
	}
	if err := json.Unmarshal(p.HealthJSON, &doc); err != nil {
		return nil, fmt.Errorf("services.ProjectDoctorSnapshot.Latest: decode: %w", err)
	}
	return &DoctorSnapshot{Critical: doc.Critical, Warn: doc.Warn, CheckedAt: p.HealthCheckedAt}, nil
}
