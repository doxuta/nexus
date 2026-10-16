package config

import (
	"fmt"
	"time"
)

// Retention/janitor defaults applied when the corresponding env var is unset. The janitor is a
// single background goroutine (mirrors the workflow scheduler) that periodically deletes aged rows
// from cross-tenant SYSTEM tables (the full list lives in janitor.Sweep). This file owns the
// knobs for sys_workflow_runs + sys_hook_deliveries (RunsRetention) and the three short-lived
// token tables mfa_pending_tokens / password_reset_tokens / sso_handoff_tokens (TokenGrace);
// sys_jobs retention is JOBS_RETENTION_DAYS (JobsConfig) and sys_idempotency_keys expire on their
// own expires_at. It is ENABLED by default (an hourly sweep) because these tables grow unbounded
// otherwise; set JANITOR_INTERVAL_SEC=0 to disable it.
const (
	defaultJanitorIntervalSec       = 3600 // 1h sweep cadence; 0 = disabled
	defaultWorkflowRunsRetentionDay = 90   // delete sys_workflow_runs finished more than N days ago
	// janitorTokenGraceDays is the grace period past a token's expires_at before the janitor
	// deletes it. An expired token is already unusable (queries filter expires_at); the grace
	// only keeps a short forensic window. Not env-configurable (a fixed 1-day buffer).
	janitorTokenGraceDays = 1
)

// JanitorConfig holds the verified retention-janitor configuration read from the environment. The
// janitor ticks every Interval (0 = disabled); a sweep deletes sys_workflow_runs whose finished_at
// (and sys_hook_deliveries whose created_at) is older than RunsRetention, and mfa_pending_tokens /
// password_reset_tokens / sso_handoff_tokens whose expires_at is older than now - TokenGrace.
type JanitorConfig struct {
	Interval      time.Duration // 0 = janitor disabled
	RunsRetention time.Duration // age threshold for sys_workflow_runs and sys_hook_deliveries
	TokenGrace    time.Duration // grace past expires_at for the token tables
}

// Enabled reports whether the background janitor should run (a positive tick interval).
func (c JanitorConfig) Enabled() bool { return c.Interval > 0 }

// LoadJanitorConfig reads JANITOR_INTERVAL_SEC and WORKFLOW_RUNS_RETENTION_DAYS from the
// environment, validates them, and returns a populated JanitorConfig.
//
// It fails fast (returns an error) when:
//   - JANITOR_INTERVAL_SEC is set but unparseable or negative,
//   - WORKFLOW_RUNS_RETENTION_DAYS is set but unparseable or non-positive.
//
// JANITOR_INTERVAL_SEC=0 leaves the janitor disabled (no ticker).
func LoadJanitorConfig() (*JanitorConfig, error) {
	intervalSec, err := parseNonNegativeInt("JANITOR_INTERVAL_SEC",
		getEnv("JANITOR_INTERVAL_SEC", ""), defaultJanitorIntervalSec)
	if err != nil {
		return nil, fmt.Errorf("config.LoadJanitorConfig: %w", err)
	}

	retentionDays, err := parsePositiveInt("WORKFLOW_RUNS_RETENTION_DAYS",
		getEnv("WORKFLOW_RUNS_RETENTION_DAYS", ""), defaultWorkflowRunsRetentionDay)
	if err != nil {
		return nil, fmt.Errorf("config.LoadJanitorConfig: %w", err)
	}

	return &JanitorConfig{
		Interval:      time.Duration(intervalSec) * time.Second,
		RunsRetention: time.Duration(retentionDays) * 24 * time.Hour,
		TokenGrace:    time.Duration(janitorTokenGraceDays) * 24 * time.Hour,
	}, nil
}
