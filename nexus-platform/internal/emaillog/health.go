package emaillog

import (
	"context"
	"fmt"
	"time"
)

// FailedSummary is the health screen's mail tally: failed rows of any source in the last 7 days,
// how many of them fall in the last 24 hours, and how many are still resendable.
type FailedSummary struct {
	Failed7d       int `db:"failed7d"`
	Failed24h      int `db:"failed24h"`
	UnresentFailed int `db:"unresent"`
}

// FailedSummarySince counts failed rows since since7d. Only source='workflow' rows can count as
// unresent: no other source can be resent (specs/066 L6).
func (r *Repository) FailedSummarySince(ctx context.Context, tenantID string, since7d, since24h time.Time) (FailedSummary, error) {
	var s FailedSummary
	err := r.db.GetContext(ctx, &s, `
		SELECT COUNT(*) AS failed7d,
		       CAST(COALESCE(SUM(created_at >= ?), 0) AS SIGNED) AS failed24h,
		       CAST(COALESCE(SUM(source = 'workflow' AND resent_at IS NULL), 0) AS SIGNED) AS unresent
		FROM sys_email_log
		WHERE tenant_id = ? AND created_at >= ? AND status = 'failed'`, since24h, tenantID, since7d)
	if err != nil {
		return FailedSummary{}, fmt.Errorf("emaillog.Repository.FailedSummarySince: %w", err)
	}
	return s, nil
}
