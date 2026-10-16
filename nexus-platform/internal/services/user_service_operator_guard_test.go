package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// specs/065 B2 (FR-018): an admin without the operator flag cannot act on an operator account.
func TestAssertAdminMayAct(t *testing.T) {
	ctx := context.Background()
	newF := func(t *testing.T) *ownerFixture {
		f := newOwnerFixture(t)
		f.repo.rows[f.repo.key("t1", "op2")] = &auth.User{ID: "op2", TenantID: "t1", Email: "op2@acme.test", Status: "active", IsAdmin: true, IsPlatformOperator: true}
		return f
	}

	t.Run("plain admin on operator is refused", func(t *testing.T) {
		f := newF(t)
		if err := f.svc.AssertAdminMayAct(ctx, "t1", "admin1", "op1"); !errors.Is(err, auth.ErrUserNotManageable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("operator on operator passes", func(t *testing.T) {
		f := newF(t)
		if err := f.svc.AssertAdminMayAct(ctx, "t1", "op2", "op1"); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("operator target + vanished actor fails closed", func(t *testing.T) {
		f := newF(t)
		if err := f.svc.AssertAdminMayAct(ctx, "t1", "ghost", "op1"); !errors.Is(err, auth.ErrUserNotManageable) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("non-operator target does not read the actor", func(t *testing.T) {
		f := newF(t)
		f.repo.gets = nil
		if err := f.svc.AssertAdminMayAct(ctx, "t1", "admin1", "staff1"); err != nil {
			t.Fatalf("err = %v", err)
		}
		for _, id := range f.repo.gets {
			if id == "admin1" {
				t.Fatalf("actor row was read for a non-operator target: %v", f.repo.gets)
			}
		}
	})
	t.Run("unknown target is 404", func(t *testing.T) {
		f := newF(t)
		if err := f.svc.AssertAdminMayAct(ctx, "t1", "admin1", "nobody"); codeOf(err) != "USER_NOT_FOUND" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestUpdateDeleteUser_OperatorGuard(t *testing.T) {
	ctx := context.Background()
	t.Run("update", func(t *testing.T) {
		f := newOwnerFixture(t)
		name := "hijack"
		if _, err := f.svc.UpdateUser(ctx, "t1", "admin1", true, "op1", UpdateUserInput{FullName: &name}); !errors.Is(err, auth.ErrUserNotManageable) {
			t.Fatalf("err = %v", err)
		}
		if f.repo.rows[f.repo.key("t1", "op1")].FullName == name {
			t.Fatal("operator account was written")
		}
	})
	t.Run("delete", func(t *testing.T) {
		f := newOwnerFixture(t)
		if err := f.svc.DeleteUser(ctx, "t1", "admin1", true, "op1"); !errors.Is(err, auth.ErrUserNotManageable) {
			t.Fatalf("err = %v", err)
		}
		if _, ok := f.repo.rows[f.repo.key("t1", "op1")]; !ok {
			t.Fatal("operator account was deleted")
		}
	})
	t.Run("operator may update operator", func(t *testing.T) {
		f := newOwnerFixture(t)
		f.repo.rows[f.repo.key("t1", "op2")] = &auth.User{ID: "op2", TenantID: "t1", Email: "op2@x", Status: "active", IsAdmin: true, IsPlatformOperator: true}
		name := "fine"
		if _, err := f.svc.UpdateUser(ctx, "t1", "op2", true, "op1", UpdateUserInput{FullName: &name}); err != nil {
			t.Fatalf("err = %v", err)
		}
	})
}
