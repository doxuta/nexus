package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/notify"
)

// ownerFixture: tenant t1 holds an owner (the actor), staff, admin, another owner, an operator and a
// disabled staff account; t2 holds one staff account. Roles: a plain "sales" role and the reserved owner role.
type ownerFixture struct {
	svc   *UserService
	repo  *fakeUserCRUDRepo
	roles *fakeRoleRepo
	rev   *w1FakeRevoker
	notif *notifyFakeNotifier
	inv   *claimFakeInvalidator
}

func newOwnerFixture(t *testing.T) *ownerFixture {
	t.Helper()
	rev := &w1FakeRevoker{}
	notif := &notifyFakeNotifier{}
	inv := &claimFakeInvalidator{}
	svc, repo := newW1UserSvc(rev)
	roles := newFakeRoleRepo()
	svc = svc.WithNotifier(notif).WithClaimCache(inv).WithRoleValidator(roles).WithRoleLister(roles)

	put := func(tenant, id string, mut func(*auth.User)) {
		u := &auth.User{ID: id, TenantID: tenant, Email: id + "@acme.test", FullName: "Name " + id, Status: "active"}
		if mut != nil {
			mut(u)
		}
		repo.rows[repo.key(tenant, id)] = u
	}
	put("t1", "owner1", func(u *auth.User) { u.CanManageUsers = true })
	put("t1", "owner2", func(u *auth.User) { u.CanManageUsers = true })
	put("t1", "staff1", nil)
	put("t1", "admin1", func(u *auth.User) { u.IsAdmin = true })
	put("t1", "op1", func(u *auth.User) { u.IsAdmin = true; u.IsPlatformOperator = true })
	put("t1", "dis1", func(u *auth.User) { u.Status = "disabled" })
	put("t2", "x1", nil)
	roles.roles[roles.key("t1", "r-sales")] = &auth.Role{ID: "r-sales", TenantID: "t1", Name: "sales", Label: "Bán hàng"}
	roles.roles[roles.key("t1", "r-bare")] = &auth.Role{ID: "r-bare", TenantID: "t1", Name: "bare"}
	roles.roles[roles.key("t1", "r-owner")] = &auth.Role{ID: "r-owner", TenantID: "t1", Name: auth.OwnerRoleName, Label: "Chủ khách"}
	roles.roles[roles.key("t2", "r-other")] = &auth.Role{ID: "r-other", TenantID: "t2", Name: "other"}
	return &ownerFixture{svc: svc, repo: repo, roles: roles, rev: rev, notif: notif, inv: inv}
}

func TestUpdateOrgUser_Guards(t *testing.T) {
	cases := []struct {
		name      string
		actor     string
		target    string
		in        OrgUpdateInput
		wantErr   error  // sentinel (errors.Is)
		wantCode  string // ValidationError code
		untouched bool   // target row must be byte-for-byte unchanged
	}{
		{"staff ok", "owner1", "staff1", OrgUpdateInput{FullName: sp("New")}, nil, "", false},
		{"admin target", "owner1", "admin1", OrgUpdateInput{FullName: sp("x")}, auth.ErrUserNotManageable, "", true},
		{"operator target", "owner1", "op1", OrgUpdateInput{FullName: sp("x")}, auth.ErrUserNotManageable, "", true},
		{"other owner target", "owner1", "owner2", OrgUpdateInput{FullName: sp("x")}, auth.ErrUserNotManageable, "", true},
		{"self", "owner1", "owner1", OrgUpdateInput{FullName: sp("x")}, auth.ErrUserNotManageable, "", true},
		{"disabled target", "owner1", "dis1", OrgUpdateInput{Status: sp("active")}, auth.ErrUserNotManageable, "", true},
		{"other tenant id", "owner1", "x1", OrgUpdateInput{FullName: sp("x")}, nil, "USER_NOT_FOUND", true},
		{"owner role not assignable", "owner1", "staff1", OrgUpdateInput{RoleID: sp("r-owner")}, nil, "ROLE_NOT_ASSIGNABLE", true},
		{"role of another tenant", "owner1", "staff1", OrgUpdateInput{RoleID: sp("r-other")}, nil, "ROLE_INVALID", true},
		{"status disabled refused", "owner1", "staff1", OrgUpdateInput{Status: sp("disabled")}, nil, "USER_STATUS_INVALID", true},
		{"status garbage refused", "owner1", "staff1", OrgUpdateInput{Status: sp("xyz")}, nil, "USER_STATUS_INVALID", true},
		{"actor lost the flag", "staff1", "owner2", OrgUpdateInput{FullName: sp("x")}, ErrUserForbidden, "", true},
		{"actor is an admin", "admin1", "staff1", OrgUpdateInput{FullName: sp("x")}, ErrUserForbidden, "", true},
		{"actor deleted", "ghost", "staff1", OrgUpdateInput{FullName: sp("x")}, ErrUserForbidden, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newOwnerFixture(t)
			tenant := "t1"
			if tc.target == "x1" {
				tenant = "t2" // the id exists, but in another tenant: must read as 404 and stay untouched
			}
			before := *f.repo.rows[f.repo.key(tenant, tc.target)]
			_, err := f.svc.UpdateOrgUser(context.Background(), "t1", tc.actor, tc.target, tc.in)
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantCode != "":
				if codeOf(err) != tc.wantCode {
					t.Fatalf("err = %v, want code %s", err, tc.wantCode)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
			}
			if tc.untouched {
				if got := *f.repo.rows[f.repo.key(tenant, tc.target)]; got != before {
					t.Fatalf("refused write still changed the row: %+v -> %+v", before, got)
				}
			}
		})
	}
}

func TestUpdateOrgUser_RoleClearAndSideEffects(t *testing.T) {
	ctx := context.Background()
	t.Run("roleId empty clears to NULL and revokes + E2", func(t *testing.T) {
		f := newOwnerFixture(t)
		f.repo.rows[f.repo.key("t1", "staff1")].RoleID = sp("r-sales")
		v, err := f.svc.UpdateOrgUser(ctx, "t1", "owner1", "staff1", OrgUpdateInput{RoleID: sp("")})
		if err != nil {
			t.Fatal(err)
		}
		if v.RoleID != nil {
			t.Fatalf("role not cleared: %v", *v.RoleID)
		}
		if len(f.rev.calls) != 1 || f.rev.calls[0] != [2]string{"t1", "staff1"} {
			t.Fatalf("revoke calls = %v", f.rev.calls)
		}
		if len(f.notif.events) != 1 || f.notif.events[0].Type != notify.AlertRoleChanged {
			t.Fatalf("alerts = %+v", f.notif.events)
		}
		assertOneInvalidate(t, f.inv, "t1", "staff1")
	})
	t.Run("assign real role revokes + E2", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.UpdateOrgUser(ctx, "t1", "owner1", "staff1", OrgUpdateInput{RoleID: sp("r-sales")}); err != nil {
			t.Fatal(err)
		}
		if len(f.rev.calls) != 1 || len(f.notif.events) != 1 {
			t.Fatalf("revoke=%v alerts=%+v", f.rev.calls, f.notif.events)
		}
	})
	t.Run("lock revokes + E3, reopen does not", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.UpdateOrgUser(ctx, "t1", "owner1", "staff1", OrgUpdateInput{Status: sp("inactive")}); err != nil {
			t.Fatal(err)
		}
		if len(f.rev.calls) != 1 || len(f.notif.events) != 1 || f.notif.events[0].Type != notify.AlertAccountDisabled {
			t.Fatalf("revoke=%v alerts=%+v", f.rev.calls, f.notif.events)
		}
		if _, err := f.svc.UpdateOrgUser(ctx, "t1", "owner1", "staff1", OrgUpdateInput{Status: sp("active")}); err != nil {
			t.Fatal(err)
		}
		if len(f.rev.calls) != 1 {
			t.Fatalf("reopen revoked again: %v", f.rev.calls)
		}
	})
	t.Run("rename only: no revoke, no alert", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.UpdateOrgUser(ctx, "t1", "owner1", "staff1", OrgUpdateInput{FullName: sp("Tên mới")}); err != nil {
			t.Fatal(err)
		}
		if len(f.rev.calls) != 0 || len(f.notif.events) != 0 {
			t.Fatalf("revoke=%v alerts=%+v", f.rev.calls, f.notif.events)
		}
	})
	t.Run("same role resent: no revoke", func(t *testing.T) {
		f := newOwnerFixture(t)
		f.repo.rows[f.repo.key("t1", "staff1")].RoleID = sp("r-sales")
		if _, err := f.svc.UpdateOrgUser(ctx, "t1", "owner1", "staff1", OrgUpdateInput{RoleID: sp("r-sales"), Status: sp("active")}); err != nil {
			t.Fatal(err)
		}
		if len(f.rev.calls) != 0 {
			t.Fatalf("revoked on no-op: %v", f.rev.calls)
		}
	})
}

func TestListOrgUsers_TierManageableAndShape(t *testing.T) {
	f := newOwnerFixture(t)
	views, err := f.svc.ListOrgUsers(context.Background(), "t1", "owner1")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]OrgUserView{}
	for _, v := range views {
		got[v.ID] = v
	}
	if len(got) != 6 {
		t.Fatalf("want the 6 t1 users, got %d (no t2 leakage)", len(got))
	}
	want := map[string]struct {
		tier   string
		manage bool
	}{
		"staff1": {"staff", true}, "owner1": {"owner", false}, "owner2": {"owner", false},
		"admin1": {"admin", false}, "op1": {"admin", false}, "dis1": {"staff", false},
	}
	for id, w := range want {
		if got[id].Tier != w.tier || got[id].Manageable != w.manage {
			t.Errorf("%s: tier=%q manageable=%v, want %q/%v", id, got[id].Tier, got[id].Manageable, w.tier, w.manage)
		}
	}
	raw, _ := json.Marshal(got["op1"])
	for _, banned := range []string{"operator", "Operator", "tenantId", "password", "canManageUsers"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("owner list view leaks %q: %s", banned, raw)
		}
	}
	if _, err := f.svc.ListOrgUsers(context.Background(), "t1", "staff1"); !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("non-owner list err = %v", err)
	}
}

func TestGetOrgUser(t *testing.T) {
	f := newOwnerFixture(t)
	if v, err := f.svc.GetOrgUser(context.Background(), "t1", "owner1", "staff1"); err != nil || v.ID != "staff1" {
		t.Fatalf("get staff: %v %+v", err, v)
	}
	if _, err := f.svc.GetOrgUser(context.Background(), "t1", "owner1", "x1"); codeOf(err) != "USER_NOT_FOUND" {
		t.Fatalf("cross-tenant id: %v", err)
	}
}

func TestListAssignableRoles(t *testing.T) {
	f := newOwnerFixture(t)
	roles, err := f.svc.ListAssignableRoles(context.Background(), "t1", "owner1")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]OrgRoleView{}
	for _, r := range roles {
		by[r.ID] = r
	}
	if len(by) != 3 {
		t.Fatalf("want 3 t1 roles, got %d", len(by))
	}
	if by["r-sales"].Label != "Bán hàng" || !by["r-sales"].Assignable {
		t.Errorf("sales = %+v", by["r-sales"])
	}
	if by["r-bare"].Label != "bare" {
		t.Errorf("empty label must fall back to name, got %q", by["r-bare"].Label)
	}
	if by["r-owner"].Assignable {
		t.Errorf("owner role must not be assignable")
	}
	raw, _ := json.Marshal(roles)
	if strings.Contains(string(raw), "actions") || strings.Contains(string(raw), "grant") {
		t.Errorf("role view leaks grants: %s", raw)
	}
	if _, err := f.svc.ListAssignableRoles(context.Background(), "t1", "staff1"); !errors.Is(err, ErrUserForbidden) {
		t.Fatalf("non-owner err = %v", err)
	}
}
