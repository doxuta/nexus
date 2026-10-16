package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// specs/065 FR-041 / D-18: the owner-tier role name is reserved — no create, rename to/from, delete.
func TestRole_ReservedOwnerName(t *testing.T) {
	ctx := context.Background()
	str := func(s string) *string { return &s }

	seed := func(t *testing.T, name string) (*RoleService, *fakeRoleRepo, string) {
		svc, repo := newRoleSvc()
		v, err := svc.CreateRole(ctx, "t1", CreateRoleInput{Name: "seed-" + name})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Put the real name in directly: CreateRole itself must refuse it.
		repo.roles[repo.key("t1", v.ID)].Name = name
		return svc, repo, v.ID
	}
	wantCode := func(t *testing.T, err error, code string) {
		t.Helper()
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != code {
			t.Fatalf("want %s, got %v", code, err)
		}
	}

	t.Run("create with reserved name", func(t *testing.T) {
		svc, repo := newRoleSvc()
		_, err := svc.CreateRole(ctx, "t1", CreateRoleInput{Name: " " + auth.OwnerRoleName})
		wantCode(t, err, "ROLE_NAME_RESERVED")
		if len(repo.roles) != 0 {
			t.Fatal("repo written despite refusal")
		}
	})
	// acl_roles is utf8mb4_unicode_ci: case variants are the same name to the DB, so to the guard too.
	for _, v := range []string{"Tenant_Owner", "TENANT_OWNER"} {
		v := v
		t.Run("case variant "+v, func(t *testing.T) {
			svc, repo := newRoleSvc()
			_, err := svc.CreateRole(ctx, "t1", CreateRoleInput{Name: v})
			wantCode(t, err, "ROLE_NAME_RESERVED")
			if len(repo.roles) != 0 {
				t.Fatal("repo written despite refusal")
			}
			svc, _, id := seed(t, "sales")
			_, err = svc.UpdateRole(ctx, "t1", id, UpdateRoleInput{Name: str(v)})
			wantCode(t, err, "ROLE_NAME_RESERVED")
			svc, _, id = seed(t, v)
			wantCode(t, svc.DeleteRole(ctx, "t1", id), "ROLE_NAME_RESERVED")
		})
	}
	t.Run("rename ordinary role to reserved name", func(t *testing.T) {
		svc, _, id := seed(t, "sales")
		_, err := svc.UpdateRole(ctx, "t1", id, UpdateRoleInput{Name: str(auth.OwnerRoleName)})
		wantCode(t, err, "ROLE_NAME_RESERVED")
	})
	t.Run("rename reserved role away", func(t *testing.T) {
		svc, repo, id := seed(t, auth.OwnerRoleName)
		_, err := svc.UpdateRole(ctx, "t1", id, UpdateRoleInput{Name: str("boss")})
		wantCode(t, err, "ROLE_NAME_RESERVED")
		if repo.roles[repo.key("t1", id)].Name != auth.OwnerRoleName {
			t.Fatal("name changed despite refusal")
		}
	})
	t.Run("reserved role label and same-name resend allowed", func(t *testing.T) {
		svc, _, id := seed(t, auth.OwnerRoleName)
		if _, err := svc.UpdateRole(ctx, "t1", id, UpdateRoleInput{Name: str(auth.OwnerRoleName), Label: str("Chủ")}); err != nil {
			t.Fatalf("same-name resend refused: %v", err)
		}
	})
	t.Run("delete reserved role", func(t *testing.T) {
		svc, repo, id := seed(t, auth.OwnerRoleName)
		wantCode(t, svc.DeleteRole(ctx, "t1", id), "ROLE_NAME_RESERVED")
		if _, ok := repo.roles[repo.key("t1", id)]; !ok {
			t.Fatal("reserved role deleted")
		}
	})
	t.Run("ordinary role unchanged", func(t *testing.T) {
		svc, _, id := seed(t, "sales")
		if _, err := svc.UpdateRole(ctx, "t1", id, UpdateRoleInput{Name: str("sales2")}); err != nil {
			t.Fatal(err)
		}
		if err := svc.DeleteRole(ctx, "t1", id); err != nil {
			t.Fatal(err)
		}
	})
}
