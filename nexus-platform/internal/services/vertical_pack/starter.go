package vertical_pack

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/services"
)

// packInstaller is the one Service method Starter needs (*Service satisfies it).
type packInstaller interface {
	Install(ctx context.Context, tenantID, userID string, pack *Pack) (*InstallResult, error)
}

// Starter installs a BUILT-IN pack into a freshly created customer workspace. It implements
// services.PackStarter (specs/062 L6) from this package because services cannot import vertical_pack
// (vertical_pack already imports services).
type Starter struct {
	svc packInstaller
}

// NewStarter wires the Starter over the pack installer.
func NewStarter(svc packInstaller) *Starter { return &Starter{svc: svc} }

// BuiltinName returns the display name of the built-in pack and whether it exists; a manifest-less
// pack falls back to its slug.
func (s *Starter) BuiltinName(slug string) (string, bool) {
	pack, ok := BuiltinPack(strings.ToLower(strings.TrimSpace(slug)))
	if !ok {
		return "", false
	}
	return packTitle(pack, slug), true
}

// InstallBuiltin installs the named built-in pack into tenantID with the SAME Install path the
// handover wizard uses (same validation, idempotency and ledger row). userID is the builder.
func (s *Starter) InstallBuiltin(ctx context.Context, tenantID, userID, slug string) (*services.PackStartResult, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	pack, ok := BuiltinPack(slug)
	if !ok {
		return nil, &services.ValidationError{Code: "VERTICAL_BUILTIN_NOT_FOUND", Msg: "Không tìm thấy vertical pack có sẵn: " + slug}
	}
	res, err := s.svc.Install(ctx, tenantID, userID, pack)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.Starter.InstallBuiltin: %w", err)
	}
	out := &services.PackStartResult{
		Slug: slug, Name: packTitle(pack, slug),
		EntitiesCreated: len(res.EntitiesCreated), RolesCreated: len(res.RolesCreated),
		WorkflowsCreated: len(res.WorkflowsCreated), ReportsCreated: len(res.ReportsCreated),
		AppSlug: res.AppSlug, Warnings: res.Warnings,
	}
	if pack.Manifest != nil {
		out.Version = pack.Manifest.Version
	}
	return out, nil
}

// packTitle is the manifest name, else the legacy pack name, else the slug.
func packTitle(pack *Pack, slug string) string {
	if pack.Manifest != nil && strings.TrimSpace(pack.Manifest.Name) != "" {
		return strings.TrimSpace(pack.Manifest.Name)
	}
	if strings.TrimSpace(pack.Name) != "" {
		return strings.TrimSpace(pack.Name)
	}
	return slug
}
