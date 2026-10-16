package config

import (
	"testing"
	"time"
)

// specs/064-preview-as-role-sample-data FR-003: the preview session lasts PREVIEW_TTL_MINUTES,
// 1..60, default 30 — and a value outside the range refuses to boot instead of degrading.
func TestLoadPreviewConfig(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{"", 30 * time.Minute, false},
		{"1", time.Minute, false},
		{"60", 60 * time.Minute, false},
		{"15", 15 * time.Minute, false},
		{"0", 0, true},
		{"61", 0, true},
		{"-5", 0, true},
		{"abc", 0, true},
		{"30m", 0, true},
	}
	for _, tc := range cases {
		t.Run("PREVIEW_TTL_MINUTES="+tc.raw, func(t *testing.T) {
			t.Setenv("PREVIEW_TTL_MINUTES", tc.raw)
			cfg, err := LoadPreviewConfig()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got TTL %s", cfg.TTL)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.TTL != tc.want {
				t.Fatalf("TTL = %s, want %s", cfg.TTL, tc.want)
			}
		})
	}
}
