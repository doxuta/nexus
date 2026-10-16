package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/metadata"
)

// specs/048 S7a — CSV import writes one "Tạo" audit line per stored row through the durable
// record_import_events job.

// fakeEventsDB is the read side + the audit table of the job: records keyed tenant|id, audit rows
// keyed tenant|id (one per create event written).
type fakeEventsDB struct {
	mu      sync.Mutex
	records map[string]map[string]any
	audited map[string]int
	events  []audit.Event
	failOn  string // record id whose InsertEvent fails once
}

func newFakeEventsDB() *fakeEventsDB {
	return &fakeEventsDB{records: map[string]map[string]any{}, audited: map[string]int{}}
}

func (f *fakeEventsDB) BatchRecordData(_ context.Context, _, tenantID string, ids []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, id := range ids {
		if d, ok := f.records[tenantID+"|"+id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func (f *fakeEventsDB) ExistingCreateIDs(_ context.Context, tenantID, _ string, ids []string) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]struct{}{}
	for _, id := range ids {
		if f.audited[tenantID+"|"+id] > 0 {
			out[id] = struct{}{}
		}
	}
	return out, nil
}

func (f *fakeEventsDB) InsertEvent(_ context.Context, e audit.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && e.RecordID == f.failOn {
		f.failOn = ""
		return errors.New("audit down")
	}
	f.audited[e.TenantID+"|"+e.RecordID]++
	f.events = append(f.events, e)
	return nil
}

func eventsJob(t *testing.T, tenant string, ids ...string) jobs.Job {
	t.Helper()
	b, err := json.Marshal(ImportEventsPayload{ImportID: "imp-1", Chunk: 0, EntityName: "contacts", UserID: "u-importer", RecordIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return jobs.Job{ID: "j1", TenantID: tenant, Type: ImportEventsJobType, Payload: b}
}

func eventsHandler(db *fakeEventsDB, fields ...metadata.FieldDefinition) jobs.HandlerFunc {
	cat := &fakeImportCatalog{entity: &metadata.EntityDefinition{ID: "ent-1", Name: "contacts"}, fields: fields}
	return NewImportEventsJobHandler(cat, db, db, nil)
}

func importNameCSV(t *testing.T, svc *ImportService, csv string) *ImportResult {
	t.Helper()
	res, err := svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u-importer", EntityName: "contacts", IsAdmin: true,
		Mapping: map[string]string{"col_name": "name"}, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return res
}

func TestImport_WritesAuditPerRow_ViaJob(t *testing.T) {
	svc, _ := newImportSvcForTest()
	enq := &fakeJobEnqueuer{}
	svc.WithEventJobs(enq)
	if res := importNameCSV(t, svc, "col_name\nAlice\nBob\nCarol\n"); res.Inserted != 3 {
		t.Fatalf("inserted = %d", res.Inserted)
	}
	if len(enq.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(enq.jobs))
	}
	db := newFakeEventsDB()
	for i := 1; i <= 3; i++ {
		db.records[fmt.Sprintf("t1|rec-%d", i)] = map[string]any{"name": fmt.Sprintf("n%d", i)}
	}
	if err := eventsHandler(db, metadata.FieldDefinition{Name: "name", FieldType: "text"})(context.Background(), enq.jobs[0]); err != nil {
		t.Fatalf("job: %v", err)
	}
	if len(db.events) != 3 {
		t.Fatalf("audit events = %d, want 3", len(db.events))
	}
	for _, e := range db.events {
		if e.Action != audit.ActionCreate || e.ActorUserID != "u-importer" || e.TenantID != "t1" || e.EntityName != "contacts" {
			t.Fatalf("event = %+v", e)
		}
		if e.Diff == nil || e.Diff.Before != nil || e.Diff.After["name"] == nil {
			t.Fatalf("diff must be after-only with the row data: %+v", e.Diff)
		}
	}
}

func TestImportEventsJob_RedactsSensitiveFields(t *testing.T) {
	db := newFakeEventsDB()
	db.records["t1|r1"] = map[string]any{"name": "A", "so_cccd": "001099001122"}
	h := eventsHandler(db,
		metadata.FieldDefinition{Name: "name", FieldType: "text"},
		metadata.FieldDefinition{Name: "so_cccd", FieldType: "text", Options: strPtr(`{"sensitive":true}`)})
	if err := h(context.Background(), eventsJob(t, "t1", "r1")); err != nil {
		t.Fatal(err)
	}
	if len(db.events) != 1 {
		t.Fatalf("events = %d", len(db.events))
	}
	d := db.events[0].Diff
	if d.After["so_cccd"] != auditRedactedMarker || d.After["name"] != "A" {
		t.Fatalf("after = %+v", d.After)
	}
	if b, _ := json.Marshal(db.events[0]); strings.Contains(string(b), "001099001122") {
		t.Fatalf("sensitive value leaked: %s", b)
	}
}

func TestImport_EnqueuesChunkedJobs(t *testing.T) {
	svc, _ := newImportSvcForTest()
	enq := &fakeJobEnqueuer{}
	svc.WithEventJobs(enq)
	var b strings.Builder
	b.WriteString("col_name\n")
	for i := 0; i < 1200; i++ {
		fmt.Fprintf(&b, "Name %d\n", i)
	}
	if res := importNameCSV(t, svc, b.String()); res.Inserted != 1200 {
		t.Fatalf("inserted = %d", res.Inserted)
	}
	if len(enq.jobs) != 3 {
		t.Fatalf("jobs = %d, want 3", len(enq.jobs))
	}
	seen := map[int]bool{}
	total := 0
	for _, j := range enq.jobs {
		if j.Type != ImportEventsJobType || j.TenantID != "t1" {
			t.Fatalf("job = %+v", j)
		}
		if strings.Contains(string(j.Payload), "Name ") {
			t.Fatalf("payload carries field values: %s", j.Payload)
		}
		var p ImportEventsPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.ImportID == "" || p.UserID != "u-importer" || len(p.RecordIDs) > ImportEventsChunkSize {
			t.Fatalf("payload = %+v", p)
		}
		seen[p.Chunk] = true
		total += len(p.RecordIDs)
	}
	if !seen[0] || !seen[1] || !seen[2] || total != 1200 {
		t.Fatalf("chunks seen = %v total = %d", seen, total)
	}
}

func TestImport_EnqueueFailureDoesNotFailCommittedImport(t *testing.T) {
	svc, _ := newImportSvcForTest()
	svc.WithEventJobs(&fakeJobEnqueuer{err: errors.New("queue down")})
	res := importNameCSV(t, svc, "col_name\nAlice\n")
	if res.Inserted != 1 || res.AuditJobsFailed != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestImportEventsJob_IdempotentReplay(t *testing.T) {
	db := newFakeEventsDB()
	for _, id := range []string{"r1", "r2", "r3"} {
		db.records["t1|"+id] = map[string]any{"name": id}
	}
	h := eventsHandler(db, metadata.FieldDefinition{Name: "name", FieldType: "text"})
	job := eventsJob(t, "t1", "r1", "r2", "r3")

	// The job dies on r2 (r1 already written): the failure surfaces so the queue retries it.
	db.failOn = "r2"
	if err := h(context.Background(), job); err == nil {
		t.Fatal("want the audit failure to fail the job so it is retried")
	}
	if err := h(context.Background(), job); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if err := h(context.Background(), job); err != nil { // full replay
		t.Fatalf("replay: %v", err)
	}
	for _, id := range []string{"r1", "r2", "r3"} {
		if n := db.audited["t1|"+id]; n != 1 {
			t.Fatalf("%s audited %d times, want exactly 1", id, n)
		}
	}
}

func TestImportEventsJob_TenantPinned(t *testing.T) {
	db := newFakeEventsDB()
	db.records["t1|r1"] = map[string]any{"name": "secret of t1"}
	h := eventsHandler(db, metadata.FieldDefinition{Name: "name", FieldType: "text"})
	if err := h(context.Background(), eventsJob(t, "t2", "r1")); err != nil {
		t.Fatal(err)
	}
	if len(db.events) != 0 {
		t.Fatalf("a job of tenant t2 audited a record of t1: %+v", db.events)
	}
}
