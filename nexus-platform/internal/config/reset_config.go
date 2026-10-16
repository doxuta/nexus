package config

import (
	"fmt"
	"strings"
)

// ResetConfig holds the verified forgot/reset-password mail configuration read from the
// environment (../specs/002-password-reset/spec.md §"#1" MUST-FIX 6).
//
// There is no dedicated "forgot-password enabled" flag: ALERTS_ENABLED is already the single
// switch that makes outbound email real (main.go wires a NoopSender when it is false), so
// RESET_URL_BASE + a non-empty From are only REQUIRED (fail-fast) when ALERTS_ENABLED=true.
// This mirrors LoadAlertsConfig's "enabled but mute" boot guard without introducing a second,
// redundant on/off knob, and keeps dev/test boot working with alerts disabled and no SMTP.
type ResetConfig struct {
	// URLBase is the base URL the reset link is built from: "<URLBase>?token=<raw>".
	URLBase string
	// From is the envelope/header sender for reset emails: RESET_MAIL_FROM if set, else
	// SMTP_FROM (shared with the security-alert sender), else empty (only reachable when
	// alerts are disabled, in which case it is never dispatched over real SMTP).
	From string
}

// LoadResetConfig reads RESET_URL_BASE and RESET_MAIL_FROM (falling back to SMTP_FROM) from
// the environment. When alertsEnabled is true it fails fast (returns an error) if RESET_URL_BASE
// is empty or if no From address (neither RESET_MAIL_FROM nor SMTP_FROM) is configured — an
// "enabled but the link/sender is broken" deployment would otherwise silently mail out dead
// links or an empty envelope sender. When alertsEnabled is false, both fields are best-effort
// (no fail-fast) since no real email is ever sent (NoopSender).
func LoadResetConfig(alertsEnabled bool) (*ResetConfig, error) {
	urlBase := strings.TrimSpace(getEnv("RESET_URL_BASE", ""))
	from := strings.TrimSpace(getEnv("RESET_MAIL_FROM", ""))
	if from == "" {
		from = strings.TrimSpace(getEnv("SMTP_FROM", ""))
	}

	if alertsEnabled {
		if urlBase == "" {
			return nil, fmt.Errorf("config.LoadResetConfig: RESET_URL_BASE is required when ALERTS_ENABLED=true")
		}
		if from == "" {
			return nil, fmt.Errorf("config.LoadResetConfig: RESET_MAIL_FROM (or SMTP_FROM) is required when ALERTS_ENABLED=true")
		}
	}

	return &ResetConfig{URLBase: urlBase, From: from}, nil
}
