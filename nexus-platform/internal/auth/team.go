package auth

import "time"

// Team is a tenant-scoped team (spec ../specs/001-auth-security/spec.md §A5; table `teams`, migration
// 000006_create_team_rls_a5). A team groups users (via team_memberships) so the A5 row-level
// filter can grant access_level=team viewers visibility of records owned by anyone on a team
// they share. parent_id gives an optional self-referential hierarchy (no hard FK — a team may
// be reparented or seeded out of order). Team names are NOT globally unique; uniqueness is
// enforced per tenant by the repository (a duplicate (tenant_id, name) → 409). This is a fixed
// system table → GORM-managed (rules/database.md) and always carries tenant_id.
type Team struct {
	ID        string    `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID  string    `gorm:"type:char(36);index:idx_teams_tenant" json:"tenantId"`
	Name      string    `gorm:"type:varchar(150)" json:"name"`
	ParentID  *string   `gorm:"type:char(36);index:idx_teams_parent" json:"parentId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TableName pins the fixed system table.
func (Team) TableName() string { return "teams" }

// TeamMembership is one user<->team join row (spec ../specs/001-auth-security/spec.md §A5; table
// team_memberships). It is the source of truth for "who is on a team", consumed both by the A5
// team_set resolution at record-insert time (dynrepo.listUserTeamIDs) and by the row-level team
// filter. UNIQUE(user_id, team_id) makes a user a member of a team at most once; both FKs cascade
// so removing a user or a team cleans up its memberships.
//
// NOTE: team_memberships carries NO tenant_id column — tenant isolation is enforced at the
// repository layer, which verifies BOTH the team and the user belong to the caller's tenant
// before inserting a membership (a cross-tenant pairing is rejected, never written).
type TeamMembership struct {
	ID        string    `gorm:"type:char(36);primaryKey" json:"id"`
	UserID    string    `gorm:"type:char(36);uniqueIndex:uq_tm_user_team" json:"userId"`
	TeamID    string    `gorm:"type:char(36);uniqueIndex:uq_tm_user_team;index:idx_tm_team" json:"teamId"`
	CreatedAt time.Time `json:"createdAt"`
}

// TableName pins the fixed system table.
func (TeamMembership) TableName() string { return "team_memberships" }
