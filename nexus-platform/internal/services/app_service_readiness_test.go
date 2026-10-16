package services

// Unit tests for AppService.Readiness (TRỤ 2 mục 4 item 5, docs/NEXUS-MOT-XUONG-2026-07-29.md):
// the "App sẵn sàng thao tác" honesty gate replacing a raw field-count heuristic. Builds on the
// fakes/fixture already defined in app_service_test.go (newAppSvcFixture: tenant-a owns entities
// ent-accounts/accounts and ent-contacts/contacts).

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
)

type fakeReadinessRoles struct {
	roleIDs []string
	err     error
}

func (f *fakeReadinessRoles) RolesGrantingView(_ context.Context, _ string, _ []string) ([]string, error) {
	return f.roleIDs, f.err
}

type fakeReadinessUsers struct {
	users []auth.User
	err   error
}

func (f *fakeReadinessUsers) ListUsers(_ context.Context, _ string) ([]auth.User, error) {
	return f.users, f.err
}

type fakeReadinessFields struct {
	byEntityID map[string][]metadata.FieldDefinition
	err        error
}

func (f *fakeReadinessFields) ListFields(_ context.Context, _, entityID string) ([]metadata.FieldDefinition, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byEntityID[entityID], nil
}

func oneField(name string) []metadata.FieldDefinition {
	return []metadata.FieldDefinition{{Name: name}}
}

func TestAppService_Readiness_NotWired(t *testing.T) {
	svc, store := newAppSvcFixture(nil)
	store.nav["app-1"] = []appmeta.NavItem{{TenantID: "tenant-a", AppID: "app-1", ItemType: "entity", RefID: "ent-accounts"}}

	view, err := svc.Readiness(context.Background(), "tenant-a", store.nav["app-1"])
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view != nil {
		t.Fatalf("view = %+v; want nil (WithReadiness never called)", view)
	}
}

func TestAppService_Readiness(t *testing.T) {
	staffRole := "role-staff"

	tests := []struct {
		name        string
		items       []appmeta.NavItem
		fields      *fakeReadinessFields
		roles       *fakeReadinessRoles
		users       *fakeReadinessUsers
		wantReady   bool
		wantCount   int
		wantAllFld  bool
		wantRole    bool
		wantUser    bool
		wantMissing []string
	}{
		{
			name:      "app rong (0 entity trong nav) -> KHONG duoc vacuously true",
			items:     nil,
			fields:    &fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{}},
			roles:     &fakeReadinessRoles{roleIDs: []string{staffRole}}, // dumb fake: "yes" regardless
			users:     &fakeReadinessUsers{users: []auth.User{{ID: "u1", RoleID: &staffRole, Status: statusActive}}},
			wantReady: false, wantCount: 0, wantAllFld: false, wantRole: true, wantUser: true,
			wantMissing: []string{},
		},
		{
			name: "mot entity chua co field nao -> allEntitiesHaveFields=false",
			items: []appmeta.NavItem{
				{ItemType: "entity", RefID: "ent-accounts"},
				{ItemType: "entity", RefID: "ent-contacts"},
			},
			fields: &fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{
				"ent-accounts": oneField("name"),
				"ent-contacts": {}, // no fields
			}},
			roles:     &fakeReadinessRoles{roleIDs: []string{staffRole}},
			users:     &fakeReadinessUsers{users: []auth.User{{ID: "u1", RoleID: &staffRole, Status: statusActive}}},
			wantReady: false, wantCount: 2, wantAllFld: false, wantRole: true, wantUser: true,
			wantMissing: []string{"contacts"},
		},
		{
			name:  "du field nhung khong vai nao cap view -> hasNonAdminViewRole=false",
			items: []appmeta.NavItem{{ItemType: "entity", RefID: "ent-accounts"}},
			fields: &fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{
				"ent-accounts": oneField("name"),
			}},
			roles:     &fakeReadinessRoles{roleIDs: nil},
			users:     &fakeReadinessUsers{users: []auth.User{{ID: "u1", RoleID: &staffRole, Status: statusActive}}},
			wantReady: false, wantCount: 1, wantAllFld: true, wantRole: false, wantUser: false,
			wantMissing: []string{},
		},
		{
			name:  "co vai nhung khong user khong-admin nao mang vai do -> hasNonAdminUserAssigned=false",
			items: []appmeta.NavItem{{ItemType: "entity", RefID: "ent-accounts"}},
			fields: &fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{
				"ent-accounts": oneField("name"),
			}},
			roles: &fakeReadinessRoles{roleIDs: []string{staffRole}},
			users: &fakeReadinessUsers{users: []auth.User{
				{ID: "admin-1", IsAdmin: true, Status: statusActive},         // admin doesn't count
				{ID: "u2", RoleID: nil, Status: statusActive},                // no role
				{ID: "u3", RoleID: strp("other-role"), Status: statusActive}, // wrong role
				{ID: "u4", RoleID: &staffRole, Status: "disabled"},           // disabled — doesn't count
			}},
			wantReady: false, wantCount: 1, wantAllFld: true, wantRole: true, wantUser: false,
			wantMissing: []string{},
		},
		{
			name:  "chu khach mang vai cua app KHONG tinh la nhan vien (specs/065) -> hasNonAdminUserAssigned=false",
			items: []appmeta.NavItem{{ItemType: "entity", RefID: "ent-accounts"}},
			fields: &fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{
				"ent-accounts": oneField("name"),
			}},
			roles: &fakeReadinessRoles{roleIDs: []string{staffRole}},
			users: &fakeReadinessUsers{users: []auth.User{
				{ID: "owner-1", CanManageUsers: true, RoleID: &staffRole, Status: statusActive},
			}},
			wantReady: false, wantCount: 1, wantAllFld: true, wantRole: true, wantUser: false,
			wantMissing: []string{},
		},
		{
			name: "du ca ba -> ready=true",
			items: []appmeta.NavItem{
				{ItemType: "entity", RefID: "ent-accounts"},
				{ItemType: "dashboard", RefID: "dash-1"}, // non-entity item — must be ignored, not counted
				{ItemType: "entity", RefID: "ent-ghost"}, // orphan ref — must be skipped, not counted/errored
			},
			fields: &fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{
				"ent-accounts": oneField("name"),
			}},
			roles:     &fakeReadinessRoles{roleIDs: []string{staffRole}},
			users:     &fakeReadinessUsers{users: []auth.User{{ID: "u1", RoleID: &staffRole, Status: statusActive}}},
			wantReady: true, wantCount: 1, wantAllFld: true, wantRole: true, wantUser: true,
			wantMissing: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newAppSvcFixture(nil)
			svc.WithReadiness(tt.roles, tt.users, tt.fields)

			view, err := svc.Readiness(context.Background(), "tenant-a", tt.items)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if view.Ready != tt.wantReady {
				t.Errorf("Ready = %v; want %v (view=%+v)", view.Ready, tt.wantReady, view)
			}
			if view.EntityCount != tt.wantCount {
				t.Errorf("EntityCount = %d; want %d", view.EntityCount, tt.wantCount)
			}
			if view.AllEntitiesHaveFields != tt.wantAllFld {
				t.Errorf("AllEntitiesHaveFields = %v; want %v", view.AllEntitiesHaveFields, tt.wantAllFld)
			}
			if view.HasNonAdminViewRole != tt.wantRole {
				t.Errorf("HasNonAdminViewRole = %v; want %v", view.HasNonAdminViewRole, tt.wantRole)
			}
			if view.HasNonAdminUserAssigned != tt.wantUser {
				t.Errorf("HasNonAdminUserAssigned = %v; want %v", view.HasNonAdminUserAssigned, tt.wantUser)
			}
			if view.EntitiesMissingFields == nil {
				t.Error("EntitiesMissingFields must never be nil (JSON [] not null)")
			}
			if len(view.EntitiesMissingFields) != len(tt.wantMissing) {
				t.Errorf("EntitiesMissingFields = %v; want %v", view.EntitiesMissingFields, tt.wantMissing)
			}
		})
	}
}

func TestAppService_Readiness_PropagatesReadErrors(t *testing.T) {
	items := []appmeta.NavItem{{ItemType: "entity", RefID: "ent-accounts"}}

	t.Run("field lookup loi", func(t *testing.T) {
		svc, _ := newAppSvcFixture(nil)
		svc.WithReadiness(&fakeReadinessRoles{}, &fakeReadinessUsers{}, &fakeReadinessFields{err: errors.New("db down")})
		if _, err := svc.Readiness(context.Background(), "tenant-a", items); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("role lookup loi", func(t *testing.T) {
		svc, _ := newAppSvcFixture(nil)
		svc.WithReadiness(&fakeReadinessRoles{err: errors.New("db down")}, &fakeReadinessUsers{},
			&fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{"ent-accounts": oneField("name")}})
		if _, err := svc.Readiness(context.Background(), "tenant-a", items); err == nil {
			t.Fatal("want error, got nil")
		}
	})

	t.Run("user lookup loi", func(t *testing.T) {
		staffRole := "role-staff"
		svc, _ := newAppSvcFixture(nil)
		svc.WithReadiness(&fakeReadinessRoles{roleIDs: []string{staffRole}}, &fakeReadinessUsers{err: errors.New("db down")},
			&fakeReadinessFields{byEntityID: map[string][]metadata.FieldDefinition{"ent-accounts": oneField("name")}})
		if _, err := svc.Readiness(context.Background(), "tenant-a", items); err == nil {
			t.Fatal("want error, got nil")
		}
	})
}
