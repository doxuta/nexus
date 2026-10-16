package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/metadata"
)

// countingCatalog is a fake EntityCatalog that counts how many times each method actually hits the
// "DB", so a test can assert the cache absorbed repeated reads.
type countingCatalog struct {
	entityCalls int64
	fieldCalls  int64
}

func (c *countingCatalog) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	atomic.AddInt64(&c.entityCalls, 1)
	return &metadata.EntityDefinition{ID: tenantID + ":" + name, TenantID: tenantID, Name: name}, nil
}

func (c *countingCatalog) ListFields(_ context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	atomic.AddInt64(&c.fieldCalls, 1)
	return []metadata.FieldDefinition{{ID: entityID + ":f1", TenantID: tenantID, EntityID: entityID}}, nil
}

func TestMetaCache_HitAndInvalidate(t *testing.T) {
	inner := &countingCatalog{}
	mc := NewMetaCache(inner, time.Minute)
	ctx := context.Background()

	// Two reads of the same (tenant, name) → one DB hit.
	for i := 0; i < 2; i++ {
		if _, err := mc.GetEntityByName(ctx, "t1", "accounts"); err != nil {
			t.Fatal(err)
		}
		if _, err := mc.ListFields(ctx, "t1", "t1:accounts"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&inner.entityCalls); got != 1 {
		t.Errorf("GetEntityByName hit DB %d times; want 1 (cached)", got)
	}
	if got := atomic.LoadInt64(&inner.fieldCalls); got != 1 {
		t.Errorf("ListFields hit DB %d times; want 1 (cached)", got)
	}

	// Invalidate the field list (simulating a field write) → next ListFields reloads.
	mc.InvalidateFields("t1", "t1:accounts")
	if _, err := mc.ListFields(ctx, "t1", "t1:accounts"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&inner.fieldCalls); got != 2 {
		t.Errorf("after InvalidateFields, ListFields hit DB %d times; want 2 (reload)", got)
	}

	// Invalidate the entity def (simulating an entity update) → next GetEntityByName reloads.
	mc.InvalidateEntityDef("t1", "accounts")
	if _, err := mc.GetEntityByName(ctx, "t1", "accounts"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&inner.entityCalls); got != 2 {
		t.Errorf("after InvalidateEntityDef, GetEntityByName hit DB %d times; want 2", got)
	}
}

// TestMetaCache_TenantScoped is the isolation guarantee: the same entity NAME under two tenants are
// distinct cache keys, so one tenant's cached definition is NEVER served to another.
func TestMetaCache_TenantScoped(t *testing.T) {
	inner := &countingCatalog{}
	mc := NewMetaCache(inner, time.Minute)
	ctx := context.Background()

	a, _ := mc.GetEntityByName(ctx, "tenantA", "accounts")
	b, _ := mc.GetEntityByName(ctx, "tenantB", "accounts")
	if a.TenantID != "tenantA" || b.TenantID != "tenantB" {
		t.Fatalf("cross-tenant serve: a.tenant=%q b.tenant=%q", a.TenantID, b.TenantID)
	}
	// Two distinct tenants → two DB loads (no cross-tenant cache hit).
	if got := atomic.LoadInt64(&inner.entityCalls); got != 2 {
		t.Errorf("entity loads = %d; want 2 (tenant-scoped keys, no cross-tenant hit)", got)
	}
	// Re-read tenantA → cached (still 2 total, not 3).
	mc.GetEntityByName(ctx, "tenantA", "accounts")
	if got := atomic.LoadInt64(&inner.entityCalls); got != 2 {
		t.Errorf("entity loads = %d; want 2 (tenantA re-read cached)", got)
	}

	// Invalidating tenantA must NOT drop tenantB's entry.
	mc.InvalidateEntityDef("tenantA", "accounts")
	mc.GetEntityByName(ctx, "tenantB", "accounts") // still cached
	if got := atomic.LoadInt64(&inner.entityCalls); got != 2 {
		t.Errorf("entity loads = %d; want 2 (tenantB unaffected by tenantA invalidation)", got)
	}
}

// TestMetaCache_NotFoundNotCached confirms a not-found is not cached, so a create is seen at once.
func TestMetaCache_NotFoundNotCached(t *testing.T) {
	var calls int64
	inner := catalogFunc{
		entity: func(_ context.Context, _, _ string) (*metadata.EntityDefinition, error) {
			if atomic.AddInt64(&calls, 1) == 1 {
				return nil, metadata.ErrEntityNotFound
			}
			return &metadata.EntityDefinition{Name: "now-exists"}, nil
		},
	}
	mc := NewMetaCache(inner, time.Minute)
	if _, err := mc.GetEntityByName(context.Background(), "t", "x"); err != metadata.ErrEntityNotFound {
		t.Fatalf("first = %v; want ErrEntityNotFound", err)
	}
	e, err := mc.GetEntityByName(context.Background(), "t", "x") // not-found not cached → reload finds it
	if err != nil || e.Name != "now-exists" {
		t.Fatalf("second = (%v,%v); want the now-created entity (not a cached not-found)", e, err)
	}
}

// catalogFunc adapts plain functions to EntityCatalog for the not-found test.
type catalogFunc struct {
	entity func(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
}

func (c catalogFunc) GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	return c.entity(ctx, tenantID, name)
}
func (c catalogFunc) ListFields(_ context.Context, _, _ string) ([]metadata.FieldDefinition, error) {
	return nil, nil
}
