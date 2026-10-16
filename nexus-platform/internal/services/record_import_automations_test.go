package services

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// specs/048 S7b — the import job fires the record's after_create automations (workflow + BPM) BEFORE
// it writes the audit line that marks the record handled.

type recWFEmitter struct {
	mu     sync.Mutex
	events []workflow.Event
	failOn string // record id whose EmitWait fails once
}

func (r *recWFEmitter) EmitWait(_ context.Context, e workflow.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn != "" && e.RecordID == r.failOn {
		r.failOn = ""
		return errors.New("buffer full")
	}
	r.events = append(r.events, e)
	return nil
}

type recBPMEmitter struct {
	mu     sync.Mutex
	events []bpm.Event
}

func (r *recBPMEmitter) EmitWait(_ context.Context, e bpm.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func automationJob(t *testing.T, run bool, ids ...string) jobs.Job {
	t.Helper()
	b, err := json.Marshal(ImportEventsPayload{ImportID: "imp-1", EntityName: "contacts", UserID: "u-importer", RecordIDs: ids, RunAutomations: run})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{ID: "j1", TenantID: "t1", Type: ImportEventsJobType, Payload: b}
}

func automationHandler(db *fakeEventsDB, wf *recWFEmitter, bp *recBPMEmitter) jobs.HandlerFunc {
	cat := &fakeImportCatalog{entity: &metadata.EntityDefinition{ID: "ent-1", Name: "contacts"}, fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}}}
	return NewImportEventsJobHandler(cat, db, db, nil, ImportAutomation{Workflow: wf, BPM: bp})
}

func TestImportEventsJob_EmitBeforeAuditMarker(t *testing.T) {
	db := newFakeEventsDB()
	for _, id := range []string{"r1", "r2", "r3"} {
		db.records["t1|"+id] = map[string]any{"name": id}
	}
	wf, bp := &recWFEmitter{failOn: "r2"}, &recBPMEmitter{}
	h := automationHandler(db, wf, bp)
	job := automationJob(t, true, "r1", "r2", "r3")

	if err := h(context.Background(), job); err == nil {
		t.Fatal("want the EmitWait failure to fail the job so it is retried")
	}
	if n := db.audited["t1|r2"]; n != 0 {
		t.Fatalf("r2 audited %d times although its emit failed: the marker was written before the emit", n)
	}
	if n := db.audited["t1|r1"]; n != 1 {
		t.Fatalf("r1 audited %d times, want 1", n)
	}

	if err := h(context.Background(), job); err != nil {
		t.Fatalf("retry: %v", err)
	}
	count := map[string]int{}
	for _, e := range wf.events {
		count[e.RecordID]++
		if e.Trigger != workflow.TriggerAfterCreate || e.ActorUserID != "u-importer" || e.OwnerUserID != "u-importer" ||
			e.Depth != 0 || e.TenantID != "t1" || e.EntityName != "contacts" || e.Data["name"] != e.RecordID {
			t.Fatalf("workflow event = %+v", e)
		}
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		if count[id] != 1 {
			t.Fatalf("%s fired %d times, want exactly 1 (already-handled records must not re-fire)", id, count[id])
		}
		if db.audited["t1|"+id] != 1 {
			t.Fatalf("%s audited %d times, want 1", id, db.audited["t1|"+id])
		}
	}
	if len(bp.events) != 3 || bp.events[0].Trigger != bpm.TriggerAfterCreate || bp.events[0].Depth != 0 {
		t.Fatalf("bpm events = %+v, want 3 after_create at depth 0", bp.events)
	}
}

func TestImportEventsJob_AutomationsOffEmitsNothing(t *testing.T) {
	db := newFakeEventsDB()
	db.records["t1|r1"] = map[string]any{"name": "r1"}
	wf, bp := &recWFEmitter{}, &recBPMEmitter{}
	if err := automationHandler(db, wf, bp)(context.Background(), automationJob(t, false, "r1")); err != nil {
		t.Fatal(err)
	}
	if len(wf.events)+len(bp.events) != 0 || db.audited["t1|r1"] != 1 {
		t.Fatalf("emitted %d/%d events, audited %d: want 0 events and 1 audit line", len(wf.events), len(bp.events), db.audited["t1|r1"])
	}
}

func TestImport_RunAutomationsInPayload(t *testing.T) {
	for _, run := range []bool{true, false} {
		svc, _ := newImportSvcForTest()
		enq := &fakeJobEnqueuer{}
		svc.WithEventJobs(enq)
		csv := "col_name\nAlice\n"
		if _, err := svc.Import(context.Background(), ImportInput{
			TenantID: "t1", UserID: "u-importer", EntityName: "contacts", IsAdmin: true, RunAutomations: run,
			Mapping: map[string]string{"col_name": "name"}, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
		}); err != nil {
			t.Fatal(err)
		}
		var p ImportEventsPayload
		if err := json.Unmarshal(enq.jobs[0].Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.RunAutomations != run {
			t.Fatalf("payload runAutomations = %v, want %v", p.RunAutomations, run)
		}
	}
}

type failBPMEmitter struct{ n int }

func (f *failBPMEmitter) EmitWait(context.Context, bpm.Event) error {
	f.n++
	if f.n == 1 {
		return errors.New("approval queue busy")
	}
	return nil
}

// Pins FR-035 at-least-once: workflow fired, BPM failed => job retried => workflow fires AGAIN.
func TestImportEventsJob_BPMFailureRefiresWorkflow(t *testing.T) {
	db := newFakeEventsDB()
	db.records["t1|r1"] = map[string]any{"name": "r1"}
	wf, bp := &recWFEmitter{}, &failBPMEmitter{}
	cat := &fakeImportCatalog{entity: &metadata.EntityDefinition{ID: "ent-1", Name: "contacts"}, fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}}}
	h := NewImportEventsJobHandler(cat, db, db, nil, ImportAutomation{Workflow: wf, BPM: bp})
	job := automationJob(t, true, "r1")
	if err := h(context.Background(), job); err == nil {
		t.Fatal("want the BPM failure to fail the job")
	}
	if err := h(context.Background(), job); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(wf.events) != 2 || db.audited["t1|r1"] != 1 {
		t.Fatalf("workflow fired %d times, audited %d: want 2 (documented duplicate) and 1", len(wf.events), db.audited["t1|r1"])
	}
}

func TestImportEventsJob_UnwiredConsumerIsPermanentAndUnmarked(t *testing.T) {
	db := newFakeEventsDB()
	db.records["t1|r1"] = map[string]any{"name": "r1"}
	cat := &fakeImportCatalog{entity: &metadata.EntityDefinition{ID: "ent-1", Name: "contacts"}, fields: []metadata.FieldDefinition{{Name: "name", FieldType: "text"}}}
	err := NewImportEventsJobHandler(cat, db, db, nil)(context.Background(), automationJob(t, true, "r1"))
	if err == nil || !jobs.IsPermanent(err) || db.audited["t1|r1"] != 0 {
		t.Fatalf("err=%v audited=%d: want a permanent error and no handled mark", err, db.audited["t1|r1"])
	}
}

func TestImport_RunAutomationsWithoutQueueIs422(t *testing.T) {
	svc, _ := newImportSvcForTest()
	csv := "col_name\nAlice\n"
	_, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u", EntityName: "contacts", IsAdmin: true, RunAutomations: true,
		Mapping: map[string]string{"col_name": "name"}, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "IMPORT_AUTOMATIONS_UNAVAILABLE" {
		t.Fatalf("err = %v, want IMPORT_AUTOMATIONS_UNAVAILABLE", err)
	}
}
