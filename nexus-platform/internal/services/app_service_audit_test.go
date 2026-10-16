package services

// specs/062-customer-workspace L4 T064 — an app's create / update / nav write / delete leaves an
// __app__ audit row (actor from ctx, diff before/after); nothing is written when the write is refused.
// Also pins the draft/direct classification the main.go wiring of every L4 document service relies on.

import (
	"context"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/metaversion"
)

func TestAppService_WritesEmitAppAudit(t *testing.T) {
	svc, _ := newAppSvcFixture(nil)
	aud := &captureAuditor{}
	svc.WithAuditor(aud, nil)
	ctx := audit.WithActor(context.Background(), "builder-1")

	app, err := svc.CreateApp(ctx, "tenant-a", CreateAppInput{Name: "sales", Label: "Bán hàng", ActorID: "builder-1"})
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if _, err := svc.UpdateApp(ctx, "tenant-a", app.ID, UpdateAppInput{Label: "Kinh doanh", SortOrder: 2}); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	// "thêm mục menu app": the nav is replaced as a whole.
	if _, err := svc.ReplaceNav(ctx, "tenant-a", app.ID, []AppNavItemInput{{ItemType: "entity", RefID: "ent-accounts", Label: "Khách"}}); err != nil {
		t.Fatalf("ReplaceNav: %v", err)
	}
	if err := svc.DeleteApp(ctx, "tenant-a", app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}

	want := []string{audit.ActionCreate, audit.ActionUpdate, audit.ActionUpdate, audit.ActionDelete}
	if len(aud.events) != len(want) {
		t.Fatalf("events = %d; want %d: %+v", len(aud.events), len(want), aud.events)
	}
	for i, e := range aud.events {
		if e.EntityName != audit.AppMeta || e.Action != want[i] || e.TenantID != "tenant-a" || e.ActorUserID != "builder-1" || e.RecordID != app.ID {
			t.Errorf("event %d = %+v; want %s/%s by builder-1 on %s", i, e, audit.AppMeta, want[i], app.ID)
		}
		if e.Diff == nil {
			t.Fatalf("event %d has no diff", i)
		}
	}
	if aud.events[0].Diff.After["name"] != "sales" || aud.events[0].Diff.Before != nil {
		t.Errorf("create diff = %+v; want after-only with the slug", aud.events[0].Diff)
	}
	if aud.events[1].Diff.Before["label"] != "Bán hàng" || aud.events[1].Diff.After["label"] != "Kinh doanh" {
		t.Errorf("update diff = %+v; want label change", aud.events[1].Diff)
	}
	nav, _ := aud.events[2].Diff.After["nav"].([]any)
	if len(nav) != 1 {
		t.Errorf("nav diff = %+v; want the one new menu item on the after side", aud.events[2].Diff)
	}
	if aud.events[3].Diff.After != nil || aud.events[3].Diff.Before["name"] != "sales" {
		t.Errorf("delete diff = %+v; want before-only", aud.events[3].Diff)
	}
}

func TestAppService_RefusedWriteEmitsNothing(t *testing.T) {
	svc, _ := newAppSvcFixture(nil)
	aud := &captureAuditor{}
	svc.WithAuditor(aud, nil)
	ctx := context.Background()
	if _, err := svc.CreateApp(ctx, "tenant-a", CreateAppInput{Name: "bad slug!", Label: "x"}); err == nil {
		t.Fatal("want APP_NAME_INVALID")
	}
	if err := svc.DeleteApp(ctx, "tenant-a", "ghost"); err == nil {
		t.Fatal("want APP_NOT_FOUND")
	}
	if _, err := svc.ReplaceNav(ctx, "tenant-a", "ghost", nil); err == nil {
		t.Fatal("want APP_NOT_FOUND")
	}
	if len(aud.events) != 0 {
		t.Fatalf("events after refused writes = %d; want 0", len(aud.events))
	}
}

// cmd/server/main.go hands the DraftGate to exactly the document services whose type is a draft type
// (workflow, report, dashboard, layout, branding, app) and nil to the rest: a draft-only save writes
// no row, but a direct type's save is LIVE at once and must always be audited. When a type changes
// class (specs/063 S7-4 app), this test goes red and names the wiring to flip.
func TestDocAudit_WiringMatchesDraftClasses(t *testing.T) {
	drafted := map[string]bool{"workflow": true, "report": true, "dashboard": true, "layout": true, "branding": true, "app": true}
	for _, typ := range []string{"workflow", "report", "dashboard", "layout", "branding", "app"} {
		isDraft := metaversion.ClassOf(typ).Kind == metaversion.Draft
		if isDraft != drafted[typ] {
			t.Errorf("%q: ClassOf says draft=%v but cmd/server/main.go wires its audit mode as draft=%v — pass the DraftGate to its WithAuditor (or drop it) and update this table",
				typ, isDraft, drafted[typ])
		}
	}
}

type draftModeStub struct{}

func (draftModeStub) WriteMode(context.Context, string) (metaversion.Mode, error) {
	return metaversion.ModeDraft, nil
}

// specs/063 S7-4: while the builder writes drafts, an app's update and menu save are NOT the customer's
// business yet (no row), but create and delete apply at once and must always be audited.
func TestAppService_DraftModeSkipsOnlyDraftableWrites(t *testing.T) {
	svc, _ := newAppSvcFixture(nil)
	aud := &captureAuditor{}
	svc.WithAuditor(aud, draftModeStub{})
	ctx := context.Background()

	app, err := svc.CreateApp(ctx, "tenant-a", CreateAppInput{Name: "sales", Label: "Sales"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateApp(ctx, "tenant-a", app.ID, UpdateAppInput{Label: "Kinh doanh"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReplaceNav(ctx, "tenant-a", app.ID, []AppNavItemInput{{ItemType: "entity", RefID: "ent-accounts"}}); err != nil {
		t.Fatal(err)
	}
	if len(aud.events) != 1 {
		t.Fatalf("events after create+update+nav in draft mode = %d, want 1 (create only)", len(aud.events))
	}
	if err := svc.DeleteApp(ctx, "tenant-a", app.ID); err != nil {
		t.Fatal(err)
	}
	if len(aud.events) != 2 {
		t.Fatalf("events after delete = %d, want 2 (delete applies at once)", len(aud.events))
	}
}
