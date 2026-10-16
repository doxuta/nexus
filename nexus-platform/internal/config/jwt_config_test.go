package config

import (
	"strings"
	"testing"
	"time"
)

// setJWTEnv sets the JWT-related env vars for a test and clears them on cleanup,
// so each case runs against a known environment regardless of the host.
func setJWTEnv(t *testing.T, secret, accessTTL, refreshTTL string) {
	t.Helper()
	t.Setenv("JWT_SECRET", secret)
	t.Setenv("JWT_ACCESS_TTL", accessTTL)
	t.Setenv("JWT_REFRESH_TTL", refreshTTL)
}

func TestLoadJWTConfig_Valid(t *testing.T) {
	setJWTEnv(t, "a-sufficiently-long-real-secret-value-1234567890", "15m", "168h")

	cfg, err := LoadJWTConfig()
	if err != nil {
		t.Fatalf("LoadJWTConfig: unexpected error: %v", err)
	}
	if cfg.JWTSecret != "a-sufficiently-long-real-secret-value-1234567890" {
		t.Errorf("JWTSecret = %q, want the env value", cfg.JWTSecret)
	}
	if cfg.AccessTTL != 15*time.Minute {
		t.Errorf("AccessTTL = %v, want 15m", cfg.AccessTTL)
	}
	if cfg.RefreshTTL != 168*time.Hour {
		t.Errorf("RefreshTTL = %v, want 168h", cfg.RefreshTTL)
	}
}

func TestLoadJWTConfig_MissingSecret(t *testing.T) {
	setJWTEnv(t, "", "15m", "168h")

	if _, err := LoadJWTConfig(); err == nil {
		t.Fatal("LoadJWTConfig: expected error for empty JWT_SECRET, got nil")
	}
}

func TestLoadJWTConfig_PlaceholderSecret(t *testing.T) {
	placeholders := []string{
		"your_secret",
		"your_secret_key",
		"your-secret",
		"YOUR_SECRET",
		"change-me",
		"changeme",
		"change_me",
		"secret",
		"placeholder",
	}
	for _, p := range placeholders {
		t.Run(p, func(t *testing.T) {
			setJWTEnv(t, p, "15m", "168h")
			if _, err := LoadJWTConfig(); err == nil {
				t.Fatalf("LoadJWTConfig: expected error for placeholder secret %q, got nil", p)
			}
		})
	}
}

func TestLoadJWTConfig_TooShortSecret(t *testing.T) {
	setJWTEnv(t, "short", "15m", "168h")
	if _, err := LoadJWTConfig(); err == nil {
		t.Fatal("LoadJWTConfig: expected error for too-short secret, got nil")
	}
}

// TestLoadJWTConfig_SecretLengthBoundary pins the RFC 7518 §3.2 floor (HS256 key >= 32 bytes):
// a 31-byte secret is rejected, a 32-byte secret is accepted. Lengths are measured in BYTES,
// not runes, so a multi-byte secret that is 32 runes but < 32 bytes would also be rejected.
func TestLoadJWTConfig_SecretLengthBoundary(t *testing.T) {
	const want = 32
	just31 := strings.Repeat("a", want-1) // 31 bytes -> must fail
	setJWTEnv(t, just31, "15m", "168h")
	if _, err := LoadJWTConfig(); err == nil {
		t.Fatalf("LoadJWTConfig: expected error for %d-byte secret (< %d), got nil", len(just31), want)
	}

	exactly32 := strings.Repeat("a", want) // 32 bytes -> must pass
	setJWTEnv(t, exactly32, "15m", "168h")
	cfg, err := LoadJWTConfig()
	if err != nil {
		t.Fatalf("LoadJWTConfig: unexpected error for %d-byte secret: %v", len(exactly32), err)
	}
	if cfg.JWTSecret != exactly32 {
		t.Errorf("JWTSecret = %q, want the 32-byte env value", cfg.JWTSecret)
	}
}

func TestLoadJWTConfig_InvalidAccessTTL(t *testing.T) {
	cases := []string{"not-a-duration", "0", "0s", "-5m"}
	for _, ttl := range cases {
		t.Run(ttl, func(t *testing.T) {
			setJWTEnv(t, "a-sufficiently-long-real-secret-value-1234567890", ttl, "168h")
			if _, err := LoadJWTConfig(); err == nil {
				t.Fatalf("LoadJWTConfig: expected error for AccessTTL=%q, got nil", ttl)
			}
		})
	}
}

func TestLoadJWTConfig_InvalidRefreshTTL(t *testing.T) {
	cases := []string{"garbage", "0", "0s", "-1h"}
	for _, ttl := range cases {
		t.Run(ttl, func(t *testing.T) {
			setJWTEnv(t, "a-sufficiently-long-real-secret-value-1234567890", "15m", ttl)
			if _, err := LoadJWTConfig(); err == nil {
				t.Fatalf("LoadJWTConfig: expected error for RefreshTTL=%q, got nil", ttl)
			}
		})
	}
}

func TestLoadJWTConfig_DefaultTTLs(t *testing.T) {
	// When TTL env vars are unset, sane defaults should apply (access 15m, refresh 168h).
	t.Setenv("JWT_SECRET", "a-sufficiently-long-real-secret-value-1234567890")
	t.Setenv("JWT_ACCESS_TTL", "")
	t.Setenv("JWT_REFRESH_TTL", "")

	cfg, err := LoadJWTConfig()
	if err != nil {
		t.Fatalf("LoadJWTConfig: unexpected error: %v", err)
	}
	if cfg.AccessTTL != defaultAccessTTL {
		t.Errorf("AccessTTL = %v, want default %v", cfg.AccessTTL, defaultAccessTTL)
	}
	if cfg.RefreshTTL != defaultRefreshTTL {
		t.Errorf("RefreshTTL = %v, want default %v", cfg.RefreshTTL, defaultRefreshTTL)
	}
}

func TestLoadJWTConfig_RefreshNotShorterThanAccess(t *testing.T) {
	// A refresh TTL shorter than the access TTL is almost certainly a misconfiguration.
	setJWTEnv(t, "a-sufficiently-long-real-secret-value-1234567890", "30m", "10m")
	if _, err := LoadJWTConfig(); err == nil {
		t.Fatal("LoadJWTConfig: expected error when RefreshTTL < AccessTTL, got nil")
	}
}
