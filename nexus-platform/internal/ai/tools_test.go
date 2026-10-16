package ai

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

// fakeMetaReader is a two-tenant in-memory metadata store proving the tools never cross tenants.
type fakeMetaReader struct {
	entities map[string][]metadata.EntityDefinition           // tenantID -> entities
	fields   map[string]map[string][]metadata.FieldDefinition // tenantID -> entityID -> fields
	rels     map[string][]metadata.RelationshipDefinition     // tenantID -> relationships
	roles    map[string][]string
	aggUsers map[string][]string // "entity.field" -> rollups aggregating it (#234 guard)
}

func (f *fakeMetaReader) ListEntities(tenantID string) ([]metadata.EntityDefinition, error) {
	return f.entities[tenantID], nil
}

func (f *fakeMetaReader) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	for i := range f.entities[tenantID] {
		if f.entities[tenantID][i].Name == name {
			return &f.entities[tenantID][i], nil
		}
	}
	return nil, metadata.ErrEntityNotFound
}

func (f *fakeMetaReader) ListFields(_ context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	return f.fields[tenantID][entityID], nil
}

func (f *fakeMetaReader) ListAllRelationships(_ context.Context, tenantID string) ([]metadata.RelationshipDefinition, error) {
	return f.rels[tenantID], nil
}

// ListRoleNames backs the list_roles tool. Tenant-keyed like every other read on this fake, so a
// test can prove the model never sees another tenant's roles.
func (f *fakeMetaReader) ListRoleNames(_ context.Context, tenantID string) ([]string, error) {
	return f.roles[tenantID], nil
}

func (f *fakeMetaReader) ListRollupsAggregating(_ context.Context, _, childEntity, aggField string) ([]string, error) {
	return f.aggUsers[childEntity+"."+aggField], nil
}

func twoTenantReader() *fakeMetaReader {
	return &fakeMetaReader{
		entities: map[string][]metadata.EntityDefinition{
			"tenant-A": {{ID: "eA", TenantID: "tenant-A", Name: "patients", Label: "Bệnh nhân"}},
			"tenant-B": {{ID: "eB", TenantID: "tenant-B", Name: "invoices", Label: "Hóa đơn"}},
		},
		fields: map[string]map[string][]metadata.FieldDefinition{
			"tenant-A": {"eA": {{ID: "fA1", Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true}}},
			"tenant-B": {"eB": {{ID: "fB1", Name: "total", Label: "Tổng", FieldType: "currency"}}},
		},
	}
}

// parseToolResult decodes the {ok, result|error} envelope a tool returns.
func parseToolResult(t *testing.T, s string) (ok bool, m map[string]any) {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("tool result not JSON: %v (%s)", err, s)
	}
	b, _ := out["ok"].(bool)
	return b, out
}

func TestTools_ListEntities_TenantIsolation(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	res := e.execute(context.Background(), "list_entities", "{}")

	// Tenant A must see ONLY its own entity, never tenant B's "invoices".
	if strings.Contains(res, "invoices") || strings.Contains(res, "Hóa đơn") {
		t.Fatalf("tenant A's list leaked tenant B data: %s", res)
	}
	if !strings.Contains(res, "patients") {
		t.Fatalf("tenant A's list missing its own entity: %s", res)
	}
}

func TestTools_ListFields_TenantScoped(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}

	// tenant A can read its own entity's fields.
	ok, _ := parseToolResult(t, e.execute(context.Background(), "list_fields", `{"entity":"patients"}`))
	if !ok {
		t.Fatal("list_fields on own entity should succeed")
	}

	// tenant A asking for tenant B's entity name resolves to not-found (never B's fields).
	okB, m := parseToolResult(t, e.execute(context.Background(), "list_fields", `{"entity":"invoices"}`))
	if okB {
		t.Fatal("tenant A must NOT resolve tenant B's entity 'invoices'")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND for cross-tenant entity, got %q", errStr)
	}
}

func TestTools_ProposeEntity_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	args := `{"name":"appointments","label":"Lịch hẹn","fields":[
		{"name":"start_at","label":"Bắt đầu","fieldType":"datetime","required":true},
		{"name":"status","label":"Trạng thái","fieldType":"picklist","options":{"values":["scheduled","done"]}}
	]}`
	ok, _ := parseToolResult(t, e.execute(context.Background(), "propose_entity", args))
	if !ok {
		t.Fatalf("valid propose_entity should succeed; draft=%+v", e.draft)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "entity" || e.draft[0].Entity == nil {
		t.Fatalf("expected 1 buffered entity draft, got %+v", e.draft)
	}
	if e.draft[0].Entity.Name != "appointments" || len(e.draft[0].Entity.Fields) != 2 {
		t.Fatalf("buffered entity wrong shape: %+v", e.draft[0].Entity)
	}
}

func TestTools_ProposeEntity_BadFieldType_NotBuffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	// "phone_number" is not a valid Nexus fieldType → config-time validation rejects it.
	args := `{"name":"contacts","label":"Liên hệ","fields":[
		{"name":"phone","label":"SĐT","fieldType":"phone_number"}
	]}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_entity", args))
	if ok {
		t.Fatal("propose_entity with an invalid fieldType must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "FIELD_INVALID_TYPE") {
		t.Fatalf("expected FIELD_INVALID_TYPE code, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("an invalid proposal must not enter the draft buffer, got %+v", e.draft)
	}
}

func TestTools_ProposeField_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	args := `{"entity":"patients","name":"phone","label":"SĐT","fieldType":"text","required":false}`
	ok, _ := parseToolResult(t, e.execute(context.Background(), "propose_field", args))
	if !ok {
		t.Fatalf("valid propose_field should succeed")
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "field" || e.draft[0].Field == nil {
		t.Fatalf("expected 1 buffered field draft, got %+v", e.draft)
	}
	if e.draft[0].Field.Entity != "patients" || e.draft[0].Field.Name != "phone" {
		t.Fatalf("buffered field wrong shape: %+v", e.draft[0].Field)
	}
}

func TestTools_ProposeField_PicklistMissingValues_Rejected(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	// picklist requires options.values — parity with the manual builder (B6).
	args := `{"entity":"patients","name":"gender","label":"Giới tính","fieldType":"picklist"}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_field", args))
	if ok {
		t.Fatal("picklist without options.values must be rejected at propose time")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "FIELD_PICKLIST_NO_OPTIONS") {
		t.Fatalf("expected FIELD_PICKLIST_NO_OPTIONS, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
	}
}

func TestTools_UnknownTool(t *testing.T) {
	e := &toolExecutor{reader: twoTenantReader(), tenantID: "tenant-A"}
	ok, m := parseToolResult(t, e.execute(context.Background(), "delete_everything", "{}"))
	if ok {
		t.Fatal("an unknown tool must not succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "UNKNOWN_TOOL") {
		t.Fatalf("expected UNKNOWN_TOOL, got %q", errStr)
	}
}

// TestTools_NoTenantParamInSchemas is the by-construction proof of the tenant-isolation invariant:
// not one of the 16 tool schemas' Parameters exposes a tenant parameter, so the model can never
// name a tenant. Scope note: the scan is over tool.Function.Parameters only — deliberately NOT
// tool.Function.Description, which legitimately contains the word "tenant" in its Vietnamese
// prose (e.g. "của tenant hiện tại") to explain scoping to a human/model reader; only a schema
// FIELD the model could fill in would be a real isolation hole.
func TestTools_NoTenantParamInSchemas(t *testing.T) {
	// Pin the tool count so this scan can never silently cover less than the full surface: a
	// milestone that adds a tool without bumping this expectation fails here, forcing the author
	// to confirm the new schema is tenant-free (M5 → 6 tools, M6 → 8 tools, M7 → 9 tools,
	// M8 → 10 tools, M10 → 11 tools, Apps Phase 3 → 12 tools, EASE Nước 7 edit/delete/records
	// → 16 tools, AI-MODULES approval/dashboard/workqueue/explain_access/list_packs → 21, +list_roles → 22).
	if n := len(ToolDefinitions()); n != 25 {
		t.Fatalf("expected 25 tool definitions (22 -1 list_layouts +get_layout +list_workflows +list_reports +get_app, specs/067 L10-3), got %d — update this invariant scan deliberately", n)
	}
	for _, tool := range ToolDefinitions() {
		blob, _ := json.Marshal(tool.Function.Parameters)
		lower := strings.ToLower(string(blob))
		if strings.Contains(lower, "tenant") {
			t.Fatalf("tool %q schema exposes a tenant parameter — isolation must be server-injected, not model-supplied: %s",
				tool.Function.Name, blob)
		}
	}
}

// TestFieldTypeSchema_MatchesEngineAllowlist pins the AI Builder's fieldType enum (the ONLY
// field-type list the model is ever shown) to the engine allowlist by STRICT set equality. A
// hand-copied list drifted before: the engine gained `user` + `polymorphic_relation` and the AI
// surface silently could not propose either, so every AI-drafted app lacked an assignee and a
// "related to" field. The enum stays hand-written on purpose — a new engine type must fail HERE
// so its author also teaches the model the type's options shape (fieldOptionsSchema), instead of
// the type reaching the model untaught and 422-ing at Apply.
func TestFieldTypeSchema_MatchesEngineAllowlist(t *testing.T) {
	raw, _ := fieldTypeSchema()["enum"].([]string)
	got := slices.Sorted(slices.Values(raw))
	want := services.AllowedFieldTypes()

	var missing, extra []string
	for _, ft := range want {
		if !slices.Contains(got, ft) {
			missing = append(missing, ft)
		}
	}
	for _, ft := range got {
		if !slices.Contains(want, ft) {
			extra = append(extra, ft)
		}
	}
	if len(missing) > 0 || len(extra) > 0 || len(got) != len(want) {
		t.Fatalf("fieldTypeSchema enum != services.AllowedFieldTypes()\n  engine types the AI cannot propose: %v\n  AI types the engine rejects: %v\n  enum (%d entries): %v",
			missing, extra, len(raw), got)
	}
}

// TestFieldOptionsSchema_TeachesUserAndPolymorphic locks the TOOL-CONTRACT teaching for the two
// types added to the enum: the options description must say polymorphic_relation REQUIRES
// options.targetEntities (validatePolymorphicConfig → FIELD_POLYMORPHIC_NO_TARGETS) and how `user`
// differs from `relation` — without it the model drafts a target-less field that 422s at Apply.
func TestFieldOptionsSchema_TeachesUserAndPolymorphic(t *testing.T) {
	desc, _ := fieldOptionsSchema()["description"].(string)
	for _, want := range []string{"polymorphic_relation BẮT BUỘC", "targetEntities", "user:"} {
		if !strings.Contains(desc, want) {
			t.Errorf("fieldOptionsSchema description missing %q", want)
		}
	}
}

// TestProposeRecords_TeachesUserBlankAndPolymorphicRef: once the AI can propose `user` and
// `polymorphic_relation` fields (and the persona tells it to on almost every app), the sample
// data it proposes right after must not invent ids. ValidateRecord only checks their SHAPE (a
// string / a {type,id} object), so an invented id buffers cleanly and then 422s the whole
// POST /:entity/bulk at Apply (RECORD_USER_INVALID / polymorphic target not found). The model has
// no tool that lists real user ids, so `user` stays blank; a `polymorphic_relation` id may be an
// "@ref" to a sample record of its {type} proposed earlier this turn (queue #140) — and the tool
// description has to show that shape, or the model keeps leaving the field empty.
func TestProposeRecords_TeachesUserBlankAndPolymorphicRef(t *testing.T) {
	var desc string
	for _, tool := range ToolDefinitions() {
		if tool.Function.Name == "propose_records" {
			desc = tool.Function.Description
		}
	}
	if desc == "" {
		t.Fatal("propose_records tool not found")
	}
	for _, want := range []string{"`user`", "ĐỂ TRỐNG", "`polymorphic_relation`", `{"type":`, `"id":"@`} {
		if !strings.Contains(desc, want) {
			t.Errorf("propose_records description missing %q — the model will invent ids (user) or leave polymorphic_relation blank", want)
		}
	}
	if strings.Contains(desc, `"@ref" chỉ áp cho field relation`) {
		t.Error("propose_records description still says @ref is relation-only")
	}
}
