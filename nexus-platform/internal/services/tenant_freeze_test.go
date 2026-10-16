package services

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeFreezeRepo struct {
	until   *time.Time
	reason  string
	getErr  error
	gets    int
	cleared int
}

func (f *fakeFreezeRepo) GetMaintenance(_ context.Context, _ string) (*time.Time, string, error) {
	f.gets++
	return f.until, f.reason, f.getErr
}
func (f *fakeFreezeRepo) SetMaintenance(_ context.Context, _ string, until time.Time, reason string) error {
	f.until, f.reason = &until, reason
	return nil
}
func (f *fakeFreezeRepo) ClearMaintenance(_ context.Context, _ string) error {
	f.until, f.reason = nil, ""
	f.cleared++
	return nil
}

func newTestFreeze(r *fakeFreezeRepo, now *time.Time) *TenantFreeze {
	f := NewTenantFreeze(r, nil)
	f.now = func() time.Time { return *now }
	return f
}

func TestTenantFreeze_CacheTTLAndInvalidate(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	r := &fakeFreezeRepo{until: &until}
	f := newTestFreeze(r, &now)
	ctx := context.Background()

	if u, ok, err := f.FrozenUntil(ctx, "t1"); err != nil || !ok || !u.Equal(until) {
		t.Fatalf("first read: %v %v %v", u, ok, err)
	}
	f.FrozenUntil(ctx, "t1")
	if r.gets != 1 {
		t.Fatalf("within TTL must hit cache, gets=%d", r.gets)
	}
	now = now.Add(2*time.Second + time.Millisecond)
	f.FrozenUntil(ctx, "t1")
	if r.gets != 2 {
		t.Fatalf("past TTL must re-read, gets=%d", r.gets)
	}
	f.Invalidate("t1")
	f.FrozenUntil(ctx, "t1")
	if r.gets != 3 {
		t.Fatalf("Invalidate must drop the entry, gets=%d", r.gets)
	}
	// Other tenant is independent.
	f.FrozenUntil(ctx, "t2")
	if r.gets != 4 {
		t.Fatalf("per-tenant cache, gets=%d", r.gets)
	}
}

func TestTenantFreeze_Lapsed(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	past := now.Add(-time.Second)
	r := &fakeFreezeRepo{until: &past}
	f := newTestFreeze(r, &now)
	if _, ok, err := f.FrozenUntil(context.Background(), "t1"); err != nil || ok {
		t.Fatalf("lapsed freeze must read as not frozen: ok=%v err=%v", ok, err)
	}
}

func TestTenantFreeze_ReadErrorKeepsKnownFreeze(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	until := now.Add(10 * time.Minute)
	boom := errors.New("db down")
	tests := []struct {
		name      string
		prime     bool
		advance   time.Duration
		wantOK    bool
		wantUntil bool
	}{
		{"no cache entry -> not frozen", false, 0, false, false},
		{"stale entry still in future -> frozen", true, 3 * time.Second, true, true},
		{"stale entry already lapsed -> not frozen", true, 11 * time.Minute, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cur := now
			r := &fakeFreezeRepo{until: &until}
			f := newTestFreeze(r, &cur)
			if tc.prime {
				f.FrozenUntil(context.Background(), "t1")
			}
			r.getErr = boom
			cur = cur.Add(tc.advance)
			u, ok, err := f.FrozenUntil(context.Background(), "t1")
			if err == nil || !errors.Is(err, boom) {
				t.Fatalf("error must surface wrapped, got %v", err)
			}
			if ok != tc.wantOK || (tc.wantUntil && !u.Equal(until)) {
				t.Fatalf("ok=%v until=%v", ok, u)
			}
		})
	}
}

func TestTenantFreeze_SetClearPrev(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	manual := now.Add(30 * time.Minute)
	r := &fakeFreezeRepo{until: &manual, reason: "manual"}
	f := newTestFreeze(r, &now)
	ctx := context.Background()

	prev, err := f.Set(ctx, "t1", now.Add(15*time.Minute), "restore")
	if err != nil || prev == nil || !prev.Equal(manual) {
		t.Fatalf("Set must return the live manual freeze: %v %v", prev, err)
	}
	if u, ok, _ := f.FrozenUntil(ctx, "t1"); !ok || !u.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("Set must be visible at once (cache invalidated): %v %v", u, ok)
	}
	if err := f.Extend(ctx, "t1", now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if u, _, _ := f.FrozenUntil(ctx, "t1"); !u.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("Extend not visible: %v", u)
	}
	// Clear(prev) puts the manual freeze back; Clear(nil) lifts it.
	if err := f.Clear(ctx, "t1", prev); err != nil {
		t.Fatal(err)
	}
	if u, ok, _ := f.FrozenUntil(ctx, "t1"); !ok || !u.Equal(manual) {
		t.Fatalf("Clear(prev) must restore the manual freeze: %v %v", u, ok)
	}
	if err := f.Clear(ctx, "t1", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.FrozenUntil(ctx, "t1"); ok {
		t.Fatal("Clear(nil) must lift the freeze")
	}
	// A prev that has already lapsed is not restored.
	lapsed := now.Add(-time.Minute)
	if err := f.Clear(ctx, "t1", &lapsed); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := f.FrozenUntil(ctx, "t1"); ok {
		t.Fatal("lapsed prev must not be restored")
	}
}
