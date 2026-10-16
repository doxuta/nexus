package config

import (
	"fmt"
	"strings"
	"time"
)

// JWT TTL defaults applied when the corresponding env var is unset.
// Access tokens are short-lived; refresh tokens are long-lived (see ../specs/001-auth-security/spec.md §5).
const (
	defaultAccessTTL  = 15 * time.Minute
	defaultRefreshTTL = 7 * 24 * time.Hour // 168h
	// minSecretLength is the minimum HS256 signing-secret length, in bytes. RFC 7518 §3.2
	// requires an HMAC-SHA256 key be AT LEAST the hash output size (256 bits = 32 bytes);
	// a shorter key weakens the MAC. We treat the env value as raw UTF-8 bytes, so we count
	// bytes (len) — not runes — when enforcing this floor.
	minSecretLength = 32
)

// JWTConfig holds the verified JWT signing configuration read from the environment.
// Per ../specs/001-auth-security/spec.md §2 A1 + §5, the secret must be a real value (fail-fast
// on empty/placeholder) and both TTLs must be positive durations.
type JWTConfig struct {
	JWTSecret  string
	AccessTTL  time.Duration
	RefreshTTL time.Duration
}

// placeholderSecrets is the set of well-known dummy/example secret values that must
// never reach a running service. Comparison is case-insensitive.
var placeholderSecrets = map[string]struct{}{
	"your_secret":     {},
	"your_secret_key": {},
	"your-secret":     {},
	"yoursecret":      {},
	"change-me":       {},
	"changeme":        {},
	"change_me":       {},
	"secret":          {},
	"placeholder":     {},
	"example":         {},
	"test":            {},
	"dummy":           {},
}

// LoadJWTConfig reads JWT_SECRET, JWT_ACCESS_TTL and JWT_REFRESH_TTL from the
// environment, validates them, and returns a populated JWTConfig. It fails fast
// (returns an error) when the secret is missing/placeholder/too short, when a TTL
// is unparseable or non-positive, or when the refresh TTL is shorter than the
// access TTL (an almost-certain misconfiguration).
func LoadJWTConfig() (*JWTConfig, error) {
	secret, err := validateJWTSecret(getEnv("JWT_SECRET", ""))
	if err != nil {
		return nil, fmt.Errorf("config.LoadJWTConfig: %w", err)
	}

	accessTTL, err := parsePositiveTTL("JWT_ACCESS_TTL", getEnv("JWT_ACCESS_TTL", ""), defaultAccessTTL)
	if err != nil {
		return nil, fmt.Errorf("config.LoadJWTConfig: %w", err)
	}

	refreshTTL, err := parsePositiveTTL("JWT_REFRESH_TTL", getEnv("JWT_REFRESH_TTL", ""), defaultRefreshTTL)
	if err != nil {
		return nil, fmt.Errorf("config.LoadJWTConfig: %w", err)
	}

	if refreshTTL < accessTTL {
		return nil, fmt.Errorf("config.LoadJWTConfig: JWT_REFRESH_TTL (%s) must be >= JWT_ACCESS_TTL (%s)", refreshTTL, accessTTL)
	}

	return &JWTConfig{
		JWTSecret:  secret,
		AccessTTL:  accessTTL,
		RefreshTTL: refreshTTL,
	}, nil
}

// validateJWTSecret rejects empty, placeholder, and too-short secrets.
func validateJWTSecret(raw string) (string, error) {
	secret := strings.TrimSpace(raw)
	if secret == "" {
		return "", fmt.Errorf("JWT_SECRET is required but empty")
	}
	if _, isPlaceholder := placeholderSecrets[strings.ToLower(secret)]; isPlaceholder {
		return "", fmt.Errorf("JWT_SECRET is set to a placeholder value (%q); provide a real secret", secret)
	}
	if len(secret) < minSecretLength {
		return "", fmt.Errorf("JWT_SECRET must be at least %d characters, got %d", minSecretLength, len(secret))
	}
	return secret, nil
}

// parsePositiveTTL parses a duration env value, falling back to def when empty,
// and rejects values that do not parse or are not strictly positive.
func parsePositiveTTL(name, raw string, def time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return def, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s is not a valid duration (%q): %w", name, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be > 0, got %s", name, d)
	}
	return d, nil
}
