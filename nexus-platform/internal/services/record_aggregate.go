package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"nexus/platform/internal/dynrepo"
)

// AggregateMetric mirrors dynrepo.AggregateMetric at the service boundary so the handler can
// build a request without importing dynrepo. The handler parses the ?metric= query into one
// of these; the service validates it.
type AggregateMetric string

const (
	AggMetricCount AggregateMetric = "count"
	AggMetricSum   AggregateMetric = "sum"
	AggMetricAvg   AggregateMetric = "avg"
	AggMetricMin   AggregateMetric = "min"
	AggMetricMax   AggregateMetric = "max"
)

// validAggMetrics is the set of accepted metric tokens. An unknown metric → 422.
var validAggMetrics = map[AggregateMetric]dynrepo.AggregateMetric{
	AggMetricCount: dynrepo.MetricCount,
	AggMetricSum:   dynrepo.MetricSum,
	AggMetricAvg:   dynrepo.MetricAvg,
	AggMetricMin:   dynrepo.MetricMin,
	AggMetricMax:   dynrepo.MetricMax,
}

// numericAggregatableTypes are the field types SUM/AVG/MIN/MAX may be computed on. currency is
// included via its ".amount" numeric sub-path; rating and number are plain numeric scalars.
// A formula counts as the type its result is STORED as (aggregateStorageType). A SUM/AVG/MIN/MAX
// on any other field type → 422 AGGREGATE_FIELD_NOT_NUMERIC. (date/datetime MIN/MAX is a
// meaningful report axis but is deferred — this milestone covers numeric metrics per the EPIC 6
// M1 spec.)
var numericAggregatableTypes = map[string]struct{}{
	"number": {}, "currency": {}, "rating": {},
}

// aggregateStorageType is the type whose stored JSON shape an aggregate reads for spec. A formula
// is computed AND stored at write time (applyComputedFields) exactly like the plain type of its
// resultType — number (the default) as a JSON number, currency as {amount, currencyCode} — so it
// aggregates as that type (specs/070 FR-001). A text formula returns "text"; rollup/lookup are
// computed at READ time (nothing stored to SUM in SQL) and keep their own type, so
// both stay refused — unless the roll-up is stored (options.stored, specs/083), which sums as the
// number/currency it holds; with AGGREGATE_FIELD_NOT_NUMERIC; so is a formula reading one of them
// (refused in resolveMetricField via derivedFormula).
func aggregateStorageType(spec FieldSpec) string {
	if isStoredRollup(spec) { // specs/083: kept in data as a plain number / currency
		return storedRollupValueType(spec)
	}
	if spec.FieldType != "formula" {
		return spec.FieldType
	}
	switch rt, _ := spec.Options["resultType"].(string); rt {
	case "", "number":
		return "number"
	case "currency":
		return "currency"
	default:
		return rt
	}
}

// validAggBuckets is the set of accepted time-series bucket tokens (W4). An unknown bucket →
// 422 AGGREGATE_BUCKET_INVALID. This is the SINGLE source of truth reused by the dashboard
// widget and report config validators (B6 parity: config-time and run-time accept the same set).
var validAggBuckets = map[string]dynrepo.AggregateBucket{
	"day":     dynrepo.BucketDay,
	"week":    dynrepo.BucketWeek,
	"month":   dynrepo.BucketMonth,
	"quarter": dynrepo.BucketQuarter,
	"year":    dynrepo.BucketYear,
}

// isBucketableGroupBy reports whether the groupBy field can carry a time bucket: the system
// timestamp columns created_at/updated_at, or a declared date/datetime field (whose D1 indexed
// generated column is DATETIME — DATE_FORMAT-able). `id` and every non-date field type are not.
func isBucketableGroupBy(groupBy string, byName map[string]FieldSpec) bool {
	if groupBy == "created_at" || groupBy == "updated_at" {
		return true
	}
	spec, ok := byName[groupBy]
	return ok && (spec.FieldType == "date" || spec.FieldType == "datetime")
}

// AggregateQuery is the resolved aggregate request the handler builds from the query string:
//   - Metric: sum|count|avg|min|max
//   - Field:  the field SUM/AVG/MIN/MAX operates on (ignored for count)
//   - GroupBy: optional field to GROUP BY (must resolve to an indexed physical column)
//   - Bucket:  optional day|week|month|quarter|year time bucket over a date/datetime GroupBy (W4)
//   - Filters: the same ?field[op]=value filters as a list query (reused FilterInput)
//   - Overlay: optional request-time created_at/created_by overlay (W4 dashboard/report runs)
type AggregateQuery struct {
	Metric  AggregateMetric
	Field   string
	GroupBy string
	Bucket  string
	Filters []FilterInput
	Overlay *RunOverlay
	// Joins (REPORT-JOIN v1: at most ONE, FK-on-child 1-1/1-N) makes the aggregate span a
	// related entity: GroupBy / Field may then be namespaced "<relationship>.<field>". Empty →
	// the single-table path, byte-identical to before. Filters stay BASE-only in v1.
	Joins []AggregateJoin
}

// AggregateJoin is one requested report-join hop, by relationship NAME (sys_relationships,
// tenant-scoped). Optional selects LEFT JOIN (keep base rows with no related match) vs INNER.
type AggregateJoin struct {
	Relationship string
	Optional     bool
}

// AggregateResult is the unified service return. When the request had no groupBy, Scalar is set
// (Groups nil, Grouped=false). With a groupBy, Groups is set (Scalar zero, Grouped=true). The
// handler branches on Grouped to choose the {data:{value,count}} vs {data:{groups:[…]}} shape.
type AggregateResult struct {
	Grouped bool
	Scalar  AggregateScalarResult
	Groups  []AggregateGroupResult
	// CurrencyCode is the ISO code of the aggregated field WHEN it is a currency field
	// (SUM/AVG/MIN/MAX over a currency), else empty. It lets the client format the value in the
	// field's own currency (e.g. "US$") instead of a hardcoded "VNĐ", and — because it is empty
	// for count and for number/rating metrics — signals "this is a plain number, do NOT tack a
	// currency onto it". It reflects the field's CONFIGURED currencyCode (sys_fields.options),
	// not any per-record value currency: currency values are stored {amount, currencyCode} per
	// row, but an aggregate sums the .amount and labels with the field's one configured code
	// (mixed-currency-per-record sums are meaningless and out of scope).
	CurrencyCode string
}

// AggregateScalarResult is a non-grouped aggregate's value+count. Value is nil when the metric
// is undefined over the matched set (SUM/AVG/MIN/MAX of zero rows → null); the handler emits
// JSON null. Count is always present (0 for an empty set).
type AggregateScalarResult struct {
	Value *string
	Count int64
}

// AggregateGroupResult is one group of a grouped aggregate: the group Key (null surfaced as
// nil), the metric Value (null when undefined), and the per-group Count. Label is the name a
// saved report shows for the key (specs/049 S6 — ReportService.labelAxes: picklist valueLabels,
// relation/user/owner names); nil when there is none, and ALWAYS nil from RecordService.Aggregate
// (dashboards, work queues and ⌘K read raw keys).
type AggregateGroupResult struct {
	Key   *string
	Value *string
	Count int64
	Label *string
}

// aggregatePlan is the physical execution plan resolved from an aggregate's config surface
// (metric/field/groupBy/bucket): the repo-level metric token, the aggregated field's physical
// column + currency sub-path, the groupBy's physical (indexed) column, and the time bucket.
type aggregatePlan struct {
	metric          dynrepo.AggregateMetric
	fieldColumn     string
	fieldSubPath    string
	groupByPhysical string
	bucket          dynrepo.AggregateBucket
	// currencyCode is the aggregated field's configured ISO currency, set ONLY when the metric
	// field is a currency type; empty for count and for number/rating fields.
	currencyCode string
	// groupByField is the BARE (entity-local) groupBy field name — identical to the trimmed
	// groupBy token on the single-table path, and the "<field>" part of a namespaced
	// "<rel>.<field>" token on the joined path. It is what the field-ACL check asserts.
	groupByField string
	// groupByAlias / fieldAlias (REPORT-JOIN) name the table each axis lives on: "" = base
	// entity (single-table path and un-prefixed tokens), reportJoinAlias = the joined entity.
	groupByAlias string
	fieldAlias   string
}

// resolveAggregatePlan validates and resolves the aggregate config surface against the entity's
// live field specs — the ONE deep-semantic validation body shared by the run-time Aggregate
// engine and the config-time report validator (ValidateReportConfig with liveFields — the AI
// Builder's propose_report tool, M8). Extracted VERBATIM from Aggregate so the codes and messages
// a propose surfaces at config time can never drift from what the run path enforces:
//   - metric ∈ count/sum/avg/min/max (AGGREGATE_INVALID_METRIC);
//   - SUM/AVG/MIN/MAX field: present (AGGREGATE_FIELD_REQUIRED), known (AGGREGATE_FIELD_UNKNOWN),
//     numeric (AGGREGATE_FIELD_NOT_NUMERIC); currency routes to its ".amount" numeric sub-path;
//   - groupBy resolves to an INDEXED physical column via the sort allowlist
//     (AGGREGATE_REQUIRES_INDEXED);
//   - bucket: token valid (AGGREGATE_BUCKET_INVALID), requires a groupBy
//     (AGGREGATE_BUCKET_REQUIRES_GROUPBY) that carries a date (AGGREGATE_BUCKET_FIELD_NOT_DATE).
func resolveAggregatePlan(metric AggregateMetric, field, groupBy, bucket string, specs []FieldSpec, byName map[string]FieldSpec) (aggregatePlan, error) {
	var plan aggregatePlan

	// Validate metric.
	repoMetric, err := resolveAggMetric(metric)
	if err != nil {
		return plan, err
	}
	plan.metric = repoMetric

	// Resolve the aggregated field for SUM/AVG/MIN/MAX (COUNT needs none).
	if repoMetric != dynrepo.MetricCount {
		col, subPath, ccy, ferr := resolveMetricField(metric, strings.TrimSpace(field), byName)
		if ferr != nil {
			return plan, ferr
		}
		plan.fieldColumn, plan.fieldSubPath, plan.currencyCode = col, subPath, ccy
	}

	// groupBy → indexed physical column + W4 bucket, through the ONE axis body the grid engine
	// also runs per level (resolveGroupAxis).
	ax, gerr := resolveGroupAxis(groupBy, bucket, specs, byName, nil)
	if gerr != nil {
		return plan, gerr
	}
	plan.groupByPhysical, plan.groupByField, plan.bucket = ax.physical, ax.field, ax.bucket
	return plan, nil
}

// groupAxis is one resolved GROUP BY axis: the single-axis aggregate has one (groupBy), a
// multi-level / matrix report (specs/049) has one per level.
type groupAxis struct {
	physical string                  // indexed physical column; "" when no token was given
	field    string                  // bare entity-local field name — what field-ACL asserts
	alias    string                  // "" = base entity, reportJoinAlias = the joined entity
	bucket   dynrepo.AggregateBucket // W4 time bucket ("" = none)
	byName   map[string]FieldSpec    // spec map of the entity the axis lives on (polymorphic guard)
}

// resolveGroupAxis resolves one group-by token (+ its optional time bucket) — extracted VERBATIM
// from resolveAggregatePlan / resolveAggregatePlanJoined (specs/049 S1) so every axis of every
// report shape runs the same body and the same codes/messages:
//   - join == nil: the token is a BASE field, never split (a dotted token is simply not in the
//     allowlist → AGGREGATE_REQUIRES_INDEXED, as before);
//   - join != nil: a "<rel>.<field>" token must name THE configured join
//     (REPORT_JOIN_ENTITY_MISMATCH) and resolves on the related entity's specs;
//   - the token must map to an INDEXED physical column via the sort allowlist (system cols +
//     unique + relation + D1-indexed fields) — AGGREGATE_REQUIRES_INDEXED otherwise;
//   - bucket: runs AFTER the indexed check so a non-indexed date field still surfaces
//     AGGREGATE_REQUIRES_INDEXED first (the bucket rides on the physical DATETIME column);
//     token valid, requires a group-by, which must carry a date (resolveAggBucket).
func resolveGroupAxis(token, bucket string, specs []FieldSpec, byName map[string]FieldSpec, join *resolvedReportJoin) (groupAxis, error) {
	ax := groupAxis{byName: byName}
	gb := strings.TrimSpace(token)
	if gb != "" {
		axisSpecs := specs
		if join != nil {
			if pre, bare, namespaced := splitJoinedToken(gb); namespaced {
				hop, ok := join.hopNamed(pre)
				if !ok {
					return groupAxis{}, invalid("REPORT_JOIN_ENTITY_MISMATCH",
						"groupBy %q tham chiếu %q — không khớp join %q", gb, pre, join.hopNames())
				}
				gb = bare
				axisSpecs, ax.byName = hop.specs, hop.byName
				ax.alias = hop.alias
			}
		}
		col, err := resolveGroupByIndexed(gb, axisSpecs)
		if err != nil {
			return groupAxis{}, err
		}
		ax.physical, ax.field = col, gb
	}
	rb, err := resolveAggBucket(bucket, gb, ax.byName)
	if err != nil {
		return groupAxis{}, err
	}
	ax.bucket = rb
	return ax, nil
}

// resolveAggMetric validates the metric token — extracted VERBATIM from resolveAggregatePlan
// (REPORT-JOIN) so the base path and the joined path share ONE body (codes/messages can never
// drift). Same pattern for the three helpers below.
func resolveAggMetric(metric AggregateMetric) (dynrepo.AggregateMetric, error) {
	repoMetric, ok := validAggMetrics[metric]
	if !ok {
		return "", invalid("AGGREGATE_INVALID_METRIC",
			"metric %q không hợp lệ (chấp nhận: count,sum,avg,min,max)", metric)
	}
	return repoMetric, nil
}

// resolveMetricField validates + resolves the SUM/AVG/MIN/MAX field against a field-spec map
// (the BASE entity's, or the JOINED entity's for a namespaced "<rel>.<field>" token). Returns
// the field's column, its currency sub-path ("amount" for currency, else ""), and the
// configured ISO currency code.
func resolveMetricField(metric AggregateMetric, fieldName string, byName map[string]FieldSpec) (col, subPath, currencyCode string, err error) {
	if fieldName == "" {
		return "", "", "", invalid("AGGREGATE_FIELD_REQUIRED",
			"metric %q cần tham số field để tính", metric)
	}
	spec, exists := byName[fieldName]
	if !exists {
		return "", "", "", invalid("AGGREGATE_FIELD_UNKNOWN",
			"không thể tính trên field %q (không có trong metadata)", fieldName)
	}
	// specs/080: a formula reading a lookup/rollup is computed at READ time — no stored column to SUM.
	if len(derivedFormula(spec, specsOf(byName))) > 0 {
		return "", "", "", invalid("AGGREGATE_FIELD_NOT_NUMERIC",
			"metric %q không tính được trên công thức %q: công thức đọc trường tra cứu/tổng hợp chỉ tính khi xem, không cộng gộp được",
			metric, fieldName)
	}
	storage := aggregateStorageType(spec)
	if _, numeric := numericAggregatableTypes[storage]; !numeric {
		return "", "", "", invalid("AGGREGATE_FIELD_NOT_NUMERIC",
			"metric %q chỉ dùng cho field số (number/currency/rating, hoặc công thức ra số/tiền); field %q có type %q",
			metric, fieldName, spec.FieldType)
	}
	col = spec.Name
	// Currency stores its numeric scalar at "$.field.amount" (mirrors the B1 filter path);
	// carry its configured ISO code so the client formats the aggregate in the field's own
	// currency instead of a hardcoded VNĐ (empty for number/rating → plain number). A currency
	// formula reports the code its rows actually carry (formulaCurrencyCode — the write path's).
	if storage == "currency" {
		subPath = "amount"
		if spec.FieldType == "formula" {
			currencyCode = formulaCurrencyCode(spec.Options)
		} else {
			currencyCode, _ = spec.Options["currencyCode"].(string)
		}
	}
	return col, subPath, currencyCode, nil
}

func specsOf(byName map[string]FieldSpec) []FieldSpec {
	out := make([]FieldSpec, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	return out
}

// resolveGroupByIndexed maps a groupBy field to its INDEXED physical column via the sort
// allowlist of the entity the field lives on (base or joined).
func resolveGroupByIndexed(gb string, specs []FieldSpec) (string, error) {
	allowlist := buildSortAllowlist(specs)
	col, inAllowlist := allowlist[gb]
	if !inAllowlist {
		return "", invalid("AGGREGATE_REQUIRES_INDEXED",
			"không thể group theo field %q (cần là field có index: unique/relation/indexed, hoặc cột hệ thống id/created_at/updated_at)", gb)
	}
	return col, nil
}

// resolveAggBucket validates the W4 bucket token and that the (bare) groupBy carries a date —
// byName must be the spec map of the entity the groupBy resolves on. Empty bucket → no-op.
func resolveAggBucket(bucket, groupBy string, byName map[string]FieldSpec) (dynrepo.AggregateBucket, error) {
	b := strings.ToLower(strings.TrimSpace(bucket))
	if b == "" {
		return "", nil
	}
	rb, ok := validAggBuckets[b]
	if !ok {
		return "", invalid("AGGREGATE_BUCKET_INVALID",
			"bucket %q không hợp lệ (chấp nhận: day,week,month,quarter,year)", bucket)
	}
	if groupBy == "" {
		return "", invalid("AGGREGATE_BUCKET_REQUIRES_GROUPBY",
			"bucket cần groupBy trỏ vào field ngày (date/datetime) hoặc cột hệ thống created_at/updated_at")
	}
	if !isBucketableGroupBy(groupBy, byName) {
		return "", invalid("AGGREGATE_BUCKET_FIELD_NOT_DATE",
			"không thể bucket theo field %q — chỉ field date/datetime hoặc cột hệ thống created_at/updated_at", groupBy)
	}
	return rb, nil
}

// Aggregate computes an aggregation (SUM/COUNT/AVG/MIN/MAX, optionally grouped) over an
// entity's records, scoped by tenant + A5 row-level access and narrowed by the same
// ?field[op]=value filters a list query accepts (EPIC 6 M1 — the P0 reporting gap).
//
// Validation (all → 422 with a machine code so the handler maps cleanly):
//   - metric must be one of count/sum/avg/min/max (AGGREGATE_INVALID_METRIC).
//   - SUM/AVG/MIN/MAX require a numeric field (number/currency/rating); a missing field →
//     AGGREGATE_FIELD_REQUIRED, an unknown field → AGGREGATE_FIELD_UNKNOWN, a non-numeric
//     field → AGGREGATE_FIELD_NOT_NUMERIC. COUNT ignores the field entirely.
//   - groupBy, when supplied, must resolve to an INDEXED physical column (system column,
//     unique/relation/D1-indexed field) — otherwise AGGREGATE_REQUIRES_INDEXED. This mirrors
//     the sort/cursor allowlist: grouping on a bare JSON field would force a full JSON-extract
//     scan with no index, which we refuse rather than silently run slowly.
//   - filters are validated exactly like List (field existence, operator legality, arity), and
//     routed to physical generated columns / currency sub-paths the same way.
//
// Tenant + row-scope isolation is identical to ListRecords: the aggregate counts only rows the
// caller may read, so a SUM never includes another team's / tenant's rows.
func (s *RecordService) Aggregate(ctx context.Context, scope RecordScope, tenantID, entityName string, q AggregateQuery) (AggregateResult, error) {
	pf, err := s.aggregatePreflight(ctx, scope, tenantID, entityName, q, []GridAxisInput{{Field: q.GroupBy, Bucket: q.Bucket}}, nil)
	if err != nil {
		return AggregateResult{}, err
	}
	ax := pf.axes[0]
	params := pf.params
	params.GroupByPhysical, params.GroupByBucket, params.GroupByAlias = ax.physical, ax.bucket, ax.alias

	if ax.physical == "" {
		sc, aerr := s.store.AggregateScalar(ctx, pf.entity, tenantID, scope.toRowScope(), params)
		if aerr != nil {
			return AggregateResult{}, aerr
		}
		return AggregateResult{Scalar: AggregateScalarResult{Value: sc.Value, Count: sc.Count}, CurrencyCode: pf.currencyCode}, nil
	}

	groups, gerr := s.store.AggregateGrouped(ctx, pf.entity, tenantID, scope.toRowScope(), params)
	if gerr != nil {
		return AggregateResult{}, gerr
	}
	out := make([]AggregateGroupResult, 0, len(groups))
	for _, g := range groups {
		out = append(out, AggregateGroupResult{Key: g.Key, Value: g.Value, Count: g.Count})
	}
	return AggregateResult{Grouped: true, Groups: out, CurrencyCode: pf.currencyCode}, nil
}

// GridAxisInput is one requested GROUP BY axis: a field token (a base field, or "<rel>.<field>"
// on the configured report join) plus an optional day|week|month|quarter|year time bucket.
type GridAxisInput struct {
	Field  string
	Bucket string
}

// aggregatePrepared is everything a store aggregate call needs, resolved AND authorized by
// aggregatePreflight: the canonical entity name, the metric/field/filter/join params (GroupBy*
// left unset — the caller maps its axes), the resolved axes in request order, and the metric
// field's currency code.
type aggregatePrepared struct {
	entity       string
	params       dynrepo.AggregateParams
	axes         []groupAxis
	currencyCode string
}

// aggregatePreflight is the ONE resolve + authorize body of every aggregate read: Aggregate runs
// it with one axis (its groupBy, possibly empty = scalar), AggregateGrid with the 1..3 axes of a
// grid (specs/049 S2a). A second copy of this ACL chain would drift at the next security fix, so
// both paths call this and only this. q.GroupBy / q.Bucket are NOT read — axes are. Order (every
// rejection happens before any data query):
//
//  1. resolve the entity (ErrEntityNotFound → 404);
//  2. REPORT-JOIN hop: resolve + entity-level "view" on the related entity (resolveReportJoin),
//     then the N-N fan-out gate (AGGREGATE_NN_FANOUT_UNSAFE);
//  3. metric + metric field, then each axis in order (resolveGroupAxis — indexed column, join
//     namespace, time bucket). axisNames non-nil (grid) tags an axis error with details.axis;
//     nil (Aggregate) leaves the error exactly as before;
//  4. filters: base-only on a joined report (REPORT_JOIN_FILTER_UNSUPPORTED), resolved like
//     List, then the W4 overlay;
//  5. field-ACL (RM-42): the metric field, EVERY axis on the entity that holds it (polymorphic
//     `<f>_type`/`<f>_id` mapped to the base field), every filter field → 403 FIELD_FORBIDDEN.
func (s *RecordService) aggregatePreflight(ctx context.Context, scope RecordScope, tenantID, entityName string, q AggregateQuery, axes []GridAxisInput, axisNames []string) (aggregatePrepared, error) {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return aggregatePrepared{}, err
	}

	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}

	// REPORT-JOIN v1: resolve + AUTHORIZE the (single) hop BEFORE plan resolution, so a
	// namespaced "<rel>.<field>" groupBy/metric token can resolve against the RELATED entity's
	// live specs. join == nil → the single-table path, byte-identical to before.
	var join *resolvedReportJoin
	if len(q.Joins) > 0 {
		var jerr error
		join, jerr = s.resolveReportJoin(ctx, scope, tenantID, e.Name, q.Joins)
		if jerr != nil {
			return aggregatePrepared{}, jerr
		}
		// REPORT-JOIN-NN fan-out guard: reject SUM/AVG over an N-N hop BEFORE any plan/SQL is
		// built — N-N is SYMMETRIC (either side can fan out), so this is UNCONDITIONAL regardless
		// of whether the aggregated field lives on the base or the far entity (see
		// validateNNFanoutSafety's doc for the full allow/reject matrix). Shares the SAME body the
		// config-time save check runs (ValidateReportJoinConfig), so a report that validated on
		// save can never fail this here.
		if ferr := validateNNFanoutSafety(join.isNN, q.Metric); ferr != nil {
			return aggregatePrepared{}, ferr
		}
	}

	// Validate + resolve metric/field through the ONE deep-semantic body shared with the
	// config-time report validator (resolveAggregatePlan — M8 parity, see its doc); the joined
	// variant routes a namespaced metric token to the related entity's specs. The axis is left
	// empty there (a no-op for resolveGroupAxis) and every axis is resolved below, in the order
	// the single-axis plan always ran them (metric → field → groupBy), so codes cannot drift.
	var plan aggregatePlan
	var perr error
	if join == nil {
		plan, perr = resolveAggregatePlan(q.Metric, q.Field, "", "", specs, byName)
	} else {
		plan, perr = resolveAggregatePlanJoined(q.Metric, q.Field, "", "", specs, byName, join)
	}
	if perr != nil {
		return aggregatePrepared{}, perr
	}
	resolvedAxes := make([]groupAxis, len(axes))
	for i, a := range axes {
		ax, aerr := resolveGroupAxis(a.Field, a.Bucket, specs, byName, join)
		if aerr != nil {
			return aggregatePrepared{}, tagAxis(aerr, axisNames, i)
		}
		resolvedAxes[i] = ax
	}

	// REPORT-JOIN v1: filters (and ?q search) stay BASE-only — a namespaced filter field is
	// rejected explicitly instead of surfacing a confusing LIST_UNKNOWN_FILTER.
	if join != nil {
		for _, fi := range q.Filters {
			if _, _, namespaced := splitJoinedToken(fi.Field); namespaced {
				return aggregatePrepared{}, invalid("REPORT_JOIN_FILTER_UNSUPPORTED",
					"lọc theo field liên quan (%q) chưa hỗ trợ ở v1 — filter chỉ áp trên entity gốc", fi.Field)
			}
		}
	}

	// Validate + resolve filters identically to List (RM-08/RM-14): field existence, operator
	// legality per type, arity, physical-column rewrite, currency sub-path, date canonicalization.
	//
	// Queue #317: the filter keys are resolved against the fields THIS caller may see, so a hidden
	// name is refused by the same branch (LIST_UNKNOWN_FILTER) as a name that does not exist —
	// the later AssertFieldsVisible would answer 403 FIELD_FORBIDDEN and confirm the field exists.
	// A load failure fails closed.
	hiddenForKeys, herr := s.computedHiddenFields(ctx, scope, tenantID, e.Name)
	if herr != nil {
		return aggregatePrepared{}, fmt.Errorf("services.aggregatePreflight: field-acl: %w", herr)
	}
	resolvedFilters, ferr := resolveFiltersForAggregate(visibleByName(byName, hiddenForKeys), q.Filters)
	if ferr != nil {
		return aggregatePrepared{}, ferr
	}
	// W4 run overlay: append the created_at/created_by system-column predicates AFTER the
	// declared-field filters (AND semantics). System columns exist on every obj_* table, so
	// the overlay composes with any entity; they are not sys_fields entries → no field-ACL
	// dimension (the declared-filter/metric/groupBy ACL checks below run unchanged).
	resolvedFilters = append(resolvedFilters, overlayFilters(q.Overlay)...)

	params := dynrepo.AggregateParams{
		Metric:       plan.metric,
		FieldColumn:  plan.fieldColumn,
		FieldSubPath: plan.fieldSubPath,
		Filters:      resolvedFilters,
		FieldAlias:   plan.fieldAlias,
	}
	if join != nil {
		params.Joins = join.joinSpecs
	}

	// RM-42 field-level security on aggregates: a non-admin role cannot SUM/AVG/MIN/MAX a field it
	// may not view, nor GROUP BY a field it may not view — the metric/group value would itself
	// disclose the hidden data. Reject with 403 FIELD_FORBIDDEN. COUNT (no fieldColumn) and an
	// ungrouped query (no groupBy) skip their respective checks. Admin bypasses inside the enforcer.
	// REPORT-JOIN: a JOINED groupBy/metric field is asserted against the RELATED entity's grants
	// (fail-closed — a hidden related field would otherwise leak through the bucket/metric);
	// plan.fieldColumn and every axis field are the BARE entity-local names.
	if s.fieldACL != nil {
		fieldEntity := e.Name
		if hop, ok := join.hopByAlias(plan.fieldAlias); ok {
			fieldEntity = hop.relatedEntity
		}
		if err := s.fieldACL.AssertFieldVisible(ctx, tenantID, scope.RoleID, fieldEntity, scope.IsAdmin, plan.fieldColumn); err != nil {
			return aggregatePrepared{}, err
		}
		// Every axis, on the entity that holds it. ACTIVITY-CORE #3a (security review HIGH): an
		// axis on a polymorphic `<f>_type`/`<f>_id` sub-column must assert visibility of the BASE
		// polymorphic field — else the grouped response discloses the hidden field's distinct
		// values directly. The guard maps through the axis's OWN spec map (the related entity's
		// for a joined axis, so its polymorphic fields are recognised).
		for _, ax := range resolvedAxes {
			groupEntity := e.Name
			if hop, ok := join.hopByAlias(ax.alias); ok {
				groupEntity = hop.relatedEntity
			}
			if err := s.fieldACL.AssertFieldVisible(ctx, tenantID, scope.RoleID, groupEntity, scope.IsAdmin,
				polymorphicGuardField(ax.byName, ax.field)); err != nil {
				return aggregatePrepared{}, err
			}
		}
		// Anti-oracle: also block FILTERING on a hidden field (a filtered COUNT reconstructs a
		// hidden value from match counts even when the metric field itself is visible).
		// ACTIVITY-CORE #3a: guardedFilterFields asserts the BASE polymorphic field for a
		// `<f>_type`/`<f>_id` filter, so a hidden polymorphic field cannot be probed via its
		// sub-columns — the SAME helper List and ExportCSV call.
		filterFields := guardedFilterFields(byName, q.Filters)
		if err := s.fieldACL.AssertFieldsVisible(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin, filterFields); err != nil {
			return aggregatePrepared{}, err
		}
	}

	return aggregatePrepared{entity: e.Name, params: params, axes: resolvedAxes, currencyCode: plan.currencyCode}, nil
}

// tagAxis names the grid axis a resolution error belongs to (details.axis = "groupBy" |
// "subGroups[i]" | "columnGroup", spec FR-005), so a caller can point at the broken axis without
// the message embedding a config token. No names (the single-axis Aggregate) → err unchanged.
func tagAxis(err error, axisNames []string, i int) error {
	var ve *ValidationError
	if i < len(axisNames) && errors.As(err, &ve) {
		if ve.Details == nil {
			ve.Details = map[string]any{}
		}
		ve.Details["axis"] = axisNames[i]
	}
	return err
}

// resolveFiltersForAggregate validates and resolves the aggregate's filters against the entity
// field specs, producing dynrepo.Filter values. It is the SAME validation/resolution the List
// path applies (validateFilterOperator + physical-column rewrite + currency sub-path + date
// canonicalization + CAST-drop for indexed typed columns), factored so both paths stay in sync.
// Package-level (no receiver — it never touched one) so the config-time report validator
// (ValidateReportConfig, M8) can run the SAME filter-validation body the aggregate/export run
// paths execute.
func resolveFiltersForAggregate(byName map[string]FieldSpec, filters []FilterInput) ([]dynrepo.Filter, error) {
	var resolved []dynrepo.Filter
	for _, fi := range filters {
		spec, ok := byName[fi.Field]
		if !ok {
			// ACTIVITY-CORE #3a: accept a `<poly>_type` / `<poly>_id` key for a real
			// polymorphic_relation field, routed to its physical gen-column (parity with the List
			// path). Any other unknown key stays LIST_UNKNOWN_FILTER — single-table path unchanged.
			if _, isSub := polymorphicSubField(byName, fi.Field); isSub {
				spec = polymorphicSubSpec(fi.Field)
			} else if _, isOwner := ownerReservedKeys[fi.Field]; isOwner {
				// OWNER-DIMENSION parity. The List path admits the owner key (record_orchestration.go)
				// and buildSortAllowlist — which resolveGroupByIndexed reuses — already accepts owner
				// as a groupBy. Without this branch the two halves of ONE screen disagree: a
				// dashboard could GROUP BY owner and then 422 the moment the user filtered by the
				// same owner, or a drill-down from that chart would fail on the filter it inherited.
				// Same spec, same normalization, from the same helpers — not a second copy of the
				// rule, because two copies of a rule are two things that drift.
				spec = ownerFilterSpec()
				fi.Field = ownerPhysicalColumn
			} else {
				return nil, invalid("LIST_UNKNOWN_FILTER",
					"không thể lọc theo field %q (không có trong metadata)", fi.Field)
			}
		}
		op := fi.Operator
		if op == "" {
			op = dynrepo.FilterEq
		}
		// Wave 3.6: same relative-token rewrite + loud-garbage contract as the list path — a
		// dashboard/report aggregate must resolve "today" identically to the list it drills into.
		fiValues := fi.Values
		if spec.FieldType == "date" || spec.FieldType == "datetime" {
			var derr error
			op, fiValues, derr = resolveDateFilters(spec.FieldType, op, fiValues, time.Now())
			if derr != nil {
				return nil, derr
			}
		}

		flags, jsonSubPath, verr := validateFilterOperator(spec, op, fiValues)
		if verr != nil {
			return nil, verr
		}

		physicalCol := ""
		indexedTyped := false
		if spec.IsUnique {
			physicalCol = spec.Name
		} else if spec.FieldType == "relation" || spec.FieldType == "user" {
			// USER-FIELD: reuses relation's <field>_id physical column (buildSortAllowlist).
			physicalCol = spec.Name + "_id"
		} else if spec.IsIndexed {
			physicalCol = spec.Name
			indexedTyped = true
		}

		ci := flags.CaseInsensitive
		if spec.FieldType == "relation" || spec.FieldType == "user" {
			ci = false
		}

		castNumeric := flags.CastNumeric
		castDatetime := flags.CastDatetime
		if indexedTyped {
			castNumeric = false
			castDatetime = false
		}

		filterValues := fiValues
		if spec.FieldType == "datetime" || spec.FieldType == "date" {
			filterValues = canonicalizeFilterValues(spec.FieldType, fiValues)
		}

		resolved = append(resolved, dynrepo.Filter{
			Column:          fi.Field,
			Operator:        op,
			Values:          filterValues,
			Value:           firstOrEmpty(filterValues),
			CastNumeric:     castNumeric,
			CastDatetime:    castDatetime,
			CaseInsensitive: ci,
			JSONSubPath:     jsonSubPath,
			PhysicalColumn:  physicalCol,
		})
	}
	return resolved, nil
}
