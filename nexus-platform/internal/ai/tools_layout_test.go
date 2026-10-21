package ai

import (
	"context"
	"strings"
	"testing"
)

// twoTenantLayoutReader is the M5 relationship fixture (patients/treatments for tenant-A, invoices for tenant-B).
func twoTenantLayoutReader() *fakeMetaReader { return twoTenantRelReader() }

func TestTools_ProposeLayout_ValidEdit_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"entity":"treatments","viewType":"edit","layoutJson":{
		"version":1,
		"sections":[{"title":"Thông tin chung","columns":2,"fields":["title","patient"]}]
	}}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_layout", args))
	if !ok {
		t.Fatalf("valid edit layout should buffer: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "layout" || e.draft[0].Layout == nil {
		t.Fatalf("expected 1 buffered layout draft, got %+v", e.draft)
	}
	l := e.draft[0].Layout
	if l.Entity != "treatments" || l.ViewType != "edit" {
		t.Fatalf("buffered layout wrong shape: %+v", l)
	}
	if _, has := l.LayoutJSON["sections"]; !has {
		t.Fatalf("buffered layout must carry the layoutJson verbatim: %+v", l.LayoutJSON)
	}
	if e.draft[0].Entity != nil || e.draft[0].Field != nil || e.draft[0].Relationship != nil {
		t.Fatalf("layout draft must not carry entity/field/relationship payloads: %+v", e.draft[0])
	}
}

func TestTools_ProposeLayout_ListWithDefaultSort_Buffered(t *testing.T) {
	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"entity":"treatments","viewType":"list","layoutJson":{
		"version":1,
		"columns":[{"field":"title","sortable":true}],
		"defaultSort":{"field":"created_at","order":"desc"}
	}}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_layout", args))
	if !ok {
		t.Fatalf("valid list layout with defaultSort should buffer: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "layout" || e.draft[0].Layout == nil {
		t.Fatalf("expected 1 buffered layout draft, got %+v", e.draft)
	}
	if e.draft[0].Layout.ViewType != "list" {
		t.Fatalf("buffered layout viewType wrong: %+v", e.draft[0].Layout)
	}
}

// TestTools_ProposeLayout_Invalid drives the real validator (services.ValidateLayoutConfig — the
// SAME function body SaveLayout runs) through the tool: each violation surfaces its actual
// ValidationError code for model self-correction and buffers nothing.
func TestTools_ProposeLayout_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{
			// treatments has TWO required fields (title, patient) — an edit view carrying only
			// title omits a required field.
			name: "edit view missing a required field",
			args: `{"entity":"treatments","viewType":"edit","layoutJson":{
				"version":1,"sections":[{"columns":1,"fields":["title"]}]}}`,
			wantCode: "REQUIRED_FIELD_OMITTED",
		},
		{
			name: "cell references a nonexistent field",
			args: `{"entity":"treatments","viewType":"edit","layoutJson":{
				"version":1,"sections":[{"columns":1,"fields":["title","patient","ghost_field"]}]}}`,
			wantCode: "LAYOUT_INVALID",
		},
		{
			name: "invalid viewType",
			args: `{"entity":"treatments","viewType":"form","layoutJson":{
				"version":1,"sections":[{"columns":1,"fields":["title","patient"]}]}}`,
			wantCode: "LAYOUT_INVALID",
		},
		{
			// Floor rule: title is required in metadata — a cell may not override required:false.
			name: "floor rule violation",
			args: `{"entity":"treatments","viewType":"edit","layoutJson":{
				"version":1,"sections":[{"columns":1,"fields":[{"field":"title","required":false},"patient"]}]}}`,
			wantCode: "LAYOUT_INVALID",
		},
		{
			name: "missing version",
			args: `{"entity":"treatments","viewType":"edit","layoutJson":{
				"sections":[{"columns":1,"fields":["title","patient"]}]}}`,
			wantCode: "LAYOUT_INVALID",
		},
		{
			name: "list view without defaultSort",
			args: `{"entity":"treatments","viewType":"list","layoutJson":{
				"version":1,"columns":[{"field":"title"}]}}`,
			wantCode: "LAYOUT_INVALID",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_layout", tt.args))
			if ok {
				t.Fatalf("invalid layout must NOT succeed: %+v", m)
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected proposal must not buffer, got %+v", e.draft)
			}
		})
	}
}

func TestTools_ProposeLayout_TenantScope(t *testing.T) {
	// A perfectly valid layout aimed at ANOTHER tenant's entity must be indistinguishable from
	// aiming at a missing entity — never validated against the other tenant's fields.
	layout := `{"version":1,"sections":[{"columns":1,"fields":["total"]}]}`

	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	args := `{"entity":"invoices","viewType":"detail","layoutJson":` + layout + `}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_layout", args))
	if ok {
		t.Fatal("propose_layout on a cross-tenant entity must NOT succeed")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected cross-tenant proposal must not buffer, got %+v", e.draft)
	}
}

// #30 item 1: a table proposed in the SAME draft takes its entry screen in the same turn — the
// layout is judged against the drafted fields (plus a propose_field on that table), and the card
// is a "create" (nothing is served for a table that does not exist yet).
func TestTools_ProposeLayout_TableProposedInTheSameDraft(t *testing.T) {
	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
	mustBuffer(t, e, "propose_entity", `{"name":"candidates","label":"Ứng viên","fields":[
		{"name":"full_name","label":"Họ tên","fieldType":"text","required":true},
		{"name":"email","label":"Email","fieldType":"email"},
		{"name":"code","label":"Mã","fieldType":"text","options":{"maxLength":40,"indexed":true}}]}`)
	mustBuffer(t, e, "propose_field", `{"entity":"candidates","name":"position","label":"Vị trí","fieldType":"text"}`)

	mustBuffer(t, e, "propose_layout", `{"entity":"candidates","viewType":"edit","layoutJson":{
		"version":1,"sections":[{"columns":2,"fields":["full_name","email","position"]}]}}`)
	it := e.draft[len(e.draft)-1]
	if it.Layout == nil || it.Layout.Entity != "candidates" || it.Change == nil {
		t.Fatalf("layout draft = %+v", it)
	}
	if c := it.Change; c.Op != "create" || c.Before != nil || c.Target.Entity != "candidates" || c.Target.View != "edit" {
		t.Fatalf("change = %+v; want a create on candidates/edit with no before", c)
	}
	// The drafted index makes the list sortable by it, like the live table will be.
	mustBuffer(t, e, "propose_layout", `{"entity":"candidates","viewType":"list","layoutJson":{
		"version":1,"columns":[{"field":"full_name"}],"defaultSort":{"field":"code","order":"asc"}}}`)

	// Judged against the drafted fields, not waved through.
	mustReject(t, e, "propose_layout", `{"entity":"candidates","viewType":"edit","layoutJson":{
		"version":1,"sections":[{"columns":1,"fields":["email"]}]}}`, "REQUIRED_FIELD_OMITTED")
	mustReject(t, e, "propose_layout", `{"entity":"candidates","viewType":"edit","layoutJson":{
		"version":1,"sections":[{"columns":1,"fields":["full_name","ghost"]}]}}`, "LAYOUT_INVALID")
	mustReject(t, e, "propose_layout", `{"entity":"candidates","viewType":"list","layoutJson":{
		"version":1,"columns":[{"field":"full_name"}],"defaultSort":{"field":"email","order":"asc"}}}`, "LAYOUT_INVALID")
	// A table neither live nor drafted is still refused.
	mustReject(t, e, "propose_layout", `{"entity":"nobody","viewType":"edit","layoutJson":{
		"version":1,"sections":[{"columns":1,"fields":["x"]}]}}`, "ENTITY_NOT_FOUND")
}

// #30 item 2: an index proposed in the same draft (propose_edit_field indexed:true) counts when the
// list's defaultSort is judged; before that edit the same layout is refused.
func TestTools_ProposeLayout_SeesAnIndexProposedInTheSameDraft(t *testing.T) {
	r := twoTenantLayoutReader()
	r.fields["tenant-A"]["eA2"][0].ID = "fT1" // propose_edit_field addresses the field by id
	e := &toolExecutor{reader: r, tenantID: "tenant-A", docs: fullDocs(0)}
	list := `{"entity":"treatments","viewType":"list","layoutJson":{
		"version":1,"columns":[{"field":"title"}],"defaultSort":{"field":"title","order":"asc"}}}`
	mustReject(t, e, "propose_layout", list, "LAYOUT_INVALID")

	mustBuffer(t, e, "propose_edit_field", `{"entity":"treatments","field":"title","changes":{"options":{"maxLength":80,"indexed":true}}}`)
	mustBuffer(t, e, "propose_layout", list)
	if c := e.draft[len(e.draft)-1].Change; c == nil || c.Op != "update" {
		t.Fatalf("change = %+v; want an update of the live table's served list", c)
	}
	// The overlay is a copy: the registry's own field is untouched.
	if got := *r.fields["tenant-A"]["eA2"][0].Options; got != `{"maxLength":80}` {
		t.Fatalf("live field options mutated to %s", got)
	}
}

// #30 security (the PR #21 shape on the layout path): a draft entity that re-uses a LIVE table's
// name — refused by propose_entity, but an older thread's draft may still sit in the buffer — never
// shadows the live table. The layout is judged against the live fields, and the card is an update
// of the live table, never a "create" of the harmless-looking draft.
func TestTools_ProposeLayout_LiveTableWinsOverADraftOfTheSameName(t *testing.T) {
	// Every spelling of the live name resolves to the live table (verifier round 1 of #39: a
	// leading-space variant missed the registry, fell to the draft and made a "create" card whose
	// Apply overwrote the live layout).
	for _, ref := range []string{"treatments", " treatments", "treatments ", "Treatments", " TREATMENTS "} {
		t.Run(ref, func(t *testing.T) {
			e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A", docs: fullDocs(0)}
			mustReject(t, e, "propose_entity", `{"name":"treatments","label":"Phiếu hỗ trợ","fields":[{"name":"note","label":"Ghi chú","fieldType":"text"}]}`, "ENTITY_ALREADY_EXISTS")

			e.draft = append(e.draft, DraftItem{Kind: "entity", Entity: &DraftEntity{Name: "treatments", Label: "Phiếu hỗ trợ",
				Fields: []DraftField{{Name: "note", Label: "Ghi chú", FieldType: "text"}}}})
			// Valid for the draft, not for the live table (no "note", required title/patient omitted).
			mustReject(t, e, "propose_layout", `{"entity":"`+ref+`","viewType":"edit","layoutJson":{
				"version":1,"sections":[{"columns":1,"fields":["note"]}]}}`, "LAYOUT_INVALID")

			mustBuffer(t, e, "propose_layout", `{"entity":"`+ref+`","viewType":"edit","layoutJson":{
				"version":1,"sections":[{"columns":1,"fields":["title","patient"]}]}}`)
			it := e.draft[len(e.draft)-1]
			if it.Layout.Entity != "treatments" || it.Change == nil || it.Change.Op != "update" || it.Change.Before == nil {
				t.Fatalf("layout card = %+v / %+v; want an update of the live treatments layout", it.Layout, it.Change)
			}
		})
	}
}

// resolveEntityLiveOrDrafted (propose_approval, dashboard widgets) canonicalizes once, like
// propose_layout: a spaced or mixed-case spelling of a live table resolves to it, not to a refusal.
func TestResolveEntityLiveOrDrafted_CanonicalName(t *testing.T) {
	e := &toolExecutor{reader: twoTenantLayoutReader(), tenantID: "tenant-A"}
	for _, ref := range []string{"patients", " patients", "patients ", "Patients", " PATIENTS "} {
		if name, res := e.resolveEntityLiveOrDrafted(context.Background(), ref); res != "" || name != "patients" {
			t.Fatalf("resolve(%q) = %q, %s; want the live patients", ref, name, res)
		}
	}
	if _, res := e.resolveEntityLiveOrDrafted(context.Background(), " invoices"); !strings.Contains(res, "ENTITY_NOT_FOUND") {
		t.Fatalf("another tenant's table must stay unresolved, got %s", res)
	}
}
