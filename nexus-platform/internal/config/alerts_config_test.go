package config

import (
	"testing"
)

// clearAlertsEnv unsets all alert-related env vars so each case runs against a
// known-empty environment regardless of the host. t.Setenv restores them on
// cleanup.
func clearAlertsEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"ALERTS_ENABLED",
		"SMTP_HOST",
		"SMTP_PORT",
		"SMTP_USERNAME",
		"SMTP_PASSWORD",
		"SMTP_FROM",
		"ALERTS_WORKERS",
		"ALERTS_BUFFER",
	} {
		t.Setenv(k, "")
	}
}

// TestLoadAlertsConfig_DefaultsWhenEnvEmpty verifies the safe defaults: alerts
// disabled, SMTP port 587, 2 workers, 256 buffer, and no SMTP_HOST required.
func TestLoadAlertsConfig_DefaultsWhenEnvEmpty(t *testing.T) {
	clearAlertsEnv(t)

	cfg, err := LoadAlertsConfig()
	if err != nil {
		t.Fatalf("LoadAlertsConfig: unexpected error: %v", err)
	}
	if cfg.Enabled != false {
		t.Errorf("Enabled = %v, want false (safe dev/test default)", cfg.Enabled)
	}
	if cfg.SMTPHost != "" {
		t.Errorf("SMTPHost = %q, want empty", cfg.SMTPHost)
	}
	if cfg.SMTPPort != defaultSMTPPort {
		t.Errorf("SMTPPort = %d, want default %d", cfg.SMTPPort, defaultSMTPPort)
	}
	if cfg.SMTPUser != "" {
		t.Errorf("SMTPUser = %q, want empty", cfg.SMTPUser)
	}
	if cfg.SMTPPass != "" {
		t.Errorf("SMTPPass = %q, want empty", cfg.SMTPPass)
	}
	if cfg.SMTPFrom != "" {
		t.Errorf("SMTPFrom = %q, want empty", cfg.SMTPFrom)
	}
	if cfg.Workers != defaultAlertsWorkers {
		t.Errorf("Workers = %d, want default %d", cfg.Workers, defaultAlertsWorkers)
	}
	if cfg.Buffer != defaultAlertsBuffer {
		t.Errorf("Buffer = %d, want default %d", cfg.Buffer, defaultAlertsBuffer)
	}
}

// TestLoadAlertsConfig_EnabledMissingHostFails pins the fail-fast boot rule:
// ALERTS_ENABLED=true with an empty SMTP_HOST must return an error.
func TestLoadAlertsConfig_EnabledMissingHostFails(t *testing.T) {
	clearAlertsEnv(t)
	t.Setenv("ALERTS_ENABLED", "true")
	// SMTP_HOST intentionally left empty.

	if _, err := LoadAlertsConfig(); err == nil {
		t.Fatal("LoadAlertsConfig: expected error when ALERTS_ENABLED=true and SMTP_HOST empty, got nil")
	}
}

// TestLoadAlertsConfig_EnabledWithHostOK confirms a fully-enabled config with a
// host (and provided SMTP fields) loads successfully and carries the values.
func TestLoadAlertsConfig_EnabledWithHostOK(t *testing.T) {
	clearAlertsEnv(t)
	t.Setenv("ALERTS_ENABLED", "true")
	t.Setenv("SMTP_HOST", "smtp.example.com")
	t.Setenv("SMTP_PORT", "2525")
	t.Setenv("SMTP_USERNAME", "alerts@example.com")
	t.Setenv("SMTP_PASSWORD", "s3cret")
	t.Setenv("SMTP_FROM", "no-reply@example.com")
	t.Setenv("ALERTS_WORKERS", "4")
	t.Setenv("ALERTS_BUFFER", "512")

	cfg, err := LoadAlertsConfig()
	if err != nil {
		t.Fatalf("LoadAlertsConfig: unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if cfg.SMTPHost != "smtp.example.com" {
		t.Errorf("SMTPHost = %q, want smtp.example.com", cfg.SMTPHost)
	}
	if cfg.SMTPPort != 2525 {
		t.Errorf("SMTPPort = %d, want 2525", cfg.SMTPPort)
	}
	if cfg.SMTPUser != "alerts@example.com" {
		t.Errorf("SMTPUser = %q, want alerts@example.com", cfg.SMTPUser)
	}
	if cfg.SMTPPass != "s3cret" {
		t.Errorf("SMTPPass = %q, want s3cret", cfg.SMTPPass)
	}
	if cfg.SMTPFrom != "no-reply@example.com" {
		t.Errorf("SMTPFrom = %q, want no-reply@example.com", cfg.SMTPFrom)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers = %d, want 4", cfg.Workers)
	}
	if cfg.Buffer != 512 {
		t.Errorf("Buffer = %d, want 512", cfg.Buffer)
	}
}

// TestLoadAlertsConfig_DisabledMissingHostOK confirms the SMTP validation is
// skipped entirely when alerts are disabled: an empty host must NOT fail.
func TestLoadAlertsConfig_DisabledMissingHostOK(t *testing.T) {
	clearAlertsEnv(t)
	t.Setenv("ALERTS_ENABLED", "false")
	// SMTP_HOST intentionally left empty.

	cfg, err := LoadAlertsConfig()
	if err != nil {
		t.Fatalf("LoadAlertsConfig: unexpected error when disabled with empty host: %v", err)
	}
	if cfg.Enabled {
		t.Error("Enabled = true, want false")
	}
}

// TestLoadAlertsConfig_InvalidEnabled rejects an unparseable ALERTS_ENABLED.
func TestLoadAlertsConfig_InvalidEnabled(t *testing.T) {
	clearAlertsEnv(t)
	t.Setenv("ALERTS_ENABLED", "yesplease")

	if _, err := LoadAlertsConfig(); err == nil {
		t.Fatal("LoadAlertsConfig: expected error for invalid ALERTS_ENABLED, got nil")
	}
}

// TestLoadAlertsConfig_InvalidInts rejects unparseable / non-positive integer
// envs for SMTP_PORT, ALERTS_WORKERS and ALERTS_BUFFER.
func TestLoadAlertsConfig_InvalidInts(t *testing.T) {
	cases := []struct {
		name string
		env  string
		val  string
	}{
		{"port-garbage", "SMTP_PORT", "abc"},
		{"port-zero", "SMTP_PORT", "0"},
		{"port-negative", "SMTP_PORT", "-1"},
		{"workers-garbage", "ALERTS_WORKERS", "two"},
		{"workers-zero", "ALERTS_WORKERS", "0"},
		{"buffer-garbage", "ALERTS_BUFFER", "lots"},
		{"buffer-negative", "ALERTS_BUFFER", "-5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearAlertsEnv(t)
			// Keep alerts disabled so the host guard never short-circuits the int checks.
			t.Setenv("ALERTS_ENABLED", "false")
			t.Setenv(tc.env, tc.val)

			if _, err := LoadAlertsConfig(); err == nil {
				t.Fatalf("LoadAlertsConfig: expected error for %s=%q, got nil", tc.env, tc.val)
			}
		})
	}
}
