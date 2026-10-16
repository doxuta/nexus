//go:build integration

// specs/065-customer-owner-tier lát D + D2 (T071/T072/T073/T074): the reserved owner role seed,
// the convert / revert switch, the registry counts and the Provision default — over a freshly
// migrated throwaway MySQL DB (o3aSetup), so the CHECK constraint, the unique role name and the
// FOR UPDATE locking are the real ones.
package services

import (
	"context"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

type ownerTierRevoker struct {
	mu   sync.Mutex
	ids  []string
	fail bool
}

func (r *ownerTierRevoker) RevokeAllForUser(_ context.Context, _, userID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, userID)
	if r.fail {
		return context.DeadlineExceeded
	}
	return nil
}

type ownerTierInvalidator struct {
	mu  sync.Mutex
	ids []string
}

func (i *ownerTierInvalidator) Invalidate(_, userID string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.ids = append(i.ids, userID)
}

func otMkTenant(t *testing.T, gdb *gorm.DB, id, slug, name string) {
	t.Helper()
	if err := gdb.Create(&auth.Tenant{ID: id, Slug: slug, Name: name, Status: "active"}).Error; err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
}

func otMkUser(t *testing.T, gdb *gorm.DB, u auth.User) auth.User {
	t.Helper()
	if u.Status == "" {
		u.Status = "active"
	}
	if u.PasswordHash == "" {
		u.PasswordHash = "x"
	}
	if err := gdb.Create(&u).Error; err != nil {
		t.Fatalf("create user %s: %v", u.ID, err)
	}
	return u
}

func otLoad(t *testing.T, gdb *gorm.DB, id string) auth.User {
	t.Helper()
	var u auth.User
	if err := gdb.Where("id = ?", id).First(&u).Error; err != nil {
		t.Fatalf("load user %s: %v", id, err)
	}
	return u
}

func otCount(t *testing.T, gdb *gorm.DB, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := gdb.Raw(q, args...).Scan(&n).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func otIsCode(err error, code string) bool {
	ve, ok := err.(*ValidationError)
	return ok && ve.Code == code
}

func otItem(items []TenantListItem, id string) TenantListItem {
	for _, it := range items {
		if it.ID == id {
			return it
		}
	}
	return TenantListItem{}
}

// T071: create, idempotent, never touch an edited grant.
func TestSeedOwnerRole_CreateIdempotentAndHandsOff_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	otMkTenant(t, gdb, "t-seed", "seed", "Seed")
	ctx := context.Background()

	var id1 string
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		id, created, err := SeedOwnerRole(ctx, tx, "t-seed")
		if err != nil || !created || id == "" {
			t.Fatalf("first seed = (%q,%v,%v), want created", id, created, err)
		}
		id1 = id
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ? AND entity_name = '*' AND access_level = 'all' AND action IN ('view','create','edit','delete')`, id1); n != 4 {
		t.Fatalf("grants = %d, want 4", n)
	}

	// The builder narrowed the role: a re-seed must not put the grant back.
	if err := gdb.Exec(`DELETE FROM acl_role_actions WHERE role_id = ? AND action = 'delete'`, id1).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		id, created, err := SeedOwnerRole(ctx, tx, "t-seed")
		if err != nil || created || id != id1 {
			t.Fatalf("second seed = (%q,%v,%v), want (%q,false,nil)", id, created, err, id1)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?`, id1); n != 3 {
		t.Fatalf("grants after re-seed = %d, want 3 (edited grants untouched)", n)
	}
}

// T071 race: two transactions, one tenant => one role, four grants, same id, no deadlock.
func TestSeedOwnerRole_Race_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	otMkTenant(t, gdb, "t-race", "race", "Race")
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = gdb.Transaction(func(tx *gorm.DB) error {
				id, _, err := SeedOwnerRole(context.Background(), tx, "t-race")
				ids[i] = id
				return err
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("goroutine %d: %v", i, e)
		}
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("ids = %v, want the same non-empty id", ids)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_roles WHERE tenant_id = 't-race' AND name = ?`, auth.OwnerRoleName); n != 1 {
		t.Fatalf("roles = %d, want 1", n)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_role_actions WHERE role_id = ?`, ids[0]); n != 4 {
		t.Fatalf("grants = %d, want 4", n)
	}
}

// T073 + T074: convert / revert / counts / audit / no email in the result.
func TestSetOwnerTier_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	cap := &provAuditCapture{}
	rev := &ownerTierRevoker{}
	inv := &ownerTierInvalidator{}
	svc := NewTenantProvisionService(gdb).WithAuditor(cap).WithClaimCache(inv).WithTokenRevoker(rev)

	otMkTenant(t, gdb, "t-op", "op", "Xưởng Nexus")
	otMkTenant(t, gdb, "t-cus", "cus", "Khách")
	otMkUser(t, gdb, auth.User{ID: "u-op", TenantID: "t-op", Email: "op@x.test", FullName: "Nguyễn Vận Hành", IsAdmin: true, IsPlatformOperator: true})
	otMkUser(t, gdb, auth.User{ID: "u-a1", TenantID: "t-cus", Email: "a1@cus.test", IsAdmin: true})
	otMkUser(t, gdb, auth.User{ID: "u-a2", TenantID: "t-cus", Email: "a2@cus.test", IsAdmin: true})
	roleID := "r-pre"
	if err := gdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label, created_at, updated_at) VALUES (?, 't-cus', 'sales', 'Sales', NOW(), NOW())`, roleID).Error; err != nil {
		t.Fatal(err)
	}
	otMkUser(t, gdb, auth.User{ID: "u-a3", TenantID: "t-cus", Email: "a3@cus.test", IsAdmin: true, RoleID: &roleID})
	otMkUser(t, gdb, auth.User{ID: "u-staff", TenantID: "t-cus", Email: "s@cus.test"})
	ctx := audit.WithActor(context.Background(), "u-op")

	// guards
	if _, err := svc.SetOwnerTier(ctx, "t-op", "t-op", true); !otIsCode(err, "TENANT_SELF_OWNER_TIER") {
		t.Fatalf("self convert err = %v, want TENANT_SELF_OWNER_TIER", err)
	}
	if _, err := svc.SetOwnerTier(ctx, "t-op", "nope", true); !otIsCode(err, "TENANT_NOT_FOUND") {
		t.Fatalf("unknown tenant err = %v, want TENANT_NOT_FOUND", err)
	}

	// counts before
	items, err := svc.ListTenants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	it := otItem(items, "t-cus")
	if it.UsableAdminCount != 3 || it.UsableOwnerCount != 0 || it.ConvertibleAdminCount != 2 {
		t.Fatalf("before counts = %d/%d/%d, want admins 3 owners 0 convertible 2", it.UsableAdminCount, it.UsableOwnerCount, it.ConvertibleAdminCount)
	}

	// enable
	res, err := svc.SetOwnerTier(ctx, "t-op", "t-cus", true)
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if res.Converted != 2 || res.SkippedWithRole != 1 || res.OwnerRoleID == "" || res.RevokeFailed != 0 {
		t.Fatalf("enable result = %+v, want converted 2, skipped 1", res)
	}
	for _, id := range []string{"u-a1", "u-a2"} {
		u := otLoad(t, gdb, id)
		if u.IsAdmin || !u.CanManageUsers || u.RoleID == nil || *u.RoleID != res.OwnerRoleID {
			t.Fatalf("%s after convert = admin:%v owner:%v role:%v", id, u.IsAdmin, u.CanManageUsers, u.RoleID)
		}
	}
	if u := otLoad(t, gdb, "u-a3"); !u.IsAdmin || u.CanManageUsers {
		t.Fatalf("admin with a role must be skipped, got admin:%v owner:%v", u.IsAdmin, u.CanManageUsers)
	}
	if u := otLoad(t, gdb, "u-op"); !u.IsAdmin || !u.IsPlatformOperator {
		t.Fatal("the operator account was touched")
	}
	if len(rev.ids) != 2 || len(inv.ids) != 2 {
		t.Fatalf("revoked %v invalidated %v, want 2 each", rev.ids, inv.ids)
	}
	// again => 0
	res2, err := svc.SetOwnerTier(ctx, "t-op", "t-cus", true)
	if err != nil || res2.Converted != 0 || res2.OwnerRoleID != res.OwnerRoleID {
		t.Fatalf("second enable = %+v, %v; want converted 0 same role", res2, err)
	}

	// audit: per-user in the TARGET tenant, label without email/id, plus __tenant__ in the caller's.
	var userEvt, tenEvt int
	for _, e := range cap.events {
		switch {
		case e.EntityName == "__user__" && e.TenantID == "t-cus" && e.ActorUserID == "u-op" && e.Diff != nil:
			by, _ := e.Diff.After["by"].(string)
			if by != "Nguyễn Vận Hành (Xưởng Nexus)" {
				t.Errorf("audit by = %q", by)
			}
			if strings.Contains(by, "@") || strings.Contains(by, "u-op") {
				t.Errorf("audit label leaks identity: %q", by)
			}
			userEvt++
		case e.EntityName == "__tenant__" && e.Action == "owner_tier" && e.TenantID == "t-op":
			tenEvt++
		}
	}
	if userEvt != 2 || tenEvt != 1 { // the idempotent second call changed nothing, so it audits nothing
		t.Fatalf("audit rows user=%d tenant=%d, want 2 and 1", userEvt, tenEvt)
	}

	// counts after: owners 2, convertible 0
	items, _ = svc.ListTenants(ctx)
	it = otItem(items, "t-cus")
	if it.UsableOwnerCount != 2 || it.ConvertibleAdminCount != 0 || it.UsableAdminCount != 1 {
		t.Fatalf("after counts = %d/%d/%d, want admins 1 owners 2 convertible 0", it.UsableAdminCount, it.UsableOwnerCount, it.ConvertibleAdminCount)
	}

	// revert (revoker fails => still done, revokeFailed counted)
	rev.fail = true
	back, err := svc.SetOwnerTier(ctx, "t-op", "t-cus", false)
	if err != nil || back.Reverted != 2 || back.RevokeFailed != 2 {
		t.Fatalf("revert = %+v, %v; want reverted 2 revokeFailed 2", back, err)
	}
	for _, id := range []string{"u-a1", "u-a2"} {
		u := otLoad(t, gdb, id)
		if !u.IsAdmin || u.CanManageUsers || u.RoleID != nil {
			t.Fatalf("%s after revert = admin:%v owner:%v role:%v", id, u.IsAdmin, u.CanManageUsers, u.RoleID)
		}
	}
	back2, err := svc.SetOwnerTier(ctx, "t-op", "t-cus", false)
	if err != nil || back2.Reverted != 0 {
		t.Fatalf("second revert = %+v, %v; want 0", back2, err)
	}
	// revert on a tenant that never had the role => 0, no role created
	otMkTenant(t, gdb, "t-new", "newone", "Mới")
	if r, err := svc.SetOwnerTier(ctx, "t-op", "t-new", false); err != nil || r.Reverted != 0 {
		t.Fatalf("revert on virgin tenant = %+v, %v", r, err)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_roles WHERE tenant_id = 't-new'`); n != 0 {
		t.Fatalf("revert created %d role(s) on a virgin tenant", n)
	}
}

// T072: Provision => first account is the customer owner with the role; duplicate slug leaves 0 orphans.
func TestProvision_FirstAccountIsOwner_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	svc := NewTenantProvisionService(gdb)
	ctx := context.Background()
	res, err := svc.Provision(ctx, ProvisionTenantInput{Slug: "owner-new", AdminEmail: "o@owner-new.test", GeneratePassword: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Admin.IsAdmin || !res.Admin.CanManageUsers || res.Admin.RoleID == nil {
		t.Fatalf("view = %+v, want non-admin owner with role", res.Admin)
	}
	u := otLoad(t, gdb, res.Admin.ID)
	if u.IsAdmin || !u.CanManageUsers || u.RoleID == nil || *u.RoleID != *res.Admin.RoleID {
		t.Fatalf("row = admin:%v owner:%v role:%v", u.IsAdmin, u.CanManageUsers, u.RoleID)
	}
	var name string
	gdb.Raw(`SELECT name FROM acl_roles WHERE id = ? AND tenant_id = ?`, *u.RoleID, res.Tenant.ID).Scan(&name)
	if name != auth.OwnerRoleName {
		t.Fatalf("role name = %q", name)
	}

	before := otCount(t, gdb, `SELECT COUNT(*) FROM acl_roles`)
	beforeGrants := otCount(t, gdb, `SELECT COUNT(*) FROM acl_role_actions`)
	if _, err := svc.Provision(ctx, ProvisionTenantInput{Slug: "owner-new", AdminEmail: "dup@owner-new.test", GeneratePassword: true}); !otIsCode(err, "TENANT_ALREADY_EXISTS") {
		t.Fatalf("dup err = %v", err)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_roles`); n != before {
		t.Fatalf("dup slug left %d orphan role(s)", n-before)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM acl_role_actions`); n != beforeGrants {
		t.Fatalf("dup slug left %d orphan grant(s)", n-beforeGrants)
	}

	// With a project: the registry row lands with the tenant, role and owner.
	crm := "crm"
	pres, err := svc.Provision(ctx, ProvisionTenantInput{Slug: "owner-proj", AdminEmail: "o@owner-proj.test", GeneratePassword: true,
		Project: &ProjectSeed{Industry: &crm, Origin: "blank", CreatedBy: res.Admin.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if n := otCount(t, gdb, `SELECT COUNT(*) FROM sys_project_profiles WHERE tenant_id = '`+pres.Tenant.ID+`'`); n != 1 {
		t.Fatalf("project profile rows = %d, want 1", n)
	}

	// Failure AFTER SeedOwnerRole (oversized industry => profile INSERT fails, last step of the tx):
	// the tenant, owner, role, grants, calendar and profile must ALL roll back.
	tables := []string{"tenants", "users", "acl_roles", "acl_role_actions", "sys_business_calendars", "sys_project_profiles"}
	snap := map[string]int64{}
	for _, tb := range tables {
		snap[tb] = otCount(t, gdb, `SELECT COUNT(*) FROM `+tb)
	}
	huge := strings.Repeat("x", 200)
	if _, err := svc.Provision(ctx, ProvisionTenantInput{Slug: "owner-fail", AdminEmail: "o@owner-fail.test", GeneratePassword: true,
		Project: &ProjectSeed{Industry: &huge, Origin: "blank", CreatedBy: res.Admin.ID}}); err == nil {
		t.Fatal("expected the profile insert to fail")
	}
	for _, tb := range tables {
		if n := otCount(t, gdb, `SELECT COUNT(*) FROM `+tb); n != snap[tb] {
			t.Fatalf("failed provision left %d orphan row(s) in %s", n-snap[tb], tb)
		}
	}
}

// T074: the owner role never counts as "a role that lets staff see the app".
func TestRolesGrantingView_IgnoresOwnerRole_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	otMkTenant(t, gdb, "t-rv", "rv", "RV")
	if err := gdb.Transaction(func(tx *gorm.DB) error {
		_, _, err := SeedOwnerRole(context.Background(), tx, "t-rv")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	repo := auth.NewRoleRepository(gdb)
	ids, err := repo.RolesGrantingView(context.Background(), "t-rv", []string{"crm_leads"})
	if err != nil || len(ids) != 0 {
		t.Fatalf("owner-only tenant = %v, %v; want none", ids, err)
	}
	gdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label, created_at, updated_at) VALUES ('r-app','t-rv','sales','S',NOW(),NOW())`)
	gdb.Exec(`INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level, created_at, updated_at) VALUES ('g1','r-app','crm_leads','view','all',NOW(),NOW())`)
	gdb.Exec(`INSERT INTO acl_roles (id, tenant_id, name, label, created_at, updated_at) VALUES ('r-star','t-rv','everything','E',NOW(),NOW())`)
	gdb.Exec(`INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level, created_at, updated_at) VALUES ('g2','r-star','*','view','all',NOW(),NOW())`)
	ids, err = repo.RolesGrantingView(context.Background(), "t-rv", []string{"crm_leads"})
	if err != nil || len(ids) != 2 {
		t.Fatalf("with app + wildcard roles = %v, %v; want exactly those 2", ids, err)
	}
}
