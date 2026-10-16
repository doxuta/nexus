package services

import (
	"context"
	"fmt"
	"time"

	"nexus/platform/internal/cache"
	"nexus/platform/internal/metadata"
)

// entityKey / fieldsKey are the TENANT-SCOPED cache keys (O3 discipline: metadata is on a shared
// multi-tenant table, so a key without the tenant could serve one tenant's definition to another).
type entityKey struct {
	tenant string
	name   string
}
type fieldsKey struct {
	tenant   string
	entityID string
}

// MetaCache wraps an EntityCatalog with an in-process TTL + single-flight cache for the entity and
// field metadata that RecordService.resolve reads on EVERY dynamic-record request (O3 Tier-1, see
// docs/_planner/search-cache-longterm.md). It implements EntityCatalog, so it drops into
// NewRecordService in place of the raw metadata repository with no call-site change. Metadata
// mutations invalidate via InvalidateEntityDef / InvalidateFields, called by the entity/field write
// services AFTER their DB write commits. Tier-2 (L1 in-proc + L2 Redis pub/sub) replaces only this
// type's backing store; resolve and the write hooks are unchanged.
//
// Cached values are treated as READ-ONLY: a cached *EntityDefinition / []FieldDefinition is shared
// across concurrent requests, so callers must never mutate them (resolve only reads e.ID/e.Name and
// feeds the fields to toFieldSpecs, which builds new structs). A not-found is returned as
// ErrEntityNotFound and NOT cached (errors are never cached), so a later create is seen immediately.
type MetaCache struct {
	inner    EntityCatalog
	entities *cache.TTLCache[entityKey, *metadata.EntityDefinition]
	fields   *cache.TTLCache[fieldsKey, []metadata.FieldDefinition]
}

// DefaultMetaTTL is the fail-safe expiry for metadata cache entries. Metadata changes rarely and a
// few seconds of staleness on a field definition is a UX glitch, not a breach, so this can be
// generous; invalidation-on-write is the primary freshness mechanism, TTL is the backstop.
const DefaultMetaTTL = 60 * time.Second

// NewMetaCache wraps inner (the real metadata repository) with caches expiring after ttl.
func NewMetaCache(inner EntityCatalog, ttl time.Duration) *MetaCache {
	return &MetaCache{
		inner:    inner,
		entities: cache.New[entityKey, *metadata.EntityDefinition](ttl),
		fields:   cache.New[fieldsKey, []metadata.FieldDefinition](ttl),
	}
}

// GetEntityByName serves the cached entity definition for (tenant, name), loading via the inner
// repository on a miss (single-flight). ErrEntityNotFound is propagated and not cached.
func (m *MetaCache) GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	return m.entities.GetOrLoad(entityKey{tenantID, name}, func() (*metadata.EntityDefinition, error) {
		return m.inner.GetEntityByName(ctx, tenantID, name)
	})
}

// ListFields serves the cached field definitions for (tenant, entityID), loading on a miss.
func (m *MetaCache) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	return m.fields.GetOrLoad(fieldsKey{tenantID, entityID}, func() ([]metadata.FieldDefinition, error) {
		return m.inner.ListFields(ctx, tenantID, entityID)
	})
}

// ListFieldsIncludingDeleted forwards to the inner repository WITHOUT caching, so RecordService
// can see soft-deleted field definitions when it redacts a delete/restore audit diff against the
// raw stored blob (services.redactionSpecs). Deliberately uncached: it would need its own key
// (the result is a superset of the ListFields entry and must never be served in its place), it
// runs only on delete/restore rather than on every record read, and a stale answer here means a
// sensitive value escapes redaction. The method exists so the production catalog — a MetaCache
// wrapping the repository — still satisfies the deletedFieldLister capability; without this
// forward, the assertion would fail and every delete would quietly take the incomplete path.
func (m *MetaCache) ListFieldsIncludingDeleted(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	type deletedLister interface {
		ListFieldsIncludingDeleted(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
	}
	inner, ok := m.inner.(deletedLister)
	if !ok {
		return nil, fmt.Errorf("services.MetaCache.ListFieldsIncludingDeleted: inner catalog lacks the capability")
	}
	return inner.ListFieldsIncludingDeleted(ctx, tenantID, entityID)
}

// InvalidateEntityDef drops the cached entity definition for (tenant, name) after a create / update
// / soft-delete of the ENTITY. Call AFTER the DB write commits.
func (m *MetaCache) InvalidateEntityDef(tenantID, entityName string) {
	if m == nil || entityName == "" {
		return
	}
	m.entities.Invalidate(entityKey{tenantID, entityName})
}

// InvalidateFields drops the cached field list for (tenant, entityID) after a field add / edit /
// delete (and on entity soft-delete, alongside InvalidateEntityDef). Call AFTER the DB write commits.
func (m *MetaCache) InvalidateFields(tenantID, entityID string) {
	if m == nil || entityID == "" {
		return
	}
	m.fields.Invalidate(fieldsKey{tenantID, entityID})
}

// InvalidateAll drops every cached entity definition and field list (all tenants). Used by the
// Nexus Doctor after a fix batch: a repair batch may touch several definitions at once and the
// doctor should never leave a admin wondering whether a fix "took", so the blanket flush (cheap —
// the cache repopulates on demand; TTL backstop is 60s anyway) is the safe move. Nil-safe like
// the targeted invalidators. Per-key invalidation remains the right tool for ordinary writes.
func (m *MetaCache) InvalidateAll() {
	if m == nil {
		return
	}
	m.entities.Clear()
	m.fields.Clear()
}
