package services

// specs/062-customer-workspace T085 - ProjectService.Create against fakes: every input is checked
// BEFORE anything is written, the pack installs only after the project exists, an install or session
// failure never undoes the project, and the audit row lands in the builder's home tenant.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/project"
)

type fakeProvisioner struct {
	calls          int
	afterProvision func()
	got            ProvisionTenantInput
	err            error
}

func (f *fakeProvisioner) Provision(_ context.Context, in ProvisionTenantInput) (*ProvisionedTenant, error) {
	f.calls++
	f.got = in
	if f.err != nil {
		return nil, f.err
	}
	if f.afterProvision != nil {
		f.afterProvision()
	}
	return &ProvisionedTenant{
		Tenant:          TenantView{ID: "new-tenant", Slug: in.Slug, Name: in.Name, Status: "active"},
		Admin:           UserView{ID: "owner-1", TenantID: "new-tenant", Email: in.AdminEmail},
		OneTimePassword: "s3cret-once",
	}, nil
}

type fakePackStarter struct {
	known      map[string]string // slug -> display name
	installs   int
	gotTenant  string
	gotUser    string
	gotSlug    string
	gotActor   string
	installErr error
	// honourCancel makes InstallBuiltin fail on a cancelled ctx, like a real DB call would.
	honourCancel bool
}

func (f *fakePackStarter) BuiltinName(slug string) (string, bool) {
	n, ok := f.known[slug]
	return n, ok
}

func (f *fakePackStarter) InstallBuiltin(ctx context.Context, tenantID, userID, slug string) (*PackStartResult, error) {
	f.installs++
	f.gotTenant, f.gotUser, f.gotSlug, f.gotActor = tenantID, userID, slug, audit.ActorFromContext(ctx)
	if f.honourCancel && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if f.installErr != nil {
		return nil, f.installErr
	}
	return &PackStartResult{Slug: slug, Name: f.known[slug], Version: "1.0.0", EntitiesCreated: 3, RolesCreated: 2}, nil
}

type fakeSessionOpener struct {
	calls   int
	gotHome auth.Subject
	gotSlug string
	err     error
}

func (f *fakeSessionOpener) Open(ctx context.Context, home auth.Subject, in WorkspaceOpenInput) (*WorkspaceOpenResult, error) {
	f.calls++
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f.gotHome, f.gotSlug = home, in.TenantSlug
	if f.err != nil {
		return nil, f.err
	}
	return &WorkspaceOpenResult{AccessToken: "ws-token", ExpiresAt: time.Now().Add(time.Hour),
		Session: WorkspaceSessionView{ID: "ws-1", TenantID: "new-tenant", TenantSlug: in.TenantSlug}, Created: true}, nil
}

type pcFixture struct {
	svc      *ProjectService
	prov     *fakeProvisioner
	packs    *fakePackStarter
	sessions *fakeSessionOpener
	auditor  *captureAuditor
}

var pcHome = auth.Subject{UserID: "builder-1", TenantID: "home-tenant", SessionID: "sid-1", IsAdmin: true}

func newPcFixture(t *testing.T) *pcFixture {
	t.Helper()
	f := &pcFixture{
		prov:     &fakeProvisioner{},
		packs:    &fakePackStarter{known: map[string]string{"clinic": "Phòng khám"}},
		sessions: &fakeSessionOpener{},
		auditor:  &captureAuditor{},
	}
	repo := &fakeProjectRepo{
		tenants:  map[string]*project.TenantRow{},
		profiles: map[string]project.Profile{},
		signals:  map[string]project.Signals{},
	}
	f.svc = NewProjectService(repo, &fakeProjectTenants{}, &fakeProjectDoctor{}, &fakeProjectApps{}, f.auditor, nil).
		WithCreate(f.prov, f.packs, f.sessions)
	return f
}

func validCreate() ProjectCreateInput {
	return ProjectCreateInput{Name: "Khách mới", Slug: "khach-moi", AdminEmail: "chu@khach.test",
		GeneratePassword: true, StartMode: "blank"}
}

func createCode(t *testing.T, err error) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a ValidationError", err)
	}
	return ve.Code
}

func TestProjectCreate_RefusesBadInputBeforeAnyWrite(t *testing.T) {
	tooLong := strings.Repeat("a", maxProjectIndustryRunes+1)
	tests := []struct {
		name     string
		mutate   func(*ProjectCreateInput)
		wantCode string
	}{
		{"unknown mode", func(in *ProjectCreateInput) { in.StartMode = "magic" }, "PROJECT_START_MODE_INVALID"},
		{"missing mode", func(in *ProjectCreateInput) { in.StartMode = "" }, "PROJECT_START_MODE_INVALID"},
		{"pack named on a blank start", func(in *ProjectCreateInput) { in.StartPack = "clinic" }, "PROJECT_START_MODE_INVALID"},
		{"pack mode without a pack", func(in *ProjectCreateInput) { in.StartMode = "pack" }, "VERTICAL_BUILTIN_NOT_FOUND"},
		{"pack that does not exist", func(in *ProjectCreateInput) { in.StartMode, in.StartPack = "pack", "nope" }, "VERTICAL_BUILTIN_NOT_FOUND"},
		{"industry too long", func(in *ProjectCreateInput) { in.Industry = tooLong }, "PROJECT_INDUSTRY_TOO_LONG"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPcFixture(t)
			in := validCreate()
			tc.mutate(&in)
			_, err := f.svc.Create(context.Background(), pcHome, in)
			if got := createCode(t, err); got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
			if f.prov.calls != 0 || f.packs.installs != 0 || f.sessions.calls != 0 {
				t.Errorf("provision/install/open calls = %d/%d/%d, want 0/0/0 - input must be checked before any write",
					f.prov.calls, f.packs.installs, f.sessions.calls)
			}
		})
	}
}

func TestProjectCreate_PackMode_SeedsProfileInstallsAfterProvisionAndOpensSession(t *testing.T) {
	f := newPcFixture(t)
	in := validCreate()
	in.StartMode, in.StartPack = "pack", " Clinic "
	in.Industry = "  "

	res, err := f.svc.Create(context.Background(), pcHome, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seed := f.prov.got.Project
	if seed == nil || seed.Origin != project.OriginPack || seed.OriginPack == nil || *seed.OriginPack != "clinic" || seed.CreatedBy != "builder-1" {
		t.Fatalf("seed = %+v, want origin pack / originPack clinic / createdBy builder-1", seed)
	}
	if seed.Industry == nil || *seed.Industry != "Phòng khám" {
		t.Errorf("industry = %v, want the pack's name when none was given (FR-036)", seed.Industry)
	}
	if f.prov.got.ActorTenantID != "home-tenant" {
		t.Errorf("ActorTenantID = %q, want the builder's home tenant", f.prov.got.ActorTenantID)
	}
	if f.packs.installs != 1 || f.packs.gotTenant != "new-tenant" || f.packs.gotUser != "builder-1" || f.packs.gotSlug != "clinic" {
		t.Errorf("install = %d x (%q, %q, %q), want once into new-tenant by builder-1 for clinic",
			f.packs.installs, f.packs.gotTenant, f.packs.gotUser, f.packs.gotSlug)
	}
	if f.packs.gotActor != "builder-1" {
		t.Errorf("install ctx actor = %q, want builder-1 so the customer's trail names the builder", f.packs.gotActor)
	}
	if res.Start.Mode != "pack" || res.Start.Status != "done" || res.Start.Result == nil || res.Start.Result.EntitiesCreated != 3 {
		t.Errorf("start = %+v, want pack/done with the install result", res.Start)
	}
	if res.Workspace == nil || res.Workspace.AccessToken != "ws-token" || f.sessions.gotSlug != "khach-moi" || f.sessions.gotHome.UserID != "builder-1" {
		t.Errorf("workspace = %+v (open slug %q), want a session for the new slug opened by the builder", res.Workspace, f.sessions.gotSlug)
	}
	if res.OneTimePassword != "s3cret-once" || res.Project == nil || res.Project.TenantID != "new-tenant" {
		t.Errorf("result = %+v, want the one-time password and the new project row", res)
	}
}

// The caller hanging up after the tenant is committed must not abort the install, the builder
// session or the audit row (FR-034): a half-started project whose password nobody received.
func TestProjectCreate_CallerDisconnectAfterCommitDoesNotAbortTheRest(t *testing.T) {
	f := newPcFixture(t)
	f.packs.honourCancel = true
	ctx, cancel := context.WithCancel(context.Background())
	f.prov.afterProvision = cancel // client goes away right after the commit
	in := validCreate()
	in.StartMode, in.StartPack = "pack", "clinic"

	res, err := f.svc.Create(ctx, pcHome, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Start.Status != "done" {
		t.Errorf("start = %+v, want done despite the cancelled request ctx", res.Start)
	}
	if res.Workspace == nil {
		t.Error("workspace missing: the session open was aborted by the cancelled request ctx")
	}
}

func TestProjectCreate_BlankAndAIStartsSkipTheInstall(t *testing.T) {
	for mode, wantOrigin := range map[string]string{"blank": project.OriginBlank, "ai": project.OriginAI} {
		t.Run(mode, func(t *testing.T) {
			f := newPcFixture(t)
			in := validCreate()
			in.StartMode = mode
			in.Industry = " Giáo dục "
			res, err := f.svc.Create(context.Background(), pcHome, in)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if f.packs.installs != 0 {
				t.Errorf("installs = %d, want 0", f.packs.installs)
			}
			seed := f.prov.got.Project
			if seed.Origin != wantOrigin || seed.OriginPack != nil || seed.Industry == nil || *seed.Industry != "Giáo dục" {
				t.Errorf("seed = %+v, want origin %s, no pack, trimmed industry", seed, wantOrigin)
			}
			if res.Start.Status != "skipped" || res.Start.Mode != mode {
				t.Errorf("start = %+v, want %s/skipped", res.Start, mode)
			}
			if res.Workspace == nil {
				t.Error("workspace missing - a blank / AI project still opens the builder session")
			}
		})
	}
}

func TestProjectCreate_InstallFailureKeepsTheProject(t *testing.T) {
	f := newPcFixture(t)
	f.packs.installErr = &ValidationError{Code: "VERTICAL_PACK_EMPTY", Msg: "pack rỗng"}
	in := validCreate()
	in.StartMode, in.StartPack = "pack", "clinic"
	res, err := f.svc.Create(context.Background(), pcHome, in)
	if err != nil {
		t.Fatalf("Create returned %v, want success with start.status=failed (FR-034)", err)
	}
	if res.Start.Status != "failed" || res.Start.Error == nil || res.Start.Error.Code != "VERTICAL_PACK_EMPTY" || res.Start.Result != nil {
		t.Errorf("start = %+v, want failed with the pack's code", res.Start)
	}
	if res.Workspace == nil {
		t.Error("workspace missing - the builder must still be able to enter and repair the project")
	}

	// an unexpected error reports a generic code, never its text
	f2 := newPcFixture(t)
	f2.packs.installErr = errors.New("dial tcp 10.0.0.9: secret-host refused")
	res2, err := f2.svc.Create(context.Background(), pcHome, in)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Start.Error == nil || res2.Start.Error.Code != "VERTICAL_INSTALL_FAILED" || strings.Contains(res2.Start.Error.Message, "secret-host") {
		t.Errorf("start.error = %+v, want VERTICAL_INSTALL_FAILED without internals", res2.Start.Error)
	}
}

func TestProjectCreate_SessionFailureKeepsTheProject(t *testing.T) {
	f := newPcFixture(t)
	f.sessions.err = ErrWorkspaceUnauthorized
	res, err := f.svc.Create(context.Background(), pcHome, validCreate())
	if err != nil {
		t.Fatalf("Create returned %v, want success without workspace (FR-034)", err)
	}
	if res.Workspace != nil || res.Project == nil {
		t.Errorf("result = %+v, want a project and no workspace", res)
	}
}

func TestProjectCreate_ProvisionRefusalStopsEverything(t *testing.T) {
	f := newPcFixture(t)
	f.prov.err = &ValidationError{Code: "TENANT_ALREADY_EXISTS", Msg: "đã tồn tại"}
	in := validCreate()
	in.StartMode, in.StartPack = "pack", "clinic"
	_, err := f.svc.Create(context.Background(), pcHome, in)
	if got := createCode(t, err); got != "TENANT_ALREADY_EXISTS" {
		t.Errorf("code = %q, want TENANT_ALREADY_EXISTS", got)
	}
	if f.packs.installs != 0 || f.sessions.calls != 0 || len(f.auditor.events) != 0 {
		t.Errorf("installs/opens/audits = %d/%d/%d after a refused provision, want 0/0/0",
			f.packs.installs, f.sessions.calls, len(f.auditor.events))
	}
}

func TestProjectCreate_AuditsInTheHomeTenantWithoutSecrets(t *testing.T) {
	f := newPcFixture(t)
	in := validCreate()
	in.StartMode, in.StartPack = "pack", "clinic"
	if _, err := f.svc.Create(context.Background(), pcHome, in); err != nil {
		t.Fatal(err)
	}
	var evt *audit.Event
	for i := range f.auditor.events {
		if f.auditor.events[i].EntityName == audit.ProjectMeta && f.auditor.events[i].Action == audit.ActionCreate {
			evt = &f.auditor.events[i]
		}
	}
	if evt == nil {
		t.Fatalf("no %s/create audit event in %+v", audit.ProjectMeta, f.auditor.events)
	}
	if evt.TenantID != "home-tenant" || evt.ActorUserID != "builder-1" || evt.RecordID != "new-tenant" {
		t.Errorf("audit = {tenant:%q actor:%q record:%q}, want {home-tenant builder-1 new-tenant}", evt.TenantID, evt.ActorUserID, evt.RecordID)
	}
	if raw := evt.Diff; raw != nil {
		if s := strings.ToLower(strings.Join(flattenValues(raw.After), " ")); strings.Contains(s, "s3cret-once") || strings.Contains(s, "password") {
			t.Errorf("audit diff leaks a secret: %v", raw.After)
		}
	}
}

func flattenValues(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k)
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
