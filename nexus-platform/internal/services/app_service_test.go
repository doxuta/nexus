package services

// Unit tests for AppService (../specs/010-app-container/spec.md §6): slug sanitize + duplicate slug,
// nav replace-all ref validation (cross-tenant / bad type / duplicate pair reject), and the
// INV-3 nav-read ACL filter (denied entity dropped with no existence leak, admin bypass,
// orphan-ref drop). The repository's transactional behaviours (default-clear, purge cascade)
// are covered by appmeta's unit suite + the integration suite.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/metadata"
)

// fakeAppStore is an in-memory appStore. Nav rows are stored per app in insertion order.
type fakeAppStore struct {
	apps map[string]*appmeta.App      // id → app
	nav  map[string][]appmeta.NavItem // app id → items
}

func newFakeAppStore() *fakeAppStore {
	return &fakeAppStore{apps: map[string]*appmeta.App{}, nav: map[string][]appmeta.NavItem{}}
}

func (f *fakeAppStore) Create(_ context.Context, in appmeta.CreateInput) (string, error) {
	for _, a := range f.apps {
		if a.TenantID == in.TenantID && a.Name == in.Name {
			return "", appmeta.ErrAppDuplicateName
		}
	}
	id := fmt.Sprintf("app-%d", len(f.apps)+1)
	if in.IsDefault {
		for _, a := range f.apps {
			if a.TenantID == in.TenantID {
				a.IsDefault = false
			}
		}
	}
	f.apps[id] = &appmeta.App{
		ID: id, TenantID: in.TenantID, Name: in.Name, Label: in.Label,
		IsDefault: in.IsDefault, SortOrder: in.SortOrder,
	}
	return id, nil
}

func (f *fakeAppStore) GetByID(_ context.Context, tenantID, id string) (*appmeta.App, error) {
	a, ok := f.apps[id]
	if !ok || a.TenantID != tenantID {
		return nil, appmeta.ErrAppNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeAppStore) GetByName(_ context.Context, tenantID, name string) (*appmeta.App, error) {
	for _, a := range f.apps {
		if a.TenantID == tenantID && a.Name == name {
			cp := *a
			return &cp, nil
		}
	}
	return nil, appmeta.ErrAppNotFound
}

func (f *fakeAppStore) List(_ context.Context, tenantID string) ([]appmeta.App, error) {
	var out []appmeta.App
	for _, a := range f.apps {
		if a.TenantID == tenantID {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeAppStore) Update(_ context.Context, tenantID, id string, in appmeta.UpdateInput) error {
	a, ok := f.apps[id]
	if !ok || a.TenantID != tenantID {
		return appmeta.ErrAppNotFound
	}
	a.Label = in.Label
	a.IsDefault = in.IsDefault
	a.SortOrder = in.SortOrder
	return nil
}

func (f *fakeAppStore) SoftDelete(_ context.Context, tenantID, id string) error {
	a, ok := f.apps[id]
	if !ok || a.TenantID != tenantID {
		return appmeta.ErrAppNotFound
	}
	delete(f.apps, id)
	delete(f.nav, id)
	return nil
}

func (f *fakeAppStore) ReplaceNav(_ context.Context, tenantID, appID string, items []appmeta.NavItemInput) error {
	rows := make([]appmeta.NavItem, 0, len(items))
	for i, it := range items {
		var label *string
		if it.Label != "" {
			l := it.Label
			label = &l
		}
		rows = append(rows, appmeta.NavItem{
			ID: fmt.Sprintf("nav-%d", i), TenantID: tenantID, AppID: appID,
			ItemType: it.ItemType, RefID: it.RefID, Label: label, SortOrder: i,
		})
	}
	f.nav[appID] = rows
	return nil
}

func (f *fakeAppStore) ListNav(_ context.Context, tenantID, appID string) ([]appmeta.NavItem, error) {
	var out []appmeta.NavItem
	for _, it := range f.nav[appID] {
		if it.TenantID == tenantID {
			out = append(out, it)
		}
	}
	return out, nil
}

// fakeAppEntities serves a fixed per-tenant entity registry.
type fakeAppEntities struct {
	byTenant map[string][]metadata.EntityDefinition
}

func (f *fakeAppEntities) ListEntities(tenantID string) ([]metadata.EntityDefinition, error) {
	return f.byTenant[tenantID], nil
}

// fakeAppDashboards serves a fixed per-tenant dashboard list.
type fakeAppDashboards struct{ byTenant map[string][]dashboard.Record }

func (f *fakeAppDashboards) List(_ context.Context, tenantID string) ([]dashboard.Record, error) {
	return f.byTenant[tenantID], nil
}

// fakeAppACL allows exactly the entity names in allow; err (when set) simulates an ACL fault.
type fakeAppACL struct {
	allow map[string]bool
	err   error
}

func (f *fakeAppACL) CanViewEntity(_ context.Context, _, _, entityName string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.allow[entityName], nil
}

func strp(s string) *string { return &s }

// newAppSvcFixture wires an AppService over the fakes: tenant-a owns entities accounts/contacts
// and dashboard dash-1; tenant-b owns entity leads-b (for cross-tenant reject tests).
func newAppSvcFixture(acl *fakeAppACL) (*AppService, *fakeAppStore) {
	store := newFakeAppStore()
	entities := &fakeAppEntities{byTenant: map[string][]metadata.EntityDefinition{
		"tenant-a": {
			{ID: "ent-accounts", TenantID: "tenant-a", Name: "accounts", Label: "Khách hàng", Options: strp(`{"icon":"building"}`)},
			{ID: "ent-contacts", TenantID: "tenant-a", Name: "contacts", Label: "Liên hệ"},
		},
		"tenant-b": {
			{ID: "ent-leads-b", TenantID: "tenant-b", Name: "leads", Label: "Leads B"},
		},
	}}
	dashboards := &fakeAppDashboards{byTenant: map[string][]dashboard.Record{
		"tenant-a": {{ID: "dash-1", TenantID: "tenant-a", Name: "KPI tổng"}},
	}}
	if acl == nil {
		acl = &fakeAppACL{allow: map[string]bool{"accounts": true, "contacts": true}}
	}
	return NewAppService(store, entities, dashboards, acl, nil), store
}

func TestAppService_CreateApp_Slug(t *testing.T) {
	tests := []struct {
		name    string
		input   CreateAppInput
		wantErr string // ValidationError code; "" = success
	}{
		{name: "slug hợp lệ", input: CreateAppInput{Name: "sales_crm", Label: "Sales"}},
		{name: "uppercase được hạ về lowercase", input: CreateAppInput{Name: "  SALES  ", Label: "Sales"}},
		{name: "khoảng trắng giữa chữ → reject", input: CreateAppInput{Name: "sales app", Label: "x"}, wantErr: "APP_NAME_INVALID"},
		{name: "ký tự đặc biệt → reject", input: CreateAppInput{Name: "sales!", Label: "x"}, wantErr: "APP_NAME_INVALID"},
		{name: "bắt đầu bằng số → reject", input: CreateAppInput{Name: "1sales", Label: "x"}, wantErr: "APP_NAME_INVALID"},
		{name: "rỗng → reject", input: CreateAppInput{Name: "", Label: "x"}, wantErr: "APP_NAME_INVALID"},
		{name: "quá 60 ký tự → reject", input: CreateAppInput{Name: strings.Repeat("a", 61), Label: "x"}, wantErr: "APP_NAME_INVALID"},
		{name: "thiếu label → reject", input: CreateAppInput{Name: "ok", Label: "  "}, wantErr: "APP_LABEL_REQUIRED"},
		// Security LOW-1/LOW-2: column-width + INT32 guards return 422, never a MySQL 500.
		{name: "label quá 100 rune → reject", input: CreateAppInput{Name: "ok", Label: strings.Repeat("é", 101)}, wantErr: "APP_FIELD_TOO_LONG"},
		{name: "label đúng 100 rune → ok", input: CreateAppInput{Name: "ok100", Label: strings.Repeat("é", 100)}},
		{name: "icon quá 50 rune → reject", input: CreateAppInput{Name: "ok", Label: "x", Icon: strings.Repeat("i", 51)}, wantErr: "APP_FIELD_TOO_LONG"},
		{name: "color quá 20 rune → reject", input: CreateAppInput{Name: "ok", Label: "x", Color: strings.Repeat("c", 21)}, wantErr: "APP_FIELD_TOO_LONG"},
		{name: "sortOrder ngoài INT32 → reject", input: CreateAppInput{Name: "ok", Label: "x", SortOrder: math.MaxInt32 + 1}, wantErr: "APP_SORT_ORDER_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newAppSvcFixture(nil)
			app, err := svc.CreateApp(context.Background(), "tenant-a", tt.input)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("CreateApp: %v", err)
				}
				if app.Name != strings.ToLower(strings.TrimSpace(tt.input.Name)) {
					t.Errorf("Name = %q; want lowercased trimmed slug", app.Name)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != tt.wantErr {
				t.Errorf("err = %v; want code %s", err, tt.wantErr)
			}
		})
	}
}

// TestValidateAppConfig_ParityWithCreateApp proves the exported config-time validator and the
// apply path share ONE body (Apps Phase 3 — the AI Builder's propose_app calls
// ValidateAppConfig): for every identity-field input, ValidateAppConfig and CreateApp either
// both accept or both reject with the SAME code. sortOrder stays zero — it is guarded outside
// ValidateAppConfig (the tool never proposes it).
func TestValidateAppConfig_ParityWithCreateApp(t *testing.T) {
	tests := []struct {
		name  string
		input CreateAppInput
	}{
		{"valid", CreateAppInput{Name: "spa_management", Label: "Quản lý Spa", Icon: "sparkles", Color: "beige"}},
		{"uppercase slug normalized", CreateAppInput{Name: "  SPA  ", Label: "Spa"}},
		{"bad slug", CreateAppInput{Name: "spa app", Label: "Spa"}},
		{"empty name", CreateAppInput{Name: "", Label: "Spa"}},
		{"slug over 60", CreateAppInput{Name: strings.Repeat("a", 61), Label: "Spa"}},
		{"label missing", CreateAppInput{Name: "spa", Label: "  "}},
		{"label over 100 runes", CreateAppInput{Name: "spa", Label: strings.Repeat("é", 101)}},
		{"icon over 50 runes", CreateAppInput{Name: "spa", Label: "Spa", Icon: strings.Repeat("i", 51)}},
		{"color over 20 runes", CreateAppInput{Name: "spa", Label: "Spa", Color: strings.Repeat("c", 21)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verr := ValidateAppConfig(tt.input.Name, tt.input.Label, tt.input.Icon, tt.input.Color)
			svc, _ := newAppSvcFixture(nil)
			_, cerr := svc.CreateApp(context.Background(), "tenant-a", tt.input)
			if (verr == nil) != (cerr == nil) {
				t.Fatalf("parity broken: ValidateAppConfig err=%v, CreateApp err=%v", verr, cerr)
			}
			if verr == nil {
				return
			}
			var vve, cve *ValidationError
			if !errors.As(verr, &vve) || !errors.As(cerr, &cve) {
				t.Fatalf("both must be ValidationError: %v vs %v", verr, cerr)
			}
			if vve.Code != cve.Code {
				t.Fatalf("codes diverge: ValidateAppConfig=%s, CreateApp=%s", vve.Code, cve.Code)
			}
		})
	}
}

func TestAppService_CreateApp_Duplicate(t *testing.T) {
	svc, _ := newAppSvcFixture(nil)
	ctx := context.Background()
	if _, err := svc.CreateApp(ctx, "tenant-a", CreateAppInput{Name: "sales", Label: "Sales"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.CreateApp(ctx, "tenant-a", CreateAppInput{Name: "sales", Label: "Sales 2"})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "APP_ALREADY_EXISTS" {
		t.Fatalf("err = %v; want APP_ALREADY_EXISTS", err)
	}
}

func TestAppService_ReplaceNav_Validation(t *testing.T) {
	newApp := func(t *testing.T) (*AppService, string) {
		t.Helper()
		svc, _ := newAppSvcFixture(nil)
		app, err := svc.CreateApp(context.Background(), "tenant-a", CreateAppInput{Name: "sales", Label: "Sales"})
		if err != nil {
			t.Fatalf("CreateApp: %v", err)
		}
		return svc, app.ID
	}

	t.Run("ref hợp lệ (entity + dashboard) → lưu theo thứ tự", func(t *testing.T) {
		svc, appID := newApp(t)
		saved, err := svc.ReplaceNav(context.Background(), "tenant-a", appID, []AppNavItemInput{
			{ItemType: "entity", RefID: "ent-accounts"},
			{ItemType: "dashboard", RefID: "dash-1", Label: "KPI"},
		})
		if err != nil {
			t.Fatalf("ReplaceNav: %v", err)
		}
		if len(saved) != 2 || saved[0].RefID != "ent-accounts" || saved[1].RefID != "dash-1" {
			t.Errorf("saved = %+v; want 2 items in order", saved)
		}
	})

	rejects := []struct {
		name  string
		items []AppNavItemInput
	}{
		{"refId entity của tenant KHÁC", []AppNavItemInput{{ItemType: "entity", RefID: "ent-leads-b"}}},
		{"refId entity không tồn tại", []AppNavItemInput{{ItemType: "entity", RefID: "ent-ghost"}}},
		{"refId dashboard không tồn tại", []AppNavItemInput{{ItemType: "dashboard", RefID: "dash-ghost"}}},
		{"itemType lạ", []AppNavItemInput{{ItemType: "report", RefID: "ent-accounts"}}},
		{"refId rỗng", []AppNavItemInput{{ItemType: "entity", RefID: "  "}}},
		{"cặp (type, ref) trùng lặp", []AppNavItemInput{
			{ItemType: "entity", RefID: "ent-accounts"},
			{ItemType: "entity", RefID: "ent-accounts"},
		}},
	}
	for _, tt := range rejects {
		t.Run(tt.name+" → APP_NAV_REF_INVALID", func(t *testing.T) {
			svc, appID := newApp(t)
			_, err := svc.ReplaceNav(context.Background(), "tenant-a", appID, tt.items)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "APP_NAV_REF_INVALID" {
				t.Errorf("err = %v; want APP_NAV_REF_INVALID", err)
			}
		})
	}

	t.Run("app id lạ → APP_NOT_FOUND", func(t *testing.T) {
		svc, _ := newApp(t)
		_, err := svc.ReplaceNav(context.Background(), "tenant-a", "app-ghost", nil)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "APP_NOT_FOUND" {
			t.Errorf("err = %v; want APP_NOT_FOUND", err)
		}
	})

	t.Run("nav label quá 100 rune → APP_FIELD_TOO_LONG", func(t *testing.T) {
		svc, appID := newApp(t)
		_, err := svc.ReplaceNav(context.Background(), "tenant-a", appID, []AppNavItemInput{
			{ItemType: "entity", RefID: "ent-accounts", Label: strings.Repeat("é", 101)},
		})
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "APP_FIELD_TOO_LONG" {
			t.Errorf("err = %v; want APP_FIELD_TOO_LONG", err)
		}
	})
}

func TestAppService_GetNav_ACLFilter(t *testing.T) {
	seedNav := func(t *testing.T, svc *AppService) string {
		t.Helper()
		ctx := context.Background()
		app, err := svc.CreateApp(ctx, "tenant-a", CreateAppInput{Name: "sales", Label: "Sales"})
		if err != nil {
			t.Fatalf("CreateApp: %v", err)
		}
		if _, err := svc.ReplaceNav(ctx, "tenant-a", app.ID, []AppNavItemInput{
			{ItemType: "entity", RefID: "ent-accounts"},
			{ItemType: "entity", RefID: "ent-contacts", Label: "Danh bạ"},
			{ItemType: "dashboard", RefID: "dash-1"},
		}); err != nil {
			t.Fatalf("ReplaceNav: %v", err)
		}
		return app.Name
	}
	userScope := RecordScope{UserID: "u1", RoleID: "role-1"}

	t.Run("entity bị deny bị ẨN (INV-3), dashboard vẫn hiện", func(t *testing.T) {
		acl := &fakeAppACL{allow: map[string]bool{"accounts": true}} // contacts denied
		svc, _ := newAppSvcFixture(acl)
		name := seedNav(t, svc)
		_, items, err := svc.GetNav(context.Background(), userScope, "tenant-a", name)
		if err != nil {
			t.Fatalf("GetNav: %v", err)
		}
		if len(items) != 2 {
			t.Fatalf("items = %+v; want 2 (accounts + dashboard, contacts hidden)", items)
		}
		if items[0].Type != "entity" || items[0].RefID != "ent-accounts" || items[0].Href != "/accounts" {
			t.Errorf("items[0] = %+v; want accounts entity with href /accounts", items[0])
		}
		if items[0].Icon != "building" {
			t.Errorf("items[0].Icon = %q; want options.icon 'building'", items[0].Icon)
		}
		if items[1].Type != "dashboard" || items[1].Href != "/dashboards/dash-1" || items[1].Label != "KPI tổng" {
			t.Errorf("items[1] = %+v; want dashboard with own name", items[1])
		}
		// RefName feeds the AI context resolver (specs/067); a missing one is silent (empty app.nav names).
		if items[0].RefName != "accounts" || items[1].RefName != "KPI tổng" {
			t.Errorf("RefName = %q / %q; want entity table name 'accounts' / dashboard name 'KPI tổng'", items[0].RefName, items[1].RefName)
		}
		// Wire contract of GET /apps/:name/nav is unchanged: RefName must never be serialized.
		raw, err := json.Marshal(items[0])
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(strings.ToLower(string(raw)), "refname") {
			t.Errorf("AppNavItemView JSON leaks refName: %s", raw)
		}
	})

	t.Run("admin bypass ACL — thấy đủ, label override được tôn trọng", func(t *testing.T) {
		acl := &fakeAppACL{allow: map[string]bool{}} // deny all — admin must bypass
		svc, _ := newAppSvcFixture(acl)
		name := seedNav(t, svc)
		_, items, err := svc.GetNav(context.Background(), RecordScope{UserID: "adm", IsAdmin: true}, "tenant-a", name)
		if err != nil {
			t.Fatalf("GetNav: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("items = %+v; want all 3 for admin", items)
		}
		if items[1].Label != "Danh bạ" {
			t.Errorf("items[1].Label = %q; want per-app override 'Danh bạ'", items[1].Label)
		}
	})

	t.Run("lỗi hạ tầng ACL → fail closed (error, không rò item)", func(t *testing.T) {
		acl := &fakeAppACL{err: errors.New("acl store down")}
		svc, _ := newAppSvcFixture(acl)
		name := seedNav(t, svc)
		if _, _, err := svc.GetNav(context.Background(), userScope, "tenant-a", name); err == nil {
			t.Fatal("GetNav should fail closed on an ACL fault")
		}
	})

	t.Run("ref mồ côi bị drop, không lỗi", func(t *testing.T) {
		acl := &fakeAppACL{allow: map[string]bool{"accounts": true, "contacts": true}}
		svc, store := newAppSvcFixture(acl)
		name := seedNav(t, svc)
		// Simulate an orphan: nav row survives while the entity is gone from the registry.
		store.nav["app-1"] = append(store.nav["app-1"], appmeta.NavItem{
			ID: "nav-x", TenantID: "tenant-a", AppID: "app-1", ItemType: "entity", RefID: "ent-deleted", SortOrder: 9,
		})
		_, items, err := svc.GetNav(context.Background(), userScope, "tenant-a", name)
		if err != nil {
			t.Fatalf("GetNav: %v", err)
		}
		for _, it := range items {
			if it.RefID == "ent-deleted" {
				t.Errorf("orphan ref leaked into nav: %+v", it)
			}
		}
	})

	t.Run("app lạ → APP_NOT_FOUND", func(t *testing.T) {
		svc, _ := newAppSvcFixture(nil)
		_, _, err := svc.GetNav(context.Background(), userScope, "tenant-a", "ghost")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "APP_NOT_FOUND" {
			t.Errorf("err = %v; want APP_NOT_FOUND", err)
		}
	})
}

// ── APP-DELETE-ORPHAN-ROLES (WAVE 6) ─────────────────────────────────────────────────────────────
//
// Creating an app auto-seeds two roles (app_<name>_staff / _manager); deleting the app used to
// leave them behind — found live: one create-delete test cycle left two orphan roles that had to
// be swept by hand. DeleteApp now offers the symmetric teardown, with the one guard that matters:
// a role SOMEONE STILL HOLDS is kept, because users.role_id is ON DELETE SET NULL and deleting it
// would silently drop those users to deny-all.

type fakeRoleCleanup struct {
	roles   map[string]*auth.Role // name → role
	deleted []string              // role ids passed to DeleteRole
}

func (f *fakeRoleCleanup) GetRoleByName(_ context.Context, _ string, name string) (*auth.Role, error) {
	r, ok := f.roles[name]
	if !ok {
		return nil, fmt.Errorf("role not found")
	}
	cp := *r
	return &cp, nil
}

func (f *fakeRoleCleanup) DeleteRole(_ context.Context, _ string, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

type fakeCleanupUsers struct{ users []auth.User }

func (f *fakeCleanupUsers) ListUsers(_ context.Context, _ string) ([]auth.User, error) {
	return f.users, nil
}

func TestDeleteApp_TakesSeededRolesWithIt(t *testing.T) {
	svc, store := newAppSvcFixture(nil)
	app, err := svc.CreateApp(context.Background(), "t1", CreateAppInput{Name: "thuexe", Label: "Cho thuê xe"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	_ = store // fixture keeps the row; DeleteApp reads it back for the name

	rc := &fakeRoleCleanup{roles: map[string]*auth.Role{
		"app_thuexe_staff":   {ID: "r-staff", Name: "app_thuexe_staff"},
		"app_thuexe_manager": {ID: "r-mgr", Name: "app_thuexe_manager"},
	}}
	svc.WithRoleCleanup(rc)
	svc.readinessUsers = &fakeCleanupUsers{} // no user holds anything

	if err := svc.DeleteApp(context.Background(), "t1", app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if len(rc.deleted) != 2 {
		t.Fatalf("deleted roles = %v; want both seeded roles removed", rc.deleted)
	}
}

func TestDeleteApp_KeepsRoleUsersStillHold(t *testing.T) {
	svc, _ := newAppSvcFixture(nil)
	created, err := svc.CreateApp(context.Background(), "t1", CreateAppInput{Name: "thuexe", Label: "Cho thuê xe"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	staffID := "r-staff"
	rc := &fakeRoleCleanup{roles: map[string]*auth.Role{
		"app_thuexe_staff":   {ID: staffID, Name: "app_thuexe_staff"},
		"app_thuexe_manager": {ID: "r-mgr", Name: "app_thuexe_manager"},
	}}
	svc.WithRoleCleanup(rc)
	// One real person still carries the staff role — un-giving it must be an ADMIN's decision,
	// never a side effect of deleting an app (role delete would SET NULL them into deny-all).
	svc.readinessUsers = &fakeCleanupUsers{users: []auth.User{{ID: "u1", RoleID: &staffID}}}

	if err := svc.DeleteApp(context.Background(), "t1", created.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	if len(rc.deleted) != 1 || rc.deleted[0] != "r-mgr" {
		t.Fatalf("deleted = %v; want ONLY r-mgr (r-staff is held by u1 and must survive)", rc.deleted)
	}
}

func TestDeleteApp_NoCleanupWiredKeepsOldBehaviour(t *testing.T) {
	svc, _ := newAppSvcFixture(nil)
	created, err := svc.CreateApp(context.Background(), "t1", CreateAppInput{Name: "thuexe", Label: "Cho thuê xe"})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	// No WithRoleCleanup → delete succeeds exactly as before the slice.
	if err := svc.DeleteApp(context.Background(), "t1", created.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
}
