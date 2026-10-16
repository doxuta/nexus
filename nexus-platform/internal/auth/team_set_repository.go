package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// ErrTeamSetNotFound is returned when no team_sets row matches a (tenant_id, id) pair (unknown
// id or a cross-tenant probe — indistinguishable by design).
var ErrTeamSetNotFound = errors.New("auth: team set not found")

// TeamSet is one team_sets row (A5 row-level security): the deduplicated set of teams a
// record's team_set_id points at. Sets are ENGINE-derived (resolved/created by the record
// write path from the creator's memberships — dynrepo/teamset.go), so the W7 admin surface is
// READ-ONLY: the FE sharing/teams screens need to display "which teams see this record" and
// "which sets exist"; mutating sets stays with the engine (the future sharing-rule tier).
type TeamSet struct {
	ID            string    `gorm:"type:char(36);primaryKey" json:"id"`
	TenantID      string    `gorm:"type:char(36)" json:"-"`
	TeamMD5       string    `gorm:"column:team_md5" json:"-"`
	PrimaryTeamID *string   `gorm:"column:primary_team_id" json:"primaryTeamId"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"-"`
}

// TableName pins the fixed system table.
func (TeamSet) TableName() string { return "team_sets" }

// TeamSetTeamRef is one member team of a set as the read API serves it (id + display name).
type TeamSetTeamRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// TeamSetWithTeams is a team_sets row joined with its composition (team_sets_teams → teams).
type TeamSetWithTeams struct {
	TeamSet
	Teams []TeamSetTeamRef `json:"teams"`
}

// teamSetCompositionRow is the flat JOIN row ListTeamSets/GetTeamSetByID group by set id.
type teamSetCompositionRow struct {
	TeamSetID string `gorm:"column:team_set_id"`
	TeamID    string `gorm:"column:team_id"`
	TeamName  string `gorm:"column:team_name"`
}

// ListTeamSets returns every team set of the tenant with its member teams (W7 read API).
// Tenant pinning is double: team_sets.tenant_id on the set rows AND teams.tenant_id on the
// composition JOIN, so a poisoned composition row pointing at another tenant's team can never
// surface that team's name. Sets are ordered newest-first (stable for the admin list).
func (r *TeamRepository) ListTeamSets(ctx context.Context, tenantID string) ([]TeamSetWithTeams, error) {
	var sets []TeamSet
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ?", tenantID).
		Order("created_at DESC, id DESC").
		Find(&sets).Error; err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.ListTeamSets: %w", err)
	}
	if len(sets) == 0 {
		return []TeamSetWithTeams{}, nil
	}

	byID, err := r.loadTeamSetCompositions(ctx, tenantID, "")
	if err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.ListTeamSets: %w", err)
	}

	out := make([]TeamSetWithTeams, 0, len(sets))
	for _, s := range sets {
		teams := byID[s.ID]
		if teams == nil {
			teams = []TeamSetTeamRef{}
		}
		out = append(out, TeamSetWithTeams{TeamSet: s, Teams: teams})
	}
	return out, nil
}

// GetTeamSetByID returns one tenant-scoped team set with its member teams, or
// ErrTeamSetNotFound (unknown and cross-tenant ids are indistinguishable).
func (r *TeamRepository) GetTeamSetByID(ctx context.Context, tenantID, id string) (*TeamSetWithTeams, error) {
	var set TeamSet
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, id).
		First(&set).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTeamSetNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.GetTeamSetByID: %w", err)
	}

	byID, err := r.loadTeamSetCompositions(ctx, tenantID, id)
	if err != nil {
		return nil, fmt.Errorf("auth.TeamRepository.GetTeamSetByID: %w", err)
	}
	teams := byID[set.ID]
	if teams == nil {
		teams = []TeamSetTeamRef{}
	}
	return &TeamSetWithTeams{TeamSet: set, Teams: teams}, nil
}

// loadTeamSetCompositions loads the (set → member teams) map for the tenant in ONE JOIN read,
// optionally narrowed to a single set id. Both team_sets and teams are tenant-pinned.
func (r *TeamRepository) loadTeamSetCompositions(ctx context.Context, tenantID, setID string) (map[string][]TeamSetTeamRef, error) {
	q := r.db.WithContext(ctx).
		Table("team_sets_teams AS tst").
		Select("tst.team_set_id AS team_set_id, t.id AS team_id, t.name AS team_name").
		Joins("JOIN team_sets ts ON ts.id = tst.team_set_id").
		Joins("JOIN teams t ON t.id = tst.team_id").
		Where("ts.tenant_id = ? AND t.tenant_id = ?", tenantID, tenantID).
		Order("t.name ASC")
	if setID != "" {
		q = q.Where("tst.team_set_id = ?", setID)
	}
	var rows []teamSetCompositionRow
	if err := q.Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("load compositions: %w", err)
	}
	byID := make(map[string][]TeamSetTeamRef, len(rows))
	for _, row := range rows {
		byID[row.TeamSetID] = append(byID[row.TeamSetID], TeamSetTeamRef{ID: row.TeamID, Name: row.TeamName})
	}
	return byID, nil
}
