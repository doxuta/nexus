// Unit tests for the M4 BudgetGuard (spec §12 — MED-1 hard gate): per-tenant rate sliding-window,
// per-tenant concurrency cap, and the UTC-daily token budget over the sys_ai_runs ledger. No DB —
// the ledger is a fake; the clock is injected so window-slide and midnight math are deterministic.
package ai

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/config"
)

// fakeClock is a mutex-guarded manual clock injected into the guard, mirroring how the auth
// limiter's time-based tests would pin time (the guard exposes an injectable now for exactly this).
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeTokenLedger scripts TokensSince and records what it was asked, so tests can assert both the
// budget decision and the day-boundary/tenant arguments without a database.
type fakeTokenLedger struct {
	mu         sync.Mutex
	total      int64
	err        error
	calls      int
	lastTenant string
	lastSince  time.Time
}

func (f *fakeTokenLedger) TokensSince(_ context.Context, tenantID string, since time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastTenant = tenantID
	f.lastSince = since
	return f.total, f.err
}

func (f *fakeTokenLedger) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// guardCfg is a permissive enabled baseline each test tightens for the gate under test.
func guardCfg() config.AIConfig {
	return config.AIConfig{
		Enabled:            true,
		RateLimitMaxTurns:  1000,
		RateLimitWindowSec: 60,
		MaxConcurrent:      1000,
		DailyTokenBudget:   0, // budget check off unless the test enables it
	}
}

// mustAcquire acquires and immediately releases, failing the test on any error.
func mustAcquire(t *testing.T, g *BudgetGuard, tenant string) {
	t.Helper()
	release, err := g.Acquire(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Acquire(%s) error = %v; want nil", tenant, err)
	}
	release()
}

// TestBudgetGuard_RateLimit_BlocksThenSlides: N turns pass, N+1 is ErrRateLimited carrying
// Retry-After = the window, and once the window slides past the old attempts the tenant is
// admitted again. A second tenant is never affected (per-tenant keying).
func TestBudgetGuard_RateLimit_BlocksThenSlides(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}
	cfg := guardCfg()
	cfg.RateLimitMaxTurns = 3
	cfg.RateLimitWindowSec = 60
	g := newBudgetGuard(cfg, nil, nil, clock.now)

	for i := 0; i < 3; i++ {
		mustAcquire(t, g, "tenant-a")
	}

	_, err := g.Acquire(context.Background(), "tenant-a")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("turn N+1 error = %v; want ErrRateLimited", err)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.RetryAfterSec != 60 {
		t.Fatalf("rate-limit error must carry RetryAfterSec=window (60); got %+v", le)
	}

	// Tenant isolation: tenant-b has its own window.
	mustAcquire(t, g, "tenant-b")

	// Slide past the window → the old attempts age out and tenant-a is admitted again.
	clock.advance(61 * time.Second)
	mustAcquire(t, g, "tenant-a")
}

// TestBudgetGuard_Concurrency_CapReleaseAndDoubleRelease: cap acquires hold slots, the next is
// ErrConcurrencyLimit (Retry-After 5), release frees exactly one slot, and calling the same
// release twice must NOT free a second slot (idempotent).
func TestBudgetGuard_Concurrency_CapReleaseAndDoubleRelease(t *testing.T) {
	cfg := guardCfg()
	cfg.MaxConcurrent = 2
	g := newBudgetGuard(cfg, nil, nil, time.Now)
	ctx := context.Background()

	r1, err := g.Acquire(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("acquire #1: %v", err)
	}
	if _, err = g.Acquire(ctx, "tenant-a"); err != nil {
		t.Fatalf("acquire #2: %v", err)
	}

	_, err = g.Acquire(ctx, "tenant-a")
	if !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("acquire #3 error = %v; want ErrConcurrencyLimit", err)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.RetryAfterSec != 5 {
		t.Fatalf("concurrency error must carry RetryAfterSec=5; got %+v", le)
	}

	// Other tenants have their own counter.
	if _, err = g.Acquire(ctx, "tenant-b"); err != nil {
		t.Fatalf("tenant-b must not share tenant-a's slots: %v", err)
	}

	// Release one slot → one more acquire fits.
	r1()
	r4, err := g.Acquire(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	_ = r4

	// Double-release must be a no-op: r1 was already spent, so the tenant is still AT cap and the
	// next acquire must fail. (A non-idempotent release would have freed a phantom slot here.)
	r1()
	if _, err = g.Acquire(ctx, "tenant-a"); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("after double-release acquire error = %v; want ErrConcurrencyLimit (no phantom slot)", err)
	}
}

// TestBudgetGuard_DailyBudget covers under/at/over budget, the UTC day boundary passed to the
// ledger, the Retry-After = seconds-to-next-UTC-midnight math, and that a budget rejection does
// NOT leak a concurrency slot.
func TestBudgetGuard_DailyBudget(t *testing.T) {
	// 10:00 UTC → next midnight is 14h away = 50400 seconds.
	clock := &fakeClock{t: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}

	tests := []struct {
		name    string
		total   int64
		wantErr bool
	}{
		{name: "under budget passes", total: 999, wantErr: false},
		{name: "at budget blocks (>=)", total: 1000, wantErr: true},
		{name: "over budget blocks", total: 1500, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := &fakeTokenLedger{total: tt.total}
			cfg := guardCfg()
			cfg.DailyTokenBudget = 1000
			cfg.MaxConcurrent = 1 // 1 slot: proves rejection releases it (see below)
			g := newBudgetGuard(cfg, ledger, nil, clock.now)

			release, err := g.Acquire(context.Background(), "tenant-a")
			if tt.wantErr {
				if !errors.Is(err, ErrBudgetExceeded) {
					t.Fatalf("Acquire error = %v; want ErrBudgetExceeded", err)
				}
				var le *LimitError
				if !errors.As(err, &le) || le.RetryAfterSec != 50400 {
					t.Fatalf("budget error must carry RetryAfterSec=50400 (10:00→24:00 UTC); got %+v", le)
				}
				// The single concurrency slot must have been released on rejection: dropping the
				// budget under the cap must admit the tenant immediately.
				ledger.mu.Lock()
				ledger.total = 0
				ledger.mu.Unlock()
				mustAcquire(t, g, "tenant-a")
			} else {
				if err != nil {
					t.Fatalf("Acquire error = %v; want nil under budget", err)
				}
				release()
			}

			// The ledger must be asked from the START OF THE UTC DAY for THIS tenant.
			if ledger.lastTenant != "tenant-a" {
				t.Fatalf("ledger asked for tenant %q; want tenant-a", ledger.lastTenant)
			}
			wantSince := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
			if !ledger.lastSince.Equal(wantSince) {
				t.Fatalf("ledger since = %v; want start of UTC day %v", ledger.lastSince, wantSince)
			}
		})
	}
}

// TestBudgetGuard_BudgetZero_CheckDisabled: DailyTokenBudget=0 is the explicit opt-out — the
// ledger must never be consulted.
func TestBudgetGuard_BudgetZero_CheckDisabled(t *testing.T) {
	ledger := &fakeTokenLedger{total: 1 << 40} // astronomically over any budget
	cfg := guardCfg()
	cfg.DailyTokenBudget = 0
	g := newBudgetGuard(cfg, ledger, nil, time.Now)

	mustAcquire(t, g, "tenant-a")
	if ledger.callCount() != 0 {
		t.Fatalf("ledger consulted %d times with budget=0; want 0 (check disabled)", ledger.callCount())
	}
}

// TestBudgetGuard_LedgerError_FailClosed: a budget-query DB error must PROPAGATE as a plain error
// that matches NONE of the three limit sentinels — the handler's default branch turns it into a
// 500 AI_CHAT_FAILED (the cost gate fails closed, unlike the fail-open ledger write). The
// concurrency slot must also be released on this path.
func TestBudgetGuard_LedgerError_FailClosed(t *testing.T) {
	ledger := &fakeTokenLedger{err: errors.New("db down")}
	cfg := guardCfg()
	cfg.DailyTokenBudget = 1000
	cfg.MaxConcurrent = 1
	g := newBudgetGuard(cfg, ledger, nil, time.Now)

	_, err := g.Acquire(context.Background(), "tenant-a")
	if err == nil {
		t.Fatal("Acquire must fail closed when the budget query errors")
	}
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrConcurrencyLimit) || errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("a ledger DB error must not be classified as a 429 limit; got %v", err)
	}

	// The slot leaked? Clearing the error must admit the tenant on its single slot.
	ledger.mu.Lock()
	ledger.err = nil
	ledger.mu.Unlock()
	mustAcquire(t, g, "tenant-a")
}

// TestBudgetGuard_NilLedgerWithBudget_FailClosed: wiring a guard with an active budget but no
// ledger is a construction bug — it must fail closed (error), never silently skip the cost check.
func TestBudgetGuard_NilLedgerWithBudget_FailClosed(t *testing.T) {
	cfg := guardCfg()
	cfg.DailyTokenBudget = 1000
	g := newBudgetGuard(cfg, nil, nil, time.Now)

	if _, err := g.Acquire(context.Background(), "tenant-a"); err == nil {
		t.Fatal("Acquire must fail closed when budget > 0 but no ledger is wired")
	}
}

// TestBudgetGuard_Disabled_PassThrough: with AI disabled the guard is a pure pass-through — the
// agent's honest-disabled 503 must win, never a 429 — and NOTHING is charged: no rate slot, no
// concurrency slot, no ledger query, even under limits of 1.
func TestBudgetGuard_Disabled_PassThrough(t *testing.T) {
	ledger := &fakeTokenLedger{total: 1 << 40}
	cfg := config.AIConfig{
		Enabled:            false,
		RateLimitMaxTurns:  1,
		RateLimitWindowSec: 60,
		MaxConcurrent:      1,
		DailyTokenBudget:   1,
	}
	g := newBudgetGuard(cfg, ledger, nil, time.Now)

	for i := 0; i < 10; i++ {
		release, err := g.Acquire(context.Background(), "tenant-a")
		if err != nil {
			t.Fatalf("disabled guard must pass through; Acquire #%d error = %v", i+1, err)
		}
		release() // must be non-nil and safe even though nothing was acquired
	}
	if ledger.callCount() != 0 {
		t.Fatalf("disabled guard consulted the ledger %d times; want 0", ledger.callCount())
	}
}

// fakeUserTokenLedger is a fakeTokenLedger that ALSO sums per user (satisfies UserTokenLedger),
// scripting the user total independently from the tenant total and recording the asked user.
type fakeUserTokenLedger struct {
	fakeTokenLedger
	userTotal int64
	userErr   error
	userCalls int
	lastUser  string
}

func (f *fakeUserTokenLedger) TokensSinceByUser(_ context.Context, tenantID, userID string, since time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.userCalls++
	f.lastTenant = tenantID
	f.lastUser = userID
	f.lastSince = since
	return f.userTotal, f.userErr
}

// TestBudgetGuard_UserDailyShare covers the AcquireUser share math and gate: with budget 1000 and
// share 50% the user cap is 500 — under passes, at/over rejects with the SAME ErrBudgetExceeded /
// Retry-After-to-midnight contract as the tenant gate, and the concurrency slot is released on
// rejection. The tiny-budget clamp (budget×pct/100 == 0 → 1) admits a user who has spent nothing.
func TestBudgetGuard_UserDailyShare(t *testing.T) {
	// 10:00 UTC → next midnight is 14h away = 50400 seconds (same math as the tenant gate).
	clock := &fakeClock{t: time.Date(2026, 7, 8, 10, 0, 0, 0, time.UTC)}

	tests := []struct {
		name      string
		budget    int64
		pct       int
		userTotal int64
		wantErr   bool
	}{
		{name: "under share passes", budget: 1000, pct: 50, userTotal: 499, wantErr: false},
		{name: "at share blocks (>=)", budget: 1000, pct: 50, userTotal: 500, wantErr: true},
		{name: "over share blocks", budget: 1000, pct: 50, userTotal: 900, wantErr: true},
		{name: "clamp: zero-truncated share admits an unspent user", budget: 10, pct: 5, userTotal: 0, wantErr: false},
		{name: "clamp: zero-truncated share still blocks after any spend", budget: 10, pct: 5, userTotal: 1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger := &fakeUserTokenLedger{userTotal: tt.userTotal}
			cfg := guardCfg()
			cfg.DailyTokenBudget = tt.budget
			cfg.AssistantUserDailySharePct = tt.pct
			cfg.MaxConcurrent = 1 // 1 slot: proves rejection releases it
			g := newBudgetGuard(cfg, ledger, nil, clock.now)

			release, err := g.AcquireUser(context.Background(), "tenant-a", "user-1")
			if tt.wantErr {
				if !errors.Is(err, ErrBudgetExceeded) {
					t.Fatalf("AcquireUser error = %v; want ErrBudgetExceeded", err)
				}
				var le *LimitError
				if !errors.As(err, &le) || le.RetryAfterSec != 50400 {
					t.Fatalf("share error must carry RetryAfterSec=50400; got %+v", le)
				}
				// The slot must not leak: dropping the user's spend admits immediately.
				ledger.mu.Lock()
				ledger.userTotal = 0
				ledger.mu.Unlock()
				if r, err := g.AcquireUser(context.Background(), "tenant-a", "user-1"); err != nil {
					t.Fatalf("AcquireUser after clearing spend: %v (slot leaked?)", err)
				} else {
					r()
				}
			} else {
				if err != nil {
					t.Fatalf("AcquireUser error = %v; want nil under share", err)
				}
				release()
			}
			if ledger.lastUser != "user-1" {
				t.Fatalf("ledger asked for user %q; want user-1", ledger.lastUser)
			}
			wantSince := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
			if !ledger.lastSince.Equal(wantSince) {
				t.Fatalf("user ledger since = %v; want start of UTC day %v", ledger.lastSince, wantSince)
			}
		})
	}
}

// TestBudgetGuard_UserDailyShare_Boundaries: the user gate never runs for the builder path
// (Acquire — no user), never runs at share 100 (whole budget: the tenant gate already covers it),
// never runs when the budget is opted out (0), and stays SECOND to the tenant gate (a tenant
// already over budget rejects before the user ledger is consulted).
func TestBudgetGuard_UserDailyShare_Boundaries(t *testing.T) {
	newGuard := func(budget int64, pct int, tenantTotal, userTotal int64) (*BudgetGuard, *fakeUserTokenLedger) {
		ledger := &fakeUserTokenLedger{userTotal: userTotal}
		ledger.total = tenantTotal
		cfg := guardCfg()
		cfg.DailyTokenBudget = budget
		cfg.AssistantUserDailySharePct = pct
		return newBudgetGuard(cfg, ledger, nil, time.Now), ledger
	}

	t.Run("builder Acquire never consults the user ledger", func(t *testing.T) {
		g, ledger := newGuard(1000, 50, 0, 1<<40) // user astronomically over — must not matter
		mustAcquire(t, g, "tenant-a")
		if ledger.userCalls != 0 {
			t.Fatalf("Acquire consulted the user ledger %d times; want 0", ledger.userCalls)
		}
	})

	t.Run("share 100 skips the user query (tenant gate covers it)", func(t *testing.T) {
		g, ledger := newGuard(1000, 100, 0, 1<<40)
		if _, err := g.AcquireUser(context.Background(), "tenant-a", "user-1"); err != nil {
			t.Fatalf("AcquireUser at share 100: %v; want pass-through to the tenant gate", err)
		}
		if ledger.userCalls != 0 {
			t.Fatalf("share 100 consulted the user ledger %d times; want 0", ledger.userCalls)
		}
	})

	t.Run("budget 0 opt-out disables the user gate too", func(t *testing.T) {
		g, ledger := newGuard(0, 50, 1<<40, 1<<40)
		if _, err := g.AcquireUser(context.Background(), "tenant-a", "user-1"); err != nil {
			t.Fatalf("AcquireUser with budget 0: %v; want nil (whole budget gate off)", err)
		}
		if ledger.userCalls != 0 || ledger.callCount() != 0 {
			t.Fatalf("budget 0 consulted a ledger (tenant %d / user %d calls); want 0/0", ledger.callCount(), ledger.userCalls)
		}
	})

	t.Run("tenant gate rejects before the user ledger is consulted", func(t *testing.T) {
		g, ledger := newGuard(1000, 50, 1000, 0)
		if _, err := g.AcquireUser(context.Background(), "tenant-a", "user-1"); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("AcquireUser over tenant budget = %v; want ErrBudgetExceeded", err)
		}
		if ledger.userCalls != 0 {
			t.Fatalf("tenant-over rejection consulted the user ledger %d times; want 0", ledger.userCalls)
		}
	})
}

// TestBudgetGuard_UserDailyShare_FailClosed: a user-ledger read error and a ledger that CANNOT sum
// per user (plain TokenLedger, no TokensSinceByUser) while a share is configured are both wiring/
// infra failures — the gate must fail CLOSED with a plain (non-429) error, releasing the slot.
func TestBudgetGuard_UserDailyShare_FailClosed(t *testing.T) {
	t.Run("user ledger read error", func(t *testing.T) {
		ledger := &fakeUserTokenLedger{userErr: errors.New("db down")}
		cfg := guardCfg()
		cfg.DailyTokenBudget = 1000
		cfg.AssistantUserDailySharePct = 50
		cfg.MaxConcurrent = 1
		g := newBudgetGuard(cfg, ledger, nil, time.Now)

		_, err := g.AcquireUser(context.Background(), "tenant-a", "user-1")
		if err == nil {
			t.Fatal("AcquireUser must fail closed on a user-ledger read error")
		}
		if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrConcurrencyLimit) || errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("a ledger DB error must not be classified as a 429 limit; got %v", err)
		}
		// The slot must have been released.
		ledger.mu.Lock()
		ledger.userErr = nil
		ledger.mu.Unlock()
		if r, err := g.AcquireUser(context.Background(), "tenant-a", "user-1"); err != nil {
			t.Fatalf("AcquireUser after clearing error: %v (slot leaked?)", err)
		} else {
			r()
		}
	})

	t.Run("ledger cannot sum per user", func(t *testing.T) {
		cfg := guardCfg()
		cfg.DailyTokenBudget = 1000
		cfg.AssistantUserDailySharePct = 50
		g := newBudgetGuard(cfg, &fakeTokenLedger{}, nil, time.Now) // no TokensSinceByUser

		_, err := g.AcquireUser(context.Background(), "tenant-a", "user-1")
		if err == nil {
			t.Fatal("AcquireUser must fail closed when the ledger cannot sum per user")
		}
		if errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("a wiring bug must not masquerade as a 429; got %v", err)
		}
	})
}

// TestBudgetGuard_ConcurrentHammer (-race): goroutines hammer Acquire/release and the number of
// simultaneously-held slots must never exceed the cap, with no slot leaked at the end.
func TestBudgetGuard_ConcurrentHammer(t *testing.T) {
	const maxSlots = 4 // named to avoid shadowing the builtin cap() (post-review NIT-5)
	cfg := guardCfg()
	cfg.RateLimitMaxTurns = 1 << 20 // rate out of the way; this test is about slots + data races
	cfg.MaxConcurrent = maxSlots
	g := newBudgetGuard(cfg, nil, nil, time.Now)

	var held int64
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				release, err := g.Acquire(context.Background(), "tenant-a")
				if err != nil {
					if !errors.Is(err, ErrConcurrencyLimit) {
						t.Errorf("unexpected error: %v", err)
						return
					}
					continue
				}
				if n := atomic.AddInt64(&held, 1); n > maxSlots {
					t.Errorf("held %d slots concurrently; cap is %d", n, maxSlots)
				}
				atomic.AddInt64(&held, -1)
				release()
				release() // double-release under contention must stay safe
			}
		}()
	}
	wg.Wait()

	// No slot leaked: the full cap must be acquirable now.
	for i := 0; i < maxSlots; i++ {
		if _, err := g.Acquire(context.Background(), "tenant-a"); err != nil {
			t.Fatalf("slot %d not acquirable after hammer (leak?): %v", i+1, err)
		}
	}
	if _, err := g.Acquire(context.Background(), "tenant-a"); !errors.Is(err, ErrConcurrencyLimit) {
		t.Fatalf("cap+1 after hammer = %v; want ErrConcurrencyLimit", err)
	}
}
