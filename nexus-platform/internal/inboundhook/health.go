package inboundhook

import (
	"context"
	"fmt"
	"time"
)

// HookFailure is one inbound hook's failed-delivery tally. Name only: the delivery's error and
// payload_head are never selected (specs/066 FR-062).
type HookFailure struct {
	HookID string
	Name   string
	Count  int
	LastAt time.Time
}

// FailedByHookSince counts the tenant's error deliveries per hook since `since`, riding
// idx_hookdel_tenant_created; the name comes from the tenant's own hook row.
func (r *Repository) FailedByHookSince(ctx context.Context, tenantID string, since time.Time) ([]HookFailure, error) {
	var rows []HookFailure
	err := r.db.WithContext(ctx).Raw(`
		SELECT f.hook_id AS hook_id, COALESCE(h.name, '') AS name, f.n AS count, f.last_at AS last_at
		FROM (SELECT hook_id, COUNT(*) AS n, MAX(created_at) AS last_at
		        FROM sys_hook_deliveries
		        WHERE tenant_id = ? AND created_at >= ? AND status = 'error'
		        GROUP BY hook_id) f
		LEFT JOIN sys_inbound_hooks h ON h.id = f.hook_id AND h.tenant_id = ?
		ORDER BY f.n DESC, f.last_at DESC`, tenantID, since, tenantID).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("inboundhook.FailedByHookSince: %w", err)
	}
	return rows, nil
}
