package services

import (
	"context"
	"fmt"
	"time"

	"nexus/platform/internal/metaversion"
)

// cutoffReader is the optional second capability of the version store: the newest numbered version's
// schema_cutoff_at. Satisfied by *metaversion.VersionRepo; a fake that only answers HasNumbered leaves
// the schema-hiding off (no cutoff ever).
type cutoffReader interface {
	HeadCutoff(ctx context.Context, tenantID string) (*time.Time, error)
}

// Compile-time proof the production store still answers HeadCutoff: the assertion in DraftGate.HeadCutoff
// would otherwise fall back to "hide nothing" silently if the signature drifted.
var _ cutoffReader = (*metaversion.VersionRepo)(nil)

// cutoffTTL is how long one tenant's cutoff is served from memory (SC-008: at most one query per
// tenant per 5 seconds on the customer's hot path). A publish refreshes it in place on its own process.
const cutoffTTL = 5 * time.Second

type cutoffEntry struct {
	at      *time.Time // nil: never delivered / no cutoff
	expires time.Time
}

// HeadCutoff returns the instant of the last publish for the tenant, cut to the second (the
// created_at columns of sys_fields / sys_entities / sys_relationships have no fraction), nil when the
// tenant was never delivered. Cached for cutoffTTL.
func (g *DraftGate) HeadCutoff(ctx context.Context, tenantID string) (*time.Time, error) {
	cr, ok := g.versions.(cutoffReader)
	if !ok {
		return nil, nil
	}
	now := g.now()
	if v, hit := g.cutoffs.Load(tenantID); hit {
		if e := v.(cutoffEntry); now.Before(e.expires) {
			return e.at, nil
		}
	}
	at, err := cr.HeadCutoff(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DraftGate.HeadCutoff: %w", err)
	}
	if at != nil {
		t := at.Truncate(time.Second)
		at = &t
	}
	g.cutoffs.Store(tenantID, cutoffEntry{at: at, expires: now.Add(cutoffTTL)})
	return at, nil
}

// RefreshCutoff sets the cached cutoff right after a publish / baseline commits, so this process stops
// hiding what the publish just released without waiting out the TTL. It never writes the DB.
func (g *DraftGate) RefreshCutoff(tenantID string, at time.Time) {
	t := at.Truncate(time.Second)
	g.cutoffs.Store(tenantID, cutoffEntry{at: &t, expires: g.now().Add(cutoffTTL)})
}

// SchemaCutoff answers, for a reader of the tenant's schema: "hide what was created after this
// instant" (apply=true) or "show everything" (apply=false). The builder's draft-overlay reader and a
// never-delivered tenant see everything (S7-8, FR-060).
func (g *DraftGate) SchemaCutoff(ctx context.Context, tenantID string) (cutoff time.Time, apply bool, err error) {
	overlay, err := g.ReadOverlay(ctx, tenantID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("services.DraftGate.SchemaCutoff: %w", err)
	}
	if overlay {
		return time.Time{}, false, nil
	}
	at, err := g.HeadCutoff(ctx, tenantID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("services.DraftGate.SchemaCutoff: %w", err)
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return *at, true, nil
}

func (g *DraftGate) now() time.Time {
	if g.clock != nil {
		return g.clock()
	}
	return time.Now()
}

// schemaCutoffer is what a reader of the schema (layout service, metadata handler) asks to know
// which fields / tables / relations are not yet the customer's. *DraftGate satisfies it.
type schemaCutoffer interface {
	SchemaCutoff(ctx context.Context, tenantID string) (time.Time, bool, error)
}
