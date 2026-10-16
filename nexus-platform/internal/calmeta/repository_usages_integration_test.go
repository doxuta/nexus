//go:build integration

package calmeta_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/calmeta"
)

// DAILY-QUEUE #326: Usages finds live rules pinning a calendar through BOTH doors (the
// date_offset timer in `condition`, a set_deadline action), counts a disabled rule, ignores a
// soft-deleted rule, an unrelated calendar id, and another tenant's rule pinning the same id.
func TestCalendarRepo_UsagesBothDoorsTenantScoped(t *testing.T) {
	db := testDB(t)
	cleanTenants(t, db)
	ctx := context.Background()
	repo := calmeta.NewSQLRepository(db)

	cal, err := repo.Create(ctx, createInput(tenantA, "cal_pinned", false))
	if err != nil {
		t.Fatalf("create calendar: %v", err)
	}
	other := uuid.NewString()
	t.Cleanup(func() {
		cleanTenants(t, db)
		_, _ = db.ExecContext(ctx, "DELETE FROM sys_workflows WHERE tenant_id IN (?, ?)", tenantA, tenantB)
	})

	rule := func(tenant, name, cond, actions string, active, deleted bool) {
		t.Helper()
		_, err := db.ExecContext(ctx, "INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, `condition`, actions, is_active, deleted_at)\n"+
			"VALUES (?, ?, ?, 'crm_contacts', 'after_create', ?, ?, ?, IF(?, NOW(3), NULL))",
			uuid.NewString(), tenant, name, cond, actions, active, deleted)
		if err != nil {
			t.Fatalf("insert rule %s: %v", name, err)
		}
	}
	timer := `{"match":"and","timer":{"dateField":"d","calendarId":"` + cal + `"}}`
	deadline := `[{"type":"field_update","field":"x","value":"y"},{"type":"set_deadline","field":"due","calendarId":"` + cal + `"}]`
	none := `[{"type":"field_update","field":"x","value":"y"}]`

	rule(tenantA, "b-timer", timer, none, true, false)
	rule(tenantA, "a-deadline-disabled", `{"match":"and"}`, deadline, false, false)
	rule(tenantA, "z-deleted", timer, deadline, true, true)
	rule(tenantA, "z-other-calendar", `{"timer":{"calendarId":"`+other+`"}}`, `[{"type":"set_deadline","calendarId":"`+other+`"}]`, true, false)
	rule(tenantA, "z-no-pin", `{"match":"and"}`, none, true, false)
	rule(tenantB, "tenant-b-same-id", timer, deadline, true, false)

	got, err := repo.Usages(ctx, tenantA, cal)
	if err != nil {
		t.Fatalf("Usages: %v", err)
	}
	if len(got) != 2 || got[0].WorkflowName != "a-deadline-disabled" || got[1].WorkflowName != "b-timer" {
		t.Fatalf("usages = %+v; want [a-deadline-disabled b-timer] (name order, once each, tenant A only)", got)
	}
	free, err := repo.Usages(ctx, tenantA, other+"-nobody")
	if err != nil || len(free) != 0 {
		t.Fatalf("unpinned id usages = %+v (%v); want none", free, err)
	}
}
