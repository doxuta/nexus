package vertical_pack

// specs/069 L3 (T061, T062): the role / grant / field-restriction part of the upgrade, over fakes.
// Planning goes through computePlan with fabricated sides; the single-write rule of a role goes
// through applyEnv.ensureRole with a recording RoleService fake.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

func roleJSON(name string, grants ...string) json.RawMessage { // grants: "entity:action=level"
	var as []string
	for _, g := range grants {
		ea, lvl, _ := strings.Cut(g, "=")
		e, a, _ := strings.Cut(ea, ":")
		as = append(as, fmt.Sprintf(`{"entityName":%q,"action":%q,"accessLevel":%q}`, e, a, lvl))
	}
	return json.RawMessage(fmt.Sprintf(`{"name":%q,"label":"L","actions":[%s]}`, name, strings.Join(as, ",")))
}

func aclJSON(role, ent, field string, view, edit bool) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"roleName":%q,"entityName":%q,"fieldName":%q,"canView":%t,"canEdit":%t}`, role, ent, field, view, edit))
}

func readyFixture() *planFixture {
	f := newFixture()
	f.in.ready = func(string) bool { return true }
	return f
}

func (f *planFixture) roleSides(name string, b, t, l json.RawMessage) {
	k := ItemRole + "/" + name
	if b != nil {
		f.in.base[k] = StoredPackItem{Type: ItemRole, Key: name, PackVersion: "1.0.0", Content: b, LiveID: "r-" + name}
	}
	if t != nil {
		f.in.theirs[k] = t
	}
	if l != nil {
		f.in.live[k] = l
		f.in.idx.add(ItemRole, name, "r-"+name)
	}
}

func TestPlanRoles_GrantDecisions(t *testing.T) {
	f := readyFixture()
	// v2: pack adds qa_contracts view; builder widened qa_devices edit owner -> all by hand.
	f.roleSides("qa_staff",
		roleJSON("qa_staff", "qa_devices:view=owner", "qa_devices:edit=owner"),
		roleJSON("qa_staff", "qa_devices:view=owner", "qa_devices:edit=owner", "qa_contracts:view=owner"),
		roleJSON("qa_staff", "qa_devices:view=owner", "qa_devices:edit=all"))
	// v3-like: pack lowers a grant the builder did not touch, and drops another.
	f.roleSides("qa_lead",
		roleJSON("qa_lead", "t1:view=all", "t2:view=all"),
		roleJSON("qa_lead", "t1:view=owner"),
		roleJSON("qa_lead", "t1:view=all", "t2:view=all"))
	// pack ships '*' for a role that already exists.
	f.roleSides("qa_wild",
		roleJSON("qa_wild", "t1:view=all"),
		roleJSON("qa_wild", "t1:view=all", "*:view=all"),
		roleJSON("qa_wild", "t1:view=all"))
	p := f.plan(t)

	added := find(t, p, ItemRoleGrant, "qa_staff/qa_contracts/view")
	if added.Status != StatusAdded || added.DefaultAction != ActionApply || hasConfirm(added.Warnings) {
		t.Errorf("new grant: %s default=%s warnings=%v; want added, pre-selected apply", added.Status, added.DefaultAction, added.Warnings)
	}
	widened := find(t, p, ItemRoleGrant, "qa_staff/qa_devices/edit")
	if widened.Status != StatusKeptCustom || widened.DefaultAction != ActionKeep {
		t.Errorf("hand-widened grant: %s default=%s; want kept_custom, keep", widened.Status, widened.DefaultAction)
	}
	lowered := find(t, p, ItemRoleGrant, "qa_lead/t1/view")
	if lowered.Status != StatusUpdateSafe || lowered.DefaultAction != ActionKeep || !hasWarn(lowered, "GRANT_NARROWING") || !contains(lowered.Actions, ActionApply) {
		t.Errorf("lowered grant: %s default=%s actions=%v warnings=%v; want update_safe, keep by default, GRANT_NARROWING, apply offered", lowered.Status, lowered.DefaultAction, lowered.Actions, lowered.Warnings)
	}
	dropped := find(t, p, ItemRoleGrant, "qa_lead/t2/view")
	if dropped.Status != StatusRemovedInPack || dropped.DefaultAction != ActionKeep || !contains(dropped.Actions, ActionDelete) || !hasWarn(dropped, "GRANT_NARROWING") {
		t.Errorf("pack-dropped grant: %s default=%s actions=%v warnings=%v; want removed_in_pack, delete offered, never pre-selected, GRANT_NARROWING", dropped.Status, dropped.DefaultAction, dropped.Actions, dropped.Warnings)
	}
	wild := find(t, p, ItemRoleGrant, "qa_wild/*/view")
	if wild.Status != StatusUnsupported || wild.Reason != ReasonRoleWildcard || len(wild.Actions) != 1 || wild.Actions[0] != ActionKeep {
		t.Errorf("wildcard on existing role: %s/%s actions=%v; want unsupported ROLE_WILDCARD_REFUSED keep-only", wild.Status, wild.Reason, wild.Actions)
	}
}

func hasWarn(it UpgradeItem, code string) bool {
	for _, w := range it.Warnings {
		if w.Code == code && w.Level == "confirm" {
			return true
		}
	}
	return false
}

func TestPlanFieldACL_OpeningNeedsConfirm(t *testing.T) {
	key := func(role, ent, f string) string { return role + ":" + ent + "." + f }
	add := func(f *planFixture, k string, b, tt, l json.RawMessage) {
		full := ItemFieldACL + "/" + k
		if b != nil {
			f.in.base[full] = StoredPackItem{Type: ItemFieldACL, Key: k, Content: b}
		}
		if tt != nil {
			f.in.theirs[full] = tt
		}
		if l != nil {
			f.in.live[full] = l
		}
	}
	f := readyFixture()
	hidden := aclJSON("r", "d", "cost", false, false)
	readOnly := aclJSON("r", "d", "cost", true, false)
	add(f, key("r", "d", "cost"), hidden, readOnly, hidden)                                                            // pack relaxes hidden -> read-only
	add(f, key("r", "d", "note"), aclJSON("r", "d", "note", true, false), nil, aclJSON("r", "d", "note", true, false)) // pack drops the restriction
	add(f, key("r", "d", "new"), nil, hidden, nil)                                                                     // pack adds a restriction
	p := f.plan(t)

	open := find(t, p, ItemFieldACL, "r:d.cost")
	if open.Status != StatusUpdateSafe || open.DefaultAction != ActionKeep || !hasWarn(open, "FIELD_ACL_OPENING") {
		t.Errorf("relaxed restriction: %s default=%s warnings=%v; want update_safe, keep, FIELD_ACL_OPENING", open.Status, open.DefaultAction, open.Warnings)
	}
	gone := find(t, p, ItemFieldACL, "r:d.note")
	if gone.Status != StatusRemovedInPack || !contains(gone.Actions, ActionDelete) || gone.DefaultAction != ActionKeep || !hasWarn(gone, "FIELD_ACL_OPENING") {
		t.Errorf("dropped restriction: %s actions=%v default=%s; want removed_in_pack, delete offered, keep default, FIELD_ACL_OPENING", gone.Status, gone.Actions, gone.DefaultAction)
	}
	fresh := find(t, p, ItemFieldACL, "r:d.new")
	if fresh.Status != StatusAdded || fresh.DefaultAction != ActionApply || hasConfirm(fresh.Warnings) {
		t.Errorf("new restriction: %s default=%s; want added, pre-selected apply (it only hides more)", fresh.Status, fresh.DefaultAction)
	}
}

func TestFieldACLProvenance_SkipsFullAccessAndUnknownRoles(t *testing.T) {
	pack := &Pack{FieldACLs: []PackFieldACL{
		{RoleName: "r", EntityName: "d", FieldName: "a", CanView: false, CanEdit: false},
		{RoleName: "r", EntityName: "d", FieldName: "full", CanView: true, CanEdit: true},
		{RoleName: "ghost", EntityName: "d", FieldName: "b", CanView: false},
	}}
	plan := &InstallPlan{roleIDByName: map[string]string{"r": "id-r"}} // the install path: roles resolvable by name
	var got []string
	for _, it := range provenanceItems(pack, plan, nil, hasWriters{FieldACLs: true}) {
		got = append(got, it.Type+"/"+it.Key)
	}
	if len(got) != 1 || got[0] != "field_acl/r:d.a" {
		t.Fatalf("provenance items = %v; want only field_acl/r:d.a (full access and unresolvable roles are not tracked)", got)
	}
	if n := len(provenanceItems(pack, plan, nil, hasWriters{})); n != 0 {
		t.Fatalf("without a wired field-ACL writer/reader nothing may be tracked, got %d", n)
	}
	// canonicalize fills the default so a bare restriction compares equal to the explicit one.
	paths, err := comparePackItems(ItemFieldACL, json.RawMessage(`{"roleName":"r","entityName":"d","fieldName":"a","canView":false}`),
		json.RawMessage(`{"roleName":"r","entityName":"d","fieldName":"a","canView":false,"canEdit":true}`))
	if err != nil || len(paths) != 0 {
		t.Fatalf("canonicalize default canEdit=true: paths=%v err=%v", paths, err)
	}
}

// The customer-owner role: every state, every kind of row, keep-only (T061 table).
func TestPlanRoles_OwnerRoleIsReservedInEveryState(t *testing.T) {
	o := auth.OwnerRoleName
	sides := map[string][3]json.RawMessage{ // b, t, l
		"added":              {nil, roleJSON(o, "x:view=all"), nil},
		"update_safe":        {roleJSON(o, "x:view=all"), roleJSON(o, "x:view=owner"), roleJSON(o, "x:view=all")},
		"kept_custom":        {roleJSON(o, "x:view=all"), roleJSON(o, "x:view=all"), roleJSON(o, "x:view=owner")},
		"conflict":           {roleJSON(o, "x:view=all"), roleJSON(o, "x:view=team"), roleJSON(o, "x:view=owner")},
		"removed_in_pack":    {roleJSON(o, "x:view=all"), nil, roleJSON(o, "x:view=all")},
		"unchanged":          {roleJSON(o, "x:view=all"), roleJSON(o, "x:view=all"), roleJSON(o, "x:view=all")},
		"in_sync (no base)":  {nil, roleJSON(o, "x:view=all"), roleJSON(o, "x:view=all")},
		"wildcard from pack": {roleJSON(o, "x:view=all"), roleJSON(o, "x:view=all", "*:view=all"), roleJSON(o, "x:view=all")},
	}
	for name, s := range sides {
		f := readyFixture()
		f.roleSides(o, s[0], s[1], s[2])
		f.in.theirs[ItemFieldACL+"/"+o+":d.f"] = aclJSON(o, "d", "f", false, false)
		p := f.plan(t)
		for _, it := range p.Items {
			if it.ItemType != ItemRole && it.ItemType != ItemRoleGrant && it.ItemType != ItemFieldACL {
				continue
			}
			if it.Status != StatusProtected || it.Reason != ReasonRoleReserved || len(it.Actions) != 1 || it.Actions[0] != ActionKeep || it.DefaultAction != ActionKeep || it.Adopt {
				t.Errorf("%s: %s/%s = %s reason=%s actions=%v default=%s adopt=%v; want protected ROLE_NAME_RESERVED keep-only", name, it.ItemType, it.ItemKey, it.Status, it.Reason, it.Actions, it.DefaultAction, it.Adopt)
			}
		}
		if p.Summary["writes"] != 0 || p.Summary["adoptable"] != 0 {
			t.Errorf("%s: writes=%d adoptable=%d; the owner role must contribute nothing", name, p.Summary["writes"], p.Summary["adoptable"])
		}
	}
	// Another role in the same plan still plans normally.
	f := readyFixture()
	f.roleSides(o, nil, roleJSON(o, "x:view=all"), nil)
	f.roleSides("other", nil, roleJSON("other", "x:view=all"), nil)
	if it := find(t, f.plan(t), ItemRole, "other"); it.Status != StatusAdded || it.DefaultAction != ActionApply {
		t.Errorf("a normal role next to the owner role: %s/%s", it.Status, it.DefaultAction)
	}
}

// ── apply: one UpdateRole per role, read right before, live-only grants survive ──────────────

type upgRoleWr struct {
	order   []string
	live    map[string]*services.RoleView
	updates []services.UpdateRoleInput
	created []services.CreateRoleInput
}

func (f *upgRoleWr) ListRoles(context.Context, string) ([]services.RoleView, error) {
	var out []services.RoleView
	for _, r := range f.live {
		out = append(out, services.RoleView{ID: r.ID, Name: r.Name})
	}
	return out, nil
}
func (f *upgRoleWr) CreateRole(_ context.Context, _ string, in services.CreateRoleInput) (*services.RoleView, error) {
	f.created = append(f.created, in)
	return &services.RoleView{ID: "new-" + in.Name, Name: in.Name}, nil
}
func (f *upgRoleWr) GetRole(_ context.Context, _, id string) (*services.RoleView, error) {
	f.order = append(f.order, "get")
	return f.live[id], nil
}
func (f *upgRoleWr) UpdateRole(_ context.Context, _, id string, in services.UpdateRoleInput) (*services.RoleView, error) {
	f.order = append(f.order, "update")
	f.updates = append(f.updates, in)
	return f.live[id], nil
}

func roleEnv(t *testing.T, rw *upgRoleWr, planItems []UpgradeItem, act map[string]string, theirs map[string]json.RawMessage) (*applyEnv, *upgStore) {
	t.Helper()
	st := newUpgStore()
	svc := &Service{roleWr: rw, items: st, logger: zap.NewNop()}
	e := newApplyEnv(svc, upgTenant, "u1", "p", "1.1.0", &planArtifacts{theirs: theirs})
	var eff []UpgradeDecision
	for k, a := range act {
		typ, key, _ := strings.Cut(k, "/")
		eff = append(eff, UpgradeDecision{ItemType: typ, ItemKey: key, Action: a})
	}
	e.bind(context.Background(), context.Background(), planItems, eff)
	return e, st
}

func TestEnsureRole_OneUpdateWithLiveSetAndComposedBaseline(t *testing.T) {
	rw := &upgRoleWr{live: map[string]*services.RoleView{"r-qa": {ID: "r-qa", Name: "qa", Label: "L", Actions: []services.RoleActionView{
		{EntityName: "devices", Action: "view", AccessLevel: "owner"},
		{EntityName: "devices", Action: "edit", AccessLevel: "all"}, // widened by hand
		{EntityName: "mine", Action: "view", AccessLevel: "all"},    // builder's own, not in the pack
		{EntityName: "old", Action: "view", AccessLevel: "all"},
	}}}}
	pack := roleJSON("qa", "devices:view=owner", "devices:edit=owner", "contracts:view=owner", "old:view=all")
	grant := func(ent, act, lvl string) json.RawMessage { return roleJSON("x", ent+":"+act+"="+lvl) }
	_ = grant
	kid := func(key string, theirs string) UpgradeItem {
		return UpgradeItem{ItemType: ItemRoleGrant, ItemKey: key, ParentKey: "qa", Theirs: json.RawMessage(theirs)}
	}
	items := []UpgradeItem{
		{ItemType: ItemRole, ItemKey: "qa", Status: StatusUnchanged, LiveID: "r-qa"},
		kid("qa/contracts/view", `{"entityName":"contracts","action":"view","accessLevel":"owner"}`),
		kid("qa/devices/edit", `{"entityName":"devices","action":"edit","accessLevel":"owner"}`),
		kid("qa/old/view", `{"entityName":"old","action":"view","accessLevel":"all"}`),
	}
	act := map[string]string{
		"role_grant/qa/contracts/view": ActionApply, // new grant
		"role_grant/qa/old/view":       ActionDelete,
		// qa/devices/edit: kept (the hand-widened level must survive)
	}
	e, st := roleEnv(t, rw, items, act, map[string]json.RawMessage{"role/qa": pack})
	st.rows[upgTenant+"|p|role|qa"] = StoredPackItem{Type: ItemRole, Key: "qa", Content: roleJSON("qa", "devices:view=owner", "devices:edit=owner", "old:view=all")}

	// Members of one role arrive one by one; the role is written once.
	for _, it := range items {
		if _, err := e.ensureRole(roleNameOf(&it)); err != nil {
			t.Fatalf("ensureRole(%s): %v", it.ItemKey, err)
		}
	}
	if len(rw.updates) != 1 || strings.Join(rw.order, ",") != "get,update" {
		t.Fatalf("calls = %v updates=%d; want exactly get then one update", rw.order, len(rw.updates))
	}
	got := map[string]string{}
	for _, a := range rw.updates[0].Actions {
		got[a.EntityName+"/"+a.Action] = a.AccessLevel
	}
	want := map[string]string{"devices/view": "owner", "devices/edit": "all", "mine/view": "all", "contracts/view": "owner"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("final grant set = %v; want %v (hand-widened and builder-only kept, new added, dropped removed)", got, want)
	}
	// Baseline: contracts joined the base, old left it, the kept devices/edit stays at the OLD base
	// level (so it still reads as "yours" next time); exactly one row written.
	row := st.rows[upgTenant+"|p|role|qa"]
	var base struct {
		Actions []PackRoleAction `json:"actions"`
	}
	if err := json.Unmarshal(row.Content, &base); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	lv := map[string]string{}
	for _, a := range base.Actions {
		lv[a.EntityName+"/"+a.Action] = a.AccessLevel
	}
	if fmt.Sprint(lv) != fmt.Sprint(map[string]string{"devices/view": "owner", "devices/edit": "owner", "contracts/view": "owner"}) || row.PackVersion != "1.1.0" {
		t.Fatalf("baseline grants = %v version=%s", lv, row.PackVersion)
	}
}

func TestEnsureRole_OwnerRoleIsNeverWritten(t *testing.T) {
	rw := &upgRoleWr{live: map[string]*services.RoleView{"r-o": {ID: "r-o", Name: auth.OwnerRoleName}}}
	items := []UpgradeItem{{ItemType: ItemRole, ItemKey: auth.OwnerRoleName, Status: StatusAdded}}
	e, _ := roleEnv(t, rw, items, map[string]string{"role/" + auth.OwnerRoleName: ActionApply},
		map[string]json.RawMessage{"role/" + auth.OwnerRoleName: roleJSON(auth.OwnerRoleName, "x:view=all")})
	if _, err := e.ensureRole(auth.OwnerRoleName); vcodeOf(err) != "ROLE_NAME_RESERVED" {
		t.Fatalf("err = %v; want ROLE_NAME_RESERVED", err)
	}
	if len(rw.updates)+len(rw.created) != 0 || len(rw.order) != 0 {
		t.Fatalf("the owner role was touched: %+v", rw)
	}
}

func vcodeOf(err error) string { return vcode(err) }

// A decision outside the offered actions is refused for the owner role (keep-only) — end to end
// through ApplyUpgrade's own check.
func TestPlanRoles_OwnerRoleRefusesApplyDecision(t *testing.T) {
	f := readyFixture()
	f.roleSides(auth.OwnerRoleName, nil, roleJSON(auth.OwnerRoleName, "x:view=all"), nil)
	p := f.plan(t)
	it := find(t, p, ItemRole, auth.OwnerRoleName)
	if contains(it.Actions, ActionApply) || contains(it.Actions, ActionRestore) || contains(it.Actions, ActionDelete) {
		t.Fatalf("owner role offers %v; only keep is allowed (PACK_UPGRADE_DECISION_INVALID for the rest)", it.Actions)
	}
}

// specs/069 L3 review fix: a role the pack dropped and the tenant already deleted is adopted by
// dropping its baseline row — not by failing UPGRADE_PACK_ITEM_MISSING.
func TestEnsureRole_PackAndTenantBothDroppedRoleDropsBaseline(t *testing.T) {
	rw := &upgRoleWr{live: map[string]*services.RoleView{}}
	items := []UpgradeItem{{ItemType: ItemRole, ItemKey: "gone", Status: StatusInSync}}
	e, st := roleEnv(t, rw, items, map[string]string{"role/gone": "adopt"}, map[string]json.RawMessage{})
	st.rows[upgTenant+"|p|role|gone"] = StoredPackItem{Type: ItemRole, Key: "gone", Content: roleJSON("gone", "t1:view=all")}
	if _, err := e.ensureRole("gone"); err != nil {
		t.Fatalf("ensureRole: %v; want nil (both sides dropped the role)", err)
	}
	if len(rw.order) != 0 || len(rw.updates)+len(rw.created) != 0 {
		t.Fatalf("role service touched: %v", rw.order)
	}
	if len(st.deleted) != 1 || st.deleted[0] != [2]string{ItemRole, "gone"} {
		t.Fatalf("deleted baseline rows = %v; want [role gone]", st.deleted)
	}
}

// A role the pack dropped but the tenant still has: a hand-picked delete of one of its grants
// composes from the OLD baseline instead of the (absent) pack content.
func TestEnsureRole_PackDroppedRoleStillInTenantDeletesPickedGrant(t *testing.T) {
	rw := &upgRoleWr{live: map[string]*services.RoleView{"r-gone": {ID: "r-gone", Name: "gone", Label: "L", Actions: []services.RoleActionView{
		{EntityName: "t1", Action: "view", AccessLevel: "all"},
		{EntityName: "t2", Action: "view", AccessLevel: "all"},
	}}}}
	items := []UpgradeItem{
		{ItemType: ItemRole, ItemKey: "gone", Status: StatusRemovedInPack, LiveID: "r-gone"},
		{ItemType: ItemRoleGrant, ItemKey: "gone/t1/view", ParentKey: "gone"},
	}
	e, st := roleEnv(t, rw, items, map[string]string{"role_grant/gone/t1/view": ActionDelete}, map[string]json.RawMessage{})
	st.rows[upgTenant+"|p|role|gone"] = StoredPackItem{Type: ItemRole, Key: "gone", Content: roleJSON("gone", "t1:view=all", "t2:view=all")}
	if _, err := e.ensureRole("gone"); err != nil {
		t.Fatalf("ensureRole: %v", err)
	}
	if len(rw.updates) != 1 || len(rw.updates[0].Actions) != 1 || rw.updates[0].Actions[0].EntityName != "t2" {
		t.Fatalf("updates = %+v; want one update leaving only t2/view", rw.updates)
	}
}
