package janitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// batchResult scripts one DeleteBatched return.
type batchResult struct {
	n   int64
	err error
}

// tableScript records the calls to one table's delete and replays a scripted result sequence.
type tableScript struct {
	results []batchResult
	calls   int
	cutoff  time.Time
	limit   int
}

func (t *tableScript) next(cutoff time.Time, limit int) (int64, error) {
	t.cutoff = cutoff
	t.limit = limit
	if t.calls >= len(t.results) {
		return 0, nil // nothing left
	}
	r := t.results[t.calls]
	t.calls++
	return r.n, r.err
}

// fakeSweeper is a scripted Sweeper capturing the cutoff/limit each table received.
type fakeSweeper struct {
	runs, mfa, resets, handoffs, jobs, hookDeliveries, idemKeys, previewSessions, emailLog, usageDaily, staleRuns tableScript
}

func (f *fakeSweeper) FailStaleWorkflowRunsStartedBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.staleRuns.next(c, l)
}

func (f *fakeSweeper) DeleteUsageDailyBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.usageDaily.next(c, l)
}

func (f *fakeSweeper) DeleteEmailLogBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.emailLog.next(c, l)
}

func (f *fakeSweeper) DeleteWorkflowRunsFinishedBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.runs.next(c, l)
}
func (f *fakeSweeper) DeleteMFAPendingExpiredBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.mfa.next(c, l)
}
func (f *fakeSweeper) DeletePasswordResetExpiredBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.resets.next(c, l)
}
func (f *fakeSweeper) DeleteSSOHandoffExpiredBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.handoffs.next(c, l)
}
func (f *fakeSweeper) DeleteJobsFinishedBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.jobs.next(c, l)
}
func (f *fakeSweeper) DeleteHookDeliveriesBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.hookDeliveries.next(c, l)
}
func (f *fakeSweeper) DeleteIdempotencyKeysExpiredBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.idemKeys.next(c, l)
}
func (f *fakeSweeper) DeletePreviewSessionsExpiredBefore(_ context.Context, c time.Time, l int) (int64, error) {
	return f.previewSessions.next(c, l)
}

// TestDeleteBatched covers the batch loop: it must keep deleting until a batch returns fewer than
// batchSize rows, sum the totals, and stop early (returning the partial total) on an error.
func TestDeleteBatched(t *testing.T) {
	tests := []struct {
		name      string
		batchSize int
		results   []batchResult
		wantTotal int64
		wantCalls int
	}{
		{
			name:      "loops until short batch",
			batchSize: 3,
			results:   []batchResult{{n: 3}, {n: 3}, {n: 2}},
			wantTotal: 8,
			wantCalls: 3,
		},
		{
			name:      "single short batch stops immediately",
			batchSize: 1000,
			results:   []batchResult{{n: 2}},
			wantTotal: 2,
			wantCalls: 1,
		},
		{
			name:      "empty first batch",
			batchSize: 1000,
			results:   []batchResult{{n: 0}},
			wantTotal: 0,
			wantCalls: 1,
		},
		{
			name:      "error mid-loop returns partial total and stops",
			batchSize: 3,
			results:   []batchResult{{n: 3}, {n: 3}, {err: errors.New("db down")}},
			wantTotal: 6,
			wantCalls: 3,
		},
		{
			name:      "exactly-full then next empty (full batch is not the last)",
			batchSize: 3,
			results:   []batchResult{{n: 3}, {n: 0}},
			wantTotal: 3,
			wantCalls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := New(Config{Interval: time.Hour, RunsRetention: 90 * 24 * time.Hour, TokenGrace: 24 * time.Hour}, nil, nil)
			j.batchSize = tt.batchSize
			script := &tableScript{results: tt.results}
			got := j.deleteBatched(context.Background(), "t", time.Now(),
				func(_ context.Context, cutoff time.Time, limit int) (int64, error) {
					return script.next(cutoff, limit)
				})
			if got != tt.wantTotal {
				t.Fatalf("total deleted = %d, want %d", got, tt.wantTotal)
			}
			if script.calls != tt.wantCalls {
				t.Fatalf("delete calls = %d, want %d", script.calls, tt.wantCalls)
			}
			if script.limit != tt.batchSize {
				t.Fatalf("batch limit passed = %d, want %d", script.limit, tt.batchSize)
			}
		})
	}
}

// TestSweepCutoffs asserts the per-table cutoffs: sys_workflow_runs uses now - RunsRetention,
// both token tables use now - TokenGrace, and sys_jobs uses now - JobsRetention.
func TestSweepCutoffs(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	retention := 90 * 24 * time.Hour
	grace := 24 * time.Hour
	jobsRetention := 30 * 24 * time.Hour

	f := &fakeSweeper{
		runs:            tableScript{results: []batchResult{{n: 0}}},
		mfa:             tableScript{results: []batchResult{{n: 0}}},
		resets:          tableScript{results: []batchResult{{n: 0}}},
		handoffs:        tableScript{results: []batchResult{{n: 0}}},
		jobs:            tableScript{results: []batchResult{{n: 0}}},
		previewSessions: tableScript{results: []batchResult{{n: 0}}},
		idemKeys:        tableScript{results: []batchResult{{n: 0}}},
		emailLog:        tableScript{results: []batchResult{{n: 0}}},
		usageDaily:      tableScript{results: []batchResult{{n: 0}}},
	}
	usageRetention := 400 * 24 * time.Hour
	j := New(Config{Interval: time.Hour, RunsRetention: retention, TokenGrace: grace, JobsRetention: jobsRetention, UsageRetention: usageRetention}, f, nil)
	j.Sweep(context.Background(), now)

	wantRuns := now.Add(-retention)
	wantToken := now.Add(-grace)
	wantJobs := now.Add(-jobsRetention)
	if want := now.Add(-time.Hour); !f.staleRuns.cutoff.Equal(want) {
		t.Fatalf("stale-running cutoff = %s, want %s (a run still running an hour later is dead)", f.staleRuns.cutoff, want)
	}
	if !f.runs.cutoff.Equal(wantRuns) {
		t.Fatalf("runs cutoff = %s, want %s", f.runs.cutoff, wantRuns)
	}
	if !f.mfa.cutoff.Equal(wantToken) {
		t.Fatalf("mfa cutoff = %s, want %s", f.mfa.cutoff, wantToken)
	}
	if !f.resets.cutoff.Equal(wantToken) {
		t.Fatalf("resets cutoff = %s, want %s", f.resets.cutoff, wantToken)
	}
	if !f.handoffs.cutoff.Equal(wantToken) {
		t.Fatalf("handoffs cutoff = %s, want %s (sso_handoff_tokens shares the token grace)", f.handoffs.cutoff, wantToken)
	}
	if !f.jobs.cutoff.Equal(wantJobs) {
		t.Fatalf("jobs cutoff = %s, want %s (sys_jobs uses its own retention)", f.jobs.cutoff, wantJobs)
	}
	// sys_idempotency_keys (specs/047): expires_at already IS the end of the 24 h replay window,
	// so the cutoff is now itself — no grace (an expired key is ignored by the request path anyway).
	if f.idemKeys.calls != 1 || !f.idemKeys.cutoff.Equal(now) {
		t.Fatalf("idempotency keys swept %d time(s) with cutoff %s, want once with %s", f.idemKeys.calls, f.idemKeys.cutoff, now)
	}
	// sys_preview_sessions (specs/064): the durable trail outlives the session by the runs retention
	// (the audit log holds the long-term record), so it shares RunsRetention and sweeps expires_at.
	if f.previewSessions.calls != 1 || !f.previewSessions.cutoff.Equal(wantRuns) {
		t.Fatalf("preview sessions swept %d time(s) with cutoff %s, want once with %s", f.previewSessions.calls, f.previewSessions.cutoff, wantRuns)
	}
	// sys_email_log (specs/066): the customer-facing mail log shares RunsRetention (90 d).
	if f.emailLog.calls != 1 || !f.emailLog.cutoff.Equal(wantRuns) {
		t.Fatalf("email log swept %d time(s) with cutoff %s, want once with %s", f.emailLog.calls, f.emailLog.cutoff, wantRuns)
	}
	// sys_usage_daily (specs/066 L4): its own USAGE_RETENTION_DAYS, not the 90 d runs retention.
	if wantUsage := now.Add(-usageRetention); f.usageDaily.calls != 1 || !f.usageDaily.cutoff.Equal(wantUsage) {
		t.Fatalf("usage daily swept %d time(s) with cutoff %s, want once with %s", f.usageDaily.calls, f.usageDaily.cutoff, wantUsage)
	}
}

// TestNewClampsNonPositiveRetention guards against a misconfig deleting "everything older than now":
// a <= 0 retention/grace is clamped to a safe non-zero default.
func TestNewClampsNonPositiveRetention(t *testing.T) {
	j := New(Config{Interval: time.Hour}, nil, nil)
	if j.runsRetention <= 0 {
		t.Fatalf("runsRetention not clamped: %s", j.runsRetention)
	}
	if j.tokenGrace <= 0 {
		t.Fatalf("tokenGrace not clamped: %s", j.tokenGrace)
	}
	if j.jobsRetention <= 0 {
		t.Fatalf("jobsRetention not clamped: %s", j.jobsRetention)
	}
}

// TestSweepIsTableIndependent asserts one table's error does not abort the other tables' sweeps.
func TestSweepIsTableIndependent(t *testing.T) {
	f := &fakeSweeper{
		runs:     tableScript{results: []batchResult{{err: errors.New("runs table locked")}}},
		mfa:      tableScript{results: []batchResult{{n: 5}}},
		resets:   tableScript{results: []batchResult{{n: 7}}},
		handoffs: tableScript{results: []batchResult{{n: 3}}},
		jobs:     tableScript{results: []batchResult{{n: 2}}},
		idemKeys: tableScript{results: []batchResult{{n: 4}}},
	}
	j := New(Config{Interval: time.Hour, RunsRetention: 90 * 24 * time.Hour, TokenGrace: 24 * time.Hour}, f, nil)
	j.Sweep(context.Background(), time.Now().UTC())

	if f.mfa.calls != 1 || f.resets.calls != 1 || f.handoffs.calls != 1 || f.jobs.calls != 1 || f.idemKeys.calls != 1 {
		t.Fatalf("other tables not swept after runs error: mfa=%d resets=%d handoffs=%d jobs=%d idem=%d",
			f.mfa.calls, f.resets.calls, f.handoffs.calls, f.jobs.calls, f.idemKeys.calls)
	}
}
