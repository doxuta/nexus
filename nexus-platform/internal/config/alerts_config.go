package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Security-alert defaults applied when the corresponding env var is unset.
// See ../specs/014-security-alerts/spec.md §5. Defaults are chosen so that dev/test boot
// safely with no SMTP configured (ALERTS_ENABLED defaults to false).
const (
	defaultAlertsEnabled = false
	defaultSMTPPort      = 587 // STARTTLS submission port
	defaultAlertsWorkers = 2
	defaultAlertsBuffer  = 256
)

// AlertsConfig holds the verified security-alert / SMTP configuration read from
// the environment. Per ../specs/014-security-alerts/spec.md §5, when alerts are enabled the
// SMTP host must be present (fail-fast on boot, mirroring the JWT_SECRET rule),
// avoiding an "enabled but mute" deployment.
type AlertsConfig struct {
	Enabled  bool
	SMTPHost string
	SMTPPort int
	SMTPUser string
	SMTPPass string
	SMTPFrom string
	Workers  int
	Buffer   int
}

// LoadAlertsConfig reads ALERTS_ENABLED, SMTP_* and ALERTS_WORKERS/ALERTS_BUFFER
// from the environment, validates them, and returns a populated AlertsConfig.
//
// It fails fast (returns an error) when:
//   - ALERTS_ENABLED is set to an unparseable boolean,
//   - SMTP_PORT / ALERTS_WORKERS / ALERTS_BUFFER are unparseable or non-positive,
//   - ALERTS_ENABLED=true but SMTP_HOST is empty (provider-agnostic boot guard).
//
// When ALERTS_ENABLED=false (the default), SMTP_HOST is NOT required: the caller
// wires a NoopSender and the missing host is irrelevant.
func LoadAlertsConfig() (*AlertsConfig, error) {
	enabled, err := parseBool("ALERTS_ENABLED", getEnv("ALERTS_ENABLED", ""), defaultAlertsEnabled)
	if err != nil {
		return nil, fmt.Errorf("config.LoadAlertsConfig: %w", err)
	}

	port, err := parsePositiveInt("SMTP_PORT", getEnv("SMTP_PORT", ""), defaultSMTPPort)
	if err != nil {
		return nil, fmt.Errorf("config.LoadAlertsConfig: %w", err)
	}

	workers, err := parsePositiveInt("ALERTS_WORKERS", getEnv("ALERTS_WORKERS", ""), defaultAlertsWorkers)
	if err != nil {
		return nil, fmt.Errorf("config.LoadAlertsConfig: %w", err)
	}

	buffer, err := parsePositiveInt("ALERTS_BUFFER", getEnv("ALERTS_BUFFER", ""), defaultAlertsBuffer)
	if err != nil {
		return nil, fmt.Errorf("config.LoadAlertsConfig: %w", err)
	}

	host := strings.TrimSpace(getEnv("SMTP_HOST", ""))

	// Fail-fast boot: enabling alerts without a destination SMTP host is an
	// almost-certain misconfiguration that would silently drop every alert.
	if enabled && host == "" {
		return nil, fmt.Errorf("config.LoadAlertsConfig: SMTP_HOST is required when ALERTS_ENABLED=true")
	}

	return &AlertsConfig{
		Enabled:  enabled,
		SMTPHost: host,
		SMTPPort: port,
		SMTPUser: getEnv("SMTP_USERNAME", ""),
		SMTPPass: getEnv("SMTP_PASSWORD", ""),
		SMTPFrom: getEnv("SMTP_FROM", ""),
		Workers:  workers,
		Buffer:   buffer,
	}, nil
}

// parseBool parses a boolean env value, falling back to def when empty, and
// rejects values that strconv.ParseBool cannot interpret. Accepted true values
// include 1/t/T/TRUE/true/True; false values include 0/f/F/FALSE/false/False.
func parseBool(name, raw string, def bool) (bool, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s is not a valid boolean (%q): %w", name, value, err)
	}
	return b, nil
}

// parsePositiveInt parses an integer env value, falling back to def when empty,
// and rejects values that do not parse or are not strictly positive.
func parsePositiveInt(name, raw string, def int) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return def, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid integer (%q): %w", name, value, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s must be > 0, got %d", name, n)
	}
	return n, nil
}

// parseNonNegativeInt parses an integer env value, falling back to def when empty,
// and rejects values that do not parse or are negative (0 is allowed — used as a
// "disabled" sentinel, e.g. WORKFLOW_SCHEDULER_INTERVAL_SEC=0 disables the scheduler).
func parseNonNegativeInt(name, raw string, def int) (int, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return def, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid integer (%q): %w", name, value, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be >= 0, got %d", name, n)
	}
	return n, nil
}
