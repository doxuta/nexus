package vertical_pack

// specs/069 L4 (T075, T078-unit): table / field / relation part of the upgrade over fakes.
// Planning goes through computePlan with fabricated sides; the writes go through the adapters with
// recording fakes of the admin services.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
)

func entJSON(name string, fields ...string) json.RawMessage { // fields: "name:type[:req][:uniq]"
	var fs []string
	for _, f := range fields {
		p := strings.Split(f, ":")
		req, uniq := len(p) > 2 && p[2] == "req", len(p) > 3 && p[3] == "uniq"
		fs = append(fs, fmt.Sprintf(`{"name":%q,"label":%q,"fieldType":%q,"isRequired":%t,"isUnique":%t}`, p[0], p[0], p[1], req, uniq))
	}
	return json.RawMessage(fmt.Sprintf(`{"name":%q,"label":%q,"fields":[%s]}`, name, name, strings.Join(fs, ",")))
}

func (f *planFixture) entSides(name string, b, t, l json.RawMessage) {
	k := ItemEntity + "/" + name
	if b != nil {
		f.in.base[k] = StoredPackItem{Type: ItemEntity, Key: name, PackVersion: "1.0.0", Content: b, LiveID: "e-" + name}
	}
	if t != nil {
		f.in.theirs[k] = t
	}
	if l != nil {
		f.in.live[k] = l
		f.in.idx.add(ItemEntity, name, "e-"+name)
		f.in.idx.ok[ItemEntity] = true
	}
}

func hasWarnCode(it UpgradeItem, code string) bool {
	for _, w := range it.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// T075: unsupported field changes, confirm warnings on existing data / new relations.
func TestPlanFields_SchemaDecisions(t *testing.T) {
	f := readyFixture()
	f.in.hasRecords = func(e string) bool { return e == "tk" }
	base := entJSON("tk", "a:text", "b:text", "c:text")
	f.entSides("tk", base,
		entJSON("tk", "a:number", "b:text:req", "c:text:no:uniq", "d:text", "fk:relation"),
		base)
	f.entSides("empty", entJSON("empty", "x:text"), entJSON("empty", "x:text:req"), entJSON("empty", "x:text"))
	p := f.plan(t)

	if it := find(t, p, ItemField, "tk.a"); it.Status != StatusUnsupported || it.Reason != ReasonFieldTypeChange || len(it.Actions) != 1 {
		t.Errorf("type change: %s/%s actions=%v; want unsupported FIELD_TYPE_CHANGE keep-only", it.Status, it.Reason, it.Actions)
	}
	if it := find(t, p, ItemField, "tk.c"); it.Status != StatusUnsupported || it.Reason != ReasonFieldUnique {
		t.Errorf("unique change: %s/%s; want unsupported FIELD_UNIQUE_CHANGE", it.Status, it.Reason)
	}
	req := find(t, p, ItemField, "tk.b")
	if !hasWarn(req, "REQUIRED_ON_EXISTING_DATA") || req.DefaultAction != ActionKeep || !contains(req.Actions, ActionApply) {
		t.Errorf("required on a table with records: default=%s actions=%v warnings=%v; want keep by default + REQUIRED_ON_EXISTING_DATA, apply offered", req.DefaultAction, req.Actions, req.Warnings)
	}
	if it := find(t, p, ItemField, "empty.x"); hasWarnCode(it, "REQUIRED_ON_EXISTING_DATA") || it.DefaultAction != ActionApply {
		t.Errorf("required on an empty table: default=%s warnings=%v; want silent pre-selected apply", it.DefaultAction, it.Warnings)
	}
	rel := find(t, p, ItemField, "tk.fk")
	if rel.Status != StatusAdded || !hasWarn(rel, "NEW_RELATION_ON_EXISTING_TABLE") || rel.DefaultAction != ActionKeep {
		t.Errorf("new relation on a live table: %s default=%s warnings=%v; want added, keep, NEW_RELATION_ON_EXISTING_TABLE", rel.Status, rel.DefaultAction, rel.Warnings)
	}
	if it := find(t, p, ItemField, "tk.d"); it.Status != StatusAdded || it.DefaultAction != ActionApply || hasConfirm(it.Warnings) {
		t.Errorf("new plain field: %s default=%s; want added, pre-selected apply", it.Status, it.DefaultAction)
	}
}

func TestPlanNN_UpdateUnsupported_DroppedFieldKeepOnly(t *testing.T) {
	f := readyFixture()
	nn := func(a, b string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"name":"a_b","entityA":%q,"entityB":%q}`, a, b))
	}
	f.in.base[ItemNN+"/a_b"] = StoredPackItem{Type: ItemNN, Key: "a_b", Content: nn("a", "b")}
	f.in.theirs[ItemNN+"/a_b"] = nn("a", "c")
	f.in.live[ItemNN+"/a_b"] = nn("a", "b")
	f.entSides("tk", entJSON("tk", "a:text", "gone:text"), entJSON("tk", "a:text"), entJSON("tk", "a:text", "gone:text"))
	p := f.plan(t)
	if it := find(t, p, ItemNN, "a_b"); it.Status != StatusUnsupported || it.Reason != ReasonNNUpdate {
		t.Errorf("nn update: %s/%s; want unsupported NN_UPDATE_UNSUPPORTED", it.Status, it.Reason)
	}
	g := find(t, p, ItemField, "tk.gone")
	if g.Status != StatusRemovedInPack || g.Reason != ReasonHardDeleteKind || contains(g.Actions, ActionDelete) {
		t.Errorf("pack-dropped field: %s/%s actions=%v; want removed_in_pack HARD_DELETE_KIND without delete", g.Status, g.Reason, g.Actions)
	}
}

// ── adapters over fakes ──────────────────────────────────────────────────────────────────────

type recEntityWr struct {
	created  []string
	failName string
	updated  map[string]services.UpdateEntityInput
}

func (r *recEntityWr) CreateEntity(_ context.Context, _ string, in services.CreateEntityInput) (*metadata.EntityDefinition, []metadata.FieldDefinition, error) {
	if in.Name == r.failName {
		return nil, nil, invalid("ENTITY_BOOM", "boom")
	}
	r.created = append(r.created, in.Name+":"+fmt.Sprint(len(in.Fields)))
	return &metadata.EntityDefinition{ID: "id-" + in.Name, Name: in.Name}, nil, nil
}
func (r *recEntityWr) SoftDeleteEntity(context.Context, string, string) error { return nil }
func (r *recEntityWr) UpdateEntity(_ context.Context, _, name string, in services.UpdateEntityInput) (*metadata.EntityDefinition, error) {
	if r.updated == nil {
		r.updated = map[string]services.UpdateEntityInput{}
	}
	r.updated[name] = in
	return &metadata.EntityDefinition{Name: name}, nil
}

type recFieldWr struct {
	added  []string
	edited []services.EditFieldInput
}

func (r *recFieldWr) AddField(_ context.Context, _ string, in services.AddFieldInput) (*metadata.FieldDefinition, error) {
	r.added = append(r.added, in.EntityName+"."+in.Field.Name)
	return &metadata.FieldDefinition{ID: "f-" + in.Field.Name, Name: in.Field.Name}, nil
}
func (r *recFieldWr) EditField(_ context.Context, _ string, in services.EditFieldInput) (*metadata.FieldDefinition, error) {
	r.edited = append(r.edited, in)
	return &metadata.FieldDefinition{ID: in.FieldID}, nil
}

type schemaMeta struct{ fakeMintMeta }

func (m *schemaMeta) ListEntities(string) ([]metadata.EntityDefinition, error) {
	return []metadata.EntityDefinition{{ID: "ent-tk", Name: "tk"}}, nil
}
func (m *schemaMeta) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	return []metadata.FieldDefinition{{ID: "fid-a", Name: "a"}, {ID: "fid-b", Name: "b"}}, nil
}

func schemaEnv(ew *recEntityWr, fw *recFieldWr, theirs map[string]json.RawMessage, plan ...UpgradeItem) *applyEnv {
	s := NewService(Deps{EntityReader: &schemaMeta{}, EntityWriter: ew, FieldWriter: fw, RoleReader: fakeMintRoles{}})
	e := newApplyEnv(s, "t1", "u1", "p", "1.1.0", &planArtifacts{theirs: theirs})
	e.plan, e.act = map[string]*UpgradeItem{}, map[string]string{}
	for i := range plan {
		e.plan[plan[i].ItemType+"/"+plan[i].ItemKey] = &plan[i]
		e.act[plan[i].ItemType+"/"+plan[i].ItemKey] = ActionApply
	}
	return e
}

func TestSchemaAdapters_ReadyOnlyWithUpdatePorts(t *testing.T) {
	s := NewService(Deps{EntityReader: &schemaMeta{}, EntityWriter: &recEntityWr{}, FieldWriter: &recFieldWr{}, RoleReader: fakeMintRoles{}})
	for _, typ := range []string{ItemEntity, ItemField, ItemDeferredField} {
		if !s.adapterReady(typ) {
			t.Errorf("%s adapter not ready with update ports wired", typ)
		}
	}
	bare := NewService(Deps{EntityReader: &schemaMeta{}, EntityWriter: &fakeMintMeta{}, RoleReader: fakeMintRoles{}})
	if bare.adapterReady(ItemEntity) {
		t.Error("entity adapter ready although the writer cannot UpdateEntity (D12)")
	}
}

func TestWriteEntity_CreateWholeVsUpdateLabel(t *testing.T) {
	ew, fw := &recEntityWr{}, &recFieldWr{}
	theirs := map[string]json.RawMessage{ItemEntity + "/new": entJSON("new", "x:text", "y:text"), ItemEntity + "/tk": entJSON("tk", "a:text")}
	newIt := UpgradeItem{ItemType: ItemEntity, ItemKey: "new", Status: StatusAdded}
	tk := UpgradeItem{ItemType: ItemEntity, ItemKey: "tk", Status: StatusUpdateSafe, LiveID: "ent-tk"}
	e := schemaEnv(ew, fw, theirs, newIt, tk)
	if _, err := writeEntity(context.Background(), e, &newIt); err != nil {
		t.Fatal(err)
	}
	if len(ew.created) != 1 || ew.created[0] != "new:2" {
		t.Fatalf("created = %v; want the whole table with its 2 fields", ew.created)
	}
	if _, err := writeEntity(context.Background(), e, &tk); err != nil {
		t.Fatal(err)
	}
	if u, ok := ew.updated["tk"]; !ok || u.Label == nil || *u.Label != "tk" || len(ew.created) != 1 {
		t.Fatalf("existing table: updated=%v created=%v; want UpdateEntity label only", ew.updated, ew.created)
	}
}

func TestBlockedBy_FailedTableBlocksChildren(t *testing.T) {
	ew, fw := &recEntityWr{failName: "new"}, &recFieldWr{}
	theirs := map[string]json.RawMessage{ItemEntity + "/new": entJSON("new", "x:text")}
	newIt := UpgradeItem{ItemType: ItemEntity, ItemKey: "new", Status: StatusAdded}
	grant := UpgradeItem{ItemType: ItemRoleGrant, ItemKey: "staff/new/view", ParentKey: "staff", Status: StatusAdded}
	acl := UpgradeItem{ItemType: ItemFieldACL, ItemKey: "staff:new.x", Status: StatusAdded}
	other := UpgradeItem{ItemType: ItemRoleGrant, ItemKey: "staff/tk/view", ParentKey: "staff", Status: StatusAdded}
	e := schemaEnv(ew, fw, theirs, newIt, grant, acl, other)
	if _, err := writeEntity(context.Background(), e, &newIt); err == nil {
		t.Fatal("injected create failure did not surface")
	}
	for _, it := range []*UpgradeItem{&grant, &acl} {
		if !strings.Contains(e.blockedBy(it), "new") {
			t.Errorf("%s/%s not blocked by the failed table", it.ItemType, it.ItemKey)
		}
	}
	if msg := e.blockedBy(&other); msg != "" {
		t.Errorf("grant on another table blocked: %q", msg)
	}
}

func TestWriteField_AddVsEditAndMergedOptions(t *testing.T) {
	ew, fw := &recEntityWr{}, &recFieldWr{}
	add := UpgradeItem{ItemType: ItemField, ItemKey: "tk.n", ParentKey: "tk", Status: StatusAdded,
		Theirs: json.RawMessage(`{"name":"n","label":"N","fieldType":"text"}`)}
	edit := UpgradeItem{ItemType: ItemField, ItemKey: "tk.a", ParentKey: "tk", Status: StatusUpdateSafe,
		Theirs: json.RawMessage(`{"name":"a","label":"A2","fieldType":"date","isRequired":true,"options":{"indexed":true}}`),
		Live:   json.RawMessage(`{"name":"a","label":"A","fieldType":"date","options":{"note":"hand"}}`)}
	e := schemaEnv(ew, fw, nil, add, edit)
	if _, err := writeField(context.Background(), e, &add); err != nil || len(fw.added) != 1 || fw.added[0] != "tk.n" {
		t.Fatalf("add: %v %v", err, fw.added)
	}
	if _, err := writeField(context.Background(), e, &edit); err != nil || len(fw.edited) != 1 {
		t.Fatalf("edit: %v %v", err, fw.edited)
	}
	g := fw.edited[0]
	if g.FieldID != "fid-a" || g.Label == nil || *g.Label != "A2" || g.IsRequired == nil || !*g.IsRequired {
		t.Errorf("edit input = %+v", g)
	}
	if g.Options["indexed"] != true || g.Options["note"] != "hand" {
		t.Errorf("options = %v; want pack indexed overlaid on the builder's own keys", g.Options)
	}
}

// T078 (unit): a kept child keeps its OLD content in the composed entity baseline, an applied child
// takes the pack's, so the next plan still proposes the kept one.
func TestComposeEntityBaseline_KeptChildKeepsOldAppliedTakesPack(t *testing.T) {
	theirs := json.RawMessage(`{"name":"tk","label":"NEW","fields":[{"name":"a","label":"A-new","fieldType":"text"},{"name":"n","label":"N","fieldType":"text"}]}`)
	old := json.RawMessage(`{"name":"tk","label":"OLD","fields":[{"name":"a","label":"A-old","fieldType":"text"}]}`)
	st := newUpgStore()
	st.rows["t1|p|entity|tk"] = StoredPackItem{Type: ItemEntity, Key: "tk", Content: old}
	s := NewService(Deps{EntityReader: &schemaMeta{}, EntityWriter: &recEntityWr{}, RoleReader: fakeMintRoles{}}).WithItemStore(st)
	e := newApplyEnv(s, "t1", "u1", "p", "1.1.0", &planArtifacts{theirs: map[string]json.RawMessage{ItemEntity + "/tk": theirs}})
	e.book = context.Background()
	e.plan = map[string]*UpgradeItem{
		"entity/tk":  {ItemType: ItemEntity, ItemKey: "tk", LiveID: "ent-tk"},
		"field/tk.a": {ItemType: ItemField, ItemKey: "tk.a", ParentKey: "tk"},
		"field/tk.n": {ItemType: ItemField, ItemKey: "tk.n", ParentKey: "tk"},
	}
	e.done = map[string]bool{"field/tk.n": true} // a kept, n applied
	got := string(e.composeEntityBaseline("tk"))
	if !strings.Contains(got, "A-old") || strings.Contains(got, "A-new") || !strings.Contains(got, `"n"`) || !strings.Contains(got, "OLD") {
		t.Errorf("composed baseline = %s; want old parent + old a + pack n", got)
	}
}
