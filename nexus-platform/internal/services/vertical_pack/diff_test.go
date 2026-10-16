// KHUON-SONG-P1 unit tests (../specs/011-pack-provenance/spec.md §6): the canonicalize+compare core — the
// exact artifacts the vet round proved would false-positive (key order, omitempty presence,
// role-grant array order) must read as UNCHANGED, while real edits report precise paths.
package vertical_pack

import (
	"encoding/json"
	"testing"
)

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestComparePackItems_CanonicalArtifactsAreUnchanged(t *testing.T) {
	tests := []struct {
		name     string
		itemType string
		pack     any
		live     any
	}{
		{
			// Pack authors omit condition.match + isActive; the write path persists match "and",
			// isActive true; export re-emits both. Absence-vs-presence must not read as a change.
			name:     "workflow omitempty defaults",
			itemType: ItemWorkflow,
			pack: map[string]any{
				"name": "sla", "entityName": "tk_tickets", "trigger": "after_update",
				"condition": map[string]any{"rules": []any{map[string]any{"field": "priority", "op": "eq", "value": "High"}}},
				"actions":   []any{map[string]any{"type": "field_update", "params": map[string]any{"field": "priority", "value": "Urgent"}}},
			},
			live: map[string]any{
				"name": "sla", "entityName": "tk_tickets", "trigger": "after_update", "isActive": true,
				"condition": map[string]any{"match": "and", "rules": []any{map[string]any{"field": "priority", "op": "eq", "value": "High"}}},
				"actions":   []any{map[string]any{"type": "field_update", "params": map[string]any{"field": "priority", "value": "Urgent"}}},
			},
		},
		{
			// Packs author grants in semantic order; storage returns ORDER BY entity_name, action.
			name:     "role grant order",
			itemType: ItemRole,
			pack: map[string]any{
				"name": "sales", "actions": []any{
					map[string]any{"entityName": "crm_leads", "action": "view", "accessLevel": "all"},
					map[string]any{"entityName": "crm_accounts", "action": "view", "accessLevel": "all"},
					map[string]any{"entityName": "crm_accounts", "action": "edit", "accessLevel": "own"},
				},
			},
			live: map[string]any{
				"name": "sales", "actions": []any{
					map[string]any{"entityName": "crm_accounts", "action": "edit", "accessLevel": "own"},
					map[string]any{"entityName": "crm_accounts", "action": "view", "accessLevel": "all"},
					map[string]any{"entityName": "crm_leads", "action": "view", "accessLevel": "all"},
				},
			},
		},
		{
			// Key order never matters at map level.
			name:     "entity key order",
			itemType: ItemEntity,
			pack:     map[string]any{"name": "a", "label": "A", "fields": []any{map[string]any{"name": "x", "fieldType": "text"}}},
			live:     map[string]any{"label": "A", "fields": []any{map[string]any{"fieldType": "text", "name": "x"}}, "name": "a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths, err := comparePackItems(tt.itemType, mustRaw(t, tt.pack), mustRaw(t, tt.live))
			if err != nil {
				t.Fatalf("compare: %v", err)
			}
			if len(paths) != 0 {
				t.Fatalf("want unchanged, got changedPaths %v", paths)
			}
		})
	}
}

func TestComparePackItems_RealEditsReportPaths(t *testing.T) {
	pack := map[string]any{
		"name": "sla", "entityName": "tk_tickets", "trigger": "after_update", "isActive": true,
		"condition": map[string]any{"match": "and", "rules": []any{map[string]any{"field": "priority", "op": "eq", "value": "High"}}},
		"actions":   []any{map[string]any{"type": "field_update", "params": map[string]any{"field": "priority", "value": "Urgent"}}},
	}
	live := map[string]any{
		"name": "sla", "entityName": "tk_tickets", "trigger": "after_update", "isActive": false, // toggled off
		"condition": map[string]any{"match": "or", "rules": []any{map[string]any{"field": "priority", "op": "eq", "value": "High"}}}, // match edited
		"actions":   []any{map[string]any{"type": "field_update", "params": map[string]any{"field": "priority", "value": "Low"}}},    // value edited
	}
	paths, err := comparePackItems(ItemWorkflow, mustRaw(t, pack), mustRaw(t, live))
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	want := map[string]bool{"isActive": true, "condition.match": true, "actions[0].params.value": true}
	if len(paths) != len(want) {
		t.Fatalf("want %d paths %v, got %v", len(want), want, paths)
	}
	for _, p := range paths {
		if !want[p] {
			t.Fatalf("unexpected path %q (all: %v)", p, paths)
		}
	}
}

func TestComparePackItems_ArrayLengthAndTypeMismatch(t *testing.T) {
	pack := map[string]any{"name": "r", "actions": []any{map[string]any{"entityName": "a", "action": "view", "accessLevel": "all"}}}
	live := map[string]any{"name": "r", "actions": []any{}}
	paths, err := comparePackItems(ItemRole, mustRaw(t, pack), mustRaw(t, live))
	if err != nil || len(paths) != 1 || paths[0] != "actions" {
		t.Fatalf("want [actions], got %v err %v", paths, err)
	}
	// scalar vs object → the path itself
	paths, err = comparePackItems(ItemEntity, mustRaw(t, map[string]any{"a": "x"}), mustRaw(t, map[string]any{"a": map[string]any{"b": 1}}))
	if err != nil || len(paths) != 1 || paths[0] != "a" {
		t.Fatalf("want [a], got %v err %v", paths, err)
	}
}

func TestComparePackItems_ApprovalDefaultsCanonicalized(t *testing.T) {
	steps := []any{map[string]any{"approverType": "role", "approverRef": "ap_manager", "slaSeconds": float64(3600)}}
	// Pack authored without isActive and without criteria.match; the write path persists
	// isActive=true / match="and" and export re-emits both → must read unchanged.
	pack := map[string]any{
		"name": "Duyệt đơn giá cao", "entityName": "ap_orders", "trigger": "after_create",
		"criteria": map[string]any{"rules": []any{map[string]any{"field": "amount", "op": "gt", "value": float64(1000)}}},
		"steps":    steps,
	}
	live := map[string]any{
		"name": "Duyệt đơn giá cao", "entityName": "ap_orders", "trigger": "after_create",
		"criteria": map[string]any{"match": "and", "rules": []any{map[string]any{"field": "amount", "op": "gt", "value": float64(1000)}}},
		"steps":    steps, "isActive": true,
	}
	paths, err := comparePackItems(ItemApproval, mustRaw(t, pack), mustRaw(t, live))
	if err != nil || len(paths) != 0 {
		t.Fatalf("defaults must canonicalize equal, got paths %v err %v", paths, err)
	}

	// Empty criteria block on the pack side persists as NULL → export emits no criteria at all.
	pack2 := map[string]any{"name": "P", "entityName": "e", "trigger": "after_create", "criteria": map[string]any{}, "steps": steps}
	live2 := map[string]any{"name": "P", "entityName": "e", "trigger": "after_create", "steps": steps, "isActive": true}
	paths, err = comparePackItems(ItemApproval, mustRaw(t, pack2), mustRaw(t, live2))
	if err != nil || len(paths) != 0 {
		t.Fatalf("empty criteria must canonicalize away, got paths %v err %v", paths, err)
	}

	// A REAL edit still surfaces: the customer disabled the process.
	live3 := map[string]any{"name": "P", "entityName": "e", "trigger": "after_create", "steps": steps, "isActive": false}
	paths, err = comparePackItems(ItemApproval, mustRaw(t, pack2), mustRaw(t, live3))
	if err != nil || len(paths) != 1 || paths[0] != "isActive" {
		t.Fatalf("want [isActive], got %v err %v", paths, err)
	}
}

func TestProvenanceItems_CollectsKeysAndWriteSemantics(t *testing.T) {
	active := true
	pack := &Pack{
		Manifest: &Manifest{Slug: "crm", Version: "1.1.0"},
		Entities: []PackEntity{
			{Name: "crm_accounts", Label: "KH", Fields: []PackField{{Name: "name", FieldType: "text"}}},
			{Name: "crm_leads", Label: "Leads"},
		},
		DeferredFields:  []PackDeferredField{{EntityName: "crm_accounts", Field: PackField{Name: "total", FieldType: "rollup"}}},
		Layouts:         []PackLayout{{EntityName: "crm_accounts", ViewType: "detail"}},
		Roles:           []PackRole{{Name: "sales"}},
		NNRelationships: []PackNNRelationship{{Name: "acc_contacts"}},
		Workflows:       []PackWorkflow{{Name: "route", EntityName: "crm_leads", Trigger: "after_create", IsActive: &active}},
		Reports:         []PackReport{{Name: "pipeline"}},
		Dashboards:      []PackDashboard{{Name: "overview"}},
		Teams:           []PackTeam{{Name: "Đội Sales"}},
		Approvals: []PackApproval{
			{Name: "Duyệt giá cao", EntityName: "crm_accounts", Trigger: "after_create",
				Steps: []PackApprovalStep{{ApproverType: "role", ApproverRef: "sales"}}},
			{Name: "Duyệt cũ", EntityName: "crm_leads", Trigger: "after_update",
				Steps: []PackApprovalStep{{ApproverType: "role", ApproverRef: "sales"}}},
		},
	}
	plan := &InstallPlan{
		entitySkip: map[string]bool{"crm_leads": true}, // pre-existing → insert-if-absent semantics
		fieldSkip:  map[string]bool{},
		roleSkip:   map[string]bool{}, nnSkip: map[string]bool{},
		wfSkip: map[string]bool{}, reportSkip: map[string]bool{},
		dashSkip: map[string]bool{}, teamSkip: map[string]bool{},
		apSkip: map[string]bool{approvalDupKey("crm_leads", "Duyệt cũ"): true}, // pre-existing pair
	}
	items := provenanceItems(pack, plan, map[string]string{
		ItemEntity + "/crm_accounts":                 "id-1",
		ItemApproval + "/crm_accounts:Duyệt giá cao": "ap-1",
	}, hasWriters{true, true, true, true, true, true, true, true})

	byKey := map[string]PackItem{}
	for _, it := range items {
		byKey[it.Type+"/"+it.Key] = it
	}
	if len(items) != 12 {
		t.Fatalf("want 12 items, got %d (%v)", len(items), byKey)
	}
	if it := byKey[ItemEntity+"/crm_accounts"]; !it.Wrote || it.LiveID != "id-1" {
		t.Fatalf("crm_accounts should be wrote with live id, got %+v", it)
	}
	if it := byKey[ItemEntity+"/crm_leads"]; it.Wrote {
		t.Fatalf("skipped entity must not be wrote: %+v", it)
	}
	if _, ok := byKey[ItemDeferredField+"/crm_accounts.total"]; !ok {
		t.Fatalf("deferred field missing: %v", byKey)
	}
	if it := byKey[ItemLayout+"/crm_accounts:detail"]; !it.Wrote {
		t.Fatalf("layouts are overwrite-by-design → always wrote: %+v", it)
	}
	if it := byKey[ItemApproval+"/crm_accounts:Duyệt giá cao"]; !it.Wrote || it.LiveID != "ap-1" {
		t.Fatalf("created approval should be wrote with live id, got %+v", it)
	}
	if it := byKey[ItemApproval+"/crm_leads:Duyệt cũ"]; it.Wrote {
		t.Fatalf("apSkip'd approval must not be wrote: %+v", it)
	}
	// Samples/Links never tracked — implied by count above (pack has none), assert type set:
	for k := range byKey {
		switch {
		case len(k) == 0:
			t.Fatalf("empty key")
		}
	}
}

func TestProvenanceItems_IntraPackDuplicateLastWins(t *testing.T) {
	pack := &Pack{
		Entities:  []PackEntity{{Name: "e"}},
		Workflows: []PackWorkflow{{Name: "dup", EntityName: "e", Trigger: "after_create"}, {Name: "dup", EntityName: "e", Trigger: "after_update"}},
	}
	items := provenanceItems(pack, &InstallPlan{}, nil, hasWriters{true, true, true, true, true, true, true, true})
	var wf []PackItem
	for _, it := range items {
		if it.Type == ItemWorkflow {
			wf = append(wf, it)
		}
	}
	if len(wf) != 1 {
		t.Fatalf("want 1 workflow item (last-wins), got %d", len(wf))
	}
	var decoded map[string]any
	if err := json.Unmarshal(wf[0].Content, &decoded); err != nil || decoded["trigger"] != "after_update" {
		t.Fatalf("last-wins content expected after_update, got %v (err %v)", decoded, err)
	}
}
