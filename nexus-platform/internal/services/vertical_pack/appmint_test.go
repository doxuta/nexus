package vertical_pack

// Unit tests for Pass 9 (app mint — Phase 4, ../specs/010-app-container/spec.md §0.2) over in-memory fakes:
// a successful install mints ONE sys_app whose nav follows PACK order (entities then
// dashboards), a re-install leaves the existing app untouched (no dup, no error), a mint
// failure never fails the install (recorded on the result instead), and the nil optional
// dep keeps the pre-App behavior (back-compat: no mint at all).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// fakeMintMeta is an in-memory entity registry satisfying entityReader + entityWriter: writes
// through CreateEntity are visible to ListEntities, mirroring how the real install's post-mint
// registry read resolves the entities it just created.
type fakeMintMeta struct {
	mu   sync.Mutex
	seq  int
	defs map[string][]metadata.EntityDefinition // tenantID → defs
}

func (f *fakeMintMeta) ListEntities(tenantID string) ([]metadata.EntityDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]metadata.EntityDefinition(nil), f.defs[tenantID]...), nil
}

func (f *fakeMintMeta) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	return nil, nil
}

func (f *fakeMintMeta) CreateEntity(_ context.Context, tenantID string, in services.CreateEntityInput) (*metadata.EntityDefinition, []metadata.FieldDefinition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	def := metadata.EntityDefinition{ID: fmt.Sprintf("ent-%d", f.seq), TenantID: tenantID, Name: in.Name, Label: in.Label}
	if f.defs == nil {
		f.defs = map[string][]metadata.EntityDefinition{}
	}
	f.defs[tenantID] = append(f.defs[tenantID], def)
	return &def, nil, nil
}

func (f *fakeMintMeta) SoftDeleteEntity(_ context.Context, tenantID, entityName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.defs[tenantID][:0]
	for _, d := range f.defs[tenantID] {
		if d.Name != entityName {
			kept = append(kept, d)
		}
	}
	f.defs[tenantID] = kept
	return nil
}

// idOf resolves a registered entity name to its fake id (test helper).
func (f *fakeMintMeta) idOf(t *testing.T, tenantID, name string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.defs[tenantID] {
		if d.Name == name {
			return d.ID
		}
	}
	t.Fatalf("fakeMintMeta: entity %q not registered for %s", name, tenantID)
	return ""
}

// fakeMintRoles satisfies roleReader (buildPlan reads roles unconditionally).
type fakeMintRoles struct{}

func (fakeMintRoles) ListRoles(context.Context, string) ([]auth.Role, error) { return nil, nil }
func (fakeMintRoles) ListActionsForRole(context.Context, string, string) ([]auth.RoleAction, error) {
	return nil, nil
}

// fakeMintDash is an in-memory dashboardWriter (Pass 7 + the mint's name→id nav resolve).
type fakeMintDash struct {
	mu   sync.Mutex
	seq  int
	recs map[string][]dashboard.Record
}

func (f *fakeMintDash) ListDashboards(_ context.Context, tenantID string) ([]dashboard.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dashboard.Record(nil), f.recs[tenantID]...), nil
}

func (f *fakeMintDash) CreateDashboard(_ context.Context, tenantID string, in services.CreateDashboardInput) (*dashboard.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	rec := dashboard.Record{ID: fmt.Sprintf("dash-%d", f.seq), TenantID: tenantID, Name: in.Name}
	if f.recs == nil {
		f.recs = map[string][]dashboard.Record{}
	}
	f.recs[tenantID] = append(f.recs[tenantID], rec)
	return &rec, nil
}

// fakeAppMinter is an in-memory appMinter: duplicate slug per tenant → APP_ALREADY_EXISTS
// (same ValidationError code the real *services.AppService returns). Stores full app rows per
// tenant so ListApps (the F7-3 default pre-check) sees prior mints and seeded defaults.
type fakeAppMinter struct {
	mu          sync.Mutex
	seq         int
	createErr   error                     // when set, CreateApp fails with it
	navErr      error                     // when set, ReplaceNav fails with it
	listErr     error                     // when set, ListApps fails with it (default pre-check unavailable)
	apps        map[string][]*appmeta.App // tenantID → apps
	createCalls []services.CreateAppInput
	navCalls    int
	lastNav     []services.AppNavItemInput
	lastNavApp  string
}

// seedApp pre-loads an existing app (test setup — e.g. an admin-created default).
func (f *fakeAppMinter) seedApp(tenantID, name string, isDefault bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	if f.apps == nil {
		f.apps = map[string][]*appmeta.App{}
	}
	f.apps[tenantID] = append(f.apps[tenantID], &appmeta.App{
		ID: fmt.Sprintf("app-%d", f.seq), TenantID: tenantID, Name: name, Label: name, IsDefault: isDefault,
	})
}

// appsOf returns the tenant's stored apps (test helper).
func (f *fakeAppMinter) appsOf(tenantID string) []*appmeta.App {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*appmeta.App(nil), f.apps[tenantID]...)
}

func (f *fakeAppMinter) CreateApp(_ context.Context, tenantID string, in services.CreateAppInput) (*appmeta.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, in)
	if f.createErr != nil {
		return nil, f.createErr
	}
	for _, a := range f.apps[tenantID] {
		if a.Name == in.Name {
			return nil, invalid("APP_ALREADY_EXISTS", "app %q đã tồn tại", in.Name)
		}
	}
	f.seq++
	id := fmt.Sprintf("app-%d", f.seq)
	if f.apps == nil {
		f.apps = map[string][]*appmeta.App{}
	}
	// Mirror the real repo Create: setting IsDefault clears the tenant's previous default in
	// the same write (appmeta.Create → clearDefault).
	if in.IsDefault {
		for _, a := range f.apps[tenantID] {
			a.IsDefault = false
		}
	}
	app := &appmeta.App{ID: id, TenantID: tenantID, Name: in.Name, Label: in.Label, IsDefault: in.IsDefault}
	f.apps[tenantID] = append(f.apps[tenantID], app)
	return app, nil
}

func (f *fakeAppMinter) ListApps(_ context.Context, tenantID string) ([]appmeta.App, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]appmeta.App, 0, len(f.apps[tenantID]))
	for _, a := range f.apps[tenantID] {
		out = append(out, *a)
	}
	return out, nil
}

func (f *fakeAppMinter) GetApp(_ context.Context, tenantID, id string) (*appmeta.App, []appmeta.NavItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.apps[tenantID] {
		if a.ID == id {
			return a, nil, nil
		}
	}
	return nil, nil, invalid("APP_NOT_FOUND", "không tìm thấy app")
}

func (f *fakeAppMinter) ReplaceNav(_ context.Context, _ string, appID string, items []services.AppNavItemInput) ([]appmeta.NavItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.navErr != nil {
		return nil, f.navErr
	}
	f.navCalls++
	f.lastNavApp = appID
	f.lastNav = append([]services.AppNavItemInput(nil), items...)
	// Reflect reality: after a successful nav write the app is no longer empty. mintApp reads
	// NavCount to decide whether an EXISTING app is a half-built leftover worth filling or a live
	// one that must be left alone — a fake that always reports 0 would make every re-install look
	// like a repair.
	for _, list := range f.apps {
		for _, a := range list {
			if a.ID == appID {
				a.NavCount = len(items)
			}
		}
	}
	return nil, nil
}

// newMintTestService wires a Service over the fakes; minter nil = optional dep absent.
func newMintTestService(meta *fakeMintMeta, dash *fakeMintDash, minter *fakeAppMinter) *Service {
	d := Deps{
		EntityReader: meta,
		RoleReader:   fakeMintRoles{},
		EntityWriter: meta,
	}
	if dash != nil {
		d.DashboardWriter = dash // avoid a typed-nil interface tripping the != nil guards
	}
	s := NewService(d)
	if minter != nil {
		s.WithAppService(minter)
	}
	return s
}

// mintPack lists the CHILD entity first: dependency order creates crm_accounts before
// crm_contacts, so a nav that follows pack order (contacts, accounts, dashboard) proves the
// mint uses the pack's authored order, not creation order.
func mintPack() *Pack {
	return &Pack{
		Manifest: &Manifest{Name: "CRM", Slug: "crm", Version: "1.0.0"},
		Entities: []PackEntity{
			{Name: "crm_contacts", Label: "Liên hệ", Fields: []PackField{
				{Name: "name", Label: "Tên", FieldType: "text"},
				{Name: "account", Label: "Khách hàng", FieldType: "relation", Options: map[string]any{"relatedEntity": "crm_accounts"}},
			}},
			{Name: "crm_accounts", Label: "Khách hàng", Fields: []PackField{
				{Name: "name", Label: "Tên", FieldType: "text"},
			}},
		},
		Dashboards: []PackDashboard{{Name: "Bán hàng", Widgets: []PackWidget{
			{ID: "w1", Title: "Đếm liên hệ", Type: "kpi", Entity: "crm_contacts", Metric: "count"},
		}}},
	}
}

func TestInstall_MintsApp_NavInPackOrder(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
	svc := newMintTestService(meta, dash, minter)

	res, err := svc.Install(context.Background(), "t1", "u1", mintPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.AppMinted || res.AppSlug != "crm" || res.AppError != "" {
		t.Fatalf("result = minted:%v slug:%q err:%q; want minted:true slug:crm err:empty",
			res.AppMinted, res.AppSlug, res.AppError)
	}
	if len(minter.createCalls) != 1 {
		t.Fatalf("CreateApp calls = %d; want 1", len(minter.createCalls))
	}
	in := minter.createCalls[0]
	if in.Name != "crm" || in.Label != "CRM" || in.Icon != "briefcase" || in.ActorID != "u1" {
		t.Errorf("CreateApp input = %+v; want name:crm label:CRM icon:briefcase actor:u1", in)
	}
	// F7-3: fresh tenant (no apps at all) → the minted app becomes the default lens.
	if !in.IsDefault {
		t.Errorf("CreateApp IsDefault = false; want true (fresh tenant has no default app)")
	}
	// Nav follows PACK order: contacts (listed first) before accounts (created first),
	// dashboard last.
	want := []services.AppNavItemInput{
		{ItemType: appmeta.ItemTypeEntity, RefID: meta.idOf(t, "t1", "crm_contacts")},
		{ItemType: appmeta.ItemTypeEntity, RefID: meta.idOf(t, "t1", "crm_accounts")},
		{ItemType: appmeta.ItemTypeDashboard, RefID: "dash-1"},
	}
	if len(minter.lastNav) != len(want) {
		t.Fatalf("nav items = %+v; want %+v", minter.lastNav, want)
	}
	for i := range want {
		if minter.lastNav[i] != want[i] {
			t.Errorf("nav[%d] = %+v; want %+v", i, minter.lastNav[i], want[i])
		}
	}
}

// TestInstall_MintsApp_UsesManifestIcon pins PACK-EXPORT-APPSLUG: an exported app carries the
// maker's chosen icon in Manifest.Icon, and mintApp uses it verbatim instead of the slug-derived
// fallback (which would be the generic "package" for a non-built-in slug).
func TestInstall_MintsApp_UsesManifestIcon(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
	svc := newMintTestService(meta, dash, minter)

	pack := mintPack()
	// Manifest slug is kebab (install validates it so); mintApp maps -→_ back to the app snake slug.
	pack.Manifest = &Manifest{Name: "Quản lý Sự kiện", Slug: "quan-ly-su-kien", Version: "1.0.0", Icon: "calendar-days"}

	res, err := svc.Install(context.Background(), "t1", "u1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.AppMinted || res.AppSlug != "quan_ly_su_kien" {
		t.Fatalf("result = minted:%v slug:%q; want minted:true slug:quan_ly_su_kien", res.AppMinted, res.AppSlug)
	}
	if len(minter.createCalls) != 1 {
		t.Fatalf("CreateApp calls = %d; want 1", len(minter.createCalls))
	}
	in := minter.createCalls[0]
	if in.Icon != "calendar-days" {
		t.Errorf("CreateApp icon = %q; want calendar-days (manifest icon, not slug fallback)", in.Icon)
	}
	if in.Label != "Quản lý Sự kiện" {
		t.Errorf("CreateApp label = %q; want Quản lý Sự kiện", in.Label)
	}
}

func TestInstall_Reinstall_AppLeftUntouched(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
	svc := newMintTestService(meta, dash, minter)
	ctx := context.Background()

	if _, err := svc.Install(ctx, "t1", "u1", mintPack()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	res2, err := svc.Install(ctx, "t1", "u1", mintPack())
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	// Idempotent skip: no dup app, no error recorded, existing nav NOT rewritten.
	if res2.AppMinted || res2.AppSlug != "crm" || res2.AppError != "" {
		t.Errorf("re-install result = minted:%v slug:%q err:%q; want minted:false slug:crm err:empty",
			res2.AppMinted, res2.AppSlug, res2.AppError)
	}
	if got := len(minter.appsOf("t1")); got != 1 {
		t.Errorf("apps after re-install = %d; want 1 (no dup)", got)
	}
	if minter.navCalls != 1 {
		t.Errorf("ReplaceNav calls = %d; want 1 (existing app untouched)", minter.navCalls)
	}
}

func TestInstall_AppMintFailure_InstallStillSucceeds(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{createErr: errors.New("boom")}
	svc := newMintTestService(meta, dash, minter)

	res, err := svc.Install(context.Background(), "t1", "u1", mintPack())
	if err != nil {
		t.Fatalf("install must not fail on app-mint failure, got: %v", err)
	}
	if len(res.EntitiesCreated) != 2 {
		t.Errorf("entities created = %v; want 2 (install committed)", res.EntitiesCreated)
	}
	if res.AppMinted || res.AppSlug != "crm" || res.AppError == "" {
		t.Errorf("result = minted:%v slug:%q err:%q; want minted:false slug:crm err recorded",
			res.AppMinted, res.AppSlug, res.AppError)
	}
	// AppError alone is not a signal: no screen reads it, so the install banner stayed green
	// while the app never appeared and nothing on screen said why. Warnings IS rendered.
	if !hasWarningContaining(res.Warnings, "KHÔNG tạo được app") {
		t.Errorf("warnings = %v; want the failure surfaced where the UI actually reads", res.Warnings)
	}
}

// hasWarningContaining reports whether any warning mentions sub.
func hasWarningContaining(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestInstall_NavFailure_RecordedButAppMinted(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{navErr: errors.New("nav down")}
	svc := newMintTestService(meta, dash, minter)

	res, err := svc.Install(context.Background(), "t1", "u1", mintPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.AppMinted || res.AppError == "" {
		t.Errorf("result = minted:%v err:%q; want minted:true (shell exists) + err recorded",
			res.AppMinted, res.AppError)
	}
	// The confusing branch: the app IS in the sidebar and is EMPTY. Silence here is the worst of
	// the three, because the symptom looks like a bug in the pack rather than a failed step.
	if !hasWarningContaining(res.Warnings, "CHƯA gắn được bảng nào") {
		t.Errorf("warnings = %v; want the empty-app cause explained", res.Warnings)
	}
}

func TestInstall_NilAppDep_NoMint(t *testing.T) {
	meta, dash := &fakeMintMeta{}, &fakeMintDash{}
	svc := newMintTestService(meta, dash, nil) // optional dep absent — back-compat

	res, err := svc.Install(context.Background(), "t1", "u1", mintPack())
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if res.AppMinted || res.AppSlug != "" || res.AppError != "" {
		t.Errorf("nil dep result = minted:%v slug:%q err:%q; want all zero", res.AppMinted, res.AppSlug, res.AppError)
	}
}

func TestInstall_NoManifestSlug_NoMint(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
	svc := newMintTestService(meta, dash, minter)

	pack := mintPack()
	pack.Manifest = nil // legacy manifest-less pack: no stable slug → nothing to mint
	res, err := svc.Install(context.Background(), "t1", "u1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if res.AppMinted || res.AppSlug != "" || len(minter.createCalls) != 0 {
		t.Errorf("manifest-less result = minted:%v slug:%q createCalls:%d; want no mint attempt",
			res.AppMinted, res.AppSlug, len(minter.createCalls))
	}
}

// TestInstall_Mint_DefaultOnlyWhenNone pins F7-3 (default app lens): the minted app becomes
// the tenant default IFF no default exists — an admin's explicit choice is never stolen, and
// an unknown state (list error) mints without the flag.
func TestInstall_Mint_DefaultOnlyWhenNone(t *testing.T) {
	t.Run("existing default → untouched, mint is NOT default", func(t *testing.T) {
		meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
		minter.seedApp("t1", "ops", true) // admin's explicit default
		svc := newMintTestService(meta, dash, minter)

		if _, err := svc.Install(context.Background(), "t1", "u1", mintPack()); err != nil {
			t.Fatalf("install: %v", err)
		}
		if in := minter.createCalls[0]; in.IsDefault {
			t.Errorf("CreateApp IsDefault = true; want false (tenant already has a default)")
		}
		for _, a := range minter.appsOf("t1") {
			if a.Name == "ops" && !a.IsDefault {
				t.Errorf("pre-existing default app lost its flag — admin choice was stolen")
			}
		}
	})

	t.Run("existing apps but NO default → mint becomes default", func(t *testing.T) {
		meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
		minter.seedApp("t1", "ops", false)
		svc := newMintTestService(meta, dash, minter)

		if _, err := svc.Install(context.Background(), "t1", "u1", mintPack()); err != nil {
			t.Fatalf("install: %v", err)
		}
		if in := minter.createCalls[0]; !in.IsDefault {
			t.Errorf("CreateApp IsDefault = false; want true (no default existed)")
		}
	})

	t.Run("pre-check list error → fail-open, mint WITHOUT default", func(t *testing.T) {
		meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{listErr: errors.New("db down")}
		svc := newMintTestService(meta, dash, minter)

		res, err := svc.Install(context.Background(), "t1", "u1", mintPack())
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !res.AppMinted {
			t.Fatalf("app not minted — list error must not block the mint")
		}
		if in := minter.createCalls[0]; in.IsDefault {
			t.Errorf("CreateApp IsDefault = true; want false (unknown state — never risk stealing)")
		}
	})
}

func TestInstall_KebabSlug_ConvertedToAppSnake(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
	svc := newMintTestService(meta, dash, minter)

	pack := mintPack()
	pack.Manifest = &Manifest{Name: "Field Service", Slug: "field-service", Version: "1.0.0"}
	res, err := svc.Install(context.Background(), "t1", "u1", pack)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !res.AppMinted || res.AppSlug != "field_service" {
		t.Fatalf("result = minted:%v slug:%q; want minted:true slug:field_service", res.AppMinted, res.AppSlug)
	}
	in := minter.createCalls[0]
	if in.Name != "field_service" || in.Icon != "package" {
		t.Errorf("CreateApp input = name:%q icon:%q; want field_service/package", in.Name, in.Icon)
	}
}

// TestInstall_Reinstall_RepairsAnEmptyApp pins the half-built state a re-install could not fix.
//
// mintApp writes the app, then its nav. If the nav write fails the tenant is left with an EMPTY
// app — and the code told them to re-run the install, while APP_ALREADY_EXISTS made the re-run stop
// before ever reaching the nav step. The customer got an app with nothing in it and the only real
// remedy was building the nav by hand, which is exactly what the factory exists to avoid.
//
// The repair is FILL-ONLY: an app that already has nav entries is still left alone (the test above),
// because a re-install must never overwrite a nav the customer or a later delivery arranged.
func TestInstall_Reinstall_RepairsAnEmptyApp(t *testing.T) {
	meta, dash, minter := &fakeMintMeta{}, &fakeMintDash{}, &fakeAppMinter{}
	svc := newMintTestService(meta, dash, minter)
	ctx := context.Background()

	// Delivery 1: the app is created but the nav write fails — the exact half-built outcome.
	minter.navErr = errors.New("nav write failed")
	if _, err := svc.Install(ctx, "t1", "u1", mintPack()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if minter.navCalls != 0 {
		t.Fatalf("nav unexpectedly written on the failing run (calls=%d)", minter.navCalls)
	}

	// Delivery 2: the operator re-runs the install, as the warning tells them to.
	minter.navErr = nil
	res, err := svc.Install(ctx, "t1", "u1", mintPack())
	if err != nil {
		t.Fatalf("re-install: %v", err)
	}
	if minter.navCalls != 1 {
		t.Fatalf("ReplaceNav calls = %d; want 1 — the empty app was never repaired", minter.navCalls)
	}
	if len(minter.createCalls) != 2 {
		t.Errorf("CreateApp attempts = %d; want 2 (the second one 409s and is adopted, not duplicated)", len(minter.createCalls))
	}
	if res.AppError != "" {
		t.Errorf("AppError = %q; a successful repair must not report an error", res.AppError)
	}
}
