package services

// Queue #229 (specs/052 §6): a workflow field_update used to judge ONE field alone, so a rule that
// moved Tỉnh left the Quận of the old province behind and nobody was told. The write now judges the
// pair it forms against the record's before-image (checkDependentPicklists), like a user's PUT.

import (
	"context"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

func geoCatalog() *relCatalog {
	return &relCatalog{
		entities: map[string]*metadata.EntityDefinition{"sites": {ID: "ent-site", TenantID: "t1", Name: "sites"}},
		fields: map[string][]metadata.FieldDefinition{"ent-site": {
			{Name: "province", FieldType: "picklist", Options: strOpt(`{"values":["Hà Nội","Đà Nẵng"]}`)},
			{Name: "district", FieldType: "picklist", Options: strOpt(
				`{"values":["Ba Đình","Hải Châu"],"controllingField":"province","dependentValues":{"Hà Nội":["Ba Đình"],"Đà Nẵng":["Hải Châu"]}}`)},
			{Name: "note", FieldType: "text"},
		}},
	}
}

func TestWorkflowFieldUpdate_DependentPicklistPair(t *testing.T) {
	tests := []struct {
		name    string
		fields  map[string]any
		wantErr bool
	}{
		{name: "parent moved, child left behind", fields: map[string]any{"province": "Đà Nẵng"}, wantErr: true},
		{name: "child outside the stored parent's list", fields: map[string]any{"district": "Hải Châu"}, wantErr: true},
		{name: "right pair written together", fields: map[string]any{"province": "Đà Nẵng", "district": "Hải Châu"}},
		{name: "unrelated field is not judged", fields: map[string]any{"note": "x"}},
		{name: "parent resent unchanged", fields: map[string]any{"province": "Hà Nội"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &trailStore{before: map[string]any{"province": "Hà Nội", "district": "Ba Đình"}}
			mut := NewWorkflowRecordMutator(store).WithCatalog(geoCatalog())
			err := mut.UpdateFieldsIfVersion(context.Background(), "sites", "t1", "rec-1", workflow.SystemActorID, tt.fields, 1)
			if tt.wantErr {
				relWantCode(t, err, codeRecordDependentInvalid)
				if store.written != nil {
					t.Fatalf("a refused pair was written: %v", store.written)
				}
				return
			}
			if err != nil {
				t.Fatalf("write = %v; want ok", err)
			}
		})
	}
}
