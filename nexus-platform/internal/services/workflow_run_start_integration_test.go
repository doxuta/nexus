//go:build integration

// #338: the run row is written BEFORE the actions (StartRun) and finished after (FinishRun); a
// delivery that gives up in between is kept, and a row the server died on is closed by the janitor.
//
//	TEST_DATABASE_URL=… go test -tags=integration ./internal/services/ -run 'TestRunStart'
package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/janitor"
	"nexus/platform/internal/workflow"
)

const rsTenant = "a0a0a0a0-0000-4000-8000-000000000338"

func TestRunStart_LateGiveUpSurvivesFinish(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	const id = "10000000-0000-4000-8000-000000000338"
	now := time.Now().UTC()
	if err := repo.StartRun(ctx, workflow.RunInput{ID: id, TenantID: rsTenant, WorkflowID: "wf-1", TriggerType: "after_create",
		RecordID: "rec-1", ActionsResult: []byte(`[{"type":"field_update"},{"type":"email_alert"}]`), StartedAt: now}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	var st string
	if err := sdb.GetContext(ctx, &st, `SELECT status FROM sys_workflow_runs WHERE tenant_id = ? AND id = ?`, rsTenant, id); err != nil || st != "running" {
		t.Fatalf("after start: status %q err %v; want running", st, err)
	}
	// The email gave up on its first attempt, before the dispatcher finished the run.
	if found, err := repo.MarkRunActionFailed(ctx, rsTenant, id, 1, "job-1", "gửi lỗi [redacted]"); err != nil || !found {
		t.Fatalf("mark on a RUNNING row: found=%v err=%v; want found (no waiting for the row)", found, err)
	}
	// The dispatcher then finishes with the results it saw: both actions ok (the email only queued).
	final, _ := json.Marshal([]workflow.ActionResult{{Type: "field_update", OK: true}, {Type: "email_alert", OK: true}})
	if err := repo.FinishRun(ctx, workflow.RunInput{ID: id, TenantID: rsTenant, WorkflowID: "wf-1", TriggerType: "after_create",
		RecordID: "rec-1", Status: workflow.RunSuccess, ActionsResult: final, StartedAt: now, FinishedAt: now.Add(time.Second)}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var errText *string
	var actions string
	if err := sdb.QueryRowxContext(ctx, `SELECT status, error, CAST(actions_result AS CHAR) FROM sys_workflow_runs WHERE tenant_id = ? AND id = ?`,
		rsTenant, id).Scan(&st, &errText, &actions); err != nil {
		t.Fatalf("read: %v", err)
	}
	var slots []map[string]any
	_ = json.Unmarshal([]byte(actions), &slots)
	if st != "failed" || errText == nil || *errText != "gửi lỗi [redacted]" || len(slots) != 2 || slots[0]["ok"] != true || slots[1]["ok"] != false {
		t.Fatalf("after finish: %s / %v / %s; want failed, the late reason kept, slot 0 ok, slot 1 (the email) failed", st, errText, actions)
	}
}

func TestRunStart_FinishWithoutStartInsertsTheRow(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	now := time.Now().UTC()
	const id = "10000000-0000-4000-8000-000000000339"
	if err := repo.FinishRun(ctx, workflow.RunInput{ID: id, TenantID: rsTenant, WorkflowID: "wf-1", TriggerType: "after_create",
		Status: workflow.RunSuccess, ActionsResult: []byte(`[]`), StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var n int
	if err := sdb.GetContext(ctx, &n, `SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND id = ? AND status = 'success'`, rsTenant, id); err != nil || n != 1 {
		t.Fatalf("rows = %d err %v; want the row inserted (the start was lost, fail-open)", n, err)
	}
}

func TestRunStart_ScheduledLateFailuresAddToTheSummary(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	now := time.Now().UTC()
	const id = "10000000-0000-4000-8000-000000000340"
	if err := repo.StartRun(ctx, workflow.RunInput{ID: id, TenantID: rsTenant, WorkflowID: "wf-s", TriggerType: "scheduled",
		ActionsResult: []byte(`{"scanned":0,"matched":0,"failed":0}`), StartedAt: now}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if found, err := repo.MarkRunActionFailed(ctx, rsTenant, id, 0, "job-1", "x"); err != nil || !found {
		t.Fatalf("mark: %v %v", found, err)
	}
	if err := repo.FinishRun(ctx, workflow.RunInput{ID: id, TenantID: rsTenant, WorkflowID: "wf-s", TriggerType: "scheduled",
		Status: workflow.RunSuccess, ActionsResult: []byte(`{"scanned":5,"matched":3,"failed":1}`), StartedAt: now, FinishedAt: now}); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var st, actions string
	if err := sdb.QueryRowxContext(ctx, `SELECT status, CAST(actions_result AS CHAR) FROM sys_workflow_runs WHERE tenant_id = ? AND id = ?`, rsTenant, id).Scan(&st, &actions); err != nil {
		t.Fatalf("read: %v", err)
	}
	var sum map[string]any
	_ = json.Unmarshal([]byte(actions), &sum)
	// 1 record's own action error + 1 late give-up = 2; matched kept; and a repeat of job-1 is still ignored.
	if st != "failed" || sum["failed"] != float64(2) || sum["matched"] != float64(3) {
		t.Fatalf("scheduled finish: %s %v; want failed with failed=2 (1 own + 1 late), matched=3", st, sum)
	}
}

func TestRunStart_JanitorClosesARunTheServerDiedOn(t *testing.T) {
	_, sdb := o3aSetup(t)
	ctx := context.Background()
	repo := workflow.NewSQLRepository(sdb)
	now := time.Now().UTC()
	const stale, fresh = "10000000-0000-4000-8000-0000000003a1", "10000000-0000-4000-8000-0000000003a2"
	for id, at := range map[string]time.Time{stale: now.Add(-2 * time.Hour), fresh: now.Add(-time.Minute)} {
		if err := repo.StartRun(ctx, workflow.RunInput{ID: id, TenantID: rsTenant, WorkflowID: "wf-1", TriggerType: "after_create",
			ActionsResult: []byte(`[]`), StartedAt: at}); err != nil {
			t.Fatalf("StartRun %s: %v", id, err)
		}
	}
	n, err := janitor.NewSQLSweeper(sdb).FailStaleWorkflowRunsStartedBefore(ctx, now.Add(-time.Hour), 100)
	if err != nil || n != 1 {
		t.Fatalf("closed %d (err %v); want exactly the 2-hour-old run", n, err)
	}
	for id, want := range map[string]string{stale: "failed", fresh: "running"} {
		var st string
		if err := sdb.GetContext(ctx, &st, `SELECT status FROM sys_workflow_runs WHERE tenant_id = ? AND id = ?`, rsTenant, id); err != nil || st != want {
			t.Errorf("run %s: status %q err %v; want %q", id, st, err, want)
		}
	}
}
