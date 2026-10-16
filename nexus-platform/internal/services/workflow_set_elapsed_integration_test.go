//go:build integration

// DAILY-QUEUE #41 (`stop-clock-action`) — the set_elapsed action exercised END-TO-END through the
// production composition over a freshly-migrated throwaway MySQL DB (wfWire + the CALENDAR-1
// resolver, mirroring cmd/server/main.go).
//
// What this proves that internal/workflow/set_elapsed_test.go (fakes only) cannot:
//   - the record's creation instant actually reaches the action: RecordService.Update copies
//     rec.CreatedAt into the after_update Event (a site nobody threaded would leave it zero, and
//     the action would record a FAILED run instead of a number);
//   - the working calendar is the tenant's persisted DEFAULT, resolved through the real store;
//   - the float64 the action hands over survives the real `number` validator and lands in
//     obj_*.data, and the W6 run history says `success` with no wall-clock warning.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -count=1 -run SetElapsed ./internal/services/
package services

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/workflow"
)

func TestSetElapsed_ResolvedTicketCarriesBusinessMinutes(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "11111111-0000-0000-0000-0000000000e1"

	// The tenant's default working calendar — the seed's exact bytes (Mon-Fri 08:00-17:00 ICT).
	calRepo := calmeta.NewSQLRepository(st.sdb)
	if _, err := calRepo.Create(ctx, calmeta.CreateInput{
		TenantID: tenant, Name: DefaultCalendarName, Timezone: DefaultCalendarTimezone,
		Hours: []byte(DefaultCalendarHoursJSON), IsDefault: true, CreatedBy: user,
	}); err != nil {
		t.Fatalf("create default calendar: %v", err)
	}
	sched, err := DefaultCalendarSchedule()
	if err != nil {
		t.Fatalf("DefaultCalendarSchedule: %v", err)
	}
	st.dispatcher.WithScheduleResolver(NewWorkflowScheduleResolver(calRepo, zap.NewNop())).
		WithRunRecorder(workflow.NewSQLRepository(st.sdb))

	st.createEntity(t, ctx, tenant, "el_tickets", []FieldInput{
		{Name: "subject", Label: "Subject", FieldType: "text"},
		{Name: "status", Label: "Status", FieldType: "text"},
		{Name: "resolution_minutes", Label: "Số phút xử lý", FieldType: "number"},
	})
	// The acceptance rule: when a ticket BECOMES Resolved, stamp the working minutes it took.
	wfRec, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name:       "resolution-minutes",
		EntityName: "el_tickets",
		Trigger:    "after_update",
		Rules:      []WorkflowRuleInput{{Field: "status", Op: "changedTo", Value: "Resolved"}},
		Actions:    []WorkflowActionInput{{Type: "set_elapsed", Field: "resolution_minutes"}},
		ActorID:    user,
	})
	if err != nil {
		t.Fatalf("CreateWorkflow: %v", err)
	}

	id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "el_tickets",
		map[string]any{"subject": "Máy in tầng 3 kẹt giấy", "status": "Open"})
	if err != nil {
		t.Fatalf("Create ticket: %v", err)
	}
	// Opened a week ago: the window crosses nights AND a weekend, so working minutes and
	// wall-clock minutes differ by thousands and a wall-clock implementation cannot pass.
	if _, err := st.sdb.ExecContext(ctx,
		"UPDATE `obj_el_tickets` SET created_at = created_at - INTERVAL 7 DAY WHERE tenant_id = ? AND id = ?",
		tenant, id); err != nil {
		t.Fatalf("backdate created_at: %v", err)
	}
	// Expectation is derived from created_at AS THE REPO READS IT BACK — the same value the update
	// path hands the engine — so the DSN's loc= setting cannot skew expectation against result.
	rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "el_tickets", id, nil, nil)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}
	created := rec.CreatedAt

	before := time.Now().UTC()
	if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "el_tickets", id,
		map[string]any{"status": "Resolved"}, nil); err != nil {
		t.Fatalf("Update to Resolved: %v", err)
	}
	after := time.Now().UTC()
	st.drain(t)

	rec, err = st.recordSvc.Get(ctx, wfAdmin(user), tenant, "el_tickets", id, nil, nil)
	if err != nil {
		t.Fatalf("Get after close: %v", err)
	}
	got, ok := rec.Data["resolution_minutes"].(float64)
	if !ok {
		t.Fatalf("resolution_minutes = %#v after the ticket was resolved, want a number — data=%+v", rec.Data["resolution_minutes"], rec.Data)
	}
	lo := math.Floor(sched.BusinessDurationBetween(created, before).Minutes())
	hi := math.Floor(sched.BusinessDurationBetween(created, after).Minutes())
	if got < lo || got > hi {
		t.Fatalf("resolution_minutes = %v, want the working minutes since creation within [%v, %v]", got, lo, hi)
	}
	if wall := before.Sub(created).Minutes(); got > wall-3000 {
		t.Fatalf("resolution_minutes = %v against %v wall-clock minutes — nights/weekend were counted", got, wall)
	}

	var status string
	var actionsJSON []byte
	if err := st.sdb.QueryRow(
		"SELECT status, actions_result FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ? ORDER BY started_at DESC LIMIT 1",
		tenant, wfRec.ID,
	).Scan(&status, &actionsJSON); err != nil {
		t.Fatalf("read run history: %v", err)
	}
	var recorded []workflow.ActionResult
	if err := json.Unmarshal(actionsJSON, &recorded); err != nil {
		t.Fatalf("decode actions_result %s: %v", actionsJSON, err)
	}
	if status != string(workflow.RunSuccess) || len(recorded) != 1 || !recorded[0].OK || recorded[0].Warning != "" {
		t.Fatalf("run = %q %s; want one successful set_elapsed with no wall-clock warning", status, actionsJSON)
	}
}
