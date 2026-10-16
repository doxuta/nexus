package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"nexus/platform/internal/metaversion"
)

// deliveredReader answers "does the tenant have a numbered version?". Satisfied by
// *metaversion.VersionRepo.
type deliveredReader interface {
	HasNumbered(ctx context.Context, tenantID string) (bool, error)
}

// DraftGate is the ONE place that decides how a configuration write lands on a tenant and whether a
// read overlays the builder's drafts (specs/063-draft-publish FR-002a, FR-004, FR-005, FR-012). The
// wrapped repositories ask it; handlers and service bodies stay unaware.
type DraftGate struct {
	versions deliveredReader
	// delivered caches only `true`: delivery is one-way, so a positive answer is good for the life
	// of the process, while `false` is re-asked (another replica may have delivered the tenant).
	delivered sync.Map
	// cutoffs: tenant -> cutoffEntry (S7-8), 5-second cache of the head version's schema cutoff.
	cutoffs sync.Map
	clock   func() time.Time // tests only
}

// NewDraftGate wires the gate over the version store.
func NewDraftGate(v deliveredReader) *DraftGate { return &DraftGate{versions: v} }

// Delivered reports whether the tenant has been delivered (has a numbered version).
func (g *DraftGate) Delivered(ctx context.Context, tenantID string) (bool, error) {
	if _, ok := g.delivered.Load(tenantID); ok {
		return true, nil
	}
	ok, err := g.versions.HasNumbered(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("services.DraftGate.Delivered: %w", err)
	}
	if ok {
		g.delivered.Store(tenantID, struct{}{})
	}
	return ok, nil
}

// CacheDelivered records, in this process only, that the tenant was just delivered (after the
// baseline commits). It never writes the DB — the numbered version is the only source of truth.
func (g *DraftGate) CacheDelivered(tenantID string) { g.delivered.Store(tenantID, struct{}{}) }

// WriteMode says how a write to the tenant's configuration documents must land:
//
//   - ModeLive: the tenant is not delivered, or ctx is PublishService applying a publish;
//   - ModeDraft: delivered + the caller is the builder acting on their own tenant;
//   - ModeLiveDirect: delivered + a non-builder (the legacy customer admin) on their own tenant;
//   - metaversion.ErrNoCaller: delivered but ctx never passed DraftCaller — fail CLOSED, never a
//     silent write to the customer's running app;
//   - metaversion.ErrCrossTenantDraftWrite: delivered tenant other than the caller's token tenant.
func (g *DraftGate) WriteMode(ctx context.Context, tenantID string) (metaversion.Mode, error) {
	if metaversion.IsPublishApply(ctx) {
		return metaversion.ModeLive, nil
	}
	delivered, err := g.Delivered(ctx, tenantID)
	if err != nil {
		return metaversion.ModeLive, err
	}
	if !delivered {
		return metaversion.ModeLive, nil
	}
	caller, ok := metaversion.CallerFrom(ctx)
	if !ok {
		return metaversion.ModeLive, fmt.Errorf("services.DraftGate.WriteMode: tenant %s: %w", tenantID, metaversion.ErrNoCaller)
	}
	if tenantID != caller.TenantID {
		return metaversion.ModeLive, fmt.Errorf("services.DraftGate.WriteMode: tenant %s, caller tenant %s: %w",
			tenantID, caller.TenantID, metaversion.ErrCrossTenantDraftWrite)
	}
	if caller.IsBuilder {
		return metaversion.ModeDraft, nil
	}
	return metaversion.ModeLiveDirect, nil
}

// ReadOverlay reports whether a read of the tenant's documents must overlay the builder's drafts:
// the caller asked for it (ReadDraft), nothing forces a live read (WithLiveRead), the tenant is the
// caller's own and it is delivered. A request that does not ask costs ZERO queries (the customer's
// hot path); a cross-tenant read (?targetTenant=) always reads live.
func (g *DraftGate) ReadOverlay(ctx context.Context, tenantID string) (bool, error) {
	caller, ok := metaversion.CallerFrom(ctx)
	if !ok || !caller.ReadDraft || metaversion.IsLiveRead(ctx) || tenantID != caller.TenantID {
		return false, nil
	}
	return g.Delivered(ctx, tenantID)
}
