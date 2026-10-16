//go:build integration

// specs/065 lat B against a real MySQL 8: the can_manage_users column + CHECK chk_users_tier and the owner's
// conditional write (UpdateUserIfManageable). In-memory fakes cannot prove the CHECK nor the SQL WHERE.
//
//	bash scripts/run-integration.sh -run 'TestUserManageable_' ./internal/auth/
package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type manageFixture struct {
	repo   *UserRepository
	db     *gorm.DB
	tenant string
	other  string // a second tenant
	owner  string
}

func manageSetup(t *testing.T) *manageFixture {
	t.Helper()
	db, err := gorm.Open(gormmysql.Open(testDSN), &gorm.Config{})
	if err != nil {
		t.Skipf("MySQL not reachable (%v) - skipping", err)
	}
	f := &manageFixture{repo: NewUserRepository(db), db: db, tenant: uuid.NewString(), other: uuid.NewString(), owner: uuid.NewString()}
	for _, tn := range []string{f.tenant, f.other} {
		if err := db.Exec(`INSERT INTO tenants (id, slug, name) VALUES (?, ?, ?)`, tn, "mg-"+tn[:8], "T").Error; err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	f.seed(t, f.tenant, f.owner, "owner", "active", false, true, false)
	return f
}

func (f *manageFixture) seed(t *testing.T, tenant, id, label, status string, admin, owner, operator bool) {
	t.Helper()
	if err := f.db.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, status, is_admin, can_manage_users, is_platform_operator)
		VALUES (?, ?, ?, 'x', ?, ?, ?, ?)`, id, tenant, label+"-"+id[:8]+"@mg.test", status, admin, owner, operator).Error; err != nil {
		t.Fatalf("seed user %s: %v", label, err)
	}
}

func TestUserManageable_CheckRejectsAdminAndOwner(t *testing.T) {
	f := manageSetup(t)
	id := uuid.NewString()
	f.seed(t, f.tenant, id, "staff", "active", false, false, false)

	// 3819 is mapped to the typed sentinel by the repo (the service's 422 safety net).
	_, err := f.repo.UpdateUser(context.Background(), f.tenant, id, map[string]any{"is_admin": true, "can_manage_users": true})
	if !errors.Is(err, ErrUserTierConflict) {
		t.Fatalf("admin+owner update err = %v, want ErrUserTierConflict", err)
	}
	// And a direct INSERT of both is refused by the database itself.
	if err := f.db.Exec(`INSERT INTO users (id, tenant_id, email, password_hash, is_admin, can_manage_users) VALUES (?, ?, 'both@mg.test', 'x', 1, 1)`,
		uuid.NewString(), f.tenant).Error; err == nil {
		t.Fatal("CHECK chk_users_tier let admin AND owner in")
	}
	// A CreateUser carrying both maps the same way.
	err = f.repo.CreateUser(context.Background(), &User{ID: uuid.NewString(), TenantID: f.tenant, Email: "both2@mg.test", PasswordHash: "x", Status: "active", IsAdmin: true, CanManageUsers: true})
	if !errors.Is(err, ErrUserTierConflict) {
		t.Fatalf("CreateUser err = %v, want ErrUserTierConflict", err)
	}
}

func TestUserManageable_UpdateIfManageable(t *testing.T) {
	ctx := context.Background()
	f := manageSetup(t)
	staff, admin, owner2, op, dis := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	f.seed(t, f.tenant, staff, "staff", "active", false, false, false)
	f.seed(t, f.tenant, admin, "admin", "active", true, false, false)
	f.seed(t, f.tenant, owner2, "owner2", "active", false, true, false)
	f.seed(t, f.tenant, op, "op", "active", true, false, true)
	f.seed(t, f.tenant, dis, "dis", "disabled", false, false, false)
	foreign := uuid.NewString()
	f.seed(t, f.other, foreign, "foreign", "active", false, false, false)

	name := func(id string) string {
		var n string
		if err := f.db.Raw(`SELECT COALESCE(full_name, '') FROM users WHERE id = ?`, id).Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}

	// staff: written.
	u, err := f.repo.UpdateUserIfManageable(ctx, f.tenant, staff, f.owner, map[string]any{"full_name": "Moi"})
	if err != nil || u.FullName != "Moi" {
		t.Fatalf("staff: %v %+v", err, u)
	}
	// saving the SAME values again is a success (200), not a 404, regardless of clientFoundRows.
	if u, err = f.repo.UpdateUserIfManageable(ctx, f.tenant, staff, f.owner, map[string]any{"full_name": "Moi"}); err != nil || u.FullName != "Moi" {
		t.Fatalf("unchanged save: %v %+v", err, u)
	}
	// not manageable: 403 sentinel, row untouched.
	for label, id := range map[string]string{"admin": admin, "owner": owner2, "operator": op, "disabled": dis, "self": f.owner} {
		if _, err := f.repo.UpdateUserIfManageable(ctx, f.tenant, id, f.owner, map[string]any{"full_name": "HACK"}); !errors.Is(err, ErrUserNotManageable) {
			t.Errorf("%s: err = %v, want ErrUserNotManageable", label, err)
		}
		if name(id) == "HACK" {
			t.Errorf("%s: the conditional UPDATE wrote a non-manageable row", label)
		}
	}
	// other tenant's id under this tenant: 404, and the foreign row is untouched.
	if _, err := f.repo.UpdateUserIfManageable(ctx, f.tenant, foreign, f.owner, map[string]any{"full_name": "HACK"}); !errors.Is(err, ErrUserNotFound) {
		t.Errorf("foreign: err = %v, want ErrUserNotFound", err)
	}
	if name(foreign) == "HACK" {
		t.Error("cross-tenant write")
	}
	// TOCTOU: the target is promoted to admin between the service's read and the write -> 403, unchanged.
	if err := f.db.Exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, staff).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.UpdateUserIfManageable(ctx, f.tenant, staff, f.owner, map[string]any{"full_name": "LATE"}); !errors.Is(err, ErrUserNotManageable) {
		t.Errorf("promoted target: err = %v, want ErrUserNotManageable", err)
	}
	if name(staff) == "LATE" {
		t.Error("write landed on a target promoted to admin")
	}
}
