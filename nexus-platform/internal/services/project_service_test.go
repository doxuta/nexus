package services

// specs/062-customer-workspace T075 — ProjectService against fakes: the registry list (who is
// shown, what each row carries), PATCH (validation, trial vs delivery, audit in the workshop's
// home tenant) and the health check (what counts toward the status, what is stored).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/audit"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/project"
)

const (
	pjHome = "home-tenant"
	pjA    = "tenant-a"
	pjB    = "tenant-b-retired"
	pjC    = "tenant-c-suspended"
)

type fakeProjectTenants struct{ items []TenantListItem }

func (f *fakeProjectTenants) ListTenants(context.Context) ([]TenantListItem, error) {
	return f.items, nil
}

type fakeProjectRepo struct {
	tenants  map[string]*project.TenantRow
	profiles map[string]project.Profile
	signals  map[string]project.Signals
	upserts  []project.Profile
	cols     [][]string
	askedIDs [][]string
}

func (f *fakeProjectRepo) TenantRow(_ context.Context, id string) (*project.TenantRow, error) {
	if t, ok := f.tenants[id]; ok {
		return t, nil
	}
	return nil, project.ErrNotFound
}

func (f *fakeProjectRepo) ListByTenants(_ context.Context, ids []string) (map[string]project.Profile, error) {
	f.askedIDs = append(f.askedIDs, ids)
	out := map[string]project.Profile{}
	for _, id := range ids {
		if p, ok := f.profiles[id]; ok {
			out[id] = p
		}
	}
	return out, nil
}

func (f *fakeProjectRepo) Signals(_ context.Context, ids []string) (map[string]project.Signals, error) {
	out := map[string]project.Signals{}
	for _, id := range ids {
		out[id] = f.signals[id]
	}
	return out, nil
}

func (f *fakeProjectRepo) Get(_ context.Context, tenantID string) (*project.Profile, error) {
	if p, ok := f.profiles[tenantID]; ok {
		return &p, nil
	}
	return nil, project.ErrNotFound
}

func (f *fakeProjectRepo) Upsert(_ context.Context, p *project.Profile, cols ...string) error {
	f.upserts = append(f.upserts, *p)
	f.cols = append(f.cols, cols)
	f.profiles[p.TenantID] = *p
	return nil
}

type fakeDelivery struct{ at map[string]time.Time }

func (f fakeDelivery) DeliveredAt(context.Context, []string) (map[string]time.Time, error) {
	return f.at, nil
}

type fakeProjectDoctor struct {
	results []doctor.CheckResult
	err     error
	gotTID  string
}

func (f *fakeProjectDoctor) Scan(_ context.Context, tenantID string) ([]doctor.CheckResult, error) {
	f.gotTID = tenantID
	return f.results, f.err
}

type fakeProjectApps struct {
	apps      []appmeta.App
	readiness map[string]*AppReadinessView
}

func (f *fakeProjectApps) ListApps(context.Context, string) ([]appmeta.App, error) {
	return f.apps, nil
}

func (f *fakeProjectApps) GetApp(_ context.Context, _, id string) (*appmeta.App, []appmeta.NavItem, error) {
	for i := range f.apps {
		if f.apps[i].ID == id {
			return &f.apps[i], []appmeta.NavItem{{ID: "nav-" + id}}, nil
		}
	}
	return nil, nil, errors.New("no app")
}

func (f *fakeProjectApps) Readiness(_ context.Context, _ string, items []appmeta.NavItem) (*AppReadinessView, error) {
	return f.readiness[strings.TrimPrefix(items[0].ID, "nav-")], nil
}

type pjFixture struct {
	svc      *ProjectService
	repo     *fakeProjectRepo
	doctor   *fakeProjectDoctor
	apps     *fakeProjectApps
	auditor  *captureAuditor
	delivery fakeDelivery
	now      time.Time
}

func newPjFixture(t *testing.T) *pjFixture {
	t.Helper()
	f := &pjFixture{
		repo: &fakeProjectRepo{
			tenants: map[string]*project.TenantRow{
				pjA: {ID: pjA, Slug: "a", Name: "Khách A", Status: "active"},
				pjC: {ID: pjC, Slug: "c", Name: "Khách C", Status: "suspended"},
			},
			profiles: map[string]project.Profile{},
			signals:  map[string]project.Signals{},
		},
		doctor:   &fakeProjectDoctor{},
		apps:     &fakeProjectApps{readiness: map[string]*AppReadinessView{}},
		auditor:  &captureAuditor{},
		delivery: fakeDelivery{at: map[string]time.Time{}},
		now:      time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC),
	}
	retired := f.now.Add(-time.Hour)
	tenants := &fakeProjectTenants{items: []TenantListItem{
		{TenantView: TenantView{ID: pjHome, Slug: "northwind", Name: "Nexus", Status: "active"}},
		{TenantView: TenantView{ID: pjA, Slug: "a", Name: "Khách A", Status: "active"}},
		{TenantView: TenantView{ID: pjB, Slug: "b", Name: "Khách B", Status: "active"}, RetiredAt: &retired},
		{TenantView: TenantView{ID: pjC, Slug: "c", Name: "Khách C", Status: "suspended"}},
	}}
	f.svc = NewProjectService(f.repo, tenants, f.doctor, f.apps, f.auditor, nil)
	f.svc.now = func() time.Time { return f.now }
	f.svc.WithDelivery(f.delivery)
	return f
}

func (f *pjFixture) actor() ProjectActor { return ProjectActor{TenantID: pjHome, UserID: "builder-1"} }

func projectIDs(vs []ProjectView) []string {
	out := make([]string, len(vs))
	for i := range vs {
		out[i] = vs[i].TenantID
	}
	return out
}

func TestProjectList_SkipsRetiredAndCaller_KeepsSuspended_DefaultsWithoutProfile(t *testing.T) {
	f := newPjFixture(t)
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	ids := projectIDs(got)
	if len(ids) != 2 || ids[0] != pjA || ids[1] != pjC {
		t.Fatalf("list = %v, want [a c] (no caller, no retired, suspended kept)", ids)
	}
	if got[1].TenantStatus != "suspended" {
		t.Fatalf("suspended tenant must show its status, got %q", got[1].TenantStatus)
	}
	a := got[0]
	if a.Stage != "building" || a.Health.Status != "unknown" || a.Health.CheckedAt != nil {
		t.Fatalf("no profile => building/unknown, got stage=%q health=%+v", a.Stage, a.Health)
	}
	// the repository is only asked about the surviving tenants
	for _, asked := range f.repo.askedIDs {
		for _, id := range asked {
			if id == pjHome || id == pjB {
				t.Fatalf("repo asked about excluded tenant %q", id)
			}
		}
	}
}

func TestProjectList_MergesCountsBrandedLastActiveDeliveryAndProfile(t *testing.T) {
	f := newPjFixture(t)
	active := f.now.Add(-2 * time.Hour)
	f.repo.signals[pjA] = project.Signals{
		Counts: project.Counts{Apps: 1, Entities: 3, CustomLayouts: 2, Workflows: 4, Approvals: 1, Roles: 2,
			Users: 5, StaffUsers: 4, Reports: 6, Dashboards: 7},
		Branded: true, LastActiveAt: &active,
	}
	industry, origin, pack := "Phòng khám", "pack", "clinic"
	f.repo.profiles[pjA] = project.Profile{TenantID: pjA, Industry: &industry, Origin: &origin, OriginPack: &pack}
	delivered := f.now.Add(-48 * time.Hour)
	f.delivery.at[pjA] = delivered

	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	a := got[0]
	want := ProjectCounts{Apps: 1, Entities: 3, CustomLayouts: 2, Workflows: 4, Approvals: 1, Roles: 2,
		Users: 5, StaffUsers: 4, Reports: 6, Dashboards: 7}
	if a.Counts != want {
		t.Fatalf("counts = %+v, want %+v", a.Counts, want)
	}
	if !a.Branded || a.LastActiveAt == nil || !a.LastActiveAt.Equal(active) {
		t.Fatalf("branded/lastActive not merged: %+v", a)
	}
	if a.DeliveredAt == nil || !a.DeliveredAt.Equal(delivered) || a.Stage != "live" {
		t.Fatalf("delivered tenant must be live, got stage=%q deliveredAt=%v", a.Stage, a.DeliveredAt)
	}
	if a.Industry == nil || *a.Industry != industry || *a.Origin != "pack" || *a.OriginPack != "clinic" {
		t.Fatalf("profile fields not merged: %+v", a)
	}
}

func TestProjectList_OpsKeyInSnapshotNeverChangesStatusOrStage(t *testing.T) {
	f := newPjFixture(t)
	f.delivery.at[pjA] = f.now.Add(-time.Hour)
	status := "ok"
	f.repo.profiles[pjA] = project.Profile{
		TenantID: pjA, HealthStatus: &status,
		HealthJSON: json.RawMessage(`{"critical":0,"warn":0,"checks":[],"appsNotReady":[],"ops":{"status":"critical"}}`),
	}
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Health.Status != "ok" || got[0].Stage != "live" {
		t.Fatalf("ops signal leaked into status/stage: health=%q stage=%q", got[0].Health.Status, got[0].Stage)
	}
}

func TestProjectList_DeliveredAndCriticalSnapshotNeedsAttention(t *testing.T) {
	f := newPjFixture(t)
	f.delivery.at[pjA] = f.now.Add(-time.Hour)
	status := "critical"
	f.repo.profiles[pjA] = project.Profile{TenantID: pjA, HealthStatus: &status,
		HealthJSON: json.RawMessage(`{"critical":2,"warn":0,"checks":[{"checkId":"A1_MISSING_GEN_COLUMN","severity":"critical","count":2}],"appsNotReady":[]}`)}
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Stage != "needs_attention" || got[0].Health.Critical != 2 || len(got[0].Health.Checks) != 1 {
		t.Fatalf("got stage=%q health=%+v", got[0].Stage, got[0].Health)
	}
}

func TestProjectPatch(t *testing.T) {
	long := strings.Repeat("ă", 65) // 65 runes, 130 bytes — the column counts characters
	ok64 := strings.Repeat("ă", 64)
	yes, no := true, false
	nine := "Phòng khám"

	cases := []struct {
		name     string
		tenant   string
		delivery bool
		profile  *project.Profile
		in       ProjectPatch
		wantCode string
		check    func(t *testing.T, f *pjFixture, v *ProjectView)
	}{
		{name: "industry 65 runes", tenant: pjA, in: ProjectPatch{IndustrySet: true, Industry: &long}, wantCode: "PROJECT_INDUSTRY_TOO_LONG"},
		{name: "industry 64 runes ok", tenant: pjA, in: ProjectPatch{IndustrySet: true, Industry: &ok64},
			check: func(t *testing.T, _ *pjFixture, v *ProjectView) {
				if v.Industry == nil || *v.Industry != ok64 {
					t.Fatalf("industry not stored: %+v", v.Industry)
				}
			}},
		{name: "trial after delivery", tenant: pjA, delivery: true, in: ProjectPatch{Trial: &yes}, wantCode: "PROJECT_TRIAL_AFTER_DELIVERY"},
		{name: "unknown tenant", tenant: "nope", in: ProjectPatch{Trial: &yes}, wantCode: "TENANT_NOT_FOUND"},
		{name: "trial on", tenant: pjA, in: ProjectPatch{Trial: &yes},
			check: func(t *testing.T, f *pjFixture, v *ProjectView) {
				if v.Stage != "trial" || v.TrialSince == nil || !v.TrialSince.Equal(f.now) {
					t.Fatalf("want trial since now, got stage=%q since=%v", v.Stage, v.TrialSince)
				}
			}},
		{name: "trial on keeps the original start", tenant: pjA,
			profile: &project.Profile{TenantID: pjA, TrialSince: ptrTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))},
			in:      ProjectPatch{Trial: &yes},
			check: func(t *testing.T, _ *pjFixture, v *ProjectView) {
				if v.TrialSince == nil || v.TrialSince.Month() != time.September {
					t.Fatalf("trial start moved: %v", v.TrialSince)
				}
			}},
		{name: "trial off", tenant: pjA,
			profile: &project.Profile{TenantID: pjA, TrialSince: ptrTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))},
			in:      ProjectPatch{Trial: &no},
			check: func(t *testing.T, _ *pjFixture, v *ProjectView) {
				if v.Stage != "building" || v.TrialSince != nil {
					t.Fatalf("want building, got %q %v", v.Stage, v.TrialSince)
				}
			}},
		{name: "trial off after delivery is allowed (clears the mark)", tenant: pjA, delivery: true,
			profile: &project.Profile{TenantID: pjA, TrialSince: ptrTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))},
			in:      ProjectPatch{Trial: &no},
			check: func(t *testing.T, _ *pjFixture, v *ProjectView) {
				if v.Stage != "live" || v.TrialSince != nil {
					t.Fatalf("want live, got %q %v", v.Stage, v.TrialSince)
				}
			}},
		{name: "industry null clears", tenant: pjA,
			profile: &project.Profile{TenantID: pjA, Industry: &nine},
			in:      ProjectPatch{IndustrySet: true, Industry: nil},
			check: func(t *testing.T, _ *pjFixture, v *ProjectView) {
				if v.Industry != nil {
					t.Fatalf("industry not cleared: %v", *v.Industry)
				}
			}},
		{name: "industry blank clears", tenant: pjA,
			profile: &project.Profile{TenantID: pjA, Industry: &nine},
			in:      ProjectPatch{IndustrySet: true, Industry: ptrStr("   ")},
			check: func(t *testing.T, _ *pjFixture, v *ProjectView) {
				if v.Industry != nil {
					t.Fatalf("blank industry must clear, got %q", *v.Industry)
				}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPjFixture(t)
			if c.delivery {
				f.delivery.at[c.tenant] = f.now.Add(-time.Hour)
			}
			if c.profile != nil {
				f.repo.profiles[c.tenant] = *c.profile
			}
			v, err := f.svc.Patch(context.Background(), f.actor(), c.tenant, c.in)
			if c.wantCode != "" {
				if err == nil {
					t.Fatalf("want %s, got success", c.wantCode)
				}
				if got := codeOf(err); got != c.wantCode {
					t.Fatalf("code = %s, want %s", got, c.wantCode)
				}
				if len(f.repo.upserts) != 0 || len(f.auditor.events) != 0 {
					t.Fatal("a refused PATCH must write nothing")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, f, v)
			if len(f.repo.upserts) != 1 {
				t.Fatalf("want one upsert, got %d", len(f.repo.upserts))
			}
		})
	}
}

func TestProjectPatch_AuditsInTheWorkshopHomeTenant(t *testing.T) {
	f := newPjFixture(t)
	yes := true
	if _, err := f.svc.Patch(context.Background(), f.actor(), pjA, ProjectPatch{Trial: &yes}); err != nil {
		t.Fatal(err)
	}
	if len(f.auditor.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(f.auditor.events))
	}
	e := f.auditor.events[0]
	if e.TenantID != pjHome || e.EntityName != audit.ProjectMeta || e.Action != audit.ActionUpdate || e.RecordID != pjA {
		t.Fatalf("audit = tenant %q entity %q action %q record %q", e.TenantID, e.EntityName, e.Action, e.RecordID)
	}
	// the trail must name the builder even when the caller passed a bare request context
	if e.ActorUserID != f.actor().UserID {
		t.Fatalf("audit actor = %q, want %q", e.ActorUserID, f.actor().UserID)
	}
}

func TestProjectPatch_EmptyPatchWritesNothing(t *testing.T) {
	f := newPjFixture(t)
	v, err := f.svc.Patch(context.Background(), f.actor(), pjA, ProjectPatch{})
	if err != nil {
		t.Fatal(err)
	}
	if v == nil || v.TenantID != pjA {
		t.Fatalf("want the current view of %s, got %+v", pjA, v)
	}
	if len(f.repo.upserts) != 0 || len(f.auditor.events) != 0 {
		t.Fatalf("an empty PATCH must write no profile and no audit line, got %d upserts %d events", len(f.repo.upserts), len(f.auditor.events))
	}
}

func TestProjectPatch_OnlyTouchesTheColumnsItOwns(t *testing.T) {
	f := newPjFixture(t)
	yes := true
	if _, err := f.svc.Patch(context.Background(), f.actor(), pjA, ProjectPatch{Trial: &yes}); err != nil {
		t.Fatal(err)
	}
	for _, col := range f.repo.cols[0] {
		if strings.HasPrefix(col, "health") {
			t.Fatalf("PATCH must not overwrite %s (a concurrent health-check owns it)", col)
		}
	}
}

func TestProjectHealthCheck(t *testing.T) {
	res := func(id, sev string, count int, reportOnly bool) doctor.CheckResult {
		return doctor.CheckResult{CheckID: id, Severity: sev, Count: count, ReportOnly: reportOnly}
	}
	notReady := &AppReadinessView{EntityCount: 2, Ready: false, HasNonAdminViewRole: false}
	ready := &AppReadinessView{EntityCount: 2, Ready: true, AllEntitiesHaveFields: true, HasNonAdminViewRole: true, HasNonAdminUserAssigned: true}

	cases := []struct {
		name       string
		results    []doctor.CheckResult
		readiness  map[string]*AppReadinessView
		wantStatus string
		wantCrit   int
		wantWarn   int
		wantNotRdy int
	}{
		{"clean", nil, map[string]*AppReadinessView{"app1": ready}, "ok", 0, 0, 0},
		{"info and report-only are ignored", []doctor.CheckResult{res("I1", "info", 5, false), res("R1", "critical", 3, true), res("R2", "warn", 3, true)},
			map[string]*AppReadinessView{"app1": ready}, "ok", 0, 0, 0},
		{"zero-count findings are ignored", []doctor.CheckResult{res("W0", "warn", 0, false), res("C0", "critical", 0, false)},
			map[string]*AppReadinessView{"app1": ready}, "ok", 0, 0, 0},
		{"warn finding", []doctor.CheckResult{res("W1", "warn", 2, false)}, map[string]*AppReadinessView{"app1": ready}, "warn", 0, 2, 0},
		{"critical finding", []doctor.CheckResult{res("A1_MISSING_GEN_COLUMN", "critical", 1, false), res("W1", "warn", 2, false)},
			map[string]*AppReadinessView{"app1": ready}, "critical", 1, 2, 0},
		{"app not ready is a warn", nil, map[string]*AppReadinessView{"app1": notReady}, "warn", 0, 0, 1},
		{"readiness not wired (nil) is not a finding", nil, map[string]*AppReadinessView{}, "ok", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPjFixture(t)
			f.doctor.results = c.results
			f.apps.apps = []appmeta.App{{ID: "app1", Name: "crm", Label: "CRM"}}
			f.apps.readiness = c.readiness

			h, err := f.svc.HealthCheck(context.Background(), f.actor(), pjA)
			if err != nil {
				t.Fatal(err)
			}
			if f.doctor.gotTID != pjA {
				t.Fatalf("doctor scanned %q, want the customer %q", f.doctor.gotTID, pjA)
			}
			if h.Status != c.wantStatus || h.Critical != c.wantCrit || h.Warn != c.wantWarn || len(h.AppsNotReady) != c.wantNotRdy {
				t.Fatalf("health = %+v, want %s crit=%d warn=%d notReady=%d", h, c.wantStatus, c.wantCrit, c.wantWarn, c.wantNotRdy)
			}
			if h.CheckedAt == nil || !h.CheckedAt.Equal(f.now) {
				t.Fatalf("checkedAt = %v, want now", h.CheckedAt)
			}
			// the snapshot was stored, without touching industry/trial columns
			if len(f.repo.upserts) != 1 {
				t.Fatalf("want one upsert, got %d", len(f.repo.upserts))
			}
			p := f.repo.upserts[0]
			if p.HealthStatus == nil || *p.HealthStatus != c.wantStatus || len(p.HealthJSON) == 0 {
				t.Fatalf("stored snapshot wrong: %+v", p)
			}
			for _, col := range f.repo.cols[0] {
				if col == "industry" || col == "trial_since" {
					t.Fatalf("health-check must not overwrite %s", col)
				}
			}
			// the app's not-ready reasons travel with the snapshot
			if c.wantNotRdy == 1 {
				if h.AppsNotReady[0].AppID != "app1" || len(h.AppsNotReady[0].Reasons) == 0 {
					t.Fatalf("not-ready app lacks id/reasons: %+v", h.AppsNotReady[0])
				}
			}
			if len(f.auditor.events) != 1 || f.auditor.events[0].TenantID != pjHome || f.auditor.events[0].EntityName != audit.ProjectMeta {
				t.Fatalf("health-check must audit once in the home tenant, got %+v", f.auditor.events)
			}
			if got := f.auditor.events[0].ActorUserID; got != f.actor().UserID {
				t.Fatalf("health-check audit actor = %q, want %q", got, f.actor().UserID)
			}
		})
	}
}

func TestProjectHealthCheck_UnknownOrRetiredTenant(t *testing.T) {
	f := newPjFixture(t)
	_, err := f.svc.HealthCheck(context.Background(), f.actor(), pjB)
	if got := codeOf(err); got != "TENANT_NOT_FOUND" {
		t.Fatalf("code = %s", got)
	}
	if f.doctor.gotTID != "" || len(f.repo.upserts) != 0 {
		t.Fatal("nothing may run for a retired/unknown tenant")
	}
}

func TestProjectHealthCheck_DoctorFailureIsAnErrorAndStoresNothing(t *testing.T) {
	f := newPjFixture(t)
	f.doctor.err = errors.New("boom")
	if _, err := f.svc.HealthCheck(context.Background(), f.actor(), pjA); err == nil {
		t.Fatal("want error")
	}
	if len(f.repo.upserts) != 0 {
		t.Fatal("a failed scan must not overwrite the last good snapshot")
	}
}

func ptrStr(s string) *string { return &s }

func TestProjectList_MaintenanceUntilOnlyWhileFrozen(t *testing.T) {
	f := newPjFixture(t)
	future, past := f.now.Add(time.Hour), f.now.Add(-time.Minute)
	ts := f.svc.tenants.(*fakeProjectTenants)
	for i := range ts.items {
		switch ts.items[i].ID {
		case pjA:
			ts.items[i].MaintenanceUntil = &future
		case pjC:
			ts.items[i].MaintenanceUntil = &past
		}
	}
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].MaintenanceUntil == nil || !got[0].MaintenanceUntil.Equal(future) {
		t.Fatalf("frozen tenant must carry maintenanceUntil, got %v", got[0].MaintenanceUntil)
	}
	if got[1].MaintenanceUntil != nil {
		t.Fatalf("expired freeze must be omitted, got %v", got[1].MaintenanceUntil)
	}
}

// specs/065 T074: the project card copies usableOwnerCount from the registry row - no extra query.
func TestProjectList_CarriesUsableOwnerCount(t *testing.T) {
	f := newPjFixture(t)
	ts := f.svc.tenants.(*fakeProjectTenants)
	for i := range ts.items {
		if ts.items[i].ID == pjA {
			ts.items[i].UsableOwnerCount = 2
		}
	}
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range got {
		want := 0
		if v.TenantID == pjA {
			want = 2
		}
		if v.UsableOwnerCount != want {
			t.Fatalf("tenant %s usableOwnerCount = %d, want %d", v.TenantID, v.UsableOwnerCount, want)
		}
	}
}

// specs/073 so3 T500: the card carries the backup signal (lastReadyAt, lastFailed) from the same Signals read.
func TestProjectList_CarriesBackupSignal(t *testing.T) {
	f := newPjFixture(t)
	ready := f.now.Add(-3 * time.Hour)
	f.repo.signals[pjA] = project.Signals{Backup: project.BackupSignal{LastReadyAt: &ready, LastFailed: true}}
	got, err := f.svc.List(context.Background(), pjHome)
	if err != nil {
		t.Fatal(err)
	}
	if b := got[0].Backup; b == nil || b.LastReadyAt == nil || !b.LastReadyAt.Equal(ready) || !b.LastFailed {
		t.Fatalf("backup signal not carried: %+v", got[0].Backup)
	}
	// no backup ever: the key is present (the UI reads "no backup in 48h" from it), empty
	if b := got[1].Backup; b == nil || b.LastReadyAt != nil || b.LastFailed {
		t.Fatalf("empty backup signal expected, got %+v", got[1].Backup)
	}
}
