package services

// specs/048 S1b — the before_save veto on RecordService.Update, over a fake store and a fake rule
// loader (no MySQL). record_hooks_integration_test.go proves the same on real MySQL.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

const hookVetoMsg = "Chốt Thắng phải có Ngày chốt"

// vetoStore serves one stored record to Update's reads and counts every store call, so a test can
// prove a vetoed save never reached UpdateRecord and a rule-less entity costs no extra call.
type vetoStore struct {
	crCountingStore
	mu      sync.Mutex
	before  map[string]any
	getErr  error
	gets    int
	updates []map[string]any
}

func (s *vetoStore) GetRecord(_ context.Context, _, tenantID, id string, _ dynrepo.RowScope) (*dynrepo.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return &dynrepo.Record{ID: id, TenantID: tenantID, Data: cloneData(s.before), OwnerID: "u-owner", CreatedAt: time.Unix(0, 0)}, nil
}

func (s *vetoStore) UpdateRecord(_ context.Context, _, _, _, _ string, _ dynrepo.RowScope, data map[string]any, _ *int, _ []dynrepo.UniqueFieldSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, data)
	return nil
}

// ListRecords serves the stored record to the payment-match lookup.
func (s *vetoStore) ListRecords(_ context.Context, _, tenantID string, _ dynrepo.RowScope, _ dynrepo.ListParams) ([]dynrepo.Record, int, error) {
	return []dynrepo.Record{{ID: "q1", TenantID: tenantID, Data: cloneData(s.before)}}, 1, nil
}

func (s *vetoStore) counts() (gets, updates int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets, len(s.updates)
}

// vetoLoader serves fixed rules per trigger and counts calls; err fails every load.
type vetoLoader struct {
	mu    sync.Mutex
	rules []workflow.Workflow
	err   error
	calls int
}

func (l *vetoLoader) ListActive(_ context.Context, _, entity string, t workflow.Trigger) ([]workflow.Workflow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	var out []workflow.Workflow
	for _, wf := range l.rules {
		if wf.EntityName == entity && wf.Trigger == t {
			out = append(out, wf)
		}
	}
	return out, nil
}

func dealsCatalog() *fakeRecordCatalog {
	return &fakeRecordCatalog{
		entity: &metadata.EntityDefinition{ID: "ent-d", TenantID: "t1", Name: "deals", DataTable: "obj_deals"},
		fields: []metadata.FieldDefinition{
			{Name: "name", FieldType: "text"},
			{Name: "stage", FieldType: "text"},
			{Name: "close_date", FieldType: "date"},
			{Name: "probability", FieldType: "number"},
		},
	}
}

// wonNeedsDate is the US1 rule: stage changedTo Closed Won AND close_date empty ⇒ veto on close_date.
func wonNeedsDate(saveOn string) workflow.Workflow {
	return workflow.Workflow{ID: "rule-won", TenantID: "t1", EntityName: "deals", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{SaveOn: saveOn, Rules: []workflow.Rule{
			{Field: "stage", Op: workflow.OpChangedTo, Value: "Closed Won"},
			{Field: "close_date", Op: workflow.OpIsNull},
		}},
		Actions: []workflow.Action{{Type: workflow.ActionVeto, Field: "close_date", Message: hookVetoMsg}}}
}

var rep = RecordScope{UserID: "u-rep", AccessLevel: "all", RoleID: "role-rep"}

func newHookService(before map[string]any, rules ...workflow.Workflow) (*RecordService, *vetoStore, *vetoLoader) {
	store := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: before}
	loader := &vetoLoader{rules: rules}
	return NewRecordService(dealsCatalog(), store).WithBeforeHooks(loader, nil), store, loader
}

func wantVeto(t *testing.T, err error) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != "RECORD_HOOK_VETO" {
		t.Fatalf("err = %v, want 422 RECORD_HOOK_VETO", err)
	}
	return ve
}

func violationsOf(t *testing.T, ve *ValidationError) []workflow.Violation {
	t.Helper()
	vs, ok := ve.Details["violations"].([]workflow.Violation)
	if !ok {
		t.Fatalf("details.violations = %#v, want []workflow.Violation", ve.Details["violations"])
	}
	return vs
}

// T027: a PUT the rule refuses never reaches the store; the error is the 422 the form anchors.
func TestRecordUpdate_BeforeSaveVeto(t *testing.T) {
	svc, store, _ := newHookService(map[string]any{"name": "Deal", "stage": "Proposal"}, wonNeedsDate(""))
	err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil)
	ve := wantVeto(t, err)
	if _, updates := store.counts(); updates != 0 {
		t.Fatalf("UpdateRecord called %d times on a vetoed save, want 0", updates)
	}
	if ve.Msg != hookVetoMsg || ve.Field != "close_date" || ve.Details["field"] != "close_date" {
		t.Fatalf("veto = msg %q field %q details %v", ve.Msg, ve.Field, ve.Details)
	}
	vs := violationsOf(t, ve)
	if len(vs) != 1 || vs[0] != (workflow.Violation{Field: "close_date", Message: hookVetoMsg}) {
		t.Fatalf("violations = %+v (a non-admin never sees the rule id)", vs)
	}

	t.Run("saveOn=update rules apply to updates too", func(t *testing.T) {
		svc, store, _ := newHookService(map[string]any{"stage": "Proposal"}, wonNeedsDate(workflow.SaveOnUpdate))
		wantVeto(t, svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil))
		if _, updates := store.counts(); updates != 0 {
			t.Fatal("vetoed save reached the store")
		}
	})
	t.Run("a create-only rule does not judge an update", func(t *testing.T) {
		createOnly := wonNeedsDate(workflow.SaveOnCreate)
		createOnly.Condition.Rules = []workflow.Rule{{Field: "stage", Op: workflow.OpEq, Value: "Closed Won"}}
		svc, store, _ := newHookService(map[string]any{"stage": "Proposal"}, createOnly)
		if err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, updates := store.counts(); updates != 1 {
			t.Fatalf("UpdateRecord calls = %d, want 1", updates)
		}
	})
	t.Run("admin callers are judged too, and see the rule id", func(t *testing.T) {
		svc, _, _ := newHookService(map[string]any{"stage": "Proposal"}, wonNeedsDate(""))
		admin := RecordScope{UserID: "u-admin", AccessLevel: "all", IsAdmin: true}
		ve := wantVeto(t, svc.Update(context.Background(), admin, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil))
		if vs := violationsOf(t, ve); len(vs) != 1 || vs[0].RuleID != "rule-won" {
			t.Fatalf("admin violations = %+v, want ruleId rule-won", vs)
		}
	})
	t.Run("every matching veto is reported", func(t *testing.T) {
		second := workflow.Workflow{ID: "rule-prob", TenantID: "t1", EntityName: "deals", Trigger: workflow.TriggerBeforeSave, IsActive: true,
			Condition: &workflow.Condition{Rules: []workflow.Rule{{Field: "probability", Op: workflow.OpLt, Value: 10.0}}},
			Actions:   []workflow.Action{{Type: workflow.ActionVeto, Field: "probability", Message: "Xác suất phải từ 10%"}}}
		svc, _, _ := newHookService(map[string]any{"stage": "Proposal", "probability": 50.0}, wonNeedsDate(""), second)
		ve := wantVeto(t, svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won", "probability": 5.0}, nil))
		if vs := violationsOf(t, ve); len(vs) != 2 || vs[0].Field != "close_date" || vs[1].Field != "probability" {
			t.Fatalf("violations = %+v, want close_date then probability", vs)
		}
	})
}

// T028: the condition reads the record as it will be stored, not just the keys the PUT sent.
func TestRecordUpdate_BeforeSaveVeto_EvaluatesMergedImage(t *testing.T) {
	t.Run("close_date empty on the record ⇒ vetoed", func(t *testing.T) {
		svc, _, _ := newHookService(map[string]any{"name": "Deal", "stage": "Proposal"}, wonNeedsDate(""))
		wantVeto(t, svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil))
	})
	t.Run("close_date already on the record ⇒ saved", func(t *testing.T) {
		svc, store, _ := newHookService(map[string]any{"name": "Deal", "stage": "Proposal", "close_date": "2026-10-01"}, wonNeedsDate(""))
		if err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, updates := store.counts(); updates != 1 {
			t.Fatalf("UpdateRecord calls = %d, want 1", updates)
		}
	})
	t.Run("clearing close_date on a won deal is judged on the cleared record", func(t *testing.T) {
		// eq, not changedTo: "a won deal always has a close date".
		rule := wonNeedsDate("")
		rule.Condition.Rules[0].Op = workflow.OpEq
		svc, _, _ := newHookService(map[string]any{"stage": "Closed Won", "close_date": "2026-10-01"}, rule)
		wantVeto(t, svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"close_date": nil}, nil))
	})
	t.Run("a deal already won with no date saves an unrelated edit", func(t *testing.T) {
		svc, store, _ := newHookService(map[string]any{"name": "Deal", "stage": "Closed Won"}, wonNeedsDate(""))
		if err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"name": "Renamed"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, updates := store.counts(); updates != 1 {
			t.Fatal("unrelated edit did not reach the store")
		}
	})
}

// T029: a violation anchored to a field the caller's role cannot see loses its anchor.
func TestRecordUpdate_BeforeSaveVeto_HiddenFieldUnanchored(t *testing.T) {
	put := func(t *testing.T, acl *fakeListHiddenACL, scope RecordScope, before map[string]any) error {
		t.Helper()
		svc, _, _ := newHookService(before, wonNeedsDate(""))
		svc.WithFieldACL(acl)
		return svc.Update(context.Background(), scope, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil)
	}
	hiddenDate := map[string]map[string]struct{}{"deals": {"close_date": {}}}

	t.Run("hidden anchor dropped from field, details.field and the violation", func(t *testing.T) {
		acl := &fakeListHiddenACL{hidden: hiddenDate}
		ve := wantVeto(t, put(t, acl, rep, map[string]any{"stage": "Proposal"}))
		if _, has := ve.Details["field"]; has || ve.Field != "" {
			t.Fatalf("hidden field leaked: Field %q details %v", ve.Field, ve.Details)
		}
		if vs := violationsOf(t, ve); len(vs) != 1 || vs[0].Field != "" || vs[0].Message != hookVetoMsg {
			t.Fatalf("violations = %+v, want the message without an anchor", vs)
		}
	})
	t.Run("visible anchor kept", func(t *testing.T) {
		acl := &fakeListHiddenACL{}
		ve := wantVeto(t, put(t, acl, rep, map[string]any{"stage": "Proposal"}))
		if ve.Details["field"] != "close_date" {
			t.Fatalf("details = %v, want field close_date", ve.Details)
		}
	})
	t.Run("hidden-field lookup failing drops every anchor, still 422", func(t *testing.T) {
		acl := &fakeListHiddenACL{err: errors.New("acl store down")}
		ve := wantVeto(t, put(t, acl, rep, map[string]any{"stage": "Proposal"}))
		if _, has := ve.Details["field"]; has || violationsOf(t, ve)[0].Field != "" {
			t.Fatalf("anchor kept although visibility is unknown: %v", ve.Details)
		}
	})
	t.Run("no violation, no hidden-field lookup", func(t *testing.T) {
		acl := &fakeListHiddenACL{}
		if err := put(t, acl, rep, map[string]any{"stage": "Proposal", "close_date": "2026-10-01"}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if acl.hiddenCalls != 0 {
			t.Fatalf("HiddenFields called %d times without a violation, want 0", acl.hiddenCalls)
		}
	})
	t.Run("rule id only for admins", func(t *testing.T) {
		ve := wantVeto(t, put(t, &fakeListHiddenACL{}, rep, map[string]any{"stage": "Proposal"}))
		if violationsOf(t, ve)[0].RuleID != "" {
			t.Fatal("non-admin saw the rule id")
		}
		admin := RecordScope{UserID: "u-admin", AccessLevel: "all", IsAdmin: true}
		ve = wantVeto(t, put(t, &fakeListHiddenACL{}, admin, map[string]any{"stage": "Proposal"}))
		if violationsOf(t, ve)[0].RuleID != "rule-won" {
			t.Fatal("admin did not see the rule id")
		}
	})
}

// T030: without the rules, or without the record they judge, the save is refused — never let
// through unjudged. With no rule at all, the old best-effort read stays exactly as it was.
func TestRecordUpdate_BeforeRulesLoadError_FailsClosed503(t *testing.T) {
	svc, store, loader := newHookService(map[string]any{"stage": "Proposal"}, wonNeedsDate(""))
	loader.err = errors.New("sys_workflows: connection refused")
	err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"name": "x"}, nil)
	if !errors.Is(err, ErrRecordHooksUnavailable) {
		t.Fatalf("err = %v, want ErrRecordHooksUnavailable", err)
	}
	if _, updates := store.counts(); updates != 0 {
		t.Fatal("a save with unreadable rules reached the store")
	}
}

func TestRecordUpdate_BeforeReadError_FailsClosed(t *testing.T) {
	t.Run("rules + unreadable record ⇒ unavailable", func(t *testing.T) {
		svc, store, _ := newHookService(nil, wonNeedsDate(""))
		store.getErr = errors.New("dynrepo.GetRecord: i/o timeout")
		err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil)
		if !errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordHooksUnavailable", err)
		}
		if _, updates := store.counts(); updates != 0 {
			t.Fatal("unjudged save reached the store")
		}
	})
	t.Run("rules + missing record ⇒ 404 as before", func(t *testing.T) {
		svc, store, _ := newHookService(nil, wonNeedsDate(""))
		store.getErr = dynrepo.ErrRecordNotFound
		err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil)
		if !errors.Is(err, ErrRecordNotFound) || errors.Is(err, ErrRecordHooksUnavailable) {
			t.Fatalf("err = %v, want ErrRecordNotFound", err)
		}
	})
	t.Run("no rules + unreadable record ⇒ best-effort write as before", func(t *testing.T) {
		svc, store, _ := newHookService(nil)
		store.getErr = errors.New("dynrepo.GetRecord: i/o timeout")
		if err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if _, updates := store.counts(); updates != 1 {
			t.Fatal("rule-less save did not reach the store")
		}
	})
}

// sharedOnlyVetoStore hides the stored record from every read that does not carry the manual-share
// flag — the row-scope miss of a caller who sees the record only because it was shared with them.
type sharedOnlyVetoStore struct{ vetoStore }

func (s *sharedOnlyVetoStore) GetRecord(ctx context.Context, entity, tenantID, id string, sc dynrepo.RowScope) (*dynrepo.Record, error) {
	if !sc.IncludeShares {
		return nil, dynrepo.ErrRecordNotFound
	}
	return s.vetoStore.GetRecord(ctx, entity, tenantID, id, sc)
}

// #314: the error code must not depend on whether the entity has a before_save rule. Update itself
// answers 403 up front for a record the caller sees only through a share (#226), so
// checkBeforeUpdate — which re-judges a miss on its own — is exercised directly: a miss on a shared
// record is 403 RECORD_SHARED_READ_ONLY, a miss on a record that is not shared stays 404.
func TestCheckBeforeUpdate_MissOnSharedRecordIs403(t *testing.T) {
	owner := RecordScope{UserID: "u-b", AccessLevel: "owner", RoleID: "role-rep"}
	for _, tc := range []struct {
		name   string
		shared bool
		want   error
	}{
		{"shared with caller ⇒ 403", true, ErrRecordSharedReadOnly},
		{"not shared ⇒ 404", false, ErrRecordNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &sharedOnlyVetoStore{vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: map[string]any{"stage": "Proposal"}}}
			if !tc.shared {
				store.getErr = dynrepo.ErrRecordNotFound // the share probe misses too
			}
			svc := NewRecordService(dealsCatalog(), store).WithBeforeHooks(&vetoLoader{rules: []workflow.Workflow{wonNeedsDate("")}}, nil)
			_, err := svc.checkBeforeUpdate(context.Background(), owner, "t1", "deals", "d1", nil, nil, nil, nil, false, ErrRecordNotFound)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// T031: an entity with no before rule pays one in-process cache lookup and no store call.
func TestRecordUpdate_NoBeforeRules_NoExtraQueries(t *testing.T) {
	before := map[string]any{"stage": "Proposal"}
	plain := &vetoStore{crCountingStore: crCountingStore{hardCap: 10}, before: before}
	if err := NewRecordService(dealsCatalog(), plain).Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
		t.Fatal(err)
	}
	baseGets, baseUpdates := plain.counts()

	inner := &vetoLoader{} // no rules at all
	svc, store, _ := newHookService(before)
	svc.WithBeforeHooks(workflow.NewCachedLoader(inner, time.Hour), nil)
	for i := 0; i < 3; i++ {
		if err := svc.Update(context.Background(), rep, "t1", "deals", "d1", map[string]any{"stage": "Closed Won"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("rule loads = %d over 3 saves, want 1 (the empty list is cached)", inner.calls)
	}
	gets, updates := store.counts()
	if gets != 3*baseGets || updates != 3*baseUpdates {
		t.Fatalf("store calls per save = %d reads / %d writes, want %d / %d as without hooks",
			gets/3, updates/3, baseGets, baseUpdates)
	}
}

// quoteAcceptedVeto: "a quote is only Accepted through approval" (US8).
func quoteAcceptedVeto() workflow.Workflow {
	return workflow.Workflow{ID: "rule-quote", TenantID: "t1", EntityName: "deals", Trigger: workflow.TriggerBeforeSave, IsActive: true,
		Condition: &workflow.Condition{Rules: []workflow.Rule{{Field: "stage", Op: workflow.OpChangedTo, Value: "Accepted"}}},
		Actions:   []workflow.Action{{Type: workflow.ActionVeto, Field: "stage", Message: "Chỉ duyệt mới được Chấp nhận"}}}
}

// T032: the decided outcome of an approval and a matched payment are written even when a veto
// matches; the same change made by hand is refused.
func TestApprovalOutcome_BypassesVeto(t *testing.T) {
	svc, store, _ := newHookService(map[string]any{"stage": "Pending"}, quoteAcceptedVeto())
	if err := NewApprovalOutcomeWriter(svc).SetField(context.Background(), "t1", "deals", "q1", "stage", "Accepted", "u-approver"); err != nil {
		t.Fatalf("approval outcome write-back refused: %v", err)
	}
	if _, updates := store.counts(); updates != 1 {
		t.Fatalf("UpdateRecord calls = %d, want 1", updates)
	}
	wantVeto(t, svc.Update(context.Background(), rep, "t1", "deals", "q1", map[string]any{"stage": "Accepted"}, nil))
	admin := RecordScope{UserID: "u-admin", AccessLevel: "all", IsAdmin: true}
	wantVeto(t, svc.Update(context.Background(), admin, "t1", "deals", "q1", map[string]any{"stage": "Accepted"}, nil))
}

func TestPaymentMatch_BypassesVeto(t *testing.T) {
	svc, store, _ := newHookService(map[string]any{"name": "INV-0042", "stage": "Pending"}, quoteAcceptedVeto())
	ih := &InboundHookService{rec: svc, logger: zap.NewNop()}
	hook := &inboundhook.Hook{TenantID: "t1", ID: "h1", CreatedBy: "u-hook-owner", PaymentMatch: &inboundhook.PaymentMatch{
		TargetEntity: "deals", RefField: "name", RefPattern: `INV-\d+`, MemoSource: "data.desc",
		StatusField: "stage", StatusValue: "Accepted",
	}}
	status, id, reason := ih.runPaymentMatch(context.Background(), hook, map[string]any{"data": map[string]any{"desc": "CK INV-0042"}})
	if status != "matched" || id != "q1" {
		t.Fatalf("payment match = %q %q %q, want matched q1", status, id, reason)
	}
	if _, updates := store.counts(); updates != 1 {
		t.Fatalf("UpdateRecord calls = %d, want 1", updates)
	}
	// The same hook owner setting the same value by hand is an ordinary write: refused.
	owner := RecordScope{UserID: "u-hook-owner", AccessLevel: "all", IsAdmin: true}
	wantVeto(t, svc.Update(context.Background(), owner, "t1", "deals", "q1", map[string]any{"stage": "Accepted"}, nil))
}
