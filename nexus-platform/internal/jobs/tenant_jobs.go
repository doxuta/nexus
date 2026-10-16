package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/dynrepo"
)

// The tenant-facing side of the queue (DAILY-QUEUE #31/#32, specs/021-durable-job-queue §3): a
// customer's own admin reads the delivery log of THEIR background jobs (GET /admin/jobs) and sends
// a failed one again. The queue operations in store.go are deliberately tenant-blind (a system
// table); everything in this file is the opposite — every statement pins tenant_id = ?, and since
// system jobs carry tenant_id NULL an equality on a real tenant can never reach them.

// TenantJob is one row of a tenant's background-job listing: the operational columns of a sys_jobs
// row. The payload never leaves the store — a webhook body is record data and this is a delivery
// log, not a data export; only the rule the job came from is lifted out of it (workflowId, the
// top-level key every workflow-produced payload carries; null for any other job). last_error is
// shown as stored: producers write it already masked (no secret, no header, no URL userinfo —
// specs/050 D-10; a transport error drops the URL entirely).
type TenantJob struct {
	ID          string  `db:"id" json:"id"`
	Type        string  `db:"job_type" json:"jobType"`
	Status      string  `db:"status" json:"status"`
	Attempts    int     `db:"attempts" json:"attempts"`
	MaxAttempts int     `db:"max_attempts" json:"maxAttempts"`
	LastError   *string `db:"last_error" json:"lastError"`
	WorkflowID  *string `db:"workflow_id" json:"workflowId"`
	// CredentialName is the vault key a webhook job names in its payload (specs/050 S2b), by
	// NAME only — looked up inside the job's own tenant, live keys only; nil for a job with no key,
	// a deleted key, or a foreign id. The secret columns are never selected.
	CredentialName *string    `db:"credential_name" json:"credentialName"`
	RunAt          time.Time  `db:"run_at" json:"runAt"`
	CreatedAt      time.Time  `db:"created_at" json:"createdAt"`
	FinishedAt     *time.Time `db:"finished_at" json:"finishedAt"`
}

// TenantJobFilter narrows ListForTenant. Empty Type/Status = any; Limit is clamped by
// ClampListLimit; After is the opaque cursor of the previous page ("" = first page).
type TenantJobFilter struct {
	Type   string
	Status string
	Limit  int
	After  string
}

// Page size bounds of the listing (the audit-log's, one house rule).
const (
	defaultListLimit = 50
	maxListLimit     = 200
)

// ClampListLimit normalizes a requested page size to [1, 200], defaulting non-positive values to
// 50. Exported so the handler echoes the EFFECTIVE limit — the one ListForTenant applied.
func ClampListLimit(n int) int {
	if n <= 0 {
		return defaultListLimit
	}
	return min(n, maxListLimit)
}

// The listing's keyset is (created_at DESC, id DESC). The cursor is bound to a label no other
// surface mints, so a record-list or audit cursor replayed here is 400 INVALID_CURSOR instead of
// a silently wrong page; its boundary is created_at's wall text as MySQL returned it.
const (
	tenantListCursorSort = "sys_jobs.created_at"
	tenantListTimeLayout = "2006-01-02 15:04:05.000"
)

// ListForTenant returns one page of the tenant's jobs, newest first, plus the cursor of the next
// page ("" on the last). Keyset pagination, so every page costs the rows it walks — no OFFSET, no
// COUNT; migration 000075 gives the walk its index (TestJobsTenantListSeeks_Integration).
func (s *SQLStore) ListForTenant(ctx context.Context, tenantID string, f TenantJobFilter) ([]TenantJob, string, error) {
	query, args, err := tenantListSelect(tenantID, f)
	if err != nil {
		return nil, "", fmt.Errorf("jobs.SQLStore.ListForTenant: %w", err)
	}
	var rows []TenantJob
	if err := s.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, "", fmt.Errorf("jobs.SQLStore.ListForTenant: %w", err)
	}
	next := ""
	if limit := ClampListLimit(f.Limit); len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		// No zone conversion: the driver parsed the column's text in its own location, so Format
		// gives that text back — the literal the next page compares against in the same session.
		next = dynrepo.EncodeCursor(tenantListCursorSort, "DESC", false, last.CreatedAt.Format(tenantListTimeLayout), last.ID)
	}
	return rows, next, nil
}

// ErrJobNotFound: no job with this id in the caller's tenant — another tenant's job and a system
// job answer exactly the same, so the id of a job one cannot see stays unconfirmed.
var ErrJobNotFound = errors.New("job not found")

// GetForTenant returns one of the tenant's jobs (the listing's shape), or ErrJobNotFound.
func (s *SQLStore) GetForTenant(ctx context.Context, tenantID, id string) (*TenantJob, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("jobs.SQLStore.GetForTenant: tenantID is required")
	}
	var j TenantJob
	err := s.db.GetContext(ctx, &j, `
		SELECT id, job_type, status, attempts, max_attempts, last_error,
		       payload->>'$.workflowId' AS workflow_id, (SELECT c.name FROM sys_named_credentials c
		        WHERE c.id = payload->>'$.credentialId' AND c.tenant_id = sys_jobs.tenant_id AND c.deleted_at IS NULL) AS credential_name,
		       run_at, created_at, finished_at
		FROM sys_jobs
		WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrJobNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("jobs.SQLStore.GetForTenant: %w", err)
	}
	return &j, nil
}

// RequeueFinished is "Gửi lại" (queue #32, #256): it puts ONE finished job of the tenant — `from` is
// StatusFailed (the delivery dead-lettered) or StatusDone (it was delivered and the receiver lost
// it) — back in the queue and reports whether it did. It is the SAME row — never a copy — so an
// outbound_webhook keeps its id, which is the X-Nexus-Delivery-Id the receiver deduplicates on
// (#27), and its payload.
//
// The row gets a FRESH attempt budget: attempts = 0 under the unchanged max_attempts, run_at = now.
// Keeping attempts at the exhausted count would fail the job again on its very first error (the
// manager fails at attempts >= max_attempts) and back it off at the ladder's 1h cap; a resend is
// a new delivery cycle, so it walks the whole ladder again (a webhook: 10 attempts ≈ 3h). The old
// cycle's last_error/finished_at are cleared with it — the row describes the cycle in flight; the
// caller records the old state in the audit trail before calling.
//
// The UPDATE is its own guard: tenant_id = ? (the list-side filter cannot protect this path — any
// id can be posted), status = ? (the `from` the caller read: a queued/running row is never moved,
// a failed press cannot grab a done row, and a second press after the first one has requeued it
// finds nothing), payload IS NOT NULL (a ScrubPayloadOnFinish type has nothing left to send).
// applied=false covers all of them. A `from` other than failed/done is a caller bug and an error:
// no argument may make a row a worker may hold requeueable.
// Which job TYPES may be resent at all is the caller's policy, not the store's: a workflow_timer
// must not be (HasJobWithPayload skips failed rows, so its re-parked twin may already have fired;
// a done one would run every action of its rule again) — services.BackgroundJobService.Retry
// admits outbound_webhook only.
func (s *SQLStore) RequeueFinished(ctx context.Context, tenantID, id, from string, now time.Time) (bool, error) {
	if tenantID == "" {
		return false, fmt.Errorf("jobs.SQLStore.RequeueFinished: tenantID is required")
	}
	if from != StatusFailed && from != StatusDone {
		return false, fmt.Errorf("jobs.SQLStore.RequeueFinished: status %q is not a finished status", from)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE sys_jobs
		SET status = ?, run_at = ?, attempts = 0, last_error = NULL, finished_at = NULL,
		    claimed_at = NULL, claimed_by = NULL
		WHERE id = ? AND tenant_id = ? AND status = ? AND payload IS NOT NULL`,
		StatusQueued, now.UTC(), id, tenantID, from)
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.RequeueFinished: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("jobs.SQLStore.RequeueFinished: rows affected: %w", err)
	}
	return n > 0, nil
}

// tenantListSelect builds ListForTenant's statement. Only placeholders are added to the text; the
// tenant, the filters, the cursor boundary and the limit are all bound arguments. The fetch is
// limit+1 to learn whether another page exists without a COUNT.
//
// The optimizer hint pins idx_sys_jobs_tenant_list (000075): left alone, a status filter picks
// idx_sys_jobs_claim — the failed rows of every tenant, then a sort. A hint naming a missing index
// is a warning, not an error, so a schema without 000075 still lists (sorting) — FORCE INDEX would
// fail there.
func tenantListSelect(tenantID string, f TenantJobFilter) (string, []any, error) {
	if tenantID == "" {
		// Never "every job of no tenant": an empty tenant is a caller bug, not the system jobs.
		return "", nil, fmt.Errorf("tenantID is required")
	}
	where := "tenant_id = ?"
	args := []any{tenantID}
	if f.Type != "" {
		where += " AND job_type = ?"
		args = append(args, f.Type)
	}
	if f.Status != "" {
		where += " AND status = ?"
		args = append(args, f.Status)
	}
	if strings.TrimSpace(f.After) != "" {
		isNull, at, id, err := dynrepo.DecodeCursor(f.After, tenantListCursorSort, "DESC")
		if err != nil {
			return "", nil, err
		}
		// created_at is NOT NULL; a boundary that is not its text would coerce to a zero date
		// (MySQL warns, it does not fail) and answer an empty page — a sync loop's "done".
		if _, perr := time.Parse(tenantListTimeLayout, at); isNull || perr != nil {
			return "", nil, fmt.Errorf("cursor boundary %q is not a created_at: %w", at, dynrepo.ErrInvalidCursor)
		}
		where += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, at, at, id)
	}
	args = append(args, ClampListLimit(f.Limit)+1)
	return `
		SELECT /*+ INDEX(sys_jobs idx_sys_jobs_tenant_list) */
		       id, job_type, status, attempts, max_attempts, last_error,
		       payload->>'$.workflowId' AS workflow_id, (SELECT c.name FROM sys_named_credentials c
		        WHERE c.id = payload->>'$.credentialId' AND c.tenant_id = sys_jobs.tenant_id AND c.deleted_at IS NULL) AS credential_name,
		       run_at, created_at, finished_at
		FROM sys_jobs
		WHERE ` + where + `
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, args, nil
}
