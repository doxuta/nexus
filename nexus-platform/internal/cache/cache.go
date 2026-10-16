// Package cache is the Nexus in-process cache layer for read-mostly, rarely-mutated data —
// entity/field metadata and ACL role grants — that is otherwise re-queried from MySQL on EVERY
// dynamic-record request (O3). It is the Tier-1 implementation of the staged plan in
// docs/_planner/search-cache-longterm.md: a single generic cache primitive behind which callers
// use GetOrLoad/Invalidate, so a later swap to an L1-in-proc + L2-Redis near-cache changes only
// this package's internals and the cmd/server wiring — never the call sites.
//
// Discipline this package enforces for its callers:
//   - Keys MUST be TENANT-SCOPED (the K type carries the tenant) — an ACL grant keyed by roleID
//     alone would let a forged (tenantA, roleFromB) request hit tenantB's entry and bypass the
//     data-layer forge-tenant guard. Tenant-scoped keys preserve default-deny.
//   - Invalidate is called AFTER the DB write commits (publish-after-commit): invalidating before
//     commit lets a concurrent read repopulate the entry with the pre-commit value.
//   - ACL caches use a SHORT fail-safe TTL (the backstop for a missed invalidation of a security
//     decision); metadata caches may use a longer TTL.
package cache

import (
	"sync"
	"time"
)

// TTLCache is a concurrency-safe in-process cache with per-entry TTL and single-flight load
// coalescing. K must be comparable (use a small tenant-scoped key struct); V is the cached value.
// The zero value is not usable — construct with New.
//
// Correctness under concurrent invalidation: a load that is in flight when Invalidate (or Clear)
// runs will NOT cache its now-possibly-stale result. This is enforced by a monotonically-increasing
// generation that Invalidate/Clear bump and GetOrLoad samples before loading — so a revoke that
// races a concurrent authorize-load can never re-cache the old (allowed) grants. The cost is mild
// over-invalidation (any concurrent invalidation drops the in-flight load's caching), which is fine
// because invalidations only happen on rare metadata/ACL writes.
type TTLCache[K comparable, V any] struct {
	ttl   time.Duration
	mu    sync.Mutex
	items map[K]entry[V]
	calls map[K]*call[V] // in-flight loads, keyed for single-flight coalescing
	gen   uint64         // bumped by every Invalidate/Clear; in-flight loads sampling an older gen do not cache
}

type entry[V any] struct {
	val V
	exp time.Time
}

type call[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

// New builds a cache whose entries live for ttl. A ttl <= 0 disables time-based expiry (entries
// live until invalidated) — valid, but callers should prefer a positive ttl as the fail-safe.
func New[K comparable, V any](ttl time.Duration) *TTLCache[K, V] {
	return &TTLCache[K, V]{
		ttl:   ttl,
		items: make(map[K]entry[V]),
		calls: make(map[K]*call[V]),
	}
}

// GetOrLoad returns the cached value for key when fresh; otherwise it runs loader exactly once
// across all concurrent callers for that key (single-flight) and caches the result for ttl. A
// loader error is returned to every coalesced caller and is NOT cached (so a transient DB fault
// does not pin a failure). If the cache is invalidated while loader runs, the result is returned
// to callers but not cached.
func (c *TTLCache[K, V]) GetOrLoad(key K, loader func() (V, error)) (V, error) {
	c.mu.Lock()
	if e, ok := c.items[key]; ok && (c.ttl <= 0 || time.Now().Before(e.exp)) {
		c.mu.Unlock()
		return e.val, nil
	}
	if cl, ok := c.calls[key]; ok {
		// A load for this key is already in flight — wait for it instead of stampeding the DB.
		c.mu.Unlock()
		cl.wg.Wait()
		return cl.val, cl.err
	}
	cl := &call[V]{}
	cl.wg.Add(1)
	c.calls[key] = cl
	startGen := c.gen
	c.mu.Unlock()

	cl.val, cl.err = loader()

	c.mu.Lock()
	delete(c.calls, key)
	// Cache only a successful load that was NOT invalidated mid-flight (generation unchanged).
	if cl.err == nil && c.gen == startGen {
		c.items[key] = entry[V]{val: cl.val, exp: time.Now().Add(c.ttl)}
	}
	c.mu.Unlock()
	cl.wg.Done()
	return cl.val, cl.err
}

// Invalidate removes the given keys immediately (hard invalidation on write) and cancels caching
// of any in-flight load. Safe to call for absent keys and safe for concurrent use. Call it AFTER
// the DB write commits.
func (c *TTLCache[K, V]) Invalidate(keys ...K) {
	c.mu.Lock()
	c.gen++
	for _, k := range keys {
		delete(c.items, k)
	}
	c.mu.Unlock()
}

// Clear drops every entry (e.g. a bulk metadata reload) and cancels in-flight caching.
func (c *TTLCache[K, V]) Clear() {
	c.mu.Lock()
	c.gen++
	c.items = make(map[K]entry[V])
	c.mu.Unlock()
}

// Len reports the number of cached (not necessarily unexpired) entries — for tests/metrics.
func (c *TTLCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
