package services

// specs/048 S5 (T093, T094, T099) — the value-changing before_save rules (fill, normalize) on the two
// batch create paths that do not go through Create: bulk create atomic and CSV import, over fakes (no
// MySQL). Phase 1 runs on every row before the batch's duplicate checks and before any auto_number
// claim, so what is stored — and what is compared for uniqueness — is the rewritten value; each rule
// leaves ONE summary run per batch, counting only rows that were stored.
// record_hooks_normalize_integration_test.go proves the same on real MySQL.

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

func widgetRule(id string, actions ...workflow.Action) workflow.Workflow {
	return workflow.Workflow{ID: id, TenantID: "tenant-1", EntityName: "widgets", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Actions: actions}
}

func normalizeAction(field string, ops ...string) workflow.Action {
	return workflow.Action{Type: workflow.ActionNormalize, Field: field, Ops: ops}
}

// batchRuns renders each run as rule=status:error:actions_result, sorted, after checking it is a
// batch summary (before_save, no record).
func batchRuns(t *testing.T, runs []workflow.RunInput) string {
	t.Helper()
	out := make([]string, len(runs))
	for i, r := range runs {
		if r.RecordID != "" || r.TriggerType != "before_save" {
			t.Fatalf("run = %+v, want a before_save batch summary with no record", r)
		}
		out[i] = r.WorkflowID + "=" + string(r.Status) + ":" + r.Error + ":" + string(r.ActionsResult)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// T093 (unit twin): an all-or-nothing batch stores every row as the rules rewrote it, one rule load,
// one summary run per rule that changed rows.
func TestCreateBulk_Atomic_BeforeSaveMutations(t *testing.T) {
	ctx := context.Background()
	sink, drain := newRunSink(t)
	inserter, claimer := &fakeBulkInserter{}, &fakeSequenceClaimer{}
	loader := &vetoLoader{rules: []workflow.Workflow{
		widgetRule("rule-email", normalizeAction("email", workflow.NormalizeTrim, workflow.NormalizeLower)),
		widgetRule("rule-name", normalizeAction("name", workflow.NormalizeTrim, workflow.NormalizeCollapseSpaces)),
	}}
	svc := newBulkSvc(&bulkTestStore{}, claimer, inserter, nil, t).WithBeforeHooks(loader, sink)
	res, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", []map[string]any{
		{"name": "  Cái   A ", "email": " A@X.VN "},
		{"name": "B", "email": "b@x.vn"},
		{"name": "C  D", "email": "C@x.vn"},
	}, BulkModeAtomic)
	if err != nil || res.Created != 3 {
		t.Fatalf("CreateBulk = %+v, %v; want 3 created", res, err)
	}
	var names, emails []string
	for _, row := range inserter.lastRows {
		names, emails = append(names, row["name"].(string)), append(emails, row["email"].(string))
	}
	if strings.Join(names, "|") != "Cái A|B|C D" || strings.Join(emails, "|") != "a@x.vn|b@x.vn|c@x.vn" {
		t.Fatalf("stored names %q emails %q, want the rewritten text", names, emails)
	}
	if loader.calls != 1 || claimer.calls != 3 {
		t.Fatalf("rule loads %d, claims %d — want 1 and 3", loader.calls, claimer.calls)
	}
	want := `rule-email=success::{"applied":2,"blocked":0,"failed":0} rule-name=success::{"applied":2,"blocked":0,"failed":0}`
	if got := batchRuns(t, drain()); got != want {
		t.Fatalf("runs = %s\nwant   %s", got, want)
	}

	t.Run("rows the rules make equal on a unique field refuse the batch before any claim", func(t *testing.T) {
		sink, drain := newRunSink(t)
		inserter, claimer := &fakeBulkInserter{}, &fakeSequenceClaimer{}
		svc := newBulkSvc(&bulkTestStore{}, claimer, inserter, nil, t).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
			widgetRule("rule-email", normalizeAction("email", workflow.NormalizeLower)),
		}}, sink)
		res, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", []map[string]any{
			{"name": "A", "email": "An@x.vn"}, {"name": "B", "email": "an@X.vn"},
		}, BulkModeAtomic)
		if err != nil {
			t.Fatalf("CreateBulk: %v", err)
		}
		if res.Created != 0 || res.Failed != 1 || res.Results[1].Code != "RECORD_DUPLICATE" || res.Results[1].Details["field"] != "email" ||
			res.Results[0].Status != "skipped" {
			t.Fatalf("results = %+v, want row 1 RECORD_DUPLICATE on email, row 0 skipped", res.Results)
		}
		if inserter.calls != 0 || claimer.calls != 0 {
			t.Fatalf("%d insert calls, %d claims — want 0 and 0", inserter.calls, claimer.calls)
		}
		if runs := drain(); len(runs) != 0 {
			t.Fatalf("runs = %s — a batch that stored nothing leaves no run of a rule that changes values", runSummary(runs))
		}
	})
	t.Run("a refused batch: the veto's blocked run, no run of the rule that changed values", func(t *testing.T) {
		sink, drain := newRunSink(t)
		inserter := &fakeBulkInserter{}
		svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, inserter, nil, t).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
			widgetRule("rule-email", normalizeAction("email", workflow.NormalizeLower)), badName(""),
		}}, sink)
		res, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", []map[string]any{
			{"name": "A", "email": "A@x.vn"}, {"name": "BAD", "email": "B@x.vn"},
		}, BulkModeAtomic)
		if err != nil || res.Created != 0 || res.Results[1].Code != "RECORD_HOOK_VETO" || inserter.calls != 0 {
			t.Fatalf("CreateBulk = %+v, %v; want the batch refused by the veto", res, err)
		}
		if got, want := batchRuns(t, drain()), `rule-bad=blocked::{"applied":0,"blocked":1,"failed":0}`; got != want {
			t.Fatalf("runs = %s, want %s", got, want)
		}
	})
	t.Run("the veto judges the rewritten row", func(t *testing.T) {
		// "  BAD " is not BAD until the name is trimmed: judged as sent it would pass.
		svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
			widgetRule("rule-name", normalizeAction("name", workflow.NormalizeTrim)), badName(""),
		}}, nil)
		res, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "  BAD "), BulkModeAtomic)
		if err != nil || res.Created != 0 || res.Results[1].Code != "RECORD_HOOK_VETO" {
			t.Fatalf("CreateBulk = %+v, %v; want row 1 refused once its name is trimmed", res, err)
		}
	})
	t.Run("a value refused at run time: the batch is stored without it, the rule's run says failed", func(t *testing.T) {
		sink, drain := newRunSink(t)
		inserter := &fakeBulkInserter{}
		svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, inserter, nil, t).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{
			widgetRule("rule-bad-value", workflow.Action{Type: workflow.ActionFieldUpdate, Field: "email", Value: 5.0}),
		}}, sink)
		res, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "B"), BulkModeAtomic)
		if err != nil || res.Created != 2 {
			t.Fatalf("CreateBulk = %+v, %v; want 2 created", res, err)
		}
		for _, row := range inserter.lastRows {
			if _, has := row["email"]; has {
				t.Fatalf("stored %v — the refused value was written", row)
			}
		}
		want := `rule-bad-value=failed:RECORD_INVALID_TYPE:{"applied":0,"blocked":0,"failed":2}`
		if got := batchRuns(t, drain()); got != want {
			t.Fatalf("runs = %s, want %s", got, want)
		}
	})
}

// importLeadsSvc: leads with a unique text email, a score filled by a rule and a formula over it.
func importLeadsSvc(store *fakeImportStore, sink *workflow.RunSink, rules ...workflow.Workflow) *ImportService {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-l", Name: "leads"},
		fields: []metadata.FieldDefinition{
			{Name: "full_name", FieldType: "text", IsRequired: true},
			{Name: "email", FieldType: "text", IsUnique: true},
			{Name: "score", FieldType: "number"},
			{Name: "double", FieldType: "formula", Options: strPtr(`{"formula":"score * 2"}`)},
		},
	}
	return NewImportService(cat, store, nil).WithBeforeHooks(&vetoLoader{rules: rules}, sink)
}

func importLeads(t *testing.T, svc *ImportService, csv, dedup string) (*ImportResult, error) {
	t.Helper()
	return svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u-rep", RoleID: "role-rep", EntityName: "leads",
		Mapping:    map[string]string{"full_name": "full_name", "email": "email"},
		DedupField: dedup, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
	})
}

func leadRule(id string, actions ...workflow.Action) workflow.Workflow {
	return workflow.Workflow{ID: id, TenantID: "t1", EntityName: "leads", Trigger: workflow.TriggerBeforeSave, IsActive: true, Actions: actions}
}

var us7ImportRules = []workflow.Workflow{
	leadRule("rule-email", normalizeAction("email", workflow.NormalizeTrim, workflow.NormalizeLower)),
	leadRule("rule-name", normalizeAction("full_name", workflow.NormalizeTrim, workflow.NormalizeCollapseSpaces)),
}

// T094 (US7 scenario 2): a CSV of leads typed any which way is stored normalized; a value a rule
// fills recomputes the formula that reads it (the file's formulas were computed before the rules ran).
func TestImport_BeforeSaveNormalizes(t *testing.T) {
	sink, drain := newRunSink(t)
	store := &fakeImportStore{}
	rules := append(append([]workflow.Workflow{}, us7ImportRules...),
		leadRule("rule-score", workflow.Action{Type: workflow.ActionFieldUpdate, Field: "score", Value: 10.0, OnlyIfEmpty: true}))
	svc := importLeadsSvc(store, sink, rules...)
	res, err := importLeads(t, svc, "full_name,email\n  Nguyễn   Văn  An ,  An.Nguyen@EXAMPLE.COM \nBình,binh@example.com\n", "")
	if err != nil || res.Inserted != 2 {
		t.Fatalf("Import = %+v, %v; want 2 inserted", res, err)
	}
	first := store.insertedRows[0]
	if first["full_name"] != "Nguyễn Văn An" || first["email"] != "an.nguyen@example.com" {
		t.Fatalf("row 1 stored %q / %q, want \"Nguyễn Văn An\" / \"an.nguyen@example.com\"", first["full_name"], first["email"])
	}
	if first["score"] != 10.0 || first["double"] != 20.0 {
		t.Fatalf("row 1 score %v double %v, want 10 and the formula recomputed to 20", first["score"], first["double"])
	}
	want := `rule-email=success::{"applied":1,"blocked":0,"failed":0} rule-name=success::{"applied":1,"blocked":0,"failed":0} ` +
		`rule-score=success::{"applied":2,"blocked":0,"failed":0}`
	if got := batchRuns(t, drain()); got != want {
		t.Fatalf("runs = %s\nwant   %s", got, want)
	}
}

// T094: the duplicate checks of an import read the normalized value. With dedupField the second of two
// emails that differ only by case and spaces is reported by its line (in the file, or already stored);
// without it the unique index refuses the whole file (IMPORT_BATCH_DUPLICATE, as for any duplicate) and
// no rule that changed values leaves a run — nothing was stored.
func TestImport_NormalizeBeforeUniqueCheck(t *testing.T) {
	t.Run("in the file, with dedupField", func(t *testing.T) {
		store := &fakeImportStore{}
		res, err := importLeads(t, importLeadsSvc(store, nil, us7ImportRules...), "full_name,email\nA,An@EXAMPLE.COM\nB, an@example.com \n", "email")
		if err != nil || res.Inserted != 1 {
			t.Fatalf("Import = %+v, %v; want 1 inserted", res, err)
		}
		if len(res.Errors) != 1 || res.Errors[0].Row != 2 || res.Errors[0].Code != "RECORD_DUPLICATE" || res.Errors[0].Field != "email" {
			t.Fatalf("errors = %+v, want row 2 RECORD_DUPLICATE on email", res.Errors)
		}
	})
	t.Run("already stored, with dedupField: the probe asks for the normalized value", func(t *testing.T) {
		store := &fakeImportStore{existingDedupVals: map[string]struct{}{"old@example.com": {}}}
		res, err := importLeads(t, importLeadsSvc(store, nil, us7ImportRules...), "full_name,email\nC, OLD@EXAMPLE.COM\n", "email")
		if err != nil || res.Inserted != 0 || len(res.Errors) != 1 || res.Errors[0].Code != "RECORD_DUPLICATE" {
			t.Fatalf("Import = %+v, %v; want the row reported as already stored", res, err)
		}
	})
	// queue #358: without dedupField the unique field is probed all the same — the second row is
	// named by its line instead of the unique index refusing the whole file.
	t.Run("without dedupField the unique probe still reads the normalized value", func(t *testing.T) {
		store := &fakeImportStore{}
		res, err := importLeads(t, importLeadsSvc(store, nil, us7ImportRules...), "full_name,email\nA,An@EXAMPLE.COM\nB, an@example.com \n", "")
		if err != nil || res.Inserted != 1 || len(res.Errors) != 1 || res.Errors[0].Row != 2 || res.Errors[0].Code != "RECORD_DUPLICATE" {
			t.Fatalf("Import = %+v, %v; want row 2 RECORD_DUPLICATE on the normalized email", res, err)
		}
	})
	// A row that slips in between the probe and the insert still hits the unique index: the whole file
	// is refused (IMPORT_BATCH_DUPLICATE) and no rule that changed values leaves a run — nothing was stored.
	t.Run("a collision after the probe: the unique index refuses the file; no run of a rule that changed values", func(t *testing.T) {
		sink, drain := newRunSink(t)
		store := &fakeImportStore{insertErr: dynrepo.ErrDuplicateUnique}
		_, err := importLeads(t, importLeadsSvc(store, sink, us7ImportRules...), "full_name,email\nA,An@EXAMPLE.COM\nB,Binh@EXAMPLE.COM\n", "")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "IMPORT_BATCH_DUPLICATE" {
			t.Fatalf("err = %v, want IMPORT_BATCH_DUPLICATE", err)
		}
		if got := store.insertedRows; len(got) != 2 || got[0]["email"] != "an@example.com" || got[1]["email"] != "binh@example.com" {
			t.Fatalf("rows sent to the store = %v, want both normalized", got)
		}
		if runs := drain(); len(runs) != 0 {
			t.Fatalf("runs = %s, want none — the file stored nothing", runSummary(runs))
		}
	})
}
