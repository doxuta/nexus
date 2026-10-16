package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
)

// Routing (specs/043 S6a): the lookups behind a step that names its holder by rule. bpm owns WHEN a
// rule is resolved (once, as the instance enters the step) and what happens when nobody is found;
// this file owns HOW the two dynamic rules find a person — the user a record field points at, and
// the person who sent the approval — and which teams a person belongs to.

// teamDirectory answers which teams a user belongs to (bpm.SQLRepository.UserTeamIDs).
type teamDirectory interface {
	UserTeamIDs(ctx context.Context, tenantID, userID string) ([]string, error)
}

// teamIDsOf is the teams of the user, or nil. A directory that fails or is absent yields no team —
// fail closed: the person simply does not see the team pools until it answers (never a wider view).
func teamIDsOf(ctx context.Context, d teamDirectory, logger *zap.Logger, tenantID, userID string) []string {
	if d == nil || strings.TrimSpace(userID) == "" {
		return nil
	}
	ids, err := d.UserTeamIDs(ctx, tenantID, userID)
	if err != nil {
		if logger == nil {
			logger = zap.NewNop()
		}
		logger.Warn("approval: team membership unreadable — team pools left out (fail closed)",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.Error(err))
		return nil
	}
	return ids
}

// routingRecords reads one record WITHOUT a caller's row scope (dynrepo.Repository.GetRecord): the
// engine routes on behalf of the system, and what comes out is a user id, never record data.
type routingRecords interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// routingUsers tells whether a user id is a live, active user of the tenant (bpm.SQLRepository.UserActive).
type routingUsers interface {
	UserActive(ctx context.Context, tenantID, userID string) (bool, error)
}

// routingTeams deals a team out for a step that names a strategy (S6b): who is next in the ring,
// who has the fewest open seats, who the active members are. Satisfied by AssignmentService.
type routingTeams interface {
	NextAssignee(ctx context.Context, tenantID, workflowID, actionKey, teamID string) (string, bool, error)
	LeastLoadedSeatAssignee(ctx context.Context, tenantID, workflowID, actionKey, teamID string) (string, bool, error)
	ActiveMembers(ctx context.Context, tenantID, teamID string, limit int) ([]string, error)
}

// BPMRouter resolves field and submitter assignments and team strategies (bpm.Router).
type BPMRouter struct {
	records routingRecords
	users   routingUsers
	teams   routingTeams
	logger  *zap.Logger
}

// WithTeams wires the team strategies (round_robin / load_based / each_member). Chainable. Unwired,
// a team strategy finds nobody (fallback, else the step is stuck with a reason).
func (r *BPMRouter) WithTeams(t routingTeams) *BPMRouter {
	r.teams = t
	return r
}

// NewBPMRouter wires the router over the record store and the user directory.
func NewBPMRouter(records routingRecords, users routingUsers, logger *zap.Logger) *BPMRouter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &BPMRouter{records: records, users: users, logger: logger}
}

var _ bpm.Router = (*BPMRouter)(nil)

// Resolve finds the holder of a dynamic assignment. found=false when nobody can be named — the
// field is empty or not a user id, the person is gone / inactive / of another tenant, the record is
// deleted; err only when a store could not be read.
//
//	field      the user the record's field holds (owner_id = the record's current owner)
//	submitter  the person who sent the approval; when unknown (an API key, a rule) the record's owner
func (r *BPMRouter) Resolve(ctx context.Context, in bpm.RouteInput, a bpm.Assignment) (bpm.Approver, bool, error) {
	switch a.Type {
	case bpm.AssignField:
		id, err := r.fieldValue(ctx, in, a.Field)
		if err != nil {
			return bpm.Approver{}, false, err
		}
		return r.active(ctx, in.TenantID, id)
	case bpm.AssignSubmitter:
		if h, ok, err := r.active(ctx, in.TenantID, in.Submitter); err != nil || ok {
			return h, ok, err
		}
		id, err := r.fieldValue(ctx, in, bpm.OwnerField)
		if err != nil {
			return bpm.Approver{}, false, err
		}
		return r.active(ctx, in.TenantID, id)
	}
	return bpm.Approver{}, false, nil // user / role / team / queue are named, never looked up
}

// ResolveTeam deals a team out (bpm.Router): round_robin takes the next member in the ring and
// load_based the member with the fewest open seats — both through the cursor of THIS node
// (sys_assignment_cursors: workflow_id = the process, action_key = "bpm:"+node, shared with the
// workflow assign action's table) — each_member every active member, at most bpm.MaxApprovers.
// An empty team yields nobody; err only when a store could not be read.
func (r *BPMRouter) ResolveTeam(ctx context.Context, in bpm.RouteInput, a bpm.Assignment) ([]bpm.Approver, error) {
	if r.teams == nil || !a.TeamStrategy() {
		return nil, nil
	}
	if strings.TrimSpace(in.ProcessID) == "" || strings.TrimSpace(in.NodeID) == "" {
		return nil, fmt.Errorf("services.BPMRouter.ResolveTeam: no process / node to key the rotation by")
	}
	var ids []string
	switch a.Strategy {
	case bpm.StrategyEachMember:
		all, err := r.teams.ActiveMembers(ctx, in.TenantID, a.ID, bpm.MaxApprovers+1)
		if err != nil {
			return nil, fmt.Errorf("services.BPMRouter.ResolveTeam: members: %w", err)
		}
		if len(all) > bpm.MaxApprovers {
			r.logger.Warn("approval: team is larger than a council can be — only the first members are seated",
				zap.String("tenant_id", in.TenantID), zap.String("process_id", in.ProcessID),
				zap.String("node_id", in.NodeID), zap.String("team_id", a.ID), zap.Int("cap", bpm.MaxApprovers))
			all = all[:bpm.MaxApprovers]
		}
		ids = all
	case bpm.StrategyRoundRobin, bpm.StrategyLoadBased:
		pick := r.teams.NextAssignee
		if a.Strategy == bpm.StrategyLoadBased {
			pick = r.teams.LeastLoadedSeatAssignee
		}
		id, ok, err := pick(ctx, in.TenantID, in.ProcessID, "bpm:"+in.NodeID, a.ID)
		if err != nil {
			return nil, fmt.Errorf("services.BPMRouter.ResolveTeam: %s: %w", a.Strategy, err)
		}
		if ok {
			ids = []string{id}
		}
	}
	out := make([]bpm.Approver, len(ids))
	for i, id := range ids {
		out[i] = bpm.Approver{Type: bpm.ApproverUser, ID: id}
	}
	return out, nil
}

// fieldValue reads the user id a record field holds. The record is read tenant-pinned and unscoped
// (the system routes; the id it yields is checked against the tenant's users right after).
func (r *BPMRouter) fieldValue(ctx context.Context, in bpm.RouteInput, field string) (string, error) {
	if r.records == nil || strings.TrimSpace(in.RecordID) == "" {
		return "", nil
	}
	rec, err := r.records.GetRecord(ctx, in.EntityName, in.TenantID, in.RecordID, dynrepo.RowScope{Level: dynrepo.AccessAll, IsAdmin: true})
	if err != nil {
		if errors.Is(err, dynrepo.ErrRecordNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("services.BPMRouter.fieldValue: %w", err)
	}
	if field == bpm.OwnerField {
		return rec.OwnerID, nil
	}
	v, _ := rec.Data[field].(string)
	return strings.TrimSpace(v), nil
}

// active names the user as a holder only when they are a real, active user of THIS tenant: a field
// that still holds the id of someone who left must not park the step on a ghost.
func (r *BPMRouter) active(ctx context.Context, tenantID, userID string) (bpm.Approver, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" || r.users == nil {
		return bpm.Approver{}, false, nil
	}
	ok, err := r.users.UserActive(ctx, tenantID, userID)
	if err != nil {
		return bpm.Approver{}, false, fmt.Errorf("services.BPMRouter.active: %w", err)
	}
	if !ok {
		return bpm.Approver{}, false, nil
	}
	return bpm.Approver{Type: bpm.ApproverUser, ID: userID}, true, nil
}
