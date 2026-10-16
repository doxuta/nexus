package dynrepo

import (
	"context"
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
)

// AggregateMetric is the aggregation function applied by an aggregate query (EPIC 6 M1).
// COUNT(*) counts rows; SUM/AVG/MIN/MAX operate on a single numeric field. The zero value
// is invalid — the service validates the metric before it reaches the repo.
type AggregateMetric string

const (
	// MetricCount is COUNT(*) over the filtered, scoped row set. No field is required.
	MetricCount AggregateMetric = "count"
	// MetricSum / MetricAvg / MetricMin / MetricMax operate on a numeric field, which is
	// CAST to DECIMAL(38,10) so currency/number values aggregate exactly (no float drift).
	MetricSum AggregateMetric = "sum"
	MetricAvg AggregateMetric = "avg"
	MetricMin AggregateMetric = "min"
	MetricMax AggregateMetric = "max"
)

// AggregateBucket is the calendar bucket a grouped aggregate collapses its date/datetime
// GROUP BY column into (W4 time-series bucketing). The service validates the token AND that
// the group-by column is a date/datetime physical column (or created_at/updated_at) before
// it reaches the repo; the repo maps it to a fixed SQL expression — the bucket is NEVER
// client-interpolated text.
type AggregateBucket string

const (
	// BucketDay groups by calendar day — key "2026-01-31".
	BucketDay AggregateBucket = "day"
	// BucketWeek groups by ISO-8601 week (Monday-first) — key "2026-W05" (%x-W%v pairs the
	// ISO year with the ISO week so a January 1st falling in week 53 keys correctly).
	BucketWeek AggregateBucket = "week"
	// BucketMonth groups by calendar month — key "2026-01".
	BucketMonth AggregateBucket = "month"
	// BucketQuarter groups by calendar quarter — key "2026-Q1".
	BucketQuarter AggregateBucket = "quarter"
	// BucketYear groups by calendar year — key "2026".
	BucketYear AggregateBucket = "year"
)

// bucketKeyExpr returns the SQL expression producing the canonical bucket key text over the
// SANITIZED physical column col. Every format string is a compile-time literal; col is the
// only interpolated part and has already passed ddl.SanitizeIdentifier (defense-in-depth in
// AggregateGrouped). All key formats are zero-padded so lexicographic ORDER BY agg_key ASC
// is chronological order.
func bucketKeyExpr(bucket AggregateBucket, col string) (string, error) {
	return bucketKeyExprAliased(bucket, "", col)
}

// bucketKeyExprAliased is bucketKeyExpr with the physical column qualified by a table alias
// (REPORT-JOIN). alias="" reproduces bucketKeyExpr (unqualified) for the single-table path.
func bucketKeyExprAliased(bucket AggregateBucket, alias, col string) (string, error) {
	q := qualCol(alias, col)
	switch bucket {
	case BucketDay:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m-%%d')", q), nil
	case BucketWeek:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%x-W%%v')", q), nil
	case BucketMonth:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y-%%m')", q), nil
	case BucketQuarter:
		return fmt.Sprintf("CONCAT(YEAR(%s), '-Q', QUARTER(%s))", q, q), nil
	case BucketYear:
		return fmt.Sprintf("DATE_FORMAT(%s, '%%Y')", q), nil
	default:
		return "", fmt.Errorf("dynrepo: unknown aggregate bucket %q", bucket)
	}
}

// AggregateParams is the resolved input for an aggregate query. Every identifier
// (FieldColumn, FieldSubPath, GroupByPhysical) is a sanitized, service-validated value —
// no raw client input is ever interpolated. Filters/Search reuse the existing predicate
// builder (buildPredicates) so the aggregate is narrowed by the SAME ?field[op]=value
// filters as a list query. Values always travel as `?` placeholders.
type AggregateParams struct {
	// Metric selects the aggregation function (count/sum/avg/min/max).
	Metric AggregateMetric

	// FieldColumn is the sanitized field name aggregated by SUM/AVG/MIN/MAX. It is empty
	// for COUNT(*). The repo reads it from the JSON path (or FieldSubPath for currency).
	FieldColumn string
	// FieldSubPath, when non-empty, targets a nested JSON key under the field — set to
	// "amount" for currency fields whose value is stored as {"amount":N,...}. It is a
	// service-supplied literal (never client input).
	FieldSubPath string

	// GroupByPhysical, when non-empty, is the PHYSICAL column to GROUP BY (a generated /
	// system column with an index — resolved by the service from the sort allowlist). An
	// empty value yields a single scalar aggregate over the whole scoped set.
	GroupByPhysical string

	// Joins, when non-empty (REPORT-JOIN v1: exactly one FK-on-child hop), makes the aggregate
	// span a related entity. The base table is aliased "base" and each hop's related table by
	// its JoinSpec.Alias; every base column (tenant/deleted_at/scope/filter/group/metric) is
	// then alias-qualified to stay unambiguous. Empty → the single-table path (unqualified,
	// zero-regression). Each hop carries its own tenant + row-level ACL predicate (see JoinSpec).
	Joins []JoinSpec
	// GroupByAlias / FieldAlias name which table the GROUP BY column / metric field belong to
	// when Joins is set ("base" for a base field, a hop's alias for a joined field). Ignored
	// (treated as "") when Joins is empty.
	GroupByAlias string
	FieldAlias   string

	// GroupByBucket, when non-empty, collapses GroupByPhysical (a DATETIME/TIMESTAMP
	// physical column — validated by the service) into calendar buckets (W4 time-series):
	// day|week|month|quarter|year. The group key becomes the canonical bucket text
	// ("2026-01", "2026-Q1", …) and the groups are returned in CHRONOLOGICAL order
	// (agg_key ASC) instead of the default value-DESC "top N" order. Empty buckets (no
	// matching rows) are OMITTED — the caller zero-fills if it needs a dense series.
	GroupByBucket AggregateBucket

	// Filters / Search reuse the list-query predicate builder so an aggregate honours the
	// same ?field[op]=value filters and ?q search as ListRecords.
	Filters []Filter
	Search  *Search

	// GroupLimit caps the number of groups returned (GROUP BY ... LIMIT n) so a high-card
	// group-by can never stream unbounded rows. ≤0 falls back to a safe default.
	GroupLimit int
}

// maxAggregateGroups is the hard cap on groups a single aggregate query returns. A group-by
// on a high-cardinality column (e.g. a free-text field accidentally indexed) could otherwise
// produce thousands of rows; the cap keeps the response bounded. The service may request a
// smaller limit but never a larger one.
const maxAggregateGroups = 1000

// AggregateScalar is the result of a non-grouped aggregate: the metric Value plus the row
// Count over the scoped/filtered set. Value is nil when the metric is undefined for the set
// (e.g. AVG/MIN/MAX/SUM over zero matching rows → SQL NULL); Count is always present.
//
// Value is returned as a *string carrying MySQL's exact DECIMAL text (e.g. "1234.5600") so a
// currency/number sum is never round-tripped through float64 (the float-drift bug ENGINE-
// COVERAGE-SPEC §EPIC6 calls out). The handler forwards it verbatim; a client parses it as a
// decimal. COUNT always yields a concrete integer string ("0" for an empty set).
type AggregateScalar struct {
	Value *string
	Count int64
}

// AggregateGroup is one row of a grouped aggregate: the group Key (the GROUP BY column's
// value, NULL surfaced as a nil *string), the metric Value (nil when undefined for the group),
// and the per-group row Count.
type AggregateGroup struct {
	Key   *string
	Value *string
	Count int64
}

// metricSQLFunc maps a metric to its SQL function name. COUNT is handled separately (it uses
// COUNT(*), not COUNT(<expr>)), so it is absent here.
var metricSQLFunc = map[AggregateMetric]string{
	MetricSum: "SUM",
	MetricAvg: "AVG",
	MetricMin: "MIN",
	MetricMax: "MAX",
}

// aggregateValueExpr builds the SQL expression for the metric's aggregated value.
//   - COUNT          → "COUNT(*)"
//   - SUM/AVG/MIN/MAX → "FUNC(CAST(NULLIF(<jsonpath>, 'null') AS DECIMAL(38,10)))"
//
// The CAST to DECIMAL(38,10) is the float-drift guard (mirrors the W5-D D1 filter CAST and the
// genCol DECIMAL scale): currency/number values are stored as JSON numbers, so without the CAST
// MySQL would aggregate them as DOUBLE and accumulate rounding error on money. col/subPath are
// sanitized service literals (jsonPath re-validates nothing raw is interpolated).
func aggregateValueExpr(metric AggregateMetric, col, subPath string) (string, error) {
	return aggregateValueExprAliased(metric, "", col, subPath)
}

// aggregateValueExprAliased is aggregateValueExpr with the metric field qualified by a table
// alias (REPORT-JOIN — the metric field may live on the base or a joined entity). alias=""
// reproduces the single-table expression. COUNT(*) has no column, so alias is irrelevant there.
func aggregateValueExprAliased(metric AggregateMetric, alias, col, subPath string) (string, error) {
	if metric == MetricCount {
		return "COUNT(*)", nil
	}
	fn, ok := metricSQLFunc[metric]
	if !ok {
		return "", fmt.Errorf("dynrepo: unknown aggregate metric %q", metric)
	}
	// Aggregated field reads the JSON path (or the currency sub-path), CAST to DECIMAL so the
	// arithmetic is exact. jsonPath here is always the JSON_EXTRACT form (physicalCol left "")
	// because a generated column for a numeric field is itself DECIMAL — but reading the JSON
	// path keeps the metric independent of whether the field happens to be indexed, and the
	// CAST normalises both. The service passes the sanitized col/subPath.
	// NULLIF: a number cleared to JSON null unquotes to the string 'null', and CAST('null' AS
	// DECIMAL) is 0 — which would drag AVG/MIN/MAX down (#448). SQL NULL is skipped by aggregates.
	valExpr := jsonPathAliased(alias, "", col, subPath)
	return fmt.Sprintf("%s(CAST(NULLIF(%s, 'null') AS DECIMAL(38,10)))", fn, valExpr), nil
}

// AggregateScalar runs a non-grouped aggregate (no GROUP BY) and returns the metric value plus
// the matched row count, scoped by tenant + A5 row-level access + the supplied filters/search.
// It mirrors ListRecords' WHERE construction exactly (tenant_id leads, then scope, then
// filter/search predicates) so the aggregate covers precisely the rows the caller may read.
//
// The query is `SELECT <metricExpr> AS agg_value, COUNT(*) AS agg_count FROM <table> WHERE …`.
// agg_value is scanned as a nullable string (MySQL DECIMAL text or NULL); agg_count as int64.
func (r *Repository) AggregateScalar(ctx context.Context, entity, tenantID string, scope RowScope, p AggregateParams) (AggregateScalar, error) {
	table, err := tableFor(entity)
	if err != nil {
		return AggregateScalar{}, fmt.Errorf("dynrepo.AggregateScalar: %w", err)
	}
	valExpr, err := r.aggregateValueExprSanitized(p)
	if err != nil {
		return AggregateScalar{}, fmt.Errorf("dynrepo.AggregateScalar: %w", err)
	}

	fromJoin, where, args, err := r.aggregateFromWhereScoped(ctx, entity, table, tenantID, scope, p)
	if err != nil {
		return AggregateScalar{}, fmt.Errorf("dynrepo.AggregateScalar: %w", err)
	}

	stmt := fmt.Sprintf(
		"SELECT %s AS `agg_value`, COUNT(*) AS `agg_count` FROM %s WHERE %s",
		valExpr, fromJoin, where)

	var row struct {
		Value *string `db:"agg_value"`
		Count int64   `db:"agg_count"`
	}
	if err := r.db.GetContext(ctx, &row, stmt, args...); err != nil {
		return AggregateScalar{}, fmt.Errorf("dynrepo.AggregateScalar: %w", err)
	}
	return AggregateScalar{Value: row.Value, Count: row.Count}, nil
}

// AggregateGrouped runs a grouped aggregate (GROUP BY <physical column>) and returns up to
// GroupLimit groups, each with its key, metric value and per-group count. Scoping and
// predicate construction match AggregateScalar / ListRecords (tenant → scope → filter/search).
//
// The GROUP BY targets a PHYSICAL column (a generated/system column with an index) so the
// grouping uses the B-tree rather than a JSON-extraction expression that the optimizer cannot
// index. The service guarantees GroupByPhysical names such a column (AGGREGATE_REQUIRES_INDEXED
// otherwise); the repo re-sanitizes it as defense-in-depth.
//
// ORDER BY: groups are returned ordered by the aggregated value DESC (largest first — the
// natural "top N" report ordering), with the key ASC as a deterministic tie-break so paging /
// snapshots are stable. The LIMIT bounds the number of groups (high-cardinality guard).
func (r *Repository) AggregateGrouped(ctx context.Context, entity, tenantID string, scope RowScope, p AggregateParams) ([]AggregateGroup, error) {
	table, err := tableFor(entity)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.AggregateGrouped: %w", err)
	}
	groupCol, err := ddl.SanitizeIdentifier(p.GroupByPhysical)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.AggregateGrouped: group column %q: %w", p.GroupByPhysical, err)
	}
	groupAlias, err := sanitizeAlias(p.aliasFor(p.GroupByAlias))
	if err != nil {
		return nil, fmt.Errorf("dynrepo.AggregateGrouped: group alias %q: %w", p.GroupByAlias, err)
	}
	valExpr, err := r.aggregateValueExprSanitized(p)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.AggregateGrouped: %w", err)
	}

	limit := p.GroupLimit
	if limit <= 0 || limit > maxAggregateGroups {
		limit = maxAggregateGroups
	}

	// CAST(`group` AS CHAR) → return the group key as MySQL-native text regardless of the
	// generated column's physical type (DECIMAL/DATETIME/VARCHAR), so the handler forwards a
	// uniform string key. A NULL group value stays NULL → scanned as a nil *string.
	//
	// W4 bucketing: when GroupByBucket is set, the key/group expression becomes the bucket
	// DATE_FORMAT over the physical column and the ORDER BY flips to agg_key ASC — a time
	// series reads chronologically, not by value. A NULL date value buckets to a NULL key
	// (sorted first by MySQL ASC). Empty buckets are omitted (no zero-fill server-side).
	keyExpr := fmt.Sprintf("CAST(%s AS CHAR)", qualCol(groupAlias, groupCol))
	groupExpr := qualCol(groupAlias, groupCol)
	orderBy := "`agg_value` DESC, `agg_key` ASC"
	if p.GroupByBucket != "" {
		keyExpr, err = bucketKeyExprAliased(p.GroupByBucket, groupAlias, groupCol)
		if err != nil {
			return nil, fmt.Errorf("dynrepo.AggregateGrouped: %w", err)
		}
		groupExpr = keyExpr
		orderBy = "`agg_key` ASC"
	}
	fromJoin, where, args, err := r.aggregateFromWhereScoped(ctx, entity, table, tenantID, scope, p)
	if err != nil {
		return nil, fmt.Errorf("dynrepo.AggregateGrouped: %w", err)
	}
	stmt := fmt.Sprintf(
		"SELECT %s AS `agg_key`, %s AS `agg_value`, COUNT(*) AS `agg_count` "+
			"FROM %s WHERE %s "+
			"GROUP BY %s ORDER BY %s LIMIT ?",
		keyExpr, valExpr, fromJoin, where, groupExpr, orderBy)
	args = append(args, limit)

	var rows []struct {
		Key   *string `db:"agg_key"`
		Value *string `db:"agg_value"`
		Count int64   `db:"agg_count"`
	}
	if err := r.db.SelectContext(ctx, &rows, stmt, args...); err != nil {
		return nil, fmt.Errorf("dynrepo.AggregateGrouped: %w", err)
	}
	out := make([]AggregateGroup, 0, len(rows))
	for _, rrow := range rows {
		out = append(out, AggregateGroup{Key: rrow.Key, Value: rrow.Value, Count: rrow.Count})
	}
	return out, nil
}

// aggregateValueExprSanitized re-sanitizes the aggregated field name (defense-in-depth, the
// service already validated it against sys_fields) and builds the metric value expression.
// COUNT needs no field; SUM/AVG/MIN/MAX require a non-empty FieldColumn.
func (r *Repository) aggregateValueExprSanitized(p AggregateParams) (string, error) {
	if p.Metric == MetricCount {
		return aggregateValueExpr(MetricCount, "", "")
	}
	if strings.TrimSpace(p.FieldColumn) == "" {
		return "", fmt.Errorf("dynrepo: metric %q requires a field", p.Metric)
	}
	col, err := ddl.SanitizeIdentifier(p.FieldColumn)
	if err != nil {
		return "", fmt.Errorf("dynrepo: aggregate field %q: %w", p.FieldColumn, err)
	}
	fieldAlias, err := sanitizeAlias(p.aliasFor(p.FieldAlias))
	if err != nil {
		return "", fmt.Errorf("dynrepo: aggregate field alias %q: %w", p.FieldAlias, err)
	}
	// FieldSubPath is a service literal ("amount" for currency) — pass through jsonPath which
	// only interpolates validated literals.
	return aggregateValueExprAliased(p.Metric, fieldAlias, col, p.FieldSubPath)
}

// baseAlias is "base" when the aggregate spans a JOIN (so base columns are unambiguous) and ""
// for the single-table path (unqualified, zero-regression).
func (p AggregateParams) baseAlias() string {
	if len(p.Joins) > 0 {
		return "base"
	}
	return ""
}

// aliasFor resolves the effective table alias for a group/metric column. When there is no JOIN,
// columns are unqualified (""). When there is, a caller-supplied alias ("base" or a hop alias)
// is used; an empty caller alias defaults to the base alias so a base field stays qualified.
func (p AggregateParams) aliasFor(want string) string {
	if len(p.Joins) == 0 {
		return ""
	}
	if strings.TrimSpace(want) == "" {
		return "base"
	}
	return want
}

// sanitizeAlias validates a table alias identifier (defense-in-depth); "" passes through as the
// unqualified single-table case.
func sanitizeAlias(alias string) (string, error) {
	if alias == "" {
		return "", nil
	}
	return ddl.SanitizeIdentifier(alias)
}

// aggregateFromWhere builds the shared FROM(+JOIN) and WHERE fragments (and their args, in
// statement order) for both AggregateScalar and AggregateGrouped. It is the single place the
// JOIN path and the single-table path converge, so the tenant + A5 scope predicates are emitted
// identically for both. Arg order MUST match placeholder order in the final statement:
//
//	FROM <base> <JOIN…?…>   →  join args first (JOIN text precedes WHERE)
//	WHERE base.tenant_id=?  →  base tenant
//	<base scope args> <base filter/search args>
//
// The caller appends any trailing LIMIT arg after this.
//
// v1: report filters/search apply to the BASE entity only (base alias), each JOIN hop carries
// its own tenant + row-level ACL predicate inside its ON clause (JoinSpec.joinSQL).
func aggregateFromWhere(table, tenantID string, scope RowScope, p AggregateParams) (fromJoin, where string, args []any, err error) {
	baseA := p.baseAlias()
	if baseA == "" {
		fromJoin = "`" + table + "`"
	} else {
		fromJoin = "`" + table + "` `" + baseA + "`"
	}
	// JOIN clauses first — their placeholders precede the WHERE placeholders in the statement.
	for _, js := range p.Joins {
		jsql, jargs, jerr := js.joinSQL(baseA, tenantID)
		if jerr != nil {
			return "", "", nil, jerr
		}
		fromJoin += jsql
		args = append(args, jargs...)
	}
	// Base WHERE: tenant + soft-delete + row-level scope + (base-only) filters/search.
	predSQL, predArgs, perr := buildPredicatesAliased(ListParams{Filters: p.Filters, Search: p.Search}, baseA)
	if perr != nil {
		return "", "", nil, perr
	}
	scopeSQL, scopeArgs := scope.rowFilterFor(baseA)
	where = fmt.Sprintf("%s = ? AND %s IS NULL%s%s",
		qualCol(baseA, "tenant_id"), qualCol(baseA, "deleted_at"), scopeSQL, predSQL)
	args = append(args, tenantID)
	args = append(args, scopeArgs...)
	args = append(args, predArgs...)
	return fromJoin, where, args, nil
}
