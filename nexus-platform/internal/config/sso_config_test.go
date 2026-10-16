package config

import "testing"

func TestLoadSSOConfig_AbsentKey_DisabledNoError(t *testing.T) {
	t.Setenv("SSO_SECRET_ENC_KEY", "")
	t.Setenv("SSO_REDIRECT_BASE", "")
	cfg, err := LoadSSOConfig()
	if err != nil {
		t.Fatalf("LoadSSOConfig() error = %v; want nil (absent key must boot cleanly)", err)
	}
	if cfg.Enabled {
		t.Fatal("Enabled = true; want false when SSO_SECRET_ENC_KEY is unset")
	}
}

func TestLoadSSOConfig_ValidKeyAndRedirectBase_Enabled(t *testing.T) {
	t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
	t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
	cfg, err := LoadSSOConfig()
	if err != nil {
		t.Fatalf("LoadSSOConfig() error = %v; want nil", err)
	}
	if !cfg.Enabled {
		t.Fatal("Enabled = false; want true when a valid 32-byte key + redirect base are configured")
	}
	if len(cfg.Key) != 32 {
		t.Fatalf("len(Key) = %d; want 32", len(cfg.Key))
	}
	if cfg.KeyID == "" {
		t.Fatal("KeyID is empty; want a non-empty default")
	}
	if cfg.RedirectBase != "https://api.example.com/api/v1" {
		t.Fatalf("RedirectBase = %q; want the configured value", cfg.RedirectBase)
	}
}

func TestLoadSSOConfig_FailFast_NotBase64(t *testing.T) {
	t.Setenv("SSO_SECRET_ENC_KEY", "not-valid-base64!!!")
	t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
	if _, err := LoadSSOConfig(); err == nil {
		t.Fatal("LoadSSOConfig() error = nil; want fail-fast error for non-base64 key")
	}
}

func TestLoadSSOConfig_FailFast_WrongLength(t *testing.T) {
	tests := []int{16, 31, 33, 64}
	for _, n := range tests {
		t.Run("", func(t *testing.T) {
			t.Setenv("SSO_SECRET_ENC_KEY", b64Key(n))
			t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
			if _, err := LoadSSOConfig(); err == nil {
				t.Fatalf("LoadSSOConfig() with %d-byte decoded key: want error, got nil", n)
			}
		})
	}
}

// TestLoadSSOConfig_FailFast_KeyPresentButRedirectBaseEmpty is the boot fail-fast: a key
// configured but SSO_REDIRECT_BASE empty means /callback can never build a valid redirect_uri —
// this must fail boot rather than let every provider silently answer with a broken flow
// (mirrors config.LoadResetConfig's RESET_URL_BASE fail-fast, ../specs/004-sso-oidc/spec.md §"#8").
func TestLoadSSOConfig_FailFast_KeyPresentButRedirectBaseEmpty(t *testing.T) {
	t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
	t.Setenv("SSO_REDIRECT_BASE", "")
	if _, err := LoadSSOConfig(); err == nil {
		t.Fatal("LoadSSOConfig() error = nil; want fail-fast error when key present but SSO_REDIRECT_BASE empty")
	}
}

// SSO-STATE-COOKIE review NIT-1: a malformed SSO_REDIRECT_BASE (missing scheme, no host)
// silently produced a broken callback redirect_uri and cookie Path — fail fast at boot instead.
func TestLoadSSOConfig_FailFast_MalformedRedirectBase(t *testing.T) {
	for _, base := range []string{"api.example.com/api/v1", "ftp://api.example.com", "/api/v1", "https://"} {
		t.Run(base, func(t *testing.T) {
			t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
			t.Setenv("SSO_REDIRECT_BASE", base)
			if _, err := LoadSSOConfig(); err == nil {
				t.Fatalf("LoadSSOConfig() error = nil for malformed base %q; want fail-fast", base)
			}
		})
	}
}

// SSO-FE-REDIRECT: SSO_FRONTEND_BASE is OPTIONAL (absent -> "" keeps /callback in legacy JSON
// mode), must be an absolute http(s) URL when present (fail fast, mirrors SSO_REDIRECT_BASE),
// and trailing slashes are trimmed so URL joins never produce "//".
func TestLoadSSOConfig_FrontendBase(t *testing.T) {
	t.Run("absent stays empty (legacy JSON mode)", func(t *testing.T) {
		t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
		t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
		t.Setenv("SSO_FRONTEND_BASE", "")
		cfg, err := LoadSSOConfig()
		if err != nil {
			t.Fatalf("LoadSSOConfig() error = %v; want nil (frontend base is optional)", err)
		}
		if cfg.FrontendBase != "" {
			t.Fatalf("FrontendBase = %q; want empty when SSO_FRONTEND_BASE is unset", cfg.FrontendBase)
		}
	})

	for _, base := range []string{"not a url", "ftp://x"} {
		t.Run("malformed "+base+" fails fast", func(t *testing.T) {
			t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
			t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
			t.Setenv("SSO_FRONTEND_BASE", base)
			if _, err := LoadSSOConfig(); err == nil {
				t.Fatalf("LoadSSOConfig() error = nil for malformed SSO_FRONTEND_BASE %q; want fail-fast", base)
			}
		})
	}

	t.Run("valid base trims trailing slash", func(t *testing.T) {
		t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
		t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
		t.Setenv("SSO_FRONTEND_BASE", "https://app.example.com/")
		cfg, err := LoadSSOConfig()
		if err != nil {
			t.Fatalf("LoadSSOConfig() error = %v; want nil", err)
		}
		if cfg.FrontendBase != "https://app.example.com" {
			t.Fatalf("FrontendBase = %q; want %q (trailing slash trimmed)", cfg.FrontendBase, "https://app.example.com")
		}
	})
}

func TestLoadSSOConfig_CustomKeyID(t *testing.T) {
	t.Setenv("SSO_SECRET_ENC_KEY", b64Key(32))
	t.Setenv("SSO_SECRET_ENC_KEY_ID", "rotated-2026")
	t.Setenv("SSO_REDIRECT_BASE", "https://api.example.com/api/v1")
	cfg, err := LoadSSOConfig()
	if err != nil {
		t.Fatalf("LoadSSOConfig() error = %v", err)
	}
	if cfg.KeyID != "rotated-2026" {
		t.Fatalf("KeyID = %q; want %q", cfg.KeyID, "rotated-2026")
	}
}
