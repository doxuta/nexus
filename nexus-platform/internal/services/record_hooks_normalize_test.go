package services

// specs/048 S5 — before_save normalize ("chuẩn hoá") inside the saves that go through Create and
// Update, over fakes (no MySQL): the text the user typed is stored rewritten, in the ONE store call of
// the save, and the rule's run names the field, never the text. Sửa hàng loạt goes through Update per
// record (T095). The batch paths (bulk create atomic, CSV import) are record_hooks_normalize_batch_test.go.

import (
	"context"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// contactLeadsCatalog: the US7 lead — a required name, a text email (as the CRM pack declares it),
// a phone kept as text and an email-typed field.
func contactLeadsCatalog() *fakeRecordCatalog {
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-cl", TenantID: "t1", Name: "leads", DataTable: "obj_leads"},
		fields: []metadata.FieldDefinition{
			{Name: "full_name", FieldType: "text", IsRequired: true},
			{Name: "email", FieldType: "text"},
			{Name: "phone", FieldType: "text"},
			{Name: "work_email", FieldType: "email"},
		},
	}
}

// us7Rules are the two rules of US7: email trimmed + lower-cased, the name trimmed + collapsed.
func us7Rules(entity string) []workflow.Workflow {
	return []workflow.Workflow{
		fillRule("rule-email", entity, workflow.Action{Type: workflow.ActionNormalize, Field: "email",
			Ops: []string{workflow.NormalizeTrim, workflow.NormalizeLower}}),
		fillRule("rule-name", entity, workflow.Action{Type: workflow.ActionNormalize, Field: "full_name",
			Ops: []string{workflow.NormalizeTrim, workflow.NormalizeCollapseSpaces}}),
	}
}

// US7 scenario 1: the lead typed with stray spaces and capitals is stored normalized; each rule that
// changed a value leaves one success run naming only its field.
func TestRecordCreate_BeforeSaveNormalizes(t *testing.T) {
	sink, drain := newRunSink(t)
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
	svc := NewRecordService(contactLeadsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: us7Rules("leads")}, sink)

	id, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads",
		map[string]any{"full_name": "  Nguyễn   Văn  An ", "email": "  An.Nguyen@EXAMPLE.COM "})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, _ := store.at(0)
	if got.data["full_name"] != "Nguyễn Văn An" || got.data["email"] != "an.nguyen@example.com" {
		t.Fatalf("stored = %q / %q, want \"Nguyễn Văn An\" / \"an.nguyen@example.com\"", got.data["full_name"], got.data["email"])
	}
	runs := drain()
	summary := runSummary(runs)
	if len(runs) != 2 || strings.Count(summary, " success rec="+id+" ") != 2 ||
		!strings.Contains(summary, `[{"type":"normalize","field":"email","ok":true}]`) ||
		!strings.Contains(summary, `[{"type":"normalize","field":"full_name","ok":true}]`) {
		t.Fatalf("runs = %s, want one success run per rule on %s", summary, id)
	}
	for _, v := range []string{"Nguyễn", "nguyen", "Nexus"} {
		if strings.Contains(summary, v) {
			t.Fatalf("a run carries the value %q: %s", v, summary)
		}
	}

	t.Run("an email-typed field is lower-cased too", func(t *testing.T) {
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
		rules := []workflow.Workflow{fillRule("rule-we", "leads", workflow.Action{Type: workflow.ActionNormalize, Field: "work_email",
			Ops: []string{workflow.NormalizeLower}})}
		svc := NewRecordService(contactLeadsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: rules}, nil)
		if _, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads",
			map[string]any{"full_name": "An", "work_email": "An@EXAMPLE.COM"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got, _ := store.at(0); got.data["work_email"] != "an@example.com" {
			t.Fatalf("stored work_email = %v, want an@example.com", got.data["work_email"])
		}
	})
	t.Run("a phone trimmed of its spaces is stored trimmed", func(t *testing.T) {
		store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}}
		rules := []workflow.Workflow{fillRule("rule-ph", "leads", workflow.Action{Type: workflow.ActionNormalize, Field: "phone",
			Ops: []string{workflow.NormalizeTrim}})}
		svc := NewRecordService(contactLeadsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: rules}, nil)
		if _, err := svc.Create(context.Background(), rep, "t1", rep.UserID, "leads",
			map[string]any{"full_name": "An", "phone": " 0901234567 "}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if got, _ := store.at(0); got.data["phone"] != "0901234567" {
			t.Fatalf("stored phone = %q, want \"0901234567\"", got.data["phone"])
		}
	})
}

// T095 (US7 scenario 3): sửa hàng loạt the email of two leads stores both normalized — each record
// through Update, in its one UPDATE.
func TestUpdateBulk_BeforeSaveNormalizes(t *testing.T) {
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
		before: map[string]any{"full_name": "Nguyễn Văn An", "email": "old@example.com"}}
	svc := NewRecordService(contactLeadsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: us7Rules("leads")}, nil)

	res, err := svc.UpdateBulk(context.Background(), rep, "t1", "leads", []string{"l1", "l2"}, map[string]any{"email": "  Sales@EXAMPLE.COM "})
	if err != nil || res.Updated != 2 || res.Failed != 0 {
		t.Fatalf("UpdateBulk = %+v, %v; want 2 updated", res, err)
	}
	if _, updates := store.counts(); updates != 2 {
		t.Fatalf("UpdateRecord calls = %d, want one per lead", updates)
	}
	for i, data := range store.updates {
		if data["email"] != "sales@example.com" {
			t.Fatalf("update %d wrote email %q, want \"sales@example.com\"", i, data["email"])
		}
	}
}
