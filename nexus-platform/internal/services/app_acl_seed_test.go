package services

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
)

// fakeSeedApps is an in-memory seedAppLookup fake keyed by (tenantID, id).
type fakeSeedApps struct {
	byID map[string]*appmeta.App // key: tenantID + "/" + id
}

func newFakeSeedApps(apps ...*appmeta.App) *fakeSeedApps {
	f := &fakeSeedApps{byID: map[string]*appmeta.App{}}
	for _, a := range apps {
		f.byID[a.TenantID+"/"+a.ID] = a
	}
	return f
}

func (f *fakeSeedApps) GetByID(_ context.Context, tenantID, id string) (*appmeta.App, error) {
	a, ok := f.byID[tenantID+"/"+id]
	if !ok {
		return nil, appmeta.ErrAppNotFound
	}
	return a, nil
}

// fakeSeedRoles is an in-memory seedRoleStore fake reproducing the real repository's contract:
// CreateRole rejects a duplicate (tenant, name); AddGrants requires the role to exist and
// silently drops a row that would collide with an already-held (entity, action) grant (mirrors
// the real ON CONFLICT DO NOTHING), so callers can observe the exact same idempotency the
// production auth.RoleRepository provides.
type fakeSeedRoles struct {
	byName       map[string]*auth.Role // key: tenantID + "/" + name
	byID         map[string]*auth.Role
	grants       map[string][]auth.RoleAction // key: roleID
	createCalls  int
	createErr    error
	addGrantsErr error
}

func newFakeSeedRoles() *fakeSeedRoles {
	return &fakeSeedRoles{
		byName: map[string]*auth.Role{},
		byID:   map[string]*auth.Role{},
		grants: map[string][]auth.RoleAction{},
	}
}

func (f *fakeSeedRoles) GetRoleByName(_ context.Context, tenantID, name string) (*auth.Role, error) {
	r, ok := f.byName[tenantID+"/"+name]
	if !ok {
		return nil, auth.ErrRoleNotFound
	}
	return r, nil
}

func (f *fakeSeedRoles) CreateRole(_ context.Context, role *auth.Role, actions []auth.RoleAction) error {
	f.createCalls++
	if f.createErr != nil {
		return f.createErr
	}
	key := role.TenantID + "/" + role.Name
	if _, exists := f.byName[key]; exists {
		return auth.ErrRoleAlreadyExists
	}
	cp := *role
	f.byName[key] = &cp
	f.byID[role.ID] = &cp
	for i := range actions {
		actions[i].RoleID = role.ID
	}
	f.grants[role.ID] = append(f.grants[role.ID], actions...)
	return nil
}

func (f *fakeSeedRoles) AddGrants(_ context.Context, tenantID, roleID string, actions []auth.RoleAction) error {
	if f.addGrantsErr != nil {
		return f.addGrantsErr
	}
	r, ok := f.byID[roleID]
	if !ok || r.TenantID != tenantID {
		return auth.ErrRoleNotFound
	}
	for i := range actions {
		actions[i].RoleID = roleID
		dup := false
		for _, existing := range f.grants[roleID] {
			if existing.EntityName == actions[i].EntityName && existing.Action == actions[i].Action {
				dup = true
				break
			}
		}
		if !dup {
			f.grants[roleID] = append(f.grants[roleID], actions[i])
		}
	}
	return nil
}

// seedIDs returns a deterministic, exhaustion-free id generator for tests (real uuid.NewString
// would work too, but a counter makes failures easier to read).
func seedIDs() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%d", n)
	}
}

func grantsFor(roles *fakeSeedRoles, roleID string) []auth.RoleAction {
	return roles.grants[roleID]
}

func hasGrant(actions []auth.RoleAction, entity, action, level string) bool {
	for _, a := range actions {
		if a.EntityName == entity && a.Action == action && a.AccessLevel == level {
			return true
		}
	}
	return false
}

func TestAppACLSeedService_SeedForEntity(t *testing.T) {
	const tenant = "tenant-1"
	app := &appmeta.App{ID: "app-1", TenantID: tenant, Name: "myapp", Label: "App Của Tôi"}

	t.Run("vai mac dinh duoc tao va cap dung grant cho entity dau tien", func(t *testing.T) {
		apps := newFakeSeedApps(app)
		roles := newFakeSeedRoles()
		svc := NewAppACLSeedService(apps, roles)
		svc.newID = seedIDs()

		seeded, warning, err := svc.SeedForEntity(context.Background(), tenant, app.ID, "widgets")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !seeded {
			t.Fatalf("seeded = false, warning = %q; want true", warning)
		}
		if warning != "" {
			t.Fatalf("warning = %q on success; want empty", warning)
		}
		if roles.createCalls != 2 {
			t.Fatalf("createCalls = %d; want 2 (staff + manager)", roles.createCalls)
		}

		staff, err := roles.GetRoleByName(context.Background(), tenant, "app_myapp_staff")
		if err != nil {
			t.Fatalf("staff role not found: %v", err)
		}
		if staff.Label != "App Của Tôi — Nhân viên" {
			t.Errorf("staff label = %q", staff.Label)
		}
		mgr, err := roles.GetRoleByName(context.Background(), tenant, "app_myapp_manager")
		if err != nil {
			t.Fatalf("manager role not found: %v", err)
		}
		if mgr.Label != "App Của Tôi — Quản lý" {
			t.Errorf("manager label = %q", mgr.Label)
		}

		staffGrants := grantsFor(roles, staff.ID)
		if len(staffGrants) != 3 {
			t.Fatalf("staff grants = %d; want 3 (view/create/edit)", len(staffGrants))
		}
		// Founder 2026-07-29: "Thấy hết, chỉ sửa được của mình" — view@all, create/edit@owner.
		// Bản đầu để view@owner: đúng cho pipeline bán hàng, SAI cho bảng dùng chung (bảng dự án /
		// hàng đợi ticket render TRỐNG với người mới vào, đọc ra như "app hỏng").
		if !hasGrant(staffGrants, "widgets", "view", "all") {
			t.Errorf("staff phải có view@all (thấy hết): %+v", staffGrants)
		}
		for _, a := range []string{"create", "edit"} {
			if !hasGrant(staffGrants, "widgets", a, "owner") {
				t.Errorf("staff missing %s@owner on widgets: %+v", a, staffGrants)
			}
		}
		if hasGrant(staffGrants, "widgets", "view", "owner") {
			t.Errorf("staff KHÔNG được còn view@owner sau quyết định Founder: %+v", staffGrants)
		}
		if hasGrant(staffGrants, "widgets", "delete", "owner") || hasGrant(staffGrants, "widgets", "delete", "all") {
			t.Errorf("staff must NEVER hold delete: %+v", staffGrants)
		}

		mgrGrants := grantsFor(roles, mgr.ID)
		if len(mgrGrants) != 4 {
			t.Fatalf("manager grants = %d; want 4 (view/create/edit/delete)", len(mgrGrants))
		}
		for _, a := range []string{"view", "create", "edit", "delete"} {
			if !hasGrant(mgrGrants, "widgets", a, "all") {
				t.Errorf("manager missing %s@all on widgets: %+v", a, mgrGrants)
			}
		}
	})

	t.Run("entity thu hai cung app tai dung 2 vai, khong tao them, khong mat grant cu", func(t *testing.T) {
		apps := newFakeSeedApps(app)
		roles := newFakeSeedRoles()
		svc := NewAppACLSeedService(apps, roles)
		svc.newID = seedIDs()

		if _, _, err := svc.SeedForEntity(context.Background(), tenant, app.ID, "widgets"); err != nil {
			t.Fatalf("seed widgets: %v", err)
		}
		if roles.createCalls != 2 {
			t.Fatalf("after 1st entity: createCalls = %d; want 2", roles.createCalls)
		}

		seeded, _, err := svc.SeedForEntity(context.Background(), tenant, app.ID, "gadgets")
		if err != nil {
			t.Fatalf("seed gadgets: %v", err)
		}
		if !seeded {
			t.Fatal("seeded = false for 2nd entity")
		}
		// Idempotent reuse: still exactly 2 roles for this (tenant, app) — no 3rd/4th role.
		if roles.createCalls != 2 {
			t.Fatalf("after 2nd entity: createCalls = %d; want STILL 2 (reused, not recreated)", roles.createCalls)
		}

		staff, _ := roles.GetRoleByName(context.Background(), tenant, "app_myapp_staff")
		staffGrants := grantsFor(roles, staff.ID)
		if len(staffGrants) != 6 {
			t.Fatalf("staff grants after 2 entities = %d; want 6 (3 widgets + 3 gadgets)", len(staffGrants))
		}
		// The FIRST entity's grants must still be there — AddGrants must never replace.
		// Mức theo quyết định Founder 2026-07-29: view@all, create/edit@owner.
		lvl := map[string]string{"view": "all", "create": "owner", "edit": "owner"}
		for _, a := range []string{"view", "create", "edit"} {
			if !hasGrant(staffGrants, "widgets", a, lvl[a]) {
				t.Errorf("widgets grant %s@%s lost after seeding a 2nd entity: %+v", a, lvl[a], staffGrants)
			}
			if !hasGrant(staffGrants, "gadgets", a, lvl[a]) {
				t.Errorf("gadgets grant %s@%s missing: %+v", a, lvl[a], staffGrants)
			}
		}
	})

	t.Run("app khong ton tai -> fail-open, khong loi, khong seed", func(t *testing.T) {
		apps := newFakeSeedApps() // empty
		roles := newFakeSeedRoles()
		svc := NewAppACLSeedService(apps, roles)
		svc.newID = seedIDs()

		seeded, warning, err := svc.SeedForEntity(context.Background(), tenant, "ghost-app", "widgets")
		if err != nil {
			t.Fatalf("app-not-found must fail OPEN (nil err), got: %v", err)
		}
		if seeded {
			t.Fatal("seeded = true for a nonexistent app")
		}
		if warning == "" {
			t.Fatal("warning empty — FE has nothing honest to show")
		}
		if roles.createCalls != 0 {
			t.Fatalf("createCalls = %d; want 0 (nothing should be seeded)", roles.createCalls)
		}
	})

	t.Run("role store loi -> fail-open, seeded=false, warning + err boc", func(t *testing.T) {
		apps := newFakeSeedApps(app)
		roles := newFakeSeedRoles()
		roles.createErr = errors.New("boom: connection reset")
		svc := NewAppACLSeedService(apps, roles)
		svc.newID = seedIDs()

		seeded, warning, err := svc.SeedForEntity(context.Background(), tenant, app.ID, "widgets")
		if seeded {
			t.Fatal("seeded = true despite role-store error")
		}
		if warning == "" {
			t.Fatal("warning empty on failure")
		}
		if err == nil {
			t.Fatal("err must be non-nil so the caller can zap.Warn the real cause")
		}
		if !errors.Is(err, roles.createErr) {
			t.Fatalf("err = %v; want it to wrap the original createErr %v", err, roles.createErr)
		}
	})

	t.Run("AddGrants loi sau khi vai da co -> fail-open", func(t *testing.T) {
		apps := newFakeSeedApps(app)
		roles := newFakeSeedRoles()
		roles.addGrantsErr = errors.New("boom: deadlock")
		svc := NewAppACLSeedService(apps, roles)
		svc.newID = seedIDs()

		seeded, warning, err := svc.SeedForEntity(context.Background(), tenant, app.ID, "widgets")
		if seeded {
			t.Fatal("seeded = true despite AddGrants error")
		}
		if warning == "" {
			t.Fatal("warning empty on failure")
		}
		if err == nil {
			t.Fatal("err must be non-nil")
		}
		// The roles themselves were still created (fail-open leaves an empty-grant role behind,
		// which is harmless — it grants nothing until a retry succeeds).
		if roles.createCalls != 2 {
			t.Fatalf("createCalls = %d; want 2 (roles created before the grant step failed)", roles.createCalls)
		}
	})
}

// TestAppACLSeedService_InvalidatesACLCache proves the O3 contract: a role's cached grants must
// be dropped AFTER a successful seed, not left to expire on the TTL, so a staff/manager
// principal sees the newly seeded entity on their VERY NEXT request.
func TestAppACLSeedService_InvalidatesACLCache(t *testing.T) {
	const tenant = "tenant-1"
	app := &appmeta.App{ID: "app-1", TenantID: tenant, Name: "myapp", Label: "App"}
	apps := newFakeSeedApps(app)
	roles := newFakeSeedRoles()

	loadCalls := 0
	inner := aclLoaderFunc(func(_ context.Context, _, _ string) ([]auth.RoleAction, error) {
		loadCalls++
		return nil, nil
	})
	cache := NewCachedACLLoader(inner, DefaultACLTTL)

	svc := NewAppACLSeedService(apps, roles).WithACLCache(cache)
	svc.newID = seedIDs()

	// Warm the cache for the staff role BEFORE it exists (mirrors a request that ran moments
	// before the entity was created and cached "no grants").
	staffRoleID := "id-1" // first id the seeder mints, for the staff role (ensureRole runs first)
	if _, err := cache.LoadActionsForACL(context.Background(), tenant, staffRoleID); err != nil {
		t.Fatalf("warm cache: %v", err)
	}
	if loadCalls != 1 {
		t.Fatalf("loadCalls after warm = %d; want 1", loadCalls)
	}

	if _, _, err := svc.SeedForEntity(context.Background(), tenant, app.ID, "widgets"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A cache hit would keep loadCalls at 1; invalidation forces a reload.
	if _, err := cache.LoadActionsForACL(context.Background(), tenant, staffRoleID); err != nil {
		t.Fatalf("reload cache: %v", err)
	}
	if loadCalls != 2 {
		t.Fatalf("loadCalls after seed+reload = %d; want 2 (cache must be invalidated, not stale)", loadCalls)
	}
}

// aclLoaderFunc adapts a plain function to the aclActionLoader interface for the cache test.
type aclLoaderFunc func(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error)

func (f aclLoaderFunc) LoadActionsForACL(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	return f(ctx, tenantID, roleID)
}
