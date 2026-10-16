// mfa_attempt_limiter.go implements a per-account failed-MFA-attempt throttle guarding
// POST /auth/mfa/enroll, POST /auth/mfa/activate, and POST /auth/mfa/disable (reviewer MED-1 /
// security review 🟡#4; enroll added by MFA-STEPUP once it too gained a guessable secret — the
// caller's account password).
//
// Why this exists: those three endpoints sit on the PROTECTED router group (Auth -> Tenant only —
// see router.go's authGroup, which carries RM-15's rate limiter, versus the plain `protected`
// group they are mounted on) precisely because they need a caller's verified identity to run at
// all — a user manages only their own MFA (auth_handler.go's RegisterProtectedRoutes doc). That
// means a HIJACKED SESSION (a stolen/leaked access token) could otherwise brute-force a 6-digit
// TOTP code, a 20-hex backup code, or (MFA-STEPUP) the account password itself against an
// already-authenticated account with ZERO rate limiting: RM-15 only guards the PRE-auth
// /auth/mfa/verify path (step 2 of login), which this slice does NOT touch. This closes that
// specific gap.
//
// Design: an in-memory sliding-window limiter keyed per (tenant_id, user_id) — deliberately NOT
// per source IP, because the identity behind a stolen session token is fixed while the network
// path (IP) is fully attacker-controlled and freely rotatable. Mirrors the shape of
// internal/middleware/auth_ratelimit.go's memRateLimiter (mutex-guarded map + pruned timestamp
// slice + a background janitor bounding memory), adapted for a POST-auth identity key and for
// "count only FAILURES, not every attempt" semantics (an attempt that turns out CORRECT must
// reset the count, not add to it — see mfaAttemptLimiter's Reset).
package services

import (
	"sync"
	"time"
)

const (
	// mfaAttemptMaxFailures is how many WRONG codes (not total attempts) a single (tenant_id,
	// user_id) may present within MFAAttemptLockoutWindow before Allow starts returning false.
	mfaAttemptMaxFailures = 5
)

// MFAAttemptLockoutWindow is the sliding window the per-account failed-MFA-code throttle uses.
// Exported so the HTTP layer (handlers.writeMFAError) can set an accurate Retry-After header
// without duplicating the constant.
const MFAAttemptLockoutWindow = 15 * time.Minute

// mfaAttemptLimiter is the consumer-side interface AuthService depends on for the per-account
// failed-MFA-code throttle. Injectable (WithMFAAttemptLimiter) so tests can swap in a
// controllable-clock instance, or a future Redis-backed limiter for horizontal-scale
// deployments (mirrors the in-memory-now/Redis-later note in auth_ratelimit.go).
type mfaAttemptLimiter interface {
	// Allow reports whether tenantID/userID is currently UNDER the failed-attempt threshold.
	// Call BEFORE attempting to verify a presented code — a caller already over the threshold
	// must be rejected WITHOUT the code ever being checked (fail-secure, ACL-before-business-
	// logic discipline).
	Allow(tenantID, userID string) bool
	// RecordFailure registers one more failed verify attempt for tenantID/userID. Call ONLY
	// when a presented code OR (MFA-STEPUP) the step-up account password was genuinely wrong —
	// both share this one counter — never for an unrelated state error like "not enrolled" or
	// "already enabled".
	RecordFailure(tenantID, userID string)
	// Reset clears the failure count for tenantID/userID. Call on a SUCCESSFUL verify so a
	// legitimate user who fat-fingered a couple of codes is not left sitting near the
	// threshold for the rest of the window.
	Reset(tenantID, userID string)
}

// mfaAttemptEntry tracks recent failure timestamps for a single (tenantID, userID) pair.
type mfaAttemptEntry struct {
	mu         sync.Mutex
	timestamps []time.Time // kept sorted ascending; pruned on each Allow/RecordFailure call
}

// mfaMemAttemptLimiter is a thread-safe, in-memory sliding-window implementation of
// mfaAttemptLimiter. It is bounded by a background janitor goroutine that evicts entries idle
// for more than 2x the window, exactly like middleware.memRateLimiter, so the map cannot grow
// without bound under sustained attack.
type mfaMemAttemptLimiter struct {
	mu          sync.Mutex
	entries     map[string]*mfaAttemptEntry
	maxFailures int
	window      time.Duration
	now         func() time.Time // injectable clock; defaults to time.Now (tests override directly)
}

// newMFAMemAttemptLimiter constructs a ready-to-use limiter and starts its janitor goroutine.
func newMFAMemAttemptLimiter(maxFailures int, window time.Duration) *mfaMemAttemptLimiter {
	lim := &mfaMemAttemptLimiter{
		entries:     make(map[string]*mfaAttemptEntry),
		maxFailures: maxFailures,
		window:      window,
		now:         time.Now,
	}
	go lim.janitor()
	return lim
}

func mfaAttemptKey(tenantID, userID string) string { return tenantID + "\x00" + userID }

// Allow reports whether the pair is currently under the failure threshold.
func (l *mfaMemAttemptLimiter) Allow(tenantID, userID string) bool {
	entry := l.getOrCreate(mfaAttemptKey(tenantID, userID))
	entry.mu.Lock()
	defer entry.mu.Unlock()
	l.pruneLocked(entry)
	return len(entry.timestamps) < l.maxFailures
}

// RecordFailure appends one failure timestamp for the pair.
func (l *mfaMemAttemptLimiter) RecordFailure(tenantID, userID string) {
	entry := l.getOrCreate(mfaAttemptKey(tenantID, userID))
	entry.mu.Lock()
	defer entry.mu.Unlock()
	l.pruneLocked(entry)
	entry.timestamps = append(entry.timestamps, l.now())
}

// Reset clears every recorded failure for the pair.
func (l *mfaMemAttemptLimiter) Reset(tenantID, userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, mfaAttemptKey(tenantID, userID))
}

// pruneLocked drops timestamps outside the current sliding window. Caller must hold entry.mu.
func (l *mfaMemAttemptLimiter) pruneLocked(entry *mfaAttemptEntry) {
	cutoff := l.now().Add(-l.window)
	valid := entry.timestamps[:0]
	for _, ts := range entry.timestamps {
		if ts.After(cutoff) {
			valid = append(valid, ts)
		}
	}
	entry.timestamps = valid
}

func (l *mfaMemAttemptLimiter) getOrCreate(key string) *mfaAttemptEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		e = &mfaAttemptEntry{}
		l.entries[key] = e
	}
	return e
}

// janitor evicts entries idle for more than 2x the window, bounding memory under sustained
// attack. Runs for the lifetime of the process, exactly like middleware.memRateLimiter's.
func (l *mfaMemAttemptLimiter) janitor() {
	ticker := time.NewTicker(l.window)
	defer ticker.Stop()
	for range ticker.C {
		l.evictStale()
	}
}

func (l *mfaMemAttemptLimiter) evictStale() {
	cutoff := l.now().Add(-2 * l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, entry := range l.entries {
		entry.mu.Lock()
		stale := len(entry.timestamps) == 0 || entry.timestamps[len(entry.timestamps)-1].Before(cutoff)
		entry.mu.Unlock()
		if stale {
			delete(l.entries, key)
		}
	}
}
