// Unit tests for the read-only 3-way upgrade plan (../specs/069-pack-apply-upgrade tasks T005-T011,
// T017a): the truth table, sub-item expansion, live resolution, planHash, semver, unwired writers
// and secret hygiene. They drive computePlan directly with fabricated sides — no DB.
package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

// wfJSON builds a workflow whose single field_update action carries `v`, so v1/v2/v3 are three
// distinguishable contents differing exactly at actions[0].value.
func wfJSON(name, v string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(
		`{"name":%q,"entityName":"e","trigger":"after_create","actions":[{"type":"field_update","field":"x","value":%q}]}`, name, v))
}

// side is a compact description of one (base, theirs, live) combination; "" = absent.
type side struct{ b, t, l string }

type planFixture struct {
	in planInput
}

func newFixture() *planFixture {
	idx := newIDIndex()
	idx.ok[ItemWorkflow] = true // the workflow probe succeeded: absence from live is trustworthy
	return &planFixture{in: planInput{
		tenantID: "t1", slug: "p", toVersion: "1.1.0", source: "builtin",
		fromVersions: []string{"1.0.0"},
		base:         map[string]StoredPackItem{}, theirs: map[string]json.RawMessage{}, live: map[string]json.RawMessage{},
		idx: idx, shared: map[string][]string{},
	}}
}

func (f *planFixture) workflow(name string, s side) {
	k := ItemWorkflow + "/" + name
	if s.b != "" {
		f.in.base[k] = StoredPackItem{Type: ItemWorkflow, Key: name, PackVersion: "1.0.0", Content: wfJSON(name, s.b), LiveID: "id-" + name}
	}
	if s.t != "" {
		f.in.theirs[k] = wfJSON(name, s.t)
	}
	if s.l != "" {
		f.in.live[k] = wfJSON(name, s.l)
		f.in.idx.add(ItemWorkflow, name, "id-"+name)
	}
}

func (f *planFixture) plan(t *testing.T) *UpgradePlan {
	t.Helper()
	p, err := computePlan(f.in)
	if err != nil {
		t.Fatalf("computePlan: %v", err)
	}
	return p
}

func find(t *testing.T, p *UpgradePlan, typ, key string) UpgradeItem {
	t.Helper()
	for _, it := range p.Items {
		if it.ItemType == typ && it.ItemKey == key {
			return it
		}
	}
	t.Fatalf("plan has no %s/%s (items: %d)", typ, key, len(p.Items))
	return UpgradeItem{}
}

// T005: one case per row of the classification table (plan.md §3).
func TestComputePlan_TruthTable(t *testing.T) {
	tests := []struct {
		name   string
		s      side
		status string
		needs  bool // needs a decision ⇒ keep-only at L1
	}{
		{"added", side{"", "a", ""}, StatusAdded, true},
		{"in_sync no base", side{"", "a", "a"}, StatusInSync, false},
		{"conflict_no_base", side{"", "a", "b"}, StatusConflictNoBase, true},
		{"unchanged", side{"a", "a", "a"}, StatusUnchanged, false},
		{"update_safe", side{"a", "b", "a"}, StatusUpdateSafe, true},
		{"kept_custom", side{"a", "a", "b"}, StatusKeptCustom, true},
		{"in_sync both changed same", side{"a", "b", "b"}, StatusInSync, false},
		{"conflict", side{"a", "b", "c"}, StatusConflict, true},
		{"deleted_by_builder", side{"a", "a", ""}, StatusDeletedByBuilder, true},
		{"conflict_deleted", side{"a", "b", ""}, StatusConflictDeleted, true},
		{"removed_in_pack", side{"a", "", "a"}, StatusRemovedInPack, true},
		{"removed_in_pack_modified", side{"a", "", "b"}, StatusRemovedInPackModified, true},
		{"in_sync both dropped", side{"a", "", ""}, StatusInSync, false},
	}
	f := newFixture()
	for i, tt := range tests {
		f.workflow(fmt.Sprintf("w%02d", i), tt.s)
	}
	// 14th row: still exists but the export cannot read it back.
	f.in.base[ItemWorkflow+"/wU"] = StoredPackItem{Type: ItemWorkflow, Key: "wU", Content: wfJSON("wU", "a"), LiveID: "id-wU"}
	f.in.theirs[ItemWorkflow+"/wU"] = wfJSON("wU", "a")
	f.in.idx.add(ItemWorkflow, "wU", "id-wU")
	p := f.plan(t)
	for i, tt := range tests {
		it := find(t, p, ItemWorkflow, fmt.Sprintf("w%02d", i))
		if it.Status != tt.status {
			t.Errorf("%s: status = %s, want %s", tt.name, it.Status, tt.status)
		}
		if tt.needs {
			if len(it.Actions) != 1 || it.Actions[0] != ActionKeep || it.DefaultAction != ActionKeep || it.Reason != ReasonAdapterNotReady {
				t.Errorf("%s: want keep-only/ADAPTER_NOT_READY, got actions=%v default=%s reason=%s", tt.name, it.Actions, it.DefaultAction, it.Reason)
			}
		} else if len(it.Actions) != 0 {
			t.Errorf("%s: want no actions, got %v", tt.name, it.Actions)
		}
	}
	if it := find(t, p, ItemWorkflow, "wU"); it.Status != StatusUndiffable || it.Reason != ReasonUndiffable {
		t.Errorf("undiffable row: got %s/%s", it.Status, it.Reason)
	}
	if p.Summary["writes"] != 0 {
		t.Errorf("L1 must not pre-select any write, summary.writes = %d", p.Summary["writes"])
	}
}

// Override layers: ambiguous_name, unsupported, protected; and the FR-010a "expected-only" downgrade.
func TestComputePlan_OverridesAndExpectedOnly(t *testing.T) {
	f := newFixture()
	// ambiguous: two live records carry the name and the base has no live_id to disambiguate.
	f.workflow("amb", side{"", "a", "a"})
	f.in.idx.add(ItemWorkflow, "amb", "other-id")
	// protected: builder chose "Luôn giữ" earlier and the two sides still differ.
	f.workflow("prot", side{"a", "b", "a"})
	row := f.in.base[ItemWorkflow+"/prot"]
	row.Protected = true
	f.in.base[ItemWorkflow+"/prot"] = row
	// expected-only: base row has no live_id (the install SKIPPED an existing rule).
	f.workflow("exp", side{"a", "a", "b"})
	row = f.in.base[ItemWorkflow+"/exp"]
	row.LiveID = ""
	f.in.base[ItemWorkflow+"/exp"] = row
	// expected-only but live absent: the skip proves the rule once existed ⇒ deleted_by_builder.
	f.workflow("expgone", side{"a", "a", ""})
	row = f.in.base[ItemWorkflow+"/expgone"]
	row.LiveID = ""
	f.in.base[ItemWorkflow+"/expgone"] = row
	// a type whose create path returns no id never downgrades.
	f.in.base[ItemLayout+"/e:detail"] = StoredPackItem{Type: ItemLayout, Key: "e:detail", Content: json.RawMessage(`{"entityName":"e","viewType":"detail","layout":"a"}`)}
	f.in.theirs[ItemLayout+"/e:detail"] = json.RawMessage(`{"entityName":"e","viewType":"detail","layout":"a"}`)
	f.in.live[ItemLayout+"/e:detail"] = json.RawMessage(`{"entityName":"e","viewType":"detail","layout":"b"}`)

	p := f.plan(t)
	if it := find(t, p, ItemWorkflow, "amb"); it.Status != StatusAmbiguousName || it.Reason != ReasonAmbiguousName || it.DefaultAction != ActionKeep {
		t.Errorf("ambiguous: %+v", it)
	}
	if it := find(t, p, ItemWorkflow, "prot"); it.Status != StatusProtected {
		t.Errorf("protected: status = %s", it.Status)
	}
	if it := find(t, p, ItemWorkflow, "exp"); it.Status != StatusConflictNoBase || it.Reason != ReasonBaseExpectedOnly {
		t.Errorf("expected-only downgrade: %s/%s", it.Status, it.Reason)
	}
	if it := find(t, p, ItemWorkflow, "expgone"); it.Status != StatusDeletedByBuilder {
		t.Errorf("expected-only + live gone must stay deleted_by_builder, got %s", it.Status)
	}
	if it := find(t, p, ItemLayout, "e:detail"); it.Status != StatusKeptCustom {
		t.Errorf("layout (no live id by design) must not downgrade, got %s", it.Status)
	}
}

// The key set is base ∪ theirs: a rule only the builder created never enters the plan nor the hash.
func TestComputePlan_LiveOnlyItemsAreInvisible(t *testing.T) {
	f := newFixture()
	f.workflow("w", side{"a", "a", "a"})
	p1 := f.plan(t)
	f.in.live[ItemWorkflow+"/mine"] = wfJSON("mine", "z")
	f.in.idx.add(ItemWorkflow, "mine", "id-mine")
	p2 := f.plan(t)
	if len(p2.Items) != 1 {
		t.Fatalf("builder-only rule leaked into the plan: %d items", len(p2.Items))
	}
	if p1.PlanHash != p2.PlanHash {
		t.Fatalf("planHash changed by an item outside the pack")
	}
}

// adopt / summary.adoptable semantics (plan.md §4).
func TestComputePlan_Adopt(t *testing.T) {
	f := newFixture()
	f.workflow("nobase", side{"", "a", "a"})  // no base ⇒ adopt
	f.workflow("clean", side{"a", "a", "a"})  // has live_id ⇒ nothing to adopt
	f.workflow("synced", side{"a", "b", "b"}) // base content will change ⇒ adopt
	f.workflow("skipped", side{"a", "a", "a"})
	row := f.in.base[ItemWorkflow+"/skipped"]
	row.LiveID = "" // expected-only base, exactly one live id ⇒ adopt gives it the id
	f.in.base[ItemWorkflow+"/skipped"] = row
	p := f.plan(t)
	want := map[string]bool{"nobase": true, "clean": false, "synced": true, "skipped": true}
	for k, w := range want {
		if got := find(t, p, ItemWorkflow, k).Adopt; got != w {
			t.Errorf("%s: adopt = %v, want %v", k, got, w)
		}
	}
	if p.Summary["adoptable"] != 3 {
		t.Errorf("summary.adoptable = %d, want 3", p.Summary["adoptable"])
	}
}

// T006: tables/roles expand into fields/grants; whole item when only the pack has them.
func TestComputePlan_ExpandSubItems(t *testing.T) {
	ent := func(fields ...string) json.RawMessage {
		var fs []string
		for _, f := range fields {
			fs = append(fs, fmt.Sprintf(`{"name":%q,"label":%q,"fieldType":"text"}`, f, f))
		}
		return json.RawMessage(`{"name":"tk","label":"Phiếu","fields":[` + strings.Join(fs, ",") + `]}`)
	}
	role := func(levels ...string) json.RawMessage {
		var as []string
		for i, l := range levels {
			as = append(as, fmt.Sprintf(`{"entityName":"tk%d","action":"view","accessLevel":%q}`, i, l))
		}
		return json.RawMessage(`{"name":"agent","label":"Agent","actions":[` + strings.Join(as, ",") + `]}`)
	}
	f := newFixture()
	// Base lacks field "due" (an additive re-install added it without touching the base row);
	// theirs and live both carry it ⇒ the field is in_sync, NOT a fake "Đã tùy chỉnh" table.
	f.in.base[ItemEntity+"/tk"] = StoredPackItem{Type: ItemEntity, Key: "tk", Content: ent("name"), LiveID: "ent-1"}
	f.in.theirs[ItemEntity+"/tk"] = ent("name", "due")
	f.in.live[ItemEntity+"/tk"] = ent("name", "due")
	f.in.idx.add(ItemEntity, "tk", "ent-1")
	// Role: grant 0 raised by the builder, grant 1 untouched, grant 2 new in the pack.
	f.in.base[ItemRole+"/agent"] = StoredPackItem{Type: ItemRole, Key: "agent", Content: role("own", "own"), LiveID: "r-1"}
	f.in.theirs[ItemRole+"/agent"] = role("own", "own", "all")
	f.in.live[ItemRole+"/agent"] = role("all", "own")
	f.in.idx.add(ItemRole, "agent", "r-1")
	// Table only the pack ships ⇒ ONE whole entity item, no field items.
	f.in.theirs[ItemEntity+"/brandnew"] = json.RawMessage(`{"name":"brandnew","label":"Mới","fields":[{"name":"a","label":"A","fieldType":"text"}]}`)
	p := f.plan(t)

	if it := find(t, p, ItemEntity, "tk"); it.Status != StatusUnchanged {
		t.Errorf("entity attrs: %s", it.Status)
	}
	due := find(t, p, ItemField, "tk.due")
	if due.Status != StatusInSync || !due.Adopt || due.ParentKey != "tk" {
		t.Errorf("field tk.due: status=%s adopt=%v parent=%q", due.Status, due.Adopt, due.ParentKey)
	}
	if it := find(t, p, ItemField, "tk.name"); it.Status != StatusUnchanged {
		t.Errorf("field tk.name: %s", it.Status)
	}
	if it := find(t, p, ItemRoleGrant, "agent/tk0/view"); it.Status != StatusKeptCustom {
		t.Errorf("builder-raised grant: %s", it.Status)
	}
	if it := find(t, p, ItemRoleGrant, "agent/tk2/view"); it.Status != StatusAdded {
		t.Errorf("new pack grant: %s", it.Status)
	}
	if it := find(t, p, ItemEntity, "brandnew"); it.Status != StatusAdded {
		t.Errorf("new table: %s", it.Status)
	}
	for _, it := range p.Items {
		if it.ItemType == ItemField && strings.HasPrefix(it.ItemKey, "brandnew.") {
			t.Errorf("a table only the pack ships must not expand, got %s", it.ItemKey)
		}
	}
}

// unsupported: a field whose type changed and a wildcard grant on an existing role.
func TestComputePlan_Unsupported(t *testing.T) {
	ent := func(ft string) json.RawMessage {
		return json.RawMessage(`{"name":"tk","label":"T","fields":[{"name":"f","label":"F","fieldType":"` + ft + `"}]}`)
	}
	f := newFixture()
	f.in.base[ItemEntity+"/tk"] = StoredPackItem{Type: ItemEntity, Key: "tk", Content: ent("text"), LiveID: "e1"}
	f.in.theirs[ItemEntity+"/tk"] = ent("number")
	f.in.live[ItemEntity+"/tk"] = ent("text")
	f.in.idx.add(ItemEntity, "tk", "e1")
	f.in.base[ItemRole+"/r"] = StoredPackItem{Type: ItemRole, Key: "r", Content: json.RawMessage(`{"name":"r","actions":[]}`), LiveID: "r1"}
	f.in.theirs[ItemRole+"/r"] = json.RawMessage(`{"name":"r","actions":[{"entityName":"*","action":"view","accessLevel":"all"}]}`)
	f.in.live[ItemRole+"/r"] = json.RawMessage(`{"name":"r","actions":[]}`)
	f.in.idx.add(ItemRole, "r", "r1")
	p := f.plan(t)
	if it := find(t, p, ItemField, "tk.f"); it.Status != StatusUnsupported || it.Reason != ReasonFieldTypeChange {
		t.Errorf("field type change: %s/%s", it.Status, it.Reason)
	}
	if it := find(t, p, ItemRoleGrant, "r/*/view"); it.Status != StatusUnsupported || it.Reason != ReasonRoleWildcard {
		t.Errorf("wildcard grant: %s/%s", it.Status, it.Reason)
	}
}

// T007: live resolution — rename via live_id, ambiguity, undiffable vs absent.
func TestComputePlan_ResolveLive(t *testing.T) {
	f := newFixture()
	// renamed: base.live_id now carries another name; compare against THAT content.
	f.in.base[ItemWorkflow+"/old"] = StoredPackItem{Type: ItemWorkflow, Key: "old", Content: wfJSON("old", "a"), LiveID: "id-1"}
	f.in.theirs[ItemWorkflow+"/old"] = wfJSON("old", "b")
	f.in.live[ItemWorkflow+"/Renamed"] = wfJSON("Renamed", "a")
	f.in.idx.add(ItemWorkflow, "Renamed", "id-1")
	// the same name no longer resolves to anything but the builder made a different rule of that name
	// (must NOT be taken for the pack's rule when live_id resolved elsewhere).
	f.in.live[ItemWorkflow+"/old"] = wfJSON("old", "zzz")
	f.in.idx.add(ItemWorkflow, "old", "id-other")
	p := f.plan(t)
	it := find(t, p, ItemWorkflow, "old")
	// The comparison runs against the RENAMED rule (value still "a" = base): the only builder-side
	// edit is the name itself, the pack-side edit is the value ⇒ both sides changed, no overlap.
	if it.RenamedTo != "Renamed" || it.Status != StatusConflict {
		t.Errorf("rename: renamedTo=%q status=%s (want Renamed / conflict)", it.RenamedTo, it.Status)
	}
	if fmt.Sprint(it.OursPaths) != "[name]" || fmt.Sprint(it.TheirsPaths) != "[actions[0].value]" || len(it.Overlap) != 0 {
		t.Errorf("rename paths: ours=%v theirs=%v overlap=%v", it.OursPaths, it.TheirsPaths, it.Overlap)
	}
	if it.LiveID != "id-1" {
		t.Errorf("rename: liveId = %q", it.LiveID)
	}
}

func TestComputePlan_NoLiveIDDuplicatesAndAbsence(t *testing.T) {
	f := newFixture()
	f.in.base[ItemWorkflow+"/dup"] = StoredPackItem{Type: ItemWorkflow, Key: "dup", Content: wfJSON("dup", "a")}
	f.in.theirs[ItemWorkflow+"/dup"] = wfJSON("dup", "a")
	f.in.live[ItemWorkflow+"/dup"] = wfJSON("dup", "a")
	f.in.idx.add(ItemWorkflow, "dup", "x1")
	f.in.idx.add(ItemWorkflow, "dup", "x2")
	// absent from export, probe still lists it ⇒ undiffable (never a false "deleted").
	f.in.base[ItemWorkflow+"/hidden"] = StoredPackItem{Type: ItemWorkflow, Key: "hidden", Content: wfJSON("hidden", "a")}
	f.in.theirs[ItemWorkflow+"/hidden"] = wfJSON("hidden", "a")
	f.in.idx.add(ItemWorkflow, "hidden", "x3")
	// absent and probe agrees ⇒ confirmed gone.
	f.in.base[ItemWorkflow+"/gone"] = StoredPackItem{Type: ItemWorkflow, Key: "gone", Content: wfJSON("gone", "a")}
	f.in.theirs[ItemWorkflow+"/gone"] = wfJSON("gone", "a")
	// probe failed for the type ⇒ never conclude "deleted".
	g := newFixture()
	g.in.idx.ok = map[string]bool{}
	g.in.base[ItemWorkflow+"/w"] = StoredPackItem{Type: ItemWorkflow, Key: "w", Content: wfJSON("w", "a")}
	g.in.theirs[ItemWorkflow+"/w"] = wfJSON("w", "a")

	p := f.plan(t)
	if it := find(t, p, ItemWorkflow, "dup"); it.Status != StatusAmbiguousName {
		t.Errorf("dup: %s", it.Status)
	}
	if it := find(t, p, ItemWorkflow, "hidden"); it.Status != StatusUndiffable {
		t.Errorf("hidden: %s", it.Status)
	}
	if it := find(t, p, ItemWorkflow, "gone"); it.Status != StatusDeletedByBuilder {
		t.Errorf("gone: %s", it.Status)
	}
	if it := find(t, g.plan(t), ItemWorkflow, "w"); it.Status != StatusUndiffable {
		t.Errorf("failed probe must be undiffable, got %s", it.Status)
	}
}

// T009: planHash stability and sensitivity.
func TestPlanHash(t *testing.T) {
	build := func(v string) string {
		f := newFixture()
		f.workflow("a", side{"x", "y", "x"})
		f.workflow("b", side{"x", v, "x"})
		return f.plan(t).PlanHash
	}
	if build("y") != build("y") {
		t.Fatal("same input must give the same hash")
	}
	if build("y") == build("changed") {
		t.Fatal("a one-byte change on THEIRS must change the hash")
	}
	// LIVE and BASE changes too.
	f1, f2 := newFixture(), newFixture()
	f1.workflow("a", side{"x", "y", "x"})
	f2.workflow("a", side{"x", "y", "x"})
	f2.in.live[ItemWorkflow+"/a"] = wfJSON("a", "x2")
	if f1.plan(t).PlanHash == f2.plan(t).PlanHash {
		t.Fatal("a change on LIVE must change the hash")
	}
	f3 := newFixture()
	f3.workflow("a", side{"x2", "y", "x"})
	if f1.plan(t).PlanHash == f3.plan(t).PlanHash {
		t.Fatal("a change on BASE must change the hash")
	}
	// Only the default action differs (a stand-in for REQUIRED_ON_EXISTING_DATA flipping).
	p := f1.plan(t)
	h1 := p.PlanHash
	p.Items[0].DefaultAction = "apply"
	h2, err := planHash("t1", p)
	if err != nil || h1 == h2 {
		t.Fatalf("defaultAction must be part of the hash (err=%v)", err)
	}
	// Tenant is part of the hash.
	if h3, _ := planHash("t2", p); h3 == h2 {
		t.Fatal("tenant must be part of the hash")
	}
}

// T010: semver + source.
func TestResolveVersions(t *testing.T) {
	rows := func(vs ...string) []StoredPackItem {
		var out []StoredPackItem
		for _, v := range vs {
			out = append(out, StoredPackItem{PackVersion: v})
		}
		return out
	}
	ledger := func(vs ...string) []InstallRecord {
		var out []InstallRecord
		for _, v := range vs {
			out = append(out, InstallRecord{Slug: "p", Version: v})
		}
		return out
	}
	sem := func(v string) [3]int { s, _ := parseSemver(v); return s }
	tests := []struct {
		name      string
		to        string
		rows      []StoredPackItem
		ledger    []InstallRecord
		wantErr   string
		untracked bool
		from      string
	}{
		{"1.10.0 > 1.9.0", "1.10.0", rows("1.9.0"), nil, "", false, "1.9.0"},
		{"same version allowed", "1.9.0", rows("1.9.0"), nil, "", false, "1.9.0"},
		{"lower than max base", "1.8.0", rows("1.7.0", "1.9.0"), nil, "PACK_UPGRADE_DOWNGRADE", false, ""},
		{"no base: ledger newer", "1.7.0", nil, ledger("1.8.0"), "PACK_UPGRADE_DOWNGRADE", true, ""},
		{"no base, ledger ok", "1.8.0", nil, ledger("1.7.0"), "", true, "1.7.0"},
		{"no base, no ledger", "1.8.0", nil, nil, "NOT_FOUND", true, ""},
		{"mixed base versions sorted by semver", "1.10.0", rows("1.10.0", "1.9.0"), nil, "", false, "1.9.0,1.10.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, untracked, err := resolveVersions("p", tt.to, sem(tt.to), tt.rows, tt.ledger)
			switch tt.wantErr {
			case "":
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				if untracked != tt.untracked || strings.Join(from, ",") != tt.from {
					t.Fatalf("got from=%v untracked=%v", from, untracked)
				}
			case "NOT_FOUND":
				if err != ErrInstallNotFound {
					t.Fatalf("want ErrInstallNotFound, got %v", err)
				}
			default:
				if codeOf(err) != tt.wantErr {
					t.Fatalf("want %s, got %v", tt.wantErr, err)
				}
			}
		})
	}
}

func codeOf(err error) string {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

// T010 (service level): slug / no-slug / no-store guards run before any read.
func TestPlanUpgrade_SourceGuards(t *testing.T) {
	svc := NewService(Deps{}).WithItemStore(&planItemStore{})
	ctx := context.Background()
	if _, err := svc.PlanUpgrade(ctx, "t1", "p", &Pack{}, "upload"); codeOf(err) != "PACK_UPGRADE_NO_SLUG" {
		t.Errorf("no slug: %v", err)
	}
	if _, err := svc.PlanUpgrade(ctx, "t1", "p", &Pack{Manifest: &Manifest{Slug: "other", Version: "1.0.0"}}, "upload"); codeOf(err) != "PACK_UPGRADE_SLUG_MISMATCH" {
		t.Errorf("slug mismatch: %v", err)
	}
	if _, err := svc.PlanUpgrade(ctx, "t1", "p", &Pack{Manifest: &Manifest{Slug: "p", Version: "1.0.0"}}, "upload"); err != ErrInstallNotFound {
		t.Errorf("no base + no ledger: want ErrInstallNotFound, got %v", err)
	}
}

// T011 (FR-016): a type whose writer is not wired vanishes from all three sides and is warned about.
func TestPlanUpgrade_UnwiredWriterTypeExcluded(t *testing.T) {
	meta := &fakeMintMeta{}
	store := &planItemStore{rows: []StoredPackItem{{Type: ItemWorkflow, Key: "old rule", PackVersion: "1.0.0", Content: wfJSON("old rule", "a"), LiveID: "w1"}}}
	svc := NewService(Deps{EntityReader: meta, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}}).WithItemStore(store)
	pack := &Pack{
		Manifest:  &Manifest{Slug: "p", Version: "1.1.0"},
		Workflows: []PackWorkflow{{Name: "new rule", EntityName: "e", Trigger: "after_create"}},
	}
	p, err := svc.PlanUpgrade(context.Background(), "t1", "p", pack, "upload")
	if err != nil {
		t.Fatalf("PlanUpgrade: %v", err)
	}
	for _, it := range p.Items {
		if it.ItemType == ItemWorkflow {
			t.Errorf("workflow item %q leaked although the workflow writer is not wired (false 'Gói đã bỏ')", it.ItemKey)
		}
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], ItemWorkflow) {
		t.Errorf("warnings must name the excluded type, got %v", p.Warnings)
	}
}

// T017a: a webhook secret/header on ANY side never reaches the plan payload.
func TestComputePlan_NoSecretsInPayload(t *testing.T) {
	secretWF := func(v string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"name":"w","entityName":"e","trigger":"after_create","actions":[{"type":"outbound_webhook","url":"https://x.test/%s","secret":"SECRET-MARKER-1","headers":{"Authorization":"MARKER"}}]}`, v))
	}
	f := newFixture()
	f.in.base[ItemWorkflow+"/w"] = StoredPackItem{Type: ItemWorkflow, Key: "w", Content: secretWF("a"), LiveID: "id-w"}
	f.in.theirs[ItemWorkflow+"/w"] = secretWF("b")
	f.in.live[ItemWorkflow+"/w"] = secretWF("c")
	f.in.idx.add(ItemWorkflow, "w", "id-w")
	p := f.plan(t)
	if find(t, p, ItemWorkflow, "w").Status != StatusConflict {
		t.Fatalf("fixture should be a conflict so all three bodies are echoed")
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SECRET-MARKER") || strings.Contains(string(b), "MARKER\"") {
		t.Fatalf("secret leaked into plan payload: %s", b)
	}
}

// planItemStore is a minimal in-memory itemStore for non-integration tests.
type planItemStore struct{ rows []StoredPackItem }

func (f *planItemStore) SaveItems(context.Context, string, string, string, []PackItem) error {
	return fmt.Errorf("planItemStore: L1 plan must never write provenance")
}
func (f *planItemStore) ListItems(context.Context, string, string) ([]StoredPackItem, error) {
	return f.rows, nil
}

// Review fix: approval graph/unknown keys are refused (not flattened) and untracked sections warn.
func TestPlanUpgrade_ApprovalShapeAndUntrackedWarnings(t *testing.T) {
	store := &planItemStore{rows: []StoredPackItem{{Type: ItemWorkflow, Key: "x", PackVersion: "1.0.0", Content: wfJSON("x", "a"), LiveID: "w1"}}}
	svc := NewService(Deps{EntityReader: &fakeMintMeta{}, LayoutReader: fakeApLayouts{}, RoleReader: fakeApRoleReader{}}).WithItemStore(store)
	ctx := context.Background()
	var pa PackApproval
	if err := json.Unmarshal([]byte(`{"name":"a","entityName":"e","trigger":"manual","steps":[],"graph":{"nodes":[]}}`), &pa); err != nil {
		t.Fatal(err)
	}
	pack := &Pack{Manifest: &Manifest{Slug: "p", Version: "1.1.0"}, Approvals: []PackApproval{pa}}
	if _, err := svc.PlanUpgrade(ctx, "t1", "p", pack, "upload"); err == nil {
		t.Error("approval with graph must be refused, got nil error")
	}
	pack = &Pack{Manifest: &Manifest{Slug: "p", Version: "1.1.0"},
		FieldACLs: []PackFieldACL{{}}, InboundHooks: []PackInboundHook{{}}}
	p, err := svc.PlanUpgrade(ctx, "t1", "p", pack, "upload")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) < 2 {
		t.Errorf("untracked sections must warn, got %v", p.Warnings)
	}
}
