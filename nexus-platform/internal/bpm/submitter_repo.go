package bpm

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// LiveUser reports whether userID is an active, not-deleted user of THIS tenant — what a returned
// record needs of its submitter (an API key's principal or a user who left is not one). Pinned to
// the tenant like every statement here.
func (r *SQLRepository) LiveUser(ctx context.Context, tenantID, userID string) (bool, error) {
	if strings.TrimSpace(userID) == "" {
		return false, nil
	}
	const q = `SELECT 1 FROM users WHERE tenant_id = ? AND id = ? AND status = 'active' AND deleted_at IS NULL LIMIT 1`
	var one int
	if err := r.db.GetContext(ctx, &one, q, tenantID, userID); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("bpm.SQLRepository.LiveUser: %w", err)
	}
	return true, nil
}
