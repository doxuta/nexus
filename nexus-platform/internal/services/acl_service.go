package services

import (
	"context"
	"fmt"

	"nexus/platform/internal/auth"
)

// wildcardEntity is the entity_name sentinel in acl_role_actions that matches EVERY entity
// (spec ../specs/001-auth-security/spec.md §A4: "'*' = mọi entity"). A grant of ('*', action) lets
// the role perform that action on any entity.
const wildcardEntity = "*"

// aclActionLoader is the subset of the role repository the ACL service needs: loading a role's
// (entity, action) grants for the per-request decision, pinned to the request's tenant. It is
// declared consumer-side so the service is unit-testable with an in-memory fake and so services
// does not import a concrete DB type. The implementation (auth.RoleRepository.LoadActionsForACL)
// pins the query to tenant_id via a JOIN to acl_roles, so a role id from another tenant returns
// zero grants → default-deny (D4 forge-tenant guard is enforced in the data layer, not here).
type aclActionLoader interface {
	LoadActionsForACL(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error)
}

// ACLService is the per-action authorization decider (spec §A4 line 126, "ACL trước logic" §138).
// Given the verified (tenant_id, role_id) from the access-token claims and a target
// (entity_name, action), it answers allow/deny against acl_role_actions with a strict
// DEFAULT-DENY policy: a request is allowed ONLY when the role owns a matching grant — either an
// exact (entity, action) row or a wildcard ('*', action) row. Anything else — no role, no grants,
// no matching rule, or a repository error — denies. is_admin BYPASS (D3) is NOT handled here; it
// is the middleware's job (an admin never reaches IsAllowed). The service holds no per-request
// state and is safe for concurrent use.
type ACLService struct {
	repo aclActionLoader
}

// NewACLService wires the ACL decider over a role-action loader.
func NewACLService(repo aclActionLoader) *ACLService {
	return &ACLService{repo: repo}
}

// Authorize reports whether the role identified by roleID (within tenantID) may perform action
// on entity AND, when allowed, the row-level access_level of the matching grant (none/owner/
// team/all — consumed by the A5 row filter). It loads the role's grants (tenant-pinned) and
// returns allowed=true iff some grant matches BOTH the action AND the entity (exact name or the
// '*' wildcard). DEFAULT DENY: an empty role id, a role with no grants, no matching rule, or a
// cross-tenant role all yield (false, "", nil). A repository failure FAILS CLOSED:
// (false, "", error) — the caller must refuse, never allow on error.
//
// Grant precedence: an EXACT entity grant wins over a '*' wildcard grant for the same action,
// so a per-entity access_level overrides the role's catch-all level. When only a wildcard grant
// matches, its access_level applies.
func (s *ACLService) Authorize(ctx context.Context, tenantID, roleID, entity, action string) (bool, string, error) {
	// No role on the principal → nothing can grant access. (is_admin bypass is handled upstream
	// in the middleware; a non-admin with no role is simply denied everything.)
	if roleID == "" {
		return false, "", nil
	}

	grants, err := s.repo.LoadActionsForACL(ctx, tenantID, roleID)
	if err != nil {
		// Fail closed: surface the error so the middleware can return 403/500, but the decision
		// itself is "not allowed". An infrastructure fault must never open access.
		return false, "", fmt.Errorf("services.ACLService.Authorize: %w", err)
	}

	var wildcardLevel string
	var wildcardMatched bool
	for _, g := range grants {
		if g.Action != action {
			continue
		}
		if g.EntityName == entity {
			// Exact entity grant wins immediately — its access_level is authoritative.
			return true, g.AccessLevel, nil
		}
		if g.EntityName == wildcardEntity {
			// Remember the wildcard but keep scanning for a more-specific exact grant.
			wildcardLevel = g.AccessLevel
			wildcardMatched = true
		}
	}
	if wildcardMatched {
		return true, wildcardLevel, nil
	}
	// No grant matched → default deny (spec §138).
	return false, "", nil
}

// IsAllowed is the boolean form of Authorize kept for the coarse allow/deny callers that do not
// need the row-level access_level (e.g. existing A4 tests). It is implemented on top of
// Authorize so the decision logic lives in exactly one place.
func (s *ACLService) IsAllowed(ctx context.Context, tenantID, roleID, entity, action string) (bool, error) {
	allowed, _, err := s.Authorize(ctx, tenantID, roleID, entity, action)
	return allowed, err
}

// CanViewEntity reports whether the principal identified by (tenantID, roleID) may perform
// the "view" action on entityName. Implements services.TargetACLChecker.
// Admins always return (true, nil) — this check is only ever called for non-admin callers
// (the RelationService skips it when isAdmin=true), but we defend in depth anyway.
func (s *ACLService) CanViewEntity(ctx context.Context, tenantID, roleID, entityName string) (bool, error) {
	allowed, _, err := s.Authorize(ctx, tenantID, roleID, entityName, "view")
	if err != nil {
		return false, fmt.Errorf("services.ACLService.CanViewEntity: %w", err)
	}
	return allowed, nil
}
