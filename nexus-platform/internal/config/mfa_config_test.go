package config

import (
	"encoding/base64"
	"testing"
)

func b64Key(n int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, n))
}

func TestLoadMFAConfig_AbsentKey_DisabledNoError(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", "")
	cfg, err := LoadMFAConfig()
	if err != nil {
		t.Fatalf("LoadMFAConfig() error = %v; want nil (absent key must boot cleanly)", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true; want false when MFA_SECRET_ENC_KEY is unset")
	}
}

func TestLoadMFAConfig_ValidKey_Enabled(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", b64Key(32))
	cfg, err := LoadMFAConfig()
	if err != nil {
		t.Fatalf("LoadMFAConfig() error = %v; want nil", err)
	}
	if !cfg.Enabled {
		t.Fatal("Enabled = false; want true when a valid 32-byte key is configured")
	}
	if len(cfg.Key) != 32 {
		t.Fatalf("len(Key) = %d; want 32", len(cfg.Key))
	}
	if cfg.KeyID == "" {
		t.Fatal("KeyID is empty; want a non-empty default")
	}
}

func TestLoadMFAConfig_FailFast_NotBase64(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", "not-valid-base64!!!")
	if _, err := LoadMFAConfig(); err == nil {
		t.Fatal("LoadMFAConfig() error = nil; want fail-fast error for non-base64 key")
	}
}

func TestLoadMFAConfig_FailFast_WrongLength(t *testing.T) {
	// n=0 is deliberately excluded: base64("") == "" collapses to the SAME "absent" branch as
	// an unset var (config.getEnv cannot distinguish "set to empty" from "unset" anywhere in
	// this package) — that is the disabled case (see TestLoadMFAConfig_AbsentKey_DisabledNoError),
	// not a fail-fast case.
	tests := []int{16, 31, 33, 64}
	for _, n := range tests {
		t.Run("", func(t *testing.T) {
			t.Setenv("MFA_SECRET_ENC_KEY", b64Key(n))
			if _, err := LoadMFAConfig(); err == nil {
				t.Fatalf("LoadMFAConfig() with %d-byte decoded key: want error, got nil", n)
			}
		})
	}
}

func TestLoadMFAConfig_CustomKeyID(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", b64Key(32))
	t.Setenv("MFA_SECRET_ENC_KEY_ID", "rotated-2026")
	cfg, err := LoadMFAConfig()
	if err != nil {
		t.Fatalf("LoadMFAConfig() error = %v", err)
	}
	if cfg.KeyID != "rotated-2026" {
		t.Fatalf("KeyID = %q; want %q", cfg.KeyID, "rotated-2026")
	}
}

// --- MFA_FAIL_OPEN (MFA-ENFORCE INFRA-EMERGENCY kill-switch) ---------------------------------

func TestLoadMFAConfig_FailOpen_DefaultFalse(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", "")
	t.Setenv("MFA_FAIL_OPEN", "")
	cfg, err := LoadMFAConfig()
	if err != nil {
		t.Fatalf("LoadMFAConfig() error = %v", err)
	}
	if cfg.FailOpen {
		t.Fatal("FailOpen = true; want false by default (fail-closed is the safe default)")
	}
}

func TestLoadMFAConfig_FailOpen_True(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", "")
	t.Setenv("MFA_FAIL_OPEN", "true")
	cfg, err := LoadMFAConfig()
	if err != nil {
		t.Fatalf("LoadMFAConfig() error = %v", err)
	}
	if !cfg.FailOpen {
		t.Fatal("FailOpen = false; want true when MFA_FAIL_OPEN=true")
	}
}

// TestLoadMFAConfig_FailOpen_MalformedFailsFast: a mistyped kill-switch value must fail boot —
// never be silently read as "off" (operator believes the emergency override is active when it
// is not) or "on" (a typo silently downgrades every MFA account).
func TestLoadMFAConfig_FailOpen_MalformedFailsFast(t *testing.T) {
	t.Setenv("MFA_SECRET_ENC_KEY", "")
	t.Setenv("MFA_FAIL_OPEN", "yes-please")
	if _, err := LoadMFAConfig(); err == nil {
		t.Fatal("LoadMFAConfig() error = nil; want fail-fast for malformed MFA_FAIL_OPEN")
	}
}
