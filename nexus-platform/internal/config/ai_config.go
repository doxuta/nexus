package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// AI_GATEWAY_MODE values. live calls the gateway; record does too and appends every exchange to the
// cassette; replay answers from the cassette alone, with no network, base URL or key.
const (
	AIGatewayLive   = "live"
	AIGatewayRecord = "record"
	AIGatewayReplay = "replay"
)

// AI-Builder (E9) gateway defaults: an OpenAI-compatible chat-completions gateway (LiteLLM, vLLM,
// Ollama, a hosted OpenAI-compatible endpoint …) pinned to
// localhost (optional bearer key via AI_GATEWAY_API_KEY). The base URL is read from config (NOT a request) so the agent can
// never be steered at a third-party host (no SSRF). max_steps bounds the agentic tool-loop so a
// misbehaving model cannot spin cost/latency unbounded.
const (
	defaultAIGatewayBaseURL = "http://127.0.0.1:4000/v1"
	defaultAIModel          = "claude-opus-5-5"
	defaultAITimeoutSec     = 45
	defaultAIMaxTokens      = 4096
	// defaultAIMaxSteps must clear the most tool-hungry mode, Discovery (M10): one
	// snapshot_model + up to 6 propose_* batches + a few reasoning turns. The original 6
	// truncated Discovery mid-blueprint (E9-DISCOVERY-STEPS); 12 covers it with margin while
	// the per-turn ceiling stays bounded (MaxSteps × MaxTokens) and the M4 daily budget caps
	// total spend. Deployments with longer transcripts can still raise AI_MAX_STEPS.
	defaultAIMaxSteps = 12

	// M4 (MED-1, spec §12) — per-tenant cost gates BETWEEN turns. Per-turn spend is already
	// bounded (MaxSteps × MaxTokens × Timeout); these bound how many turns a tenant can start.
	defaultAIRateLimitMaxTurns  = 10
	defaultAIRateLimitWindowSec = 60
	defaultAIMaxConcurrent      = 2
	defaultAIDailyTokenBudget   = 200000
	// ⌘K assistant is every-user (unlike the admin-only builder), so it gets a PER-USER BURST cap
	// on its share of the per-tenant turn window (defaultAIRateLimitMaxTurns, SHARED by all users
	// of the tenant + the admin builder). The default is deliberately set BELOW that tenant cap so
	// it actually BITES: with the tenant admitting 10 turns/window, a per-user cap of 5 means one
	// user can consume at most HALF the shared burst, reserving the rest for colleagues — a
	// fair-share floor, not an abuse ceiling (a cap ABOVE the tenant's 10 would never fire because
	// the tenant gate trips first). NOTE: this bounds the per-window BURST only, NOT a user's daily
	// token total — that is bounded by defaultAssistantDailyTokenSharePct below.
	defaultAssistantRateLimitPerUser = 5
	// defaultAssistantDailyTokenSharePct closes the "steady user burns the whole tenant budget"
	// gap the burst cap leaves open: one user's ⌘K assistant spend per UTC day is capped at this
	// percentage of the tenant's DailyTokenBudget (sys_ai_runs, per-user sum). Default 50 BITES —
	// one user can consume at most half the tenant's day, reserving the rest for colleagues.
	// 100 = the whole budget (effectively opting the per-user gate out); the env knob degrades to
	// this default outside 1..100 so a typo can never silently widen a cost gate.
	defaultAssistantDailyTokenSharePct = 50
)

// AIConfig holds the E9 AI-Builder runtime configuration read from the environment.
//
// Like MFAConfig, AI is OFF by default: AI_ENABLED must be explicitly "true" to enable the
// conversational builder. When Enabled is false the route still mounts but every call answers 503
// AI_DISABLED (honest-disabled — the manual builder is always the source of truth; the AI is an
// accelerator that can be turned off without losing any P0 capability). BaseURL is always a
// localhost-pinned config value, never a client-supplied URL.
type AIConfig struct {
	Enabled    bool
	BaseURL    string
	APIKey     string // AI_GATEWAY_API_KEY: bearer token for the gateway; never logged
	Model      string
	TimeoutSec int
	MaxTokens  int
	MaxSteps   int

	// GatewayMode is AI_GATEWAY_MODE (AIGatewayLive, the default, | AIGatewayRecord | AIGatewayReplay);
	// Cassette is AI_GATEWAY_CASSETTE, the JSON-lines file record writes and replay reads.
	GatewayMode string
	Cassette    string

	// M4 (MED-1) budget/rate/concurrency knobs, all applied PER TENANT by ai.BudgetGuard
	// (the tenant is the unit of cost, and the caller is an already-authenticated admin — so
	// keying is by tenant, not by IP like the /auth/* limiter).
	//
	// RateLimitMaxTurns chat turns are admitted per RateLimitWindowSec seconds (sliding window).
	RateLimitMaxTurns  int
	RateLimitWindowSec int
	// MaxConcurrent is the maximum number of in-flight chat turns per tenant.
	MaxConcurrent int
	// DailyTokenBudget caps SUM(prompt_tokens+output_tokens) per tenant per UTC day, read from
	// the sys_ai_runs ledger. 0 disables the budget check (EXPLICIT opt-out — the default is
	// always bounded; a negative or unparsable env value degrades to the default, not to 0).
	DailyTokenBudget int64

	// AssistantRateLimitPerUser caps /assistant/ask requests per (tenant,user) per
	// RateLimitWindowSec — a PER-USER BURST gate in front of the per-tenant budget so one user can't
	// monopolize the tenant's SHARED turn window (the ⌘K assistant is every-user, unlike the
	// admin-only builder). Bounds burst share only, NOT a user's daily token total.
	AssistantRateLimitPerUser int

	// AssistantUserDailySharePct caps ONE user's assistant token spend per UTC day at this
	// percentage of DailyTokenBudget (enforced by BudgetGuard.AcquireUser on the /assistant/ask
	// path only — the admin builder stays tenant-gated). 1..100; env values outside that range
	// degrade to the biting default (50). No effect when DailyTokenBudget is 0 (budget opt-out)
	// or when set to 100 (whole-budget share = the tenant gate already covers it).
	AssistantUserDailySharePct int
}

// LoadAIConfig reads the AI_* environment variables, applying safe defaults so the process boots
// with the AI Builder inert (Enabled=false) when nothing is configured. A bad or absent knob falls
// back to the documented default, mirroring the getEnv* helpers used across the config package. AI
// is an optional accelerator — a misconfigured knob must degrade to honest-disabled, never fail the
// whole server boot. The one exception is the gateway mode: an unknown AI_GATEWAY_MODE, or record/
// replay without AI_GATEWAY_CASSETTE, is an error, because degrading there would quietly run live
// (network, key, cost) where a replay was meant.
func LoadAIConfig() (*AIConfig, error) {
	enabled := strings.EqualFold(strings.TrimSpace(getEnv("AI_ENABLED", "false")), "true")

	rawMode := os.Getenv("AI_GATEWAY_MODE")
	mode := strings.ToLower(strings.TrimSpace(rawMode))
	if mode == "" {
		mode = AIGatewayLive
	}
	cassette := strings.TrimSpace(os.Getenv("AI_GATEWAY_CASSETTE"))
	switch mode {
	case AIGatewayLive:
	case AIGatewayRecord, AIGatewayReplay:
		if cassette == "" {
			return nil, fmt.Errorf("config.LoadAIConfig: AI_GATEWAY_MODE=%s needs AI_GATEWAY_CASSETTE, the path of the cassette file", mode)
		}
	default:
		return nil, fmt.Errorf("config.LoadAIConfig: AI_GATEWAY_MODE=%q: want live, record or replay", rawMode)
	}

	baseURL := strings.TrimSpace(getEnv("AI_GATEWAY_BASE_URL", defaultAIGatewayBaseURL))
	if baseURL == "" {
		baseURL = defaultAIGatewayBaseURL
	}
	// Normalize a trailing slash so callers can always append "/chat/completions" without
	// producing a double slash.
	baseURL = strings.TrimRight(baseURL, "/")

	model := strings.TrimSpace(getEnv("AI_MODEL", defaultAIModel))
	if model == "" {
		model = defaultAIModel
	}

	return &AIConfig{
		Enabled:    enabled,
		BaseURL:    baseURL,
		APIKey:     strings.TrimSpace(os.Getenv("AI_GATEWAY_API_KEY")),
		Model:      model,
		TimeoutSec: getEnvInt("AI_TIMEOUT_SEC", defaultAITimeoutSec),
		MaxTokens:  getEnvInt("AI_MAX_TOKENS", defaultAIMaxTokens),
		MaxSteps:   getEnvInt("AI_MAX_STEPS", defaultAIMaxSteps),

		GatewayMode: mode,
		Cassette:    cassette,

		// getEnvInt folds non-positive values back to the default, so the rate/concurrency
		// gates can never be silently zeroed by a typo'd env var (degrade-to-default).
		RateLimitMaxTurns:         getEnvInt("AI_RATELIMIT_MAX_TURNS", defaultAIRateLimitMaxTurns),
		RateLimitWindowSec:        getEnvInt("AI_RATELIMIT_WINDOW_SEC", defaultAIRateLimitWindowSec),
		MaxConcurrent:             getEnvInt("AI_MAX_CONCURRENT", defaultAIMaxConcurrent),
		AssistantRateLimitPerUser: getEnvInt("ASSISTANT_RATELIMIT_MAX_PER_USER", defaultAssistantRateLimitPerUser),
		// Percent knob: only 1..100 is meaningful; anything else degrades to the default.
		AssistantUserDailySharePct: getEnvPct("ASSISTANT_DAILY_TOKEN_SHARE_PCT", defaultAssistantDailyTokenSharePct),
		// Budget is the one knob where 0 is meaningful (spec §12: explicit opt-out), so it
		// uses a zero-allowing reader instead of getEnvInt64.
		DailyTokenBudget: getEnvInt64ZeroAllowed("AI_DAILY_TOKEN_BUDGET", defaultAIDailyTokenBudget),
	}, nil
}

// getEnvPct reads an integer PERCENT env var: only 1..100 is accepted; unparsable, zero, negative
// or >100 values degrade to the default (never-error, parity with the other AI_* knobs — a typo
// must never silently disable or widen a cost gate; opting the gate out is explicit: set 100).
func getEnvPct(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 100 {
		return def
	}
	return n
}

// getEnvInt64ZeroAllowed reads an int64 env var where 0 is a MEANINGFUL value (explicit opt-out of
// the daily-budget check), unlike getEnvInt64 which folds every non-positive value back to the
// default. Negative or unparsable values still degrade to the default (never-error): a mistyped
// knob must NOT silently disable a cost gate.
func getEnvInt64ZeroAllowed(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return def
	}
	return n
}
