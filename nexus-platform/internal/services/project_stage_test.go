package services

import (
	"testing"
	"time"
)

// specs/062-customer-workspace T074 — DeriveStage is pure: delivered+critical => needs_attention,
// delivered => live, trial => trial, else building. It never returns "maintenance" (that word is
// the write-freeze key of specs/068-tenant-backup-restore).
func TestDeriveStage(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		delivered *time.Time
		trial     *time.Time
		health    string
		want      string
	}{
		{"fresh, never checked", nil, nil, "unknown", "building"},
		{"building ignores health", nil, nil, "critical", "building"},
		{"trial", nil, &at, "ok", "trial"},
		{"trial ignores critical health", nil, &at, "critical", "trial"},
		{"delivered ok", &at, nil, "ok", "live"},
		{"delivered warn stays live", &at, nil, "warn", "live"},
		{"delivered unknown stays live", &at, nil, "unknown", "live"},
		{"delivered critical", &at, nil, "critical", "needs_attention"},
		{"delivered beats trial switch", &at, &at, "ok", "live"},
		{"delivered + trial + critical", &at, &at, "critical", "needs_attention"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DeriveStage(c.delivered, c.trial, c.health)
			if got != c.want {
				t.Fatalf("DeriveStage = %q, want %q", got, c.want)
			}
			if got == "maintenance" {
				t.Fatal("stage must never be the word maintenance")
			}
		})
	}
}
