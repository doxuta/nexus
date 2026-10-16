//go:build integration

// specs/048 S7b — CSV import with runAutomations fires the SAME after_create workflow rules a
// hand-made create does, end-to-end on a real MySQL: Import → job payload → record_import_events
// handler → workflow.AsyncDispatcher.EmitWait → field_update written to the row. Without the
// flag the rule never fires; both paths leave exactly one history line per row.
//
//	TEST_DATABASE_URL=... go test -tags=integration ./internal/services/ -run TestImport_RunAutomations
package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/bpm"
)

func TestImport_RunAutomations_FiresRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  bool
		want string // owner_note on every imported row ("" = the rule must not have fired)
	}{
		{"runAutomations=true fires the rule", true, "routed"},
		{"runAutomations=false does not", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := frWire(t)
			ctx := context.Background()
			const tenant = o3aTenantA
			const user = "cccc1111-0000-0000-0000-0000000000a1"

			st.createEntity(t, ctx, tenant, "import_leads", []FieldInput{
				{Name: "name", Label: "Name", FieldType: "text"},
				{Name: "owner_note", Label: "Owner note", FieldType: "text"},
			})
			if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
				Name: "route-new", EntityName: "import_leads", Trigger: "after_create",
				Actions: []WorkflowActionInput{{Type: "field_update", Field: "owner_note", Value: "routed"}},
				ActorID: user,
			}); err != nil {
				t.Fatalf("CreateWorkflow: %v", err)
			}

			enq := &fakeJobEnqueuer{}
			svc := NewImportService(st.meta, st.dyn, nil).WithEventJobs(enq)
			csv := "col_name\nAn\nBinh\nCuong\n"
			res, err := svc.Import(ctx, ImportInput{
				TenantID: tenant, UserID: user, EntityName: "import_leads", IsAdmin: true, RunAutomations: tc.run,
				Mapping: map[string]string{"col_name": "name"}, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
			})
			if err != nil || res.Inserted != 3 {
				t.Fatalf("Import: res=%+v err=%v", res, err)
			}
			if len(enq.jobs) != 1 {
				t.Fatalf("jobs = %d, want 1", len(enq.jobs))
			}

			auditRepo := audit.NewSQLRepository(st.sdb)
			bpmEng := bpm.NewAsyncEngine(bpm.EngineConfig{}, nil, nil, nil, nil)
			h := NewImportEventsJobHandler(st.meta, st.dyn, auditRepo, nil, ImportAutomation{Workflow: st.dispatcher, BPM: bpmEng})
			job := enq.jobs[0]
			job.ID = "j-it"
			if err := h(ctx, job); err != nil {
				t.Fatalf("job: %v", err)
			}
			st.drain(t) // the dispatcher worker has applied every field_update

			var p ImportEventsPayload
			if err := json.Unmarshal(job.Payload, &p); err != nil {
				t.Fatal(err)
			}
			rows, err := st.dyn.BatchRecordData(ctx, "import_leads", tenant, p.RecordIDs)
			if err != nil || len(rows) != 3 {
				t.Fatalf("BatchRecordData: %d rows, err %v", len(rows), err)
			}
			for id, row := range rows {
				got, _ := row["owner_note"].(string)
				if got != tc.want {
					t.Errorf("row %s owner_note = %q, want %q", id, got, tc.want)
				}
			}
			done, err := auditRepo.ExistingCreateIDs(ctx, tenant, "import_leads", p.RecordIDs)
			if err != nil || len(done) != 3 {
				t.Fatalf("history lines: %d, err %v; want one per row", len(done), err)
			}
		})
	}
}
