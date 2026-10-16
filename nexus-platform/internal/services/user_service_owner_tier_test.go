package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/notify"
)

func TestSetUserManager(t *testing.T) {
	ctx := context.Background()

	t.Run("non-operator actor is forbidden, nothing written", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.SetUserManager(ctx, "t1", false, "staff1", true); !errors.Is(err, ErrUserForbidden) {
			t.Fatalf("err = %v", err)
		}
		if f.repo.rows[f.repo.key("t1", "staff1")].CanManageUsers {
			t.Fatal("flag written by a non-operator")
		}
	})
	t.Run("admin target is a tier conflict", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.SetUserManager(ctx, "t1", true, "admin1", true); codeOf(err) != "USER_TIER_CONFLICT" {
			t.Fatalf("err = %v", err)
		}
		if f.repo.rows[f.repo.key("t1", "admin1")].CanManageUsers {
			t.Fatal("admin was made owner")
		}
	})
	t.Run("unknown / other-tenant id is 404", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.SetUserManager(ctx, "t1", true, "x1", true); codeOf(err) != "USER_NOT_FOUND" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("enable: view + invalidate + revoke + E2", func(t *testing.T) {
		f := newOwnerFixture(t)
		v, err := f.svc.SetUserManager(ctx, "t1", true, "staff1", true)
		if err != nil || !v.CanManageUsers {
			t.Fatalf("v=%+v err=%v", v, err)
		}
		assertOneInvalidate(t, f.inv, "t1", "staff1")
		if len(f.rev.calls) != 1 {
			t.Fatalf("revoke calls = %v", f.rev.calls)
		}
		if len(f.notif.events) != 1 || f.notif.events[0].Type != notify.AlertRoleChanged {
			t.Fatalf("alerts = %+v", f.notif.events)
		}
	})
	t.Run("same value is a no-op", func(t *testing.T) {
		f := newOwnerFixture(t)
		if _, err := f.svc.SetUserManager(ctx, "t1", true, "owner2", true); err != nil {
			t.Fatal(err)
		}
		if len(f.rev.calls) != 0 || len(f.inv.calls) != 0 || len(f.notif.events) != 0 {
			t.Fatalf("no-op had side effects: rev=%v inv=%v alerts=%+v", f.rev.calls, f.inv.calls, f.notif.events)
		}
	})
	t.Run("disable leaves role_id alone (FR-008)", func(t *testing.T) {
		f := newOwnerFixture(t)
		f.repo.rows[f.repo.key("t1", "owner2")].RoleID = sp("r-owner")
		v, err := f.svc.SetUserManager(ctx, "t1", true, "owner2", false)
		if err != nil {
			t.Fatal(err)
		}
		if v.CanManageUsers || v.RoleID == nil || *v.RoleID != "r-owner" {
			t.Fatalf("disable touched the role: %+v", v)
		}
		if len(f.rev.calls) != 1 {
			t.Fatalf("revoke calls = %v", f.rev.calls)
		}
	})
}

// An admin cannot promote an owner to admin: 422 before the write, and the repo's CHECK (3819) is the net.
func TestUpdateUser_AdminOnOwnerIsTierConflict(t *testing.T) {
	f := newOwnerFixture(t)
	yes := true
	_, err := f.svc.UpdateUser(context.Background(), "t1", "actor-admin", true, "owner2", UpdateUserInput{IsAdmin: &yes})
	if codeOf(err) != "USER_TIER_CONFLICT" {
		t.Fatalf("err = %v", err)
	}
	if f.repo.rows[f.repo.key("t1", "owner2")].IsAdmin {
		t.Fatal("owner promoted to admin")
	}
}
