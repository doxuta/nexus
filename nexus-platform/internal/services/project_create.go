package services

// project_create.go - specs/062-customer-workspace L6: POST /admin/projects in one call. The new
// customer workspace, its first owner, its calendar and its registry row are ONE transaction
// (Provision); the starter pack is installed AFTER that commit, so a pack that fails to install
// leaves a usable, empty project the builder can repair - never a half-created tenant. The last
// step opens the builder's session inside the new workspace so the FE can go straight to its Studio.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/project"
)

// Start modes of ProjectCreateInput.StartMode.
const (
	ProjectStartBlank = "blank"
	ProjectStartPack  = "pack"
	ProjectStartAI    = "ai"
)

// Statuses of ProjectStart.Status.
const (
	ProjectStartDone    = "done"
	ProjectStartFailed  = "failed"
	ProjectStartSkipped = "skipped"
)

// PackStartResult is what installing a starter pack reports back: enough for the FE to say what was
// built, nothing that identifies a record. Declared in services so the installer
// (vertical_pack.Starter) can fill it without services importing vertical_pack (an import cycle).
type PackStartResult struct {
	Slug             string   `json:"slug"`
	Name             string   `json:"name"`
	Version          string   `json:"version"`
	EntitiesCreated  int      `json:"entitiesCreated"`
	RolesCreated     int      `json:"rolesCreated"`
	WorkflowsCreated int      `json:"workflowsCreated"`
	ReportsCreated   int      `json:"reportsCreated"`
	AppSlug          string   `json:"appSlug,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

// PackStarter installs a built-in starter pack into a brand-new customer workspace. Implemented by
// vertical_pack.Starter and wired in main.go.
type PackStarter interface {
	// BuiltinName returns the pack's display name and whether a built-in pack with that slug exists.
	BuiltinName(slug string) (string, bool)
	// InstallBuiltin installs the pack into tenantID; userID is the builder, recorded as the actor.
	InstallBuiltin(ctx context.Context, tenantID, userID, slug string) (*PackStartResult, error)
}

// projectProvisioner creates the tenant + owner + calendar + profile (*TenantProvisionService).
type projectProvisioner interface {
	Provision(ctx context.Context, in ProvisionTenantInput) (*ProvisionedTenant, error)
}

// projectSessionOpener opens the builder's session in a customer workspace (*WorkspaceSessionService).
type projectSessionOpener interface {
	Open(ctx context.Context, home auth.Subject, in WorkspaceOpenInput) (*WorkspaceOpenResult, error)
}

// ProjectCreateInput is the POST /admin/projects body after binding.
type ProjectCreateInput struct {
	Name             string
	Slug             string
	Industry         string
	AdminEmail       string
	AdminFullName    string
	AdminPassword    string
	GeneratePassword bool
	StartMode        string
	StartPack        string
}

// ProjectStartError is the reason a starter install failed (code + a message safe to show).
type ProjectStartError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ProjectStart reports how the starter step went. Status is done | failed | skipped.
type ProjectStart struct {
	Mode   string             `json:"mode"`
	Status string             `json:"status"`
	Result *PackStartResult   `json:"result,omitempty"`
	Error  *ProjectStartError `json:"error,omitempty"`
}

// ProjectCreated is the outcome of Create. OneTimePassword is shown once by the handler and never
// stored; Workspace is absent when the session could not be opened (the project still exists).
type ProjectCreated struct {
	Project         *ProjectView         `json:"project"`
	Admin           UserView             `json:"admin"`
	OneTimePassword string               `json:"-"`
	Start           ProjectStart         `json:"start"`
	Workspace       *WorkspaceOpenResult `json:"workspace,omitempty"`
}

// WithCreate plugs in what POST /admin/projects needs on top of the registry. Until it is called
// Create refuses (the route is only mounted when it is).
func (s *ProjectService) WithCreate(p projectProvisioner, packs PackStarter, sessions projectSessionOpener) *ProjectService {
	s.provisioner, s.packs, s.sessions = p, packs, sessions
	return s
}

// Create makes a customer project in one call. home is the verified Subject of the builder's HOME
// token (the route's gate has already required a live platform operator). Order: every input is
// checked before any write (FR-033) -> Provision (one tx) -> starter pack -> builder session ->
// audit. A starter or session failure after the commit is reported inside a 201, never undone
// (FR-034): the tenant exists and the one-time password has already been minted.
func (s *ProjectService) Create(ctx context.Context, home auth.Subject, in ProjectCreateInput) (*ProjectCreated, error) {
	if s.provisioner == nil {
		return nil, errors.New("services.ProjectService.Create: project creation is not wired")
	}
	mode := strings.ToLower(strings.TrimSpace(in.StartMode))
	packSlug := strings.ToLower(strings.TrimSpace(in.StartPack))
	industry := strings.TrimSpace(in.Industry)

	switch mode {
	case ProjectStartBlank, ProjectStartAI:
		if packSlug != "" {
			return nil, invalid("PROJECT_START_MODE_INVALID", "chỉ cách bắt đầu 'pack' mới có tên gói mẫu")
		}
	case ProjectStartPack:
	default:
		return nil, invalid("PROJECT_START_MODE_INVALID", "cách bắt đầu %q không hợp lệ — dùng blank, pack hoặc ai", in.StartMode)
	}
	if utf8.RuneCountInString(industry) > maxProjectIndustryRunes {
		return nil, invalid("PROJECT_INDUSTRY_TOO_LONG", "ngành tối đa %d ký tự", maxProjectIndustryRunes)
	}
	origin := project.OriginBlank
	var originPack *string
	switch mode {
	case ProjectStartAI:
		origin = project.OriginAI
	case ProjectStartPack:
		name, ok := s.builtinName(packSlug)
		if !ok {
			return nil, invalid("VERTICAL_BUILTIN_NOT_FOUND", "không tìm thấy gói mẫu có sẵn: %s", packSlug)
		}
		origin, originPack = project.OriginPack, &packSlug
		if industry == "" {
			industry = name
		}
	}
	var industryPtr *string
	if industry != "" {
		industryPtr = &industry
	}

	ctx = audit.WithActor(ctx, home.UserID)
	prov, err := s.provisioner.Provision(ctx, ProvisionTenantInput{
		Slug: in.Slug, Name: in.Name, AdminEmail: in.AdminEmail, AdminFullName: in.AdminFullName,
		AdminPassword: in.AdminPassword, GeneratePassword: in.GeneratePassword, ActorTenantID: home.TenantID,
		Project: &ProjectSeed{Industry: industryPtr, Origin: origin, OriginPack: originPack, CreatedBy: home.UserID},
	})
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.Create: %w", err)
	}
	out := &ProjectCreated{Admin: prov.Admin, OneTimePassword: prov.OneTimePassword, Start: ProjectStart{Mode: mode, Status: ProjectStartSkipped}}
	log := s.logger.With(zap.String("tenant_id", home.TenantID), zap.String("user_id", home.UserID), zap.String("target_tenant_id", prov.Tenant.ID))
	// The tenant is committed and the one-time password minted: a caller who disconnects (or times
	// out) from here on must not abort the pack install, the builder session or the audit row and
	// leave a half-started project nobody can receive the password for (FR-034). Values (actor) stay.
	ctx = context.WithoutCancel(ctx)

	if mode == ProjectStartPack {
		out.Start = s.startPack(ctx, log, prov.Tenant.ID, home.UserID, packSlug)
	}

	if s.sessions != nil {
		ws, err := s.sessions.Open(ctx, home, WorkspaceOpenInput{TenantSlug: prov.Tenant.Slug})
		if err != nil {
			log.Warn("services.ProjectService.Create: builder session not opened", zap.Error(err))
		} else {
			out.Workspace = ws
		}
	}

	if views, err := s.views(ctx, []project.TenantRow{{
		ID: prov.Tenant.ID, Slug: prov.Tenant.Slug, Name: prov.Tenant.Name, Status: prov.Tenant.Status, CreatedAt: prov.Tenant.CreatedAt,
	}}); err != nil {
		// the tenant is committed and the password minted: answer without the row rather than 500
		log.Error("services.ProjectService.Create: project row not assembled", zap.Error(err))
	} else {
		out.Project = &views[0]
	}

	type created struct {
		Slug       string  `json:"slug"`
		Name       string  `json:"name"`
		Industry   *string `json:"industry"`
		Origin     string  `json:"origin"`
		OriginPack *string `json:"originPack"`
		Start      string  `json:"start"`
	}
	emitMetaAuditFor(ctx, s.auditor, home.TenantID, audit.ProjectMeta, prov.Tenant.ID, audit.ActionCreate, nil,
		created{Slug: prov.Tenant.Slug, Name: prov.Tenant.Name, Industry: industryPtr, Origin: origin, OriginPack: originPack, Start: out.Start.Status})
	log.Info("services.ProjectService.Create: project created", zap.String("origin", origin), zap.String("start", out.Start.Status))
	return out, nil
}

// builtinName asks the starter whether the pack exists; no starter wired means no pack does.
func (s *ProjectService) builtinName(slug string) (string, bool) {
	if s.packs == nil || slug == "" {
		return "", false
	}
	return s.packs.BuiltinName(slug)
}

// startPack installs the pack and folds any failure into the reported status instead of an error.
func (s *ProjectService) startPack(ctx context.Context, log *zap.Logger, tenantID, userID, slug string) ProjectStart {
	res, err := s.packs.InstallBuiltin(ctx, tenantID, userID, slug)
	if err == nil {
		return ProjectStart{Mode: ProjectStartPack, Status: ProjectStartDone, Result: res}
	}
	log.Error("services.ProjectService.Create: starter pack failed", zap.String("pack", slug), zap.Error(err))
	fail := &ProjectStartError{Code: "VERTICAL_INSTALL_FAILED", Message: "Không cài được gói mẫu — dự án vẫn được tạo, vào Studio để thử lại"}
	var ve *ValidationError
	if errors.As(err, &ve) {
		fail = &ProjectStartError{Code: ve.Code, Message: ve.Msg}
	}
	return ProjectStart{Mode: ProjectStartPack, Status: ProjectStartFailed, Error: fail}
}
