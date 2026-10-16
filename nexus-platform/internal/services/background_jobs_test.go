package services

// BackgroundJobService.Retry ("Gửi lại", DAILY-QUEUE #32) against a fake store: which refusals
// exist, that none of them touches the row or writes an audit row, and that the one success
// records WHO pressed and what the row looked like before.

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/jobs"
)

// fakeBgJobStore is an in-memory tenant-facing jobs store.
type fakeBgJobStore struct {
	rows       map[string]jobs.TenantJob // id → row (tenant in tenants)
	tenants    map[string]string         // id → tenant
	getErr     error
	requeueErr error
	lostRace   bool // RequeueFinished finds the row already changed
	requeued   []string
}

func (f *fakeBgJobStore) ListForTenant(context.Context, string, jobs.TenantJobFilter) ([]jobs.TenantJob, string, error) {
	return nil, "", nil
}

func (f *fakeBgJobStore) GetForTenant(_ context.Context, tenantID, id string) (*jobs.TenantJob, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.rows[id]
	if !ok || f.tenants[id] != tenantID {
		return nil, jobs.ErrJobNotFound
	}
	return &r, nil
}

func (f *fakeBgJobStore) RequeueFinished(_ context.Context, tenantID, id, from string, _ time.Time) (bool, error) {
	if f.requeueErr != nil {
		return false, f.requeueErr
	}
	if f.lostRace || f.tenants[id] != tenantID || f.rows[id].Status != from {
		return false, nil
	}
	f.requeued = append(f.requeued, id)
	return true, nil
}

func TestBackgroundJobServiceRetry(t *testing.T) {
	lastErr := "services.OutboundWebhookJobHandler: workflow.HTTPWebhookSender.Deliver: status 422"
	finished := time.Date(2026, 9, 30, 8, 0, 0, 0, time.UTC)
	newStore := func() *fakeBgJobStore {
		return &fakeBgJobStore{
			rows: map[string]jobs.TenantJob{
				"job-failed": {ID: "job-failed", Type: "outbound_webhook", Status: jobs.StatusFailed, Attempts: 10,
					MaxAttempts: 10, LastError: &lastErr, FinishedAt: &finished},
				"job-done":       {ID: "job-done", Type: "outbound_webhook", Status: jobs.StatusDone, Attempts: 1, MaxAttempts: 10},
				"job-other":      {ID: "job-other", Type: "outbound_webhook", Status: jobs.StatusFailed},
				"job-queued":     {ID: "job-queued", Type: "outbound_webhook", Status: jobs.StatusQueued, MaxAttempts: 10},
				"job-timer-done": {ID: "job-timer-done", Type: WorkflowTimerJobType, Status: jobs.StatusDone, Attempts: 1, MaxAttempts: 5},
				"job-done-err": {ID: "job-done-err", Type: "outbound_webhook", Status: jobs.StatusDone, Attempts: 4,
					MaxAttempts: 10, LastError: &lastErr, FinishedAt: &finished},
				"job-timer": {ID: "job-timer", Type: WorkflowTimerJobType, Status: jobs.StatusFailed, Attempts: 5,
					MaxAttempts: 5, LastError: &lastErr, FinishedAt: &finished},
			},
			tenants: map[string]string{"job-failed": "t1", "job-done": "t1", "job-other": "t2", "job-timer": "t1",
				"job-queued": "t1", "job-timer-done": "t1", "job-done-err": "t1"},
		}
	}
	cases := []struct {
		name     string
		id       string
		queueOff bool
		setup    func(*fakeBgJobStore)
		code     string // ValidationError code; "" = success or a plain error
		plainErr bool
	}{
		{name: "failed job of the tenant is resent", id: "job-failed"},
		{name: "queue switched off: nothing would run it", id: "job-failed", queueOff: true, code: "JOBS_DISABLED"},
		{name: "another tenant's job is not found", id: "job-other", code: "JOB_NOT_FOUND"},
		{name: "unknown id is not found", id: "nope", code: "JOB_NOT_FOUND"},
		// #256: a webhook the receiver answered 200 and then lost is resendable — the delivery id lets
		// it deduplicate. Not a timer (re-fires every action of the rule), not a queued/running row.
		{name: "a delivered webhook is resent", id: "job-done"},
		{name: "a delivered webhook that had retried is resent", id: "job-done-err"},
		{name: "a delivered workflow_timer is not resent", id: "job-timer-done", code: "JOB_NOT_RETRYABLE"},
		{name: "a queued webhook is not resent", id: "job-queued", code: "JOB_NOT_RETRYABLE"},
		// A failed timer may already have a re-parked twin that fired (the dedup probe skips failed
		// rows); resending it would run every action of the rule twice.
		{name: "a failed workflow_timer is not resent", id: "job-timer", code: "JOB_NOT_RETRYABLE"},
		{name: "a concurrent press already requeued it", id: "job-failed", code: "JOB_NOT_RETRYABLE",
			setup: func(f *fakeBgJobStore) { f.lostRace = true }},
		{name: "store read error", id: "job-failed", plainErr: true,
			setup: func(f *fakeBgJobStore) { f.getErr = errors.New("db down") }},
		{name: "store write error", id: "job-failed", plainErr: true,
			setup: func(f *fakeBgJobStore) { f.requeueErr = errors.New("db down") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore()
			if tc.setup != nil {
				tc.setup(store)
			}
			spy := &spyAuditor{}
			svc := NewBackgroundJobService(store).WithAuditor(spy).WithQueueEnabled(!tc.queueOff)
			job, err := svc.Retry(context.Background(), "t1", "u-admin", tc.id)

			success := tc.code == "" && !tc.plainErr
			if !success {
				var ve *ValidationError
				if tc.code != "" && (!errors.As(err, &ve) || ve.Code != tc.code) {
					t.Fatalf("err = %v; want ValidationError %s", err, tc.code)
				}
				if tc.plainErr && (err == nil || errors.As(err, &ve)) {
					t.Fatalf("err = %v; want a wrapped store error", err)
				}
				if len(spy.events) != 0 {
					t.Fatalf("a refused resend wrote %d audit rows", len(spy.events))
				}
				if tc.name != "store write error" && tc.name != "a concurrent press already requeued it" && len(store.requeued) != 0 {
					t.Fatalf("a refused resend requeued %v", store.requeued)
				}
				return
			}
			if err != nil {
				t.Fatalf("Retry: %v", err)
			}
			if len(store.requeued) != 1 || store.requeued[0] != tc.id {
				t.Fatalf("requeued %v; want exactly %s", store.requeued, tc.id)
			}
			if job.ID != tc.id || job.Status != jobs.StatusQueued || job.Attempts != 0 || job.MaxAttempts != 10 ||
				job.LastError != nil || job.FinishedAt != nil {
				t.Fatalf("returned job = %+v; want the same row, queued, 0/10, no error/finish", job)
			}
			if len(spy.events) != 1 {
				t.Fatalf("audit events = %d; want 1", len(spy.events))
			}
			ev := spy.events[0]
			if ev.TenantID != "t1" || ev.ActorUserID != "u-admin" || ev.EntityName != audit.JobMeta ||
				ev.RecordID != tc.id || ev.Action != audit.ActionUpdate || ev.Diff == nil {
				t.Fatalf("audit event = %+v; want t1/u-admin/%s/job-failed/update", ev, audit.JobMeta)
			}
			wantBefore := store.rows[tc.id]
			var wantErr any
			if wantBefore.LastError != nil {
				wantErr = *wantBefore.LastError
			}
			if ev.Diff.Before["status"] != wantBefore.Status || ev.Diff.Before["attempts"] != wantBefore.Attempts ||
				ev.Diff.Before["lastError"] != wantErr || ev.Diff.After["status"] != jobs.StatusQueued ||
				ev.Diff.After["attempts"] != 0 {
				t.Fatalf("audit diff = %+v; want the %s state (%d attempts) -> queued/0", ev.Diff, wantBefore.Status, wantBefore.Attempts)
			}
		})
	}
}
