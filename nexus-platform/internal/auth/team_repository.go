package auth

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrTeamNotFound is returned when no teams row matches a (tenant_id, id) pair (unknown or
// cross-tenant id). The team service maps it to a 404 TEAM_NOT_FOUND.
var ErrTeamNotFound = errors.New("auth: team not found")

// ErrTeamAlreadyExists is returned when creating/renaming a team would collide with an existing
// (tenant_id, name) team. The team service maps it to a 409 TEAM_ALREADY_EXISTS. Uniqueness is
// enforced in the repository (the teams table has no DB unique key on name), inside the same
// transaction as the write, so two concurrent creates cannot both win.
var ErrTeamAlreadyExists = errors.New("auth: team already exists")

// ErrTeamParentCycle is returned by UpdateTeam when the requested parent_id is the team itself or
// any of its descendants — linking would close a loop in the hierarchy (queue #21). The team
// service maps it to a 422 TEAM_PARENT_CYCLE with a readable reason.
var ErrTeamParentCycle = errors.New("auth: parent assignment would create a cycle")

// ErrMemberUserNotFound is returned by AddMember when the user id does not resolve to a live
// (non-soft-deleted) user IN THE CALLER'S TENANT. It is a tenant-isolation guard: a user from
// another tenant can never be added to this tenant's team. The service maps it to a 404.
var ErrMemberUserNotFound = errors.New("auth: member user not found in tenant")

// ErrMembershipNotFound is returned by RemoveMember when no (team, user) membership exists for a
// tenant-scoped team. The service maps it to a 404 MEMBERSHIP_NOT_FOUND.
var ErrMembershipNotFound = errors.New("auth: membership not found")

// TeamRepository persists and reads the A5 team tables (teams, team_memberships) over GORM. Every
// query is parameterized and pinned to tenant_id: a team from tenant A can never be read or mutated
// under tenant B. team_memberships has no tenant_id column, so the repository enforces isolation by
// confirming BOTH the team and the user belong to the caller's tenant before writing a membership
// (R2/R3 forge-tenant guard lives here in the data layer). It is GORM-managed because teams are a
// fixed system table (rules/database.md).
type TeamRepository struct {
	db *gorm.DB
}

// NewTeamRepository wires a team repository over an existing GORM connection.
func NewTeamRepository(db *gorm.DB) *TeamRepository {
	return &TeamRepository{db: db}
}

// ListTeams returns every team in the tenant ordered by name for a stable page. Always pinned to
// tenant_id so it can never return another tenant's teams.
func (r *TeamRepository) ListTeams(ctx context.Context, tenantID string) ([]Team, error) {
	var teams []Team
	err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("name ASC").
		Find(&teams).Error
	if err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.ListTeams: %w", err)
	}
	return teams, nil
}

// GetTeamByID returns the (tenant_id, id) team or ErrTeamNotFound. Pinned to tenant_id so a
// guessed/cross-tenant id misses.
func (r *TeamRepository) GetTeamByID(ctx context.Context, tenantID, id string) (*Team, error) {
	var team Team
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&team).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTeamNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.GetTeamByID: %w", err)
	}
	return &team, nil
}

// CreateTeam inserts a team. team must already carry a server-derived ID and TenantID. The name is
// unique within the tenant: the check + insert run in ONE transaction so two concurrent creates of
// the same name cannot both succeed (the second sees the first's row → ErrTeamAlreadyExists). When
// ParentID is set it is verified to point at a live team in the SAME tenant (else ErrTeamNotFound),
// so a hierarchy can never cross the tenant boundary.
func (r *TeamRepository) CreateTeam(ctx context.Context, team *Team) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&Team{}).
			Where("tenant_id = ? AND name = ?", team.TenantID, team.Name).
			Count(&n).Error; err != nil {
			return fmt.Errorf("dup-check: %w", err)
		}
		if n > 0 {
			return ErrTeamAlreadyExists
		}
		if team.ParentID != nil {
			if err := r.assertTeamInTenant(tx, team.TenantID, *team.ParentID); err != nil {
				return err
			}
		}
		if err := tx.Create(team).Error; err != nil {
			return fmt.Errorf("insert team: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTeamAlreadyExists) || errors.Is(err, ErrTeamNotFound) {
			return err
		}
		return fmt.Errorf("auth.TeamRepository.CreateTeam: %w", err)
	}
	return nil
}

// UpdateTeam applies the non-empty columns in fields to the (tenant_id, id) team and returns the
// refreshed row, all in ONE transaction. The WHERE is pinned to (tenant_id, id); an unknown/
// cross-tenant id is ErrTeamNotFound. A rename that collides with another team in the tenant is
// ErrTeamAlreadyExists. A parent_id in fields is verified to be a live team in the same tenant and
// not to close a loop (itself or any descendant → ErrTeamParentCycle) before the update.
func (r *TeamRepository) UpdateTeam(ctx context.Context, tenantID, id string, fields map[string]any) (*Team, error) {
	var updated *Team
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Team
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTeamNotFound
			}
			return fmt.Errorf("load team: %w", err)
		}

		if name, ok := fields["name"].(string); ok {
			var n int64
			if err := tx.Model(&Team{}).
				Where("tenant_id = ? AND name = ? AND id <> ?", tenantID, name, id).
				Count(&n).Error; err != nil {
				return fmt.Errorf("dup-check: %w", err)
			}
			if n > 0 {
				return ErrTeamAlreadyExists
			}
		}
		if pid, ok := fields["parent_id"]; ok {
			if s, isStr := pid.(string); isStr && s != "" {
				if err := r.assertTeamInTenant(tx, tenantID, s); err != nil {
					return err
				}
				if err := r.assertNoParentCycle(tx, tenantID, id, s); err != nil {
					return err
				}
			}
		}

		if len(fields) > 0 {
			if err := tx.Model(&Team{}).
				Where("tenant_id = ? AND id = ?", tenantID, id).
				Updates(fields).Error; err != nil {
				return fmt.Errorf("update team: %w", err)
			}
		}

		var refreshed Team
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).First(&refreshed).Error; err != nil {
			return fmt.Errorf("reload team: %w", err)
		}
		updated = &refreshed
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTeamNotFound) || errors.Is(err, ErrTeamAlreadyExists) || errors.Is(err, ErrTeamParentCycle) {
			return nil, err
		}
		return nil, fmt.Errorf("auth.TeamRepository.UpdateTeam: %w", err)
	}
	return updated, nil
}

// DeleteTeam hard-deletes the (tenant_id, id) team. The team_memberships FK (ON DELETE CASCADE)
// drops its memberships and team_sets_teams FK (ON DELETE CASCADE) drops its set compositions
// atomically. teams.parent_id has NO FK, so in the same transaction the deleted team's children are
// re-homed to its parent (NULL for a root): a dangling parent_id would silently cut the subtree out
// of every ancestor's downward-inherited row scope (dynrepo/teamtree.go). An unknown/cross-tenant
// id is ErrTeamNotFound.
func (r *TeamRepository) DeleteTeam(ctx context.Context, tenantID, id string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Team
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).First(&existing).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrTeamNotFound
			}
			return fmt.Errorf("load team: %w", err)
		}
		if err := tx.Model(&Team{}).
			Where("tenant_id = ? AND parent_id = ?", tenantID, id).
			Update("parent_id", existing.ParentID).Error; err != nil {
			return fmt.Errorf("re-home children: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&Team{}).Error; err != nil {
			return fmt.Errorf("delete team: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTeamNotFound) {
			return err
		}
		return fmt.Errorf("auth.TeamRepository.DeleteTeam: %w", err)
	}
	return nil
}

// ListMembers returns the users on a tenant-scoped team (joined to users for the tenant pin and to
// exclude soft-deleted accounts), ordered by email for a stable view. The team is first verified to
// belong to the tenant (ErrTeamNotFound otherwise) so membership rows never leak via a guessed id.
func (r *TeamRepository) ListMembers(ctx context.Context, tenantID, teamID string) ([]User, error) {
	if _, err := r.GetTeamByID(ctx, tenantID, teamID); err != nil {
		return nil, err
	}
	var users []User
	err := r.db.WithContext(ctx).
		Model(&User{}).
		Joins("JOIN team_memberships tm ON tm.user_id = users.id").
		Where("tm.team_id = ? AND users.tenant_id = ? AND users.deleted_at IS NULL", teamID, tenantID).
		Order("users.email ASC").
		Find(&users).Error
	if err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.ListMembers: %w", err)
	}
	return users, nil
}

// AddMember adds userID to teamID, both of which MUST belong to the caller's tenant. It runs in a
// transaction that (1) confirms the team is in the tenant (ErrTeamNotFound), (2) confirms the user
// is a live user in the SAME tenant (ErrMemberUserNotFound — the cross-tenant guard), then (3)
// inserts the membership. A re-add of an existing membership is idempotent (the UNIQUE(user_id,
// team_id) duplicate is swallowed) so the operation is safe to retry. Returns the membership id
// (existing or new).
func (r *TeamRepository) AddMember(ctx context.Context, tenantID, teamID, userID, membershipID string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := r.assertTeamInTenant(tx, tenantID, teamID); err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&User{}).
			Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, userID).
			Count(&n).Error; err != nil {
			return fmt.Errorf("verify user: %w", err)
		}
		if n == 0 {
			return ErrMemberUserNotFound
		}

		m := TeamMembership{ID: membershipID, UserID: userID, TeamID: teamID}
		if err := tx.Create(&m).Error; err != nil {
			if isDuplicateEntry(err) {
				return nil // already a member → idempotent success
			}
			return fmt.Errorf("insert membership: %w", err)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTeamNotFound) || errors.Is(err, ErrMemberUserNotFound) {
			return err
		}
		return fmt.Errorf("auth.TeamRepository.AddMember: %w", err)
	}
	return nil
}

// RemoveMember removes userID from teamID. The team is first verified to belong to the tenant
// (ErrTeamNotFound). If no such membership exists, ErrMembershipNotFound is returned (so the API
// can answer 404 rather than a silent 200). The delete is keyed on (team_id, user_id).
func (r *TeamRepository) RemoveMember(ctx context.Context, tenantID, teamID, userID string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := r.assertTeamInTenant(tx, tenantID, teamID); err != nil {
			return err
		}
		res := tx.Where("team_id = ? AND user_id = ?", teamID, userID).Delete(&TeamMembership{})
		if res.Error != nil {
			return fmt.Errorf("delete membership: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return ErrMembershipNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrTeamNotFound) || errors.Is(err, ErrMembershipNotFound) {
			return err
		}
		return fmt.Errorf("auth.TeamRepository.RemoveMember: %w", err)
	}
	return nil
}

// assertNoParentCycle returns ErrTeamParentCycle when making parentID the parent of teamID would
// close a loop: parentID is teamID itself, or teamID already sits somewhere above parentID
// (queue #21). It walks the ancestor chain of parentID from ONE tenant-pinned read of
// (id, parent_id) — no recursive SQL — with a visited set, so a loop already present in the data
// (parent_id has no hard FK) terminates instead of hanging; such a pre-existing loop that never
// reaches teamID is not this write's fault and is not rejected here. Runs inside the update tx as
// a LOCKING read (FOR UPDATE over the tenant's team rows — tens of rows, admin-only, rare) so two
// concurrent reparents in opposite directions (A→B and B→A) serialize: the second reads the
// first's committed parent and is refused, instead of both passing on a stale snapshot and
// committing a loop.
func (r *TeamRepository) assertNoParentCycle(tx *gorm.DB, tenantID, teamID, parentID string) error {
	var rows []struct {
		ID       string
		ParentID *string
	}
	if err := tx.Model(&Team{}).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "parent_id").
		Where("tenant_id = ?", tenantID).
		Find(&rows).Error; err != nil {
		return fmt.Errorf("load tree: %w", err)
	}
	parent := make(map[string]string, len(rows))
	for _, row := range rows {
		if row.ParentID != nil {
			parent[row.ID] = *row.ParentID
		}
	}
	seen := map[string]bool{}
	for cur := parentID; cur != "" && !seen[cur]; cur = parent[cur] {
		if cur == teamID {
			return ErrTeamParentCycle
		}
		seen[cur] = true
	}
	return nil
}

// assertTeamInTenant returns ErrTeamNotFound unless a team with (tenant_id, id) exists. Used as a
// guard before membership writes / parent links so a team from another tenant is never referenced.
func (r *TeamRepository) assertTeamInTenant(tx *gorm.DB, tenantID, teamID string) error {
	var n int64
	if err := tx.Model(&Team{}).
		Where("tenant_id = ? AND id = ?", tenantID, teamID).
		Count(&n).Error; err != nil {
		return fmt.Errorf("verify team: %w", err)
	}
	if n == 0 {
		return ErrTeamNotFound
	}
	return nil
}
