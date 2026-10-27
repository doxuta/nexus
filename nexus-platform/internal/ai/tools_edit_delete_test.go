package ai

// Unit tests for propose_edit_field / propose_delete_field / propose_delete_entity (EASE Nước 7
// AI-EDIT): validator parity with services.ValidateEditFieldChanges (the SAME body
// FieldService.EditField runs pre-DDL — FIELD_TYPE_IMMUTABLE / FIELD_PICKLIST_NO_OPTIONS /
// FIELD_RELATION_OPTIONS_IMMUTABLE / default coercion), tool-side immutable-name + empty-changes
// rejects, the old→new diff echo, delete confirm-card payloads, and tenant isolation. Mirrors the
// propose_app test style (two-tenant fake reader, buffer-untouched assertions).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// editFixtureReader is a two-tenant registry with enough field variety to exercise every
// edit/delete reject family: picklist (stored options), relation (immutable options), a
// computed/server-assigned auto_number, and plain text/number fields.
func editFixtureReader() *fakeMetaReader {
	statusOpts := `{"values":["open","closed"]}`
	relOpts := `{"relatedEntity":"patients"}`
	return &fakeMetaReader{
		entities: map[string][]metadata.EntityDefinition{
			"tenant-A": {
				{ID: "eA", TenantID: "tenant-A", Name: "patients", Label: "Bệnh nhân"},
				{ID: "eT", TenantID: "tenant-A", Name: "tickets", Label: "Phiếu hỗ trợ"},
			},
			"tenant-B": {{ID: "eB", TenantID: "tenant-B", Name: "invoices", Label: "Hóa đơn"}},
		},
		fields: map[string]map[string][]metadata.FieldDefinition{
			"tenant-A": {
				"eA": {
					{ID: "f-name", Name: "full_name", Label: "Họ tên", FieldType: "text", IsRequired: true},
					{ID: "f-age", Name: "age", Label: "Tuổi", FieldType: "number"},
				},
				"eT": {
					{ID: "f-subj", Name: "subject", Label: "Tiêu đề", FieldType: "text", IsRequired: true},
					{ID: "f-status", Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: &statusOpts},
					{ID: "f-pat", Name: "patient", Label: "Bệnh nhân", FieldType: "relation", Options: &relOpts},
					{ID: "f-code", Name: "code", Label: "Mã phiếu", FieldType: "auto_number"},
				},
			},
			"tenant-B": {"eB": {{ID: "fB1", Name: "total", Label: "Tổng", FieldType: "currency"}}},
		},
	}
}

func TestTools_ProposeEditField_Valid_Buffered_Diff(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"patients","field":"age","changes":{"label":"Tuổi bệnh nhân","isRequired":true}}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_edit_field", args))
	if !ok {
		t.Fatalf("valid propose_edit_field should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "edit_field" || e.draft[0].EditField == nil {
		t.Fatalf("expected 1 buffered edit_field draft, got %+v", e.draft)
	}
	ef := e.draft[0].EditField
	if ef.Entity != "patients" || ef.Field != "age" || ef.FieldID != "f-age" || ef.FieldType != "number" {
		t.Fatalf("edit draft identity wrong: %+v", ef)
	}
	// The FE renders an old→new diff card: Before must echo CURRENT stored values, After the
	// effective post-edit values.
	if ef.Before.Label != "Tuổi" || ef.Before.IsRequired {
		t.Fatalf("Before must echo stored values: %+v", ef.Before)
	}
	if ef.After.Label != "Tuổi bệnh nhân" || !ef.After.IsRequired {
		t.Fatalf("After must carry proposed values: %+v", ef.After)
	}
}

func TestTools_ProposeEditField_PicklistValues_Diff(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	args := `{"entity":"tickets","field":"status","changes":{"options":{"values":["open","closed","pending"]}}}`
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_edit_field", args))
	if !ok {
		t.Fatalf("picklist values extension should succeed: %+v", m)
	}
	ef := e.draft[0].EditField
	before, _ := ef.Before.Options["values"].([]any)
	after, _ := ef.After.Options["values"].([]any)
	if len(before) != 2 || len(after) != 3 {
		t.Fatalf("diff must show stored (2) vs proposed (3) values, got before=%v after=%v", before, after)
	}
}

// TestTools_ProposeEditField_Immutable_Rejected pins the immutable axes: fieldType via the SAME
// shared-body code the apply path returns (FIELD_TYPE_IMMUTABLE), name and relation options via
// the tool/shared guards. None of them may buffer a draft.
func TestTools_ProposeEditField_Immutable_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"fieldType change", `{"entity":"patients","field":"full_name","changes":{"fieldType":"number"}}`, "FIELD_TYPE_IMMUTABLE"},
		{"rename attempt", `{"entity":"patients","field":"full_name","changes":{"name":"ho_ten"}}`, "FIELD_NAME_IMMUTABLE"},
		{"relation retarget", `{"entity":"tickets","field":"patient","changes":{"options":{"relatedEntity":"tickets"}}}`, "FIELD_RELATION_OPTIONS_IMMUTABLE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_edit_field", tt.args))
			if ok {
				t.Fatal("immutable-axis edit must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected edit must not buffer, got %+v", e.draft)
			}
		})
	}
}

// TestTools_ProposeEditField_ValidatorParity pins that the shared body's config-time rejects
// surface at propose with the REAL apply-path codes (picklist emptied, bad default coercion).
func TestTools_ProposeEditField_ValidatorParity(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"picklist emptied", `{"entity":"tickets","field":"status","changes":{"options":{"values":[]}}}`, "FIELD_PICKLIST_NO_OPTIONS"},
		{"non-numeric default on number", `{"entity":"patients","field":"age","changes":{"defaultValue":"abc"}}`, "RECORD_INVALID_TYPE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_edit_field", tt.args))
			if ok {
				t.Fatal("invalid edit must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected edit must not buffer, got %+v", e.draft)
			}
		})
	}
}

func TestTools_ProposeEditField_ResolutionAndEmptyChanges(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"empty changes", `{"entity":"patients","field":"age","changes":{}}`, "EDIT_CHANGES_EMPTY"},
		{"unknown field", `{"entity":"patients","field":"nope","changes":{"label":"X"}}`, "FIELD_NOT_FOUND"},
		{"unknown entity", `{"entity":"ghosts","field":"age","changes":{"label":"X"}}`, "ENTITY_NOT_FOUND"},
		// Tenant B's entity must be indistinguishable from missing (isolation).
		{"cross-tenant entity", `{"entity":"invoices","field":"total","changes":{"label":"X"}}`, "ENTITY_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_edit_field", tt.args))
			if ok {
				t.Fatal("must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected call must not buffer, got %+v", e.draft)
			}
		})
	}
}

func TestTools_ProposeDeleteField_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_delete_field", `{"entity":"tickets","field":"status"}`))
	if !ok {
		t.Fatalf("valid propose_delete_field should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "delete_field" || e.draft[0].DeleteField == nil {
		t.Fatalf("expected 1 buffered delete_field draft, got %+v", e.draft)
	}
	df := e.draft[0].DeleteField
	// The confirm card needs name + label + type, and the FieldID the DELETE endpoint addresses.
	if df.Entity != "tickets" || df.Field != "status" || df.FieldID != "f-status" ||
		df.Label != "Trạng thái" || df.FieldType != "picklist" {
		t.Fatalf("delete_field draft wrong shape: %+v", df)
	}
}

func TestTools_ProposeDeleteField_Unknown_Rejected(t *testing.T) {
	tests := []struct {
		name     string
		args     string
		wantCode string
	}{
		{"unknown field", `{"entity":"patients","field":"nope"}`, "FIELD_NOT_FOUND"},
		{"cross-tenant entity", `{"entity":"invoices","field":"total"}`, "ENTITY_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_delete_field", tt.args))
			if ok {
				t.Fatal("must NOT succeed")
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected call must not buffer, got %+v", e.draft)
			}
		})
	}
}

func TestTools_ProposeDeleteEntity_Valid_Buffered(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_delete_entity", `{"entity":"tickets"}`))
	if !ok {
		t.Fatalf("valid propose_delete_entity should succeed: %+v", m)
	}
	if len(e.draft) != 1 || e.draft[0].Kind != "delete_entity" || e.draft[0].DeleteEntity == nil {
		t.Fatalf("expected 1 buffered delete_entity draft, got %+v", e.draft)
	}
	de := e.draft[0].DeleteEntity
	// The impact-honest confirm card: canonical name, label, live field count.
	if de.Entity != "tickets" || de.Label != "Phiếu hỗ trợ" || de.FieldCount != 4 {
		t.Fatalf("delete_entity draft wrong shape: %+v", de)
	}
}

// fakePacks / fakeCounts are the delete card's probes; each records the tenant it was asked about.
type fakePacks struct {
	packs map[string][]string // entity -> slugs, tenant-A only
	err   error
	asked []string
}

func (f *fakePacks) PacksOf(_ context.Context, tenantID, itemType, itemKey string) ([]string, error) {
	f.asked = append(f.asked, tenantID+"|"+itemType+"|"+itemKey)
	if f.err != nil || tenantID != "tenant-A" {
		return nil, f.err
	}
	return f.packs[itemKey], nil
}

type fakeCounts struct {
	n     int64
	err   error
	asked []string
}

func (f *fakeCounts) AggregateScalar(_ context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.AggregateParams) (dynrepo.AggregateScalar, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s|%s|admin=%v|%s", tenantID, entity, scope.IsAdmin, p.Metric))
	return dynrepo.AggregateScalar{Count: f.n}, f.err
}

// Issue #30 item 4: the delete card is honest about a pack table (which pack ships it, how many records the delete
// hides), and the result tells the agent it cannot skip the human's confirmation.
func TestTools_ProposeDeleteEntity_NamesThePackAndCountsTheRecords(t *testing.T) {
	packs, counts := &fakePacks{packs: map[string][]string{"tickets": {"ticketing"}}}, &fakeCounts{n: 42}
	e := &toolExecutor{reader: ReaderWithAccessProbes(editFixtureReader(), &AccessProbes{Packs: packs, Counts: counts}), tenantID: "tenant-A"}
	res := mustBuffer(t, e, "propose_delete_entity", `{"entity":"tickets"}`)
	de := e.draft[0].DeleteEntity
	if !slices.Equal(de.Packs, []string{"ticketing"}) || de.RecordCount == nil || *de.RecordCount != 42 {
		t.Fatalf("card = %+v (count %v); want packs [ticketing] and 42 records", de, de.RecordCount)
	}
	// The executor's tenant, the canonical name, every record (admin scope), a COUNT.
	if !slices.Equal(packs.asked, []string{"tenant-A|entity|tickets"}) || !slices.Equal(counts.asked, []string{"tenant-A|tickets|admin=true|count"}) {
		t.Fatalf("probes asked %v / %v", packs.asked, counts.asked)
	}
	r, _ := res["result"].(map[string]any)
	if r["recordCount"] != float64(42) || fmt.Sprint(r["packs"]) != "[ticketing]" {
		t.Errorf("the model is not told the facts: %v", r)
	}
	if note, _ := r["confirm"].(string); !strings.Contains(note, "không bỏ qua được bước xác nhận") || !strings.Contains(note, "gõ lại tên bảng") {
		t.Errorf("the result does not tell the agent it cannot skip the confirmation: %q", note)
	}
	b, _ := json.Marshal(e.draft[0])
	if !strings.Contains(string(b), `"packs":["ticketing"],"recordCount":42`) {
		t.Errorf("wire shape = %s", b)
	}
}

// A table no pack ships, a probe that fails, and a server with no probes: the proposal still lands (it deletes
// nothing), the facts it could not read are left out rather than guessed, and the confirmation note stays.
func TestTools_ProposeDeleteEntity_UnknownFactsAreLeftOut(t *testing.T) {
	for name, reader := range map[string]MetadataReader{
		"no pack ships it, an empty table": ReaderWithAccessProbes(editFixtureReader(), &AccessProbes{Packs: &fakePacks{}, Counts: &fakeCounts{}}),
		"both reads fail":                  ReaderWithAccessProbes(editFixtureReader(), &AccessProbes{Packs: &fakePacks{err: errors.New("boom")}, Counts: &fakeCounts{err: errors.New("boom")}}),
		"no probes wired":                  editFixtureReader(),
	} {
		t.Run(name, func(t *testing.T) {
			e := &toolExecutor{reader: reader, tenantID: "tenant-A"}
			res := mustBuffer(t, e, "propose_delete_entity", `{"entity":"tickets"}`)
			de := e.draft[0].DeleteEntity
			if len(de.Packs) != 0 {
				t.Errorf("packs = %v; want none", de.Packs)
			}
			r, _ := res["result"].(map[string]any)
			if _, ok := r["packs"]; ok {
				t.Errorf("the model is told packs it does not have: %v", r)
			}
			if name == "no pack ships it, an empty table" {
				if de.RecordCount == nil || *de.RecordCount != 0 {
					t.Errorf("an empty table counts 0, not unknown: %v", de.RecordCount)
				}
				return
			}
			if de.RecordCount != nil {
				t.Errorf("an unread count must be unknown, got %d", *de.RecordCount)
			}
			if _, ok := r["recordCount"]; ok {
				t.Errorf("the model is told a count nobody read: %v", r)
			}
			if r["confirm"] != deleteConfirmNote {
				t.Errorf("confirm = %v", r["confirm"])
			}
			// A read that failed is not swallowed: the model is told what it could not read.
			if got := fmt.Sprint(r["unread"]); (name == "both reads fail") != strings.Contains(got, "recordCount: ai.proposeDeleteEntity: boom") ||
				(name == "both reads fail") != strings.Contains(got, "packs: ai.proposeDeleteEntity: boom") {
				t.Errorf("unread = %v", r["unread"])
			}
		})
	}
}

// The description the model reads before it calls the tool says the same thing as the result.
func TestTools_ProposeDeleteEntity_DescriptionSaysTheConfirmationCannotBeSkipped(t *testing.T) {
	for _, d := range ToolDefinitions() {
		if d.Function.Name == "propose_delete_entity" {
			if !strings.Contains(d.Function.Description, "khỏi hỏi lại") || !strings.Contains(d.Function.Description, "không bỏ qua được bước xác nhận") {
				t.Fatalf("description: %s", d.Function.Description)
			}
			return
		}
	}
	t.Fatal("no propose_delete_entity tool")
}

func TestTools_ProposeDeleteEntity_CrossTenant_Rejected(t *testing.T) {
	e := &toolExecutor{reader: editFixtureReader(), tenantID: "tenant-A"}
	ok, m := parseToolResult(t, e.execute(context.Background(), "propose_delete_entity", `{"entity":"invoices"}`))
	if ok {
		t.Fatal("tenant A must NOT resolve tenant B's entity for deletion")
	}
	if errStr, _ := m["error"].(string); !strings.Contains(errStr, "ENTITY_NOT_FOUND") {
		t.Fatalf("expected ENTITY_NOT_FOUND, got %q", errStr)
	}
	if len(e.draft) != 0 {
		t.Fatalf("rejected call must not buffer, got %+v", e.draft)
	}
}

// TestTools_ProposeEditField_FormulaRefs (queue #107): the AI edit path runs the same sibling
// ref check as PUT /admin/fields/:id — a typo'd field name inside IF comes back to the model
// as FIELD_FORMULA_UNKNOWN_REF instead of buffering a draft that computes 0 forever.
func TestTools_ProposeEditField_FormulaRefs(t *testing.T) {
	commOpts := `{"formula":"amount * 0.05","resultType":"number"}`
	reader := &fakeMetaReader{
		entities: map[string][]metadata.EntityDefinition{
			"tenant-A": {{ID: "eD", TenantID: "tenant-A", Name: "deals", Label: "Cơ hội"}},
		},
		fields: map[string]map[string][]metadata.FieldDefinition{
			"tenant-A": {"eD": {
				{ID: "f-amount", Name: "amount", Label: "Giá trị", FieldType: "number"},
				{ID: "f-note", Name: "note", Label: "Ghi chú", FieldType: "text"},
				{ID: "f-comm", Name: "commission", Label: "Hoa hồng", FieldType: "formula", Options: &commOpts},
			}},
		},
	}
	tests := []struct {
		name     string
		expr     string
		wantCode string // "" = buffered
	}{
		{name: "typo inside IF", expr: "IF(amout > 1, 1, 0)", wantCode: "FIELD_FORMULA_UNKNOWN_REF"},
		{name: "text sibling", expr: "note * 2", wantCode: "FIELD_FORMULA_NON_NUMERIC_REF"},
		{name: "valid", expr: "IF(amount > 1, amount * 0.1, 0)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &toolExecutor{reader: reader, tenantID: "tenant-A"}
			args := `{"entity":"deals","field":"commission","changes":{"options":{"formula":"` + tt.expr + `","resultType":"number"}}}`
			ok, m := parseToolResult(t, e.execute(context.Background(), "propose_edit_field", args))
			if tt.wantCode == "" {
				if !ok || len(e.draft) != 1 {
					t.Fatalf("valid formula edit should buffer one draft: ok=%v m=%+v draft=%+v", ok, m, e.draft)
				}
				return
			}
			if ok {
				t.Fatalf("formula edit %q must NOT succeed", tt.expr)
			}
			if errStr, _ := m["error"].(string); !strings.Contains(errStr, tt.wantCode) {
				t.Fatalf("expected %s, got %q", tt.wantCode, errStr)
			}
			if len(e.draft) != 0 {
				t.Fatalf("rejected edit must not buffer, got %+v", e.draft)
			}
		})
	}
}

// TestTools_ProposeEditField_FormulaInUseGuard_ProductionWrappedReader pins queue #234/#284 on the
// reader production really injects (ReaderWithAccessProbes wraps the repo): the guard must still
// see ListRollupsAggregating through the wrapper, else propose passes what Apply later 422s.
func TestTools_ProposeEditField_FormulaInUseGuard_ProductionWrappedReader(t *testing.T) {
	fo := `{"formula":"age * 2","resultType":"number"}`
	base := editFixtureReader()
	base.fields["tenant-A"]["eA"] = append(base.fields["tenant-A"]["eA"],
		metadata.FieldDefinition{ID: "f-dbl", Name: "dbl", Label: "Gấp đôi", FieldType: "formula", Options: &fo})
	base.aggUsers = map[string][]string{"patients.dbl": {"tickets.sum_dbl"}}
	e := &toolExecutor{reader: ReaderWithAccessProbes(base, &AccessProbes{}), tenantID: "tenant-A"}

	tests := []struct {
		name, args, wantCode string
	}{
		{"resultType change", `{"entity":"patients","field":"dbl","changes":{"options":{"formula":"age * 2","resultType":"currency","currencyCode":"VND"}}}`, "FIELD_ROLLUP_AGG_FIELD_RESULT_TYPE_IN_USE"},
		{"same resultType passes", `{"entity":"patients","field":"dbl","changes":{"options":{"formula":"age * 2","resultType":"number"}}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.draft = nil
			raw := e.execute(context.Background(), "propose_edit_field", tt.args)
			ok, m := parseToolResult(t, raw)
			if tt.wantCode == "" {
				if !ok {
					t.Fatalf("unchanged formula must pass: %+v", m)
				}
				return
			}
			if ok || !strings.Contains(raw, tt.wantCode) || len(e.draft) != 0 {
				t.Fatalf("want %s and no draft, ok=%v draft=%d m=%+v", tt.wantCode, ok, len(e.draft), m)
			}
		})
	}
}
