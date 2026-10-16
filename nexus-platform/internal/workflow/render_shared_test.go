package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// DAILY-QUEUE #335: create_record values and in_app_notification title/message render through the
// SAME resolver seam as email_alert, so {{benh_nhan.ten}} means the linked record's field everywhere.

func TestRunAction_InAppNotification_RendersLinkedFields(t *testing.T) {
	notif := &fakeNotif{}
	res := &fakeTemplateResolver{values: map[string]any{"benh_nhan.ten": "Nguyễn Thị Lan"}}
	a := Action{Type: ActionInAppNotification, UserID: "u1", Title: "Hẹn của {{benh_nhan.ten}}", Message: "{{gio}} — {{benh_nhan.ten}}"}
	evt := Event{TenantID: "t1", EntityName: "lich_hen", Data: map[string]any{"benh_nhan": "p-1", "gio": "09:00"}}

	if _, err := runAction(context.Background(), Workflow{ID: "w1"}, evt, a, 0, actionDeps{notif: notif, templates: res, logger: zap.NewNop()}); err != nil {
		t.Fatalf("runAction: %v", err)
	}
	if len(notif.rows) != 1 || notif.rows[0].Title != "Hẹn của Nguyễn Thị Lan" || notif.rows[0].Message != "09:00 — Nguyễn Thị Lan" {
		t.Fatalf("notification = %+v", notif.rows)
	}
	if len(res.calls) != 1 || res.calls[0].tenant != "t1" || res.calls[0].entity != "lich_hen" {
		t.Errorf("resolver calls = %+v; want ONE call pinned to the event tenant/entity", res.calls)
	}
}

func TestRunAction_InAppNotification_ResolverFailureFailsTheAction(t *testing.T) {
	notif := &fakeNotif{}
	res := &fakeTemplateResolver{err: errors.New("boom")}
	a := Action{Type: ActionInAppNotification, UserID: "u1", Title: "{{benh_nhan.ten}}"}
	_, err := runAction(context.Background(), Workflow{}, Event{TenantID: "t1", Data: map[string]any{}}, a, 0, actionDeps{notif: notif, templates: res})
	if err == nil || notif.calls != 0 {
		t.Fatalf("err=%v inserts=%d; want a failed action and no row with holes in it", err, notif.calls)
	}
}

func TestRunAction_CreateRecord_RendersLinkedFields(t *testing.T) {
	creator := &crFakeCreator{t: t, ceiling: 10}
	res := &fakeTemplateResolver{values: map[string]any{"benh_nhan.ten": "Lan"}}
	a := Action{Type: ActionCreateRecord, CreateEntity: "nhac_viec", Values: map[string]any{
		"tieu_de":  "Gọi lại {{benh_nhan.ten}} ({{gio}})",
		"nguoi":    OwnerSentinel,
		"so_luong": float64(3),
	}}
	evt := Event{TenantID: "t1", EntityName: "lich_hen", OwnerUserID: "o1", Data: map[string]any{"benh_nhan": "p-1", "gio": "09:00"}}
	deps := actionDeps{creator: creator, maxDepth: 5, templates: res, logger: zap.NewNop()}

	if _, err := runAction(context.Background(), Workflow{}, evt, a, 0, deps); err != nil {
		t.Fatalf("runAction: %v", err)
	}
	got := creator.lastValues
	if got["tieu_de"] != "Gọi lại Lan (09:00)" || got["nguoi"] != "o1" || got["so_luong"] != float64(3) {
		t.Fatalf("values = %+v", got)
	}
	if len(res.calls) != 1 || len(res.calls[0].names) != 1 || res.calls[0].names[0] != "benh_nhan.ten" {
		t.Errorf("resolver asked %+v; want only the unanswered placeholder, never a sentinel", res.calls)
	}
}

func TestRunAction_CreateRecord_ResolverFailureCreatesNothing(t *testing.T) {
	creator := &crFakeCreator{t: t, ceiling: 10}
	res := &fakeTemplateResolver{err: errors.New("boom")}
	a := Action{Type: ActionCreateRecord, CreateEntity: "x", Values: map[string]any{"a": "{{benh_nhan.ten}}"}}
	_, err := runAction(context.Background(), Workflow{}, Event{TenantID: "t1", Data: map[string]any{}}, a, 0, actionDeps{creator: creator, maxDepth: 5, templates: res})
	if err == nil || !strings.Contains(err.Error(), "boom") || creator.created != 0 {
		t.Fatalf("err=%v created=%d; want a failed run and no half-filled record", err, creator.created)
	}
}

func TestRenderData_DoesNotMutateEventData(t *testing.T) {
	res := &fakeTemplateResolver{values: map[string]any{"a.b": "v"}}
	evt := Event{Data: map[string]any{"x": 1}}
	got, err := RenderData(context.Background(), evt, res, "{{a.b}}")
	if err != nil || got["a.b"] != "v" {
		t.Fatalf("RenderData = %v, %v", got, err)
	}
	if _, leaked := evt.Data["a.b"]; leaked {
		t.Fatal("resolved value leaked into the shared evt.Data")
	}
}
