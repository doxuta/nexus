//go:build integration

// TENANT-PROVISION integration test: the create-workspace transaction, exercised END-TO-END over a
// freshly-migrated throwaway MySQL DB (o3aSetup runs ALL migrations incl. 000052
// is_platform_operator via the REAL golang-migrate applier). Proves what the nil-db unit tests
// cannot: the tenant + first admin land atomically, the admin is a byte-for-byte loggable-in admin
// (its stored bcrypt hash verifies against the one-time password), a duplicate slug is a 409 that
// leaves ZERO orphan rows, and the registry list reports the usable-admin count.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run TenantProvision
package services

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/calendar"
	"nexus/platform/internal/calmeta"
)

// provAuditCapture records emitted RM-19 events so the test can assert the provision audit fired.
type provAuditCapture struct {
	mu     sync.Mutex
	events []audit.Event
}

func (c *provAuditCapture) Record(_ context.Context, e audit.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func TestTenantProvision_Integration(t *testing.T) {
	gdb, sdb := o3aSetup(t)
	cap := &provAuditCapture{}
	svc := NewTenantProvisionService(gdb).WithAuditor(cap)
	// Actor id (operator) rides in ctx via WithActor, exactly as the handler's auditContext does.
	ctx := audit.WithActor(context.Background(), "operator-u1")

	// 1. Provision a NEW workspace + first admin with a server-generated password.
	res, err := svc.Provision(ctx, ProvisionTenantInput{
		Slug: "acme-int", Name: "ACME Integration", AdminEmail: "admin@acme-int.test",
		GeneratePassword: true, ActorTenantID: "operator-tenant",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.OneTimePassword == "" {
		t.Fatal("generated password absent from result — show-once contract broken")
	}
	if res.Tenant.Slug != "acme-int" || res.Tenant.Status != "active" {
		t.Errorf("tenant view = %+v, want slug=acme-int status=active", res.Tenant)
	}

	// 2. The admin is a real, ACTIVE, is_admin=1, is_platform_operator=0 row whose stored bcrypt
	//    hash verifies against the one-time password — i.e. it can actually log in.
	var u auth.User
	if e := gdb.Where("tenant_id = ? AND email = ?", res.Tenant.ID, "admin@acme-int.test").First(&u).Error; e != nil {
		t.Fatalf("load provisioned admin: %v", e)
	}
	// specs/065 D2: the first account is the CUSTOMER OWNER, not a builder admin.
	if u.IsAdmin || !u.CanManageUsers || u.IsPlatformOperator || u.Status != "active" {
		t.Errorf("owner flags = {admin:%v owner:%v operator:%v status:%q}, want {false true false active}", u.IsAdmin, u.CanManageUsers, u.IsPlatformOperator, u.Status)
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(res.OneTimePassword)) != nil {
		t.Error("stored hash does not verify against the one-time password — provisioned admin cannot log in")
	}

	// 2b. The new workspace comes with a DEFAULT WORKING CALENDAR (DAILY-QUEUE #10 / Q9). Without
	//     it every business_hours SLA in this tenant would silently take the wall-clock fallback,
	//     and a ticket raised at 16:00 Friday would be "due" at 19:00 Friday. The row must also be
	//     READABLE by the engine: the seed writes its JSON with a raw INSERT that bypasses
	//     validateCalendarInput, and the resolver treats an unparseable calendar as NO calendar —
	//     so a malformed seed would degrade exactly like no seed at all, silently.
	calRepo := calmeta.NewSQLRepository(sdb)
	cal, err := calRepo.GetDefault(context.Background(), res.Tenant.ID)
	if err != nil {
		t.Fatalf("provisioned tenant has no default business calendar: %v", err)
	}
	if cal.Name != DefaultCalendarName || cal.Timezone != DefaultCalendarTimezone || !cal.IsDefault {
		t.Errorf("seeded calendar = {name:%q tz:%q default:%v}, want {%q %q true}",
			cal.Name, cal.Timezone, cal.IsDefault, DefaultCalendarName, DefaultCalendarTimezone)
	}
	sched, err := calendar.FromJSON(cal.Timezone, cal.Hours, cal.Holidays)
	if err != nil {
		t.Fatalf("the PERSISTED calendar does not parse — the engine would read it as no calendar: %v", err)
	}
	loc := sched.Location()
	due, err := sched.AddBusinessDuration(time.Date(2026, 7, 10, 15, 0, 0, 0, loc), 4*time.Hour)
	if err != nil {
		t.Fatalf("AddBusinessDuration over the persisted calendar: %v", err)
	}
	if want := time.Date(2026, 7, 13, 10, 0, 0, 0, loc); !due.Equal(want) {
		t.Errorf("15:00 Friday + 4 business hours = %s, want %s", due.In(loc), want)
	}

	// 3. Re-provision the SAME slug → 409 TENANT_ALREADY_EXISTS, and the dup rolls back the WHOLE
	//    tx so NO orphan admin row is left behind (atomicity). The calendar is written in the same
	//    tx, so it must roll back too — checked below.
	_, err = svc.Provision(ctx, ProvisionTenantInput{
		Slug: "acme-int", AdminEmail: "other@acme-int.test", GeneratePassword: true,
	})
	code, _ := provisionErrCode(t, err)
	if code != "TENANT_ALREADY_EXISTS" {
		t.Errorf("re-provision code = %q, want TENANT_ALREADY_EXISTS", code)
	}
	var orphans int64
	gdb.Model(&auth.User{}).Where("email = ?", "other@acme-int.test").Count(&orphans)
	if orphans != 0 {
		t.Errorf("dup-slug provision left %d orphan admin row(s), want 0 (atomic rollback)", orphans)
	}
	var calOrphans int64
	gdb.Raw(`SELECT COUNT(*) FROM sys_business_calendars c
		JOIN tenants t ON t.id = c.tenant_id
		WHERE t.slug = ? AND c.deleted_at IS NULL`, "acme-int").Scan(&calOrphans)
	if calOrphans != 1 {
		t.Errorf("tenant acme-int has %d live calendars after the failed re-provision, want exactly 1 (the seed is inside the tx)", calOrphans)
	}

	// 4. The registry lists the new tenant with usableAdminCount == 1 and leaks no user data.
	items, err := svc.ListTenants(ctx)
	if err != nil {
		t.Fatalf("list tenants: %v", err)
	}
	found := false
	for _, it := range items {
		if it.Slug == "acme-int" {
			found = true
			if it.UsableAdminCount != 0 || it.UsableOwnerCount != 1 {
				t.Errorf("usableAdminCount/usableOwnerCount = %d/%d, want 0/1", it.UsableAdminCount, it.UsableOwnerCount)
			}
		}
	}
	if !found {
		t.Error("provisioned tenant not present in ListTenants")
	}

	// 5. HIGH-1: the successful provision emitted an RM-19 audit row into the OPERATOR's tenant,
	//    attributed to the operator, carrying the non-secret shape and NEVER the password. This is
	//    the detection control the no-rate-limit posture (spec §11 #4) leans on.
	var provEvt *audit.Event
	for i := range cap.events {
		if cap.events[i].EntityName == "__tenant__" && cap.events[i].Action == "provision" {
			provEvt = &cap.events[i]
		}
	}
	if provEvt == nil {
		t.Fatal("no __tenant__/provision audit event emitted — the no-rate-limit detection control is missing")
	}
	if provEvt.TenantID != "operator-tenant" || provEvt.ActorUserID != "operator-u1" {
		t.Errorf("audit actor = {tenant:%q user:%q}, want {operator-tenant operator-u1}", provEvt.TenantID, provEvt.ActorUserID)
	}
	if raw, _ := json.Marshal(provEvt); strings.Contains(string(raw), res.OneTimePassword) {
		t.Error("audit event contains the generated password — secret leaked into the audit trail")
	}
}

// specs/062-customer-workspace L6 (T086): ProvisionTenantInput.Project writes the registry profile
// row in the SAME transaction as the tenant, its first user and its calendar. A duplicate slug
// rolls ALL of it back (no orphan profile), and a call without Project keeps its old behaviour
// (no profile row at all - the registry reads that as "building / unknown").
func TestTenantProvision_ProjectSeed_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	svc := NewTenantProvisionService(gdb)
	ctx := audit.WithActor(context.Background(), "builder-1")
	industry := "Phòng khám"
	packSlug := "clinic"
	seed := &ProjectSeed{Industry: &industry, Origin: "pack", OriginPack: &packSlug, CreatedBy: "builder-1"}

	res, err := svc.Provision(ctx, ProvisionTenantInput{
		Slug: "proj-int", Name: "Khách dự án", AdminEmail: "owner@proj-int.test", GeneratePassword: true, Project: seed,
	})
	if err != nil {
		t.Fatalf("provision with project: %v", err)
	}
	var row struct {
		Industry   *string
		Origin     *string
		OriginPack *string
		CreatedBy  string
	}
	if e := gdb.Raw(`SELECT industry, origin, origin_pack, created_by FROM sys_project_profiles WHERE tenant_id = ? AND deleted_at IS NULL`,
		res.Tenant.ID).Scan(&row).Error; e != nil {
		t.Fatalf("read profile: %v", e)
	}
	if row.Industry == nil || *row.Industry != industry || row.Origin == nil || *row.Origin != "pack" ||
		row.OriginPack == nil || *row.OriginPack != "clinic" || row.CreatedBy != "builder-1" {
		t.Errorf("profile = %+v, want industry/origin/origin_pack/created_by from the seed", row)
	}

	var before int64
	gdb.Raw(`SELECT COUNT(*) FROM sys_project_profiles`).Scan(&before)
	_, err = svc.Provision(ctx, ProvisionTenantInput{
		Slug: "proj-int", AdminEmail: "other@proj-int.test", GeneratePassword: true, Project: seed,
	})
	if code, _ := provisionErrCode(t, err); code != "TENANT_ALREADY_EXISTS" {
		t.Errorf("duplicate slug code = %q, want TENANT_ALREADY_EXISTS", code)
	}
	var after int64
	gdb.Raw(`SELECT COUNT(*) FROM sys_project_profiles`).Scan(&after)
	if after != before {
		t.Errorf("profiles %d -> %d after a refused duplicate, want unchanged (profile is inside the tx)", before, after)
	}

	plain, err := svc.Provision(ctx, ProvisionTenantInput{
		Slug: "plain-int", AdminEmail: "owner@plain-int.test", GeneratePassword: true,
	})
	if err != nil {
		t.Fatalf("provision without project: %v", err)
	}
	var n int64
	gdb.Raw(`SELECT COUNT(*) FROM sys_project_profiles WHERE tenant_id = ?`, plain.Tenant.ID).Scan(&n)
	if n != 0 {
		t.Errorf("a provision without Project wrote %d profile rows, want 0", n)
	}
}

// A profile insert that fails AFTER tenant, first user and calendar were written must roll all of
// them back. The duplicate-slug case above never reaches the profile step (it dies at the tenant
// insert), so it cannot tell "profile inside the tx" from "profile after the commit" - this one can:
// an industry over VARCHAR(64) makes only the profile insert fail (strict mode rejects it).
func TestTenantProvision_ProfileFailureRollsBackEverything_Integration(t *testing.T) {
	gdb, _ := o3aSetup(t)
	svc := NewTenantProvisionService(gdb)
	ctx := audit.WithActor(context.Background(), "builder-1")
	tooLong := strings.Repeat("x", 65)

	_, err := svc.Provision(ctx, ProvisionTenantInput{
		Slug: "proj-rollback", AdminEmail: "owner@proj-rollback.test", GeneratePassword: true,
		Project: &ProjectSeed{Industry: &tooLong, Origin: "blank", CreatedBy: "builder-1"},
	})
	if err == nil {
		t.Fatal("provision with an over-long industry succeeded, want the profile insert to fail")
	}
	for _, q := range []struct{ table, sql string }{
		{"tenants", `SELECT COUNT(*) FROM tenants WHERE slug = 'proj-rollback'`},
		{"users", `SELECT COUNT(*) FROM users WHERE email = 'owner@proj-rollback.test'`},
		{"sys_business_calendars", `SELECT COUNT(*) FROM sys_business_calendars WHERE tenant_id NOT IN (SELECT id FROM tenants)`},
		{"sys_project_profiles", `SELECT COUNT(*) FROM sys_project_profiles WHERE tenant_id NOT IN (SELECT id FROM tenants)`},
	} {
		var n int64
		if e := gdb.Raw(q.sql).Scan(&n).Error; e != nil {
			t.Fatalf("count %s: %v", q.table, e)
		}
		if n != 0 {
			t.Errorf("%s: %d orphan rows after a failed profile insert, want 0 (whole provision is one tx)", q.table, n)
		}
	}
}
