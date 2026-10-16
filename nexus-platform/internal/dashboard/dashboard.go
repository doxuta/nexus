// Package dashboard is the EPIC 6 Dashboard M2 storage layer. It persists dashboard definitions
// as rows in sys_dashboards — never materialized data. Each dashboard holds an ordered array of
// widget configs in a JSON column; the run endpoint executes each widget THROUGH the existing
// aggregate engine (RecordService.Aggregate), so a dashboard always reflects live, tenant + A5
// row-scoped records.
//
// Multi-tenant invariant: every repository query filters tenant_id.
package dashboard
