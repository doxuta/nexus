package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// roleGuardFakeValidator is the in-memory stand-in for the role repository used by the
// cross-tenant role guard tests. It holds a set of (tenantID, roleID) pairs that are considered
// valid (i.e. the role exists in that tenant); GetRoleByID returns auth.ErrRoleNotFound for any
// pair NOT in the set, exactly as the real tenant-pinned auth.RoleRepository.GetRoleByID does for
// an unknown or cross-tenant id. The name is suffixed to avoid colliding with other fakes in the
// concurrently-edited services test package.
type roleGuardFakeValidator struct {
	roles map[string]struct{} // keyed by tenantID + "\x00" + roleID
	err   error               // when set, returned from GetRoleByID to exercise the 500 path
}

func newRoleGuardFakeValidator() *roleGuardFakeValidator {
	return &roleGuardFakeValidator{roles: map[string]struct{}{}}
}

func (f *roleGuardFakeValidator) add(tenantID, roleID string) {
	f.roles[tenantID+"\x00"+roleID] = struct{}{}
}

func (f *roleGuardFakeValidator) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	if f.err != nil {
		return nil, f.err
	}
	if _, ok := f.roles[tenantID+"\x00"+id]; ok {
		return &auth.Role{ID: id, TenantID: tenantID}, nil
	}
	return nil, auth.ErrRoleNotFound
}

// newRoleGuardUserSvc builds a user service wired to a roleGuardFakeValidator (the cross-tenant
// role guard active), returning both so a test can seed valid roles AND drive CreateUser /
// UpdateUser. It uses its own deterministic id generator so it does not depend on helpers in the
// concurrently-edited user_service_test.go.
func newRoleGuardUserSvc(rv *roleGuardFakeValidator) (*UserService, *fakeUserCRUDRepo) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo).WithRoleValidator(rv)
	n := 0
	svc.newID = func() string {
		n++
		return "rg-id-" + string(rune('0'+n))
	}
	return svc, repo
}

func ptr(s string) *string { return &s }

// --- CreateUser cross-tenant role guard --------------------------------------

// A role_id that belongs to ANOTHER tenant must be rejected with 422 ROLE_INVALID and no user
// row may be written (a foreign role can never be attached at create time).
func TestCreateUser_CrossTenantRoleRejected(t *testing.T) {
	rv := newRoleGuardFakeValidator()
	rv.add("t2", "role-foreign") // role exists only in tenant t2
	svc, repo := newRoleGuardUserSvc(rv)

	_, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "new@acme.test", Password: "secret123", RoleID: ptr("role-foreign"),
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_INVALID" {
		t.Fatalf("err = %v, want ROLE_INVALID validation error", err)
	}
	if len(repo.rows) != 0 {
		t.Fatalf("cross-tenant role create wrote %d rows; want 0", len(repo.rows))
	}
}

// A role_id that belongs to the SAME tenant must succeed and be persisted on the user.
func TestCreateUser_SameTenantRoleAccepted(t *testing.T) {
	rv := newRoleGuardFakeValidator()
	rv.add("t1", "role-ok") // role exists in the caller's tenant
	svc, repo := newRoleGuardUserSvc(rv)

	view, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "new@acme.test", Password: "secret123", RoleID: ptr("role-ok"),
	})
	if err != nil {
		t.Fatalf("same-tenant role create: %v", err)
	}
	if view.RoleID == nil || *view.RoleID != "role-ok" {
		t.Fatalf("view.RoleID = %v, want role-ok", view.RoleID)
	}
	stored := repo.rows[repo.key("t1", view.ID)]
	if stored.RoleID == nil || *stored.RoleID != "role-ok" {
		t.Fatalf("stored RoleID = %v, want role-ok", stored.RoleID)
	}
}

// A create with NO role_id (nil) must skip the guard entirely and succeed even when no roles
// exist for the tenant.
func TestCreateUser_NoRoleSkipsGuard(t *testing.T) {
	rv := newRoleGuardFakeValidator() // no roles registered at all
	svc, _ := newRoleGuardUserSvc(rv)

	view, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "norole@acme.test", Password: "secret123",
	})
	if err != nil {
		t.Fatalf("no-role create: %v", err)
	}
	if view.RoleID != nil {
		t.Fatalf("view.RoleID = %v, want nil", view.RoleID)
	}
}

// A create with a blank (whitespace) role_id collapses to "no role" and skips the guard.
func TestCreateUser_BlankRoleSkipsGuard(t *testing.T) {
	rv := newRoleGuardFakeValidator()
	svc, repo := newRoleGuardUserSvc(rv)

	view, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "blank@acme.test", Password: "secret123", RoleID: ptr("   "),
	})
	if err != nil {
		t.Fatalf("blank-role create: %v", err)
	}
	stored := repo.rows[repo.key("t1", view.ID)]
	if stored.RoleID != nil {
		t.Fatalf("blank role_id should persist as nil; got %v", stored.RoleID)
	}
}

// --- UpdateUser cross-tenant role guard --------------------------------------

// Re-assigning a cross-tenant role_id on update must be rejected with 422 ROLE_INVALID and the
// stored role must be left untouched.
func TestUpdateUser_CrossTenantRoleRejected(t *testing.T) {
	rv := newRoleGuardFakeValidator()
	rv.add("t2", "role-foreign")
	svc, repo := newRoleGuardUserSvc(rv)
	// Seed a plain user in t1 with no role.
	repo.rows[repo.key("t1", "u1")] = &auth.User{ID: "u1", TenantID: "t1", Email: "u1@acme.test", Status: "active"}

	_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: ptr("role-foreign")})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "ROLE_INVALID" {
		t.Fatalf("err = %v, want ROLE_INVALID validation error", err)
	}
	if repo.rows[repo.key("t1", "u1")].RoleID != nil {
		t.Fatalf("cross-tenant update mutated role_id; want unchanged")
	}
}

// Re-assigning a same-tenant role_id on update must succeed and persist.
func TestUpdateUser_SameTenantRoleAccepted(t *testing.T) {
	rv := newRoleGuardFakeValidator()
	rv.add("t1", "role-ok")
	svc, repo := newRoleGuardUserSvc(rv)
	repo.rows[repo.key("t1", "u1")] = &auth.User{ID: "u1", TenantID: "t1", Email: "u1@acme.test", Status: "active"}

	view, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: ptr("role-ok")})
	if err != nil {
		t.Fatalf("same-tenant role update: %v", err)
	}
	if view.RoleID == nil || *view.RoleID != "role-ok" {
		t.Fatalf("view.RoleID = %v, want role-ok", view.RoleID)
	}
	if got := repo.rows[repo.key("t1", "u1")].RoleID; got == nil || *got != "role-ok" {
		t.Fatalf("stored RoleID = %v, want role-ok", got)
	}
}

// Clearing role_id to NULL (empty string) on update must NOT trigger the guard and must clear it.
func TestUpdateUser_ClearRoleSkipsGuard(t *testing.T) {
	rv := newRoleGuardFakeValidator() // no roles registered → would reject any lookup
	svc, repo := newRoleGuardUserSvc(rv)
	existing := "role-ok"
	repo.rows[repo.key("t1", "u1")] = &auth.User{ID: "u1", TenantID: "t1", Email: "u1@acme.test", Status: "active", RoleID: &existing}

	view, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{RoleID: ptr("")})
	if err != nil {
		t.Fatalf("clear-role update: %v", err)
	}
	if view.RoleID != nil {
		t.Fatalf("view.RoleID = %v, want nil after clear", view.RoleID)
	}
	if repo.rows[repo.key("t1", "u1")].RoleID != nil {
		t.Fatalf("stored RoleID not cleared; got %v", repo.rows[repo.key("t1", "u1")].RoleID)
	}
}

// An update that does NOT touch role_id (nil) must skip the guard entirely.
func TestUpdateUser_OmittedRoleSkipsGuard(t *testing.T) {
	rv := newRoleGuardFakeValidator() // empty: any lookup would fail
	svc, repo := newRoleGuardUserSvc(rv)
	repo.rows[repo.key("t1", "u1")] = &auth.User{ID: "u1", TenantID: "t1", Email: "u1@acme.test", Status: "active"}

	newName := "Renamed"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{FullName: &newName}); err != nil {
		t.Fatalf("update without role_id should not invoke role guard: %v", err)
	}
}

// A non-ErrRoleNotFound repo error (e.g. a DB failure) must surface as a wrapped 500-class error,
// NOT a ROLE_INVALID validation error — only a genuine missing/cross-tenant role is a 422.
func TestCreateUser_RoleLookupErrorIsNotValidation(t *testing.T) {
	rv := newRoleGuardFakeValidator()
	rv.err = errors.New("db down")
	svc, _ := newRoleGuardUserSvc(rv)

	_, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "x@acme.test", Password: "secret123", RoleID: ptr("role-ok"),
	})
	var ve *ValidationError
	if errors.As(err, &ve) {
		t.Fatalf("DB error surfaced as ValidationError %q; want a wrapped internal error", ve.Code)
	}
	if err == nil {
		t.Fatalf("expected an error from a failing role lookup")
	}
}
