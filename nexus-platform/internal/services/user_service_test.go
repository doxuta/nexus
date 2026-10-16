package services

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// fakeUserCRUDRepo is an in-memory stand-in for the user CRUD repository. It is keyed by
// (tenantID, id) so the tenant-scoping behaviour can be exercised without a database; the
// uniqueness check mirrors the (tenant_id, email) constraint.
type fakeUserCRUDRepo struct {
	rows map[string]*auth.User // keyed by tenantID + "\x00" + id
	gets []string              // ids read through GetUserByID, in call order (spec 065 B2 read-count pins)
}

func newFakeUserCRUDRepo() *fakeUserCRUDRepo {
	return &fakeUserCRUDRepo{rows: map[string]*auth.User{}}
}

func (f *fakeUserCRUDRepo) key(tenantID, id string) string { return tenantID + "\x00" + id }

func (f *fakeUserCRUDRepo) ListUsers(_ context.Context, tenantID string) ([]auth.User, error) {
	var out []auth.User
	for _, u := range f.rows {
		if u.TenantID == tenantID {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (f *fakeUserCRUDRepo) GetUserByID(_ context.Context, tenantID, id string) (*auth.User, error) {
	f.gets = append(f.gets, id)
	if u, ok := f.rows[f.key(tenantID, id)]; ok {
		return u, nil
	}
	return nil, auth.ErrUserNotFound
}

func (f *fakeUserCRUDRepo) CreateUser(_ context.Context, u *auth.User) error {
	// Enforce (tenant_id, email) uniqueness like the real unique index.
	for _, ex := range f.rows {
		if ex.TenantID == u.TenantID && ex.Email == u.Email {
			return auth.ErrUserAlreadyExists
		}
	}
	cp := *u
	f.rows[f.key(u.TenantID, u.ID)] = &cp
	return nil
}

func (f *fakeUserCRUDRepo) UpdateUser(_ context.Context, tenantID, id string, fields map[string]any) (*auth.User, error) {
	u, ok := f.rows[f.key(tenantID, id)]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	if v, ok := fields["full_name"].(string); ok {
		u.FullName = v
	}
	if v, ok := fields["status"].(string); ok {
		u.Status = v
	}
	if v, ok := fields["is_admin"].(bool); ok {
		u.IsAdmin = v
	}
	if v, ok := fields["can_manage_users"].(bool); ok {
		u.CanManageUsers = v
	}
	if u.IsAdmin && u.CanManageUsers { // mirrors CHECK chk_users_tier (3819)
		return nil, auth.ErrUserTierConflict
	}
	if v, ok := fields["password_hash"].(string); ok {
		u.PasswordHash = v
	}
	if rv, present := fields["role_id"]; present {
		if rv == nil {
			u.RoleID = nil
		} else if s, ok := rv.(string); ok {
			u.RoleID = &s
		}
	}
	cp := *u
	return &cp, nil
}

// UpdateUserIfManageable mirrors the real conditional write: only a manageable target is touched.
func (f *fakeUserCRUDRepo) UpdateUserIfManageable(ctx context.Context, tenantID, id, actorID string, fields map[string]any) (*auth.User, error) {
	u, ok := f.rows[f.key(tenantID, id)]
	if !ok {
		return nil, auth.ErrUserNotFound
	}
	if !u.ManageableBy(actorID) {
		return nil, auth.ErrUserNotManageable
	}
	return f.UpdateUser(ctx, tenantID, id, fields)
}

func (f *fakeUserCRUDRepo) SoftDeleteUser(_ context.Context, tenantID, id string) error {
	k := f.key(tenantID, id)
	if _, ok := f.rows[k]; !ok {
		return auth.ErrUserNotFound
	}
	delete(f.rows, k)
	return nil
}

func (f *fakeUserCRUDRepo) CountActiveAdmins(_ context.Context, tenantID string) (int, error) {
	n := 0
	for _, u := range f.rows {
		if u.TenantID == tenantID && u.IsAdmin && u.Status == "active" {
			n++
		}
	}
	return n, nil
}

// UpdateUserGuardLastAdmin mirrors the real repo's atomic last-admin guard: it refuses with
// auth.ErrLastAdmin when only one active admin remains, otherwise applies fields like UpdateUser.
// The in-memory fake has no real concurrency, but it lets the service's demote path be exercised
// and the LAST_ADMIN mapping verified without a DB.
func (f *fakeUserCRUDRepo) UpdateUserGuardLastAdmin(ctx context.Context, tenantID, id string, fields map[string]any) (*auth.User, error) {
	n, _ := f.CountActiveAdmins(ctx, tenantID)
	if n <= 1 {
		return nil, auth.ErrLastAdmin
	}
	return f.UpdateUser(ctx, tenantID, id, fields)
}

// SoftDeleteUserGuardLastAdmin mirrors the real repo's atomic last-admin guard for delete.
func (f *fakeUserCRUDRepo) SoftDeleteUserGuardLastAdmin(ctx context.Context, tenantID, id string) error {
	n, _ := f.CountActiveAdmins(ctx, tenantID)
	if n <= 1 {
		return auth.ErrLastAdmin
	}
	return f.SoftDeleteUser(ctx, tenantID, id)
}

func newUserSvc() (*UserService, *fakeUserCRUDRepo) {
	repo := newFakeUserCRUDRepo()
	svc := NewUserService(repo)
	// Deterministic ids for assertions.
	n := 0
	svc.newID = func() string {
		n++
		return "id-" + string(rune('0'+n))
	}
	return svc, repo
}

// --- 🔒 GATE: create/delete are admin-only -----------------------------------

func TestCreateUser_NonAdminDenied(t *testing.T) {
	svc, repo := newUserSvc()
	_, err := svc.CreateUser(context.Background(), "t1", false, CreateUserInput{
		Email: "new@acme.test", Password: "secret123",
	})
	if !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("err = %v, want ErrUserForbidden", err)
	}
	// And nothing must have been written (no escalation side effect).
	if len(repo.rows) != 0 {
		t.Fatalf("a non-admin create wrote %d rows; want 0", len(repo.rows))
	}
}

func TestDeleteUser_NonAdminDenied(t *testing.T) {
	svc, repo := newUserSvc()
	// Seed a user (as admin) then attempt a non-admin delete.
	created, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "victim@acme.test", Password: "secret123",
	})
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	err = svc.DeleteUser(context.Background(), "t1", "actor-1", false, created.ID)
	if !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("err = %v, want ErrUserForbidden", err)
	}
	if len(repo.rows) != 1 {
		t.Fatalf("non-admin delete removed the row; want it preserved")
	}
}

// 🔒 GATE §17: UpdateUser is admin-gated at the service layer (defense-in-depth alongside the
// RequireAdmin middleware) because setting is_admin/role_id is a direct privilege escalation. A
// non-admin actor is refused with ErrUserForbidden BEFORE any read or write — and crucially
// nothing is mutated (no escalation side effect).
func TestUpdateUser_NonAdminDenied(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "u1", false, "active")
	yes := true
	_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", false, "u1", UpdateUserInput{IsAdmin: &yes})
	if !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("err = %v, want ErrUserForbidden", err)
	}
	if repo.rows[repo.key("t1", "u1")].IsAdmin {
		t.Fatalf("non-admin update escalated is_admin; want unchanged")
	}
}

// --- happy paths -------------------------------------------------------------

func TestCreateUser_AdminHashesPasswordAndHidesHash(t *testing.T) {
	svc, repo := newUserSvc()
	view, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
		Email: "ceo@acme.test", Password: "topsecret", FullName: "CEO", IsAdmin: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if view.Email != "ceo@acme.test" || !view.IsAdmin || view.Status != "active" {
		t.Fatalf("unexpected view: %+v", view)
	}
	// The stored hash must verify against the plaintext and must be a real bcrypt digest
	// at the configured cost (12) — never the plaintext.
	stored := repo.rows[repo.key("t1", view.ID)]
	if stored.PasswordHash == "topsecret" {
		t.Fatalf("password stored in plaintext")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("topsecret")); err != nil {
		t.Fatalf("stored hash does not verify: %v", err)
	}
	if cost, _ := bcrypt.Cost([]byte(stored.PasswordHash)); cost != bcryptCost {
		t.Fatalf("bcrypt cost = %d, want %d", cost, bcryptCost)
	}
}

func TestCreateUser_DuplicateEmailConflict(t *testing.T) {
	svc, _ := newUserSvc()
	in := CreateUserInput{Email: "dup@acme.test", Password: "secret123"}
	if _, err := svc.CreateUser(context.Background(), "t1", true, in); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := svc.CreateUser(context.Background(), "t1", true, in)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_ALREADY_EXISTS" {
		t.Fatalf("err = %v, want USER_ALREADY_EXISTS validation error", err)
	}
}

func TestCreateUser_MissingFields(t *testing.T) {
	svc, _ := newUserSvc()
	_, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{Email: "", Password: ""})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_FIELD_REQUIRED" {
		t.Fatalf("err = %v, want USER_FIELD_REQUIRED", err)
	}
}

func TestGetUser_NotFoundIsValidationError(t *testing.T) {
	svc, _ := newUserSvc()
	_, err := svc.GetUser(context.Background(), "t1", "nope")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_NOT_FOUND" {
		t.Fatalf("err = %v, want USER_NOT_FOUND", err)
	}
}

// A user created in tenant t1 must be invisible from tenant t2 (tenant isolation).
func TestGetUser_CrossTenantIsNotFound(t *testing.T) {
	svc, _ := newUserSvc()
	v, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{Email: "a@a.test", Password: "secret123"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = svc.GetUser(context.Background(), "t2", v.ID)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_NOT_FOUND" {
		t.Fatalf("cross-tenant get err = %v, want USER_NOT_FOUND", err)
	}
}

func TestUpdateUser_AppliesFieldsAndRehashesPassword(t *testing.T) {
	svc, repo := newUserSvc()
	v, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{Email: "u@acme.test", Password: "old-pass-1"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	newName := "Updated Name"
	newPass := "brandnew"
	updated, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, v.ID, UpdateUserInput{
		FullName: &newName,
		Password: &newPass,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.FullName != newName {
		t.Fatalf("full name not applied: %+v", updated)
	}
	stored := repo.rows[repo.key("t1", v.ID)]
	if err := bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte(newPass)); err != nil {
		t.Fatalf("password not re-hashed: %v", err)
	}
}

func TestUpdateUser_NotFound(t *testing.T) {
	svc, _ := newUserSvc()
	name := "x"
	_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "missing", UpdateUserInput{FullName: &name})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_NOT_FOUND" {
		t.Fatalf("err = %v, want USER_NOT_FOUND", err)
	}
}

func TestDeleteUser_AdminSoftDeletes(t *testing.T) {
	svc, repo := newUserSvc()
	v, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{Email: "d@acme.test", Password: "secret123"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeleteUser(context.Background(), "t1", "actor-1", true, v.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(repo.rows) != 0 {
		t.Fatalf("row not removed after admin delete")
	}
}

func TestDeleteUser_NotFound(t *testing.T) {
	svc, _ := newUserSvc()
	err := svc.DeleteUser(context.Background(), "t1", "actor-1", true, "missing")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "USER_NOT_FOUND" {
		t.Fatalf("err = %v, want USER_NOT_FOUND", err)
	}
}

// --- 🔒 ANTI-LOCK-OUT: self-delete + last-admin guards -----------------------

// seedUser writes a row straight into the fake repo with an explicit id/admin/status so the
// lock-out guards can be exercised deterministically (bypassing CreateUser's id generator).
func seedUser(repo *fakeUserCRUDRepo, tenantID, id string, isAdmin bool, status string) {
	repo.rows[repo.key(tenantID, id)] = &auth.User{
		ID: id, TenantID: tenantID, Email: id + "@acme.test", IsAdmin: isAdmin, Status: status,
	}
}

// An admin deleting its OWN account is refused with 422 SELF_DELETE_FORBIDDEN, row preserved.
func TestDeleteUser_SelfDeleteForbidden(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active")
	seedUser(repo, "t1", "admin-2", true, "active") // another admin so last-admin isn't the reason
	err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "admin-1")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "SELF_DELETE_FORBIDDEN" {
		t.Fatalf("err = %v, want SELF_DELETE_FORBIDDEN", err)
	}
	if _, ok := repo.rows[repo.key("t1", "admin-1")]; !ok {
		t.Fatalf("self-delete removed the row; want preserved")
	}
}

// Deleting the LAST active admin is refused with 422 LAST_ADMIN, row preserved.
func TestDeleteUser_LastAdminForbidden(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active") // sole admin
	err := svc.DeleteUser(context.Background(), "t1", "actor-other", true, "admin-1")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "LAST_ADMIN" {
		t.Fatalf("err = %v, want LAST_ADMIN", err)
	}
	if _, ok := repo.rows[repo.key("t1", "admin-1")]; !ok {
		t.Fatalf("last-admin delete removed the row; want preserved")
	}
}

// Deleting an admin while ANOTHER active admin remains succeeds.
func TestDeleteUser_AdminWithSiblingSucceeds(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active")
	seedUser(repo, "t1", "admin-2", true, "active")
	if err := svc.DeleteUser(context.Background(), "t1", "admin-2", true, "admin-1"); err != nil {
		t.Fatalf("delete with sibling admin: %v", err)
	}
	if _, ok := repo.rows[repo.key("t1", "admin-1")]; ok {
		t.Fatalf("admin-1 not removed; want deleted")
	}
}

// A non-admin or disabled target is not protected by LAST_ADMIN — deleting it is fine even as
// the only admin in the tenant is untouched.
func TestDeleteUser_NonAdminTargetNotGuarded(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active") // sole admin (must survive)
	seedUser(repo, "t1", "member-1", false, "active")
	if err := svc.DeleteUser(context.Background(), "t1", "admin-1", true, "member-1"); err != nil {
		t.Fatalf("delete non-admin member: %v", err)
	}
}

// Demoting (is_admin=false) the LAST active admin — including oneself — is refused with 422 LAST_ADMIN.
func TestUpdateUser_DemoteLastAdminForbidden(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active") // sole admin
	no := false
	_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{IsAdmin: &no})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "LAST_ADMIN" {
		t.Fatalf("err = %v, want LAST_ADMIN", err)
	}
	if !repo.rows[repo.key("t1", "admin-1")].IsAdmin {
		t.Fatalf("admin-1 was demoted; want unchanged")
	}
}

// Disabling (status != active) the LAST active admin is refused with 422 LAST_ADMIN.
func TestUpdateUser_DisableLastAdminForbidden(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active") // sole admin
	disabled := "disabled"
	_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{Status: &disabled})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "LAST_ADMIN" {
		t.Fatalf("err = %v, want LAST_ADMIN", err)
	}
	if repo.rows[repo.key("t1", "admin-1")].Status != "active" {
		t.Fatalf("admin-1 was disabled; want unchanged")
	}
}

// Demoting an admin while ANOTHER active admin remains succeeds.
func TestUpdateUser_DemoteAdminWithSiblingSucceeds(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "admin-1", true, "active")
	seedUser(repo, "t1", "admin-2", true, "active")
	no := false
	updated, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{IsAdmin: &no})
	if err != nil {
		t.Fatalf("demote with sibling admin: %v", err)
	}
	if updated.IsAdmin {
		t.Fatalf("admin-1 still admin after demote; want isAdmin=false")
	}
}

// --- RM-19: __user__ audit trail (create / update / delete) ------------------------------

func userAuditCtx() context.Context { return audit.WithActor(context.Background(), "admin-actor") }

// assertNoPasswordMaterial fails when the serialized diff carries any password secret: one of
// the plaintexts handed in, a bcrypt digest prefix, or a hash key.
func assertNoPasswordMaterial(t *testing.T, d *audit.Diff, plaintexts ...string) {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal diff: %v", err)
	}
	raw := string(b)
	for _, s := range append(plaintexts, "$2a$", "$2b$", "$2y$", "passwordHash", "password_hash") {
		if strings.Contains(raw, s) {
			t.Errorf("audit diff carries password material %q: %s", s, raw)
		}
	}
}

// A committed create emits one __user__ create row: after-only safe view, actor from ctx,
// record id = the new user's id, and never the password (plaintext or hash).
func TestCreateUser_EmitsUserAudit(t *testing.T) {
	svc, _ := newUserSvc()
	aud := &captureAuditor{}
	svc.WithAuditor(aud)
	v, err := svc.CreateUser(userAuditCtx(), "t1", true, CreateUserInput{
		Email: "new@acme.test", Password: "topsecret-1", FullName: "New",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(aud.events))
	}
	e := aud.events[0]
	if e.EntityName != audit.UserMeta || e.Action != audit.ActionCreate || e.TenantID != "t1" ||
		e.ActorUserID != "admin-actor" || e.RecordID != v.ID {
		t.Fatalf("event = %+v; want __user__ create in t1 by admin-actor with record id %q", e, v.ID)
	}
	if e.Diff == nil || e.Diff.Before != nil || e.Diff.After["email"] != "new@acme.test" || e.Diff.After["id"] != v.ID {
		t.Fatalf("create diff = %+v; want after-only safe view", e.Diff)
	}
	assertNoPasswordMaterial(t, e.Diff, "topsecret-1")
}

// A committed update emits one __user__ update row whose diff holds only the changed keys and
// whose record id names the target user (so a privilege/credential flip is attributable to the
// account it happened to); a password change appears solely as the after-side marker
// {password:"[redacted]"}.
func TestUpdateUser_EmitsUserAudit(t *testing.T) {
	name := "Renamed"
	pw := "brandnew-pw-1"
	disabled := "disabled"
	cases := []struct {
		name       string
		in         UpdateUserInput
		wantBefore map[string]any
		wantAfter  map[string]any
	}{
		{"rename only", UpdateUserInput{FullName: &name}, nil, map[string]any{"fullName": "Renamed"}},
		{"password only", UpdateUserInput{Password: &pw}, nil, map[string]any{"password": "[redacted]"}},
		{"disable", UpdateUserInput{Status: &disabled}, map[string]any{"status": "active"}, map[string]any{"status": "disabled"}},
		{"rename + password", UpdateUserInput{FullName: &name, Password: &pw}, nil,
			map[string]any{"fullName": "Renamed", "password": "[redacted]"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newUserSvc()
			seedUser(repo, "t1", "u1", false, "active")
			aud := &captureAuditor{}
			svc.WithAuditor(aud)
			if _, err := svc.UpdateUser(userAuditCtx(), "t1", "actor-admin", true, "u1", tc.in); err != nil {
				t.Fatalf("update: %v", err)
			}
			if len(aud.events) != 1 {
				t.Fatalf("want 1 audit event, got %d", len(aud.events))
			}
			e := aud.events[0]
			if e.EntityName != audit.UserMeta || e.Action != audit.ActionUpdate || e.TenantID != "t1" ||
				e.ActorUserID != "admin-actor" || e.RecordID != "u1" {
				t.Fatalf("event = %+v; want __user__ update in t1 by admin-actor with record id u1", e)
			}
			if e.Diff == nil || !reflect.DeepEqual(e.Diff.Before, tc.wantBefore) || !reflect.DeepEqual(e.Diff.After, tc.wantAfter) {
				t.Fatalf("diff = %+v; want before=%v after=%v", e.Diff, tc.wantBefore, tc.wantAfter)
			}
			assertNoPasswordMaterial(t, e.Diff, pw)
		})
	}
}

// A refused or failed update writes nothing, so it must leave no trail either (audit reflects
// committed mutations only).
func TestUpdateUser_RefusedEmitsNoAudit(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "u1", false, "active")
	aud := &captureAuditor{}
	svc.WithAuditor(aud)
	name := "x"
	if _, err := svc.UpdateUser(userAuditCtx(), "t1", "actor-admin", false, "u1", UpdateUserInput{FullName: &name}); !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("non-admin err = %v, want ErrUserForbidden", err)
	}
	if _, err := svc.UpdateUser(userAuditCtx(), "t1", "actor-admin", true, "missing", UpdateUserInput{FullName: &name}); err == nil {
		t.Fatalf("unknown id: want USER_NOT_FOUND, got nil")
	}
	if len(aud.events) != 0 {
		t.Fatalf("refused/failed updates emitted %d audit events; want 0", len(aud.events))
	}
}

// A committed delete emits one __user__ delete row: before-only safe view of the removed
// account, record id = the removed user's id.
func TestDeleteUser_EmitsUserAudit(t *testing.T) {
	svc, repo := newUserSvc()
	seedUser(repo, "t1", "u1", false, "active")
	aud := &captureAuditor{}
	svc.WithAuditor(aud)
	if err := svc.DeleteUser(userAuditCtx(), "t1", "admin-1", true, "u1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("want 1 audit event, got %d", len(aud.events))
	}
	e := aud.events[0]
	if e.EntityName != audit.UserMeta || e.Action != audit.ActionDelete || e.TenantID != "t1" ||
		e.ActorUserID != "admin-actor" || e.RecordID != "u1" {
		t.Fatalf("event = %+v; want __user__ delete in t1 by admin-actor with record id u1", e)
	}
	wantBefore := map[string]any{"id": "u1", "tenantId": "t1", "email": "u1@acme.test", "status": "active", "isAdmin": false, "canManageUsers": false}
	if e.Diff == nil || e.Diff.After != nil || !reflect.DeepEqual(e.Diff.Before, wantBefore) {
		t.Fatalf("delete diff = %+v; want before-only %v", e.Diff, wantBefore)
	}
	assertNoPasswordMaterial(t, e.Diff)
}
