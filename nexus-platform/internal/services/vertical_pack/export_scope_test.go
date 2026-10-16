package vertical_pack

// Queue #296 + #303: handover must pull in (or loudly warn about) the tables a field needs, and
// the warnings must be about the app being handed over — not 40+ lines about other apps.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// fieldedEntities serves fixed entities + fields (fakeMintMeta returns no fields).
type fieldedEntities struct {
	ents   []metadata.EntityDefinition
	fields map[string][]metadata.FieldDefinition // entity ID → fields
}

func (f fieldedEntities) ListEntities(string) ([]metadata.EntityDefinition, error) {
	return f.ents, nil
}
func (f fieldedEntities) ListFields(_ context.Context, _, id string) ([]metadata.FieldDefinition, error) {
	return f.fields[id], nil
}

func optJSON(t *testing.T, m map[string]any) *string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	return &s
}

// invoice(hd_invoices) ← rollup "Tổng tiền" over hd_lines ← relation to hd_items (Vật tư).
func invoiceGraph(t *testing.T) fieldedEntities {
	return fieldedEntities{
		ents: []metadata.EntityDefinition{
			{ID: "e1", Name: "hd_invoices", Label: "Hoá đơn"},
			{ID: "e2", Name: "hd_lines", Label: "Dòng hoá đơn"},
			{ID: "e3", Name: "hd_items", Label: "Vật tư"},
			{ID: "e4", Name: "other_things", Label: "Khác"},
		},
		fields: map[string][]metadata.FieldDefinition{
			"e1": {{Name: "total", Label: "Tổng tiền", FieldType: "rollup",
				Options: optJSON(t, map[string]any{"childEntity": "hd_lines", "viaRelation": "invoice", "metric": "count"})}},
			"e2": {{Name: "item", Label: "Vật tư", FieldType: "relation",
				Options: optJSON(t, map[string]any{"relatedEntity": "hd_items"})}},
		},
	}
}

func TestEntityDependencies_PullsChildAndTransitive(t *testing.T) {
	svc := NewService(Deps{EntityReader: invoiceGraph(t), LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}})
	got, err := svc.EntityDependencies(context.Background(), "t1", []string{"hd_invoices"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "hd_lines" || got[1].Name != "hd_items" {
		t.Fatalf("deps = %+v, want hd_lines then hd_items (transitive), never other_things", got)
	}
	if !strings.Contains(got[0].Reason, "Tổng tiền") {
		t.Errorf("reason %q should name the rollup field that needs the table", got[0].Reason)
	}
	// already-named tables are not repeated
	got, _ = svc.EntityDependencies(context.Background(), "t1", []string{"hd_invoices", "hd_lines", "hd_items"})
	if len(got) != 0 {
		t.Errorf("deps = %+v, want none when everything is already named", got)
	}
}

func TestExport_WarnsWhenRollupChildLeftOut(t *testing.T) {
	svc := NewService(Deps{EntityReader: invoiceGraph(t), LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}})
	_, warnings, err := svc.Export(context.Background(), "t1", []string{"hd_invoices"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "Tổng tiền") && strings.Contains(w, "Dòng hoá đơn") && strings.Contains(w, "gãy") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings %v; want one naming the rollup 'Tổng tiền' and the missing table 'Dòng hoá đơn'", warnings)
	}
}

func TestExport_WorkflowScopeWarningsFoldedAndEmailAlertSilent(t *testing.T) {
	acts, _ := json.Marshal([]workflow.Action{{Type: "email_alert", To: "a@b.c", Subject: "s", Body: "b"}})
	rule := func(name, ent string) workflow.WorkflowRecord {
		return workflow.WorkflowRecord{ID: name, Name: name, EntityName: ent, Trigger: "after_create", Actions: acts, IsActive: true}
	}
	wf := &fakeWorkflowWriter{existing: []workflow.WorkflowRecord{
		rule("mail hoá đơn", "hd_invoices"), rule("của app khác 1", "other_things"), rule("của app khác 2", "other_things"),
	}}
	svc := NewService(Deps{EntityReader: invoiceGraph(t), LayoutReader: fakeApLayouts{}, RoleReader: fakeMintRoles{}, WorkflowWriter: wf})
	pack, warnings, err := svc.Export(context.Background(), "t1", []string{"hd_invoices", "hd_lines", "hd_items"}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Workflows) != 1 || pack.Workflows[0].Actions[0].Body != "b" || pack.Workflows[0].Actions[0].To != "a@b.c" {
		t.Fatalf("workflows = %+v, want the email_alert rule with To/Body carried", pack.Workflows)
	}
	var folded int
	for _, w := range warnings {
		if strings.Contains(w, "chưa được exporter biết") {
			t.Errorf("false warning still raised: %q", w)
		}
		if strings.Contains(w, "của app khác") {
			t.Errorf("per-item out-of-scope warning leaked: %q", w)
		}
		if strings.Contains(w, "2 luật tự động") {
			folded++
		}
	}
	if folded != 1 {
		t.Fatalf("warnings %v; want exactly one folded count line '2 luật tự động'", warnings)
	}
}

type fakeScopedRoles struct{ acts []auth.RoleAction }

func (fakeScopedRoles) ListRoles(context.Context, string) ([]auth.Role, error) {
	return []auth.Role{{ID: "r1", Name: "sales", Label: "Sales"}}, nil
}
func (f fakeScopedRoles) ListActionsForRole(context.Context, string, string) ([]auth.RoleAction, error) {
	return f.acts, nil
}

func TestOutOfScopeSummary_IgnoresZeroCounts(t *testing.T) {
	if got := outOfScopeSummary(map[string]int{"vai": 0, "quyền của vai": 0}); got != "" {
		t.Fatalf("zero counts produced %q, want empty", got)
	}
	got := outOfScopeSummary(map[string]int{"vai": 0, "quyền của vai": 3})
	if !strings.Contains(got, "3 quyền của vai") || strings.Contains(got, "0 vai") {
		t.Fatalf("got %q, want only the non-zero kind", got)
	}
}

func TestExport_RoleWarningOnlyWhenSomethingSkipped(t *testing.T) {
	cases := []struct {
		name     string
		acts     []auth.RoleAction
		wantLine string // substring; "" = no out-of-scope line at all
	}{
		{"all in scope", []auth.RoleAction{{EntityName: "hd_invoices", Action: "view", AccessLevel: "all"}}, ""},
		{"some out of scope", []auth.RoleAction{
			{EntityName: "hd_invoices", Action: "view", AccessLevel: "all"},
			{EntityName: "other_things", Action: "view", AccessLevel: "all"},
		}, "1 quyền của vai"},
		{"role fully out of scope", []auth.RoleAction{{EntityName: "other_things", Action: "view", AccessLevel: "all"}}, "1 vai"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(Deps{EntityReader: invoiceGraph(t), LayoutReader: fakeApLayouts{}, RoleReader: fakeScopedRoles{acts: c.acts}})
			_, warnings, err := svc.Export(context.Background(), "t1", []string{"hd_invoices", "hd_lines", "hd_items"}, false, nil)
			if err != nil {
				t.Fatal(err)
			}
			var line string
			for _, w := range warnings {
				if strings.Contains(w, "ngoài phạm vi xuất") {
					line = w
				}
			}
			if c.wantLine == "" && line != "" {
				t.Fatalf("clean export raised %q", line)
			}
			if c.wantLine != "" && !strings.Contains(line, c.wantLine) {
				t.Fatalf("line %q, want substring %q", line, c.wantLine)
			}
		})
	}
}
