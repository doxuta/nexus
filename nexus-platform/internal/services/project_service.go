package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/audit"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/project"
)

// maxProjectIndustryRunes is sys_project_profiles.industry's VARCHAR(64), counted in characters.
const maxProjectIndustryRunes = 64

// DeliveryReader answers "when was each project delivered?" — the first numbered publish of the
// draft/publish tier. Declared HERE, on the consuming side; the concrete reader is
// metaversion.DeliveryRepo (specs/063-draft-publish S7-1a). A tenant never delivered is absent from
// the map.
type DeliveryReader interface {
	DeliveredAt(ctx context.Context, tenantIDs []string) (map[string]time.Time, error)
}

// noopDeliveryReader delivers nobody: the default until a real reader is wired.
type noopDeliveryReader struct{}

func (noopDeliveryReader) DeliveredAt(context.Context, []string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

// projectRepository is the registry's storage (implemented by *project.Repository).
type projectRepository interface {
	TenantRow(ctx context.Context, tenantID string) (*project.TenantRow, error)
	ListByTenants(ctx context.Context, tenantIDs []string) (map[string]project.Profile, error)
	Signals(ctx context.Context, tenantIDs []string) (map[string]project.Signals, error)
	Get(ctx context.Context, tenantID string) (*project.Profile, error)
	Upsert(ctx context.Context, p *project.Profile, updateCols ...string) error
}

// projectTenants lists the tenant registry (implemented by *TenantProvisionService).
type projectTenants interface {
	ListTenants(ctx context.Context) ([]TenantListItem, error)
}

// projectDoctor scans ONE tenant read-only (implemented by *DoctorService).
type projectDoctor interface {
	Scan(ctx context.Context, tenantID string) ([]doctor.CheckResult, error)
}

// projectApps reads a tenant's apps and their readiness (implemented by *AppService).
type projectApps interface {
	ListApps(ctx context.Context, tenantID string) ([]appmeta.App, error)
	GetApp(ctx context.Context, tenantID, id string) (*appmeta.App, []appmeta.NavItem, error)
	Readiness(ctx context.Context, tenantID string, items []appmeta.NavItem) (*AppReadinessView, error)
}

// ProjectActor is the builder behind a registry change. TenantID is the builder's HOME workshop —
// the registry's audit trail lives there, never in the customer.
type ProjectActor struct {
	TenantID string
	UserID   string
}

// ProjectCounts are the per-project signals the S5 build steps read (FR-027a).
type ProjectCounts = project.Counts

// HealthCheckItem is one finding group kept in the stored snapshot (no item detail: counts only).
type HealthCheckItem struct {
	CheckID  string `json:"checkId"`
	Severity string `json:"severity"`
	Count    int    `json:"count"`
}

// AppNotReady names an app that failed the "ready to operate" gate and says why.
type AppNotReady struct {
	AppID   string   `json:"appId"`
	Name    string   `json:"name"`
	Reasons []string `json:"reasons"`
}

// ProjectHealth is the stored health snapshot of a project. Status is "unknown" until the first
// check. The registry list only ever READS it — it never scans.
type ProjectHealth struct {
	Status       string            `json:"status"`
	Critical     int               `json:"critical"`
	Warn         int               `json:"warn"`
	Checks       []HealthCheckItem `json:"checks"`
	AppsNotReady []AppNotReady     `json:"appsNotReady"`
	CheckedAt    *time.Time        `json:"checkedAt"`
	// Ops is the operating label written by HealthCheck (specs/066 L6): null until a check ran with
	// the ops reader wired. It is NOT part of Status or the stage.
	Ops *OpsHealth `json:"ops"`
}

// OpsHealth is the operating-health label of a project: DeriveHealth over HealthService.Summary.
type OpsHealth struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checkedAt"`
}

// projectOps gathers one customer's operating signals (implemented by *HealthService).
type projectOps interface {
	Summary(ctx context.Context, tenantID string) (HealthSummary, error)
}

// storedHealth is the health_json document (status and checkedAt live in their own columns). The
// optional `ops` key (specs/066 L6) is carried to the card as health.ops only; it never feeds Status.
type storedHealth struct {
	Ops          *OpsHealth        `json:"ops,omitempty"`
	Critical     int               `json:"critical"`
	Warn         int               `json:"warn"`
	Checks       []HealthCheckItem `json:"checks"`
	AppsNotReady []AppNotReady     `json:"appsNotReady"`
}

// ProjectView is one registry row (GET /admin/projects). It is the ONLY source of the project card:
// lanes landing later add their field here rather than growing a second endpoint.
type ProjectView struct {
	TenantID     string        `json:"tenantId"`
	Slug         string        `json:"slug"`
	Name         string        `json:"name"`
	TenantStatus string        `json:"tenantStatus"`
	Industry     *string       `json:"industry"`
	Origin       *string       `json:"origin"`
	OriginPack   *string       `json:"originPack"`
	Stage        string        `json:"stage"`
	CreatedAt    time.Time     `json:"createdAt"`
	TrialSince   *time.Time    `json:"trialSince"`
	DeliveredAt  *time.Time    `json:"deliveredAt"`
	Branded      bool          `json:"branded"`
	Health       ProjectHealth `json:"health"`
	Counts       ProjectCounts `json:"counts"`
	LastActiveAt *time.Time    `json:"lastActiveAt"`
	// MaintenanceUntil is set only while the workspace is write-frozen (specs/068); omitted otherwise.
	MaintenanceUntil *time.Time `json:"maintenanceUntil,omitempty"`
	// UsableOwnerCount is the active customer owners of the workspace (specs/065), copied from the
	// registry row; 0 means nobody can manage that customer's users from inside the app.
	UsableOwnerCount int `json:"usableOwnerCount"`
	// Backup is always present (specs/068 T100): an empty one means "never backed up".
	Backup *ProjectBackup `json:"backup"`
}

// ProjectBackup is the card's backup signal.
type ProjectBackup struct {
	LastReadyAt *time.Time `json:"lastReadyAt"`
	LastFailed  bool       `json:"lastFailed"`
}

// ProjectPatch is PATCH /admin/projects/:tenantId. An absent field is left alone; Industry nil with
// IndustrySet true (JSON null) or blank clears it.
type ProjectPatch struct {
	IndustrySet bool
	Industry    *string
	Trial       *bool
}

// ProjectService is the project registry: list, patch, health check.
type ProjectService struct {
	repo     projectRepository
	tenants  projectTenants
	doctor   projectDoctor
	apps     projectApps
	delivery DeliveryReader
	ops      projectOps // nil = no operating label (specs/066 L6)
	// create-a-project collaborators (WithCreate, project_create.go); nil until wired
	provisioner projectProvisioner
	packs       PackStarter
	sessions    projectSessionOpener
	auditor     audit.Auditor
	logger      *zap.Logger
	now         func() time.Time
	newID       func() string
}

// NewProjectService wires the service. delivery defaults to "nobody delivered" until WithDelivery.
// logger may be nil.
func NewProjectService(repo projectRepository, tenants projectTenants, d projectDoctor, apps projectApps, a audit.Auditor, logger *zap.Logger) *ProjectService {
	if logger == nil {
		logger = zap.NewNop()
	}
	if a == nil {
		a = audit.NewNoopAuditor()
	}
	return &ProjectService{repo: repo, tenants: tenants, doctor: d, apps: apps, delivery: noopDeliveryReader{},
		auditor: a, logger: logger, now: func() time.Time { return time.Now().UTC() }, newID: uuid.NewString}
}

// WithOps plugs the operating-health reader: HealthCheck then stores health_json.ops.
func (s *ProjectService) WithOps(o projectOps) *ProjectService {
	s.ops = o
	return s
}

// WithDelivery plugs the reader of the "delivered" mark (metaversion.DeliveryRepo).
func (s *ProjectService) WithDelivery(d DeliveryReader) *ProjectService {
	if d != nil {
		s.delivery = d
	}
	return s
}

// List returns every live project except the caller's own workshop: retired workspaces are skipped,
// suspended ones stay (with their status). It runs a FIXED number of statements whatever the number
// of customers and NEVER scans — health is the stored snapshot.
//
// This is the single source of the project card. A lane that lands later (usable owner count,
// maintenanceUntil + backup, health.ops) adds its field here with one `WHERE tenant_id IN (?)`
// statement, keeping the count constant.
func (s *ProjectService) List(ctx context.Context, callerTenantID string) ([]ProjectView, error) {
	all, err := s.tenants.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.List: %w", err)
	}
	rows := make([]project.TenantRow, 0, len(all))
	frozen := map[string]time.Time{}
	owners := map[string]int{}
	for i := range all {
		if all[i].RetiredAt != nil || all[i].ID == callerTenantID {
			continue
		}
		if mu := all[i].MaintenanceUntil; mu != nil && mu.After(s.now()) {
			frozen[all[i].ID] = *mu
		}
		owners[all[i].ID] = all[i].UsableOwnerCount
		rows = append(rows, project.TenantRow{ID: all[i].ID, Slug: all[i].Slug, Name: all[i].Name,
			Status: all[i].Status, CreatedAt: all[i].CreatedAt})
	}
	views, err := s.views(ctx, rows)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.List: %w", err)
	}
	for i := range views {
		views[i].UsableOwnerCount = owners[views[i].TenantID]
		if mu, ok := frozen[views[i].TenantID]; ok {
			mu := mu
			views[i].MaintenanceUntil = &mu
		}
	}
	return views, nil
}

// views assembles registry rows for the given tenants: 1 profile statement, the fixed signal
// statements and 1 delivery statement, merged in memory.
func (s *ProjectService) views(ctx context.Context, rows []project.TenantRow) ([]ProjectView, error) {
	out := make([]ProjectView, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	profiles, err := s.repo.ListByTenants(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	signals, err := s.repo.Signals(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("signals: %w", err)
	}
	delivered, err := s.delivery.DeliveredAt(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("delivery: %w", err)
	}
	for i := range rows {
		t := rows[i]
		p, hasProfile := profiles[t.ID]
		v := ProjectView{
			TenantID: t.ID, Slug: t.Slug, Name: t.Name, TenantStatus: t.Status, CreatedAt: t.CreatedAt,
			Branded: signals[t.ID].Branded, Counts: signals[t.ID].Counts, LastActiveAt: signals[t.ID].LastActiveAt,
			Backup: &ProjectBackup{LastReadyAt: signals[t.ID].Backup.LastReadyAt, LastFailed: signals[t.ID].Backup.LastFailed},
			Health: ProjectHealth{Status: "unknown", Checks: []HealthCheckItem{}, AppsNotReady: []AppNotReady{}},
		}
		if hasProfile {
			v.Industry, v.Origin, v.OriginPack, v.TrialSince = p.Industry, p.Origin, p.OriginPack, p.TrialSince
			v.Health = s.healthOf(p)
		}
		if at, ok := delivered[t.ID]; ok {
			at := at
			v.DeliveredAt = &at
		}
		v.Stage = DeriveStage(v.DeliveredAt, v.TrialSince, v.Health.Status)
		out = append(out, v)
	}
	return out, nil
}

// healthOf decodes a profile's stored snapshot. A corrupt document degrades to the bare status
// (logged) instead of failing the whole list.
func (s *ProjectService) healthOf(p project.Profile) ProjectHealth {
	h := ProjectHealth{Status: "unknown", Checks: []HealthCheckItem{}, AppsNotReady: []AppNotReady{}, CheckedAt: p.HealthCheckedAt}
	if p.HealthStatus != nil && *p.HealthStatus != "" {
		h.Status = *p.HealthStatus
	}
	if len(p.HealthJSON) == 0 {
		return h
	}
	// ponytail: one decode per project (tens of rows), not a hot loop; cache if the registry ever grows past that.
	var sh storedHealth
	if err := json.Unmarshal(p.HealthJSON, &sh); err != nil {
		s.logger.Warn("services.ProjectService.healthOf: unreadable health_json",
			zap.String("tenant_id", p.TenantID), zap.Error(err))
		return h
	}
	h.Critical, h.Warn = sh.Critical, sh.Warn
	h.Ops = sh.Ops
	if sh.Checks != nil {
		h.Checks = sh.Checks
	}
	if sh.AppsNotReady != nil {
		h.AppsNotReady = sh.AppsNotReady
	}
	return h
}

// liveTenant returns the tenant row or the 404 a missing / retired workspace deserves.
func (s *ProjectService) liveTenant(ctx context.Context, op, tenantID string) (*project.TenantRow, error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return nil, invalid("TENANT_NOT_FOUND", "không tìm thấy workspace")
	}
	t, err := s.repo.TenantRow(ctx, tenantID)
	if errors.Is(err, project.ErrNotFound) {
		return nil, invalid("TENANT_NOT_FOUND", "không tìm thấy workspace")
	}
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.%s: %w", op, err)
	}
	return t, nil
}

// profileOrNew loads the tenant's profile or starts a blank one owned by the actor.
func (s *ProjectService) profileOrNew(ctx context.Context, actor ProjectActor, tenantID string) (*project.Profile, error) {
	p, err := s.repo.Get(ctx, tenantID)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, project.ErrNotFound) {
		return nil, err
	}
	return &project.Profile{ID: s.newID(), TenantID: tenantID, CreatedBy: actor.UserID}, nil
}

// Patch sets the industry and/or the "Chạy thử" mark of one project and returns its fresh row.
// "Delivered" is never settable here: it is read from the publish history, and switching the trial
// mark ON for a delivered project is refused (switching it OFF is always allowed — it only clears).
func (s *ProjectService) Patch(ctx context.Context, actor ProjectActor, tenantID string, in ProjectPatch) (*ProjectView, error) {
	t, err := s.liveTenant(ctx, "Patch", tenantID)
	if err != nil {
		return nil, err
	}
	if in.IndustrySet && in.Industry != nil {
		trimmed := strings.TrimSpace(*in.Industry)
		if utf8.RuneCountInString(trimmed) > maxProjectIndustryRunes {
			return nil, invalid("PROJECT_INDUSTRY_TOO_LONG", "ngành tối đa %d ký tự", maxProjectIndustryRunes)
		}
		in.Industry = &trimmed
		if trimmed == "" {
			in.Industry = nil
		}
	}
	if in.Trial != nil && *in.Trial {
		delivered, derr := s.delivery.DeliveredAt(ctx, []string{t.ID})
		if derr != nil {
			return nil, fmt.Errorf("services.ProjectService.Patch: delivery: %w", derr)
		}
		if _, ok := delivered[t.ID]; ok {
			return nil, invalid("PROJECT_TRIAL_AFTER_DELIVERY", "dự án đã giao — không bật Chạy thử")
		}
	}

	if !in.IndustrySet && in.Trial == nil {
		// nothing to change: no profile row, no audit line for a change that did not happen
		views, err := s.views(ctx, []project.TenantRow{*t})
		if err != nil {
			return nil, fmt.Errorf("services.ProjectService.Patch: %w", err)
		}
		return &views[0], nil
	}

	p, err := s.profileOrNew(ctx, actor, t.ID)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.Patch: %w", err)
	}
	type snap struct {
		Industry *string `json:"industry"`
		Trial    bool    `json:"trial"`
	}
	before := snap{Industry: p.Industry, Trial: p.TrialSince != nil}

	cols := []string{"updated_by"}
	if in.IndustrySet {
		p.Industry = in.Industry
		cols = append(cols, "industry")
	}
	if in.Trial != nil {
		cols = append(cols, "trial_since")
		switch {
		case !*in.Trial:
			p.TrialSince = nil
		case p.TrialSince == nil:
			now := s.now()
			p.TrialSince = &now
		}
	}
	p.UpdatedBy, p.UpdatedAt = actor.UserID, s.now()
	if err := s.repo.Upsert(ctx, p, cols...); err != nil {
		return nil, fmt.Errorf("services.ProjectService.Patch: %w", err)
	}
	// the trail names the builder from the verified actor, not from whatever ctx the caller passed
	emitMetaAuditFor(audit.WithActor(ctx, actor.UserID), s.auditor, actor.TenantID, audit.ProjectMeta, t.ID, audit.ActionUpdate,
		before, snap{Industry: p.Industry, Trial: p.TrialSince != nil})
	s.logger.Info("services.ProjectService.Patch: project updated",
		zap.String("tenant_id", actor.TenantID), zap.String("user_id", actor.UserID), zap.String("target_tenant_id", t.ID))

	views, err := s.views(ctx, []project.TenantRow{*t})
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.Patch: %w", err)
	}
	return &views[0], nil
}

// HealthCheck runs the Doctor and the readiness gate of every app on ONE customer and stores the
// result as that project's snapshot. critical = a critical Doctor finding; warn = a warn finding or
// an app that is not ready; info findings and report-only checks never count (a shared table would
// turn red for things nobody can fix with a button). If the scan fails nothing is stored: the last
// good snapshot stays.
func (s *ProjectService) HealthCheck(ctx context.Context, actor ProjectActor, tenantID string) (*ProjectHealth, error) {
	t, err := s.liveTenant(ctx, "HealthCheck", tenantID)
	if err != nil {
		return nil, err
	}
	results, err := s.doctor.Scan(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.HealthCheck: doctor: %w", err)
	}
	sh := storedHealth{Checks: []HealthCheckItem{}, AppsNotReady: []AppNotReady{}}
	for i := range results {
		r := results[i]
		if r.ReportOnly || r.Count <= 0 {
			continue
		}
		switch r.Severity {
		case doctor.SeverityCritical:
			sh.Critical += r.Count
		case doctor.SeverityWarn:
			sh.Warn += r.Count
		default:
			continue // info: not a finding that changes the status
		}
		sh.Checks = append(sh.Checks, HealthCheckItem{CheckID: r.CheckID, Severity: r.Severity, Count: r.Count})
	}

	apps, err := s.apps.ListApps(ctx, t.ID)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.HealthCheck: apps: %w", err)
	}
	for i := range apps {
		_, items, gerr := s.apps.GetApp(ctx, t.ID, apps[i].ID)
		if gerr != nil {
			return nil, fmt.Errorf("services.ProjectService.HealthCheck: app %s: %w", apps[i].ID, gerr)
		}
		view, rerr := s.apps.Readiness(ctx, t.ID, items)
		if rerr != nil {
			return nil, fmt.Errorf("services.ProjectService.HealthCheck: readiness %s: %w", apps[i].ID, rerr)
		}
		if view == nil || view.Ready {
			continue // nil = readiness not wired for this server: not a finding
		}
		sh.AppsNotReady = append(sh.AppsNotReady, AppNotReady{AppID: apps[i].ID, Name: apps[i].Label, Reasons: notReadyReasons(view)})
	}

	if s.ops != nil {
		// Fail-open: the operating label is optional, a failed read must not lose the Doctor snapshot.
		sum, oerr := s.ops.Summary(ctx, t.ID)
		if oerr != nil {
			s.logger.Warn("services.ProjectService.HealthCheck: ops summary", zap.String("target_tenant_id", t.ID), zap.Error(oerr))
		} else {
			sh.Ops = &OpsHealth{Status: sum.Status, CheckedAt: s.now()}
		}
	}

	status := project.HealthOK
	switch {
	case sh.Critical > 0:
		status = project.HealthCritical
	case sh.Warn > 0 || len(sh.AppsNotReady) > 0:
		status = project.HealthWarn
	}
	doc, err := json.Marshal(sh)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.HealthCheck: encode: %w", err)
	}

	p, err := s.profileOrNew(ctx, actor, t.ID)
	if err != nil {
		return nil, fmt.Errorf("services.ProjectService.HealthCheck: %w", err)
	}
	before := ""
	if p.HealthStatus != nil {
		before = *p.HealthStatus
	}
	now := s.now()
	p.HealthStatus, p.HealthJSON, p.HealthCheckedAt = &status, doc, &now
	p.UpdatedBy, p.UpdatedAt = actor.UserID, now
	if err := s.repo.Upsert(ctx, p, "health_status", "health_json", "health_checked_at", "updated_by"); err != nil {
		return nil, fmt.Errorf("services.ProjectService.HealthCheck: %w", err)
	}
	emitMetaAuditFor(audit.WithActor(ctx, actor.UserID), s.auditor, actor.TenantID, audit.ProjectMeta, t.ID, audit.ActionUpdate,
		map[string]any{"health": before},
		map[string]any{"health": status, "critical": sh.Critical, "warn": sh.Warn, "checkedAt": now})
	s.logger.Info("services.ProjectService.HealthCheck: project checked",
		zap.String("tenant_id", actor.TenantID), zap.String("user_id", actor.UserID),
		zap.String("target_tenant_id", t.ID), zap.String("status", status))

	return &ProjectHealth{Status: status, Critical: sh.Critical, Warn: sh.Warn, Checks: sh.Checks,
		AppsNotReady: sh.AppsNotReady, CheckedAt: &now, Ops: sh.Ops}, nil
}

// notReadyReasons turns a failed readiness view into stable machine reasons for the FE to label.
func notReadyReasons(v *AppReadinessView) []string {
	var r []string
	if v.EntityCount == 0 {
		r = append(r, "no_entities")
	}
	if len(v.EntitiesMissingFields) > 0 {
		r = append(r, "entities_missing_fields")
	}
	if !v.HasNonAdminViewRole {
		r = append(r, "no_non_admin_view_role")
	}
	if !v.HasNonAdminUserAssigned {
		r = append(r, "no_non_admin_user")
	}
	if len(r) == 0 {
		r = append(r, "not_ready")
	}
	return r
}
