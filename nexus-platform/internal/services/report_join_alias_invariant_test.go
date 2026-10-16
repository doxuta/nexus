package services

// jsonPathAliased (dynrepo/record.go) and RowScope.rowFilterFor (dynrepo/rowscope.go) inline the
// table alias into SQL WITHOUT sanitizing it, while every sibling emit site validates. Both carry a
// comment saying the alias is "a SERVER-generated identifier, never client input" — which is true
// today, and is the whole reason this is latent rather than a hole.
//
// The right place to enforce that is where aliases are PRODUCED, not at each of the places they are
// consumed: adding an error return to two string builders would change signatures across the
// package for a value that is a compile-time constant. So this test pins the invariant those
// comments rely on. If someone later derives an alias from a field name, an entity name, or
// anything a user can influence, this fails before the value can reach a query.
//
// It lives in services (not dynrepo) because that is where the constants are declared.

import (
	"testing"

	"nexus/platform/internal/ddl"
)

func TestReportJoinAliases_AreLegalIdentifiers(t *testing.T) {
	// Every alias the engine emits into SQL. "base" is dynrepo's own (aggregate.go); the other two
	// are the report-join hops declared in record_aggregate_join.go.
	for _, alias := range []string{"base", reportJoinAlias, reportJoinLinkAlias} {
		t.Run(alias, func(t *testing.T) {
			got, err := ddl.SanitizeIdentifier(alias)
			if err != nil {
				t.Fatalf("alias %q is not a legal identifier: %v — it is inlined into SQL unsanitized", alias, err)
			}
			if got != alias {
				t.Errorf("sanitizer rewrote %q to %q; the emitted alias must be the literal one", alias, got)
			}
		})
	}
}

// The aliases must also stay DISTINCT: two hops sharing one alias would silently make a predicate
// bind to the wrong table rather than fail, which is the kind of bug a JOIN test can pass through.
func TestReportJoinAliases_AreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, alias := range []string{"base", reportJoinAlias, reportJoinLinkAlias} {
		if seen[alias] {
			t.Fatalf("alias %q is used twice — a predicate would bind to the wrong table", alias)
		}
		seen[alias] = true
	}
}
