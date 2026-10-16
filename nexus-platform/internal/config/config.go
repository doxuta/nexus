package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
)

// DB connection-pool defaults (RM-20).
// Total open conns across BOTH pools (GORM + sqlx) is 2×DBMaxOpenConns = 50, well under
// MySQL's default max_connections=151. Adjust with env vars when tuning for higher load.
const (
	defaultDBMaxOpenConns       = 25
	defaultDBMaxIdleConns       = 10
	defaultDBConnMaxLifetimeMin = 5
)

// DB statement / connection timeout defaults (W5-E2, ops resilience).
// Repro workflow#28: a slow/hung MySQL stalls the API (no statement timeout + a finite
// pool ⇒ pile-up cascade) instead of failing fast. These defaults bound how long any single
// query may read/write on the wire and how long the driver waits to establish a connection,
// so a slow query fails fast with a clean DB_TIMEOUT → 503 rather than hanging a request.
//   - DB_QUERY_TIMEOUT_SEC      → DSN readTimeout/writeTimeout (per-query wire deadline).
//   - DB_CONN_ACQUIRE_TIMEOUT_SEC → DSN timeout (dial/connect deadline).
const (
	defaultDBQueryTimeoutSec       = 10
	defaultDBConnAcquireTimeoutSec = 5
)

// Request body-size limit defaults (RM-16).
// 1 MiB is sufficient for the richest JSON payloads in the current API surface;
// raise via MAX_REQUEST_BODY_BYTES when composite/rich_text fields grow.
const defaultMaxRequestBodyBytes = 1 << 20 // 1 MiB

// HTTP server timeout defaults (RM-16).
const (
	defaultReadHeaderTimeoutSec = 5
	defaultReadTimeoutSec       = 15
	defaultWriteTimeoutSec      = 30
	defaultIdleTimeoutSec       = 120
)

// Auth rate-limit defaults (RM-15).
// A sliding window of AuthRateLimitMaxAttempts per AuthRateLimitWindowSec seconds,
// keyed per (tenant+email+IP). Chosen conservatively: 10 attempts per 60 s allows
// a legitimate user to mis-type several times without lockout while still throttling
// a rapid brute-force script (bcrypt cost-12 ≈ 100-300 ms/attempt → 10 guesses in
// the window is already expensive).
const (
	defaultAuthRateLimitMaxAttempts = 10
	defaultAuthRateLimitWindowSec   = 60
	// defaultAuthRateLimitIPMaxAttempts is the coarser per-IP backstop (W5-A3): it caps total
	// /auth attempts per source IP per window regardless of email, throttling a password-spray
	// that varies the email to dodge the per-(ip,email) window. Set well above a busy office's
	// legitimate burst (NAT/corporate egress share one IP) so real users are not locked out —
	// 30/60 s = 3× the per-email cap. Set AUTH_RATELIMIT_IP_MAX_ATTEMPTS=0 to disable.
	defaultAuthRateLimitIPMaxAttempts = 30
)

// Persistent account-lockout defaults (prod-ops slice b, mig 000043). After MaxFailures
// consecutive wrong passwords on an existing account, login is refused (generic 401,
// anti-enumeration) for DurationMin minutes. Mirrors services.defaultLockout* so an unset
// env and a bare NewAuthService behave identically. getEnvInt treats 0/negative as the
// default — the env can TUNE the lockout but never disable it (consistent with the AI_* knobs).
const (
	defaultAuthLockoutMaxFailures = 10
	defaultAuthLockoutDurationMin = 15
)

// SSO rate-limit defaults (SSO-RATELIMIT-NAT).
// The public SSO endpoints (/auth/sso/:tenant/start, /auth/sso/callback) get a DEDICATED
// sliding window, sized for a workforce login burst rather than for credential guessing:
// an enterprise behind NAT funnels every employee through one egress IP, and SSO logins
// carry no password to brute-force — throttling them with the strict 10/60 s /auth bucket
// locked whole companies out of login (password login included, since the budget was
// shared). 60/60 s = 6× the auth default; keying is per (ip, tenant, provider) on /start
// and per ip on /callback (see middleware.SSORateLimit). Env-tunable per deployment.
const (
	defaultSSORateLimitMaxAttempts = 60
	defaultSSORateLimitWindowSec   = 60
)

// Data-plane API-key throttle default (DAILY-QUEUE #52, ../specs/045-api-key-throttle/spec.md).
// Requests per API key per 60 s sliding window, on every protected route. Deliberately generous
// (10 req/s sustained — the same order as common SaaS per-app burst limits) so an honest
// integration never trips it; it exists to stop a retry storm, not to meter usage. Browser (JWT)
// sessions are never counted. getEnvInt semantics: the env can TUNE it, never disable it.
const defaultDataRateLimitPerMin = 600

// Requests one API key may have RUNNING at the same moment (#52 follow-up). The per-minute budget
// alone admits a whole window at once, so it does not stop one key from holding every pooled DB
// connection; this does. 8 of a 25-connection pool leaves the rest to every other caller. Keep it
// well under DB_MAX_OPEN_CONNS if that is lowered. getEnvInt semantics: tunable, never disabled.
const defaultDataMaxInFlightPerKey = 8

// Failed `nxk_` lookups one client IP may cause per minute before it is refused with 429 without
// the sys_api_keys read (queue #356, specs/045 FR-9). Only lookups that READ the table and find
// nothing usable count — a repeated dead string is answered from memory and costs nothing — so an
// honest integration with a typo'd or rotated key never gets near it; a prober changing the string
// every request does within a second. Keys that already verified live are exempt. getEnvInt
// semantics: tunable, never disabled.
const defaultAPIKeyProbeMaxPerMin = 30

// Config holds the application-wide runtime configuration loaded from environment
// variables. Safe defaults are applied for all optional fields. DATABASE_URL is required
// (LoadConfig fails fast when it is empty); JWT_SECRET etc. are fail-fast guarded in their
// own loaders.
type Config struct {
	ServerPort string
	DBDSN      string
	// CORSAllowedOrigins is a comma-separated list of allowed origins for CORS.
	// An empty value means CORS is fail-closed (no origin is allowed).
	// Example: "https://app.nexus.io,https://admin.nexus.io"
	CORSAllowedOrigins []string

	// DB connection-pool settings (RM-20). Applied to both the GORM pool (sys_* tables)
	// and the sqlx pool (obj_* dynamic records) after each Open call.
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration

	// DB statement / connection timeouts (W5-E2). DBQueryTimeout is the per-query wire
	// deadline (DSN readTimeout/writeTimeout): a query whose read/write blocks longer than
	// this is aborted by the driver with an i/o-timeout net error, which the handler layer
	// maps to a clean 503 DB_TIMEOUT instead of letting the request hang. DBConnAcquireTimeout
	// is the dial/connect deadline (DSN timeout). Both are injected into the DSN by
	// ApplyDBTimeouts before any pool is opened. A value ≤ 0 disables that knob.
	DBQueryTimeout       time.Duration
	DBConnAcquireTimeout time.Duration

	// HTTP request body-size cap (RM-16). Requests whose body exceeds this limit are
	// rejected with 413 before the JSON decoder runs, preventing OOM on huge payloads.
	MaxRequestBodyBytes int64

	// HTTP server timeout settings (RM-16). Set on the net/http.Server alongside the
	// existing graceful-shutdown logic so the server never hangs on slow/idle clients.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration

	// Auth rate-limiter settings (RM-15). Applied to the /auth/* public group.
	AuthRateLimitMaxAttempts int
	// AuthRateLimitIPMaxAttempts is the coarser per-IP backstop window (W5-A3 password-spray).
	AuthRateLimitIPMaxAttempts int
	AuthRateLimitWindow        time.Duration

	// Persistent account-lockout settings (AUTH_LOCKOUT_MAX_FAILURES /
	// AUTH_LOCKOUT_DURATION_MIN) — wired into AuthService.WithLockoutPolicy.
	AuthLockoutMaxFailures int
	AuthLockoutDuration    time.Duration

	// SSO rate-limiter settings (SSO-RATELIMIT-NAT). Applied to the public SSO endpoints
	// (/auth/sso/:tenant/start, /auth/sso/callback) via their DEDICATED limiter — separate
	// from (and larger than) the /auth credential bucket above, so a NAT'd enterprise's SSO
	// login burst never exhausts the password-login budget.
	SSORateLimitMaxAttempts int
	SSORateLimitWindow      time.Duration

	// DataRateLimitPerMin is the per-API-key request budget per 60 s sliding window on the
	// protected data plane (DATA_RATE_LIMIT_PER_MIN, #52). Only `nxk_` principals are counted.
	DataRateLimitPerMin int
	// DataMaxInFlightPerKey caps concurrent requests per API key on the same data plane
	// (DATA_MAX_INFLIGHT_PER_KEY); a request over it gets 429 + Retry-After: 1.
	DataMaxInFlightPerKey int
	// APIKeyProbeMaxPerMin is the per-client-IP budget of failed API-key lookups per minute
	// (APIKEY_PROBE_MAX_PER_MIN, #356); past it a never-seen-live `nxk_` string from that IP is 429.
	APIKeyProbeMaxPerMin int

	// MetricsBearerToken gates GET /metrics (RM-38, OBS-1). Empty (default) = the endpoint
	// answers 404 (fail-closed OFF); set = the scraper must send `Authorization: Bearer <token>`.
	MetricsBearerToken string

	// TrustedProxyCIDRs are the reverse-proxy networks (parsed from TRUSTED_PROXY_CIDRS) whose
	// X-Forwarded-For may be trusted for client-IP extraction (HIGH-2/MED-3). Wired into the
	// auth/SSO limiters (per-IP keying) AND gin's SetTrustedProxies (access-log client_ip).
	// Empty (the default) → XFF is never trusted; the direct peer address is used. OPT-IN.
	TrustedProxyCIDRs []*net.IPNet
	// TrustedProxyCIDRsSkipped holds the raw TRUSTED_PROXY_CIDRS entries that failed to parse.
	// They are dropped (degrade-to-default — a bad CIDR never fails boot) and surfaced here so
	// main can log a warning at startup, mirroring the other tolerant config knobs.
	TrustedProxyCIDRsSkipped []string
}

func LoadConfig() (*Config, error) {
	port := getEnv("PORT", "8080")

	// Required, fail-fast like JWT_SECRET (DAILY-QUEUE #100). There used to be a silent fallback
	// to root-without-password on 127.0.0.1 in EVERY environment, so a go-live box that forgot the
	// variable booted as MySQL root instead of stopping. The dev path loads nexus-platform/.env.
	// Prefer loc=UTC in the DSN: the DB stores timestamps in UTC, and loc=Local on a +07 host
	// mislabels them → instants 7h off (R2-03).
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		return nil, fmt.Errorf("config.LoadConfig: DATABASE_URL is required but empty (dev: load nexus-platform/.env, e.g. `set -a; . ./.env; set +a`)")
	}

	var corsOrigins []string
	if raw := getEnv("CORS_ALLOWED_ORIGINS", ""); raw != "" {
		for _, o := range strings.Split(raw, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				corsOrigins = append(corsOrigins, o)
			}
		}
	}

	dbMaxOpen := getEnvInt("DB_MAX_OPEN_CONNS", defaultDBMaxOpenConns)
	dbMaxIdle := getEnvInt("DB_MAX_IDLE_CONNS", defaultDBMaxIdleConns)
	dbLifetimeMin := getEnvInt("DB_CONN_MAX_LIFETIME_MIN", defaultDBConnMaxLifetimeMin)

	// W5-E2: per-query wire deadline + dial deadline. Injected into the DSN below so BOTH
	// the GORM (sys_*) and sqlx (obj_*) pools pick them up — a slow query then fails fast
	// (i/o timeout) instead of hanging the request. The same call sets clientFoundRows (#91).
	dbQueryTimeout := time.Duration(getEnvInt("DB_QUERY_TIMEOUT_SEC", defaultDBQueryTimeoutSec)) * time.Second
	dbConnAcquireTimeout := time.Duration(getEnvInt("DB_CONN_ACQUIRE_TIMEOUT_SEC", defaultDBConnAcquireTimeoutSec)) * time.Second
	dsn = ApplyDBTimeouts(dsn, dbQueryTimeout, dbConnAcquireTimeout)

	maxBody := getEnvInt64("MAX_REQUEST_BODY_BYTES", defaultMaxRequestBodyBytes)

	readHeaderSec := getEnvInt("HTTP_READ_HEADER_TIMEOUT_SEC", defaultReadHeaderTimeoutSec)
	readSec := getEnvInt("HTTP_READ_TIMEOUT_SEC", defaultReadTimeoutSec)
	writeSec := getEnvInt("HTTP_WRITE_TIMEOUT_SEC", defaultWriteTimeoutSec)
	idleSec := getEnvInt("HTTP_IDLE_TIMEOUT_SEC", defaultIdleTimeoutSec)

	rlMaxAttempts := getEnvInt("AUTH_RATELIMIT_MAX_ATTEMPTS", defaultAuthRateLimitMaxAttempts)
	rlIPMaxAttempts := getEnvInt("AUTH_RATELIMIT_IP_MAX_ATTEMPTS", defaultAuthRateLimitIPMaxAttempts)
	rlWindowSec := getEnvInt("AUTH_RATELIMIT_WINDOW_SEC", defaultAuthRateLimitWindowSec)

	lockoutMaxFailures := getEnvInt("AUTH_LOCKOUT_MAX_FAILURES", defaultAuthLockoutMaxFailures)
	lockoutDurationMin := getEnvInt("AUTH_LOCKOUT_DURATION_MIN", defaultAuthLockoutDurationMin)

	ssoRLMaxAttempts := getEnvInt("SSO_RATELIMIT_MAX_ATTEMPTS", defaultSSORateLimitMaxAttempts)
	ssoRLWindowSec := getEnvInt("SSO_RATELIMIT_WINDOW_SEC", defaultSSORateLimitWindowSec)

	trustedProxyCIDRs, trustedProxySkipped := ParseTrustedProxyCIDRs(getEnv("TRUSTED_PROXY_CIDRS", ""))

	return &Config{
		ServerPort:         port,
		DBDSN:              dsn,
		CORSAllowedOrigins: corsOrigins,

		DBMaxOpenConns:    dbMaxOpen,
		DBMaxIdleConns:    dbMaxIdle,
		DBConnMaxLifetime: time.Duration(dbLifetimeMin) * time.Minute,

		DBQueryTimeout:       dbQueryTimeout,
		DBConnAcquireTimeout: dbConnAcquireTimeout,

		MaxRequestBodyBytes: maxBody,

		ReadHeaderTimeout: time.Duration(readHeaderSec) * time.Second,
		ReadTimeout:       time.Duration(readSec) * time.Second,
		WriteTimeout:      time.Duration(writeSec) * time.Second,
		IdleTimeout:       time.Duration(idleSec) * time.Second,

		AuthRateLimitMaxAttempts:   rlMaxAttempts,
		AuthRateLimitIPMaxAttempts: rlIPMaxAttempts,
		AuthRateLimitWindow:        time.Duration(rlWindowSec) * time.Second,

		AuthLockoutMaxFailures: lockoutMaxFailures,
		AuthLockoutDuration:    time.Duration(lockoutDurationMin) * time.Minute,

		SSORateLimitMaxAttempts: ssoRLMaxAttempts,
		SSORateLimitWindow:      time.Duration(ssoRLWindowSec) * time.Second,

		DataRateLimitPerMin:   getEnvInt("DATA_RATE_LIMIT_PER_MIN", defaultDataRateLimitPerMin),
		DataMaxInFlightPerKey: getEnvInt("DATA_MAX_INFLIGHT_PER_KEY", defaultDataMaxInFlightPerKey),
		APIKeyProbeMaxPerMin:  getEnvInt("APIKEY_PROBE_MAX_PER_MIN", defaultAPIKeyProbeMaxPerMin),

		MetricsBearerToken: getEnv("METRICS_BEARER_TOKEN", ""),

		TrustedProxyCIDRs:        trustedProxyCIDRs,
		TrustedProxyCIDRsSkipped: trustedProxySkipped,
	}, nil
}

// ParseTrustedProxyCIDRs parses a comma-separated TRUSTED_PROXY_CIDRS value into *net.IPNet
// blocks. Each entry may be a CIDR ("172.16.0.0/12") or a bare IP ("10.0.0.1", normalised to a
// /32 or /128). Whitespace and empty entries are ignored. An entry that parses as neither is
// SKIPPED and returned in `skipped` (the caller logs it) — one bad CIDR degrades to default
// (that entry ignored, XFF not trusted from it) and never fails boot, matching the other tolerant
// config knobs. Empty input → nil (no trusted proxy; XFF is never trusted — the safe default).
func ParseTrustedProxyCIDRs(raw string) (nets []*net.IPNet, skipped []string) {
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, ipnet, err := net.ParseCIDR(part); err == nil {
			// Reject a /0 ("0.0.0.0/0", "::/0"): it would trust X-Forwarded-For from EVERY
			// direct peer, defeating the spoof guard entirely (any client could forge its own
			// limiter key). There is no legitimate reason to trust the whole address space —
			// treat it as a misconfiguration: skip it (XFF not trusted from it) and surface the
			// warning via `skipped`, exactly like an unparseable entry (fail-safe).
			if ones, _ := ipnet.Mask.Size(); ones == 0 {
				skipped = append(skipped, part)
				continue
			}
			nets = append(nets, ipnet)
			continue
		}
		if ip := net.ParseIP(part); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		skipped = append(skipped, part)
	}
	return nets, skipped
}

// getEnv returns the value of the environment variable named by key, or def when
// the variable is unset or empty.
func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// getEnvInt returns the int value of the env variable named by key, or def when the
// variable is unset, empty, or not a valid positive integer. Negative/zero values from
// the env are treated as the default to prevent misconfigured limits from opening
// unbounded or zeroed pools.
func getEnvInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// getEnvInt64 is like getEnvInt but returns int64, used for byte-count limits.
func getEnvInt64(key string, def int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// ApplyDBTimeouts injects the W5-E2 statement/connection timeouts into a go-sql-driver DSN
// (the format gorm + sqlx both consume): queryTimeout → ReadTimeout + WriteTimeout (the
// per-query wire deadline so a slow query fails fast with an i/o-timeout net error instead
// of hanging), connAcquireTimeout → Timeout (the dial/connect deadline). A non-positive
// duration leaves that knob untouched. The DSN is parsed and re-formatted with the driver's
// own parser so existing params (charset, parseTime, loc, …) are preserved and we never
// hand-splice the query string. If the DSN cannot be parsed (e.g. a malformed test value),
// the original is returned unchanged so a bad timeout config never bricks boot — the pool
// limits and HTTP timeouts still apply.
//
// It also forces clientFoundRows (queue #91): RowsAffected then counts MATCHED rows, not
// CHANGED rows. Repositories read `RowsAffected()==0` as "no such row"; under MySQL's default
// an UPDATE that re-writes identical values reports 0, so a second identical "Lưu" came back
// 404. The readers that mean "did THIS call win" (claims, consume-once tokens, mark-read, lease
// finishes) all exclude the target state in their WHERE, so matched == changed for them — keep
// it that way when adding one (specs/057-found-rows-dsn).
func ApplyDBTimeouts(dsn string, queryTimeout, connAcquireTimeout time.Duration) string {
	cfg, err := gomysql.ParseDSN(dsn)
	if err != nil {
		return dsn
	}
	cfg.ClientFoundRows = true
	if queryTimeout > 0 {
		cfg.ReadTimeout = queryTimeout
		cfg.WriteTimeout = queryTimeout
	}
	if connAcquireTimeout > 0 {
		cfg.Timeout = connAcquireTimeout
	}
	return cfg.FormatDSN()
}
