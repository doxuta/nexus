package services

// specs/048 S2 (T048) — the before_save veto on the CSV import path, over fakes (no MySQL): each row
// is judged after its field checks and BEFORE the dedup probe and the auto_number claim; a refused
// row is reported by its CSV line with the rule's words (one entry per broken rule) and the rest of
// the file is imported; unreadable rules refuse the whole import unjudged.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

func newImportHookSvc(rules ...workflow.Workflow) (*ImportService, *fakeImportStore, *fakeSequenceClaimer, *vetoLoader) {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-o", Name: "opps"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "stage", FieldType: "text"},
			{Name: "close_date", FieldType: "date"},
			{Name: "code", FieldType: "auto_number", Options: strPtr(`{"prefix":"OPP-","padding":3}`)},
		},
	}
	store, claimer, loader := &fakeImportStore{}, &fakeSequenceClaimer{}, &vetoLoader{rules: rules}
	svc := NewImportService(cat, store, nil).WithSequenceClaimer(claimer).WithBeforeHooks(loader, nil)
	return svc, store, claimer, loader
}

func importOpps(t *testing.T, svc *ImportService, csv, dedup string) (*ImportResult, error) {
	t.Helper()
	return svc.Import(context.Background(), ImportInput{
		TenantID: "t1", UserID: "u-rep", RoleID: "role-rep", EntityName: "opps",
		Mapping:    map[string]string{"name": "name", "stage": "stage", "close_date": "close_date"},
		DedupField: dedup, Reader: strings.NewReader(csv), FileSize: int64(len(csv)),
	})
}

const oppsCSV = "name,stage,close_date\nA,Closed Won,2026-10-01\nB,Closed Won,\nC,Proposal,\n"

func TestImport_BeforeSaveVeto_RowError(t *testing.T) {
	svc, store, claimer, loader := newImportHookSvc(wonNeedsDateOn("opps", ""))
	res, err := importOpps(t, svc, oppsCSV, "")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.Total != 3 || res.Inserted != 2 || res.Failed != 1 {
		t.Fatalf("total/inserted/failed = %d/%d/%d, want 3/2/1", res.Total, res.Inserted, res.Failed)
	}
	want := ImportRowError{Row: 2, Field: "close_date", Code: "RECORD_HOOK_VETO", Message: hookVetoMsg}
	if len(res.Errors) != 1 || res.Errors[0] != want {
		t.Fatalf("errors = %+v, want [%+v]", res.Errors, want)
	}
	if len(store.insertedRows) != 2 || store.insertedRows[0]["name"] != "A" || store.insertedRows[1]["name"] != "C" {
		t.Fatalf("inserted = %v, want rows A and C", store.insertedRows)
	}
	if claimer.calls != 2 {
		t.Fatalf("sequence claims = %d, want 2 (the refused row claims no number)", claimer.calls)
	}
	if loader.calls != 1 {
		t.Fatalf("rule loads = %d, want 1 per import", loader.calls)
	}

	t.Run("hidden anchors dropped, one lookup for the file", func(t *testing.T) {
		svc, _, _, _ := newImportHookSvc(wonNeedsDateOn("opps", ""))
		acl := &fakeListHiddenACL{hidden: map[string]map[string]struct{}{"opps": {"close_date": {}}}}
		svc.WithFieldACL(acl)
		// The role cannot map a hidden column (queue #317 answers that as an unknown field), so the
		// CSV leaves close_date unmapped; the rule still vetoes and must not anchor on it.
		csv := "name,stage\nA,Closed Won\nB,Closed Won\n"
		res, err := svc.Import(context.Background(), ImportInput{
			TenantID: "t1", UserID: "u-rep", RoleID: "role-rep", EntityName: "opps",
			Mapping: map[string]string{"name": "name", "stage": "stage"},
			Reader:  strings.NewReader(csv), FileSize: int64(len(csv)),
		})
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if len(res.Errors) != 2 || res.Errors[0].Field != "" || res.Errors[1].Field != "" || res.Errors[1].Message != hookVetoMsg {
			t.Fatalf("errors = %+v, want two unanchored vetoes", res.Errors)
		}
		// Two loads for the whole file — the mapping guard's and the veto anchors' — never one per row.
		if acl.hiddenCalls != 2 {
			t.Fatalf("HiddenFields calls = %d, want 2", acl.hiddenCalls)
		}
	})
	t.Run("every broken rule of a row is listed", func(t *testing.T) {
		banB := workflow.Workflow{ID: "rule-b", TenantID: "t1", EntityName: "opps", Trigger: workflow.TriggerBeforeSave, IsActive: true,
			Condition: &workflow.Condition{Rules: []workflow.Rule{{Field: "name", Op: workflow.OpEq, Value: "B"}}},
			Actions:   []workflow.Action{{Type: workflow.ActionVeto, Message: "Tên B không dùng được"}}}
		svc, _, _, _ := newImportHookSvc(wonNeedsDateOn("opps", ""), banB)
		res, err := importOpps(t, svc, oppsCSV, "")
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.Failed != 1 || len(res.Errors) != 2 || res.Errors[0].Row != 2 || res.Errors[1].Row != 2 ||
			res.Errors[0].Message != hookVetoMsg || res.Errors[1].Message != "Tên B không dùng được" {
			t.Fatalf("errors = %+v, want both rules on row 2", res.Errors)
		}
	})
	t.Run("judged before the dedup probe", func(t *testing.T) {
		// Row 2 repeats row 1's name AND breaks the rule: judged first, it is reported as the
		// rule's refusal, not as a duplicate of row 1.
		svc, _, _, _ := newImportHookSvc(wonNeedsDateOn("opps", ""))
		res, err := importOpps(t, svc, "name,stage,close_date\nA,Proposal,\nA,Closed Won,\n", "name")
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.Inserted != 1 || len(res.Errors) != 1 || res.Errors[0].Code != "RECORD_HOOK_VETO" {
			t.Fatalf("result = %+v, want row 2 refused by the rule", res)
		}
	})
	t.Run("an update-only rule does not judge an import", func(t *testing.T) {
		svc, _, _, _ := newImportHookSvc(wonNeedsDateOn("opps", workflow.SaveOnUpdate))
		res, err := importOpps(t, svc, oppsCSV, "")
		if err != nil || res.Inserted != 3 {
			t.Fatalf("result = %+v, %v; want 3 inserted", res, err)
		}
	})
	t.Run("rules unreadable ⇒ the import is refused unjudged", func(t *testing.T) {
		svc, store, claimer, loader := newImportHookSvc(wonNeedsDateOn("opps", ""))
		loader.err = errors.New("sys_workflows: connection refused")
		_, err := importOpps(t, svc, oppsCSV, "")
		if !errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordHooksUnavailable", err)
		}
		if store.insertCalled || claimer.calls != 0 {
			t.Fatalf("insert called %v, claims %d — want nothing written", store.insertCalled, claimer.calls)
		}
	})
}

// specs/061 FR-010: a preview judges every row by the before_save rules but leaves no run in the
// rule's history; the real import of the same file leaves its one summary run (S3) — so the sink is
// wired and a missing dry-run guard would show (review MEDIUM, mutation-found).
func TestImport_DryRunRecordsNoRuleRun(t *testing.T) {
	for _, tt := range []struct {
		name     string
		dryRun   bool
		wantRuns int
	}{{"preview", true, 0}, {"real import", false, 1}} {
		t.Run(tt.name, func(t *testing.T) {
			sink, drain := newRunSink(t)
			svc, store, _, loader := newImportHookSvc(wonNeedsDateOn("opps", ""))
			svc.WithBeforeHooks(loader, sink)
			res, err := svc.Import(context.Background(), ImportInput{
				TenantID: "t1", UserID: "u-rep", RoleID: "role-rep", EntityName: "opps", DryRun: tt.dryRun,
				Mapping: map[string]string{"name": "name", "stage": "stage", "close_date": "close_date"},
				Reader:  strings.NewReader(oppsCSV), FileSize: int64(len(oppsCSV)),
			})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if res.Valid != 2 || len(res.Errors) != 1 || res.Errors[0].Code != "RECORD_HOOK_VETO" {
				t.Fatalf("result = %+v; want the rule to refuse row 2 in both runs", res)
			}
			if store.insertCalled == tt.dryRun {
				t.Fatalf("insertCalled = %v on dryRun = %v", store.insertCalled, tt.dryRun)
			}
			if runs := drain(); len(runs) != tt.wantRuns {
				t.Fatalf("rule runs recorded = %d (%+v); want %d", len(runs), runs, tt.wantRuns)
			}
		})
	}
}
