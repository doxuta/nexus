package shares

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// RecordGuard is the slice of *services.RecordService the share subsystem needs: re-resolve the
// caller's EDIT grant and read the record under that edit scope (Q4a — whoever may edit the record
// may share it). Consumer-side so Service is testable with a fake — mirrors notes.RecordGuard.
type RecordGuard interface {
	ResolveShareTarget(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string) (canonicalEntity string, err error)
}

// UserExister reports which ids are LIVE ACTIVE users of the tenant. Satisfied by
// *dynrepo.Repository (the same probe the `user` field type uses).
type UserExister interface {
	BatchActiveUserExists(ctx context.Context, tenantID string, ids []string) (map[string]bool, error)
}

// TeamExister reports whether a team is a live team of the tenant (auth.ErrTeamNotFound when not).
// Satisfied by *auth.TeamRepository (tenant-pinned GetTeamByID) — the same slice the workflow
// ASSIGN-1 check uses.
type TeamExister interface {
	GetTeamByID(ctx context.Context, tenantID, id string) (*auth.Team, error)
}

// store is the persistence the service drives; *Repository in production.
type store interface {
	Upsert(ctx context.Context, id, tenantID, entity, recordID, userID, teamID, sharedBy string) (sh *Share, changed bool, err error)
	ListByRecord(ctx context.Context, tenantID, entity, recordID string) ([]Share, error)
	Revoke(ctx context.Context, tenantID, entity, recordID, granteeID string) (team bool, err error)
}

// Service is the manual-share business logic (specs/051-record-manual-share). Every method runs the
// record gate FIRST (ACL before logic), then validates, then touches sys_record_shares. It knows
// nothing about HTTP.
type Service struct {
	repo    store
	guard   RecordGuard
	users   UserExister
	teams   TeamExister
	auditor audit.Auditor
	newID   func() string
}

// NewService wires the share service. auditor may be nil (no trail — tests only).
func NewService(repo *Repository, guard RecordGuard, users UserExister, auditor audit.Auditor) *Service {
	if auditor == nil {
		auditor = audit.NewNoopAuditor()
	}
	return &Service{repo: repo, guard: guard, users: users, auditor: auditor, newID: uuid.NewString}
}

// List returns who the record is shared with. Only a caller who may EDIT the record may see it (the
// same gate as sharing): the list names colleagues, which a read-only viewer does not need.
func (s *Service) List(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID string) ([]Share, error) {
	canonical, err := s.guard.ResolveShareTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	rows, err := s.repo.ListByRecord(ctx, tenantID, canonical, recordID)
	if err != nil {
		return nil, fmt.Errorf("shares.Service.List: %w", err)
	}
	return rows, nil
}

// Create shares the record with userID (read-only). sharedBy is ALWAYS the verified caller. The
// grantee must be a live active user of the SAME tenant and not the caller.
func (s *Service) Create(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, userID string) (*Share, error) {
	canonical, err := s.guard.ResolveShareTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, shareUserInvalid()
	}
	if userID == scope.UserID {
		return nil, &services.ValidationError{Code: "RECORD_SHARE_SELF", Msg: "Không cần chia sẻ hồ sơ cho chính mình", Details: map[string]any{}}
	}
	if s.users == nil {
		return nil, fmt.Errorf("shares.Service.Create: user probe not wired")
	}
	found, err := s.users.BatchActiveUserExists(ctx, tenantID, []string{userID})
	if err != nil {
		return nil, fmt.Errorf("shares.Service.Create: %w", err)
	}
	if !found[userID] {
		return nil, shareUserInvalid()
	}
	return s.grant(ctx, scope, tenantID, canonical, recordID, userID, "")
}

// CreateTeam shares the record read-only with a whole TEAM (DAILY-QUEUE #224): its members — and the
// members of any team above it, by the same closure the team access level uses — see the record.
// Same gate as Create; the team must be a live team of the SAME tenant (the tenant-pinned lookup
// folds a guessed or foreign id into the same 422 as an unknown one).
func (s *Service) CreateTeam(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, teamID string) (*Share, error) {
	canonical, err := s.guard.ResolveShareTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return nil, err
	}
	teamID = strings.TrimSpace(teamID)
	if s.teams == nil {
		return nil, fmt.Errorf("shares.Service.CreateTeam: team probe not wired")
	}
	if teamID != "" {
		if _, err := s.teams.GetTeamByID(ctx, tenantID, teamID); err != nil {
			if !errors.Is(err, auth.ErrTeamNotFound) {
				return nil, fmt.Errorf("shares.Service.CreateTeam: %w", err)
			}
			teamID = ""
		}
	}
	if teamID == "" {
		return nil, &services.ValidationError{
			Code: "RECORD_SHARE_TEAM_INVALID", Msg: "Nhóm được chia sẻ phải là một nhóm có trong tổ chức",
			Details: map[string]any{"field": "teamId"},
		}
	}
	return s.grant(ctx, scope, tenantID, canonical, recordID, "", teamID)
}

// grant stores the share (one of userID / teamID) and audits it when it changed the table.
func (s *Service) grant(ctx context.Context, scope services.RecordScope, tenantID, canonical, recordID, userID, teamID string) (*Share, error) {
	sh, changed, err := s.repo.Upsert(ctx, s.newID(), tenantID, canonical, recordID, userID, teamID, scope.UserID)
	if err != nil {
		return nil, fmt.Errorf("shares.Service.grant: %w", err)
	}
	if !changed { // already shared with them: nothing happened, so nothing to put in the History
		return sh, nil
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID: tenantID, ActorUserID: scope.UserID, EntityName: canonical, RecordID: recordID,
		Action: audit.ActionShare, Diff: &audit.Diff{After: shareDiff(userID, teamID)}, At: time.Now().UTC(),
	})
	return sh, nil
}

// shareDiff is the History payload: {"sharedWith": userId} for a person (unchanged since #34) or
// {"sharedWithTeam": teamId} for a team.
func shareDiff(userID, teamID string) map[string]any {
	if teamID != "" {
		return map[string]any{"sharedWithTeam": teamID}
	}
	return map[string]any{"sharedWith": userID}
}

// WithTeams wires the team probe CreateTeam needs. Chainable; without it CreateTeam fails loudly.
func (s *Service) WithTeams(t TeamExister) *Service {
	s.teams = t
	return s
}

// Delete revokes the share of the record with granteeID — a person's or a team's id. Anyone who may EDIT the record may revoke
// (Q4a: the record must not get stuck when the person who shared it is away).
func (s *Service) Delete(ctx context.Context, scope services.RecordScope, tenantID, entityName, recordID, granteeID string) error {
	canonical, err := s.guard.ResolveShareTarget(ctx, scope, tenantID, entityName, recordID)
	if err != nil {
		return err
	}
	granteeID = strings.TrimSpace(granteeID)
	team, err := s.repo.Revoke(ctx, tenantID, canonical, recordID, granteeID)
	if err != nil {
		return fmt.Errorf("shares.Service.Delete: %w", err)
	}
	before := shareDiff(granteeID, "")
	if team {
		before = shareDiff("", granteeID)
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID: tenantID, ActorUserID: scope.UserID, EntityName: canonical, RecordID: recordID,
		Action: audit.ActionUnshare, Diff: &audit.Diff{Before: before}, At: time.Now().UTC(),
	})
	return nil
}

func shareUserInvalid() error {
	return &services.ValidationError{
		Code: "RECORD_SHARE_USER_INVALID", Msg: "Người được chia sẻ phải là một người dùng đang hoạt động trong tổ chức",
		Details: map[string]any{"field": "userId"},
	}
}
