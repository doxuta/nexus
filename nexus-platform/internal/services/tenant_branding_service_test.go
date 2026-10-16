// AO-THUONG-HIEU p1 — config-time validation matrix (../specs/013-tenant-branding/spec.md §2) + the
// fail-soft public read. The FE consumes the stored document verbatim, so the XSS-bearing
// shapes (javascript:, data:text/html) MUST die here.
package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
)

func TestValidateBranding(t *testing.T) {
	tests := []struct {
		name    string
		in      Branding
		wantErr string // "" = valid; else the offending details.field
	}{
		{"empty document is valid", Branding{}, ""},
		{"full valid document", Branding{AppName: "Phần mềm ABC", LogoURL: "https://abc.vn/logo.png", AccentColor: "#0F6E84"}, ""},
		{"data-uri png valid", Branding{LogoURL: "data:image/png;base64,iVBORw0KGgo="}, ""},
		{"data-uri svg valid", Branding{LogoURL: "data:image/svg+xml;base64,PHN2Zz4="}, ""},
		{"appName too long", Branding{AppName: strings.Repeat("ă", 61)}, "appName"},
		{"appName 60 runes ok (rune not byte)", Branding{AppName: strings.Repeat("ă", 60)}, ""},
		{"javascript url rejected", Branding{LogoURL: "javascript:alert(1)"}, "logoUrl"},
		{"data text/html rejected", Branding{LogoURL: "data:text/html;base64,PHNjcmlwdD4="}, "logoUrl"},
		{"logo too large", Branding{LogoURL: "data:image/png;base64," + strings.Repeat("A", 100_001)}, "logoUrl"},
		{"bad hex", Branding{AccentColor: "#12345"}, "accentColor"},
		{"css injection via color", Branding{AccentColor: "#123456;background:url(x)"}, "accentColor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.in
			err := ValidateBranding(&b)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			ve, ok := err.(*ValidationError)
			if !ok || ve.Code != "BRANDING_INVALID" || ve.Details["field"] != tt.wantErr {
				t.Fatalf("want BRANDING_INVALID on %s, got %v", tt.wantErr, err)
			}
		})
	}
}

// fakeBrandingStore backs the fail-soft public-read contract without MySQL.
type fakeBrandingStore struct {
	bySlug map[string]*auth.Tenant
}

func (f *fakeBrandingStore) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	if t, ok := f.bySlug[slug]; ok {
		return t, nil
	}
	return nil, auth.ErrTenantNotFound
}
func (f *fakeBrandingStore) GetTenantByID(context.Context, string) (*auth.Tenant, error) {
	return nil, auth.ErrTenantNotFound
}
func (f *fakeBrandingStore) UpdateTenantBranding(context.Context, string, json.RawMessage) error {
	return nil
}

func TestGetPublicBySlug_FailSoft(t *testing.T) {
	svc := NewTenantBrandingService(&fakeBrandingStore{bySlug: map[string]*auth.Tenant{
		"abc":    {ID: "t1", Slug: "abc", Branding: json.RawMessage(`{"appName":"ABC","accentColor":"#112233"}`)},
		"broken": {ID: "t2", Slug: "broken", Branding: json.RawMessage(`{not json`)},
	}})
	ctx := context.Background()

	if b := svc.GetPublicBySlug(ctx, "ABC "); b.AppName != "ABC" { // slug normalized (trim+lower)
		t.Fatalf("branded tenant = %+v", b)
	}
	// Unknown slug, empty slug, malformed stored JSON — all the SAME empty shape, no error.
	for _, slug := range []string{"ghost", "", "broken"} {
		if b := svc.GetPublicBySlug(ctx, slug); b != (Branding{}) {
			t.Fatalf("slug %q: want empty branding, got %+v", slug, b)
		}
	}
}
