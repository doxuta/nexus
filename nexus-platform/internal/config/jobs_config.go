package config

import (
	"strconv"
	"time"
)

// Durable job-queue defaults (JOBS-1). The queue is ON by default — it is core infrastructure
// (durable email alerts today; the timer engine and campaign send stand on it next), not an
// opt-in feature. Every knob parses degrade-to-default (like the AI_*/DOCTOR_* knobs, unlike the
// fail-fast janitor knobs): a typo can only restore a default, never fail boot or zero a pool.
const (
	defaultJobsWorkers         = 2
	defaultJobsPollIntervalSec = 5
	defaultJobsLeaseSec        = 300 // 5min running-job lease before the reaper reclaims it
	defaultJobsMaxAttempts     = 5
	defaultJobsRetentionDays   = 30 // done/failed rows older than this are janitor-swept
	defaultJobsWebhookWorkers  = 2  // outbound_webhook's own lane (see JobsConfig.WebhookWorkers)
)

// JobsConfig holds the durable job-queue configuration read from the environment.
type JobsConfig struct {
	Enabled      bool          // JOBS_ENABLED (default true)
	Workers      int           // JOBS_WORKERS
	PollInterval time.Duration // JOBS_POLL_INTERVAL_SEC
	Lease        time.Duration // JOBS_LEASE_SEC
	MaxAttempts  int           // JOBS_MAX_ATTEMPTS (default for jobs enqueued without one)
	Retention    time.Duration // JOBS_RETENTION_DAYS (swept by the retention janitor)
	// WebhookWorkers (JOBS_WEBHOOK_WORKERS) is outbound_webhook's OWN worker lane; the Workers
	// pool never claims that type. An attempt holds its worker up to WEBHOOK_TIMEOUT_SEC, so a
	// lane of N survives a receiver that swallows packets at ~N/timeout attempts a second — and
	// email_alert/workflow_timer keep their pool whatever it does. Size it for the webhook
	// traffic of the deployment, not for the other types.
	WebhookWorkers int
}

// LoadJobsConfig reads the JOBS_* env vars with degrade-to-default semantics: unset, unparsable
// or non-positive values fall back to the defaults above. JOBS_ENABLED accepts strconv.ParseBool
// forms; an unparsable value degrades to true (the queue must not silently vanish on a typo —
// turning it OFF requires a literal "false"/"0").
func LoadJobsConfig() JobsConfig {
	enabled := true
	if raw := getEnv("JOBS_ENABLED", ""); raw != "" {
		if v, err := strconv.ParseBool(raw); err == nil {
			enabled = v
		}
	}
	return JobsConfig{
		Enabled:      enabled,
		Workers:      getEnvInt("JOBS_WORKERS", defaultJobsWorkers),
		PollInterval: time.Duration(getEnvInt("JOBS_POLL_INTERVAL_SEC", defaultJobsPollIntervalSec)) * time.Second,
		Lease:        time.Duration(getEnvInt("JOBS_LEASE_SEC", defaultJobsLeaseSec)) * time.Second,
		MaxAttempts:  getEnvInt("JOBS_MAX_ATTEMPTS", defaultJobsMaxAttempts),
		Retention:    time.Duration(getEnvInt("JOBS_RETENTION_DAYS", defaultJobsRetentionDays)) * 24 * time.Hour,
		// Degrade-to-default like every knob here — and 0 must not be honoured: the shared pool
		// excludes outbound_webhook, so a zero lane would strand every webhook job.
		WebhookWorkers: getEnvInt("JOBS_WEBHOOK_WORKERS", defaultJobsWebhookWorkers),
	}
}
