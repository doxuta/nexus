//go:build integration

// Queue #255 END-TO-END on MySQL: creating a record under a parent that does not allow the child's
// DEFAULT used to 422 under the blank child cell; now the default is skipped and the create is 201.
// Mutation proof: remove the dropDisallowedChildDefaults call in ValidateRecord and the "skipped"
// cases go red with RECORD_DEPENDENT_PICKLIST_INVALID.
package services

import (
	"context"
	"testing"
)

func TestDependentPicklist_ChildDefaultSkip_Integration(t *testing.T) {
	st := frWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const user = "66666666-0000-0000-0000-0000000000d2"
	st.createEntity(t, ctx, tenant, "geo_default_sites", depPicklistFields())
	var districtID string
	if err := st.sdb.Get(&districtID, "SELECT id FROM sys_fields WHERE tenant_id = ? AND name = 'district'", tenant); err != nil {
		t.Fatalf("district field id: %v", err)
	}
	if _, err := st.fieldSvc.EditField(ctx, tenant, EditFieldInput{FieldID: districtID, DefaultVal: strPtr("Ba Đình")}); err != nil {
		t.Fatalf("EditField default: %v", err)
	}

	tests := []struct {
		name     string
		payload  map[string]any
		wantDist any
	}{
		{"parent allows the default: filled", map[string]any{"province": "Hà Nội"}, "Ba Đình"},
		{"parent disallows the default: skipped", map[string]any{"province": "Đà Nẵng"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := st.recordSvc.Create(ctx, wfAdmin(user), tenant, user, "geo_default_sites", tt.payload)
			if err != nil {
				t.Fatalf("Create = %v; want 201", err)
			}
			rec, err := st.recordSvc.Get(ctx, wfAdmin(user), tenant, "geo_default_sites", id, nil, nil)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if rec.Data["district"] != tt.wantDist {
				t.Errorf("district = %v; want %v", rec.Data["district"], tt.wantDist)
			}
		})
	}
}
