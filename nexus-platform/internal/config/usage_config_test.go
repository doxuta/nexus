package config

import (
	"testing"
	"time"
)

func TestLoadUsageConfig(t *testing.T) {
	cases := []struct {
		name, interval, days string
		wantErr              bool
		wantInterval         time.Duration
		wantRetention        time.Duration
		wantEnabled          bool
	}{
		{name: "defaults", wantInterval: time.Hour, wantRetention: 730 * 24 * time.Hour, wantEnabled: true},
		{name: "off", interval: "0", wantInterval: 0, wantRetention: 730 * 24 * time.Hour},
		{name: "custom", interval: "60", days: "30", wantInterval: time.Minute, wantRetention: 30 * 24 * time.Hour, wantEnabled: true},
		{name: "negative interval", interval: "-1", wantErr: true},
		{name: "junk interval", interval: "abc", wantErr: true},
		{name: "zero retention", days: "0", wantErr: true},
		{name: "junk retention", days: "x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("USAGE_ROLLUP_INTERVAL_SEC", tc.interval)
			t.Setenv("USAGE_RETENTION_DAYS", tc.days)
			cfg, err := LoadUsageConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.RollupInterval != tc.wantInterval || cfg.Retention != tc.wantRetention || cfg.Enabled() != tc.wantEnabled {
				t.Fatalf("cfg = %+v", cfg)
			}
		})
	}
}
