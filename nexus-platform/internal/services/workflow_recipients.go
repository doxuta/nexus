package services

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// WorkflowRecipientResolver expands a notification's $role: / $team: recipient into the ACTIVE users
// of ONE tenant (queue #281). Both queries pin users.tenant_id, so a role or team id from another
// tenant — pasted, or left in a pack — resolves to nobody rather than to a stranger.
type WorkflowRecipientResolver struct{ db *sqlx.DB }

// NewWorkflowRecipientResolver wires the resolver over the shared sqlx connection.
func NewWorkflowRecipientResolver(db *sqlx.DB) *WorkflowRecipientResolver {
	return &WorkflowRecipientResolver{db: db}
}

const recipientRoleQuery = `SELECT id FROM users
	WHERE tenant_id = ? AND role_id = ? AND status = 'active' AND deleted_at IS NULL
	ORDER BY id`

// RoleMembers lists the active users holding roleID in tenantID.
func (r *WorkflowRecipientResolver) RoleMembers(ctx context.Context, tenantID, roleID string) ([]string, error) {
	var ids []string
	if err := r.db.SelectContext(ctx, &ids, recipientRoleQuery, tenantID, roleID); err != nil {
		return nil, fmt.Errorf("services.WorkflowRecipientResolver.RoleMembers: %w", err)
	}
	return ids, nil
}

// TeamMembers lists the active members of teamID in tenantID (the same eligibility as assign).
func (r *WorkflowRecipientResolver) TeamMembers(ctx context.Context, tenantID, teamID string) ([]string, error) {
	var ids []string
	if err := r.db.SelectContext(ctx, &ids, assignMemberQuery, teamID, tenantID); err != nil {
		return nil, fmt.Errorf("services.WorkflowRecipientResolver.TeamMembers: %w", err)
	}
	return ids, nil
}
