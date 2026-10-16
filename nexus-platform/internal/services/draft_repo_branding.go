package services

import (
	"context"
	"encoding/json"
	"fmt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metaversion"
)

// DraftBrandingStore implements brandingTenantStore around the live auth repository
// (specs/063-draft-publish S7-3, same contract as DraftReportRepo): on a delivered tenant the
// builder's PUT /admin/tenant/branding becomes a draft and GET /admin/tenant/branding overlays it.
//
// ResolveTenantBySlug — the PUBLIC login-page path — is passed straight through: it has no Caller in
// its ctx and this wrapper never overlays it, so the customer's login page can never show a draft
// (FR-013). TenantBrandingService.GetPublicBySlug is the only caller of it.
type DraftBrandingStore struct {
	live   brandingTenantStore
	drafts draftStore
	gate   *DraftGate
	// rec + adapter: direct version entry of a non-builder\'s live write (S7-5); nil = none.
	rec     *VersionRecorder
	adapter metaversion.DocAdapter
}

// NewDraftBrandingStore wraps live.
func NewDraftBrandingStore(live brandingTenantStore, drafts draftStore, gate *DraftGate) *DraftBrandingStore {
	return &DraftBrandingStore{live: live, drafts: drafts, gate: gate}
}

var _ brandingTenantStore = (*DraftBrandingStore)(nil)

// ResolveTenantBySlug implements brandingTenantStore: always live.
func (s *DraftBrandingStore) ResolveTenantBySlug(ctx context.Context, slug string) (*auth.Tenant, error) {
	return s.live.ResolveTenantBySlug(ctx, slug)
}

// GetTenantByID implements brandingTenantStore: live, or (overlay) the tenant with the draft
// document in place of the live one and DraftState set.
func (s *DraftBrandingStore) GetTenantByID(ctx context.Context, id string) (*auth.Tenant, error) {
	t, err := s.live.GetTenantByID(ctx, id)
	if err != nil {
		return nil, err
	}
	overlay, err := s.gate.ReadOverlay(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("services.DraftBrandingStore.GetTenantByID: %w", err)
	}
	if !overlay {
		return t, nil
	}
	d, err := s.drafts.GetByKey(ctx, id, DocTypeBranding, id)
	if err != nil {
		return nil, fmt.Errorf("services.DraftBrandingStore.GetTenantByID: %w", err)
	}
	if d == nil || d.Op != metaversion.OpUpsert {
		t.DraftState = draftStateLive
		return t, nil
	}
	t.Branding, t.DraftState = json.RawMessage(d.DraftJSON), draftStateEdited
	return t, nil
}

// UpdateTenantBranding implements brandingTenantStore.
func (s *DraftBrandingStore) UpdateTenantBranding(ctx context.Context, tenantID string, raw json.RawMessage) error {
	mode, err := s.gate.WriteMode(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("services.DraftBrandingStore.UpdateTenantBranding: %w", err)
	}
	if mode != metaversion.ModeDraft {
		return s.rec.TraceLive(ctx, mode, s.adapter, tenantID, "update", tenantID, func() (string, error) { return tenantID, s.live.UpdateTenantBranding(ctx, tenantID, raw) })
	}
	if _, err := s.live.GetTenantByID(ctx, tenantID); err != nil { // unknown / inactive tenant: same answer as a live save
		return err
	}
	doc, err := canonicalBranding(raw)
	if err != nil {
		return fmt.Errorf("services.DraftBrandingStore.UpdateTenantBranding: %w", err)
	}
	if err := s.drafts.Upsert(ctx, tenantID, metaversion.UpsertInput{DocType: DocTypeBranding, DocKey: tenantID, Op: metaversion.OpUpsert,
		Doc: doc, Label: brandingDocLabel, UserID: draftActor(ctx, "")}); err != nil {
		return fmt.Errorf("services.DraftBrandingStore.UpdateTenantBranding: %w", err)
	}
	return nil
}

// WithRecorder arms the direct version entries (specs/063 S7-5) with the type's document adapter.
func (s *DraftBrandingStore) WithRecorder(rec *VersionRecorder, ad metaversion.DocAdapter) *DraftBrandingStore {
	s.rec, s.adapter = rec, ad
	return s
}
