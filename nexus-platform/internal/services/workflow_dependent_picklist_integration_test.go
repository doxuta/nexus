//go:build integration

// Queue #229 (specs/052 §6) END-TO-END over a migrated throwaway MySQL: a workflow field_update
// that moves the parent picklist must not leave the old parent's child behind.
//
//	RecordService.Update → AsyncDispatcher → WorkflowRecordMutator.writeFields
//	  → checkDependentPicklists(specs, data, before-image read from MySQL)
//
// Mutation proof: delete the checkDependentPicklists call in writeFields and the "refused" case
// goes red (the record flips to the new parent with the old child).
package services

import (
	"context"
	"strings"
	"testing"
)

func depPicklistFields() []FieldInput {
	return []FieldInput{
		{Name: "note", Label: "Note", FieldType: "text"},
		{Name: "province", Label: "Tỉnh", FieldType: "picklist",
			Options: map[string]any{"values": []any{"Hà Nội", "Đà Nẵng"}}},
		{Name: "district", Label: "Quận", FieldType: "picklist",
			Options: map[string]any{
				"values":           []any{"Ba Đình", "Hải Châu"},
				"controllingField": "province",
				"dependentValues":  map[string]any{"Hà Nội": []any{"Ba Đình"}, "Đà Nẵng": []any{"Hải Châu"}},
			}},
	}
}

func TestWorkflowFieldUpdate_DependentPicklist_Integration(t *testing.T) {
	tests := []struct {
		name       string
		actions    []WorkflowActionInput
		wantStatus string
		wantProv   string
		wantDist   any
	}{
		{name: "refused: parent moved, child left behind",
			actions:    []WorkflowActionInput{{Type: "field_update", Field: "province", Value: "Đà Nẵng"}},
			wantStatus: "failed", wantProv: "Hà Nội", wantDist: "Ba Đình"},
		{name: "ok: child cleared, then parent moved",
			actions: []WorkflowActionInput{
				{Type: "field_update", Field: "district", Value: nil},
				{Type: "field_update", Field: "province", Value: "Đà Nẵng"}},
			wantStatus: "success", wantProv: "Đà Nẵng", wantDist: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := w6Wire(t)
			ctx := context.Background()
			const tenant = o3aTenantA
			const user = "66666666-0000-0000-0000-0000000000d1"
			st.createEntity(t, ctx, tenant, "geo_sites", depPicklistFields())
			if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
				Name: "move-province", EntityName: "geo_sites", Trigger: "after_update", ActorID: user,
				Rules:   []WorkflowRuleInput{{Field: "note", Op: "eq", Value: "go"}},
				Actions: tt.actions,
			}); err != nil {
				t.Fatalf("CreateWorkflow: %v", err)
			}
			id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "geo_sites",
				map[string]any{"note": "x", "province": "Hà Nội", "district": "Ba Đình"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := st.recordSvc.Update(ctx, wfAdmin(user), tenant, "geo_sites", id, map[string]any{"note": "go"}, nil); err != nil {
				t.Fatalf("Update: %v", err)
			}
			st.drain(t)

			rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "geo_sites", id, nil, nil)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if rec.Data["province"] != tt.wantProv || rec.Data["district"] != tt.wantDist {
				t.Errorf("record = %v / %v; want %v / %v", rec.Data["province"], rec.Data["district"], tt.wantProv, tt.wantDist)
			}
			var got *runRow
			for _, r := range st.runRows(t, tenant) {
				if r.Status == tt.wantStatus {
					r := r
					got = &r
				}
			}
			if got == nil {
				t.Fatalf("no %q run; rows = %+v", tt.wantStatus, st.runRows(t, tenant))
			}
			if tt.wantStatus == "failed" && (got.Error == nil || !strings.Contains(*got.Error, "RECORD_DEPENDENT_PICKLIST_INVALID")) {
				t.Errorf("failed run error = %v; want RECORD_DEPENDENT_PICKLIST_INVALID", got.Error)
			}
		})
	}
}
