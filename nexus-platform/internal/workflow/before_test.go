package workflow

// specs/048 S1a — the two "before" triggers exist as config (ValidTrigger / IsBeforeTrigger) and the
// rule cache forgets them on Invalidate, exactly like the after-commit buckets. Nothing here runs a
// before rule yet (S1b); these tests pin the foundation the runtime slices build on.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidTrigger_BeforeTriggers(t *testing.T) {
	tests := []struct {
		trigger Trigger
		want    bool
	}{
		{TriggerBeforeSave, true},
		{TriggerBeforeDelete, true},
		{TriggerAfterCreate, true},
		{TriggerDateOffset, true},
		{"before_create", false}, // not a trigger: create/update is picked with condition.saveOn
		{"", false},
	}
	for _, tt := range tests {
		if got := ValidTrigger(tt.trigger); got != tt.want {
			t.Errorf("ValidTrigger(%q) = %v, want %v", tt.trigger, got, tt.want)
		}
	}
}

func TestIsBeforeTrigger(t *testing.T) {
	tests := []struct {
		trigger Trigger
		want    bool
	}{
		{TriggerBeforeSave, true},
		{TriggerBeforeDelete, true},
		{TriggerAfterCreate, false},
		{TriggerAfterUpdate, false},
		{TriggerAfterDelete, false},
		{TriggerScheduled, false},
		{TriggerDateOffset, false},
		{"before_create", false},
	}
	for _, tt := range tests {
		if got := IsBeforeTrigger(tt.trigger); got != tt.want {
			t.Errorf("IsBeforeTrigger(%q) = %v, want %v", tt.trigger, got, tt.want)
		}
	}
}

// countingLoader counts calls that reach the inner store, per trigger.
type countingLoader struct{ calls atomic.Int64 }

func (l *countingLoader) ListActive(context.Context, string, string, Trigger) ([]Workflow, error) {
	l.calls.Add(1)
	return nil, nil
}

// A disabled/edited before rule must stop (or start) applying on the next save, not up to 30 s later:
// the admin write services call Invalidate(tenant, entity) after commit, and that has to drop the
// before_* buckets too. Without it a veto the admin just switched off keeps blocking saves.
func TestCachedLoader_Invalidate_CoversBeforeTriggers(t *testing.T) {
	for _, trig := range []Trigger{TriggerBeforeSave, TriggerBeforeDelete} {
		t.Run(string(trig), func(t *testing.T) {
			inner := &countingLoader{}
			l := NewCachedLoader(inner, time.Hour)
			ctx := context.Background()
			if _, err := l.ListActive(ctx, "t1", "deals", trig); err != nil {
				t.Fatal(err)
			}
			if _, err := l.ListActive(ctx, "t1", "deals", trig); err != nil {
				t.Fatal(err)
			}
			if n := inner.calls.Load(); n != 1 {
				t.Fatalf("warm cache: inner loader called %d times, want 1 (an empty list is cached too)", n)
			}
			l.Invalidate("t1", "deals")
			if _, err := l.ListActive(ctx, "t1", "deals", trig); err != nil {
				t.Fatal(err)
			}
			if n := inner.calls.Load(); n != 2 {
				t.Fatalf("after Invalidate: inner loader called %d times, want 2 — the %s bucket survived", n, trig)
			}
		})
	}
}

// vetoRule builds an active before rule whose only action is a veto.
func vetoRule(id string, trig Trigger, saveOn, match, field, msg string, rules ...Rule) Workflow {
	return Workflow{ID: id, Trigger: trig, IsActive: true,
		Condition: &Condition{Match: match, SaveOn: saveOn, Rules: rules},
		Actions:   []Action{{Type: ActionVeto, Field: field, Message: msg}}}
}

// T025 (specs/048 S1b): which rules veto which write. The edge operators compare against the
// stored record, so they can only match an update; saveOn narrows a before_save rule to one half;
// every matching veto is returned in rule order, not just the first.
func TestCollectVetoes_AndOrSaveOnEdgeOps(t *testing.T) {
	wonNoDate := vetoRule("r-won", TriggerBeforeSave, "", "", "close_date", "Chốt Thắng phải có Ngày chốt",
		Rule{Field: "stage", Op: OpChangedTo, Value: "Closed Won"}, Rule{Field: "close_date", Op: OpIsNull})
	lowProb := vetoRule("r-prob", TriggerBeforeSave, "", "", "probability", "Xác suất phải từ 10% khi Đàm phán",
		Rule{Field: "stage", Op: OpEq, Value: "Negotiation"}, Rule{Field: "probability", Op: OpLt, Value: 10.0})
	anyOr := vetoRule("r-or", TriggerBeforeSave, "", "or", "", "Tên hoặc giá trị bị cấm",
		Rule{Field: "name", Op: OpEq, Value: "X"}, Rule{Field: "amount", Op: OpGt, Value: 1000.0})
	createOnly := vetoRule("r-create", TriggerBeforeSave, SaveOnCreate, "", "", "Không tạo mới ở Thua",
		Rule{Field: "stage", Op: OpEq, Value: "Closed Lost"})
	updateOnly := vetoRule("r-update", TriggerBeforeSave, SaveOnUpdate, "", "", "Không sửa sang Thua",
		Rule{Field: "stage", Op: OpEq, Value: "Closed Lost"})
	onDelete := vetoRule("r-del", TriggerBeforeDelete, "", "", "", "Không xoá cơ hội đã Thắng",
		Rule{Field: "stage", Op: OpEq, Value: "Closed Won"})
	changedOnDelete := vetoRule("r-del-edge", TriggerBeforeDelete, "", "", "", "never",
		Rule{Field: "stage", Op: OpChanged})
	afterRule := Workflow{ID: "r-after", Trigger: TriggerAfterUpdate, IsActive: true,
		Actions: []Action{{Type: ActionVeto, Message: "an after rule never vetoes"}}}

	open := map[string]any{"name": "Deal", "stage": "Prospecting"}
	tests := []struct {
		name          string
		rules         []Workflow
		op            SaveOp
		image, before map[string]any
		want          []string // rule ids, in order
	}{
		{"changedTo matches the update that enters the state", []Workflow{wonNoDate}, SaveOpUpdate,
			map[string]any{"name": "Deal", "stage": "Closed Won"}, open, []string{"r-won"}},
		{"changedTo ignores an update that leaves the stage alone", []Workflow{wonNoDate}, SaveOpUpdate,
			map[string]any{"name": "Renamed", "stage": "Closed Won"}, map[string]any{"name": "Deal", "stage": "Closed Won"}, nil},
		{"AND needs every rule — close_date present passes", []Workflow{wonNoDate}, SaveOpUpdate,
			map[string]any{"stage": "Closed Won", "close_date": "2026-10-01"}, open, nil},
		{"changedTo never matches a create", []Workflow{wonNoDate}, SaveOpCreate,
			map[string]any{"stage": "Closed Won"}, nil, nil},
		{"OR fires on either rule", []Workflow{anyOr}, SaveOpUpdate,
			map[string]any{"name": "Deal", "amount": 5000.0}, open, []string{"r-or"}},
		{"OR with no match passes", []Workflow{anyOr}, SaveOpUpdate,
			map[string]any{"name": "Deal", "amount": 5.0}, open, nil},
		{"saveOn=create skips an update", []Workflow{createOnly, updateOnly}, SaveOpUpdate,
			map[string]any{"stage": "Closed Lost"}, open, []string{"r-update"}},
		{"saveOn=update skips a create", []Workflow{createOnly, updateOnly}, SaveOpCreate,
			map[string]any{"stage": "Closed Lost"}, nil, []string{"r-create"}},
		{"before_save never runs on delete, before_delete never on save", []Workflow{wonNoDate, onDelete}, SaveOpDelete,
			map[string]any{"stage": "Closed Won"}, nil, []string{"r-del"}},
		{"before_delete does not veto an update", []Workflow{onDelete}, SaveOpUpdate,
			map[string]any{"stage": "Closed Won"}, open, nil},
		{"changed never matches a delete", []Workflow{changedOnDelete}, SaveOpDelete,
			map[string]any{"stage": "Closed Won"}, nil, nil},
		{"an after_* rule is inert", []Workflow{afterRule}, SaveOpUpdate,
			map[string]any{"stage": "Closed Won"}, open, nil},
		{"every matching veto, in rule order", []Workflow{lowProb, wonNoDate, anyOr}, SaveOpUpdate,
			map[string]any{"name": "X", "stage": "Closed Won", "probability": 5.0},
			map[string]any{"name": "X", "stage": "Negotiation", "probability": 5.0},
			[]string{"r-won", "r-or"}},
		{"both vetoes when both hold", []Workflow{lowProb, wonNoDate}, SaveOpUpdate,
			map[string]any{"stage": "Closed Won", "probability": 5.0},
			map[string]any{"stage": "Negotiation", "probability": 5.0},
			[]string{"r-won"}},
		{"two vetoes on one save", []Workflow{wonNoDate, anyOr}, SaveOpUpdate,
			map[string]any{"name": "X", "stage": "Closed Won"}, open, []string{"r-won", "r-or"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollectVetoes(tt.rules, tt.op, tt.image, tt.before)
			var ids []string
			for _, v := range got {
				ids = append(ids, v.RuleID)
			}
			if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("vetoes = %v, want %v", ids, tt.want)
			}
		})
	}

	t.Run("violation carries the rule's message and anchor", func(t *testing.T) {
		got := CollectVetoes([]Workflow{wonNoDate}, SaveOpUpdate, map[string]any{"stage": "Closed Won"}, open)
		if len(got) != 1 || got[0] != (Violation{RuleID: "r-won", Field: "close_date", Message: "Chốt Thắng phải có Ngày chốt"}) {
			t.Fatalf("violation = %+v", got)
		}
	})
}

// T026: the rule slice comes straight out of the cache and is shared by every concurrent save of
// the entity — collecting vetoes must only read it (run with -race).
func TestCollectVetoes_DoesNotMutateCachedRules(t *testing.T) {
	rules := []Workflow{
		vetoRule("r1", TriggerBeforeSave, SaveOnUpdate, "", " close_date ", "m1",
			Rule{Field: "stage", Op: OpChangedTo, Value: "Closed Won"}),
		vetoRule("r2", TriggerBeforeSave, "", "or", "", "m2", Rule{Field: "name", Op: OpNotNull}),
	}
	snapshot := fmt.Sprintf("%#v %#v %#v", rules, *rules[0].Condition, *rules[1].Condition)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			image := map[string]any{"name": fmt.Sprint(i), "stage": "Closed Won"}
			if got := CollectVetoes(rules, SaveOpUpdate, image, map[string]any{"stage": "Open"}); len(got) != 2 {
				t.Errorf("goroutine %d: %d vetoes, want 2", i, len(got))
			}
		}(i)
	}
	wg.Wait()
	if after := fmt.Sprintf("%#v %#v %#v", rules, *rules[0].Condition, *rules[1].Condition); after != snapshot {
		t.Fatalf("cached rules mutated:\nbefore %s\nafter  %s", snapshot, after)
	}
}

// setRule builds an active before_save rule whose actions all change values.
func setRule(id, saveOn string, rules []Rule, actions ...Action) Workflow {
	return Workflow{ID: id, Trigger: TriggerBeforeSave, IsActive: true,
		Condition: &Condition{SaveOn: saveOn, Rules: rules}, Actions: actions}
}

func setTo(field string, value any) Action {
	return Action{Type: ActionFieldUpdate, Field: field, Value: value}
}

// outcomeIDs renders outcomes as rule:field[:error] for a compact comparison.
func outcomeIDs(outs []MutationOutcome) string {
	parts := make([]string, len(outs))
	for i, o := range outs {
		parts[i] = o.RuleID + ":" + o.Field
		if o.Error != "" {
			parts[i] += ":" + o.Error
		}
	}
	return strings.Join(parts, ",")
}

// T073 (specs/048 S4): value-changing rules run once, in the order the loader serves them
// (created_at, id), each seeing what the ones before it wrote; onlyIfEmpty reads "empty" the way the
// record validator does; saveOn narrows the rule; changedTo compares the progressing image to the
// stored record.
func TestApplyMutations_ProgressiveOrderOnlyIfEmpty(t *testing.T) {
	webIfEmpty := Action{Type: ActionFieldUpdate, Field: "source", Value: "Web", OnlyIfEmpty: true}
	tests := []struct {
		name          string
		rules         []Workflow
		op            SaveOp
		image, before map[string]any
		wantPatch     map[string]any
		wantOutcomes  string
	}{
		{"a later rule wins on the same field", []Workflow{
			setRule("r1", "", nil, setTo("probability", 50.0)),
			setRule("r2", "", nil, setTo("probability", 100.0)),
		}, SaveOpCreate, map[string]any{"probability": 10.0}, nil,
			map[string]any{"probability": 100.0}, "r1:probability,r2:probability"},
		{"a later rule sees what an earlier one wrote", []Workflow{
			setRule("r1", "", []Rule{{Field: "source", Op: OpIsNull}}, setTo("source", "Web")),
			setRule("r2", "", []Rule{{Field: "source", Op: OpEq, Value: "Web"}}, setTo("company", "Khách web")),
		}, SaveOpCreate, map[string]any{"full_name": "An"}, nil,
			map[string]any{"source": "Web", "company": "Khách web"}, "r1:source,r2:company"},
		{"onlyIfEmpty fills an absent field", []Workflow{setRule("r", "", nil, webIfEmpty)}, SaveOpCreate,
			map[string]any{"full_name": "An"}, nil, map[string]any{"source": "Web"}, "r:source"},
		{"onlyIfEmpty fills a nil field", []Workflow{setRule("r", "", nil, webIfEmpty)}, SaveOpCreate,
			map[string]any{"source": nil}, nil, map[string]any{"source": "Web"}, "r:source"},
		{"onlyIfEmpty fills a whitespace-only field", []Workflow{setRule("r", "", nil, webIfEmpty)}, SaveOpCreate,
			map[string]any{"source": "   "}, nil, map[string]any{"source": "Web"}, "r:source"},
		{"onlyIfEmpty fills an empty list", []Workflow{setRule("r", "", nil, webIfEmpty)}, SaveOpCreate,
			map[string]any{"source": []any{}}, nil, map[string]any{"source": "Web"}, "r:source"},
		{"onlyIfEmpty fills an empty object", []Workflow{setRule("r", "", nil, webIfEmpty)}, SaveOpCreate,
			map[string]any{"source": map[string]any{}}, nil, map[string]any{"source": "Web"}, "r:source"},
		{"onlyIfEmpty keeps a value that is there", []Workflow{setRule("r", "", nil, webIfEmpty)}, SaveOpCreate,
			map[string]any{"source": "Event"}, nil, map[string]any{}, ""},
		{"saveOn=update does not run on a create", []Workflow{setRule("r", SaveOnUpdate, nil, setTo("probability", 1.0))},
			SaveOpCreate, map[string]any{}, nil, map[string]any{}, ""},
		{"saveOn=create does not run on an update", []Workflow{setRule("r", SaveOnCreate, nil, setTo("probability", 1.0))},
			SaveOpUpdate, map[string]any{}, map[string]any{}, map[string]any{}, ""},
		{"never on a delete", []Workflow{setRule("r", "", nil, setTo("probability", 1.0))},
			SaveOpDelete, map[string]any{}, nil, map[string]any{}, ""},
		{"changedTo reads the progressing image against the stored record", []Workflow{
			setRule("r1", "", []Rule{{Field: "amount", Op: OpGt, Value: 1000.0}}, setTo("stage", "Closed Won")),
			setRule("r2", "", []Rule{{Field: "stage", Op: OpChangedTo, Value: "Closed Won"}}, setTo("probability", 100.0)),
		}, SaveOpUpdate, map[string]any{"stage": "Proposal", "amount": 5000.0, "probability": 40.0},
			map[string]any{"stage": "Proposal", "amount": 100.0, "probability": 40.0},
			map[string]any{"stage": "Closed Won", "probability": 100.0}, "r1:stage,r2:probability"},
		{"veto rules and after_* rules change nothing", []Workflow{
			vetoRule("v", TriggerBeforeSave, "", "", "close_date", "m"),
			{ID: "after", Trigger: TriggerAfterUpdate, IsActive: true, Actions: []Action{setTo("probability", 1.0)}},
		}, SaveOpUpdate, map[string]any{"probability": 40.0}, map[string]any{}, map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, outs := ApplyMutations(tt.rules, tt.op, tt.image, tt.before, nil, nil)
			if fmt.Sprint(patch) != fmt.Sprint(tt.wantPatch) {
				t.Fatalf("patch = %v, want %v", patch, tt.wantPatch)
			}
			if got := outcomeIDs(outs); got != tt.wantOutcomes {
				t.Fatalf("outcomes = %s, want %s", got, tt.wantOutcomes)
			}
		})
	}
}

// T074: the three value sources, a patch that only carries real changes, and one pass only.
func TestApplyMutations_ValueSources(t *testing.T) {
	amount := map[string]any{"amount": 1500.0, "currencyCode": "VND"}
	image := map[string]any{"full_name": "Nguyễn An", "company": "Nexus", "amount": amount, "budget": nil}
	sources := map[string]bool{"full_name": true, "company": true, "amount": true, "budget": true, "note": true, "missing": true}
	tests := []struct {
		name   string
		action Action
		want   any
	}{
		{"constant (valueSource absent)", setTo("note", "Khách web"), "Khách web"},
		{"constant (explicit) keeps a leading $ literally",
			Action{Type: ActionFieldUpdate, Field: "note", Value: "$record.company", ValueSource: ValueSourceConstant}, "$record.company"},
		{"field copies the typed value",
			Action{Type: ActionFieldUpdate, Field: "budget", Value: "amount", ValueSource: ValueSourceField}, amount},
		{"field copying a live field that is empty clears the target (the rule mirrors it)",
			Action{Type: ActionFieldUpdate, Field: "company", Value: "missing", ValueSource: ValueSourceField}, nil},
		{"template renders {{field}} from the record",
			Action{Type: ActionFieldUpdate, Field: "note", Value: "{{full_name}} — {{company}}", ValueSource: ValueSourceTemplate}, "Nguyễn An — Nexus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, _ := ApplyMutations([]Workflow{setRule("r", "", nil, tt.action)}, SaveOpCreate, image, nil, sources, nil)
			got, ok := patch[tt.action.Field]
			if !ok || fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Fatalf("patch = %v, want %s = %v", patch, tt.action.Field, tt.want)
			}
		})
	}
}

// A source that is no longer a live field of the entity — deleted since the rule was saved, its old
// value still in the stored record; or never one, like {{account.name}} — fails the action: the target
// keeps its value, the outcome carries the code, the next action still runs.
func TestApplyMutations_SourceMustBeLive(t *testing.T) {
	image := map[string]any{"full_name": "An", "company": "Nexus", "cmnd_cu": "012345678"}
	sources := map[string]bool{"full_name": true, "company": true, "note": true}
	for _, a := range []Action{
		{Type: ActionFieldUpdate, Field: "company", Value: "cmnd_cu", ValueSource: ValueSourceField},
		{Type: ActionFieldUpdate, Field: "company", Value: "Mã {{cmnd_cu}}", ValueSource: ValueSourceTemplate},
		{Type: ActionFieldUpdate, Field: "company", Value: "{{full_name}} ({{account.name}})", ValueSource: ValueSourceTemplate},
	} {
		t.Run(fmt.Sprint(a.Value), func(t *testing.T) {
			rules := []Workflow{setRule("r", "", nil, a, setTo("note", "x"))}
			patch, outs := ApplyMutations(rules, SaveOpUpdate, image, image, sources, nil)
			if fmt.Sprint(patch) != fmt.Sprint(map[string]any{"note": "x"}) {
				t.Fatalf("patch = %v, want only note (company untouched)", patch)
			}
			if got := outcomeIDs(outs); got != "r:company:WORKFLOW_BEFORE_VALUE_INVALID,r:note" {
				t.Fatalf("outcomes = %s", got)
			}
		})
	}
}

// A template that renders blank does nothing — it never clears the target; one that renders anything
// else is written as rendered.
func TestApplyMutations_BlankTemplateDoesNothing(t *testing.T) {
	sources := map[string]bool{"full_name": true, "source": true, "company": true}
	tmpl := func(v string) Action {
		return Action{Type: ActionFieldUpdate, Field: "company", Value: v, ValueSource: ValueSourceTemplate}
	}
	tests := []struct {
		name, tmpl string
		want       map[string]any
	}{
		{"placeholder empty on the record", "{{source}}", map[string]any{}},
		{"only whitespace around empty placeholders", " {{source}}  {{source}} ", map[string]any{}},
		{"part of it renders", "{{full_name}} {{source}}", map[string]any{"company": "An "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, outs := ApplyMutations([]Workflow{setRule("r", "", nil, tmpl(tt.tmpl))}, SaveOpUpdate,
				map[string]any{"full_name": "An", "company": "Ghi chú viết tay"}, map[string]any{}, sources, nil)
			if fmt.Sprint(patch) != fmt.Sprint(tt.want) {
				t.Fatalf("patch = %v, want %v", patch, tt.want)
			}
			if len(tt.want) == 0 && len(outs) != 0 {
				t.Fatalf("outcomes = %s, want none (nothing changed)", outcomeIDs(outs))
			}
		})
	}
}

func TestApplyMutations_PatchOnlyChangedKeys(t *testing.T) {
	rules := []Workflow{
		setRule("same", "", nil, setTo("probability", "40")), // "40" and 40 are the same value (looseEqual)
		setRule("there-and-back", "", nil, setTo("stage", "Closed Won"), setTo("stage", "Proposal")),
		setRule("object", "", nil, setTo("amount", map[string]any{"amount": 1.0, "currencyCode": "VND"})),
	}
	image := map[string]any{"probability": 40.0, "stage": "Proposal", "amount": map[string]any{"amount": 2.0, "currencyCode": "VND"}}
	patch, outs := ApplyMutations(rules, SaveOpCreate, image, nil, nil, nil)
	if fmt.Sprint(patch) != fmt.Sprint(map[string]any{"amount": map[string]any{"amount": 1.0, "currencyCode": "VND"}}) {
		t.Fatalf("patch = %v, want only amount (a different object; the rest ends where it started)", patch)
	}
	if got := outcomeIDs(outs); got != "there-and-back:stage,there-and-back:stage,object:amount" {
		t.Fatalf("outcomes = %s", got)
	}
}

func TestApplyMutations_SinglePass(t *testing.T) {
	// r1 would fire once r2 has run — but a patch never re-triggers a rule, so r1 stays unrun.
	rules := []Workflow{
		setRule("r1", "", []Rule{{Field: "probability", Op: OpEq, Value: 100.0}}, setTo("stage", "Closed Won")),
		setRule("r2", "", []Rule{{Field: "stage", Op: OpEq, Value: "Negotiation"}}, setTo("probability", 100.0)),
	}
	patch, _ := ApplyMutations(rules, SaveOpCreate, map[string]any{"stage": "Negotiation", "probability": 40.0}, nil, nil, nil)
	if fmt.Sprint(patch) != fmt.Sprint(map[string]any{"probability": 100.0}) {
		t.Fatalf("patch = %v, want probability only (no second pass)", patch)
	}
}

// A value the record validator refuses drops that action alone: the image keeps the value it had,
// the outcome carries the code, and the next action still runs.
func TestApplyMutations_CheckRefusesOneAction(t *testing.T) {
	check := func(field string, v any, _ map[string]any) (any, string) {
		if field == "source" && v == "Web" {
			return nil, "RECORD_INVALID_PICKLIST"
		}
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s), "" // the stored form, not the raw one
		}
		return v, ""
	}
	rules := []Workflow{
		setRule("r1", "", nil, setTo("source", "Web"), setTo("company", " Nexus ")),
		setRule("r2", "", []Rule{{Field: "source", Op: OpIsNull}}, setTo("status", "New")),
	}
	patch, outs := ApplyMutations(rules, SaveOpCreate, map[string]any{}, nil, nil, check)
	if fmt.Sprint(patch) != fmt.Sprint(map[string]any{"company": "Nexus", "status": "New"}) {
		t.Fatalf("patch = %v", patch)
	}
	if got := outcomeIDs(outs); got != "r1:source:RECORD_INVALID_PICKLIST,r1:company,r2:status" {
		t.Fatalf("outcomes = %s", got)
	}
}

// The check judges each value against the record as the rules before it left it — a dependent
// picklist's child against the parent an earlier rule just set — never against the image as sent.
func TestApplyMutations_CheckReadsTheProgressingImage(t *testing.T) {
	var seen []string
	check := func(field string, v any, image map[string]any) (any, string) {
		seen = append(seen, fmt.Sprintf("%s=%v on province=%v", field, v, image["province"]))
		return v, ""
	}
	rules := []Workflow{
		setRule("r1", "", nil, setTo("province", "Hà Nội")),
		setRule("r2", "", nil, setTo("district", "Ba Đình")),
	}
	ApplyMutations(rules, SaveOpCreate, map[string]any{"province": "Đà Nẵng"}, nil, nil, check)
	if got := strings.Join(seen, "; "); got != "province=Hà Nội on province=Đà Nẵng; district=Ba Đình on province=Hà Nội" {
		t.Fatalf("check saw %s", got)
	}
}

// T075: the rule slice (and an object constant inside it) comes straight from the cache and is shared
// by every concurrent save — applying it, and then writing into what it returned, must not touch it.
func TestApplyMutations_DoesNotMutateCachedRules(t *testing.T) {
	rules := []Workflow{
		setRule("r1", "", []Rule{{Field: "stage", Op: OpChangedTo, Value: "Closed Won"}},
			setTo("amount", map[string]any{"amount": 1.0, "currencyCode": "VND"}), setTo("tags", []any{"won"})),
		setRule("r2", "", nil, Action{Type: ActionFieldUpdate, Field: "note", Value: "{{name}}", ValueSource: ValueSourceTemplate}),
	}
	snapshot := fmt.Sprintf("%#v %#v %#v", rules, *rules[0].Condition, rules[0].Actions[0].Value)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			image := map[string]any{"name": fmt.Sprint(i), "stage": "Closed Won"}
			patch, _ := ApplyMutations(rules, SaveOpUpdate, image, map[string]any{"stage": "Open"}, map[string]bool{"name": true}, nil)
			if len(patch) != 3 {
				t.Errorf("goroutine %d: patch = %v, want 3 keys", i, patch)
				return
			}
			patch["amount"].(map[string]any)["amount"] = float64(i) // downstream writes into the patch
			patch["tags"].([]any)[0] = "mutated"
			if image["amount"] != nil {
				t.Errorf("goroutine %d: the caller's image was written", i)
			}
		}(i)
	}
	wg.Wait()
	if after := fmt.Sprintf("%#v %#v %#v", rules, *rules[0].Condition, rules[0].Actions[0].Value); after != snapshot {
		t.Fatalf("cached rules mutated:\nbefore %s\nafter  %s", snapshot, after)
	}
}

// T091 (specs/048 S5): the four text rewrites of a normalize action, applied in the order the rule
// lists them. Letters are cased rune by rune (Vietnamese with its diacritics included); whitespace is
// unicode.IsSpace — a no-break space or a tab is whitespace too — so what the user pasted from a
// spreadsheet comes out as one ordinary space.
func TestNormalizeOps_Table(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ops  []string
		want string
	}{
		{"trim", "  An  ", []string{NormalizeTrim}, "An"},
		{"trim unicode whitespace", " \tAn　\n", []string{NormalizeTrim}, "An"},
		{"lower an email", "An.Nguyen@EXAMPLE.COM", []string{NormalizeLower}, "an.nguyen@example.com"},
		{"lower Vietnamese", "ĐỖ XUÂN TÀI Ở ĐÀ NẴNG", []string{NormalizeLower}, "đỗ xuân tài ở đà nẵng"},
		{"upper Vietnamese", "nguyễn văn ấn, ưng ý", []string{NormalizeUpper}, "NGUYỄN VĂN ẤN, ƯNG Ý"},
		{"collapse keeps one space at each edge", "  Nguyễn   Văn  An ", []string{NormalizeCollapseSpaces}, " Nguyễn Văn An "},
		{"collapse tabs, newlines and no-break spaces", "a\t\n b  c", []string{NormalizeCollapseSpaces}, "a b c"},
		{"trim then collapse", "  Nguyễn   Văn  An ", []string{NormalizeTrim, NormalizeCollapseSpaces}, "Nguyễn Văn An"},
		{"collapse then trim", "  Nguyễn   Văn  An ", []string{NormalizeCollapseSpaces, NormalizeTrim}, "Nguyễn Văn An"},
		{"the email of US7", "  An.Nguyen@EXAMPLE.COM ", []string{NormalizeTrim, NormalizeLower}, "an.nguyen@example.com"},
		{"order: upper then lower ends lower", "Ánh", []string{NormalizeUpper, NormalizeLower}, "ánh"},
		{"order: lower then upper ends upper", "Ánh", []string{NormalizeLower, NormalizeUpper}, "ÁNH"},
		{"whitespace only trims to empty", "   ", []string{NormalizeTrim}, ""},
		{"no op leaves the text", "  X  ", nil, "  X  "},
		{"an unknown op is inert", "  X  ", []string{"titlecase"}, "  X  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeText(tt.in, tt.ops); got != tt.want {
				t.Fatalf("normalizeText(%q, %v) = %q, want %q", tt.in, tt.ops, got, tt.want)
			}
		})
	}
}

func normalizeOf(field string, ops ...string) Action {
	return Action{Type: ActionNormalize, Field: field, Ops: ops}
}

// T097: normalize is a value-changing action of Phase 1 — it rewrites the field's text as the rules
// before it left it, leaves anything that is not text alone, and a change the image already holds
// is no change (no patch key, no outcome). Text is compared as text: a phone number trimmed of its
// spaces IS a change even though both sides read as the same number.
func TestApplyMutations_Normalize(t *testing.T) {
	tests := []struct {
		name         string
		rules        []Workflow
		op           SaveOp
		image        map[string]any
		wantPatch    map[string]any
		wantOutcomes string
	}{
		{"the lead of US7", []Workflow{
			setRule("email", "", nil, normalizeOf("email", NormalizeTrim, NormalizeLower)),
			setRule("name", "", nil, normalizeOf("full_name", NormalizeTrim, NormalizeCollapseSpaces)),
		}, SaveOpCreate, map[string]any{"email": "  An.Nguyen@EXAMPLE.COM ", "full_name": "  Nguyễn   Văn  An "},
			map[string]any{"email": "an.nguyen@example.com", "full_name": "Nguyễn Văn An"}, "email:email,name:full_name"},
		{"numeric-looking text is compared as text", []Workflow{setRule("r", "", nil, normalizeOf("phone", NormalizeTrim))},
			SaveOpCreate, map[string]any{"phone": " 0901234567 "}, map[string]any{"phone": "0901234567"}, "r:phone"},
		{"already normalized: nothing to do", []Workflow{setRule("r", "", nil, normalizeOf("email", NormalizeTrim, NormalizeLower))},
			SaveOpUpdate, map[string]any{"email": "an@example.com"}, map[string]any{}, ""},
		{"absent field: nothing to do", []Workflow{setRule("r", "", nil, normalizeOf("email", NormalizeLower))},
			SaveOpCreate, map[string]any{"full_name": "An"}, map[string]any{}, ""},
		{"a value that is not text is left alone", []Workflow{
			setRule("r", "", nil, normalizeOf("probability", NormalizeTrim), normalizeOf("tags", NormalizeLower), normalizeOf("amount", NormalizeUpper)),
		}, SaveOpCreate, map[string]any{"probability": 40.0, "tags": []any{"A"}, "amount": map[string]any{"currencyCode": "vnd"}},
			map[string]any{}, ""},
		{"normalizes what an earlier rule wrote", []Workflow{
			setRule("fill", "", nil, setTo("company", "  ACME   Corp ")),
			setRule("norm", "", nil, normalizeOf("company", NormalizeTrim, NormalizeCollapseSpaces, NormalizeUpper)),
		}, SaveOpCreate, map[string]any{}, map[string]any{"company": "ACME CORP"}, "fill:company,norm:company"},
		{"a later rule sees the normalized text", []Workflow{
			setRule("norm", "", nil, normalizeOf("email", NormalizeLower)),
			setRule("tag", "", []Rule{{Field: "email", Op: OpEq, Value: "vip@example.com"}}, setTo("segment", "VIP")),
		}, SaveOpCreate, map[string]any{"email": "VIP@example.com"}, map[string]any{"email": "vip@example.com", "segment": "VIP"}, "norm:email,tag:segment"},
		{"saveOn narrows it", []Workflow{setRule("r", SaveOnUpdate, nil, normalizeOf("email", NormalizeLower))},
			SaveOpCreate, map[string]any{"email": "A@B.VN"}, map[string]any{}, ""},
		{"never on a delete", []Workflow{setRule("r", "", nil, normalizeOf("email", NormalizeLower))},
			SaveOpDelete, map[string]any{"email": "A@B.VN"}, map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, outs := ApplyMutations(tt.rules, tt.op, tt.image, map[string]any{}, nil, nil)
			if fmt.Sprint(patch) != fmt.Sprint(tt.wantPatch) {
				t.Fatalf("patch = %v, want %v", patch, tt.wantPatch)
			}
			if got := outcomeIDs(outs); got != tt.wantOutcomes {
				t.Fatalf("outcomes = %s, want %s", got, tt.wantOutcomes)
			}
		})
	}
}

// A normalized value goes through the same check as any value a rule writes: refused (a pattern the
// lower-cased code no longer matches, a required field trimmed to nothing), the action is dropped
// alone and the field keeps what it had.
func TestApplyMutations_NormalizeIsChecked(t *testing.T) {
	check := func(field string, v any, _ map[string]any) (any, string) {
		if s, _ := v.(string); s == "" && field == "full_name" {
			return nil, "RECORD_REQUIRED_MISSING"
		}
		return v, ""
	}
	rules := []Workflow{
		setRule("name", "", nil, normalizeOf("full_name", NormalizeTrim)),
		setRule("email", "", nil, normalizeOf("email", NormalizeLower)),
	}
	patch, outs := ApplyMutations(rules, SaveOpUpdate, map[string]any{"full_name": "   ", "email": "A@B.VN"}, map[string]any{}, nil, check)
	if fmt.Sprint(patch) != fmt.Sprint(map[string]any{"email": "a@b.vn"}) {
		t.Fatalf("patch = %v, want the email alone", patch)
	}
	if got := outcomeIDs(outs); got != "name:full_name:RECORD_REQUIRED_MISSING,email:email" {
		t.Fatalf("outcomes = %s", got)
	}
}
