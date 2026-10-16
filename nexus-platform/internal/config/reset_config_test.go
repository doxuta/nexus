package config

import "testing"

func TestLoadResetConfig_AlertsDisabled_NoFailFast(t *testing.T) {
	t.Setenv("RESET_URL_BASE", "")
	t.Setenv("RESET_MAIL_FROM", "")
	t.Setenv("SMTP_FROM", "")

	cfg, err := LoadResetConfig(false)
	if err != nil {
		t.Fatalf("LoadResetConfig(false): unexpected error: %v", err)
	}
	if cfg.URLBase != "" || cfg.From != "" {
		t.Errorf("expected empty URLBase/From when unset, got URLBase=%q From=%q", cfg.URLBase, cfg.From)
	}
}

func TestLoadResetConfig_AlertsEnabled_MissingURLBase_FailsFast(t *testing.T) {
	t.Setenv("RESET_URL_BASE", "")
	t.Setenv("RESET_MAIL_FROM", "reset@nexus.test")

	if _, err := LoadResetConfig(true); err == nil {
		t.Fatal("expected fail-fast error when ALERTS_ENABLED=true and RESET_URL_BASE is empty")
	}
}

func TestLoadResetConfig_AlertsEnabled_MissingFrom_FailsFast(t *testing.T) {
	t.Setenv("RESET_URL_BASE", "https://app.nexus.test/reset")
	t.Setenv("RESET_MAIL_FROM", "")
	t.Setenv("SMTP_FROM", "")

	if _, err := LoadResetConfig(true); err == nil {
		t.Fatal("expected fail-fast error when ALERTS_ENABLED=true and no From is configured")
	}
}

func TestLoadResetConfig_AlertsEnabled_FromFallsBackToSMTPFrom(t *testing.T) {
	t.Setenv("RESET_URL_BASE", "https://app.nexus.test/reset")
	t.Setenv("RESET_MAIL_FROM", "")
	t.Setenv("SMTP_FROM", "alerts@nexus.test")

	cfg, err := LoadResetConfig(true)
	if err != nil {
		t.Fatalf("LoadResetConfig(true): unexpected error: %v", err)
	}
	if cfg.From != "alerts@nexus.test" {
		t.Errorf("From = %q, want fallback to SMTP_FROM", cfg.From)
	}
}

func TestLoadResetConfig_AlertsEnabled_Valid(t *testing.T) {
	t.Setenv("RESET_URL_BASE", "https://app.nexus.test/reset")
	t.Setenv("RESET_MAIL_FROM", "reset@nexus.test")
	t.Setenv("SMTP_FROM", "alerts@nexus.test")

	cfg, err := LoadResetConfig(true)
	if err != nil {
		t.Fatalf("LoadResetConfig(true): unexpected error: %v", err)
	}
	if cfg.URLBase != "https://app.nexus.test/reset" {
		t.Errorf("URLBase = %q", cfg.URLBase)
	}
	if cfg.From != "reset@nexus.test" {
		t.Errorf("From = %q, want RESET_MAIL_FROM to take priority over SMTP_FROM", cfg.From)
	}
}
