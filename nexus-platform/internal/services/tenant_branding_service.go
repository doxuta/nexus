// AO-THUONG-HIEU p1 (../specs/013-tenant-branding/spec.md): per-tenant white-label branding.
//
// The document is small on purpose — appName / logoUrl / accentColor cover "phần mềm của
// công ty tôi" on the customer-facing App chrome + login page. Custom domain and per-tenant
// email sender are PHASE 2 (activation-gated, see the spec preamble + §8). Validation is config-time
// only: the FE consumes the stored document verbatim, so everything that could hurt at
// render time (javascript: URLs, arbitrary CSS strings) must be rejected here.
package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// Branding is the parsed white-label document. All fields optional; zero value = default
// Nexus branding. omitempty keeps the stored JSON minimal ({} → SQL NULL on write).
type Branding struct {
	AppName     string `json:"appName,omitempty"`
	LogoURL     string `json:"logoUrl,omitempty"`
	AccentColor string `json:"accentColor,omitempty"`
}

// brandingTenantStore is the narrow slice of auth.UserRepository the service needs.
type brandingTenantStore interface {
	ResolveTenantBySlug(ctx context.Context, slug string) (*auth.Tenant, error)
	GetTenantByID(ctx context.Context, id string) (*auth.Tenant, error)
	UpdateTenantBranding(ctx context.Context, tenantID string, raw json.RawMessage) error
}

// TenantBrandingService validates and persists the per-tenant branding document.
type TenantBrandingService struct {
	store brandingTenantStore
	// auditor receives one __branding__ row per committed Update (specs/062 L4); noop until
	// WithAuditor. draftMode skips the row for a draft-only write (nil = audit every write: branding
	// is not a draft type yet — see TestDocAudit_WiringMatchesDraftClasses).
	auditor   audit.Auditor
	draftMode docWriteModer
}

// NewTenantBrandingService wires the service over the auth user/tenant repository.
func NewTenantBrandingService(store brandingTenantStore) *TenantBrandingService {
	return &TenantBrandingService{store: store, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for branding writes and the optional draft mode. Chainable; a
// nil auditor keeps the noop.
func (s *TenantBrandingService) WithAuditor(a audit.Auditor, mode docWriteModer) *TenantBrandingService {
	if a != nil {
		s.auditor = a
	}
	s.draftMode = mode
	return s
}

const (
	brandingAppNameMaxRunes = 60
	brandingLogoMaxChars    = 100_000 // ~72KB image as base64 data-URI
)

var brandingHexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// brandingLogoPrefixes is the allowlist for logoUrl (spec §2): plain web URLs or inline
// base64 raster/SVG images. Everything else (javascript:, data:text/html, blob:, …) is a
// stored-XSS vector through <img src> / <link rel=icon> and is rejected.
//
// INVARIANT (security audit): SVG is the one allowed format that can CARRY active content —
// it is safe ONLY because every FE sink is <img src> / favicon (script-inert-by-spec
// contexts). logoUrl must NEVER be rendered via <object>/<iframe>/inline-SVG/innerHTML or
// opened as a top-level navigation; revisit this allowlist before adding any such sink.
var brandingLogoPrefixes = []string{
	"https://", "http://",
	"data:image/png;base64,", "data:image/jpeg;base64,",
	"data:image/svg+xml;base64,", "data:image/webp;base64,",
}

// ValidateBranding trims and validates the document in place (422 BRANDING_INVALID with
// details.field pinning the offender).
func ValidateBranding(b *Branding) error {
	b.AppName = strings.TrimSpace(b.AppName)
	b.LogoURL = strings.TrimSpace(b.LogoURL)
	b.AccentColor = strings.TrimSpace(b.AccentColor)

	if utf8.RuneCountInString(b.AppName) > brandingAppNameMaxRunes {
		return invalidField("BRANDING_INVALID", "appName", "appName tối đa %d ký tự", brandingAppNameMaxRunes)
	}
	if b.LogoURL != "" {
		if len(b.LogoURL) > brandingLogoMaxChars {
			return invalidField("BRANDING_INVALID", "logoUrl", "logoUrl tối đa %d ký tự (ảnh ~72KB)", brandingLogoMaxChars)
		}
		ok := false
		for _, p := range brandingLogoPrefixes {
			if strings.HasPrefix(b.LogoURL, p) {
				ok = true
				break
			}
		}
		if !ok {
			return invalidField("BRANDING_INVALID", "logoUrl",
				"logoUrl phải là https:// / http:// hoặc data:image/(png|jpeg|svg+xml|webp);base64")
		}
	}
	if b.AccentColor != "" && !brandingHexColor.MatchString(b.AccentColor) {
		return invalidField("BRANDING_INVALID", "accentColor", "accentColor phải là mã hex dạng #RRGGBB")
	}
	return nil
}

// Get returns the caller tenant's branding (empty document when unset).
func (s *TenantBrandingService) Get(ctx context.Context, tenantID string) (Branding, error) {
	b, _, err := s.GetState(ctx, tenantID)
	return b, err
}

// GetState is Get plus the draft state of the document: "" when the caller reads live (everyone but
// the builder asking for the overlay), "live" / "edited" when the overlay is on.
func (s *TenantBrandingService) GetState(ctx context.Context, tenantID string) (Branding, string, error) {
	t, err := s.store.GetTenantByID(ctx, tenantID)
	if err != nil {
		return Branding{}, "", fmt.Errorf("services.TenantBrandingService.GetState: %w", err)
	}
	return decodeBranding(t.Branding), t.DraftState, nil
}

// GetPublicBySlug returns the branding for a login slug, FAIL-SOFT: unknown/suspended tenant
// or unset branding all yield the same empty document (spec §3 posture — one shape, no
// existence signal beyond what a branded login page inherently is).
func (s *TenantBrandingService) GetPublicBySlug(ctx context.Context, slug string) Branding {
	slug = strings.TrimSpace(strings.ToLower(slug))
	if slug == "" {
		return Branding{}
	}
	t, err := s.store.ResolveTenantBySlug(ctx, slug)
	if err != nil {
		return Branding{} // ErrTenantNotFound and transient errors alike — public read is best-effort
	}
	return decodeBranding(t.Branding)
}

// stored is the column value of a validated document: nil (SQL NULL, back to default Nexus branding)
// for the empty document.
func (b Branding) stored() (json.RawMessage, error) {
	if b == (Branding{}) {
		return nil, nil
	}
	enc, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("marshal branding: %w", err)
	}
	return enc, nil
}

// Update validates and full-replaces the caller tenant's branding. An empty document stores
// NULL (back to default Nexus).
func (s *TenantBrandingService) Update(ctx context.Context, tenantID string, b Branding) (Branding, error) {
	if err := ValidateBranding(&b); err != nil {
		return Branding{}, err
	}
	raw, err := b.stored()
	if err != nil {
		return Branding{}, fmt.Errorf("services.TenantBrandingService.Update: %w", err)
	}
	// The prior document, read before the write so the row carries both sides. Best-effort: an
	// unreadable tenant makes the write below fail on its own terms.
	var before any
	if t := priorDoc(func() (*auth.Tenant, error) { return s.store.GetTenantByID(ctx, tenantID) }); t != nil {
		before = brandingAuditSnapshot(decodeBranding(t.Branding))
	}
	if err := s.store.UpdateTenantBranding(ctx, tenantID, raw); err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			return Branding{}, invalid("TENANT_NOT_FOUND", "tenant không tồn tại")
		}
		return Branding{}, fmt.Errorf("services.TenantBrandingService.Update: %w", err)
	}
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.BrandingMeta, tenantID, audit.ActionUpdate,
		before, brandingAuditSnapshot(b))
	return b, nil
}

// decodeBranding parses a stored document; malformed rows read as default (fail-safe, the
// write path guarantees shape so this only guards hand-edited SQL).
func decodeBranding(raw json.RawMessage) Branding {
	var b Branding
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}
