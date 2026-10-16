package config

import (
	"strings"
	"testing"
)

// loadAI is LoadAIConfig for the tests that leave AI_GATEWAY_MODE alone, where it cannot fail.
func loadAI(t *testing.T) *AIConfig {
	t.Helper()
	cfg, err := LoadAIConfig()
	if err != nil {
		t.Fatalf("LoadAIConfig: %v", err)
	}
	return cfg
}

func TestLoadAIConfig_Defaults(t *testing.T) {
	// No AI_* vars set → inert, localhost-pinned defaults.
	t.Setenv("AI_ENABLED", "")
	t.Setenv("AI_GATEWAY_MODE", "")
	t.Setenv("AI_GATEWAY_CASSETTE", "")
	t.Setenv("AI_GATEWAY_BASE_URL", "")
	t.Setenv("AI_MODEL", "")
	t.Setenv("AI_TIMEOUT_SEC", "")
	t.Setenv("AI_MAX_TOKENS", "")
	t.Setenv("AI_MAX_STEPS", "")
	t.Setenv("AI_RATELIMIT_MAX_TURNS", "")
	t.Setenv("AI_RATELIMIT_WINDOW_SEC", "")
	t.Setenv("AI_MAX_CONCURRENT", "")
	t.Setenv("AI_DAILY_TOKEN_BUDGET", "")

	cfg := loadAI(t)
	if cfg.Enabled {
		t.Fatal("Enabled = true; want false by default (AI off unless AI_ENABLED=true)")
	}
	if cfg.BaseURL != defaultAIGatewayBaseURL {
		t.Fatalf("BaseURL = %q; want %q", cfg.BaseURL, defaultAIGatewayBaseURL)
	}
	if cfg.GatewayMode != AIGatewayLive || cfg.Cassette != "" {
		t.Fatalf("GatewayMode/Cassette = %q/%q; want live and no cassette", cfg.GatewayMode, cfg.Cassette)
	}
	if cfg.Model != defaultAIModel {
		t.Fatalf("Model = %q; want %q", cfg.Model, defaultAIModel)
	}
	if cfg.TimeoutSec != defaultAITimeoutSec {
		t.Fatalf("TimeoutSec = %d; want %d", cfg.TimeoutSec, defaultAITimeoutSec)
	}
	if cfg.MaxTokens != defaultAIMaxTokens {
		t.Fatalf("MaxTokens = %d; want %d", cfg.MaxTokens, defaultAIMaxTokens)
	}
	if cfg.MaxSteps != defaultAIMaxSteps {
		t.Fatalf("MaxSteps = %d; want %d", cfg.MaxSteps, defaultAIMaxSteps)
	}
	// M4 (MED-1): the cost gates default BOUNDED — never zero/unlimited out of the box.
	if cfg.RateLimitMaxTurns != defaultAIRateLimitMaxTurns {
		t.Fatalf("RateLimitMaxTurns = %d; want %d", cfg.RateLimitMaxTurns, defaultAIRateLimitMaxTurns)
	}
	if cfg.RateLimitWindowSec != defaultAIRateLimitWindowSec {
		t.Fatalf("RateLimitWindowSec = %d; want %d", cfg.RateLimitWindowSec, defaultAIRateLimitWindowSec)
	}
	if cfg.MaxConcurrent != defaultAIMaxConcurrent {
		t.Fatalf("MaxConcurrent = %d; want %d", cfg.MaxConcurrent, defaultAIMaxConcurrent)
	}
	if cfg.DailyTokenBudget != defaultAIDailyTokenBudget {
		t.Fatalf("DailyTokenBudget = %d; want %d", cfg.DailyTokenBudget, defaultAIDailyTokenBudget)
	}
	if cfg.AssistantUserDailySharePct != defaultAssistantDailyTokenSharePct {
		t.Fatalf("AssistantUserDailySharePct = %d; want %d", cfg.AssistantUserDailySharePct, defaultAssistantDailyTokenSharePct)
	}
}

// TestLoadAIConfig_AssistantDailySharePct: the percent knob accepts 1..100 and degrades to the
// biting default (50) on anything else — it can never be zeroed/disabled through the env (100 is
// the explicit whole-budget opt-out).
func TestLoadAIConfig_AssistantDailySharePct(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{raw: "", want: defaultAssistantDailyTokenSharePct},
		{raw: "25", want: 25},
		{raw: "1", want: 1},
		{raw: "100", want: 100}, // whole budget — explicit opt-out of the user gate
		{raw: "0", want: defaultAssistantDailyTokenSharePct},
		{raw: "-5", want: defaultAssistantDailyTokenSharePct},
		{raw: "101", want: defaultAssistantDailyTokenSharePct},
		{raw: "abc", want: defaultAssistantDailyTokenSharePct},
	}
	for _, tt := range tests {
		t.Run("val_"+tt.raw, func(t *testing.T) {
			t.Setenv("ASSISTANT_DAILY_TOKEN_SHARE_PCT", tt.raw)
			if got := loadAI(t).AssistantUserDailySharePct; got != tt.want {
				t.Fatalf("ASSISTANT_DAILY_TOKEN_SHARE_PCT=%q → %d; want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestLoadAIConfig_EnvOverride(t *testing.T) {
	t.Setenv("AI_ENABLED", "true")
	t.Setenv("AI_GATEWAY_BASE_URL", "http://127.0.0.1:9999/v1/") // trailing slash must be trimmed
	t.Setenv("AI_MODEL", "cc/claude-opus-4-8")
	t.Setenv("AI_TIMEOUT_SEC", "30")
	t.Setenv("AI_MAX_TOKENS", "2048")
	t.Setenv("AI_MAX_STEPS", "10")
	t.Setenv("AI_RATELIMIT_MAX_TURNS", "5")
	t.Setenv("AI_RATELIMIT_WINDOW_SEC", "120")
	t.Setenv("AI_MAX_CONCURRENT", "4")
	t.Setenv("AI_DAILY_TOKEN_BUDGET", "500000")

	cfg := loadAI(t)
	if !cfg.Enabled {
		t.Fatal("Enabled = false; want true when AI_ENABLED=true")
	}
	if cfg.BaseURL != "http://127.0.0.1:9999/v1" {
		t.Fatalf("BaseURL = %q; want the trailing slash trimmed", cfg.BaseURL)
	}
	if cfg.Model != "cc/claude-opus-4-8" {
		t.Fatalf("Model = %q; want the override", cfg.Model)
	}
	if cfg.TimeoutSec != 30 || cfg.MaxTokens != 2048 || cfg.MaxSteps != 10 {
		t.Fatalf("numeric overrides not applied: %+v", cfg)
	}
	if cfg.RateLimitMaxTurns != 5 || cfg.RateLimitWindowSec != 120 || cfg.MaxConcurrent != 4 {
		t.Fatalf("M4 rate/concurrency overrides not applied: %+v", cfg)
	}
	if cfg.DailyTokenBudget != 500000 {
		t.Fatalf("DailyTokenBudget = %d; want 500000", cfg.DailyTokenBudget)
	}
}

// TestLoadAIConfig_M4Knobs_Degradation proves the never-error contract on the M4 knobs: a mistyped
// or negative value falls back to the bounded default (a typo must never zero a cost gate), while
// AI_DAILY_TOKEN_BUDGET=0 is the ONE meaningful zero — an explicit opt-out of the budget check.
func TestLoadAIConfig_M4Knobs_Degradation(t *testing.T) {
	tests := []struct {
		name       string
		env        map[string]string
		wantTurns  int
		wantWindow int
		wantConc   int
		wantBudget int64
	}{
		{
			name: "garbage values degrade to defaults",
			env: map[string]string{
				"AI_RATELIMIT_MAX_TURNS":  "abc",
				"AI_RATELIMIT_WINDOW_SEC": "-1",
				"AI_MAX_CONCURRENT":       "0",
				"AI_DAILY_TOKEN_BUDGET":   "not-a-number",
			},
			wantTurns:  defaultAIRateLimitMaxTurns,
			wantWindow: defaultAIRateLimitWindowSec,
			wantConc:   defaultAIMaxConcurrent,
			wantBudget: defaultAIDailyTokenBudget,
		},
		{
			name:       "budget 0 is an explicit opt-out (kept, not folded to default)",
			env:        map[string]string{"AI_DAILY_TOKEN_BUDGET": "0"},
			wantTurns:  defaultAIRateLimitMaxTurns,
			wantWindow: defaultAIRateLimitWindowSec,
			wantConc:   defaultAIMaxConcurrent,
			wantBudget: 0,
		},
		{
			name:       "negative budget degrades to the default, never to unlimited",
			env:        map[string]string{"AI_DAILY_TOKEN_BUDGET": "-100"},
			wantTurns:  defaultAIRateLimitMaxTurns,
			wantWindow: defaultAIRateLimitWindowSec,
			wantConc:   defaultAIMaxConcurrent,
			wantBudget: defaultAIDailyTokenBudget,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"AI_RATELIMIT_MAX_TURNS", "AI_RATELIMIT_WINDOW_SEC", "AI_MAX_CONCURRENT", "AI_DAILY_TOKEN_BUDGET"} {
				t.Setenv(k, tt.env[k]) // unset (empty) unless the case overrides it
			}
			cfg := loadAI(t)
			if cfg.RateLimitMaxTurns != tt.wantTurns || cfg.RateLimitWindowSec != tt.wantWindow || cfg.MaxConcurrent != tt.wantConc {
				t.Fatalf("rate/concurrency = %d/%d/%d; want %d/%d/%d",
					cfg.RateLimitMaxTurns, cfg.RateLimitWindowSec, cfg.MaxConcurrent,
					tt.wantTurns, tt.wantWindow, tt.wantConc)
			}
			if cfg.DailyTokenBudget != tt.wantBudget {
				t.Fatalf("DailyTokenBudget = %d; want %d", cfg.DailyTokenBudget, tt.wantBudget)
			}
		})
	}
}

func TestLoadAIConfig_EnabledCaseInsensitive(t *testing.T) {
	// AI_ENABLED accepts any casing of "true"; anything else is disabled.
	for _, v := range []string{"TRUE", "True", "tRuE"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("AI_ENABLED", v)
			if !loadAI(t).Enabled {
				t.Fatalf("AI_ENABLED=%q → Enabled=false; want true", v)
			}
		})
	}
	for _, v := range []string{"", "false", "1", "yes", "on"} {
		t.Run("disabled_"+v, func(t *testing.T) {
			t.Setenv("AI_ENABLED", v)
			if loadAI(t).Enabled {
				t.Fatalf("AI_ENABLED=%q → Enabled=true; want false (only \"true\" enables)", v)
			}
		})
	}
}

// The default must be an id the gateway actually lists (a model that vanished from a gateway's /v1/models
// once made every call 502). Pin the literal so a silent change is visible.
func TestLoadAIConfig_DefaultModelIsListedByGateway(t *testing.T) {
	t.Setenv("AI_MODEL", "")
	if got := loadAI(t).Model; got != "claude-opus-5-5" {
		t.Fatalf("default Model = %q; want claude-opus-5-5", got)
	}
}

// AI_GATEWAY_MODE is the one AI_* knob that fails fast instead of degrading: a typo must not quietly
// run live (network, key, cost) where a replay was meant. replay needs neither base URL nor key.
func TestLoadAIConfig_GatewayMode(t *testing.T) {
	tests := []struct {
		name, mode, cassette   string
		wantMode, wantCassette string
		wantErr                string // substring of the error; "" = must load
	}{
		{name: "unset is live", wantMode: AIGatewayLive},
		{name: "live", mode: "live", wantMode: AIGatewayLive},
		{name: "record", mode: "record", cassette: "testdata/demo.jsonl", wantMode: AIGatewayRecord, wantCassette: "testdata/demo.jsonl"},
		{name: "replay trimmed, any case", mode: " Replay ", cassette: " demo.jsonl ", wantMode: AIGatewayReplay, wantCassette: "demo.jsonl"},
		{name: "unknown mode", mode: "replya", cassette: "demo.jsonl", wantErr: `AI_GATEWAY_MODE="replya"`},
		{name: "record without cassette", mode: "record", wantErr: "AI_GATEWAY_CASSETTE"},
		{name: "replay without cassette", mode: "replay", wantErr: "AI_GATEWAY_CASSETTE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AI_GATEWAY_MODE", tt.mode)
			t.Setenv("AI_GATEWAY_CASSETTE", tt.cassette)
			t.Setenv("AI_GATEWAY_BASE_URL", "")
			t.Setenv("AI_GATEWAY_API_KEY", "")
			cfg, err := LoadAIConfig()
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LoadAIConfig() error = %v; want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadAIConfig() error = %v; want nil", err)
			}
			if cfg.GatewayMode != tt.wantMode || cfg.Cassette != tt.wantCassette {
				t.Fatalf("GatewayMode/Cassette = %q/%q; want %q/%q", cfg.GatewayMode, cfg.Cassette, tt.wantMode, tt.wantCassette)
			}
		})
	}
}

func TestLoadAIConfig_APIKeyFromEnvTrimmed(t *testing.T) {
	t.Setenv("AI_GATEWAY_API_KEY", "  sk-x \n")
	if got := loadAI(t).APIKey; got != "sk-x" {
		t.Fatalf("APIKey = %q; want sk-x", got)
	}
}
