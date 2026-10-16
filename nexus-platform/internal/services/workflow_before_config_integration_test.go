//go:build integration

// specs/048 S1a (T009, T014) over a freshly migrated throwaway MySQL DB (o3aSetup applies every
// migration, including the before-hooks ENUM widening):
//   - sys_workflows accepts trigger_event before_save / before_delete and sys_workflow_runs accepts
//     status 'blocked' (without the migration the INSERT dies 1265 "Data truncated" — lesson 000063);
//   - CountActive (the 50-active-before-rules cap) is tenant-pinned and counts only ACTIVE,
//     NON-DELETED rules of the same (entity, trigger), never the excluded id.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run 'TestCountActive_TenantScoped|TestBeforeHooksMigration'
package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/workflow"
)

func TestCountActive_TenantScoped(t *testing.T) {
	_, sdb := o3aSetup(t)
	repo := workflow.NewSQLRepository(sdb)
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	mk := func(tenant, entity string, trig workflow.Trigger, active bool) string {
		t.Helper()
		id, err := repo.Create(ctx, workflow.CreateInput{
			TenantID: tenant, Name: uuid.NewString(), EntityName: entity, Trigger: trig,
			Actions: []byte(`[{"type":"veto","message":"x"}]`), IsActive: active,
		})
		if err != nil {
			t.Fatalf("create %s rule: %v", trig, err)
		}
		return id
	}
	var aIDs []string
	for i := 0; i < 50; i++ {
		aIDs = append(aIDs, mk(tenantA, "deals", workflow.TriggerBeforeSave, true))
	}
	mk(tenantA, "deals", workflow.TriggerBeforeSave, false)  // inactive: not counted
	mk(tenantA, "deals", workflow.TriggerBeforeDelete, true) // other trigger: not counted
	mk(tenantA, "leads", workflow.TriggerBeforeSave, true)   // other entity: not counted
	deleted := mk(tenantA, "deals", workflow.TriggerBeforeSave, true)
	if err := repo.Delete(ctx, tenantA, deleted); err != nil { // soft-deleted: not counted
		t.Fatal(err)
	}
	mk(tenantB, "deals", workflow.TriggerBeforeSave, true) // other tenant: not counted

	tests := []struct {
		name      string
		tenant    string
		trigger   workflow.Trigger
		excludeID string
		want      int
	}{
		{"tenant A before_save", tenantA, workflow.TriggerBeforeSave, "", 50},
		{"tenant A before_save excluding the rule being edited", tenantA, workflow.TriggerBeforeSave, aIDs[0], 49},
		{"tenant A before_delete", tenantA, workflow.TriggerBeforeDelete, "", 1},
		{"tenant B sees only its own", tenantB, workflow.TriggerBeforeSave, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := repo.CountActive(ctx, tt.tenant, "deals", tt.trigger, tt.excludeID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("CountActive = %d, want %d", got, tt.want)
			}
		})
	}
}

// The ENUM widening itself: a 'blocked' run row round-trips (S3 writes them; the value must exist
// before any binary that writes it is deployed).
func TestBeforeHooksMigration_RunsAcceptBlocked(t *testing.T) {
	_, sdb := o3aSetup(t)
	repo := workflow.NewSQLRepository(sdb)
	ctx := context.Background()
	tenant := uuid.NewString()
	wfID, err := repo.Create(ctx, workflow.CreateInput{
		TenantID: tenant, Name: "r", EntityName: "deals", Trigger: workflow.TriggerBeforeDelete,
		Actions: []byte(`[{"type":"veto","message":"x"}]`),
	})
	if err != nil {
		t.Fatalf("before_delete rule must insert after the migration: %v", err)
	}
	now := time.Now().UTC()
	if err := repo.InsertRun(ctx, workflow.RunInput{
		TenantID: tenant, WorkflowID: wfID, TriggerType: string(workflow.TriggerBeforeDelete),
		Status: workflow.RunBlocked, StartedAt: now, FinishedAt: now,
	}); err != nil {
		t.Fatalf("a blocked run must insert after the migration: %v", err)
	}
	var status string
	if err := sdb.GetContext(ctx, &status, `SELECT status FROM sys_workflow_runs WHERE tenant_id = ? AND workflow_id = ?`, tenant, wfID); err != nil {
		t.Fatal(err)
	}
	if status != "blocked" {
		t.Fatalf("status = %q, want blocked (MySQL coerced an unknown ENUM value?)", status)
	}
}

// SetActive flips only is_active: tenant-pinned, never touches the actions, a no-op flip is not a
// miss (MySQL reports 0 affected rows for it), a deleted rule is not found.
func TestSetActive_TenantScopedAndActionsUntouched(t *testing.T) {
	_, sdb := o3aSetup(t)
	repo := workflow.NewSQLRepository(sdb)
	ctx := context.Background()
	tenantA, tenantB := uuid.NewString(), uuid.NewString()
	actions := `[{"type":"outbound_webhook","url":"https://erp.example.vn/hook","secret":"k"}]`
	id, err := repo.Create(ctx, workflow.CreateInput{TenantID: tenantA, Name: "r", EntityName: "deals",
		Trigger: workflow.TriggerAfterCreate, Actions: []byte(actions), IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetActive(ctx, tenantB, id, false, ""); err != workflow.ErrWorkflowNotFound {
		t.Fatalf("another tenant's switch = %v, want ErrWorkflowNotFound", err)
	}
	if err := repo.SetActive(ctx, tenantA, id, false, "u1"); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetActive(ctx, tenantA, id, false, "u1"); err != nil {
		t.Fatalf("a switch that changes nothing must not read as not-found, got %v", err)
	}
	rec, err := repo.Get(ctx, tenantA, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.IsActive || !strings.Contains(string(rec.Actions), `"secret"`) {
		t.Fatalf("want paused with actions intact, got active=%v actions=%s", rec.IsActive, rec.Actions)
	}
	if err := repo.Delete(ctx, tenantA, id); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetActive(ctx, tenantA, id, true, ""); err != workflow.ErrWorkflowNotFound {
		t.Fatalf("a deleted rule's switch = %v, want ErrWorkflowNotFound", err)
	}
}
