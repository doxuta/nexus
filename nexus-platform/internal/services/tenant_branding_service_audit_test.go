package services

// specs/062-customer-workspace L4 T064 — changing a tenant's branding leaves a __branding__ audit row
// with the before/after document; a logo is a base64 data URI of up to ~72KB, so the trail keeps a short
// stand-in for it instead of the image.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// brandingStoreFake serves and keeps the one tenant's branding document.
type brandingStoreFake struct {
	raw json.RawMessage
}

func (f *brandingStoreFake) ResolveTenantBySlug(context.Context, string) (*auth.Tenant, error) {
	return nil, auth.ErrTenantNotFound
}
func (f *brandingStoreFake) GetTenantByID(context.Context, string) (*auth.Tenant, error) {
	return &auth.Tenant{ID: "tenant-1", Branding: f.raw}, nil
}
func (f *brandingStoreFake) UpdateTenantBranding(_ context.Context, _ string, raw json.RawMessage) error {
	f.raw = raw
	return nil
}

func TestTenantBrandingService_UpdateEmitsBrandingAudit(t *testing.T) {
	store := &brandingStoreFake{raw: json.RawMessage(`{"appName":"ABC","accentColor":"#112233"}`)}
	aud := &captureAuditor{}
	svc := NewTenantBrandingService(store).WithAuditor(aud, nil)
	ctx := audit.WithActor(context.Background(), "builder-1")

	if _, err := svc.Update(ctx, "tenant-1", Branding{AppName: "ABC", AccentColor: "#FF0000"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("events = %d; want 1", len(aud.events))
	}
	e := aud.events[0]
	if e.EntityName != audit.BrandingMeta || e.Action != audit.ActionUpdate || e.TenantID != "tenant-1" || e.ActorUserID != "builder-1" || e.RecordID != "tenant-1" {
		t.Errorf("event = %+v; want %s/update by builder-1 on tenant-1", e, audit.BrandingMeta)
	}
	if e.Diff == nil || e.Diff.Before["accentColor"] != "#112233" || e.Diff.After["accentColor"] != "#FF0000" {
		t.Errorf("diff = %+v; want accentColor #112233 -> #FF0000", e.Diff)
	}
	if _, kept := e.Diff.After["appName"]; kept {
		t.Errorf("diff = %+v; appName did not change and must not be listed", e.Diff)
	}
}

func TestTenantBrandingService_BigLogoIsSummarisedInTheTrail(t *testing.T) {
	store := &brandingStoreFake{}
	aud := &captureAuditor{}
	svc := NewTenantBrandingService(store).WithAuditor(aud, nil)
	logo := "data:image/png;base64," + strings.Repeat("QUJD", 5000) // ~20KB
	if _, err := svc.Update(context.Background(), "tenant-1", Branding{LogoURL: logo}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(aud.events) != 1 || aud.events[0].Diff == nil {
		t.Fatalf("events = %+v", aud.events)
	}
	got, _ := aud.events[0].Diff.After["logoUrl"].(string)
	if got == "" || len(got) > 200 || strings.Contains(got, strings.Repeat("QUJD", 10)) {
		t.Fatalf("logoUrl in the trail = %q; want a short summary, not the image", got)
	}
	// ...and the stored document itself is untouched.
	if !strings.Contains(string(store.raw), strings.Repeat("QUJD", 100)) {
		t.Fatal("the audit summary must not shorten the logo that is stored")
	}
}

func TestTenantBrandingService_RefusedUpdateEmitsNothing(t *testing.T) {
	aud := &captureAuditor{}
	svc := NewTenantBrandingService(&brandingStoreFake{}).WithAuditor(aud, nil)
	if _, err := svc.Update(context.Background(), "tenant-1", Branding{LogoURL: "javascript:alert(1)"}); err == nil {
		t.Fatal("want BRANDING_INVALID")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after a refused update = %d; want 0", len(aud.events))
	}
}
