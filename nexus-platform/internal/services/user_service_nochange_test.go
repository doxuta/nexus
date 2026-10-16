package services

import (
	"context"
	"testing"

	"nexus/platform/internal/notify"
)

// spec 065 lát A (FR-001): revoke + E2/E3 fire only when role / is_admin / status REALLY change
// versus the stored row. The admin form used to resend the unchanged isAdmin/roleId/status with
// every save, so a bare rename logged the user out and mailed a "role changed" alert.
func TestUpdateUser_ChangeDetection_RevokeAndAlerts(t *testing.T) {
	role := "role-a"
	empty := ""
	other := "role-b"
	yes, no := true, false
	active, inactive, disabled := "active", "inactive", "disabled"
	name := "New Name"
	pass := "Str0ngNewP@ss!"

	cases := []struct {
		name       string
		seedAdmin  bool
		seedStatus string
		seedRole   *string
		in         UpdateUserInput
		wantRevoke int
		wantAlerts []string
	}{
		{"same roleId resent", false, "active", &role, UpdateUserInput{RoleID: &role}, 0, nil},
		{"same isAdmin resent (false)", false, "active", nil, UpdateUserInput{IsAdmin: &no}, 0, nil},
		{"same isAdmin resent (true)", true, "active", nil, UpdateUserInput{IsAdmin: &yes}, 0, nil},
		{"fullName only", false, "active", &role, UpdateUserInput{FullName: &name}, 0, nil},
		{"full old form payload, nothing changed", false, "active", &role,
			UpdateUserInput{FullName: &name, IsAdmin: &no, RoleID: &role, Status: &active}, 0, nil},
		{"roleId NULL to NULL", false, "active", nil, UpdateUserInput{RoleID: &empty}, 0, nil},
		{"roleId changed", false, "active", &role, UpdateUserInput{RoleID: &other}, 1, []string{notify.AlertRoleChanged}},
		{"roleId cleared", false, "active", &role, UpdateUserInput{RoleID: &empty}, 1, []string{notify.AlertRoleChanged}},
		{"active to inactive", false, "active", nil, UpdateUserInput{Status: &inactive}, 1, []string{notify.AlertAccountDisabled}},
		{"inactive to inactive", false, "inactive", nil, UpdateUserInput{Status: &inactive}, 0, nil},
		{"inactive to disabled (a real status change off active)", false, "inactive", nil, UpdateUserInput{Status: &disabled}, 1, []string{notify.AlertAccountDisabled}},
		{"inactive to active", false, "inactive", nil, UpdateUserInput{Status: &active}, 0, nil},
		{"password change", false, "active", nil, UpdateUserInput{Password: &pass}, 1, []string{notify.AlertPasswordChanged}},
		{"role + password revokes once", false, "active", &role, UpdateUserInput{RoleID: &other, Password: &pass}, 1,
			[]string{notify.AlertPasswordChanged, notify.AlertRoleChanged}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rev := &w1FakeRevoker{}
			n := &notifyFakeNotifier{}
			svc, repo := newW1UserSvc(rev)
			svc = svc.WithNotifier(n)
			w1Seed(repo, "t1", "u1", tc.seedAdmin, tc.seedStatus, tc.seedRole)
			// sibling admin so demote/disable guards never interfere with the assertion
			w1Seed(repo, "t1", "admin-x", true, "active", nil)

			if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", tc.in); err != nil {
				t.Fatalf("update: %v", err)
			}
			if len(rev.calls) != tc.wantRevoke {
				t.Fatalf("revoke calls = %d (%v); want %d", len(rev.calls), rev.calls, tc.wantRevoke)
			}
			if len(n.events) != len(tc.wantAlerts) {
				t.Fatalf("alerts = %+v; want types %v", n.events, tc.wantAlerts)
			}
			for i, w := range tc.wantAlerts {
				if n.events[i].Type != w {
					t.Fatalf("alert[%d] = %q; want %q", i, n.events[i].Type, w)
				}
			}
		})
	}
}

// Resending isAdmin=true for the only admin is not a demotion: no LAST_ADMIN, no revoke.
func TestUpdateUser_ResendAdminTrueOnLastAdminIsNoop(t *testing.T) {
	rev := &w1FakeRevoker{}
	svc, repo := newW1UserSvc(rev)
	w1Seed(repo, "t1", "admin-1", true, "active", nil)
	yes := true
	name := "Boss"
	if _, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "admin-1", UpdateUserInput{FullName: &name, IsAdmin: &yes}); err != nil {
		t.Fatalf("resend isAdmin=true on last admin: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("revoke fired: %v", rev.calls)
	}
}

// spec 065 FR-002: status is a closed set {active, inactive, disabled}.
func TestUserStatusValidation(t *testing.T) {
	for _, tc := range []struct {
		status string
		ok     bool
	}{
		{"active", true}, {"inactive", true}, {"disabled", true},
		{"xyz", false}, {"Active", false}, {"suspended", false},
	} {
		t.Run("update "+tc.status, func(t *testing.T) {
			svc, repo := newW1UserSvc(&w1FakeRevoker{})
			w1Seed(repo, "t1", "u1", false, "active", nil)
			w1Seed(repo, "t1", "admin-x", true, "active", nil)
			st := tc.status
			_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Status: &st})
			checkStatusErr(t, err, tc.ok)
			if !tc.ok && repo.rows[repo.key("t1", "u1")].Status != "active" {
				t.Fatalf("row written despite invalid status")
			}
		})
		t.Run("create "+tc.status, func(t *testing.T) {
			svc, repo := newW1UserSvc(&w1FakeRevoker{})
			_, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{
				Email: "n@acme.test", Password: "Str0ngNewP@ss!", Status: tc.status,
			})
			checkStatusErr(t, err, tc.ok)
			if !tc.ok && len(repo.rows) != 0 {
				t.Fatalf("row written despite invalid status")
			}
		})
	}
	t.Run("update empty stays 422", func(t *testing.T) {
		svc, repo := newW1UserSvc(&w1FakeRevoker{})
		w1Seed(repo, "t1", "u1", false, "active", nil)
		e := ""
		_, err := svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "u1", UpdateUserInput{Status: &e})
		checkStatusErr(t, err, false)
	})
	t.Run("create empty defaults active", func(t *testing.T) {
		svc, _ := newW1UserSvc(&w1FakeRevoker{})
		v, err := svc.CreateUser(context.Background(), "t1", true, CreateUserInput{Email: "n@acme.test", Password: "Str0ngNewP@ss!"})
		if err != nil || v.Status != "active" {
			t.Fatalf("got %+v, %v", v, err)
		}
	})
}

func checkStatusErr(t *testing.T, err error, ok bool) {
	t.Helper()
	if ok {
		if err != nil {
			t.Fatalf("want ok, got %v", err)
		}
		return
	}
	ve, isVE := err.(*ValidationError)
	if !isVE || ve.Code != "USER_STATUS_INVALID" {
		t.Fatalf("want USER_STATUS_INVALID, got %v", err)
	}
}
