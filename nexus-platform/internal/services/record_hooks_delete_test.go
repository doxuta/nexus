package services

// specs/048 S3 — the before_delete veto on RecordService.Delete (and every path through it: bulk
// delete) and the run history every before rule now leaves (blocked / skipped), over fakes (no MySQL).
// record_hooks_delete_integration_test.go proves the same on real MySQL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

const delVetoMsg = "Không xoá cơ hội đã Thắng"

// wonNoDelete is the US4 rule: before_delete, stage eq Closed Won ⇒ veto, anchored nowhere.
func wonNoDelete() workflow.Workflow {
	return workflow.Workflow{ID: "rule-del", TenantID: "t1", EntityName: "deals", Trigger: workflow.TriggerBeforeDelete, IsActive: true,
		Condition: &workflow.Condition{Rules: []workflow.Rule{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}}},
		Actions:   []workflow.Action{{Type: workflow.ActionVeto, Message: delVetoMsg}}}
}

// runLog is the RunRecorder behind a real RunSink.
type runLog struct {
	mu   sync.Mutex
	runs []workflow.RunInput
}

func (l *runLog) InsertRun(_ context.Context, in workflow.RunInput) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.runs = append(l.runs, in)
	return nil
}

// newRunSink returns a real RunSink and drain: drain closes the sink (every queued run written) and
// returns the runs the before rules left, in write order.
func newRunSink(t *testing.T) (*workflow.RunSink, func() []workflow.RunInput) {
	t.Helper()
	log := &runLog{}
	sink := workflow.NewRunSink(log, zap.NewNop())
	drain := func() []workflow.RunInput {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := sink.Close(ctx); err != nil {
			t.Fatalf("drain the run sink: %v", err)
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		return append([]workflow.RunInput(nil), log.runs...)
	}
	t.Cleanup(func() { drain() })
	return sink, drain
}

// delStore serves stored records by id and soft-deletes them; getErr fails every read.
type delStore struct {
	crCountingStore
	mu      sync.Mutex
	rows    map[string]map[string]any
	getErr  error
	deleted []string
}

func (s *delStore) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	d, ok := s.rows[id]
	if !ok {
		return nil, dynrepo.ErrRecordNotFound
	}
	return &dynrepo.Record{ID: id, TenantID: tenantID, Data: cloneData(d), OwnerID: "u-owner"}, nil
}

func (s *delStore) SoftDeleteRecord(_ context.Context, _, _, id string, _ dynrepo.RowScope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[id]; !ok {
		return dynrepo.ErrRecordNotFound
	}
	delete(s.rows, id)
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *delStore) deletedIDs() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Sprint(s.deleted)
}

type delHarness struct {
	svc    *RecordService
	store  *delStore
	loader *vetoLoader
	eng    *captureEngine
	audits *spyAuditor
	runs   func() []workflow.RunInput
}

func newDelHarness(t *testing.T, rows map[string]map[string]any, rules ...workflow.Workflow) delHarness {
	t.Helper()
	h := delHarness{store: &delStore{crCountingStore: crCountingStore{hardCap: 10}, rows: rows},
		loader: &vetoLoader{rules: rules}, eng: &captureEngine{}, audits: &spyAuditor{}}
	sink, drain := newRunSink(t)
	h.runs = drain
	h.svc = NewRecordService(dealsCatalog(), h.store).WithBeforeHooks(h.loader, sink).WithEngine(h.eng).WithAuditor(h.audits)
	return h
}

func twoDeals() map[string]map[string]any {
	return map[string]map[string]any{
		"won":  {"name": "Deal Won", "stage": "Closed Won"},
		"open": {"name": "Deal Open", "stage": "Prospecting"},
	}
}

// T065/T067: Delete judges the record as stored; a refused delete writes nothing — no row gone, no
// audit, no after_delete event — and leaves exactly one blocked run pointing at the record.
func TestDelete_BeforeDeleteVeto(t *testing.T) {
	// A before_save rule that would match the Won deal: a delete must never load it.
	h := newDelHarness(t, twoDeals(), wonNoDelete(), wonNeedsDateOn("deals", ""))
	ctx := context.Background()

	ve := wantVeto(t, h.svc.Delete(ctx, rep, "t1", "deals", "won"))
	if ve.Msg != delVetoMsg || ve.Field != "" {
		t.Fatalf("veto = msg %q field %q", ve.Msg, ve.Field)
	}
	if vs := violationsOf(t, ve); len(vs) != 1 || vs[0] != (workflow.Violation{Message: delVetoMsg}) {
		t.Fatalf("violations = %+v, want only the delete rule, no rule id for a non-admin", vs)
	}
	if h.store.deletedIDs() != "[]" || len(h.eng.evts) != 0 || len(h.audits.events) != 0 {
		t.Fatalf("refused delete wrote: deleted %s, events %d, audits %d", h.store.deletedIDs(), len(h.eng.evts), len(h.audits.events))
	}

	if err := h.svc.Delete(ctx, rep, "t1", "deals", "open"); err != nil {
		t.Fatalf("Delete open deal: %v", err)
	}
	if h.store.deletedIDs() != "[open]" || len(h.eng.evts) != 1 || h.eng.evts[0].Trigger != workflow.TriggerAfterDelete {
		t.Fatalf("allowed delete: deleted %s, events %+v", h.store.deletedIDs(), h.eng.evts)
	}

	admin := RecordScope{UserID: "u-admin", AccessLevel: "all", IsAdmin: true}
	ve = wantVeto(t, h.svc.Delete(ctx, admin, "t1", "deals", "won"))
	if vs := violationsOf(t, ve); len(vs) != 1 || vs[0].RuleID != "rule-del" {
		t.Fatalf("admin violations = %+v, want the rule id (admins are judged too)", vs)
	}

	runs := h.runs()
	if len(runs) != 2 {
		t.Fatalf("runs = %+v, want one blocked run per refused delete", runs)
	}
	for _, r := range runs {
		if r.TenantID != "t1" || r.WorkflowID != "rule-del" || r.TriggerType != "before_delete" || r.RecordID != "won" ||
			r.Status != workflow.RunBlocked || string(r.ActionsResult) != `[{"type":"veto","ok":true}]` || r.Error != "" {
			t.Fatalf("run = %+v actions %s", r, r.ActionsResult)
		}
	}
}

// T058: bulk delete runs Delete per row — the Won deal is refused with the rule's words, the other
// is deleted, and each refused row leaves its own run (FR-031).
func TestDeleteBulk_BeforeDeleteVeto_PerRow(t *testing.T) {
	h := newDelHarness(t, twoDeals(), wonNoDelete())
	res, err := h.svc.DeleteBulk(context.Background(), rep, "t1", "deals", []string{"won", "open"})
	if err != nil {
		t.Fatalf("DeleteBulk: %v", err)
	}
	if res.Deleted != 1 || res.Failed != 1 || len(res.Results) != 2 {
		t.Fatalf("deleted/failed = %d/%d, want 1/1", res.Deleted, res.Failed)
	}
	if r := res.Results[0]; r.ID != "won" || r.OK || r.Code != "RECORD_HOOK_VETO" || r.Error != delVetoMsg {
		t.Fatalf("row won = %+v, want RECORD_HOOK_VETO %q", r, delVetoMsg)
	}
	if !res.Results[1].OK || h.store.deletedIDs() != "[open]" {
		t.Fatalf("row open = %+v, deleted %s", res.Results[1], h.store.deletedIDs())
	}
	if runs := h.runs(); len(runs) != 1 || runs[0].RecordID != "won" || runs[0].Status != workflow.RunBlocked {
		t.Fatalf("runs = %+v, want one blocked run for the refused row", runs)
	}

	t.Run("unreadable rules abort the call, nothing deleted", func(t *testing.T) {
		h := newDelHarness(t, twoDeals(), wonNoDelete())
		h.loader.err = errors.New("sys_workflows: connection refused")
		_, err := h.svc.DeleteBulk(context.Background(), rep, "t1", "deals", []string{"open", "won"})
		if !errors.Is(err, ErrRecordHooksUnavailable) || h.store.deletedIDs() != "[]" {
			t.Fatalf("err = %v, deleted %s — want ErrRecordHooksUnavailable and nothing deleted", err, h.store.deletedIDs())
		}
	})
}

// T059: with before_delete rules, a record that cannot be read is not deleted unjudged (503); a
// missing one is still 404; without rules the old best-effort read stays exactly as it was.
func TestDelete_BeforeReadError_FailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("rules + unreadable record ⇒ unavailable, nothing deleted", func(t *testing.T) {
		h := newDelHarness(t, twoDeals(), wonNoDelete())
		h.store.getErr = errors.New("dynrepo.GetRecord: i/o timeout")
		if err := h.svc.Delete(ctx, rep, "t1", "deals", "open"); !errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordHooksUnavailable", err)
		}
		if h.store.deletedIDs() != "[]" {
			t.Fatal("an unjudged delete reached the store")
		}
	})
	t.Run("rules unreadable ⇒ unavailable, nothing deleted", func(t *testing.T) {
		h := newDelHarness(t, twoDeals(), wonNoDelete())
		h.loader.err = errors.New("sys_workflows: connection refused")
		if err := h.svc.Delete(ctx, rep, "t1", "deals", "open"); !errors.Is(err, ErrRecordHooksUnavailable) || h.store.deletedIDs() != "[]" {
			t.Fatalf("err = %v deleted %s, want ErrRecordHooksUnavailable and nothing deleted", err, h.store.deletedIDs())
		}
	})
	t.Run("rules + missing record ⇒ 404 as before", func(t *testing.T) {
		h := newDelHarness(t, twoDeals(), wonNoDelete())
		err := h.svc.Delete(ctx, rep, "t1", "deals", "gone")
		if !errors.Is(err, ErrRecordNotFound) || errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordNotFound", err)
		}
	})
	t.Run("no rules + unreadable record ⇒ best-effort delete as before", func(t *testing.T) {
		h := newDelHarness(t, twoDeals())
		h.store.getErr = errors.New("dynrepo.GetRecord: i/o timeout")
		if err := h.svc.Delete(ctx, rep, "t1", "deals", "won"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if h.store.deletedIDs() != "[won]" {
			t.Fatalf("deleted %s, want [won]", h.store.deletedIDs())
		}
	})
}

// T062: an exempt system write that a veto matches is written, and the history says why the rule
// did not refuse it — a skipped run carrying the origin as the reason (FR-019, US8 scenario 3).
func TestApprovalOutcome_Exempt_WritesSkippedRun(t *testing.T) {
	for _, tc := range []struct {
		origin string
		write  func(*RecordService) error
	}{
		{originApprovalOutcome, func(svc *RecordService) error {
			return NewApprovalOutcomeWriter(svc).SetField(context.Background(), "t1", "deals", "q1", "stage", "Accepted", "u-approver")
		}},
		{originPaymentMatch, func(svc *RecordService) error {
			ih := &InboundHookService{rec: svc, logger: zap.NewNop()}
			hook := &inboundhook.Hook{TenantID: "t1", ID: "h1", CreatedBy: "u-hook-owner", PaymentMatch: &inboundhook.PaymentMatch{
				TargetEntity: "deals", RefField: "name", RefPattern: `INV-\d+`, MemoSource: "data.desc",
				StatusField: "stage", StatusValue: "Accepted",
			}}
			if status, _, reason := ih.runPaymentMatch(context.Background(), hook, map[string]any{"data": map[string]any{"desc": "CK INV-0042"}}); status != "matched" {
				return errors.New(reason)
			}
			return nil
		}},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			sink, drain := newRunSink(t)
			store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"name": "INV-0042", "stage": "Pending"}}
			svc := NewRecordService(dealsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{quoteAcceptedVeto()}}, sink)
			if err := tc.write(svc); err != nil {
				t.Fatalf("exempt write refused: %v", err)
			}
			runs := drain()
			want := fmt.Sprintf(`[{"type":"veto","field":"stage","ok":true,"skipped":%q}]`, tc.origin)
			if len(runs) != 1 || runs[0].Status != workflow.RunSkipped || runs[0].WorkflowID != "rule-quote" ||
				runs[0].RecordID != "q1" || runs[0].TriggerType != "before_save" || string(runs[0].ActionsResult) != want {
				t.Fatalf("runs = %+v, want one skipped run %s", runs, want)
			}
		})
	}
}

const secretValue = "SECRET-7731"

// T061: no run a before rule leaves carries a field value — every path, one sink. actions_result is
// either [{type, field, ok, error, skipped}] (one write) or {blocked, applied, failed} (one batch).
func TestBlockedRun_NoFieldValues(t *testing.T) {
	sink, drain := newRunSink(t)
	ctx := context.Background()

	upd := NewRecordService(dealsCatalog(), &vetoStore{crCountingStore: crCountingStore{hardCap: 10},
		before: map[string]any{"name": secretValue, "stage": "Proposal"}}).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonNeedsDate(""), quoteAcceptedVeto()}}, sink)
	wantVeto(t, upd.Update(ctx, rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won", "probability": 7.0}, nil))
	if err := NewApprovalOutcomeWriter(upd).SetField(ctx, "t1", "deals", "d1", "stage", "Accepted", "u-approver"); err != nil {
		t.Fatalf("approval write: %v", err)
	}

	crt, _, _, _ := newCreateHookService(wonNeedsDateOn("opps", ""))
	crt.WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonNeedsDateOn("opps", "")}}, sink)
	wantVeto(t, createOpp(crt, rep, map[string]any{"name": secretValue, "stage": "Closed Won", "amount": 4242.0}))

	del := NewRecordService(dealsCatalog(), &delStore{crCountingStore: crCountingStore{hardCap: 10},
		rows: map[string]map[string]any{"won": {"name": secretValue, "stage": "Closed Won"}}}).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonNoDelete()}}, sink)
	wantVeto(t, del.Delete(ctx, rep, "t1", "deals", "won"))

	bulk := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t).
		WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{badName("")}}, sink)
	if _, err := bulk.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", widgets(secretValue, "BAD"), BulkModeAtomic); err != nil {
		t.Fatalf("CreateBulk: %v", err)
	}

	imp := importWithSink(sink, wonNeedsDateOn("opps", ""))
	if _, err := importOpps(t, imp, "name,stage,close_date\n"+secretValue+",Closed Won,\n", ""); err != nil {
		t.Fatalf("Import: %v", err)
	}

	runs := drain()
	statuses := map[workflow.RunStatus]int{}
	for _, r := range runs {
		statuses[r.Status]++
		raw := string(r.ActionsResult) + r.Error
		for _, v := range []string{secretValue, "Closed Won", "Accepted", "BAD", "4242", "7"} {
			if strings.Contains(raw, v) {
				t.Fatalf("run %s/%s carries the value %q: %s", r.WorkflowID, r.Status, v, raw)
			}
		}
		if keys := jsonKeys(t, r.ActionsResult); keys != "applied,blocked,failed" &&
			!subsetOf(keys, "error,field,ok,skipped,type") {
			t.Fatalf("run %s/%s actions_result keys = %s", r.WorkflowID, r.Status, keys)
		}
	}
	if len(runs) != 6 || statuses[workflow.RunBlocked] != 5 || statuses[workflow.RunSkipped] != 1 {
		t.Fatalf("runs = %d %v, want 5 blocked (update, create, delete, batch, import) + 1 skipped", len(runs), statuses)
	}
}

// jsonKeys returns the sorted, comma-joined keys of a JSON object or of every object in a JSON array.
func jsonKeys(t *testing.T, raw []byte) string {
	t.Helper()
	var objs []map[string]any
	if err := json.Unmarshal(raw, &objs); err != nil {
		var one map[string]any
		if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatalf("actions_result %s is neither an array nor an object", raw)
		}
		objs = []map[string]any{one}
	}
	seen := map[string]bool{}
	for _, o := range objs {
		for k := range o {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func subsetOf(keys, allowed string) bool {
	for _, k := range strings.Split(keys, ",") {
		if !strings.Contains(","+allowed+",", ","+k+",") {
			return false
		}
	}
	return true
}

func importWithSink(sink *workflow.RunSink, rules ...workflow.Workflow) *ImportService {
	cat := &fakeImportCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-o", Name: "opps"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text", IsRequired: true},
			{Name: "stage", FieldType: "text"},
			{Name: "close_date", FieldType: "date"},
		},
	}
	return NewImportService(cat, &fakeImportStore{}, nil).WithBeforeHooks(&vetoLoader{rules: rules}, sink)
}

// nameHasX refuses any row whose name contains X — a second rule for the per-batch tally.
func nameHasX(entity string) workflow.Workflow {
	return workflow.Workflow{ID: "rule-x-" + entity, TenantID: "t1", EntityName: entity, Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{Rules: []workflow.Rule{{Field: "name", Op: workflow.OpContains, Value: "X"}}},
		Actions:   []workflow.Action{{Type: workflow.ActionVeto, Message: "Tên không được chứa X"}}}
}

// T061: an all-or-nothing batch and a CSV import leave ONE summary run per rule that refused rows,
// with the count — not one run per row (FR-031). A batch nothing refused leaves no run at all.
func TestBulkBlockedRun_OneRowPerRulePerBatch(t *testing.T) {
	ctx := context.Background()
	summary := func(t *testing.T, runs []workflow.RunInput) string {
		t.Helper()
		out := make([]string, len(runs))
		for i, r := range runs {
			if r.Status != workflow.RunBlocked || r.RecordID != "" || r.TriggerType != "before_save" {
				t.Fatalf("run = %+v, want a blocked before_save run with no record", r)
			}
			out[i] = r.WorkflowID + "=" + string(r.ActionsResult)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	t.Run("bulk create atomic", func(t *testing.T) {
		sink, drain := newRunSink(t)
		wX := nameHasX("widgets")
		wX.TenantID = "tenant-1"
		svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t).
			WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{badName(""), wX}}, sink)
		if _, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "BAD", "BAD", "X1"), BulkModeAtomic); err != nil {
			t.Fatalf("CreateBulk: %v", err)
		}
		if _, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "B"), BulkModeAtomic); err != nil {
			t.Fatalf("compliant CreateBulk: %v", err)
		}
		want := `rule-bad={"applied":0,"blocked":2,"failed":0} rule-x-widgets={"applied":0,"blocked":1,"failed":0}`
		if got := summary(t, drain()); got != want {
			t.Fatalf("runs = %s, want %s", got, want)
		}
	})

	t.Run("bulk create partial goes through Create: one run per refused row", func(t *testing.T) {
		sink, drain := newRunSink(t)
		svc := newBulkSvc(&bulkTestStore{}, &fakeSequenceClaimer{}, &fakeBulkInserter{}, nil, t).
			WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{badName("")}}, sink)
		if _, err := svc.CreateBulk(ctx, bulkRoleScope(), "tenant-1", "u1", "widgets", widgets("A", "BAD", "BAD"), BulkModePartial); err != nil {
			t.Fatalf("CreateBulk: %v", err)
		}
		want := `rule-bad=[{"type":"veto","field":"name","ok":true}] rule-bad=[{"type":"veto","field":"name","ok":true}]`
		if got := summary(t, drain()); got != want {
			t.Fatalf("runs = %s, want %s", got, want)
		}
	})

	t.Run("CSV import", func(t *testing.T) {
		sink, drain := newRunSink(t)
		imp := importWithSink(sink, wonNeedsDateOn("opps", ""), nameHasX("opps"))
		csv := "name,stage,close_date\nA,Proposal,\nB,Closed Won,\nC,Closed Won,\nX1,Proposal,\nD,Closed Won,2026-10-01\n"
		res, err := importOpps(t, imp, csv, "")
		if err != nil || res.Inserted != 2 {
			t.Fatalf("import = %+v, %v — want 2 inserted", res, err)
		}
		want := `rule-won-opps={"applied":0,"blocked":2,"failed":0} rule-x-opps={"applied":0,"blocked":1,"failed":0}`
		if got := summary(t, drain()); got != want {
			t.Fatalf("runs = %s, want %s", got, want)
		}
	})
}
