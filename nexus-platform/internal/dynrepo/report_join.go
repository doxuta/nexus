package dynrepo

import (
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
)

// JoinSpec describes ONE resolved hop of a cross-object aggregate report (REPORT-JOIN). The
// SERVICE resolves the relationship, the hop direction, and the caller's row-level access to the
// related entity; this struct carries only sanitized/validated inputs. The ACL SQL (tenant filter
// + A5 row-level predicate for the related entity) is emitted HERE, in dynrepo, by the SAME
// rowFilterFor the single-table path uses — so a joined row can never enter the result set
// without passing the related entity's tenant + row-level ACL (the cross-tenant / cross-
// visibility leak bug-class).
//
// A FK-on-child (1-1/1-N) join is exactly ONE JoinSpec chained off the base table (v1). An N-N
// join (REPORT-JOIN-NN) is TWO chained JoinSpecs: base → rel_<a>_<b> link table → far entity —
// the second hop's FromAlias names the first hop's Alias so its ON clause chains off the link
// table instead of the overall base.
type JoinSpec struct {
	// Alias is the server-generated table alias for this hop's related table ("j0", or "lt" for
	// an N-N link-table hop). NEVER client input. Re-sanitized on emit as defense-in-depth.
	Alias string
	// RelatedTable is the physical table name this hop joins in (an obj_<related> table, or a
	// rel_<a>_<b> N-N link table).
	RelatedTable string
	// Entity is the canonical entity name of RelatedTable (an obj_<entity> hop; empty for an N-N link
	// table). It is what lets the hop's scope honour manual shares (DAILY-QUEUE #225): the share clause
	// is pinned to (tenant, Entity) exactly like a single-table read. Empty = the hop never reads shares
	// (fail closed). Server-set by resolveReportJoin, never client input; sanitized again when armed.
	Entity string
	// Optional selects LEFT JOIN (true — keep base rows with no related match, group key NULL)
	// vs INNER JOIN (false). The related predicates go in the ON clause so a LEFT join stays
	// outer (a WHERE-clause tenant filter would collapse LEFT into INNER).
	Optional bool
	// Scope is the related entity's resolved A5 row-level scope for the caller (owner/team/all/
	// none). Its rowFilterFor(Alias) is the per-hop row-level predicate. Ignored when NoRowScope.
	Scope RowScope

	// Join condition: <Alias>.<RightCol> = <FromAlias-or-base>.<LeftCol>.
	//   FK-on-child, base is PARENT → LeftCol="id",      RightCol=<childFK>  (fan-out 1-N)
	//   FK-on-child, base is CHILD  → LeftCol=<childFK>, RightCol="id"
	//   N-N hop 1 (base→link)       → LeftCol="id",      RightCol=<base's link column>
	//   N-N hop 2 (link→far)        → LeftCol=<far's link column>, RightCol="id"
	LeftCol  string
	RightCol string

	// FromAlias overrides which table this hop's LeftCol is qualified against. Empty (v1
	// default) means the overall base alias — every existing 1-hop FK-on-child caller is
	// unaffected. REPORT-JOIN-NN's second hop sets this to the first hop's Alias so it chains
	// off the link table rather than the base.
	FromAlias string
	// NoSoftDelete skips the "<alias>.deleted_at IS NULL" predicate — the rel_<a>_<b> N-N link
	// table (ddl.BuildCreateJoinTable) carries no deleted_at column (unlink is a hard DELETE).
	NoSoftDelete bool
	// NoRowScope skips the A5 row-level predicate entirely — the link table carries no
	// created_by/team_set_id-based visibility concept; A5 for an N-N hop is enforced wholly at
	// the FAR entity (the second hop), never at the link table.
	NoRowScope bool
}

// joinSQL emits the JOIN clause + its args for this hop. defaultFromAlias is the base table's
// alias ("base"), used when js.FromAlias is empty; tenantID is bound as a placeholder (NEVER
// interpolated). Every identifier is re-sanitized here even though the service validated it
// (defense-in-depth).
func (js JoinSpec) joinSQL(defaultFromAlias, tenantID string) (string, []any, error) {
	fromAlias := js.FromAlias
	if fromAlias == "" {
		fromAlias = defaultFromAlias
	}

	alias, err := ddl.SanitizeIdentifier(js.Alias)
	if err != nil {
		return "", nil, fmt.Errorf("dynrepo: join alias %q: %w", js.Alias, err)
	}
	// COMPOSED, not a component: RelatedTable arrives as "obj_<entity>" or "rel_<a>_<b>", already
	// built from parts SanitizeIdentifier accepted (dynrepo.TableFor / ddl.JoinParts). The 26-char
	// component budget must not be charged twice — doing so rejected the shipped CRM pack's own
	// 34-char N-N link table and broke every N-N report join on it.
	relTable, err := ddl.SanitizeComposedIdentifier(js.RelatedTable)
	if err != nil {
		return "", nil, fmt.Errorf("dynrepo: join table %q: %w", js.RelatedTable, err)
	}
	leftCol, err := ddl.SanitizeIdentifier(js.LeftCol)
	if err != nil {
		return "", nil, fmt.Errorf("dynrepo: join left column %q: %w", js.LeftCol, err)
	}
	rightCol, err := ddl.SanitizeIdentifier(js.RightCol)
	if err != nil {
		return "", nil, fmt.Errorf("dynrepo: join right column %q: %w", js.RightCol, err)
	}
	baseA, err := ddl.SanitizeIdentifier(fromAlias)
	if err != nil {
		return "", nil, fmt.Errorf("dynrepo: from alias %q: %w", fromAlias, err)
	}

	joinKw := "INNER JOIN"
	if js.Optional {
		joinKw = "LEFT JOIN"
	}

	// All related-side predicates (join key, tenant, soft-delete, row-level ACL) live in the ON
	// clause so a LEFT JOIN stays outer.
	var scopeSQL string
	var scopeArgs []any
	if !js.NoRowScope {
		scopeSQL, scopeArgs = js.Scope.rowFilterFor(alias)
	}
	var b strings.Builder
	fmt.Fprintf(&b, " %s `%s` `%s` ON `%s`.`%s` = `%s`.`%s`",
		joinKw, relTable, alias, alias, rightCol, baseA, leftCol)
	fmt.Fprintf(&b, " AND `%s`.`tenant_id` = ?", alias)
	if !js.NoSoftDelete {
		fmt.Fprintf(&b, " AND `%s`.`deleted_at` IS NULL", alias)
	}
	b.WriteString(scopeSQL)

	args := make([]any, 0, 1+len(scopeArgs))
	args = append(args, tenantID)
	args = append(args, scopeArgs...)
	return b.String(), args, nil
}

// qualCol qualifies a bare column with a table alias: `alias`.`col`. alias="" returns `col`
// unqualified (single-table path). Used to qualify base tenant/deleted_at/group/metric columns
// once a JOIN makes them otherwise ambiguous.
func qualCol(alias, col string) string {
	if alias == "" {
		return "`" + col + "`"
	}
	return "`" + alias + "`.`" + col + "`"
}
