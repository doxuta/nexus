//go:build integration

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/ittest"
	"nexus/platform/internal/services"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// packageTestDSN points every bootstrap-admin integration test at a self-provisioned, migrated
// throwaway database set by TestMain (mirrors internal/calmeta/main_test.go). A single source of
// DSN truth: TEST_DATABASE_URL (or the local default) feeds ittest.Setup, which returns a DSN
// against a freshly-created, migrated DB that TestMain then drops.
var packageTestDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=true&multiStatements=true"

func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = packageTestDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_bootstrap_it", "../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap-admin integration setup failed: %v\n", err)
		os.Exit(1)
	}
	// Skip (MySQL unreachable at the DSN asked for) must not leave the package default (shared dev
	// nexus_platform on :3306) in place: a wrong TEST_DATABASE_URL would then run and WRITE there.
	packageTestDSN = input
	if !skip {
		packageTestDSN = thr
	}
	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

// openTestDB opens a GORM connection to the throwaway DB, Skipping when MySQL is unreachable so
// the suite degrades to a skip (never a false failure) exactly like the other integration suites.
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(mysql.Open(packageTestDSN), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable — skipping (%v)", err)
	}
	return gdb
}

func newTestDeps(gdb *gorm.DB) deps {
	repo := auth.NewUserRepository(gdb)
	return deps{
		db:       gdb,
		userRepo: repo,
		userSvc:  services.NewUserService(repo),
		newID:    uuid.NewString,
	}
}

// TestBootstrap_CreatesWorkingAdmin is the load-bearing check: run the tool against a real
// migrated DB and prove the admin it creates matches a working admin AND actually logs in.
func TestBootstrap_CreatesWorkingAdmin(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	const (
		slug  = "acme"
		email = "admin@acme.com"
		pw    = "a-strong-password"
	)
	in := input{tenant: slug, email: email, password: pw, name: "Acme Admin"}

	res, err := bootstrap(ctx, d, in)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if !res.adminCreated || !res.tenantCreated {
		t.Fatalf("first run: adminCreated=%v tenantCreated=%v, want both true", res.adminCreated, res.tenantCreated)
	}

	// Row shape matches a working admin: is_admin=1, status active, tenant linked, bcrypt verifies.
	u, err := d.userRepo.GetUserByEmail(ctx, res.tenantID, email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if !u.IsAdmin {
		t.Errorf("is_admin = false, want true")
	}
	if u.Status != "active" {
		t.Errorf("status = %q, want active", u.Status)
	}
	if u.TenantID != res.tenantID {
		t.Errorf("tenant_id = %q, want %q", u.TenantID, res.tenantID)
	}
	if u.RoleID != nil {
		t.Errorf("role_id = %v, want nil (gated by is_admin like the demo seed)", *u.RoleID)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pw)); err != nil {
		t.Errorf("stored password_hash does not verify against the input password: %v", err)
	}

	// Definitive proof of the Phase-0 goal: the created admin can LOG IN via the same AuthService
	// the server uses, with {slug, email, password}.
	tokenSvc, err := auth.NewTokenService("test-secret-not-a-placeholder-value-0123456789", time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	authSvc := services.NewAuthService(d.userRepo, auth.NewRefreshTokenRepository(gdb), tokenSvc)
	out, err := authSvc.Login(ctx, services.LoginInput{Tenant: slug, Email: email, Password: pw})
	if err != nil {
		t.Fatalf("Login with created admin failed: %v", err)
	}
	if out.AccessToken == "" || out.User == nil || !out.User.IsAdmin {
		t.Fatalf("Login output not a usable admin session: %+v", out)
	}
}

// TestBootstrap_Idempotent proves a re-run does not clobber or duplicate: the second run reports
// adminCreated=false and the user row count stays at exactly one.
func TestBootstrap_Idempotent(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	in := input{tenant: "beta", email: "root@beta.io", password: "another-strong-pass"}

	first, err := bootstrap(ctx, d, in)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if !first.adminCreated {
		t.Fatalf("first run: adminCreated=false, want true")
	}

	second, err := bootstrap(ctx, d, in)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.adminCreated {
		t.Errorf("second run: adminCreated=true, want false (idempotent no-op)")
	}
	if second.tenantCreated {
		t.Errorf("second run: tenantCreated=true, want false (tenant reused)")
	}

	// Exactly one user row for this (tenant, email) — no duplicate insert.
	var count int64
	if err := gdb.WithContext(ctx).Model(&auth.User{}).
		Where("tenant_id = ? AND email = ?", first.tenantID, in.email).
		Count(&count).Error; err != nil {
		t.Fatalf("count users: %v", err)
	}
	if count != 1 {
		t.Errorf("user row count = %d, want 1", count)
	}

	// And exactly one tenant row for the slug.
	var tenantCount int64
	if err := gdb.WithContext(ctx).Model(&auth.Tenant{}).
		Where("slug = ?", "beta").
		Count(&tenantCount).Error; err != nil {
		t.Fatalf("count tenants: %v", err)
	}
	if tenantCount != 1 {
		t.Errorf("tenant row count = %d, want 1", tenantCount)
	}
}

// TestBootstrap_RejectsWeakPassword proves the RM-17 gate fires end-to-end and, crucially, that
// a rejected password leaves NO stray tenant row behind (validation runs before any DB write).
func TestBootstrap_RejectsWeakPassword(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	_, err := bootstrap(ctx, d, input{tenant: "gamma", email: "admin@gamma.co", password: "short"})
	if err == nil {
		t.Fatalf("expected rejection for weak password, got nil")
	}

	var tenantCount int64
	if err := gdb.WithContext(ctx).Model(&auth.Tenant{}).
		Where("slug = ?", "gamma").
		Count(&tenantCount).Error; err != nil {
		t.Fatalf("count tenants: %v", err)
	}
	if tenantCount != 0 {
		t.Errorf("weak-password run left %d tenant rows, want 0 (must reject before any write)", tenantCount)
	}
}

// TestBootstrap_ExistingUsableActiveAdmin_IsIdempotentSuccess directly covers the PRESERVED good
// path of Finding A's fix: a pre-existing, active, is_admin=true row for the email is still the
// clean idempotent "already exists" success (adminCreated=false, err=nil) — not the new
// rejection path.
func TestBootstrap_ExistingUsableActiveAdmin_IsIdempotentSuccess(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	const (
		slug  = "eta"
		email = "root@eta.io"
	)
	tenantID, _, err := resolveOrCreateTenant(ctx, d, slug)
	if err != nil {
		t.Fatalf("resolveOrCreateTenant: %v", err)
	}
	if _, err := d.userSvc.CreateUser(ctx, tenantID, true, services.CreateUserInput{
		Email: email, Password: "some-strong-pass", FullName: "Eta Admin", IsAdmin: true,
	}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	res, err := bootstrap(ctx, d, input{tenant: slug, email: email, password: "another-strong-pass"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.adminCreated {
		t.Errorf("adminCreated = true, want false (usable admin already existed)")
	}
}

// TestBootstrap_ExistingNonAdmin_RejectsWithoutSuccess is Finding A scenario (1): a pre-existing
// user with the target email that is NOT an admin must be REJECTED (non-zero), never reported as
// the idempotent "already exists" success — an operator misreading that as "done" would go on to
// disable the demo admin while the tenant still has zero usable admins.
func TestBootstrap_ExistingNonAdmin_RejectsWithoutSuccess(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	const (
		slug  = "delta"
		email = "notadmin@delta.io"
	)
	tenantID, _, err := resolveOrCreateTenant(ctx, d, slug)
	if err != nil {
		t.Fatalf("resolveOrCreateTenant: %v", err)
	}
	if _, err := d.userSvc.CreateUser(ctx, tenantID, true, services.CreateUserInput{
		Email: email, Password: "some-strong-pass", FullName: "Not Admin", IsAdmin: false,
	}); err != nil {
		t.Fatalf("seed non-admin user: %v", err)
	}

	_, err = bootstrap(ctx, d, input{tenant: slug, email: email, password: "another-strong-pass"})
	if err == nil {
		t.Fatalf("expected rejection for existing non-admin, got nil (success)")
	}
	if !strings.Contains(err.Error(), "NOT a usable active admin") {
		t.Errorf("error = %q, want mention of 'NOT a usable active admin'", err.Error())
	}
}

// TestBootstrap_ExistingDisabledAdmin_RejectsWithoutSuccess is Finding A scenario (2): an admin
// row exists but status != active must also be rejected, not reported as success. The status
// flip goes through the raw repo (not the guarded UserService.UpdateUser, which refuses to
// disable a tenant's LAST active admin) — this seeds a historical state, not a live operator
// action.
func TestBootstrap_ExistingDisabledAdmin_RejectsWithoutSuccess(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	const (
		slug  = "epsilon"
		email = "disabled@epsilon.io"
	)
	tenantID, _, err := resolveOrCreateTenant(ctx, d, slug)
	if err != nil {
		t.Fatalf("resolveOrCreateTenant: %v", err)
	}
	view, err := d.userSvc.CreateUser(ctx, tenantID, true, services.CreateUserInput{
		Email: email, Password: "some-strong-pass", FullName: "Disabled Admin", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := d.userRepo.UpdateUser(ctx, tenantID, view.ID, map[string]any{"status": "disabled"}); err != nil {
		t.Fatalf("disable admin: %v", err)
	}

	_, err = bootstrap(ctx, d, input{tenant: slug, email: email, password: "another-strong-pass"})
	if err == nil {
		t.Fatalf("expected rejection for disabled admin, got nil (success)")
	}
	if !strings.Contains(err.Error(), "NOT a usable active admin") {
		t.Errorf("error = %q, want mention of 'NOT a usable active admin'", err.Error())
	}
}

// TestBootstrap_ExistingSoftDeletedAdmin_RejectsWithoutSuccess is Finding A scenario (3): a
// soft-deleted prior admin. The tenant-pinned GetUserByEmail cannot see it, so without the
// Unscoped inspectExisting check this would silently fall through to CreateUser and hit
// USER_ALREADY_EXISTS — which must also be rejected, never reported as success, and named as
// soft-deleted specifically.
func TestBootstrap_ExistingSoftDeletedAdmin_RejectsWithoutSuccess(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	const (
		slug  = "zeta"
		email = "gone@zeta.io"
	)
	tenantID, _, err := resolveOrCreateTenant(ctx, d, slug)
	if err != nil {
		t.Fatalf("resolveOrCreateTenant: %v", err)
	}
	view, err := d.userSvc.CreateUser(ctx, tenantID, true, services.CreateUserInput{
		Email: email, Password: "some-strong-pass", FullName: "Gone Admin", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := d.userRepo.SoftDeleteUser(ctx, tenantID, view.ID); err != nil {
		t.Fatalf("soft-delete admin: %v", err)
	}

	_, err = bootstrap(ctx, d, input{tenant: slug, email: email, password: "another-strong-pass"})
	if err == nil {
		t.Fatalf("expected rejection for soft-deleted admin, got nil (success)")
	}
	if !strings.Contains(err.Error(), "SOFT-DELETED") {
		t.Errorf("error = %q, want mention of 'SOFT-DELETED'", err.Error())
	}
}

// TestBootstrap_InactiveTenantSlug_Rejects is Finding B: a same-slug tenant that already exists
// but is NOT active must not be silently reused — an admin attached to it could never log in
// (login's ResolveTenantBySlug is active-only). The status flip goes through GORM directly since
// there is no tenant-status-change service.
func TestBootstrap_InactiveTenantSlug_Rejects(t *testing.T) {
	gdb := openTestDB(t)
	d := newTestDeps(gdb)
	ctx := context.Background()

	const slug = "theta"
	tenantID, created, err := resolveOrCreateTenant(ctx, d, slug)
	if err != nil {
		t.Fatalf("resolveOrCreateTenant: %v", err)
	}
	if !created {
		t.Fatalf("expected a fresh tenant to be created")
	}
	if err := gdb.WithContext(ctx).Model(&auth.Tenant{}).Where("id = ?", tenantID).Update("status", "suspended").Error; err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	_, err = bootstrap(ctx, d, input{tenant: slug, email: "admin@theta.io", password: "a-strong-password"})
	if err == nil {
		t.Fatalf("expected rejection for inactive tenant slug, got nil (success)")
	}
	if !strings.Contains(err.Error(), "NOT active") {
		t.Errorf("error = %q, want mention of 'NOT active'", err.Error())
	}
}
