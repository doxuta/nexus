package metaversion

import "testing"

func TestClassOf(t *testing.T) {
	for _, typ := range []string{"workflow", "report", "dashboard"} {
		if got := ClassOf(typ).Kind; got != Draft {
			t.Errorf("%s = %q, want draft", typ, got)
		}
	}
	for _, typ := range []string{"role", "field_acl"} {
		if got := ClassOf(typ).Kind; got != DirectTraced {
			t.Errorf("%s = %q, want direct_traced", typ, got)
		}
	}
	for _, unknown := range []string{"", "app_lifecycle", "totally-new"} {
		if got := ClassOf(unknown).Kind; got != DirectUntraced {
			t.Errorf("ClassOf(%q) = %q, want direct_untraced (fail-safe: never promise a draft without an adapter)", unknown, got)
		}
	}
	if got := TypesOfKind(DirectUntraced); got != nil {
		t.Errorf("TypesOfKind(untraced) = %v, want nil (open set)", got)
	}
}

// An N-N relationship has no draft (POST /admin/relationships applies at once) but a NEW one is hidden from the
// customer until publish (GET /metadata/relationships drops it by the schema cutoff): the class says so, so the
// pack upgrade promises "hidden until publish", not "applied at once".
func TestClassOf_RelationshipIsNewHidden(t *testing.T) {
	if c := ClassOf("relationship"); c.Kind != DirectUntraced || !c.NewHidden {
		t.Fatalf("ClassOf(relationship) = %+v, want direct_untraced + NewHidden", c)
	}
	if got := TypesOfKind(DirectUntraced); got != nil {
		t.Errorf("TypesOfKind(untraced) = %v, want nil (open set)", got)
	}
}
