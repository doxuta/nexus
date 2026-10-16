package config

import (
	"fmt"
	"time"
)

// Usage (specs/066 L4) defaults applied when the env var is unset.
const (
	defaultUsageRollupIntervalSec = 3600 // hourly roll-up; 0 = disabled
	defaultUsageRetentionDays     = 730  // sys_usage_daily rows older than this are swept
)

// UsageConfig is the verified configuration of the per-day usage roll-up.
type UsageConfig struct {
	RollupInterval time.Duration // 0 = roller disabled (today is still computed live on GET)
	Retention      time.Duration // age threshold of sys_usage_daily rows for the janitor
}

// Enabled reports whether the background roller should run.
func (c UsageConfig) Enabled() bool { return c.RollupInterval > 0 }

// LoadUsageConfig reads USAGE_ROLLUP_INTERVAL_SEC (>= 0, 0 = off) and USAGE_RETENTION_DAYS (> 0).
// A wrong value is a boot error, like the janitor knobs: a typo must not silently pick a cadence
// or a retention that deletes figures.
func LoadUsageConfig() (*UsageConfig, error) {
	sec, err := parseNonNegativeInt("USAGE_ROLLUP_INTERVAL_SEC",
		getEnv("USAGE_ROLLUP_INTERVAL_SEC", ""), defaultUsageRollupIntervalSec)
	if err != nil {
		return nil, fmt.Errorf("config.LoadUsageConfig: %w", err)
	}
	days, err := parsePositiveInt("USAGE_RETENTION_DAYS",
		getEnv("USAGE_RETENTION_DAYS", ""), defaultUsageRetentionDays)
	if err != nil {
		return nil, fmt.Errorf("config.LoadUsageConfig: %w", err)
	}
	return &UsageConfig{
		RollupInterval: time.Duration(sec) * time.Second,
		Retention:      time.Duration(days) * 24 * time.Hour,
	}, nil
}
