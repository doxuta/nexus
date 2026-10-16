package vertical_pack

import (
	"reflect"
	"testing"
)

// T014 — the contract with the pack authors (queue #295 and every later slice): a field added to
// Pack is either TRACKED (a provenance item type + canonicalize + an upgrade adapter, so it can
// be upgraded) or listed here as exempt WITH a reason. Without this a new pack section would
// install fine and silently never upgrade (the "đoạn nối không ai viết" class).
var (
	// packFieldTracked maps a Pack field to the provenance item type that carries it.
	packFieldTracked = map[string]string{
		"Entities":        ItemEntity,
		"DeferredFields":  ItemDeferredField,
		"Layouts":         ItemLayout,
		"Roles":           ItemRole,
		"NNRelationships": ItemNN,
		"Workflows":       ItemWorkflow,
		"Reports":         ItemReport,
		"Dashboards":      ItemDashboard,
		"Teams":           ItemTeam,
		"WorkQueues":      ItemWorkQueue,
		"Approvals":       ItemApproval,
		"FieldACLs":       ItemFieldACL,
		"InboundHooks":    ItemInboundHook,
	}
	// packFieldExempt: not upgradable on purpose.
	packFieldExempt = map[string]string{
		"Manifest":      "metadata of the pack itself",
		"Name":          "metadata (legacy display name)",
		"Description":   "metadata (legacy description)",
		"Samples":       "customer data from day one — never upgraded (specs/011 §4.1)",
		"Links":         "customer data (N-N rows between samples)",
		"Relationships": "derived view of relation fields; install ignores it",
		"Credentials":   "vault key slots made once at install, reused by name, never overwritten (specs/050 S6); the rule's provenance carries the key NAME",
		"Calendars":     "created once by name at install, never overwritten; calendar upgrade is not in scope (queue #720 debt)",
		"Views":         "shared saved views: created once by (entity, name), never overwritten; upgrade is not in scope (queue #720 debt)",
		"DemoSamples":   "install-time flag set in code for built-in packs (json:\"-\"); not pack content (specs/064 S9-6)",
	}
	// packFieldPending: tracked LATER; an entry must be removed in the named slice. Empty since L5
	// (T090) tracked InboundHooks.
	packFieldPending = map[string]string{}
)

func TestPackStructure_EveryFieldTrackedOrExempt(t *testing.T) {
	typ := reflect.TypeOf(Pack{})
	seen := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		seen[name] = true
		_, tracked := packFieldTracked[name]
		_, exempt := packFieldExempt[name]
		_, pending := packFieldPending[name]
		if !tracked && !exempt && !pending {
			t.Errorf("Pack.%s is neither tracked by provenance (+ canonicalize + upgrade adapter) nor listed as exempt with a reason — add it to pack_structure_test.go", name)
		}
	}
	for _, m := range []map[string]string{packFieldTracked, packFieldExempt, packFieldPending} {
		for name := range m {
			if !seen[name] {
				t.Errorf("stale entry %q: Pack has no such field any more", name)
			}
		}
	}
}

// The table above must not lie: a pack carrying one of each tracked section yields every claimed
// item type from the SAME walker the install and the upgrade plan use.
func TestPackStructure_TrackedTypesAreEmittedByProvenance(t *testing.T) {
	pack := &Pack{
		Entities:        []PackEntity{{Name: "e"}},
		DeferredFields:  []PackDeferredField{{EntityName: "e", Field: PackField{Name: "f", FieldType: "rollup"}}},
		Layouts:         []PackLayout{{EntityName: "e", ViewType: "form"}},
		Roles:           []PackRole{{Name: "r"}},
		NNRelationships: []PackNNRelationship{{Name: "nn"}},
		Workflows:       []PackWorkflow{{Name: "w"}},
		Reports:         []PackReport{{Name: "rp"}},
		Dashboards:      []PackDashboard{{Name: "d"}},
		Teams:           []PackTeam{{Name: "tm"}},
		WorkQueues:      []PackWorkQueue{{Name: "q"}},
		Approvals:       []PackApproval{{Name: "a", EntityName: "e"}},
		FieldACLs:       []PackFieldACL{{RoleName: "r", EntityName: "e", FieldName: "f"}},
		InboundHooks:    []PackInboundHook{{Name: "h", EntityName: "e"}},
	}
	got := map[string]bool{}
	for _, it := range provenanceItems(pack, &InstallPlan{}, nil, hasWriters{true, true, true, true, true, true, true, true}) {
		got[it.Type] = true
	}
	for field, typ := range packFieldTracked {
		if !got[typ] {
			t.Errorf("Pack.%s claims item type %q but provenanceItems emitted none", field, typ)
		}
	}
}
