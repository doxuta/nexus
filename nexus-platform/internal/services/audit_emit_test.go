package services

// specs/062-customer-workspace L3 T055 — emitDocAudit is the ONE place the "a change that only
// reached the builder's draft is not yet the customer's business" rule lives (FR-022, US3 scenario 7).

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metaversion"
)

type fakeWriteMode struct {
	mode metaversion.Mode
	err  error
}

func (f fakeWriteMode) WriteMode(context.Context, string) (metaversion.Mode, error) {
	return f.mode, f.err
}

func TestEmitDocAudit_ModeDecidesWhetherARowIsWritten(t *testing.T) {
	tests := []struct {
		name string
		mode docWriteModer
		want int
	}{
		{"nil mode always writes", nil, 1},
		{"live write is audited", fakeWriteMode{mode: metaversion.ModeLive}, 1},
		{"live-direct (legacy customer admin) is audited", fakeWriteMode{mode: metaversion.ModeLiveDirect}, 1},
		{"a draft-only write is not audited", fakeWriteMode{mode: metaversion.ModeDraft}, 0},
		{"no caller on ctx: still audited (the service answers 500 itself)", fakeWriteMode{mode: metaversion.ModeLive, err: metaversion.ErrNoCaller}, 1},
		{"cross-tenant draft write: still audited", fakeWriteMode{mode: metaversion.ModeLive, err: metaversion.ErrCrossTenantDraftWrite}, 1},
		{"unexpected error: still audited, even if it came back with ModeDraft", fakeWriteMode{mode: metaversion.ModeDraft, err: errors.New("db")}, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			aud := &captureAuditor{}
			ctx := audit.WithActor(context.Background(), "builder-1")
			emitDocAudit(ctx, aud, tc.mode, "tenant-1", audit.LayoutMeta, "rec-1", audit.ActionUpdate,
				map[string]any{"k": 1}, map[string]any{"k": 2})
			if len(aud.events) != tc.want {
				t.Fatalf("events = %d; want %d", len(aud.events), tc.want)
			}
			if tc.want == 1 {
				e := aud.events[0]
				if e.TenantID != "tenant-1" || e.ActorUserID != "builder-1" || e.EntityName != audit.LayoutMeta || e.RecordID != "rec-1" {
					t.Errorf("event = %+v", e)
				}
			}
		})
	}
}
