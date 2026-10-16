package metaversion

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// DeliveryRepo answers "when was each tenant delivered?" — the MIN(created_at) of its numbered
// versions (FR-027). It is the concrete reader a project registry plugs into its own
// DeliveryReader interface (declared on the consuming side, not here).
type DeliveryRepo struct{ db *sqlx.DB }

// NewDeliveryRepo wires the repo over the shared sqlx pool.
func NewDeliveryRepo(db *sqlx.DB) *DeliveryRepo { return &DeliveryRepo{db: db} }

// DeliveredAt returns, for the tenants in tenantIDs that were delivered, the instant of their first
// numbered version. A tenant never delivered is absent from the map. Only the listed tenants are read.
func (r *DeliveryRepo) DeliveredAt(ctx context.Context, tenantIDs []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(tenantIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`SELECT tenant_id, MIN(created_at) AS at FROM sys_meta_versions
		WHERE tenant_id IN (?) AND version_no IS NOT NULL AND deleted_at IS NULL GROUP BY tenant_id`, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("metaversion.DeliveryRepo.DeliveredAt: %w", err)
	}
	var rows []struct {
		TenantID string    `db:"tenant_id"`
		At       time.Time `db:"at"`
	}
	if err := r.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, fmt.Errorf("metaversion.DeliveryRepo.DeliveredAt: %w", err)
	}
	for _, row := range rows {
		out[row.TenantID] = row.At
	}
	return out, nil
}
