package services

import (
	"context"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/cache"
)

// aclKey is the TENANT-SCOPED cache key for a role's grants. Keying by roleID ALONE would be a
// cross-tenant leak: LoadActionsForACL pins the tenant via the acl_roles JOIN (the D4 forge-tenant
// guard), so a forged (tenantA, roleFromB) currently loads zero grants → default-deny; a role-only
// cache entry populated under tenant B could then be HIT by the forged request and bypass that
// guard. (tenant, role) keying preserves default-deny.
type aclKey struct {
	tenant string
	role   string
}

// DefaultACLTTL is the SHORT fail-safe expiry for cached ACL grants. Unlike metadata (a stale field
// def is a UX glitch), a stale ACL grant is a SECURITY hole — a revoked role keeping access — so the
// TTL is the *backstop* bound on a missed invalidation, kept short. Hard invalidation on every role
// write (Invalidate, immediate) is the PRIMARY freshness mechanism; the TTL only covers the gap if a
// future distributed (Redis pub/sub) invalidation message is ever dropped (Tier-2). 30s is the
// research-recommended ACL backstop (search-cache-longterm.md).
const DefaultACLTTL = 30 * time.Second

// CachedACLLoader caches a role's (entity, action) grants in-process so ACLService.Authorize stops
// re-querying acl_role_actions on every non-admin request (O3 Tier-1). It implements aclActionLoader,
// so it drops into NewACLService in place of the raw role repository. RoleService invalidates a
// role's entry after a create/update/delete commit (revocation must not lag). A load error is NOT
// cached (Authorize fails closed, so a transient DB fault must not pin a deny either). Cached grants
// are treated READ-ONLY (Authorize only reads Action/EntityName/AccessLevel).
type CachedACLLoader struct {
	inner aclActionLoader
	c     *cache.TTLCache[aclKey, []auth.RoleAction]
}

// NewCachedACLLoader wraps inner (the real role repository) with a grants cache expiring after ttl.
func NewCachedACLLoader(inner aclActionLoader, ttl time.Duration) *CachedACLLoader {
	return &CachedACLLoader{inner: inner, c: cache.New[aclKey, []auth.RoleAction](ttl)}
}

// LoadActionsForACL serves the cached grants for (tenant, role), loading on a miss (single-flight).
func (l *CachedACLLoader) LoadActionsForACL(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	return l.c.GetOrLoad(aclKey{tenantID, roleID}, func() ([]auth.RoleAction, error) {
		return l.inner.LoadActionsForACL(ctx, tenantID, roleID)
	})
}

// Invalidate drops a role's cached grants AFTER a role create/update/delete commit so a changed or
// revoked grant takes effect on the very next authorize. Nil-safe and safe for an absent key.
func (l *CachedACLLoader) Invalidate(tenantID, roleID string) {
	if l == nil || roleID == "" {
		return
	}
	l.c.Invalidate(aclKey{tenantID, roleID})
}

// InvalidateAll drops every cached grant list (all tenants/roles) and cancels in-flight
// caching. Used by the Nexus Doctor flush-caches action (DOCTOR-OPS). Dropping cached grants
// is always SAFE for security (the next authorize reloads from the DB — fail toward fresh
// truth, never toward a stale allow). Nil-safe like Invalidate.
func (l *CachedACLLoader) InvalidateAll() {
	if l == nil {
		return
	}
	l.c.Clear()
}

// Ensure CachedACLLoader satisfies the loader interface ACLService depends on.
var _ aclActionLoader = (*CachedACLLoader)(nil)
