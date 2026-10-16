package config

import "testing"

// TestLoadWorkflowConfig_MaxChainDepth_Degradation pins the CREATE-RECORD recursion-guard knob's
// never-error contract: absent / unparseable / <1 all degrade to DefaultMaxChainDepth — NEVER 0
// (which would disable the guard entirely) and never unbounded. A guard whose ceiling silently
// degrades to 0/unlimited on a typo would neuter the recursion protection without any signal.
func TestLoadWorkflowConfig_MaxChainDepth_Degradation(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{raw: "", want: defaultWorkflowMaxChainDepth},
		{raw: "abc", want: defaultWorkflowMaxChainDepth},
		{raw: "0", want: defaultWorkflowMaxChainDepth},
		{raw: "-3", want: defaultWorkflowMaxChainDepth},
		{raw: "1", want: 1},
		{raw: "3", want: 3},
		{raw: "12", want: 12},
	}
	for _, tt := range tests {
		t.Run("val_"+tt.raw, func(t *testing.T) {
			t.Setenv("WORKFLOW_MAX_CHAIN_DEPTH", tt.raw)
			cfg, err := LoadWorkflowConfig()
			if err != nil {
				t.Fatalf("LoadWorkflowConfig: %v", err)
			}
			if cfg.MaxChainDepth != tt.want {
				t.Fatalf("WORKFLOW_MAX_CHAIN_DEPTH=%q → MaxChainDepth=%d; want %d", tt.raw, cfg.MaxChainDepth, tt.want)
			}
		})
	}
}

// TestLoadWorkflowConfig_SchedulerDefaultOn pins #291: a fresh install (env unset) runs the
// scheduler, so a saved "scheduled" rule fires; only an explicit 0 turns it off.
func TestLoadWorkflowConfig_SchedulerDefaultOn(t *testing.T) {
	tests := []struct {
		raw     string
		want    int // seconds
		enabled bool
	}{
		{raw: "", want: 60, enabled: true},
		{raw: "5", want: 5, enabled: true},
		{raw: "0", want: 0, enabled: false},
	}
	for _, tt := range tests {
		t.Run("val_"+tt.raw, func(t *testing.T) {
			t.Setenv("WORKFLOW_SCHEDULER_INTERVAL_SEC", tt.raw)
			cfg, err := LoadWorkflowConfig()
			if err != nil {
				t.Fatalf("LoadWorkflowConfig: %v", err)
			}
			if int(cfg.SchedulerInterval.Seconds()) != tt.want || cfg.SchedulerEnabled() != tt.enabled {
				t.Fatalf("interval=%v enabled=%v; want %ds enabled=%v", cfg.SchedulerInterval, cfg.SchedulerEnabled(), tt.want, tt.enabled)
			}
		})
	}
}
