//go:build integration

// DAILY-QUEUE #79 (`set-deadline-calendar-pin`) — a set_deadline pinned to its own working calendar,
// end-to-end through the production composition over a freshly-migrated throwaway MySQL DB (wfWire
// + the real CALENDAR-1 store as both the save-time checker and the run-time resolver, exactly as
// cmd/server/main.go wires them).
//
// The tenant has TWO calendars: the seeded office week (Mon-Fri 08:00-17:00 ICT, the DEFAULT) and a
// round-the-clock one. 12 business hours on the office week can never be 12 wall hours (a working
// day is 9), so a deadline of exactly created+12h is only reachable through the pin.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -run SetDeadlinePinned ./internal/services/
package services

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/workflow"
)

const roundTheClockHours = `{"mon":[{"start":"00:00","end":"24:00"}],"tue":[{"start":"00:00","end":"24:00"}],
	"wed":[{"start":"00:00","end":"24:00"}],"thu":[{"start":"00:00","end":"24:00"}],
	"fri":[{"start":"00:00","end":"24:00"}],"sat":[{"start":"00:00","end":"24:00"}],
	"sun":[{"start":"00:00","end":"24:00"}]}`

// pinStack wires the calendar store into both halves and returns the round-the-clock calendar id.
func pinStack(t *testing.T, ctx context.Context, tenant, user string) (*wfStack, *calmeta.SQLRepository, string) {
	t.Helper()
	st := wfWire(t)
	calRepo := calmeta.NewSQLRepository(st.sdb)
	if _, err := calRepo.Create(ctx, calmeta.CreateInput{
		TenantID: tenant, Name: DefaultCalendarName, Timezone: DefaultCalendarTimezone,
		Hours: []byte(DefaultCalendarHoursJSON), IsDefault: true, CreatedBy: user,
	}); err != nil {
		t.Fatalf("create default calendar: %v", err)
	}
	pinned, err := calRepo.Create(ctx, calmeta.CreateInput{
		TenantID: tenant, Name: "Trực 24/7", Timezone: "UTC", Hours: []byte(roundTheClockHours), CreatedBy: user,
	})
	if err != nil {
		t.Fatalf("create round-the-clock calendar: %v", err)
	}
	resolver := NewWorkflowScheduleResolver(calRepo, zap.NewNop())
	st.dispatcher.WithScheduleResolver(resolver).WithRunRecorder(workflow.NewSQLRepository(st.sdb))
	st.workflowSvc.WithCalendarChecker(calRepo).WithScheduleResolver(resolver)
	st.createEntity(t, ctx, tenant, "pin_tickets", []FieldInput{
		{Name: "subject", Label: "Subject", FieldType: "text"},
		{Name: "due_date", Label: "Hạn xử lý", FieldType: "datetime"},
	})
	return st, calRepo, pinned
}

func pinnedDeadlineRule(calendarID string) CreateWorkflowInput {
	return CreateWorkflowInput{
		Name: "pinned-12bh", EntityName: "pin_tickets", Trigger: "after_create",
		Actions: []WorkflowActionInput{{
			Type: "set_deadline", Field: "due_date", CalendarID: calendarID,
			Offset: &WorkflowTimerOffsetInput{Amount: 12, Unit: workflow.TimerUnitBusinessHours},
		}},
	}
}

// lastRunAction reads the single action result of the rule's latest W6 run.
func lastRunAction(t *testing.T, st *wfStack, tenant, workflowID string) (string, workflow.ActionResult) {
	t.Helper()
	var status string
	var raw []byte
	if err := st.sdb.QueryRow(
		"SELECT status, actions_result FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ? ORDER BY started_at DESC LIMIT 1",
		tenant, workflowID,
	).Scan(&status, &raw); err != nil {
		t.Fatalf("read run history: %v", err)
	}
	var results []workflow.ActionResult
	if err := json.Unmarshal(raw, &results); err != nil || len(results) != 1 {
		t.Fatalf("actions_result %s: %v", raw, err)
	}
	return status, results[0]
}

func TestSetDeadlinePinned_WalksThePinnedCalendarNotTheDefault(t *testing.T) {
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000f1"
	st, calRepo, pinned := pinStack(t, ctx, tenant, user)

	// The save refuses a pin that cannot work — through the REAL tenant-scoped store.
	other, err := calRepo.Create(ctx, calmeta.CreateInput{
		TenantID: o3aTenantB, Name: "Lịch tổ chức khác", Timezone: "UTC", Hours: []byte(roundTheClockHours), CreatedBy: user,
	})
	if err != nil {
		t.Fatalf("create other-tenant calendar: %v", err)
	}
	for _, bad := range []string{"00000000-0000-0000-0000-00000000dead", other} {
		_, err := st.workflowSvc.CreateWorkflow(ctx, tenant, pinnedDeadlineRule(bad))
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "SET_DEADLINE_CALENDAR_NOT_FOUND" || ve.Field != "actions[0].calendarId" {
			t.Fatalf("pin %q: err = %v, want SET_DEADLINE_CALENDAR_NOT_FOUND on actions[0].calendarId", bad, err)
		}
	}

	rule, err := st.workflowSvc.CreateWorkflow(ctx, tenant, pinnedDeadlineRule(pinned))
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	before := time.Now().UTC()
	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "pin_tickets",
		map[string]any{"subject": "Sự cố đêm — trực 24/7"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	after := time.Now().UTC()
	st.drain(t)

	got, raw := readDeadline(t, st, ctx, tenant, user, "pin_tickets", id)
	// Persisted at second precision: MySQL DATETIME rounds (not truncates) the fraction, so the upper edge needs +1s and the lower edge a truncation.
	if got.Before(before.Add(12*time.Hour).Truncate(time.Second)) || got.After(after.Add(12*time.Hour+time.Second)) { // +1s: MySQL ROUNDS fractional seconds (x.5s+ stores as x+1s)
		t.Fatalf("due_date = %q; want created+12h — the round-the-clock pin, not the office-week default", raw)
	}
	status, res := lastRunAction(t, st, tenant, rule.ID)
	if status != string(workflow.RunSuccess) || !res.OK || res.Warning != "" {
		t.Fatalf("run = %q %+v; want success with no wall-clock warning", status, res)
	}
}

// The TIMER-1 contract for a pin that outlives its calendar: wall-clock, never a dead rule — and
// never a silent switch to the DEFAULT calendar, which is a different promise the admin did not pick.
func TestSetDeadlinePinned_DeletedCalendarFallsBackLoudly(t *testing.T) {
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000f2"
	st, calRepo, pinned := pinStack(t, ctx, tenant, user)

	rule, err := st.workflowSvc.CreateWorkflow(ctx, tenant, pinnedDeadlineRule(pinned))
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}
	if err := calRepo.SoftDelete(ctx, tenant, pinned); err != nil {
		t.Fatalf("delete pinned calendar: %v", err)
	}
	before := time.Now().UTC()
	id, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, user, "pin_tickets",
		map[string]any{"subject": "Lịch ghim đã bị xoá"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	after := time.Now().UTC()
	st.drain(t)

	got, raw := readDeadline(t, st, ctx, tenant, user, "pin_tickets", id)
	if got.Before(before.Add(12*time.Hour).Truncate(time.Second)) || got.After(after.Add(12*time.Hour+time.Second)) { // +1s: MySQL ROUNDS fractional seconds (x.5s+ stores as x+1s)
		t.Fatalf("due_date = %q; want the wall-clock fallback now+12h, not the office-week default", raw)
	}
	status, res := lastRunAction(t, st, tenant, rule.ID)
	if status != string(workflow.RunSuccess) || !res.OK || res.Warning == "" {
		t.Fatalf("run = %q %+v; want success carrying the pinned-calendar warning", status, res)
	}
}
