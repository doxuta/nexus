package workflow

// Two capabilities a real vertical needs and could not express:
//   • a rule that fires when a record ENTERS a state, not while it sits in one;
//   • a created record that carries values — and a link — from the record that spawned it.

import (
	"testing"
)

func TestEdgeOperators(t *testing.T) {
	before := map[string]any{"status": "Đã gửi", "total": 100}
	after := map[string]any{"status": "Chấp nhận", "total": 100}

	cond := func(op, field string, val any) *Condition {
		return &Condition{Match: "and", Rules: []Rule{{Field: field, Op: op, Value: val}}}
	}

	t.Run("changedTo fires on the write that made it true", func(t *testing.T) {
		if !EvaluateWithBefore(cond(OpChangedTo, "status", "Chấp nhận"), after, before) {
			t.Error("did not fire on the transition")
		}
	})

	t.Run("changedTo does NOT re-fire on a later save of the same state", func(t *testing.T) {
		// This is the whole point. With eq, editing any other field on an already-accepted quote
		// re-runs the rule and sends a second contract.
		same := map[string]any{"status": "Chấp nhận", "total": 250}
		if EvaluateWithBefore(cond(OpChangedTo, "status", "Chấp nhận"), same, after) {
			t.Error("re-fired on an edit that did not touch status")
		}
		if !EvaluateWithBefore(cond(OpEq, "status", "Chấp nhận"), same, after) {
			t.Error("sanity: eq should still match — that is exactly the old behaviour")
		}
	})

	t.Run("changedTo pins WHERE it landed", func(t *testing.T) {
		if EvaluateWithBefore(cond(OpChangedTo, "status", "Chấp nhận"), map[string]any{"status": "Từ chối"}, before) {
			t.Error("fired on a transition to a different value")
		}
	})

	t.Run("changed fires on any move, regardless of destination", func(t *testing.T) {
		if !EvaluateWithBefore(cond(OpChanged, "status", nil), map[string]any{"status": "Từ chối"}, before) {
			t.Error("did not fire on a change")
		}
		if EvaluateWithBefore(cond(OpChanged, "total", nil), after, before) {
			t.Error("fired on a field this write did not touch")
		}
	})

	t.Run("no before-image → edge rules are FALSE, never accidentally true", func(t *testing.T) {
		// Creates and deletes carry no prior image. A record born Accepted did not BECOME
		// anything, and firing there would make "changedTo" mean two different things.
		for _, op := range []string{OpChanged, OpChangedTo} {
			if EvaluateWithBefore(cond(op, "status", "Chấp nhận"), after, nil) {
				t.Errorf("%s fired with no before-image", op)
			}
		}
	})

	t.Run("Evaluate (no before) still behaves exactly as before for every other operator", func(t *testing.T) {
		if !Evaluate(cond(OpEq, "status", "Chấp nhận"), after) {
			t.Error("eq regressed")
		}
	})

	t.Run("both operators are accepted by config validation", func(t *testing.T) {
		for _, op := range []string{OpChanged, OpChangedTo} {
			if !ValidOperator(op) {
				t.Errorf("%s rejected at config time — the rule could never be saved", op)
			}
		}
	})
}

func TestResolveCreateValues(t *testing.T) {
	evt := Event{
		RecordID: "quote-1",
		Data: map[string]any{
			"name":  "Báo giá ABC",
			"total": map[string]any{"amount": 5000000, "currencyCode": "VND"},
			"count": float64(3),
		},
	}

	out := resolveCreateValues(map[string]any{
		"owner":      OwnerSentinel,
		"from_quote": RecordSentinel,
		"value":      RecordFieldPrefix + "total",
		"qty":        RecordFieldPrefix + "count",
		"title":      "Hợp đồng cho {{name}}",
		"note":       "không có placeholder",
		"flag":       true,
		"missing":    RecordFieldPrefix + "khong_co",
	}, "user-9", evt)

	if out["owner"] != "user-9" {
		t.Errorf("owner = %v, want the resolved user", out["owner"])
	}
	if out["from_quote"] != "quote-1" {
		t.Errorf("from_quote = %v — without this the created record is an orphan", out["from_quote"])
	}
	// The typed copy must stay typed: routing it through the text renderer would flatten a currency
	// object to a string and the write would be rejected.
	m, ok := out["value"].(map[string]any)
	if !ok || m["currencyCode"] != "VND" {
		t.Errorf("value = %#v, want the currency object copied intact", out["value"])
	}
	if out["qty"] != float64(3) {
		t.Errorf("qty = %#v, want the number copied as a number", out["qty"])
	}
	if out["title"] != "Hợp đồng cho Báo giá ABC" {
		t.Errorf("title = %q — a builder typing {{name}} used to get the literal braces written", out["title"])
	}
	if out["note"] != "không có placeholder" {
		t.Errorf("note = %q, want plain text untouched", out["note"])
	}
	if out["flag"] != true {
		t.Errorf("flag = %#v, want non-strings passed through", out["flag"])
	}
	if _, present := out["missing"]; present {
		t.Errorf("an unresolvable $record.<field> must be OMITTED, not written as the literal sentinel")
	}
}

func TestResolveCreateValues_UnresolvableSentinelsAreOmitted(t *testing.T) {
	out := resolveCreateValues(map[string]any{"owner": OwnerSentinel, "src": RecordSentinel}, "", Event{})
	if len(out) != 0 {
		t.Errorf("out = %#v, want both omitted rather than writing '$owner'/'$record' as text", out)
	}
}
