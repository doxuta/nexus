package workflow

import (
	"context"
	"fmt"
	"maps"
	"strings"
)

// Recipient spec forms an in_app_notification's userId may take (queue #281). Everything is still
// ONE string in Action.UserID, so rules stored before the picker existed ("$owner", a fixed user id)
// keep their meaning and no column or migration is involved.
//
//	$owner         whoever owns the record NOW (after any assign earlier in the same firing)
//	$field:<name>  the user held by the record's user field <name>
//	$role:<id>     every active user holding the role
//	$team:<id>     every active member of the team
//	<uuid>         that one user
const (
	RecipientFieldPrefix = "$field:"
	RecipientRolePrefix  = "$role:"
	RecipientTeamPrefix  = "$team:"
)

// RecipientResolver expands a role / team into its ACTIVE users of one tenant. Declared here so the
// workflow package keeps no database dependency; services.WorkflowRecipientResolver implements it.
type RecipientResolver interface {
	RoleMembers(ctx context.Context, tenantID, roleID string) ([]string, error)
	TeamMembers(ctx context.Context, tenantID, teamID string) ([]string, error)
}

// ValidateRecipientSpec checks the SHAPE of a recipient (config time). Existence of the field /
// role / team is the service's job — it has the metadata and the tenant.
func ValidateRecipientSpec(spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return fmt.Errorf("workflow.ValidateRecipientSpec: recipient is empty")
	}
	for _, p := range []string{RecipientFieldPrefix, RecipientRolePrefix, RecipientTeamPrefix} {
		if strings.HasPrefix(spec, p) {
			if strings.TrimSpace(strings.TrimPrefix(spec, p)) == "" {
				return fmt.Errorf("workflow.ValidateRecipientSpec: %q needs a value after the prefix", p)
			}
			return nil
		}
	}
	if strings.HasPrefix(spec, "$") && spec != OwnerSentinel {
		return fmt.Errorf("workflow.ValidateRecipientSpec: unknown recipient %q", spec)
	}
	return nil
}

// ResolveRecipients turns a recipient spec into the distinct user ids to notify, in a stable order.
// An empty result is not an error here (a user field left blank on this record); the caller names
// it. A role / team with no resolver wired IS an error — a silent empty list would read as "sent".
func ResolveRecipients(ctx context.Context, evt Event, spec string, r RecipientResolver) ([]string, error) {
	spec = strings.TrimSpace(spec)
	var ids []string
	switch {
	case spec == OwnerSentinel:
		ids = []string{evt.OwnerUserID}
	case strings.HasPrefix(spec, RecipientFieldPrefix):
		name := strings.TrimSpace(strings.TrimPrefix(spec, RecipientFieldPrefix))
		if v, ok := evt.Data[name]; ok && v != nil {
			ids = []string{toString(v)}
		}
	case strings.HasPrefix(spec, RecipientRolePrefix), strings.HasPrefix(spec, RecipientTeamPrefix):
		if r == nil {
			return nil, fmt.Errorf("workflow.ResolveRecipients: no recipient resolver wired")
		}
		var err error
		if strings.HasPrefix(spec, RecipientRolePrefix) {
			ids, err = r.RoleMembers(ctx, evt.TenantID, strings.TrimSpace(strings.TrimPrefix(spec, RecipientRolePrefix)))
		} else {
			ids, err = r.TeamMembers(ctx, evt.TenantID, strings.TrimSpace(strings.TrimPrefix(spec, RecipientTeamPrefix)))
		}
		if err != nil {
			return nil, fmt.Errorf("workflow.ResolveRecipients: %w", err)
		}
	default:
		ids = []string{spec}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, nil
}

// withAssignee is evt as the actions AFTER an assign must see it: the new owner, and the assign's
// target field holding the assignee (so "{{field}}" and "$field:" read the post-assign record).
// evt.Data is shared read-only across the rules of one event, so the change lands in a copy.
func withAssignee(evt Event, field, userID string) Event {
	evt.OwnerUserID = userID
	data := make(map[string]any, len(evt.Data)+1)
	maps.Copy(data, evt.Data)
	data[field] = userID
	evt.Data = data
	return evt
}

// RenderedNotification is an in_app_notification's title and message with {{field}} filled.
type RenderedNotification struct{ Title, Message string }

// RenderNotification is the ONE render path of the live action and the dry-run preview.
func RenderNotification(ctx context.Context, evt Event, a Action, resolver TemplateResolver) (RenderedNotification, error) {
	data, err := RenderData(ctx, evt, resolver, a.Title, a.Message)
	if err != nil {
		return RenderedNotification{}, fmt.Errorf("workflow.RenderNotification: %w", err)
	}
	return RenderedNotification{Title: renderTemplate(a.Title, data), Message: renderTemplate(a.Message, data)}, nil
}
