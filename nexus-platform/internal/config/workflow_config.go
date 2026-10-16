package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EPIC 4 M2 workflow-scheduler / webhook defaults applied when the corresponding env var is unset.
// The scheduler is ON by default (60s): Studio lets a builder save a "scheduled" rule, and a rule
// that never fires on a fresh install is a silent trap (#291). Set WORKFLOW_SCHEDULER_INTERVAL_SEC=0
// to turn it off explicitly; Studio reads the state from GET /admin/workflows/scheduler.
const (
	defaultWorkflowSchedulerIntervalSec = 60 // 0 (explicit env) = scheduler DISABLED (no ticker)
	defaultWebhookTimeoutSec            = 10
	defaultWebhookMaxRetries            = 3
	defaultWebhookBackoffMs             = 200
	// defaultWorkflowMaxChainDepth bounds create_record automation recursion (CREATE-RECORD). Kept
	// in lockstep with workflow.DefaultMaxChainDepth (the clamp WithRecordCreator applies). A create_
	// record chain terminates after this many hops, so a self-referential rule can never loop forever.
	defaultWorkflowMaxChainDepth = 5
)

// WorkflowConfig holds the verified EPIC 4 M2 scheduler + outbound-webhook configuration read from
// the environment. The scheduler ticks every SchedulerInterval (0 = disabled). WebhookTimeout caps
// every webhook POST on both paths. WebhookMaxRetries/WebhookBackoff shape the IN-PROCESS retry of
// the inline path only (JOBS_ENABLED=false): with the durable queue on (the default) each claim
// makes one POST and the queue owns retry/backoff (services.OutboundWebhookMaxAttempts,
// DAILY-QUEUE #26).
type WorkflowConfig struct {
	SchedulerInterval time.Duration // 0 = scheduler disabled
	WebhookTimeout    time.Duration
	WebhookMaxRetries int
	WebhookBackoff    time.Duration
	// MaxChainDepth bounds create_record automation recursion (CREATE-RECORD, WORKFLOW_MAX_CHAIN_DEPTH).
	// Degrade-to-default on a bad/absent value (like the AI_*/RATELIMIT_* knobs) — never fail boot —
	// so a typo can never silently disable the recursion guard or leave it unbounded.
	MaxChainDepth int
}

// SchedulerEnabled reports whether the background scheduler should run (a positive tick interval).
func (c WorkflowConfig) SchedulerEnabled() bool { return c.SchedulerInterval > 0 }

// LoadWorkflowConfig reads WORKFLOW_SCHEDULER_INTERVAL_SEC and the WEBHOOK_* tuning vars from the
// environment, validates them, and returns a populated WorkflowConfig.
//
// It fails fast (returns an error) when:
//   - WORKFLOW_SCHEDULER_INTERVAL_SEC is set but unparseable or negative,
//   - WEBHOOK_TIMEOUT_SEC / WEBHOOK_MAX_RETRIES / WEBHOOK_BACKOFF_MS are unparseable or non-positive.
//
// WORKFLOW_SCHEDULER_INTERVAL_SEC unset = 60s; an explicit 0 leaves the scheduler disabled.
func LoadWorkflowConfig() (*WorkflowConfig, error) {
	intervalSec, err := parseNonNegativeInt("WORKFLOW_SCHEDULER_INTERVAL_SEC",
		getEnv("WORKFLOW_SCHEDULER_INTERVAL_SEC", ""), defaultWorkflowSchedulerIntervalSec)
	if err != nil {
		return nil, fmt.Errorf("config.LoadWorkflowConfig: %w", err)
	}

	timeoutSec, err := parsePositiveInt("WEBHOOK_TIMEOUT_SEC",
		getEnv("WEBHOOK_TIMEOUT_SEC", ""), defaultWebhookTimeoutSec)
	if err != nil {
		return nil, fmt.Errorf("config.LoadWorkflowConfig: %w", err)
	}

	maxRetries, err := parsePositiveInt("WEBHOOK_MAX_RETRIES",
		getEnv("WEBHOOK_MAX_RETRIES", ""), defaultWebhookMaxRetries)
	if err != nil {
		return nil, fmt.Errorf("config.LoadWorkflowConfig: %w", err)
	}

	backoffMs, err := parsePositiveInt("WEBHOOK_BACKOFF_MS",
		getEnv("WEBHOOK_BACKOFF_MS", ""), defaultWebhookBackoffMs)
	if err != nil {
		return nil, fmt.Errorf("config.LoadWorkflowConfig: %w", err)
	}

	// WORKFLOW_MAX_CHAIN_DEPTH: degrade-to-default (NOT fail-fast) — a bad value must never disable
	// the create_record recursion guard (0) nor leave it unbounded. Absent/unparseable/<1 → default.
	maxChainDepth := defaultWorkflowMaxChainDepth
	if raw := strings.TrimSpace(getEnv("WORKFLOW_MAX_CHAIN_DEPTH", "")); raw != "" {
		if v, perr := strconv.Atoi(raw); perr == nil && v >= 1 {
			maxChainDepth = v
		}
	}

	return &WorkflowConfig{
		SchedulerInterval: time.Duration(intervalSec) * time.Second,
		WebhookTimeout:    time.Duration(timeoutSec) * time.Second,
		WebhookMaxRetries: maxRetries,
		WebhookBackoff:    time.Duration(backoffMs) * time.Millisecond,
		MaxChainDepth:     maxChainDepth,
	}, nil
}
