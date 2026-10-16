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
