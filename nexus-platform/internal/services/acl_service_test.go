package services

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/auth"
)

// fakeACLLoader is an in-memory aclActionLoader returning a programmed grant set (or error) so
// ACLService.Authorize / IsAllowed can be exercised without a database.
type fakeACLLoader struct {
	grants []auth.RoleAction
	err    error
}

func (f *fakeACLLoader) LoadActionsForACL(_ context.Context, _, _ string) ([]auth.RoleAction, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.grants, nil
}

// TestAuthorize_ReturnsAccessLevel proves Authorize returns the matching grant's access_level so
// the A5 row filter can use it.
func TestAuthorize_ReturnsAccessLevel(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{grants: []auth.RoleAction{
		{EntityName: "contacts", Action: "view", AccessLevel: "team"},
	}})
	allowed, level, err := svc.Authorize(context.Background(), "t1", "r1", "contacts", "view")
	if err != nil || !allowed || level != "team" {
		t.Fatalf("Authorize = (%v,%q,%v); want (true,team,nil)", allowed, level, err)
	}
}

// TestAuthorize_ExactWinsOverWildcard proves an exact-entity grant's access_level overrides a
// '*' wildcard grant for the same action.
func TestAuthorize_ExactWinsOverWildcard(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{grants: []auth.RoleAction{
		{EntityName: "*", Action: "view", AccessLevel: "all"},
		{EntityName: "contacts", Action: "view", AccessLevel: "owner"},
	}})
	allowed, level, err := svc.Authorize(context.Background(), "t1", "r1", "contacts", "view")
	if err != nil || !allowed || level != "owner" {
		t.Fatalf("Authorize = (%v,%q,%v); want (true,owner,nil) — exact must win", allowed, level, err)
	}
}

// TestAuthorize_WildcardFallback proves the wildcard grant's level applies when no exact grant
// matches the entity.
func TestAuthorize_WildcardFallback(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{grants: []auth.RoleAction{
		{EntityName: "*", Action: "view", AccessLevel: "team"},
	}})
	allowed, level, err := svc.Authorize(context.Background(), "t1", "r1", "deals", "view")
	if err != nil || !allowed || level != "team" {
		t.Fatalf("Authorize = (%v,%q,%v); want (true,team,nil)", allowed, level, err)
	}
}

// TestAuthorize_DefaultDeny proves no matching grant denies with an empty level.
func TestAuthorize_DefaultDeny(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{grants: []auth.RoleAction{
		{EntityName: "contacts", Action: "edit", AccessLevel: "all"},
	}})
	allowed, level, err := svc.Authorize(context.Background(), "t1", "r1", "contacts", "view")
	if err != nil || allowed || level != "" {
		t.Fatalf("Authorize = (%v,%q,%v); want (false,\"\",nil)", allowed, level, err)
	}
}

// TestAuthorize_EmptyRoleDenies proves a principal with no role is denied without a DB hit.
func TestAuthorize_EmptyRoleDenies(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{err: errors.New("should not be called")})
	allowed, level, err := svc.Authorize(context.Background(), "t1", "", "contacts", "view")
	if err != nil || allowed || level != "" {
		t.Fatalf("Authorize(no role) = (%v,%q,%v); want (false,\"\",nil)", allowed, level, err)
	}
}

// TestAuthorize_LoaderErrorFailsClosed proves a repository error denies (fail closed) and
// surfaces the wrapped error.
func TestAuthorize_LoaderErrorFailsClosed(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{err: errors.New("db down")})
	allowed, _, err := svc.Authorize(context.Background(), "t1", "r1", "contacts", "view")
	if err == nil || allowed {
		t.Fatalf("Authorize(loader error) = (%v,_,%v); want (false,_,error)", allowed, err)
	}
}

// TestIsAllowed_DelegatesToAuthorize proves the boolean shim returns the same allow decision.
func TestIsAllowed_DelegatesToAuthorize(t *testing.T) {
	svc := NewACLService(&fakeACLLoader{grants: []auth.RoleAction{
		{EntityName: "contacts", Action: "view", AccessLevel: "team"},
	}})
	allowed, err := svc.IsAllowed(context.Background(), "t1", "r1", "contacts", "view")
	if err != nil || !allowed {
		t.Fatalf("IsAllowed = (%v,%v); want (true,nil)", allowed, err)
	}
}
