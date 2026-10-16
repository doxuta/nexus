package services

// "Việc nền" — the customer admin's view of THEIR background jobs (DAILY-QUEUE #31/#32,
// specs/021-durable-job-queue §3). Before this, the outcome of a webhook delivery lived on a sys_jobs
// row no endpoint could read: a customer asking "why didn't we get the order?" had no answer, and
// the workflow run history only records the ENQUEUE (and lives in Studio, which the customer never
// sees). The surface is mounted on the tenant-admin gate, not the operator one.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/jobs"
)

// backgroundJobStore is the consumer-side slice of the jobs store this surface needs. Satisfied by
// *jobs.SQLStore; every method pins tenant_id, so a system (tenant-NULL) job or another tenant's
// job is unreachable by construction.
type backgroundJobStore interface {
	ListForTenant(ctx context.Context, tenantID string, f jobs.TenantJobFilter) ([]jobs.TenantJob, string, error)
	GetForTenant(ctx context.Context, tenantID, id string) (*jobs.TenantJob, error)
	RequeueFinished(ctx context.Context, tenantID, id, from string, now time.Time) (bool, error)
}

// BackgroundJobService serves the tenant's background-job log and its "Gửi lại".
type BackgroundJobService struct {
	store   backgroundJobStore
	auditor audit.Auditor
	// queueOff mirrors JOBS_ENABLED=false: no worker runs, so a resend would sit queued forever.
	queueOff bool
	now      func() time.Time
}

// NewBackgroundJobService wires the service over the jobs store (queue assumed running, no audit).
func NewBackgroundJobService(store backgroundJobStore) *BackgroundJobService {
	return &BackgroundJobService{store: store, auditor: audit.NewNoopAuditor(), now: time.Now}
}

// WithAuditor injects the RM-19 audit dispatcher: every resend writes a __job__ row naming who
// pressed it. Chainable; nil is ignored.
func (s *BackgroundJobService) WithAuditor(a audit.Auditor) *BackgroundJobService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithQueueEnabled tells the service whether the queue's workers run (JOBS_ENABLED). Chainable.
func (s *BackgroundJobService) WithQueueEnabled(enabled bool) *BackgroundJobService {
	s.queueOff = !enabled
	return s
}

// List returns one page of the tenant's jobs, newest first, plus the next page's cursor ("" on the
// last page). A malformed cursor comes back wrapping dynrepo.ErrInvalidCursor.
func (s *BackgroundJobService) List(ctx context.Context, tenantID string, f jobs.TenantJobFilter) ([]jobs.TenantJob, string, error) {
	rows, next, err := s.store.ListForTenant(ctx, tenantID, f)
	if err != nil {
		return nil, "", fmt.Errorf("services.BackgroundJobService.List: %w", err)
	}
	return rows, next, nil
}

// resendableFrom is the resend policy per job type: which finished statuses "Gửi lại" may requeue.
// Default-deny — a type that is absent here is not resendable until someone decides it is safe.
//
// outbound_webhook: failed (#32) AND done (#256, the receiver answered 200 and then lost the order) —
// both safe by construction, the receiver deduplicates on X-Nexus-Delivery-Id (the row id, kept).
// workflow_timer: neither. A failed timer may already have a re-parked twin that fired (the enqueue
// dedup probe skips failed rows on purpose), and a done one re-runs EVERY action of the rule
// (notifications, escalations, create_record) — a repeating rule even re-arms a second chain.
// email_alert: no payload left once finished.
var resendableFrom = map[string][]string{
	OutboundWebhookJobType: {jobs.StatusFailed, jobs.StatusDone},
}

// Retry is "Gửi lại" (queue #32, #256): it sends one finished job of the tenant again by requeueing
// the SAME row (jobs.SQLStore.RequeueFinished — fresh attempt budget, same id, so a webhook
// re-delivers under the X-Nexus-Delivery-Id the receiver already saw and can deduplicate). Nothing is
// re-rendered: the job handler re-reads the rule at delivery time, so a URL the admin fixed
// meanwhile is used.
//
//	queue switched off (JOBS_ENABLED=false) → JOBS_DISABLED (503) — nothing would ever run it
//	not the tenant's job / unknown / system → JOB_NOT_FOUND (404) — same answer for all three
//	a type/status resendableFrom does not list (a timer, queued/running), a concurrent press won,
//	or nothing left to send                 → JOB_NOT_RETRYABLE (409)
//
// On success one __job__ audit row records the actor and the finished cycle it replaced.
func (s *BackgroundJobService) Retry(ctx context.Context, tenantID, actorID, id string) (*jobs.TenantJob, error) {
	if s.queueOff {
		return nil, invalid("JOBS_DISABLED", "Hàng đợi việc nền đang tắt (JOBS_ENABLED=false) — gửi lại sẽ không bao giờ chạy")
	}
	job, err := s.store.GetForTenant(ctx, tenantID, id)
	if errors.Is(err, jobs.ErrJobNotFound) {
		return nil, invalid("JOB_NOT_FOUND", "Không tìm thấy việc nền")
	}
	if err != nil {
		return nil, fmt.Errorf("services.BackgroundJobService.Retry: %w", err)
	}
	statuses, ok := resendableFrom[job.Type]
	if !ok {
		return nil, invalid("JOB_NOT_RETRYABLE", "Chỉ gửi lại được lần giao webhook — loại việc nền %s không gửi lại tay được", job.Type)
	}
	if !slices.Contains(statuses, job.Status) {
		return nil, invalid("JOB_NOT_RETRYABLE", "Chỉ gửi lại được việc nền đã lỗi hoặc đã giao xong (việc này đang ở trạng thái %s)", job.Status)
	}
	now := s.now()
	applied, err := s.store.RequeueFinished(ctx, tenantID, id, job.Status, now)
	if err != nil {
		return nil, fmt.Errorf("services.BackgroundJobService.Retry: %w", err)
	}
	if !applied {
		// Read-then-write race (another press, or the row changed) — or a payload already dropped.
		return nil, invalid("JOB_NOT_RETRYABLE", "Việc nền vừa đổi trạng thái hoặc không còn gì để gửi lại")
	}

	var lastError any
	if job.LastError != nil {
		lastError = *job.LastError
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID: tenantID, ActorUserID: actorID, EntityName: audit.JobMeta, RecordID: id,
		Action: audit.ActionUpdate, At: now,
		Diff: &audit.Diff{
			Before: map[string]any{"status": job.Status, "attempts": job.Attempts, "lastError": lastError},
			After:  map[string]any{"status": jobs.StatusQueued, "attempts": 0, "lastError": nil},
		},
	})

	requeued := *job
	requeued.Status, requeued.Attempts, requeued.RunAt = jobs.StatusQueued, 0, now.UTC()
	requeued.LastError, requeued.FinishedAt = nil, nil
	return &requeued, nil
}
