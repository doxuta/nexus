package config

import (
	"testing"
	"time"
)

// TestLoadJobsConfigDefaults: unset env yields the documented defaults (queue ON).
func TestLoadJobsConfigDefaults(t *testing.T) {
	for _, k := range []string{"JOBS_ENABLED", "JOBS_WORKERS", "JOBS_POLL_INTERVAL_SEC", "JOBS_LEASE_SEC", "JOBS_MAX_ATTEMPTS", "JOBS_RETENTION_DAYS", "JOBS_WEBHOOK_WORKERS"} {
		t.Setenv(k, "")
	}
	c := LoadJobsConfig()
	if !c.Enabled {
		t.Fatal("queue must be enabled by default")
	}
	if c.Workers != 2 || c.PollInterval != 5*time.Second || c.Lease != 300*time.Second ||
		c.MaxAttempts != 5 || c.Retention != 30*24*time.Hour || c.WebhookWorkers != 2 {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

// TestLoadJobsConfigOverridesAndDegrade: valid values apply; garbage/non-positive values degrade
// to defaults (never fail boot, never zero a pool); JOBS_ENABLED=false disables; unparsable
// JOBS_ENABLED degrades to true (the queue must not vanish on a typo).
func TestLoadJobsConfigOverridesAndDegrade(t *testing.T) {
	t.Setenv("JOBS_ENABLED", "false")
	t.Setenv("JOBS_WORKERS", "8")
	t.Setenv("JOBS_POLL_INTERVAL_SEC", "1")
	t.Setenv("JOBS_LEASE_SEC", "-3")     // degrade
	t.Setenv("JOBS_MAX_ATTEMPTS", "abc") // degrade
	t.Setenv("JOBS_RETENTION_DAYS", "7")
	t.Setenv("JOBS_WEBHOOK_WORKERS", "0") // degrade: zero would strand every webhook job
	c := LoadJobsConfig()
	if c.Enabled {
		t.Fatal("JOBS_ENABLED=false must disable")
	}
	if c.Workers != 8 || c.PollInterval != time.Second || c.Retention != 7*24*time.Hour {
		t.Fatalf("overrides not applied: %+v", c)
	}
	if c.Lease != 300*time.Second || c.MaxAttempts != 5 || c.WebhookWorkers != 2 {
		t.Fatalf("bad values must degrade to defaults: %+v", c)
	}

	t.Setenv("JOBS_ENABLED", "banana")
	if !LoadJobsConfig().Enabled {
		t.Fatal("unparsable JOBS_ENABLED must degrade to true")
	}
}
