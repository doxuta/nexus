package jobs

import (
	"context"
	"testing"
	"time"
)

// TestBackoff pins the capped-exponential formula: min(base * 2^(attempts-1), cap).
func TestBackoff(t *testing.T) {
	base := 30 * time.Second
	cap := time.Hour
	tests := []struct {
		name     string
		base     time.Duration
		max      time.Duration
		attempts int
		want     time.Duration
	}{
		{"first retry = base", base, cap, 1, 30 * time.Second},
		{"second retry doubles", base, cap, 2, time.Minute},
		{"third retry", base, cap, 3, 2 * time.Minute},
		{"fourth retry", base, cap, 4, 4 * time.Minute},
		{"deep attempts capped", base, cap, 10, cap},
		{"huge attempts never overflow", base, cap, 1_000_000, cap},
		{"attempts below 1 clamps to base", base, cap, 0, 30 * time.Second},
		{"negative attempts clamps to base", base, cap, -5, 30 * time.Second},
		{"non-positive base falls back to default", 0, cap, 1, DefaultBackoffBase},
		{"non-positive cap falls back to default", base, 0, 20, DefaultBackoffCap},
		{"base above cap returns cap", 2 * time.Hour, cap, 1, cap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Backoff(tt.base, tt.max, tt.attempts); got != tt.want {
				t.Fatalf("Backoff(%s, %s, %d) = %s, want %s", tt.base, tt.max, tt.attempts, got, tt.want)
			}
		})
	}
}

// TestRegistry covers registration rules: empty type, nil handler and duplicates are rejected;
// a registered handler is retrievable; an unknown type reports !ok.
func TestRegistry(t *testing.T) {
	r := NewRegistry()
	noop := func(context.Context, Job) error { return nil }

	if err := r.Register("", noop); err == nil {
		t.Fatal("empty job type must be rejected")
	}
	if err := r.Register("x", nil); err == nil {
		t.Fatal("nil handler must be rejected")
	}
	if err := r.Register("email_alert", noop); err != nil {
		t.Fatalf("first registration failed: %v", err)
	}
	if err := r.Register("email_alert", noop); err == nil {
		t.Fatal("duplicate registration must be rejected (silent shadowing is a wiring bug)")
	}
	if _, ok := r.Handler("email_alert"); !ok {
		t.Fatal("registered handler not found")
	}
	if _, ok := r.Handler("unknown"); ok {
		t.Fatal("unknown type must report !ok")
	}
}
