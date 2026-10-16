// budget.go — E9 M4 (spec §12): the per-tenant BudgetGuard that closes MED-1, the hard
// before-enable gate. Per-turn spend was already bounded in M1 (MaxSteps × MaxTokens × Timeout);
// this guard bounds spend BETWEEN turns with three per-tenant gates around every chat turn:
//
//	(1) rate     — a sliding window of RateLimitMaxTurns per RateLimitWindowSec, so one admin
//	               token (real or stolen) cannot start turns unboundedly fast;
//	(2) inflight — at most MaxConcurrent turns running at once (a turn can hold a gateway
//	               connection for MaxSteps × Timeout — parallel spam is the expensive vector);
//	(3) budget   — SUM(prompt+output tokens) per UTC day from the sys_ai_runs ledger must be
//	               under DailyTokenBudget (0 = explicit opt-out).
//
// AcquireUser (the every-user ⌘K assistant's entry point) adds a fourth, per-USER gate: (3b) the
// caller's OWN UTC-day token sum must be under AssistantUserDailySharePct% of DailyTokenBudget, so
// a steady user under the per-user burst cap cannot drain the tenant's whole day by themselves.
//
// Keying is by TENANT, not IP: the caller is an already-authenticated admin and the tenant is the
// unit of LLM cost. Rate + concurrency state is in-memory per-process (same documented limitation
// as the RM-15 auth limiter — swap to a Redis sliding window for multi-instance); the budget is
// DB-backed so it is correct across instances either way.
//
// Fail modes are asymmetric BY DESIGN: the ledger WRITE (agent.recordRun) stays fail-open (a lost
// audit row must not kill a served turn), but the budget READ here fails CLOSED — a cost gate that
// fails open is no gate. A TokensSince error propagates as a plain (non-429) error the handler
// maps to 500 AI_CHAT_FAILED.
//
// Accepted overshoot (spec §12): the check runs PRE-turn against a ledger that only reflects
// COMPLETED turns, so every turn admitted while others are still in flight can independently pass
// the same budget snapshot before any of them commits usage back. Up to MaxConcurrent turns can be
// in that window at once, each spending up to MaxSteps × MaxTokens — so the day total may overshoot
// the configured budget by at most MaxConcurrent × MaxSteps × MaxTokens (default
// 2 × 12 × 4096 ≈ 98k, against a 200k default budget — MaxSteps was raised 6→12 for Discovery
// M10, so one worst-case turn is ~49k). That is bounded and not worth a mid-turn abort.
package ai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/config"
)

// Limit sentinels — the handler maps each to its own 429 code via errors.Is (spec §12):
// AI_RATE_LIMITED, AI_CONCURRENCY_LIMIT, AI_BUDGET_EXCEEDED.
var (
	ErrRateLimited      = errors.New("ai: tenant rate limit exceeded")
	ErrConcurrencyLimit = errors.New("ai: tenant concurrency limit reached")
	ErrBudgetExceeded   = errors.New("ai: tenant daily token budget exceeded")
)

// concurrencyRetryAfterSec is the Retry-After hint for a concurrency rejection: an in-flight turn
// usually finishes within seconds, so a short fixed backoff beats echoing the (much larger)
// worst-case turn duration.
const concurrencyRetryAfterSec = 5

// LimitError wraps one of the limit sentinels together with the Retry-After hint (in seconds) the
// handler should emit, so the HTTP layer never needs to know the guard's configuration. It matches
// the sentinel through errors.Is (via Unwrap) and surfaces the hint through errors.As.
type LimitError struct {
	// Sentinel is one of ErrRateLimited / ErrConcurrencyLimit / ErrBudgetExceeded.
	Sentinel error
	// RetryAfterSec is the value for the HTTP Retry-After header: the window length for rate, a
	// short fixed backoff for concurrency, and the seconds to the next UTC midnight for budget.
	RetryAfterSec int
}

func (e *LimitError) Error() string { return e.Sentinel.Error() }
func (e *LimitError) Unwrap() error { return e.Sentinel }

// TokenLedger is the consumer-side reader of the sys_ai_runs ledger the daily-budget gate needs
// (satisfied by *SQLRunRepository). Declared here so unit tests inject a fake and the guard never
// depends on sqlx directly.
type TokenLedger interface {
	TokensSince(ctx context.Context, tenantID string, since time.Time) (int64, error)
}

// UserTokenLedger is the OPTIONAL per-user extension of TokenLedger the assistant's daily-share
// gate needs (also satisfied by *SQLRunRepository). Kept as a separate interface (checked by type
// assertion in AcquireUser) so the many existing TokenLedger fakes stay valid; a guard asked to
// enforce a per-user share over a ledger that cannot sum per user fails CLOSED, like every other
// budget wiring bug.
type UserTokenLedger interface {
	TokensSinceByUser(ctx context.Context, tenantID, userID string, since time.Time) (int64, error)
}

// BudgetGuard enforces the three per-tenant gates around a chat turn. The handler calls Acquire
// immediately before agent.Chat on the /chat endpoint ONLY (session list/get/delete are plain DB
// reads — no LLM spend, no gate) and defers the returned release. Construct once per process
// (NewBudgetGuard) and share: the rate/inflight state is only meaningful process-wide.
type BudgetGuard struct {
	cfg    config.AIConfig
	ledger TokenLedger
	logger *zap.Logger
	now    func() time.Time

	// limiter is the per-tenant rate sliding window; nil when the rate gate is disabled
	// (non-positive knobs — LoadAIConfig never produces those, but a hand-built config might).
	limiter *slidingWindow

	// mu guards inflight, the per-tenant count of running turns. Entries are deleted at zero so
	// the map stays bounded by the number of tenants with a turn CURRENTLY in flight.
	mu       sync.Mutex
	inflight map[string]int
}

// NewBudgetGuard wires the M4 guard over the AI config and the sys_ai_runs ledger reader
// (production passes *SQLRunRepository — it must be non-nil whenever DailyTokenBudget > 0, or
// every turn fails closed). A nil logger is replaced with a no-op.
func NewBudgetGuard(cfg config.AIConfig, ledger TokenLedger, logger *zap.Logger) *BudgetGuard {
	return newBudgetGuard(cfg, ledger, logger, time.Now)
}

// newBudgetGuard is the injectable-clock constructor used by tests; the clock must be fixed at
// construction time because the sliding window's janitor goroutine reads it concurrently.
func newBudgetGuard(cfg config.AIConfig, ledger TokenLedger, logger *zap.Logger, now func() time.Time) *BudgetGuard {
	if logger == nil {
		logger = zap.NewNop()
	}
	g := &BudgetGuard{
		cfg:      cfg,
		ledger:   ledger,
		logger:   logger,
		now:      now,
		inflight: make(map[string]int),
	}
	if cfg.RateLimitMaxTurns > 0 && cfg.RateLimitWindowSec > 0 {
		g.limiter = newSlidingWindow(cfg.RateLimitMaxTurns, time.Duration(cfg.RateLimitWindowSec)*time.Second, now)
	}
	return g
}

// Acquire admits or rejects one chat turn for tenantID. On success it returns a release func the
// caller MUST invoke when the turn ends (defer it); release is idempotent — a double call never
// frees a phantom slot. On rejection it returns a *LimitError wrapping the matching sentinel (429
// at the handler), or a plain error for a budget-read failure (fail-closed 500). The returned
// release is non-nil on EVERY path so a careless defer can never panic.
//
// Gate order (spec §12): (0) disabled → pass-through, so the agent's honest-disabled 503 wins and
// nothing is charged; (1) rate; (2) concurrency; (3) daily budget. The rate attempt is charged at
// admission time — atomically inside allow(), which collapses the auth limiter's peek/record split
// (needed there to coordinate TWO windows; here there is one, and the atomic form closes its
// admit-over-cap race). Consequence, intentional: a turn later rejected by concurrency or budget
// HAS consumed a rate slot — fail-secure, and it rate-bounds the budget's DB query too, so a
// budget-exhausted tenant cannot hammer MySQL with SUM queries either.
func (g *BudgetGuard) Acquire(ctx context.Context, tenantID string) (func(), error) {
	return g.acquire(ctx, tenantID, "")
}

// AcquireUser is Acquire plus the assistant's per-user daily-share gate: after the three tenant
// gates pass, the USER's own UTC-day token sum (sys_ai_runs.user_id) must be under
// AssistantUserDailySharePct% of DailyTokenBudget, or the turn is rejected with the SAME
// ErrBudgetExceeded / 429 AI_BUDGET_EXCEEDED mapping (Retry-After = next UTC midnight). This is
// the gate that stops a steady user (under the per-user BURST cap all day) from burning the whole
// tenant's daily budget through the every-user ⌘K assistant; the admin-only builder keeps calling
// Acquire and is never user-gated.
func (g *BudgetGuard) AcquireUser(ctx context.Context, tenantID, userID string) (func(), error) {
	return g.acquire(ctx, tenantID, userID)
}

// acquire is the shared gate body. userID == "" (the builder path) skips the per-user share gate.
func (g *BudgetGuard) acquire(ctx context.Context, tenantID, userID string) (func(), error) {
	noop := func() {}

	// (0) Disabled → pure pass-through: the agent answers 503 AI_DISABLED itself, spends nothing,
	// and a 429 here would mask that honest signal.
	if !g.cfg.Enabled {
		return noop, nil
	}

	// (1) Per-tenant rate sliding window.
	if g.limiter != nil && !g.limiter.allow(tenantID) {
		g.logger.Warn("ai: chat turn rejected by rate limit",
			zap.String("tenant_id", tenantID),
			zap.Int("max_turns", g.cfg.RateLimitMaxTurns),
			zap.Int("window_sec", g.cfg.RateLimitWindowSec))
		return noop, &LimitError{Sentinel: ErrRateLimited, RetryAfterSec: g.cfg.RateLimitWindowSec}
	}

	// (2) Per-tenant concurrency try-acquire.
	release, ok := g.tryAcquireSlot(tenantID)
	if !ok {
		g.logger.Warn("ai: chat turn rejected by concurrency cap",
			zap.String("tenant_id", tenantID),
			zap.Int("max_concurrent", g.cfg.MaxConcurrent))
		return noop, &LimitError{Sentinel: ErrConcurrencyLimit, RetryAfterSec: concurrencyRetryAfterSec}
	}

	// (3) Daily token budget (UTC day) from the ledger. 0 = explicit opt-out (spec §12).
	if g.cfg.DailyTokenBudget > 0 {
		if g.ledger == nil {
			// A guard wired with an active budget but no ledger is a construction bug; skipping
			// the check would silently disable a cost gate → fail closed instead.
			release()
			return noop, fmt.Errorf("ai.BudgetGuard.Acquire: daily budget %d configured but no ledger wired", g.cfg.DailyTokenBudget)
		}
		used, err := g.ledger.TokensSince(ctx, tenantID, startOfUTCDay(g.now()))
		if err != nil {
			// FAIL-CLOSED: a cost gate must not fail open. The plain (non-sentinel) error lands
			// in the handler's default branch → 500 AI_CHAT_FAILED.
			release()
			return noop, fmt.Errorf("ai.BudgetGuard.Acquire: budget check: %w", err)
		}
		if used >= g.cfg.DailyTokenBudget {
			release()
			g.logger.Warn("ai: chat turn rejected by daily token budget",
				zap.String("tenant_id", tenantID),
				zap.Int64("tokens_used", used),
				zap.Int64("daily_budget", g.cfg.DailyTokenBudget))
			return noop, &LimitError{Sentinel: ErrBudgetExceeded, RetryAfterSec: secondsToNextUTCMidnight(g.now())}
		}

		// (3b) Per-user daily share of the tenant budget (AcquireUser only — userID != "").
		// Pct 100 = whole-budget share: mathematically covered by the tenant gate above (a user's
		// sum can never exceed the tenant's), so the second SUM query is skipped. Pct <= 0 only
		// occurs on hand-built configs (LoadAIConfig degrades to the biting default) → gate off,
		// same contract as the other non-positive knobs.
		if userID != "" && g.cfg.AssistantUserDailySharePct > 0 && g.cfg.AssistantUserDailySharePct < 100 {
			ul, ok := g.ledger.(UserTokenLedger)
			if !ok {
				// Share configured but the ledger cannot sum per user — a wiring bug; skipping
				// would silently widen a cost gate → fail closed (handler maps to 500).
				release()
				return noop, fmt.Errorf("ai.BudgetGuard.AcquireUser: per-user share %d%% configured but ledger cannot sum per user", g.cfg.AssistantUserDailySharePct)
			}
			usedByUser, err := ul.TokensSinceByUser(ctx, tenantID, userID, startOfUTCDay(g.now()))
			if err != nil {
				// FAIL-CLOSED, same as the tenant budget read above.
				release()
				return noop, fmt.Errorf("ai.BudgetGuard.AcquireUser: user budget check: %w", err)
			}
			// Integer share of the budget; clamped to >= 1 so a tiny budget × small pct can never
			// truncate to 0 and block a user who has spent NOTHING yet.
			userBudget := g.cfg.DailyTokenBudget * int64(g.cfg.AssistantUserDailySharePct) / 100
			if userBudget < 1 {
				userBudget = 1
			}
			if usedByUser >= userBudget {
				release()
				g.logger.Warn("ai: chat turn rejected by per-user daily token share",
					zap.String("tenant_id", tenantID),
					zap.String("user_id", userID),
					zap.Int64("user_tokens_used", usedByUser),
					zap.Int64("user_budget", userBudget),
					zap.Int("share_pct", g.cfg.AssistantUserDailySharePct))
				return noop, &LimitError{Sentinel: ErrBudgetExceeded, RetryAfterSec: secondsToNextUTCMidnight(g.now())}
			}
		}
	}

	return release, nil
}

// tryAcquireSlot increments the tenant's in-flight count unless it is at MaxConcurrent, returning
// an idempotent release (sync.Once) that decrements it and deletes the entry at zero (bounded
// map). A non-positive cap disables the gate (LoadAIConfig never yields one; parity with the
// AuthRateLimit <= 0 no-op contract for hand-built configs).
func (g *BudgetGuard) tryAcquireSlot(tenantID string) (func(), bool) {
	if g.cfg.MaxConcurrent <= 0 {
		return func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inflight[tenantID] >= g.cfg.MaxConcurrent {
		return nil, false
	}
	g.inflight[tenantID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.inflight[tenantID] <= 1 {
				delete(g.inflight, tenantID)
			} else {
				g.inflight[tenantID]--
			}
		})
	}, true
}

// startOfUTCDay returns 00:00:00 UTC of now's UTC calendar day — the budget window boundary.
// sys_ai_runs.created_at is written UTC (SQLRunRepository.Record uses r.now().UTC()), so this
// boundary and the ledger rows share one timeline regardless of server locale.
func startOfUTCDay(now time.Time) time.Time {
	u := now.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// secondsToNextUTCMidnight is the budget rejection's Retry-After: the budget resets at the next
// UTC day boundary. Rounds UP, not down (post-review NIT-2): a naive truncation of the sub-second
// remainder would tell the client to retry up to 1s too early — a retry landing BEFORE midnight
// would just hit the same still-exhausted budget. Clamped to >= 1 so the header is always a
// positive interval.
func secondsToNextUTCMidnight(now time.Time) int {
	next := startOfUTCDay(now).Add(24 * time.Hour)
	d := next.Sub(now.UTC())
	s := int((d + time.Second - 1) / time.Second) // ceil to the next whole second
	if s < 1 {
		s = 1
	}
	return s
}

// ---- per-tenant sliding-window rate limiter --------------------------------------------------
//
// A deliberately small sibling of middleware.memRateLimiter (the proven RM-15 pattern): same
// sliding window over recorded timestamps, same janitor eviction at 2× window so memory stays
// bounded by ACTIVE tenants, but keyed by tenant (no body parsing — the tenant is already in the
// request context) and with a single atomic allow() instead of the peek/record split (that split
// exists to coordinate auth's two windows; with one window it would only reopen the
// check-then-record admit race).

type slidingWindow struct {
	mu      sync.Mutex
	entries map[string][]time.Time
	max     int
	window  time.Duration
	now     func() time.Time
}

func newSlidingWindow(max int, window time.Duration, now func() time.Time) *slidingWindow {
	w := &slidingWindow{
		entries: make(map[string][]time.Time),
		max:     max,
		window:  window,
		now:     now,
	}
	// Janitor: evicts stale tenants for the guard's lifetime (constructed once per process, like
	// the auth limiter's). Collected by the runtime at process exit. No stop channel (post-review
	// NIT-4, accepted as-is): production wires exactly ONE guard per process, so this never
	// accumulates; the unit test suite constructs many short-lived guards, which does spawn a few
	// extra tickers for the process lifetime of `go test` — harmless (bounded by test count, GC'd
	// with the process at suite exit), not worth a stop-channel for a single-instance-per-process
	// component.
	go w.janitor()
	return w
}

// allow prunes the key's window, then atomically checks-and-records under one lock: the attempt
// is charged if and only if it is admitted, and two racing calls can never both slip through a
// single remaining slot.
func (w *slidingWindow) allow(key string) bool {
	now := w.now()
	cutoff := now.Add(-w.window)
	w.mu.Lock()
	defer w.mu.Unlock()
	valid := w.entries[key][:0]
	for _, t := range w.entries[key] {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	if len(valid) >= w.max {
		w.entries[key] = valid
		return false
	}
	w.entries[key] = append(valid, now)
	return true
}

// janitor wakes every window duration and evicts entries whose newest attempt is older than 2×
// the window, so the map is bounded by tenants active in the recent past (mirrors the auth
// limiter's janitor contract).
func (w *slidingWindow) janitor() {
	ticker := time.NewTicker(w.window)
	defer ticker.Stop()
	for range ticker.C {
		w.evictStale()
	}
}

func (w *slidingWindow) evictStale() {
	cutoff := w.now().Add(-2 * w.window)
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, ts := range w.entries {
		if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
			delete(w.entries, key)
		}
	}
}
