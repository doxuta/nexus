package ai

// #30 items 3 and 6, and two LOWs from PR #39's verifier: the field and table names a propose tool
// judges are the names the apply path will judge — a field name the table already has is refused
// now, the reserved names are in the tool contract, and every draft-aware lookup reads the live
// registry and the draft under ONE spelling (canonicalEntityName), live first.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/services"
)

func toolDescription(t *testing.T, name string) string {
	t.Helper()
	for _, tool := range ToolDefinitions() {
		if tool.Function.Name == name {
			return tool.Function.Description
		}
	}
	t.Fatalf("tool %s not found", name)
	return ""
}

// #30 item 3: the same field twice in one draft cost a bench attempt 11 model calls and then failed
// at Apply ("Trường đã tồn tại"). propose_field refuses a name the table already has — live, drafted
// with the table, or proposed earlier this turn — as propose_entity refuses a table name.
func TestTools_ProposeField_RefusesANameTheTableAlreadyHas(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	phone := `{"entity":"patients","name":"phone","label":"SĐT","fieldType":"text"}`
	mustBuffer(t, e, "propose_field", phone)
	mustReject(t, e, "propose_field", phone, "FIELD_ALREADY_EXISTS") // twice in one draft
	mustReject(t, e, "propose_field", `{"entity":"patients","name":"phone","label":"Điện thoại","fieldType":"email"}`, "FIELD_ALREADY_EXISTS")
	mustReject(t, e, "propose_field", `{"entity":"patients","name":"full_name","label":"Tên","fieldType":"text"}`, "FIELD_ALREADY_EXISTS") // live
	mustBuffer(t, e, "propose_field", `{"entity":"treatments","name":"phone","label":"SĐT","fieldType":"text"}`)                           // another table's name is free

	mustBuffer(t, e, "propose_entity", `{"name":"shops","label":"Cửa hàng","fields":[{"name":"province","label":"Tỉnh","fieldType":"text"}]}`)
	mustReject(t, e, "propose_field", `{"entity":"shops","name":"province","label":"Tỉnh","fieldType":"text"}`, "FIELD_ALREADY_EXISTS") // drafted with its table
	mustBuffer(t, e, "propose_field", `{"entity":"shops","name":"district","label":"Quận","fieldType":"text"}`)
	mustReject(t, e, "propose_field", `{"entity":"shops","name":"district","label":"Quận","fieldType":"text"}`, "FIELD_ALREADY_EXISTS")
}

// A live table wins over a draft of the same name (PR #21): an older thread's proposal of "patients"
// with a "note" field neither blocks a real "note" nor hides the live "full_name".
func TestTools_ProposeField_LiveTableWinsOverADraftOfTheSameName(t *testing.T) {
	e := &toolExecutor{reader: twoTenantRelReader(), tenantID: "tenant-A"}
	e.draft = append(e.draft, DraftItem{Kind: "entity", Entity: &DraftEntity{Name: "patients", Label: "Cũ",
		Fields: []DraftField{{Name: "note", Label: "Ghi chú", FieldType: "text"}}}})
	mustBuffer(t, e, "propose_field", `{"entity":"patients","name":"note","label":"Ghi chú","fieldType":"text"}`)
	mustReject(t, e, "propose_field", `{"entity":"patients","name":"full_name","label":"Tên","fieldType":"text"}`, "FIELD_ALREADY_EXISTS")
}

// #30 item 6: six bench attempts named a person field `owner` and retried on FIELD_NAME_RESERVED.
// Both field tools list the reserved names, generated from ddl.MandatoryColumns (the list
// ddl.IsReservedColumn reads). Pinned both ways: every name the validators refuse as reserved is
// listed, and every listed name is refused.
func TestFieldTools_DescribeEveryReservedName(t *testing.T) {
	descs := map[string]string{"propose_entity": toolDescription(t, "propose_entity"), "propose_field": toolDescription(t, "propose_field")}
	options := map[string]map[string]any{"text": nil, "user": nil,
		"relation": {"relatedEntity": "patients"}, "polymorphic_relation": {"targetEntities": []any{"patients"}}}
	refused := map[string]bool{}
	for _, c := range ddl.MandatoryColumns {
		for _, name := range []string{c.Name, strings.TrimSuffix(c.Name, "_id"), strings.TrimSuffix(c.Name, "_type")} {
			for fieldType, opts := range options {
				f := services.FieldInput{Name: name, Label: "x", FieldType: fieldType, Options: opts}
				for _, err := range []error{
					services.ValidateCreateEntity(services.CreateEntityInput{Name: "things", Label: "x", Fields: []services.FieldInput{f}}),
					services.ValidateAddField(services.AddFieldInput{EntityName: "things", Field: f}),
				} {
					if code, _, _ := validationOf(err); code == "FIELD_NAME_RESERVED" {
						refused[name] = true
					}
				}
			}
		}
	}
	if !refused["owner"] || !refused["owner_id"] {
		t.Fatalf("fixture: a user field named owner (it derives owner_id) must be refused; refused = %v", refused)
	}
	for name := range refused {
		for tool, desc := range descs {
			if !strings.Contains(desc, "`"+name+"`") {
				t.Errorf("%s description does not list the reserved name %q", tool, name)
			}
		}
	}
	listed := regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(reservedFieldNamesNote(), -1)
	if len(listed) == 0 {
		t.Fatal("reservedFieldNamesNote lists no name")
	}
	for _, m := range listed {
		if !refused[m[1]] {
			t.Errorf("the note lists %q, which no validator refuses as reserved", m[1])
		}
	}
}

// The same LOW in checkAddFieldRefs (propose_field's dependent-picklist check): it read the live
// table with the raw name, and took siblings from every draft of the name even when the table is
// live, so an older draft's picklist could vouch for a parent the live table does not have.
func TestTools_CheckAddFieldRefs_LiveTableWinsOverADraftOfTheSameName(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	e.draft = append(e.draft, DraftItem{Kind: "entity", Entity: &DraftEntity{Name: "tickets", Label: "Cũ",
		Fields: []DraftField{{Name: "tier", Label: "Hạng", FieldType: "picklist", Options: map[string]any{"values": []any{"gold"}}}}}})
	child := func(parent, key string) services.FieldInput {
		return services.FieldInput{Name: "sub", Label: "Nhóm con", FieldType: "picklist", Options: map[string]any{
			"values": []any{"a"}, "controllingField": parent, "dependentValues": map[string]any{key: []any{"a"}}}}
	}
	for _, ref := range []string{"tickets", " tickets", "Tickets", " tickets"} {
		if err := e.checkAddFieldRefs(context.Background(), ref, child("status", "open")); err != nil {
			t.Errorf("%q: the live status picklist must be the parent: %v", ref, err)
		}
		if code, _, _ := validationOf(e.checkAddFieldRefs(context.Background(), ref, child("tier", "gold"))); code != "FIELD_DEPENDENT_PICKLIST_INVALID" {
			t.Errorf("%q: a draft's tier must not vouch for the live table, got %q", ref, code)
		}
	}
	mustReject(t, e, "propose_field", `{"entity":"tickets","name":"sub","label":"Nhóm con","fieldType":"picklist",
		"options":{"values":["a"],"controllingField":"tier","dependentValues":{"gold":["a"]}}}`, "FIELD_DEPENDENT_PICKLIST_INVALID")
}
