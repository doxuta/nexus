// Package report is the EPIC 6 M2 saved-report layer (spec: docs/ENGINE-COVERAGE-SPEC.md §EPIC 6;
// benchmark: Salesforce summary/matrix reports + Metabase saved questions). It persists a report
// DEFINITION as a row in sys_reports — never materialized data — so a report always reflects live,
// tenant + A5 row-scoped records when it is RUN.
//
// A report is "chạy <type> trên <entity_name> với <config>". The run endpoint executes the saved
// config THROUGH the existing aggregate engine (summary/matrix → grouped aggregate) or list engine
// (tabular → filtered/sorted page); this package owns only the storage + the typed config the
// service validates against live entity metadata.
//
// Invariants (rules + spec):
//   - Multi-tenant: every repository query filters tenant_id — a report can never be read, run, or
//     mutated across tenants.
//   - Metadata-driven: reports are rows in sys_reports, never hardcoded Go structs; their config
//     references fields by name and is validated at RUN time against the entity's live metadata.
package report

// Type is the report shape. The values match the sys_reports.type ENUM.
type Type string

const (
	// TypeTabular is a flat, columnar report: a filtered/sorted page of records projected to a
	// chosen column set (executed through the list engine).
	TypeTabular Type = "tabular"
	// TypeSummary is a grouped aggregate (group-by one field, one metric over an optional field):
	// the Salesforce "summary report" / a Metabase grouped question. With config.subGroups (specs/049
	// S3, ≤ 2 further levels) it is a tree with sub-totals and a grand total.
	TypeSummary Type = "summary"
	// TypeMatrix is a cross-tab when its config carries a column axis (config.columnGroup — specs/049
	// S2b: rows = groupBy, columns = columnGroup, cells + row/column/grand totals). Without one —
	// every matrix saved before S2b — it runs as the single-axis grouped aggregate it always was.
	TypeMatrix Type = "matrix"
)

// ValidType reports whether t is one of the three supported report shapes.
func ValidType(t Type) bool {
	switch t {
	case TypeTabular, TypeSummary, TypeMatrix:
		return true
	default:
		return false
	}
}
