package services

import (
	"context"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/cache"
)

// userRowKey is the TENANT-SCOPED cache key for a user's live row. Keying by user id alone would let
// a forged (tenantA, userFromB) token be served tenant B's cached row; (tenant, user) keeps the
// lookup pinned exactly as the repository query is.
type userRowKey struct {
	tenant string
	user   string
}

// DefaultUserClaimTTL is the SHORT fail-safe expiry for a cached user row on the auth stage. The
// PRIMARY freshness mechanism is hard invalidation from UserService on every committed user
// mutation (create / update / delete — the same three sites that emit __user__ audit rows), so a
// demotion, role change, disable or delete lands on the user's very next request. The TTL only
// bounds a MISSED invalidation — a second replica that never saw the write (invalidation is
// in-process, like the ACL cache) — and is deliberately the same 30 s as DefaultACLTTL: a stale
// row here IS a stale security decision.
const DefaultUserClaimTTL = DefaultACLTTL

// userLoadTimeout bounds one detached row load (see GetUserByID): the loader runs single-flight
// for every coalesced caller, so it is cut from the first caller's cancellation and needs its own
// ceiling instead.
const userLoadTimeout = 5 * time.Second

// userRowReader is the subset of the user repository the cache wraps (tenant-pinned point read).
type userRowReader interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
}

// CachedUserLoader caches a user's row in-process so middleware.AuthMiddlewareLive can rebuild the
// request Subject from the database on EVERY protected request without a point-read per request
// (queue #14 claim-freshness). It satisfies middleware.LiveUserReader. Rows are cached BY VALUE and
// every read returns its own copy, so a consumer cannot poison the entry's VALUE fields through
// the returned pointer; the pointer fields (RoleID, LockedUntil) still share their pointees with
// the cached row — read them, never write through them. PasswordHash is stripped before caching:
// the auth stage reads status / is_admin / role_id only, and a hash has no business in a
// long-lived in-process cache.
// A load error — including auth.ErrUserNotFound for a soft-deleted user — is returned as-is and
// NOT cached (the next read asks the DB again; a deleted user's client redirects to /login on the
// first 401, so the DB is not hammered).
//
// ponytail: invalidation is process-local (no Redis pub/sub); on a multi-replica deployment the
// other replicas converge within DefaultUserClaimTTL. Upgrade path = the Tier-2 near-cache
// bus noted in cache.go, shared with the ACL cache.
type CachedUserLoader struct {
	inner userRowReader
	c     *cache.TTLCache[userRowKey, auth.User]
}

// NewCachedUserLoader wraps inner (the real user repository) with a row cache expiring after ttl.
func NewCachedUserLoader(inner userRowReader, ttl time.Duration) *CachedUserLoader {
	return &CachedUserLoader{inner: inner, c: cache.New[userRowKey, auth.User](ttl)}
}

// GetUserByID serves the cached row for (tenant, id), loading on a miss (single-flight). The
// returned pointer is a fresh copy per call.
func (l *CachedUserLoader) GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error) {
	u, err := l.c.GetOrLoad(userRowKey{tenantID, id}, func() (auth.User, error) {
		// Single-flight: this closure answers EVERY coalesced caller, so it must not inherit the
		// first caller's request context — a client that disconnects mid-load would otherwise
		// turn its siblings' answers into a fail-closed 500. Detached, with its own timeout.
		lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userLoadTimeout)
		defer cancel()
		row, err := l.inner.GetUserByID(lctx, tenantID, id)
		if err != nil {
			return auth.User{}, err
		}
		row.PasswordHash = ""
		return *row, nil
	})
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// Invalidate drops a user's cached row AFTER a user create/update/delete commit so the change
// is seen on that user's very next request. Nil-safe and safe for an absent key.
func (l *CachedUserLoader) Invalidate(tenantID, userID string) {
	if l == nil || userID == "" {
		return
	}
	l.c.Invalidate(userRowKey{tenantID, userID})
}

// InvalidateAll drops every cached row (all tenants) and cancels in-flight caching. Always safe
// for security (the next request reloads from the DB). Nil-safe like Invalidate.
func (l *CachedUserLoader) InvalidateAll() {
	if l == nil {
		return
	}
	l.c.Clear()
}
