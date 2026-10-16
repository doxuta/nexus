// DAILY-QUEUE #11 (`ticketing-sla-pack`) — the Ticketing pack's SLA deadline tier, asserted on the
// pack DEFINITION (no DB). The end-to-end proof that a freshly-installed tenant actually gets a
// stamped due_date lives in ticketing_sla_integration_test.go; this file pins the two invariants a
// later edit of builtin.go could break silently:
//
//  1. EVERY creation tier (after_create set_deadline) is guarded by `due_date isnull`. The #81
//     recompute tiers (after_update, 1.8.0) do not need the guard — tighten_only + the
//     due_date_auto stamp is theirs, pinned by the last test below. Without it the rule overwrites a
//     deadline the agent typed on the create form — due_date is on both the list and detail
//     layouts, so it IS user-editable, and set_deadline overwrites by design (specs/037 §4). A
//     machine quietly replacing a human's commitment is the bug class #63 exists for.
//  2. The tiers COVER every priority value, including an unset one. A ticket that falls through
//     every tier gets no deadline at all, which means no SLA reminder and no escalation — the
//     exact silence this slice removes, re-introduced for one bucket.
//
// Coverage is asserted by running the REAL evaluator (workflow.Evaluate) over the pack's own
// condition rules rather than by re-reading the literals: a test that only re-states the table
// would pass on a condition the engine reads differently.
package vertical_pack

import (
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/workflow"
)

// ticketingDeadlineRules returns the pack's set_deadline workflows on `trigger` ("" = any), in pack
// order.
func ticketingDeadlineRules(t *testing.T, trigger string) []PackWorkflow {
	t.Helper()
	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	var out []PackWorkflow
	for _, wf := range pack.Workflows {
		if trigger != "" && wf.Trigger != trigger {
			continue
		}
		for _, a := range wf.Actions {
			if a.Type == "set_deadline" {
				out = append(out, wf)
				break
			}
		}
	}
	return out
}

// ticketingSLARules returns the CREATION tiers — the after_create rules that make the promise. The
// after_update recompute tiers (#81) obey a different invariant, pinned by
// TestTicketingPack_RecomputeTiersOnlyTightenTheMachinesOwnDeadline.
func ticketingSLARules(t *testing.T) []PackWorkflow {
	t.Helper()
	return ticketingDeadlineRules(t, "after_create")
}

// toEngineCondition maps a pack condition onto the engine's own Condition so the test evaluates
// with workflow.Evaluate — the function that decides at run time — instead of a paraphrase.
func toEngineCondition(c *PackWorkflowCondition) *workflow.Condition {
	if c == nil {
		return nil
	}
	rules := make([]workflow.Rule, len(c.Rules))
	for i, r := range c.Rules {
		rules[i] = workflow.Rule{Field: r.Field, Op: r.Op, Value: r.Value}
	}
	return &workflow.Condition{Match: c.Match, Rules: rules}
}

// TestTicketingPack_SLARulesNeverOverwriteATypedDeadline pins invariant 1.
func TestTicketingPack_SLARulesNeverOverwriteATypedDeadline(t *testing.T) {
	rules := ticketingSLARules(t)
	if len(rules) == 0 {
		t.Fatal("the Ticketing pack ships no set_deadline rule — a fresh install has SLA timers on due_date but nothing ever fills due_date")
	}
	for _, wf := range rules {
		if wf.Trigger != "after_create" {
			t.Errorf("%q: trigger = %q, want after_create (the deadline is a commitment made when the ticket is raised)", wf.Name, wf.Trigger)
		}
		guarded := false
		for _, r := range wf.Condition.Rules {
			if r.Field == "due_date" && r.Op == "isnull" {
				guarded = true
			}
		}
		if !guarded {
			t.Errorf("%q has no `due_date isnull` guard — it would overwrite a deadline the agent typed on the create form", wf.Name)
		}
	}

	// The guard must actually bite: a ticket that already carries a due_date matches NO tier.
	typed := map[string]any{"priority": "Urgent", "due_date": "2026-09-16T09:00:00Z"}
	for _, wf := range rules {
		if workflow.Evaluate(toEngineCondition(wf.Condition), typed) {
			t.Errorf("%q fires on a ticket that already has a due_date — the agent's deadline would be overwritten", wf.Name)
		}
	}
}

// TestTicketingPack_SLATiersCoverEveryPriority pins invariant 2: for every priority the picklist
// offers AND for a ticket with no priority at all, EXACTLY ONE tier matches. Zero = a ticket with
// no SLA; two = two rules racing to stamp the same field.
func TestTicketingPack_SLATiersCoverEveryPriority(t *testing.T) {
	rules := ticketingSLARules(t)
	if len(rules) == 0 {
		t.Fatal("the Ticketing pack ships no set_deadline rule")
	}

	// Read the priority values off the pack itself so a new picklist value cannot be added
	// without this test noticing it has no tier.
	pack, _ := BuiltinPack("ticketing")
	var priorities []any
	for _, e := range pack.Entities {
		if e.Name != "tk_tickets" {
			continue
		}
		for _, f := range e.Fields {
			if f.Name == "priority" {
				priorities, _ = f.Options["values"].([]any)
			}
		}
	}
	if len(priorities) == 0 {
		t.Fatal("tk_tickets.priority has no picklist values — cannot check tier coverage")
	}

	cases := make([]map[string]any, 0, len(priorities)+1)
	for _, p := range priorities {
		cases = append(cases, map[string]any{"priority": p})
	}
	cases = append(cases, map[string]any{}) // a ticket raised with no priority set

	for _, data := range cases {
		matched := make([]string, 0, 1)
		for _, wf := range rules {
			if workflow.Evaluate(toEngineCondition(wf.Condition), data) {
				matched = append(matched, wf.Name)
			}
		}
		if len(matched) != 1 {
			t.Errorf("priority=%v matched %d tiers (%v), want exactly 1", data["priority"], len(matched), matched)
		}
	}
}

// slaFallThroughTier keys the catch-all tier: the rule that pins no priority with `eq` and
// matches with `neq` instead (Thấp, any value the picklist gains later, and no priority at all).
const slaFallThroughTier = "(bậc thường — mọi ưu tiên còn lại)"

// slaTierKey names the tier a set_deadline rule serves: the priority its condition pins with `eq`,
// or slaFallThroughTier.
func slaTierKey(wf PackWorkflow) string {
	if wf.Condition != nil {
		for _, r := range wf.Condition.Rules {
			if r.Field == "priority" && r.Op == "eq" {
				if v, ok := r.Value.(string); ok {
					return v
				}
			}
		}
	}
	return slaFallThroughTier
}

// slaTierAmounts maps each SLA tier to its business-hours amount. Two rules keying to the SAME tier
// fail here, by name (DAILY-QUEUE #84): in a plain map the second silently overwrote the first, so
// a second catch-all hid the real standard tier from every check below — measured by adding one,
// which left TestTicketingPack_SLAOffsetsAreOrderedBusinessHours GREEN while the 40-hour promise
// was no longer what it compared.
func slaTierAmounts(t *testing.T) map[string]int {
	t.Helper()
	amounts, owner := map[string]int{}, map[string]string{}
	for _, wf := range ticketingSLARules(t) {
		tier := slaTierKey(wf)
		if prev, dup := owner[tier]; dup {
			t.Fatalf("SLA rules %q and %q both serve tier %q — two rules would race to stamp the same ticket", prev, wf.Name, tier)
		}
		owner[tier] = wf.Name
		for _, a := range wf.Actions {
			if a.Type == "set_deadline" && a.Offset != nil {
				amounts[tier] = a.Offset.Amount
			}
		}
	}
	return amounts
}

// TestTicketingPack_SLAOffsetsAreOrderedBusinessHours pins the shape of the promise: every tier
// measures in business_hours (a wall-clock SLA is what DAILY-QUEUE #10 removed from this product),
// writes into due_date, and a more urgent ticket never gets a LATER deadline than a calmer one.
func TestTicketingPack_SLAOffsetsAreOrderedBusinessHours(t *testing.T) {
	for _, wf := range ticketingSLARules(t) {
		for _, a := range wf.Actions {
			if a.Type != "set_deadline" {
				continue
			}
			if a.Field != "due_date" {
				t.Errorf("%q: set_deadline writes %q, want due_date (the field both SLA timers anchor on)", wf.Name, a.Field)
			}
			if a.Offset == nil {
				t.Fatalf("%q: set_deadline has no offset — install would fail at Pass 8 with no rollback", wf.Name)
			}
			if a.Offset.Unit != workflow.TimerUnitBusinessHours {
				t.Errorf("%q: offset unit = %q, want business_hours", wf.Name, a.Offset.Unit)
			}
		}
	}
	byPriority := slaTierAmounts(t)
	order := []string{"Urgent", "High", "Medium", slaFallThroughTier}
	prev := 0
	for _, p := range order {
		amt, ok := byPriority[p]
		if !ok {
			t.Fatalf("no SLA tier for priority %q — tiers found: %v", p, byPriority)
		}
		if amt <= prev {
			t.Errorf("tier %q = %d business hours, which is not longer than the tier before it (%d)", p, amt, prev)
		}
		prev = amt
	}
}

// TestTicketingPack_RecomputeTiersOnlyTightenTheMachinesOwnDeadline pins the #81 tiers (1.8.0):
//
//  1. every creation tier stamps due_date_auto — without the stamp no recompute can ever prove a
//     deadline is the machine's, and the whole feature is silently inert;
//  2. every priority value has EXACTLY ONE after_update tier, gated by `priority changedTo X`, that
//     recomputes with tighten_only from created_at into due_date + the stamp;
//  3. that tier's amount equals the creation tier the same priority gets — a ticket raised Urgent
//     and one re-triaged to Urgent are promised the same thing;
//  4. the stamp field exists as a datetime on tk_tickets and is on no layout (bookkeeping only).
func TestTicketingPack_RecomputeTiersOnlyTightenTheMachinesOwnDeadline(t *testing.T) {
	for _, wf := range ticketingSLARules(t) {
		for _, a := range wf.Actions {
			if a.Type == "set_deadline" && a.StampField != "due_date_auto" {
				t.Errorf("creation tier %q stamps %q, want due_date_auto — tickets it creates could never be recomputed", wf.Name, a.StampField)
			}
		}
	}

	creation := slaTierAmounts(t)
	pack, _ := BuiltinPack("ticketing")
	var priorities []any
	stampOK := false
	for _, e := range pack.Entities {
		if e.Name != "tk_tickets" {
			continue
		}
		for _, f := range e.Fields {
			switch f.Name {
			case "priority":
				priorities, _ = f.Options["values"].([]any)
			case "due_date_auto":
				stampOK = f.FieldType == "datetime"
			}
		}
	}
	if !stampOK {
		t.Fatal("tk_tickets has no datetime due_date_auto — the stamp the recompute tiers depend on")
	}
	for _, l := range pack.Layouts {
		if l.EntityName == "tk_tickets" {
			if b, _ := json.Marshal(l.LayoutJSON); strings.Contains(string(b), "due_date_auto") {
				t.Errorf("%s layout shows due_date_auto — the machine's stamp is not for agents to edit", l.ViewType)
			}
		}
	}

	recompute := ticketingDeadlineRules(t, "after_update")
	for _, p := range priorities {
		var hits []PackWorkflow
		for _, wf := range recompute {
			before := map[string]any{"priority": "__other__"}
			if workflow.EvaluateWithBefore(toEngineCondition(wf.Condition), map[string]any{"priority": p}, before) {
				hits = append(hits, wf)
			}
		}
		if len(hits) != 1 {
			t.Errorf("priority changed to %v fires %d recompute tiers, want exactly 1", p, len(hits))
			continue
		}
		wf := hits[0]
		a := wf.Actions[0]
		if len(wf.Actions) != 1 || a.Type != "set_deadline" || a.Field != "due_date" || a.StampField != "due_date_auto" ||
			a.Mode != workflow.DeadlineModeTightenOnly || a.Anchor != workflow.DeadlineAnchorCreatedAt ||
			a.Offset == nil || a.Offset.Unit != workflow.TimerUnitBusinessHours {
			t.Errorf("%q: action %+v — want one set_deadline due_date, tighten_only, anchor created_at, stamp due_date_auto, business_hours", wf.Name, a)
			continue
		}
		tier := p.(string)
		if _, pinned := creation[tier]; !pinned {
			tier = slaFallThroughTier
		}
		if a.Offset.Amount != creation[tier] {
			t.Errorf("%q recomputes %d business hours, but a ticket RAISED at %v is promised %d", wf.Name, a.Offset.Amount, p, creation[tier])
		}
	}
	// The gate must be an edge: saving an already-Urgent ticket again must not re-fire a tier.
	for _, wf := range recompute {
		same := map[string]any{"priority": "Urgent"}
		if workflow.EvaluateWithBefore(toEngineCondition(wf.Condition), same, same) {
			t.Errorf("%q fires on a save that did not change priority", wf.Name)
		}
	}
}
