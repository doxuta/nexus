//go:build integration

// DAILY-QUEUE #9 (`set-deadline-action-a`) — the set_deadline action exercised END-TO-END through
// the production composition over a freshly-migrated throwaway MySQL DB (wfWire, mirroring
// cmd/server/main.go).
//
// What this proves that internal/workflow/set_deadline_test.go (fakes only) cannot: the computed
// instant survives the REAL write path. The action hands over an RFC3339 string; the record layer
// then runs it through the datetime validator and normalizeForStore → canonicalDatetimeUTC before
// it lands in obj_*.data. A shape mismatch anywhere along there is invisible to a fake mutator and
// fatal in production — the rule would save, fire, and record a FAILED run while the 'Hạn xử lý'
// box stayed empty, which is exactly the silence this slice exists to remove.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -run SetDeadline ./internal/services/
package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/workflow"
)

// readDeadline reads the target field back through the record service and parses the PERSISTED
// shape. The persisted form is the MySQL literal ("2006-01-02 15:04:05", UTC) — not the RFC3339
// the action wrote — so parsing it here is itself the assertion that the canonicalization ran.
func readDeadline(t *testing.T, st *wfStack, ctx context.Context, tenant, user, entity, id string) (time.Time, string) {
	t.Helper()
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, entity, id, nil, nil)
	if err != nil {
		t.Fatalf("Get after fire: %v", err)
	}
	raw, _ := rec.Data["due_date"].(string)
	if raw == "" {
		t.Fatalf("due_date is empty after the rule fired — data=%+v", rec.Data)
	}
	ts, err := time.Parse("2006-01-02 15:04:05", raw)
	if err != nil {
		t.Fatalf("due_date persisted as %q, which is not the canonical MySQL datetime literal: %v", raw, err)
	}
	return ts, raw
}

// TestSetDeadline_AfterCreate_StampsComputedDueDate is the slice's acceptance in code: an Urgent
// ticket created with NO due date comes back carrying now+4h.
func TestSetDeadline_AfterCreate_StampsComputedDueDate(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000d1"

	st.createEntity(t, ctx, tenant, "sla_tickets", []FieldInput{
		{Name: "subject", Label: "Subject", FieldType: "text"},
		{Name: "priority", Label: "Priority", FieldType: "text"},
		{Name: "due_date", Label: "Hạn xử lý", FieldType: "datetime"},
	})

	// Rule: on after_create of sla_tickets, IF priority == "Urgent" THEN due_date = now + 4 hours.
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "urgent-4h",
		EntityName: "sla_tickets",
		Trigger:    "after_create",
		Rules:      []WorkflowRuleInput{{Field: "priority", Op: "eq", Value: "Urgent"}},
		Actions: []WorkflowActionInput{{
			Type: "set_deadline", Field: "due_date",
			Offset: &WorkflowTimerOffsetInput{Amount: 4, Unit: "hours"},
		}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	before := time.Now().UTC()
	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "sla_tickets",
		map[string]any{"subject": "Máy chủ không phản hồi", "priority": "Urgent"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	st.drain(t)

	got, raw := readDeadline(t, st, ctx, tenant, user, "sla_tickets", id)
	want := before.Add(4 * time.Hour)
	if d := got.Sub(want); d < -30*time.Second || d > 30*time.Second {
		t.Fatalf("due_date persisted as %q (%s), which is %s away from now+4h (%s)",
			raw, got.Format(time.RFC3339), d, want.Format(time.RFC3339))
	}
}

// TestSetDeadline_ConditionNoMatch_LeavesDueDateEmpty: a non-Urgent ticket keeps an empty deadline.
// Without this, a rule that stamped EVERY ticket would pass the test above unnoticed.
func TestSetDeadline_ConditionNoMatch_LeavesDueDateEmpty(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000d2"

	st.createEntity(t, ctx, tenant, "sla_tickets2", []FieldInput{
		{Name: "priority", Label: "Priority", FieldType: "text"},
		{Name: "due_date", Label: "Hạn xử lý", FieldType: "datetime"},
	})
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "urgent-4h",
		EntityName: "sla_tickets2",
		Trigger:    "after_create",
		Rules:      []WorkflowRuleInput{{Field: "priority", Op: "eq", Value: "Urgent"}},
		Actions: []WorkflowActionInput{{
			Type: "set_deadline", Field: "due_date",
			Offset: &WorkflowTimerOffsetInput{Amount: 4, Unit: "hours"},
		}},
		ActorID: user,
	}); err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "sla_tickets2",
		map[string]any{"priority": "Low"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	st.drain(t)

	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "sla_tickets2", id, nil, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v, ok := rec.Data["due_date"]; ok && v != nil && v != "" {
		t.Fatalf("due_date = %v on a Low-priority ticket; the rule must not have fired", v)
	}
}

// TestSetDeadline_RecordedRunIsSuccessful reads the W6 run history: the whole failure mode this
// slice guards against is a rule that LOOKS configured and quietly records FAILED runs, so the
// history must say the action succeeded, not merely that the value happens to be there.
func TestSetDeadline_RecordedRunIsSuccessful(t *testing.T) {
	st := wfWire(t)
	// wfWire leaves the W6 run recorder unwired (its own tests read record state, not history).
	// Arm it here: whether the RUN was recorded as successful is precisely the question.
	st.dispatcher.WithRunRecorder(workflow.NewSQLRepository(st.sdb))
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000d3"

	st.createEntity(t, ctx, tenant, "sla_tickets3", []FieldInput{
		{Name: "priority", Label: "Priority", FieldType: "text"},
		{Name: "due_date", Label: "Hạn xử lý", FieldType: "datetime"},
	})
	wfRec, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "urgent-4h",
		EntityName: "sla_tickets3",
		Trigger:    "after_create",
		Rules:      []WorkflowRuleInput{{Field: "priority", Op: "eq", Value: "Urgent"}},
		Actions: []WorkflowActionInput{{
			Type: "set_deadline", Field: "due_date",
			Offset: &WorkflowTimerOffsetInput{Amount: 90, Unit: "minutes"},
		}},
		ActorID: user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	before := time.Now().UTC()
	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "sla_tickets3",
		map[string]any{"priority": "Urgent"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	st.drain(t)

	got, raw := readDeadline(t, st, ctx, tenant, user, "sla_tickets3", id)
	if d := got.Sub(before.Add(90 * time.Minute)); d < -30*time.Second || d > 30*time.Second {
		t.Fatalf("due_date %q is %s away from now+90m", raw, d)
	}

	var status string
	var actionsJSON []byte
	if err := st.sdb.QueryRow(
		"SELECT status, actions_result FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ? ORDER BY started_at DESC LIMIT 1",
		tenant, wfRec.ID,
	).Scan(&status, &actionsJSON); err != nil {
		t.Fatalf("read run history: %v", err)
	}
	if status != string(workflow.RunSuccess) {
		t.Fatalf("run status = %q (actions_result=%s); want %q", status, actionsJSON, workflow.RunSuccess)
	}
	// MySQL re-serializes the JSON column with spaces, so compare on a decoded value rather than
	// on the raw bytes (a substring match here silently passed on nothing at all).
	var recorded []struct {
		Type  string `json:"type"`
		Field string `json:"field"`
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(actionsJSON, &recorded); err != nil {
		t.Fatalf("decode actions_result %s: %v", actionsJSON, err)
	}
	if len(recorded) != 1 || recorded[0].Type != "set_deadline" || recorded[0].Field != "due_date" || !recorded[0].OK {
		t.Fatalf("actions_result = %s; want exactly one successful set_deadline on due_date", actionsJSON)
	}
}
