package workflow

import (
	"context"
	"time"

	"nexus/platform/internal/cache"
)

// WorkflowLoader is the engine's consumer-side view of the workflow store's hot path: load the
// active rules of (tenant, entity, trigger). Satisfied by *SQLRepository.ListActive directly and
// by *CachedLoader (the TTL-cached wrapper production uses). Declared here so the dispatcher
// depends on the interface, not a concrete type — unit tests inject a fake.
type WorkflowLoader interface {
	ListActive(ctx context.Context, tenantID, entityName string, trigger Trigger) ([]Workflow, error)
}

// wfKey is the TENANT-SCOPED cache key (O3 discipline: sys_workflows is a shared multi-tenant
// table, so a key without the tenant could serve one tenant's rules to another — a tenant leak
// AND a security hole, since a workflow can mutate records and notify users).
type wfKey struct {
	tenant  string
	entity  string
	trigger Trigger
}

// CachedLoader wraps a WorkflowLoader with an in-process TTL + single-flight cache for the active
// workflows the Event Engine reads after EVERY record mutation (O3 Tier-1, same pattern as
// MetaCache / CachedACLLoader). Workflow definitions change rarely (an admin edits a rule), so a
// few seconds of staleness is acceptable; the admin write services call Invalidate AFTER their DB
// write commits so a changed/disabled/deleted rule takes effect promptly. A later L2 (Redis)
// near-cache swaps only this type's backing store — the dispatcher call site is unchanged.
//
// Cached slices are treated as READ-ONLY: a cached []Workflow is shared across concurrent
// mutations, so the dispatcher only reads it (Evaluate is pure; runActions reads wf fields).
type CachedLoader struct {
	inner WorkflowLoader
	cache *cache.TTLCache[wfKey, []Workflow]
}

// DefaultWorkflowTTL is the fail-safe expiry for cached active-workflow lists. Invalidation on
// write is the primary freshness mechanism; the TTL is the backstop. Kept modest (workflows can
// gate notifications/field mutations, so stale-but-bounded is safer than long-lived).
const DefaultWorkflowTTL = 30 * time.Second

// NewCachedLoader wraps inner (the SQL repository) with a cache expiring after ttl.
func NewCachedLoader(inner WorkflowLoader, ttl time.Duration) *CachedLoader {
	return &CachedLoader{
		inner: inner,
		cache: cache.New[wfKey, []Workflow](ttl),
	}
}

// ListActive serves the cached active-workflow list for (tenant, entity, trigger), loading via the
// inner repository on a miss (single-flight). An error is propagated and not cached.
func (l *CachedLoader) ListActive(ctx context.Context, tenantID, entityName string, trigger Trigger) ([]Workflow, error) {
	return l.cache.GetOrLoad(wfKey{tenantID, entityName, trigger}, func() ([]Workflow, error) {
		return l.inner.ListActive(ctx, tenantID, entityName, trigger)
	})
}

// Invalidate drops the cached lists for an entity's event-driven triggers after a workflow
// create / update / delete in that (tenant, entity). Call AFTER the DB write commits. Because a
// single rule edit can change any trigger bucket (e.g. the admin re-pointed a rule from
// after_create to date_offset), all keys are invalidated together (date_offset is read through
// this cache too — the dispatcher's timer-scheduling pass; before_save/before_delete by
// RecordService, specs/048 — a before rule switched off must stop vetoing on the next save, not
// 30 s later). A nil receiver / empty entity is a safe no-op (the write services call this
// unconditionally).
func (l *CachedLoader) Invalidate(tenantID, entityName string) {
	if l == nil || entityName == "" {
		return
	}
	l.cache.Invalidate(
		wfKey{tenantID, entityName, TriggerAfterCreate},
		wfKey{tenantID, entityName, TriggerAfterUpdate},
		wfKey{tenantID, entityName, TriggerAfterDelete},
		wfKey{tenantID, entityName, TriggerDateOffset},
		wfKey{tenantID, entityName, TriggerBeforeSave},
		wfKey{tenantID, entityName, TriggerBeforeDelete},
	)
}

// InvalidateAll drops every cached rule list (all tenants) and cancels in-flight caching.
// Used by the Nexus Doctor flush-caches action (DOCTOR-OPS): a blanket flush is the honest
// escape hatch when an admin suspects cache-vs-DB drift — cheap, the cache repopulates on
// demand and the TTL backstop is 30s anyway. Nil-safe like Invalidate.
func (l *CachedLoader) InvalidateAll() {
	if l == nil {
		return
	}
	l.cache.Clear()
}
