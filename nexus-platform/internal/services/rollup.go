package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// maxRollupChildren caps how many N-N-linked children a single rollup aggregate may cover via
// the id-set path (sum/avg/min/max). Mirrors dynrepo.maxAggregateGroups' hard-cap discipline:
// EXCEEDING the cap is an ERROR (RECORD_ROLLUP_TOO_MANY_CHILDREN, 422), never a silent
// truncate-and-return. A rollup that reports "sum over the first 5000 of 6000 children" as an
// authoritative total is the exact class of silent corruption the read-time/stored split exists
// to avoid (../specs/005-computed-lookup-rollup/spec.md §"DEFERRED") — a clear error is strictly better than
// a wrong number. FK-on-child rollup has no such cap: it is a single `<viaRelation>_id = ?`
// AggregateScalar call, correct at any child count.
const maxRollupChildren = 5000

// rollupLinkedPageSize is the page size used when paging through ListLinked to assemble the
// full N-N child id-set. 100 is dynrepo.ClampPageLimit's max page size.
const rollupLinkedPageSize = 100

// RollupLinkedLister returns the target ids linked to sourceID via an N-N relationship, scoped
// by the resolved row-level access on the TARGET (child) entity. Implemented by
// *dynrepo.Repository (Repository.ListLinked); declared here so rollup.go is unit-testable.
type RollupLinkedLister interface {
	ListLinked(ctx context.Context, tenantID string, sourceEntity, sourceID, targetEntity string, scope dynrepo.RowScope, page, limit int) ([]string, int, error)
}

// RollupResult is the resolved value of one rollup field, ready to be written into a record's
// Data map. Exactly one of the two "value" carriers is meaningful, selected by Count-only:
//   - metric == "count": IntValue carries the row count (always present, never nil).
//   - metric in {sum,avg,min,max}: DecimalValue carries MySQL's exact DECIMAL text (nil when
//     undefined over zero matching rows, mirroring dynrepo.AggregateScalar.Value), and — for a
//     currency aggField — CurrencyCode carries the child field's declared options.currencyCode
//     so the caller can shape {amount:"<exact string>",currencyCode}.
type RollupResult struct {
	Metric       string
	IntValue     *int64
	DecimalValue *string
	IsCurrency   bool
	CurrencyCode string
}

// ToJSONValue converts a resolved RollupResult into the JSON-ready value written to
// record.Data[fieldName]. count → a JSON number (float64, matching every other integer the
// engine returns as a JSON number). sum/avg/min/max on a non-currency aggField → the exact
// DECIMAL string is returned VERBATIM (never float64-parsed — mirrors the existing /aggregate
// endpoint's contract, api-conventions.md Pagination §sum precedent) so a client never sees
// float-drifted money; nil stays nil (no matching children). On a currency aggField →
// {amount:"<exact string>",currencyCode} (amount also a string, same no-drift rule) or nil.
func (r RollupResult) ToJSONValue() any {
	if r.Metric == "count" {
		if r.IntValue == nil {
			return int64(0)
		}
		return *r.IntValue
	}
	if r.DecimalValue == nil {
		return nil
	}
	if r.IsCurrency {
		return map[string]any{"amount": *r.DecimalValue, "currencyCode": r.CurrencyCode}
	}
	return *r.DecimalValue
}

// rollupPlan is the per-FIELD precomputed state for resolving one rollup field across many
// parent records: parsed options, the child entity's aggField spec, the caller's re-derived
// child row-scope, the field-ACL verdict, and — for the N-N mode — the resolved relationship's
// source side. prepareRollup builds it ONCE per rollup field; executeRollup then runs the
// per-record aggregate against it. Hoisting matters because the production rollupMeta is the
// UNCACHED *metadata.Repository (cmd/server/main.go): resolving child metadata inside the
// per-record loop was a metadata N+1 (a 100-row ?rollup= List cost 100× GetEntityByName +
// ListFields + GetRelationshipByName instead of 1×).
type rollupPlan struct {
	spec        FieldSpec
	childEntity string
	metric      string
	aggField    string
	aggSpec     *FieldSpec
	childScope  dynrepo.RowScope
	// zero short-circuits every record to zeroRollupResult: the caller is denied entity-level
	// view on the child, or denied field-level view on the aggregated child field (RM-42/A6 —
	// a SUM over a hidden column would disclose it; mirrors /aggregate's AssertFieldVisible).
	zero         bool
	aclDenied    bool // zero because the caller's ACL hides the child entity / aggregated field
	isNN         bool
	sourceEntity string // N-N only: the relationship side that is NOT the child entity
}

// resolveRollup resolves ONE rollup field for ONE parent record (ENG-COMPUTED #3) — the
// single-record convenience form of prepareRollup + executeRollup. applyRollups does NOT use
// it: it prepares each field's plan once and reuses it across the whole record page.
//
// spec must be the resolved FieldSpec for the rollup field (FieldType == "rollup"); parentID is
// the id of the record being read. scope is the CALLER's resolved RecordScope (used only to
// detect IsAdmin — the effective row-scope applied to the CHILD entity is re-derived from the
// child entity's OWN ACL grant, mirroring RelationService.ListLinked's NN-01 fix: a caller with
// view:all on the parent but view:owner on the child must not see every child's contribution).
func (s *RecordService) resolveRollup(ctx context.Context, scope RecordScope, tenantID string, spec FieldSpec, parentID string) (RollupResult, error) {
	plan, err := s.prepareRollup(ctx, scope, tenantID, spec)
	if err != nil {
		return RollupResult{}, err
	}
	return s.executeRollup(ctx, tenantID, plan, parentID)
}

// prepareRollup resolves everything about ONE rollup field that does not depend on which parent
// record is being read: option parsing, child entity + aggField metadata, the child row-scope
// (entity-level ACL), the field-level ACL verdict on the aggregated child field, and the N-N
// relationship's source side. Called once per rollup field per request.
func (s *RecordService) prepareRollup(ctx context.Context, scope RecordScope, tenantID string, spec FieldSpec) (rollupPlan, error) {
	if s.rollupMeta == nil {
		return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
			"rollup chưa được cấu hình trên server (thiếu metadata resolver)")
	}
	childEntity, _ := spec.Options["childEntity"].(string)
	childEntity = strings.TrimSpace(childEntity)
	metric, _ := spec.Options["metric"].(string)
	aggField, _ := spec.Options["aggField"].(string)
	aggField = strings.TrimSpace(aggField)
	plan := rollupPlan{spec: spec, childEntity: childEntity, metric: metric, aggField: aggField}

	// Resolve the child entity's aggField spec (needed for currency shaping + JSONSubPath).
	// Skipped for count, which aggregates no field.
	if metric != "count" {
		child, err := s.rollupMeta.GetEntityByName(ctx, tenantID, childEntity)
		if err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
					"entity con %q của rollup %q không còn tồn tại", childEntity, spec.Name)
			}
			return rollupPlan{}, fmt.Errorf("services.resolveRollup: resolve child entity: %w", err)
		}
		childFields, err := s.rollupMeta.ListFields(ctx, tenantID, child.ID)
		if err != nil {
			return rollupPlan{}, fmt.Errorf("services.resolveRollup: list child fields: %w", err)
		}
		childSpecs, err := toFieldSpecs(childFields)
		if err != nil {
			return rollupPlan{}, err
		}
		for i := range childSpecs {
			if childSpecs[i].Name == aggField {
				plan.aggSpec = &childSpecs[i]
				break
			}
		}
		if plan.aggSpec == nil {
			return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
				"field %q trên entity con %q của rollup %q không còn tồn tại", aggField, childEntity, spec.Name)
		}
	}

	// Resolve the child-entity row-scope (NN-01 mirror): admin sees every row in the tenant;
	// otherwise re-check the CALLER's ACL on the child entity (never reuse the parent's scope —
	// the two entities can carry different access_levels for the same caller).
	childScope, allowed, err := s.rollupChildScope(ctx, scope, tenantID, childEntity)
	if err != nil {
		return rollupPlan{}, err
	}
	if !allowed {
		// Caller cannot view the child entity at all → the rollup contributes nothing (mirrors
		// ListLinked's "denied target → empty result", not an error).
		plan.zero, plan.aclDenied = true, true
		return plan, nil
	}
	plan.childScope = childScope

	// Field-level ACL gate on the aggregated CHILD field (RM-42/A6): a role denied can_view on
	// payments.amount must not read SUM(payments.amount) through a parent's rollup — the metric
	// value discloses the hidden data (mirrors /aggregate's AssertFieldVisible rule). count
	// aggregates no field, so it is gated by the entity-level scope above only. Fail-closed:
	// a restriction-load error refuses the read (mirrors stripReadFields).
	if metric != "count" && s.fieldACL != nil && !scope.IsAdmin {
		childHidden, herr := s.fieldACL.HiddenFields(ctx, tenantID, scope.RoleID, childEntity, scope.IsAdmin)
		if herr != nil {
			return rollupPlan{}, fmt.Errorf("services.resolveRollup: field-acl on child %q: %w", childEntity, herr)
		}
		if _, denied := childHidden[aggField]; denied {
			plan.zero, plan.aclDenied = true, true
			return plan, nil
		}
	}

	if _, isNN := spec.Options["relationship"]; isNN {
		plan.isNN = true
		if s.rollupLinked == nil {
			return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
				"rollup N-N chưa được cấu hình trên server (thiếu linked-lister)")
		}
		relName, _ := spec.Options["relationship"].(string)
		relName = strings.TrimSpace(relName)
		rel, err := s.rollupMeta.GetRelationshipByName(ctx, tenantID, relName)
		if err != nil {
			if errors.Is(err, metadata.ErrRelationshipNotFound) {
				return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
					"quan hệ %q của rollup %q không còn tồn tại", relName, spec.Name)
			}
			return rollupPlan{}, fmt.Errorf("services.resolveRollup: resolve relationship: %w", err)
		}
		// ENG-REL2 #4 defense-in-depth: GetRelationshipByName was broadened (migration 000028)
		// to also resolve 1-N registry rows. Config-time validation (validateRollupCrossEntity)
		// already rejects a rollup whose options.relationship names a 1-N row, so this should be
		// unreachable in practice — but mirror the not-found message here too in case that guard
		// is ever bypassed (e.g. a relationship's type changes after the field was configured).
		if rel.RelationshipType != "N-N" {
			return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
				"quan hệ %q của rollup %q không còn tồn tại", relName, spec.Name)
		}
		sourceEntity, err := otherSideOf(rel, childEntity)
		if err != nil {
			return rollupPlan{}, invalid("RECORD_ROLLUP_UNAVAILABLE",
				"quan hệ %q của rollup %q không nối tới entity con %q", relName, spec.Name, childEntity)
		}
		plan.sourceEntity = sourceEntity
	}
	return plan, nil
}

// executeRollup runs the per-record half of a rollup resolution against a prepared plan.
func (s *RecordService) executeRollup(ctx context.Context, tenantID string, plan rollupPlan, parentID string) (RollupResult, error) {
	if plan.zero {
		return zeroRollupResult(plan.metric, plan.aggSpec), nil
	}
	if plan.isNN {
		return s.resolveRollupNN(ctx, tenantID, plan, parentID)
	}
	return s.resolveRollupFKChild(ctx, tenantID, plan, parentID)
}

// rollupChildScope derives the row-level scope applied to the CHILD entity for a rollup
// aggregate, mirroring RelationService.ListLinked's NN-01 fix exactly: admin → AccessAll;
// otherwise Authorize(childEntity,"view") resolves the level, and a denied caller yields
// allowed=false (no query is ever issued under the wrong scope). A nil s.authz (unit tests
// without ACL wiring) falls back to AccessAll so existing tests keep their prior meaning.
func (s *RecordService) rollupChildScope(ctx context.Context, scope RecordScope, tenantID, childEntity string) (dynrepo.RowScope, bool, error) {
	switch {
	case scope.IsAdmin:
		return dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessAll, IsAdmin: true}, true, nil
	case s.authz != nil:
		allowed, level, err := s.authz.Authorize(ctx, tenantID, scope.RoleID, childEntity, "view")
		if err != nil {
			return dynrepo.RowScope{}, false, fmt.Errorf("services.resolveRollup: authorize child %q: %w", childEntity, err)
		}
		if !allowed {
			return dynrepo.RowScope{}, false, nil
		}
		// IncludeShares is inherited: a rollup computed during a VIEW also counts child rows shared
		// with the caller (specs/051) — exactly the rows their list of that child entity shows.
		return dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessLevel(level), IncludeShares: scope.IncludeShares}, true, nil
	default:
		// nil authz (unit-test path without ACL wiring): preserve AccessAll so existing callers
		// that never wire an authorizer keep seeing every row in the tenant.
		return dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessAll}, true, nil
	}
}

// resolveRollupFKChild resolves a 1-N (FK-on-child) rollup: aggregate obj_<childEntity> WHERE
// `<viaRelation>_id` = parentID. Correct and O(1) at any child count — no id-set, no cap.
func (s *RecordService) resolveRollupFKChild(ctx context.Context, tenantID string, plan rollupPlan, parentID string) (RollupResult, error) {
	via, _ := plan.spec.Options["viaRelation"].(string)
	via = strings.TrimSpace(via)

	params := dynrepo.AggregateParams{
		Metric: dynrepo.AggregateMetric(plan.metric),
		Filters: []dynrepo.Filter{{
			Column:         via + "_id",
			PhysicalColumn: via + "_id",
			Operator:       dynrepo.FilterEq,
			Values:         []string{parentID},
		}},
	}
	if plan.metric != "count" {
		params.FieldColumn = plan.aggField
		if isCur, _ := rollupAggCurrency(plan.aggSpec); isCur {
			params.FieldSubPath = "amount"
		}
	}

	sc, err := s.store.AggregateScalar(ctx, plan.childEntity, tenantID, plan.childScope, params)
	if err != nil {
		return RollupResult{}, fmt.Errorf("services.resolveRollup: aggregate %q: %w", plan.childEntity, err)
	}
	return toRollupResult(plan.metric, plan.aggSpec, sc), nil
}

// resolveRollupNN resolves an N-N rollup via the join-table pivot id-set (RelationService's
// ListLinked). count uses ListLinked's scoped `total` directly (O(1), correct at any child
// count — never assembles an id-set). sum/avg/min/max page through ListLinked to assemble the
// FULL visible id-set (erroring — never truncating — past maxRollupChildren), then runs ONE
// AggregateScalar with an `id IN (...)` filter. Zero linked children short-circuits before
// AggregateScalar (an empty IN-list is a builder error, not a valid query — see buildPredicates).
// The relationship + source side were already resolved into the plan by prepareRollup (once per
// field, not per record).
func (s *RecordService) resolveRollupNN(ctx context.Context, tenantID string, plan rollupPlan, parentID string) (RollupResult, error) {
	if plan.metric == "count" {
		_, total, err := s.rollupLinked.ListLinked(ctx, tenantID, plan.sourceEntity, parentID, plan.childEntity, plan.childScope, 1, 1)
		if err != nil {
			return RollupResult{}, fmt.Errorf("services.resolveRollup: ListLinked count: %w", err)
		}
		n := int64(total)
		return RollupResult{Metric: plan.metric, IntValue: &n}, nil
	}

	// Page through ListLinked to assemble the FULL visible id-set. Error (never truncate) if it
	// exceeds maxRollupChildren.
	var ids []string
	page := 1
	for {
		pageIDs, total, err := s.rollupLinked.ListLinked(ctx, tenantID, plan.sourceEntity, parentID, plan.childEntity, plan.childScope, page, rollupLinkedPageSize)
		if err != nil {
			return RollupResult{}, fmt.Errorf("services.resolveRollup: ListLinked page %d: %w", page, err)
		}
		if total > maxRollupChildren {
			return RollupResult{}, invalid("RECORD_ROLLUP_TOO_MANY_CHILDREN",
				"rollup %q có %d bản ghi liên kết (N-N), vượt giới hạn %d — không thể tính chính xác qua id-set",
				plan.spec.Name, total, maxRollupChildren)
		}
		ids = append(ids, pageIDs...)
		if len(pageIDs) < rollupLinkedPageSize || len(ids) >= total {
			break
		}
		page++
	}

	if len(ids) == 0 {
		// Zero visible linked children: AggregateScalar's zero-rows contract is "Value=nil,
		// Count=0" (aggregate.go ~66-76) — reproduce that directly; an empty `id IN ()` is a
		// buildPredicates error, not a valid "no rows" query.
		return zeroRollupResult(plan.metric, plan.aggSpec), nil
	}

	params := dynrepo.AggregateParams{
		Metric: dynrepo.AggregateMetric(plan.metric),
		Filters: []dynrepo.Filter{{
			Column: "id",
			// PhysicalColumn is REQUIRED here: `id` is a physical system column, not a JSON
			// `data` key. Without it, buildPredicates' jsonPath falls back to
			// JSON_EXTRACT(`data`,'$.id') — a path that never exists in any record's JSON blob
			// — so the IN-list filter silently matches ZERO rows and every N-N sum/avg/min/max
			// rollup resolves to nil no matter how many children are linked (bug found by the
			// N-N sum integration test; the N-N count path is unaffected because it never
			// builds this filter — it reads ListLinked's `total` directly).
			PhysicalColumn: "id",
			Operator:       dynrepo.FilterIn,
			Values:         ids,
		}},
		FieldColumn: plan.aggField,
	}
	if isCur, _ := rollupAggCurrency(plan.aggSpec); isCur {
		params.FieldSubPath = "amount"
	}
	sc, err := s.store.AggregateScalar(ctx, plan.childEntity, tenantID, plan.childScope, params)
	if err != nil {
		return RollupResult{}, fmt.Errorf("services.resolveRollup: aggregate N-N %q: %w", plan.childEntity, err)
	}
	return toRollupResult(plan.metric, plan.aggSpec, sc), nil
}

// otherSideOf mirrors relation_service.go's otherSide, duplicated here (unexported, tiny) so
// rollup.go does not need a RelationMetaRepo-shaped dependency just for this one helper.
func otherSideOf(rel *metadata.RelationshipDefinition, oneSide string) (string, error) {
	if rel.ParentEntityName == oneSide {
		return rel.ChildEntityName, nil
	}
	if rel.ChildEntityName == oneSide {
		return rel.ParentEntityName, nil
	}
	return "", fmt.Errorf("services.otherSideOf: %q is neither side of relationship %q", oneSide, rel.Name)
}

// rollupAggCurrency reports whether a rollup's aggregated child field stores the currency shape
// {amount,currencyCode} — and so must be summed on its ".amount" sub-path and returned as money —
// plus the currency code to stamp on the result. Two field kinds store that shape: a currency
// field (its declared code, verbatim) and, since #262, a currency-resultType formula, which
// applyComputedFields persists in exactly that shape (code defaults to formulaDefaultCurrency,
// the same default applyComputedFields writes). The single source for every rollup path —
// FK/N-N aggregate sub-path and result shaping — so they cannot drift apart.
func rollupAggCurrency(aggSpec *FieldSpec) (bool, string) {
	if aggSpec == nil {
		return false, ""
	}
	code, _ := aggSpec.Options["currencyCode"].(string)
	switch aggSpec.FieldType {
	case "currency":
		return true, code
	case "formula":
		if rt, _ := aggSpec.Options["resultType"].(string); rt != "currency" {
			return false, ""
		}
		if code == "" {
			code = formulaDefaultCurrency
		}
		return true, code
	}
	return false, ""
}

// toRollupResult shapes a dynrepo.AggregateScalar into a RollupResult, attaching the currency
// code when the aggregated field stores money (rollupAggCurrency).
func toRollupResult(metric string, aggSpec *FieldSpec, sc dynrepo.AggregateScalar) RollupResult {
	if metric == "count" {
		n := sc.Count
		return RollupResult{Metric: metric, IntValue: &n}
	}
	res := RollupResult{Metric: metric, DecimalValue: sc.Value}
	res.IsCurrency, res.CurrencyCode = rollupAggCurrency(aggSpec)
	return res
}

// zeroRollupResult builds the RollupResult for "no children contribute" (ACL-denied child
// entity, or an N-N parent with zero visible linked children): count → 0; sum/avg/min/max →
// nil (undefined over zero rows — mirrors dynrepo.AggregateScalar's own zero-rows contract).
func zeroRollupResult(metric string, aggSpec *FieldSpec) RollupResult {
	if metric == "count" {
		var zero int64
		return RollupResult{Metric: metric, IntValue: &zero}
	}
	res := RollupResult{Metric: metric}
	res.IsCurrency, res.CurrencyCode = rollupAggCurrency(aggSpec)
	return res
}

// applyRollups resolves every field NAMED in rollupFields (the caller's explicit ?rollup=
// request) against every record, writing RollupResult.ToJSONValue() into record.Data[name].
// Unlike lookup (always-on) and unlike applyExpand's batched IN query, a rollup is inherently
// ONE AggregateScalar/ListLinked call PER (record, rollup field) pair — no batching primitive
// exists for "sum grouped by parent" without a GROUP BY rollup engine, which is out of scope for
// the read-time MVP (../specs/005-computed-lookup-rollup/spec.md §"#2/#3"). The explicit query-param gate is what keeps
// this from becoming an implicit N+1 on every plain List — the caller opts in per request.
// A field named in rollupFields that is not a declared `rollup` field on the entity is silently
// skipped (forward-compat, mirrors applyExpand's unknown-field handling).
//
// hidden is the caller's field-ACL can_view=0 set on the CURRENT entity (resolved by the caller
// exactly like stripReadFields; nil/empty for admin or when field-ACL is unwired): a requested
// rollup field in the set is never resolved nor written — computed writes run AFTER the read
// strip, so skipping here is what keeps the strip authoritative (RM-42/A6).
//
// Per-field metadata (child entity, aggField spec, child scope, N-N relationship) is prepared
// ONCE via prepareRollup and reused across every record — the per-record cost is the aggregate
// query only, never a metadata re-resolution (the production metaRepo is uncached).
func (s *RecordService) applyRollups(ctx context.Context, scope RecordScope, tenantID string, records []dynrepo.Record, specs []FieldSpec, rollupFields []string, hidden map[string]struct{}, denied map[string]struct{}) error {
	if len(rollupFields) == 0 || len(records) == 0 {
		return nil
	}
	rollupSpecs := make(map[string]FieldSpec)
	for _, sp := range specs {
		// A stored roll-up (specs/083) already carries its value in the record's data: resolving it
		// again per row would only recompute — and could overwrite — what the refresh wrote.
		if sp.FieldType == "rollup" && !isStoredRollup(sp) {
			rollupSpecs[sp.Name] = sp
		}
	}
	var requested []FieldSpec
	for _, name := range rollupFields {
		if _, isHidden := hidden[name]; isHidden {
			// can_view=0 on the rollup field itself: never resolved, never written (the
			// original A6 bypass re-added the value after the read strip).
			continue
		}
		if sp, ok := rollupSpecs[name]; ok {
			requested = append(requested, sp)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	plans := make([]rollupPlan, 0, len(requested))
	for _, sp := range requested {
		plan, err := s.prepareRollup(ctx, scope, tenantID, sp)
		if err != nil {
			return err
		}
		if plan.aclDenied {
			markDenied(denied, sp.Name)
		}
		plans = append(plans, plan)
	}
	for i := range records {
		for _, plan := range plans {
			res, err := s.executeRollup(ctx, tenantID, plan, records[i].ID)
			if err != nil {
				return err
			}
			records[i].Data[plan.spec.Name] = res.ToJSONValue()
		}
	}
	return nil
}
