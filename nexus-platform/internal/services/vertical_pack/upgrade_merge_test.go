// L6 (specs/069 T098/T099): the plan offers "merge" on a both-sides-changed item only when the two
// edits touch different paths, and the merged document carries both.
package vertical_pack

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// wfCond builds a rule with a condition rule value and one action value: the builder edits the
// condition, the pack edits the action.
func wfCond(name, cond, act string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"name":%q,"entityName":"e","trigger":"after_create","condition":{"match":"and","rules":[{"field":"x","op":"eq","value":%q}]},"actions":[{"type":"field_update","field":"x","value":%q}]}`,
		name, cond, act))
}

func mergeFixture(name string, base, theirs, live json.RawMessage) *planFixture {
	f := newFixture()
	k := ItemWorkflow + "/" + name
	f.in.base[k] = StoredPackItem{Type: ItemWorkflow, Key: name, PackVersion: "1.0.0", Content: base, LiveID: "id-" + name}
	f.in.theirs[k] = theirs
	f.in.live[k] = live
	f.in.idx.add(ItemWorkflow, name, "id-"+name)
	f.in.ready = func(string) bool { return true }
	return f
}

func TestPlan_MergeOfferedOnDisjointEdits(t *testing.T) {
	f := mergeFixture("w", wfCond("w", "a", "old"), wfCond("w", "a", "new"), wfCond("w", "mine", "old"))
	it := find(t, f.plan(t), ItemWorkflow, "w")
	if it.Status != StatusConflict {
		t.Fatalf("status = %s", it.Status)
	}
	if !contains(it.Actions, ActionMerge) || it.DefaultAction != ActionKeep {
		t.Fatalf("actions=%v default=%s: want merge offered, default keep", it.Actions, it.DefaultAction)
	}
	s := string(it.Merged)
	if !strings.Contains(s, `"mine"`) || !strings.Contains(s, `"new"`) || strings.Contains(s, `"old"`) {
		t.Fatalf("merged lacks one side: %s", s)
	}
	if len(it.TheirsTaken) == 0 {
		t.Fatalf("theirsTakenPaths empty")
	}
}

func TestPlan_NoMergeWhenSamePathChanged(t *testing.T) {
	f := mergeFixture("w", wfCond("w", "a", "old"), wfCond("w", "a", "new"), wfCond("w", "a", "mine"))
	it := find(t, f.plan(t), ItemWorkflow, "w")
	if contains(it.Actions, ActionMerge) || len(it.Merged) != 0 {
		t.Fatalf("overlapping edit must not offer merge: actions=%v", it.Actions)
	}
}

// Picklist: builder adds VIP, pack adds "Đối tác" at the same array slot ⇒ still mergeable as a set.
func TestPlan_MergePicklistBothAdd(t *testing.T) {
	mk := func(vals ...string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"name": "tier", "label": "Tier", "fieldType": "picklist", "options": map[string]any{"values": vals}})
		return b
	}
	f := newFixture()
	key := ItemField + "/crm.tier"
	f.in.base[key] = StoredPackItem{Type: ItemField, Key: "crm.tier", Content: mk("A", "B"), LiveID: "f1"}
	f.in.theirs[key] = mk("A", "B", "Đối tác")
	f.in.live[key] = mk("A", "B", "VIP")
	f.in.ready = func(string) bool { return true }
	c := &planCtx{ready: f.in.ready, idx: f.in.idx}
	it := c.item(ItemField, "crm.tier", true, f.in.base[key].Content, f.in.theirs[key], true, f.in.live[key], liveHere, itemMeta{parentKey: "crm"})
	if !contains(it.Actions, ActionMerge) {
		t.Fatalf("picklist both-add should offer merge: status=%s actions=%v reason=%s", it.Status, it.Actions, it.Reason)
	}
	if s := string(it.Merged); !strings.Contains(s, "VIP") || !strings.Contains(s, "Đối tác") {
		t.Fatalf("merged = %s", s)
	}
}

// useMerged swaps the merged document in as the item's new-pack content (workflow) and into the
// table's field entry (field) so the baseline composed afterwards is the merge, not the pack.
func TestUseMerged(t *testing.T) {
	e := &applyEnv{theirs: map[string]json.RawMessage{
		ItemWorkflow + "/w": json.RawMessage(`{"name":"w"}`),
		ItemEntity + "/crm": json.RawMessage(`{"name":"crm","fields":[{"name":"tier","options":{"values":["A"]}},{"name":"other"}]}`),
	}}
	wf := &UpgradeItem{ItemType: ItemWorkflow, ItemKey: "w", Merged: json.RawMessage(`{"name":"w","m":1}`)}
	restoreWF, err := e.useMerged(wf)
	if err != nil || string(e.theirs[ItemWorkflow+"/w"]) != `{"name":"w","m":1}` {
		t.Fatalf("workflow: err=%v theirs=%s", err, e.theirs[ItemWorkflow+"/w"])
	}
	restoreWF()
	if string(e.theirs[ItemWorkflow+"/w"]) != `{"name":"w"}` {
		t.Fatalf("restore must put the pack content back, got %s", e.theirs[ItemWorkflow+"/w"])
	}
	fld := &UpgradeItem{ItemType: ItemField, ItemKey: "crm.tier", ParentKey: "crm", Merged: json.RawMessage(`{"name":"tier","options":{"values":["A","VIP"]}}`)}
	restoreF, err := e.useMerged(fld)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(e.theirs[ItemEntity+"/crm"]); !strings.Contains(got, `"VIP"`) || !strings.Contains(got, `"other"`) {
		t.Fatalf("table content = %s", got)
	}
	restoreF()
	if got := string(e.theirs[ItemEntity+"/crm"]); strings.Contains(got, `"VIP"`) {
		t.Fatalf("restore left the merge in the baseline source: %s", got)
	}
	if _, err := e.useMerged(&UpgradeItem{ItemType: ItemWorkflow, ItemKey: "x"}); err == nil {
		t.Fatal("no merged document must be refused")
	}
}
