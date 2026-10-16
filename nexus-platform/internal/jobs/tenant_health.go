package jobs

import (
	"context"
	"fmt"
	"time"
)

// Health reads of the tenant-facing side of the queue (specs/066 L6). Like tenant_jobs.go, every
// statement pins tenant_id = ?, which can never reach a system job (tenant_id NULL).

// JobTypeFailure is one row of the health screen's "failed jobs by type": how many of the tenant's
// jobs of this type ended failed since a cut-off, and when the latest one was created.
type JobTypeFailure struct {
	JobType string    `db:"job_type" json:"jobType"`
	Count   int       `db:"n" json:"count"`
	LastAt  time.Time `db:"last_at" json:"lastAt"`
}

// FailedByTypeSince counts the tenant's failed jobs per type since `since`.
// idx_sys_jobs_tenant_list covers tenant_id, created_at, status and job_type: index-only.
func (s *SQLStore) FailedByTypeSince(ctx context.Context, tenantID string, since time.Time) ([]JobTypeFailure, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("jobs.SQLStore.FailedByTypeSince: tenantID is required")
	}
	var rows []JobTypeFailure
	err := s.db.SelectContext(ctx, &rows, `
		SELECT /*+ INDEX(sys_jobs idx_sys_jobs_tenant_list) */ job_type, COUNT(*) AS n, MAX(created_at) AS last_at
		FROM sys_jobs
		WHERE tenant_id = ? AND created_at >= ? AND status = 'failed'
		GROUP BY job_type
		ORDER BY n DESC, job_type`, tenantID, since)
	if err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.FailedByTypeSince: %w", err)
	}
	return rows, nil
}

// StuckQueuedForTenant counts the tenant's queued jobs that were due before `dueBefore` and are
// still waiting — a worker that is down or wedged. Rides idx_sys_jobs_claim (status, run_at).
func (s *SQLStore) StuckQueuedForTenant(ctx context.Context, tenantID string, dueBefore time.Time) (int, error) {
	if tenantID == "" {
		return 0, fmt.Errorf("jobs.SQLStore.StuckQueuedForTenant: tenantID is required")
	}
	var n int
	err := s.db.GetContext(ctx, &n, `
		SELECT /*+ INDEX(sys_jobs idx_sys_jobs_claim) */ COUNT(*)
		FROM sys_jobs
		WHERE status = 'queued' AND run_at < ? AND tenant_id = ?`, dueBefore, tenantID)
	if err != nil {
		return 0, fmt.Errorf("jobs.SQLStore.StuckQueuedForTenant: %w", err)
	}
	return n, nil
}
