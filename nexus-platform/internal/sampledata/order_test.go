package sampledata

import (
	"reflect"
	"testing"
)

func rel(name, target string, required bool) Field {
	return Field{Name: name, Type: "relation", Required: required, Options: opts("relatedEntity", target)}
}

func ent(name string, fields ...Field) Entity { return Entity{Name: name, Label: name, Fields: fields} }

func skippedMap(p PlanResult) map[string]string {
	m := map[string]string{}
	for _, s := range p.Skipped {
		m[s.Entity] = s.Code
	}
	return m
}

func TestPlan_ParentsBeforeChildren(t *testing.T) {
	in := PlanInput{
		Entities: []Entity{
			ent("appointments", rel("customer", "customers", true), rel("service", "services", false)),
			ent("services", Field{Name: "n", Type: "text"}),
			ent("customers", Field{Name: "n", Type: "text"}),
		},
		Selected: []string{"appointments", "services", "customers"},
		Existing: map[string]bool{},
	}
	p := Plan(in)
	pos := map[string]int{}
	for i, e := range p.Order {
		pos[e] = i
	}
	if len(p.Order) != 3 || pos["customers"] > pos["appointments"] || pos["services"] > pos["appointments"] {
		t.Fatalf("Order = %v, want parents before appointments", p.Order)
	}
	if len(p.AddedParents) != 0 || len(p.Skipped) != 0 {
		t.Fatalf("addedParents=%v skipped=%v, want none", p.AddedParents, p.Skipped)
	}
}

func TestPlan_OrderIsDeterministic(t *testing.T) {
	in := PlanInput{Entities: []Entity{ent("b"), ent("a"), ent("c")}, Selected: []string{"b", "a", "c"}, Existing: map[string]bool{}}
	want := Plan(in).Order
	for i := 0; i < 20; i++ {
		if got := Plan(in).Order; !reflect.DeepEqual(got, want) {
			t.Fatalf("order changed: %v vs %v", got, want)
		}
	}
}

// A parent that was loaded only because the child needs it and is empty is reported as added.
func TestPlan_AddedParentsAreTheLoadedButNotSelected(t *testing.T) {
	in := PlanInput{
		Entities: []Entity{ent("appointments", rel("customer", "customers", true)), ent("customers")},
		Selected: []string{"appointments"},
		Existing: map[string]bool{},
	}
	p := Plan(in)
	if !reflect.DeepEqual(p.AddedParents, []string{"customers"}) || !reflect.DeepEqual(p.Order, []string{"customers", "appointments"}) {
		t.Fatalf("plan = %+v, want customers added and first", p)
	}
}

func TestPlan_OptionalCycleIsBroken(t *testing.T) {
	in := PlanInput{
		Entities: []Entity{ent("a", rel("b", "b", false)), ent("b", rel("a", "a", true))},
		Selected: []string{"a", "b"},
		Existing: map[string]bool{},
	}
	p := Plan(in)
	// a points at b optionally, b points at a as required: drop the optional edge, a goes first.
	if len(p.Skipped) != 0 || !reflect.DeepEqual(p.Order, []string{"a", "b"}) {
		t.Fatalf("plan = %+v, want order [a b]", p)
	}
}

func TestPlan_RequiredCycleWithoutExistingRowsIsSkipped(t *testing.T) {
	in := PlanInput{
		Entities: []Entity{ent("a", rel("b", "b", true)), ent("b", rel("a", "a", true)), ent("c")},
		Selected: []string{"a", "b", "c"},
		Existing: map[string]bool{},
	}
	p := Plan(in)
	sk := skippedMap(p)
	if sk["a"] != "SAMPLE_PARENT_EMPTY" || sk["b"] != "SAMPLE_PARENT_EMPTY" || len(sk) != 2 {
		t.Fatalf("skipped = %v, want a and b SAMPLE_PARENT_EMPTY", sk)
	}
	if !reflect.DeepEqual(p.Order, []string{"c"}) {
		t.Fatalf("order = %v, want [c]", p.Order)
	}
}

func TestPlan_RequiredCycleWithExistingRowsIsOrdered(t *testing.T) {
	in := PlanInput{
		Entities: []Entity{ent("a", rel("b", "b", true)), ent("b", rel("a", "a", true))},
		Selected: []string{"a", "b"},
		Existing: map[string]bool{"a": true},
	}
	p := Plan(in)
	if len(p.Skipped) != 0 || len(p.Order) != 2 {
		t.Fatalf("plan = %+v, want both generated (a already has rows)", p)
	}
}

func TestPlan_SelfRelation(t *testing.T) {
	optional := Plan(PlanInput{Entities: []Entity{ent("c", rel("parent", "c", false))}, Selected: []string{"c"}, Existing: map[string]bool{}})
	if len(optional.Skipped) != 0 || len(optional.Order) != 1 {
		t.Fatalf("optional self relation: %+v", optional)
	}
	required := Plan(PlanInput{Entities: []Entity{ent("c", rel("parent", "c", true))}, Selected: []string{"c"}, Existing: map[string]bool{}})
	if skippedMap(required)["c"] != "SAMPLE_PARENT_EMPTY" {
		t.Fatalf("required self relation with no rows: %+v", required)
	}
	withRows := Plan(PlanInput{Entities: []Entity{ent("c", rel("parent", "c", true))}, Selected: []string{"c"}, Existing: map[string]bool{"c": true}})
	if len(withRows.Skipped) != 0 {
		t.Fatalf("required self relation with rows: %+v", withRows)
	}
}

func TestPlan_RequiredFieldBlockers(t *testing.T) {
	in := PlanInput{
		Entities: []Entity{
			ent("docs", Field{Name: "f", Type: "file", Required: true}),
			ent("tasks", Field{Name: "u", Type: "user", Required: true}),
			ent("ok", Field{Name: "n", Type: "text"}),
			// parent skipped for a file ⇒ child cannot point at it
			ent("notes", rel("doc", "docs", true)),
		},
		Selected: []string{"docs", "tasks", "ok", "notes"},
		Existing: map[string]bool{},
		HasUsers: false,
	}
	p := Plan(in)
	sk := skippedMap(p)
	want := map[string]string{"docs": "SAMPLE_FILE_REQUIRED", "tasks": "SAMPLE_NO_USER", "notes": "SAMPLE_PARENT_EMPTY"}
	if !reflect.DeepEqual(sk, want) {
		t.Fatalf("skipped = %v, want %v", sk, want)
	}
	if !reflect.DeepEqual(p.Order, []string{"ok"}) {
		t.Fatalf("order = %v, want [ok]", p.Order)
	}
	in.HasUsers = true
	if got := skippedMap(Plan(in))["tasks"]; got != "" {
		t.Fatalf("tasks skipped with %q although a user exists", got)
	}
}

func TestPlan_RequiredParentMissingEntirely(t *testing.T) {
	in := PlanInput{Entities: []Entity{ent("a", rel("x", "ghost", true))}, Selected: []string{"a"}, Existing: map[string]bool{}}
	if got := skippedMap(Plan(in))["a"]; got != "SAMPLE_PARENT_EMPTY" {
		t.Fatalf("skipped = %q, want SAMPLE_PARENT_EMPTY", got)
	}
	in.Existing["ghost"] = true // rows exist in the DB although the table was not loaded
	if got := skippedMap(Plan(in))["a"]; got != "" {
		t.Fatalf("skipped = %q, want generated from DB candidates", got)
	}
}

func TestRequiredTargets(t *testing.T) {
	e := ent("a", rel("x", "p1", true), rel("y", "p2", false), rel("z", "p1", true), rel("self", "a", true),
		Field{Name: "poly", Type: "polymorphic_relation", Required: true, Options: opts("targetEntities", vals("p3"))})
	if got := RequiredTargets(e); !reflect.DeepEqual(got, []string{"p1"}) {
		t.Fatalf("RequiredTargets = %v, want [p1] (distinct, required relations only, no self)", got)
	}
}
