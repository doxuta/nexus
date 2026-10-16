package ai

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// Queue #230 (specs/052): the AI Builder knows the dependent-picklist keys and judges the pair at
// PREVIEW time (propose_field / propose_edit_field / propose_record_update), not at Apply.

func TestToolContract_OptionsDescribeDependentPicklist(t *testing.T) {
	d, _ := fieldOptionsSchema()["description"].(string)
	for _, k := range []string{"controllingField", "dependentValues"} {
		if !strings.Contains(d, k) {
			t.Fatalf("fieldOptionsSchema description must teach %q", k)
		}
	}
}

func TestTools_ProposeField_DependentPicklistPreview(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"valid pair → buffered", `{"entity":"tickets","name":"sub","label":"Nhóm con","fieldType":"picklist",
			"options":{"values":["a","b"],"controllingField":"status","dependentValues":{"open":["a"],"closed":["b"]}}}`, ""},
		{"map key not a parent value", `{"entity":"tickets","name":"sub","label":"Nhóm con","fieldType":"picklist",
			"options":{"values":["a"],"controllingField":"status","dependentValues":{"nope":["a"]}}}`, "FIELD_DEPENDENT_PICKLIST_INVALID"},
		{"controlling field missing", `{"entity":"tickets","name":"sub","label":"Nhóm con","fieldType":"picklist",
			"options":{"values":["a"],"controllingField":"ghost","dependentValues":{"open":["a"]}}}`, "FIELD_DEPENDENT_PICKLIST_INVALID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			res := e.execute(context.Background(), "propose_field", tc.args)
			ok, _ := parseToolResult(t, res)
			if tc.wantCode == "" {
				if !ok || len(e.draft) != 1 {
					t.Fatalf("want buffered, got %s", res)
				}
				return
			}
			if ok || !strings.Contains(res, tc.wantCode) || len(e.draft) != 0 {
				t.Fatalf("want %s and no draft, got %s (draft %d)", tc.wantCode, res, len(e.draft))
			}
		})
	}
}

// The parent may be a field drafted earlier THIS turn (propose_entity before propose_field).
func TestTools_ProposeField_DependentParentFromSameTurnDraft(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	res := e.execute(context.Background(), "propose_entity", `{"name":"shops","label":"Cửa hàng","fields":[
		{"name":"province","label":"Tỉnh","fieldType":"picklist","options":{"values":["hn","hcm"]}}]}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("propose_entity: %s", res)
	}
	res = e.execute(context.Background(), "propose_field", `{"entity":"shops","name":"district","label":"Quận","fieldType":"picklist",
		"options":{"values":["q1"],"controllingField":"province","dependentValues":{"hcm":["q1"]}}}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("drafted parent must satisfy the pair check: %s", res)
	}
	res = e.execute(context.Background(), "propose_field", `{"entity":"shops","name":"ward","label":"Phường","fieldType":"picklist",
		"options":{"values":["p1"],"controllingField":"province","dependentValues":{"zz":["p1"]}}}`)
	if ok, _ := parseToolResult(t, res); ok || !strings.Contains(res, "FIELD_DEPENDENT_PICKLIST_INVALID") {
		t.Fatalf("bad key against drafted parent must fail at preview: %s", res)
	}
}

func TestTools_ProposeEditField_DependentPicklistPreview(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	// status is a picklist; point it at a text sibling (subject) → not a picklist.
	res := e.execute(context.Background(), "propose_edit_field", `{"entity":"tickets","field":"status","changes":{
		"options":{"values":["open","closed"],"controllingField":"subject","dependentValues":{"x":["open"]}}}}`)
	if ok, _ := parseToolResult(t, res); ok || !strings.Contains(res, "FIELD_DEPENDENT_PICKLIST_INVALID") || len(e.draft) != 0 {
		t.Fatalf("want FIELD_DEPENDENT_PICKLIST_INVALID at preview, got %s", res)
	}
}

func TestAssistantTools_ProposeRecordUpdate_DependentPairPreview(t *testing.T) {
	prov := `{"values":["hn","hcm"]}`
	dist := `{"values":["bd","q1"],"controllingField":"province","dependentValues":{"hn":["bd"],"hcm":["q1"]}}`
	fields := append(ticketFields(), mkField("province", "picklist", prov), mkField("district", "picklist", dist))
	rd := &fakeRecordReader{fields: fields,
		getRec: &dynrepo.Record{ID: "r-1", Data: map[string]any{"title": "x", "province": "hn", "district": "bd"}}}
	e := &assistantExecutor{reader: rd, tenantID: "t1", scope: callerScope()}

	res := e.execute(context.Background(), "propose_record_update", `{"entity":"tickets","recordId":"r-1","data":{"district":"q1"}}`)
	if ok, _ := parseToolResult(t, res); ok || !strings.Contains(res, "RECORD_DEPENDENT_PICKLIST_INVALID") || len(e.draft) != 0 {
		t.Fatalf("q1 under province hn must be refused at preview: %s", res)
	}
	res = e.execute(context.Background(), "propose_record_update", `{"entity":"tickets","recordId":"r-1","data":{"province":"hcm","district":"q1"}}`)
	if ok, _ := parseToolResult(t, res); !ok {
		t.Fatalf("a valid pair must buffer: %s", res)
	}
}

func mkField(name, typ, opts string) metadata.FieldDefinition {
	return metadata.FieldDefinition{Name: name, Label: name, FieldType: typ, Options: &opts}
}
