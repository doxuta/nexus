package vertical_pack

import (
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

// TestComputedFieldTypeParity guards against drift between export's computed-key strip list
// (computedFieldType) and the write-seam readonly list (services.ComputedReadonlyFieldType) —
// a new computed type added to one but not the other would leak server-assigned values into
// includeData samples and 422 the re-install (security 🔵-1).
func TestComputedFieldTypeParity(t *testing.T) {
	for _, ft := range []string{"auto_number", "lookup", "rollup", "formula"} {
		if !computedFieldType(ft) {
			t.Errorf("export computedFieldType(%q)=false; want true", ft)
		}
		if !services.ComputedReadonlyFieldType(ft) {
			t.Errorf("write-seam ComputedReadonlyFieldType(%q)=false; want true — lists drifted", ft)
		}
	}
	for _, ft := range []string{"text", "number", "currency", "picklist", "relation", "date"} {
		if computedFieldType(ft) {
			t.Errorf("export computedFieldType(%q)=true; want false", ft)
		}
		if services.ComputedReadonlyFieldType(ft) {
			t.Errorf("write-seam ComputedReadonlyFieldType(%q)=true; want false", ft)
		}
	}
}

// TestResolveRefsEscaping proves a non-relation literal starting with "@" survives the
// export→install round-trip: export doubles it, resolveRefs drops exactly one — never resolved
// as a relation ref (reviewer must-fix #1).
func TestResolveRefsEscaping(t *testing.T) {
	refMap := map[string]refEntry{"@r1": {entity: "deals", id: "uuid-1"}}
	// Simulate what export writes: relation ref single-@, literals doubled by escapeRefLiteral.
	in := map[string]any{
		"account": "@r1",                         // relation ref
		"note":    escapeRefLiteral("@followup"), // literal -> "@@followup"
		"code":    escapeRefLiteral("@r1"),       // literal equal to a ref -> "@@r1"
		"plain":   "hello",
	}
	out, err := resolveRefs(in, refMap)
	if err != nil {
		t.Fatalf("resolveRefs: %v", err)
	}
	if out["account"] != "uuid-1" {
		t.Errorf("account = %v; want resolved uuid-1", out["account"])
	}
	if out["note"] != "@followup" {
		t.Errorf("note = %v; want literal @followup (was corrupted or errored pre-fix)", out["note"])
	}
	if out["code"] != "@r1" {
		t.Errorf("code = %v; want literal @r1 (NOT resolved to a uuid)", out["code"])
	}
	if out["plain"] != "hello" {
		t.Errorf("plain = %v; want hello", out["plain"])
	}
}

func TestEscapeRefLiteral(t *testing.T) {
	cases := map[string]any{"@x": "@@x", "@@x": "@@@x", "plain": "plain", "": ""}
	for in, want := range cases {
		if got := escapeRefLiteral(in); got != want {
			t.Errorf("escapeRefLiteral(%q) = %v; want %v", in, got, want)
		}
	}
	if got := escapeRefLiteral(42); got != 42 {
		t.Errorf("non-string passthrough failed: %v", got)
	}
	if !strings.HasPrefix(escapeRefLiteral("@r5").(string), "@@") {
		t.Error("escaped literal must start with @@")
	}
}
