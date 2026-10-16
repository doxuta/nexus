package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// testDSN is a syntactically valid DSN for tests that exercise the OTHER config knobs; nothing
// here opens a connection.
const testDSN = "u:p@tcp(127.0.0.1:3306)/nexus_test?charset=utf8mb4&parseTime=True&loc=UTC"

// loadConfig is LoadConfig for tests about the other knobs: DATABASE_URL is required, so it is
// supplied here when the test did not set one, and any error is fatal.
func loadConfig(t *testing.T) *Config {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Setenv("DATABASE_URL", testDSN)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

// TestLoadConfig_DBPoolDefaults verifies that when the DB pool env vars are unset,
// LoadConfig applies the documented safe defaults (RM-20): 25 open / 10 idle / 5 min.
func TestLoadConfig_DBPoolDefaults(t *testing.T) {
	// Ensure vars are unset for this test.
	t.Setenv("DB_MAX_OPEN_CONNS", "")
	t.Setenv("DB_MAX_IDLE_CONNS", "")
	t.Setenv("DB_CONN_MAX_LIFETIME_MIN", "")

	cfg := loadConfig(t)

	if cfg.DBMaxOpenConns != defaultDBMaxOpenConns {
		t.Errorf("DBMaxOpenConns = %d, want %d", cfg.DBMaxOpenConns, defaultDBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != defaultDBMaxIdleConns {
		t.Errorf("DBMaxIdleConns = %d, want %d", cfg.DBMaxIdleConns, defaultDBMaxIdleConns)
	}
	wantLifetime := time.Duration(defaultDBConnMaxLifetimeMin) * time.Minute
	if cfg.DBConnMaxLifetime != wantLifetime {
		t.Errorf("DBConnMaxLifetime = %v, want %v", cfg.DBConnMaxLifetime, wantLifetime)
	}
}

// TestLoadConfig_DBPoolFromEnv verifies that the env vars override the defaults.
func TestLoadConfig_DBPoolFromEnv(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "50")
	t.Setenv("DB_MAX_IDLE_CONNS", "20")
	t.Setenv("DB_CONN_MAX_LIFETIME_MIN", "10")

	cfg := loadConfig(t)

	if cfg.DBMaxOpenConns != 50 {
		t.Errorf("DBMaxOpenConns = %d, want 50", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 20 {
		t.Errorf("DBMaxIdleConns = %d, want 20", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetime != 10*time.Minute {
		t.Errorf("DBConnMaxLifetime = %v, want 10m", cfg.DBConnMaxLifetime)
	}
}

// TestLoadConfig_DBPoolInvalidEnvFallsToDefault verifies that invalid (non-positive or
// non-integer) env values fall back to the safe default rather than zeroing the pool,
// which would deadlock the application under its own concurrency (RM-20 risk note).
func TestLoadConfig_DBPoolInvalidEnvFallsToDefault(t *testing.T) {
	cases := []struct {
		name    string
		openEnv string
		idleEnv string
	}{
		{"zero", "0", "0"},
		{"negative", "-5", "-1"},
		{"string", "abc", "xyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DB_MAX_OPEN_CONNS", tc.openEnv)
			t.Setenv("DB_MAX_IDLE_CONNS", tc.idleEnv)
			t.Setenv("DB_CONN_MAX_LIFETIME_MIN", "")

			cfg := loadConfig(t)

			if cfg.DBMaxOpenConns != defaultDBMaxOpenConns {
				t.Errorf("DBMaxOpenConns = %d, want default %d for env %q", cfg.DBMaxOpenConns, defaultDBMaxOpenConns, tc.openEnv)
			}
			if cfg.DBMaxIdleConns != defaultDBMaxIdleConns {
				t.Errorf("DBMaxIdleConns = %d, want default %d for env %q", cfg.DBMaxIdleConns, defaultDBMaxIdleConns, tc.idleEnv)
			}
		})
	}
}

// TestLoadConfig_BodySizeDefaults verifies RM-16: the default body limit is 1 MiB.
func TestLoadConfig_BodySizeDefaults(t *testing.T) {
	t.Setenv("MAX_REQUEST_BODY_BYTES", "")

	cfg := loadConfig(t)

	const wantBytes = int64(1 << 20) // 1 MiB
	if cfg.MaxRequestBodyBytes != wantBytes {
		t.Errorf("MaxRequestBodyBytes = %d, want %d", cfg.MaxRequestBodyBytes, wantBytes)
	}
}

// TestLoadConfig_BodySizeFromEnv verifies that the env var overrides the default.
func TestLoadConfig_BodySizeFromEnv(t *testing.T) {
	t.Setenv("MAX_REQUEST_BODY_BYTES", "4194304") // 4 MiB

	cfg := loadConfig(t)

	if cfg.MaxRequestBodyBytes != 4194304 {
		t.Errorf("MaxRequestBodyBytes = %d, want 4194304", cfg.MaxRequestBodyBytes)
	}
}

// TestLoadConfig_AuthRateLimitDefaults verifies RM-15: the default is 10 attempts per 60 s.
func TestLoadConfig_AuthRateLimitDefaults(t *testing.T) {
	t.Setenv("AUTH_RATELIMIT_MAX_ATTEMPTS", "")
	t.Setenv("AUTH_RATELIMIT_WINDOW_SEC", "")

	cfg := loadConfig(t)

	if cfg.AuthRateLimitMaxAttempts != defaultAuthRateLimitMaxAttempts {
		t.Errorf("AuthRateLimitMaxAttempts = %d, want %d", cfg.AuthRateLimitMaxAttempts, defaultAuthRateLimitMaxAttempts)
	}
	wantWindow := time.Duration(defaultAuthRateLimitWindowSec) * time.Second
	if cfg.AuthRateLimitWindow != wantWindow {
		t.Errorf("AuthRateLimitWindow = %v, want %v", cfg.AuthRateLimitWindow, wantWindow)
	}
}

// TestLoadConfig_SSORateLimitDefaults verifies SSO-RATELIMIT-NAT: the dedicated SSO bucket
// defaults to 60 attempts per 60 s — deliberately LARGER than the auth default (10/60 s),
// sized for a NAT'd workforce login burst rather than credential guessing.
func TestLoadConfig_SSORateLimitDefaults(t *testing.T) {
	t.Setenv("SSO_RATELIMIT_MAX_ATTEMPTS", "")
	t.Setenv("SSO_RATELIMIT_WINDOW_SEC", "")

	cfg := loadConfig(t)

	if cfg.SSORateLimitMaxAttempts != defaultSSORateLimitMaxAttempts {
		t.Errorf("SSORateLimitMaxAttempts = %d, want %d", cfg.SSORateLimitMaxAttempts, defaultSSORateLimitMaxAttempts)
	}
	wantWindow := time.Duration(defaultSSORateLimitWindowSec) * time.Second
	if cfg.SSORateLimitWindow != wantWindow {
		t.Errorf("SSORateLimitWindow = %v, want %v", cfg.SSORateLimitWindow, wantWindow)
	}
	if cfg.SSORateLimitMaxAttempts <= cfg.AuthRateLimitMaxAttempts {
		t.Errorf("SSO default budget (%d) must exceed the auth default (%d) — the slice's premise",
			cfg.SSORateLimitMaxAttempts, cfg.AuthRateLimitMaxAttempts)
	}
}

// TestLoadConfig_SSORateLimitFromEnv verifies the env overrides land.
func TestLoadConfig_SSORateLimitFromEnv(t *testing.T) {
	t.Setenv("SSO_RATELIMIT_MAX_ATTEMPTS", "120")
	t.Setenv("SSO_RATELIMIT_WINDOW_SEC", "30")

	cfg := loadConfig(t)

	if cfg.SSORateLimitMaxAttempts != 120 {
		t.Errorf("SSORateLimitMaxAttempts = %d, want 120", cfg.SSORateLimitMaxAttempts)
	}
	if cfg.SSORateLimitWindow != 30*time.Second {
		t.Errorf("SSORateLimitWindow = %v, want 30s", cfg.SSORateLimitWindow)
	}
}

// TestLoadConfig_DataRateLimitPerMin (DAILY-QUEUE #52): the per-API-key data-plane budget is
// generous by default, tunable per deployment, and — like every getEnvInt knob — cannot be
// DISABLED from the env (0 / negative / garbage fall back to the default, never "unlimited").
func TestLoadConfig_DataRateLimitPerMin(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultDataRateLimitPerMin},
		{"5", 5},
		{"1200", 1200},
		{"0", defaultDataRateLimitPerMin},
		{"-3", defaultDataRateLimitPerMin},
		{"lots", defaultDataRateLimitPerMin},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("DATA_RATE_LIMIT_PER_MIN", tc.env)
			if got := loadConfig(t).DataRateLimitPerMin; got != tc.want {
				t.Errorf("DataRateLimitPerMin = %d, want %d", got, tc.want)
			}
		})
	}
	if defaultDataRateLimitPerMin < 300 {
		t.Errorf("default %d/min is not generous — an honest integration must never trip it", defaultDataRateLimitPerMin)
	}
}

// TestLoadConfig_DataMaxInFlightPerKey (#52 follow-up): how many requests one API key may have
// running at once. Same getEnvInt contract (tunable, never disabled), and the default must leave
// most of a DB pool to everyone else — one key must never be able to hold the whole pool.
func TestLoadConfig_DataMaxInFlightPerKey(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultDataMaxInFlightPerKey},
		{"2", 2},
		{"16", 16},
		{"0", defaultDataMaxInFlightPerKey},
		{"-1", defaultDataMaxInFlightPerKey},
		{"many", defaultDataMaxInFlightPerKey},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("DATA_MAX_INFLIGHT_PER_KEY", tc.env)
			if got := loadConfig(t).DataMaxInFlightPerKey; got != tc.want {
				t.Errorf("DataMaxInFlightPerKey = %d, want %d", got, tc.want)
			}
		})
	}
	if defaultDataMaxInFlightPerKey*2 > defaultDBMaxOpenConns {
		t.Errorf("default %d in flight per key is over half the default pool (%d) — one key could starve the rest",
			defaultDataMaxInFlightPerKey, defaultDBMaxOpenConns)
	}
}

// TestLoadConfig_APIKeyProbeMaxPerMin (#356): the per-IP budget of failed API-key lookups. Same
// getEnvInt contract: tunable, never disabled.
func TestLoadConfig_APIKeyProbeMaxPerMin(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", defaultAPIKeyProbeMaxPerMin},
		{"5", 5},
		{"0", defaultAPIKeyProbeMaxPerMin},
		{"-3", defaultAPIKeyProbeMaxPerMin},
		{"lots", defaultAPIKeyProbeMaxPerMin},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("APIKEY_PROBE_MAX_PER_MIN", tc.env)
			if got := loadConfig(t).APIKeyProbeMaxPerMin; got != tc.want {
				t.Errorf("APIKeyProbeMaxPerMin = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLoadConfig_ServerTimeoutDefaults verifies RM-16: safe HTTP server timeout defaults.
func TestLoadConfig_ServerTimeoutDefaults(t *testing.T) {
	t.Setenv("HTTP_READ_HEADER_TIMEOUT_SEC", "")
	t.Setenv("HTTP_READ_TIMEOUT_SEC", "")
	t.Setenv("HTTP_WRITE_TIMEOUT_SEC", "")
	t.Setenv("HTTP_IDLE_TIMEOUT_SEC", "")

	cfg := loadConfig(t)

	if cfg.ReadHeaderTimeout != time.Duration(defaultReadHeaderTimeoutSec)*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want %v", cfg.ReadHeaderTimeout, time.Duration(defaultReadHeaderTimeoutSec)*time.Second)
	}
	if cfg.ReadTimeout != time.Duration(defaultReadTimeoutSec)*time.Second {
		t.Errorf("ReadTimeout = %v, want %v", cfg.ReadTimeout, time.Duration(defaultReadTimeoutSec)*time.Second)
	}
	if cfg.WriteTimeout != time.Duration(defaultWriteTimeoutSec)*time.Second {
		t.Errorf("WriteTimeout = %v, want %v", cfg.WriteTimeout, time.Duration(defaultWriteTimeoutSec)*time.Second)
	}
	if cfg.IdleTimeout != time.Duration(defaultIdleTimeoutSec)*time.Second {
		t.Errorf("IdleTimeout = %v, want %v", cfg.IdleTimeout, time.Duration(defaultIdleTimeoutSec)*time.Second)
	}
}

// TestLoadConfig_DatabaseURLRequired (DAILY-QUEUE #100): an empty DATABASE_URL used to be
// replaced, in EVERY environment, by root-without-password on 127.0.0.1 — a go-live box that
// forgot the variable booted as MySQL root instead of stopping. It must fail fast, like JWT_SECRET.
func TestLoadConfig_DatabaseURLRequired(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		wantErr bool
	}{
		{"empty", "", true},
		{"whitespace only", "  \t ", true},
		{"set", testDSN, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", tc.env)
			cfg, err := LoadConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoadConfig with DATABASE_URL=%q: want error, got DSN %q", tc.env, cfg.DBDSN)
				}
				if !strings.Contains(err.Error(), "DATABASE_URL") {
					t.Errorf("error %q does not name DATABASE_URL", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if !strings.HasPrefix(cfg.DBDSN, "u:p@tcp(127.0.0.1:3306)/nexus_test?") {
				t.Errorf("DBDSN = %q, want the env DSN (plus timeouts)", cfg.DBDSN)
			}
		})
	}
}
