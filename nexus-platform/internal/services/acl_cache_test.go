package services

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

// countingACLLoader is a counting aclActionLoader. grants is keyed by tenant+"|"+role so the test can
// give two tenants DIFFERENT grants for the SAME role id and prove the cache never serves across.
type countingACLLoader struct {
	calls  int64
	grants map[string][]auth.RoleAction
}

func (f *countingACLLoader) LoadActionsForACL(_ context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	atomic.AddInt64(&f.calls, 1)
	return f.grants[tenantID+"|"+roleID], nil
}

func TestACLCache_HitAndInvalidate(t *testing.T) {
	inner := &countingACLLoader{grants: map[string][]auth.RoleAction{
		"t1|r1": {{EntityName: "accounts", Action: "view", AccessLevel: "all"}},
	}}
	cl := NewCachedACLLoader(inner, time.Minute)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := cl.LoadActionsForACL(ctx, "t1", "r1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt64(&inner.calls); got != 1 {
		t.Errorf("LoadActionsForACL hit DB %d times; want 1 (cached)", got)
	}

	cl.Invalidate("t1", "r1")
	if _, err := cl.LoadActionsForACL(ctx, "t1", "r1"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&inner.calls); got != 2 {
		t.Errorf("after Invalidate, hit DB %d times; want 2 (reload)", got)
	}
}

// TestACLCache_TenantScoped is the load-bearing security guarantee: the SAME role id under two
// tenants are distinct cache keys, so tenant A's grants are NEVER served to tenant B (which would
// bypass the forge-tenant default-deny guard).
func TestACLCache_TenantScoped(t *testing.T) {
	inner := &countingACLLoader{grants: map[string][]auth.RoleAction{
		"tenantA|shared-role": {{EntityName: "accounts", Action: "delete", AccessLevel: "all"}}, // A: powerful
		"tenantB|shared-role": {},                                                               // B: NO grants
	}}
	cl := NewCachedACLLoader(inner, time.Minute)
	acl := NewACLService(cl)
	ctx := context.Background()

	// Prime tenant A (has delete-all on accounts).
	if ok, _, _ := acl.Authorize(ctx, "tenantA", "shared-role", "accounts", "delete"); !ok {
		t.Fatal("tenantA should be allowed delete on accounts")
	}
	// Tenant B with the SAME role id must NOT inherit A's grant — default deny.
	ok, _, err := acl.Authorize(ctx, "tenantB", "shared-role", "accounts", "delete")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("CROSS-TENANT LEAK: tenantB authorized via tenantA's cached grants for the same role id")
	}
	// Both tenants caused a real load (distinct keys), and a re-check of A stays cached.
	if got := atomic.LoadInt64(&inner.calls); got != 2 {
		t.Errorf("loads = %d; want 2 (tenant-scoped, no cross-tenant hit)", got)
	}
}

// TestACLCache_RevocationImmediate proves a revoked grant takes effect on the NEXT authorize once
// the role write invalidates — not after the TTL. This is the security reason invalidation (not TTL)
// is the primary mechanism.
func TestACLCache_RevocationImmediate(t *testing.T) {
	grants := map[string][]auth.RoleAction{
		"t1|r1": {{EntityName: "accounts", Action: "edit", AccessLevel: "all"}},
	}
	inner := &countingACLLoader{grants: grants}
	cl := NewCachedACLLoader(inner, time.Hour) // long TTL: only invalidation can make revocation timely
	acl := NewACLService(cl)
	ctx := context.Background()

	// Granted + cached.
	if ok, _, _ := acl.Authorize(ctx, "t1", "r1", "accounts", "edit"); !ok {
		t.Fatal("should be allowed before revoke")
	}
	// Revoke the grant in the backing store, then invalidate (what RoleService.UpdateRole does).
	grants["t1|r1"] = []auth.RoleAction{}
	cl.Invalidate("t1", "r1")
	// Next authorize must DENY immediately (not wait for the 1h TTL).
	if ok, _, _ := acl.Authorize(ctx, "t1", "r1", "accounts", "edit"); ok {
		t.Fatal("REVOCATION LAG: still allowed after revoke+invalidate (stale cache)")
	}
}

// fakeRoleStore implements BOTH roleRepo (RoleService's write path) and aclActionLoader (the read
// path) over one in-memory grant store, so the wired test below proves RoleService.UpdateRole's
// invalidation hook actually fires end-to-end: a long-TTL cache, yet a revoke takes effect at once.
type fakeRoleStore struct {
	grants map[string][]auth.RoleAction // key: tenant|role
}

func key(t, r string) string { return t + "|" + r }

func (f *fakeRoleStore) CreateRole(_ context.Context, role *auth.Role, actions []auth.RoleAction) error {
	f.grants[key(role.TenantID, role.ID)] = actions
	return nil
}
func (f *fakeRoleStore) UpdateRole(_ context.Context, tenantID, id string, _ map[string]any, actions []auth.RoleAction) (*auth.Role, error) {
	if _, ok := f.grants[key(tenantID, id)]; !ok {
		return nil, auth.ErrRoleNotFound
	}
	if actions != nil { // REPLACE semantics (UpdateRoleInput.Actions non-nil)
		f.grants[key(tenantID, id)] = actions
	}
	return &auth.Role{ID: id, TenantID: tenantID, Name: "r"}, nil
}
func (f *fakeRoleStore) DeleteRole(_ context.Context, tenantID, id string) error {
	delete(f.grants, key(tenantID, id))
	return nil
}
func (f *fakeRoleStore) ListActionsForRole(_ context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	return f.grants[key(tenantID, roleID)], nil
}
func (f *fakeRoleStore) LoadActionsForACL(_ context.Context, tenantID, roleID string) ([]auth.RoleAction, error) {
	return f.grants[key(tenantID, roleID)], nil
}
func (f *fakeRoleStore) ListRoles(context.Context, string) ([]auth.Role, error) { return nil, nil }
func (f *fakeRoleStore) GetRoleByID(_ context.Context, tenantID, id string) (*auth.Role, error) {
	return &auth.Role{ID: id, TenantID: tenantID}, nil
}

// TestACLCache_WiredRevocationThroughRoleService is the end-to-end security proof: with the cache
// wired exactly as cmd/server does (ACLService reads through it; RoleService.WithACLCache), a grant
// REVOKED via the real RoleService.UpdateRole is denied on the next authorize — the invalidation
// hook fired, not the (1h) TTL.
func TestACLCache_WiredRevocationThroughRoleService(t *testing.T) {
	store := &fakeRoleStore{grants: map[string][]auth.RoleAction{}}
	cache := NewCachedACLLoader(store, time.Hour) // long TTL: only the hook can make revoke timely
	roleSvc := NewRoleService(store).WithACLCache(cache)
	acl := NewACLService(cache)
	ctx := context.Background()

	// Create a role granting delete-all on accounts.
	rv, err := roleSvc.CreateRole(ctx, "t1", CreateRoleInput{
		Name:    "ops",
		Actions: []RoleActionInput{{EntityName: "accounts", Action: "delete", AccessLevel: "all"}},
	})
	if err != nil {
		t.Fatalf("create role: %v", err)
	}
	// Authorize → allowed, and now cached.
	if ok, _, _ := acl.Authorize(ctx, "t1", rv.ID, "accounts", "delete"); !ok {
		t.Fatal("should be allowed after grant")
	}
	// REVOKE via the real service path: UpdateRole with an empty (non-nil) Actions clears grants.
	if _, err := roleSvc.UpdateRole(ctx, "t1", rv.ID, UpdateRoleInput{Actions: []RoleActionInput{}}); err != nil {
		t.Fatalf("revoke via UpdateRole: %v", err)
	}
	// Next authorize MUST deny — the hook invalidated the cached grant despite the 1h TTL.
	if ok, _, _ := acl.Authorize(ctx, "t1", rv.ID, "accounts", "delete"); ok {
		t.Fatal("REVOCATION LAG: still allowed after RoleService.UpdateRole revoke (hook did not fire)")
	}

	// DeleteRole must also invalidate: re-grant, authorize (cache), delete, authorize → deny.
	rv2, _ := roleSvc.CreateRole(ctx, "t1", CreateRoleInput{
		Name:    "ops2",
		Actions: []RoleActionInput{{EntityName: "accounts", Action: "view", AccessLevel: "all"}},
	})
	acl.Authorize(ctx, "t1", rv2.ID, "accounts", "view") // cache it
	if err := roleSvc.DeleteRole(ctx, "t1", rv2.ID); err != nil {
		t.Fatalf("delete role: %v", err)
	}
	if ok, _, _ := acl.Authorize(ctx, "t1", rv2.ID, "accounts", "view"); ok {
		t.Fatal("REVOCATION LAG: still allowed after RoleService.DeleteRole")
	}
}
