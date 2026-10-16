package vertical_pack

// DAILY-QUEUE #79: a set_deadline may pin its own working calendar (`calendarId`). Calendars are
// not packaged — the id is a row of the SOURCE tenant — so the pin follows the date_offset timer's
// pin exactly: a pack authored with one installs it (the target tenant's save checks it exists),
// export drops a rule carrying one WITH a warning rather than ship an id the target cannot resolve,
// and the install's "no default calendar" warning (#82) does not name a rule that never reads the
// default.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// A pin whose calendar no longer exists in the source tenant is the one case still dropped (#720:
// calendars now travel by name, so a pin that resolves is exported with its calendar).
func TestExport_SetDeadlinePinnedToAMissingCalendarIsDroppedWithAWarning(t *testing.T) {
	ctx := context.Background()
	meta := &fakeMintMeta{}
	if _, _, err := meta.CreateEntity(ctx, "tenant-1", services.CreateEntityInput{Name: "tk_tickets", Label: "Phiếu"}); err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	const pinned, unpinned = "Hạn SLA theo lịch trực 24/7", "Hạn SLA theo lịch mặc định"
	src := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{
		{ID: "wf-1", TenantID: "tenant-1", Name: pinned, EntityName: "tk_tickets", Trigger: "after_create", IsActive: true,
			Actions: json.RawMessage(`[{"type":"set_deadline","field":"due_date","offset":{"amount":4,"unit":"business_hours"},"calendarId":"cal-247"}]`)},
		{ID: "wf-2", TenantID: "tenant-1", Name: unpinned, EntityName: "tk_tickets", Trigger: "after_create", IsActive: true,
			Actions: json.RawMessage(`[{"type":"set_deadline","field":"due_date","offset":{"amount":4,"unit":"business_hours"}}]`)},
	}}
	pack, warnings, err := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: src, CalendarStore: &fakeCalendarStore{}}).
		Export(ctx, "tenant-1", []string{"tk_tickets"}, false, nil)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack.Workflows) != 1 || pack.Workflows[0].Name != unpinned {
		t.Fatalf("exported workflows = %+v; want only %q", pack.Workflows, unpinned)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, pinned) && strings.Contains(w, "lịch") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings %q; want one naming %q", warnings, pinned)
	}
}

func TestInstall_SetDeadlineCalendarPinTravels(t *testing.T) {
	pack := businessHoursPack("business_hours")
	pack.Workflows[0].Actions[0].CalendarID = "cal-own"

	t.Run("the pin reaches the service input", func(t *testing.T) {
		dst := &fakeWorkflowWriter{}
		svc, _ := newWorkflowTestService(dst)
		if _, err := svc.Install(context.Background(), "tenant-2", "user-2", pack); err != nil {
			t.Fatalf("install: %v", err)
		}
		if len(dst.created) != 1 || len(dst.created[0].Actions) != 1 || dst.created[0].Actions[0].CalendarID != "cal-own" {
			t.Fatalf("installed rule = %+v; the set_deadline lost its calendarId", dst.created)
		}
	})

	t.Run("a pinned deadline is not named in the no-default-calendar warning", func(t *testing.T) {
		meta := &fakeMintMeta{}
		out, err := NewService(Deps{
			EntityReader: meta, RoleReader: fakeMintRoles{}, EntityWriter: meta,
			WorkflowWriter: &fakeWorkflowWriter{}, ScheduleResolver: &fakePackScheduleResolver{},
		}).Install(context.Background(), "tenant-1", "user-1", pack)
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if got := calendarWarnings(out); len(got) != 0 {
			t.Fatalf("warnings = %q; a deadline pinned to its own calendar never reads the default", got)
		}
	})
}
