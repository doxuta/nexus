package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nexus/platform/internal/ddl"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// REPORT-JOIN (../specs/025-report-join/spec.md, extended by REPORT-JOIN-NN) — service-side resolution of
// the ONE cross-object report hop. The dynrepo layer owns the SQL (per-hop tenant filter + A5
// row-level predicate in each JOIN's ON clause — dynrepo.JoinSpec); this file resolves the
// relationship, the hop direction (FK-on-child: 1 hop; N-N: 2 chained hops through the
// rel_<a>_<b> link table), and — FAIL-CLOSED — the caller's entity-level + row-level +
// field-level access to the related (far) entity, BEFORE any plan is built (ACL trước business
// logic).

// reportJoinAlias is the server-generated table alias for the (far) related entity hop. NEVER
// client input; dynrepo re-sanitizes it on emit (defense-in-depth).
const reportJoinAlias = "j0"

// reportJoinLinkAlias is the server-generated table alias for an N-N join's FIRST hop (base →
// rel_<a>_<b> link table). NEVER client input.
const reportJoinLinkAlias = "lt"

// maxReportJoinHops is how many chained relationships a report may span (queue #293: Hoá đơn →
// Lịch hẹn → Bác sĩ). Hop k+1 starts from the entity hop k reached.
const maxReportJoinHops = 2

// hopAlias / hopLinkAlias are the server-generated aliases of hop k's far table and (N-N only) its
// link table. Hop 0 keeps the historical "j0"/"lt" so a one-hop report is byte-identical.
func hopAlias(k int) string { return fmt.Sprintf("j%d", k) }
func hopLinkAlias(k int) string {
	if k == 0 {
		return reportJoinLinkAlias
	}
	return fmt.Sprintf("%s%d", reportJoinLinkAlias, k)
}

// resolvedHop is one authorized hop: the relationship name (the "<rel>.<field>" namespace), the
// far entity it reaches, that entity's live field specs and the alias its columns are qualified with.
type resolvedHop struct {
	name          string
	relatedEntity string // canonical far entity name (field-ACL asserts against it)
	alias         string // hopAlias(k)
	specs         []FieldSpec
	byName        map[string]FieldSpec
}

// resolvedReportJoin is the fully authorized chain: one resolvedHop per requested join plus the
// ready dynrepo.JoinSpec list carrying every far entity's tenant + A5 predicate. joinSpecs has one
// element per FK-on-child hop and two per N-N hop (base→link, link→far) — see JoinSpec's doc.
type resolvedReportJoin struct {
	hops      []resolvedHop
	joinSpecs []dynrepo.JoinSpec
	isNN      bool // true when ANY hop crosses an N-N relationship (REPORT-JOIN-NN)
}

// hopNamed finds the hop a "<rel>." token prefix names.
func (j *resolvedReportJoin) hopNamed(name string) (*resolvedHop, bool) {
	for i := range j.hops {
		if j.hops[i].name == name {
			return &j.hops[i], true
		}
	}
	return nil, false
}

// hopByAlias finds the hop whose far table carries alias ("" / "base" are the base entity → false).
func (j *resolvedReportJoin) hopByAlias(alias string) (*resolvedHop, bool) {
	if j == nil {
		return nil, false
	}
	for i := range j.hops {
		if j.hops[i].alias == alias {
			return &j.hops[i], true
		}
	}
	return nil, false
}

// hopNames lists the configured relationship names for an error message.
func (j *resolvedReportJoin) hopNames() string {
	n := make([]string, len(j.hops))
	for i, h := range j.hops {
		n[i] = h.name
	}
	return strings.Join(n, ", ")
}

// splitJoinedToken splits a "<rel>.<field>" reference at the FIRST dot. ok=false when the token
// carries no namespace (a plain base field — backward compatible).
func splitJoinedToken(token string) (prefix, bare string, ok bool) {
	i := strings.Index(token, ".")
	if i < 0 {
		return "", token, false
	}
	return token[:i], token[i+1:], true
}

// resolveReportJoin resolves + authorizes the single REPORT-JOIN hop (FK-on-child: 1 hop; N-N:
// 2 chained hops) for an aggregate on baseEntity. Order (all fail-closed):
//  1. exactly one join (REPORT_JOIN_TOO_MANY);
//  2. relationship exists in THIS tenant (REPORT_JOIN_RELATION_UNKNOWN — tenant-scoped lookup);
//  3. baseEntity is one edge of the relationship → hop direction (REPORT_JOIN_ENTITY_MISMATCH);
//  4. caller holds entity-level "view" on the related (far) entity via the SAME authorizer
//     ?expand uses (REPORT_JOIN_FORBIDDEN, 403); the resolved access_level becomes the hop's A5
//     RowScope, exactly how the base scope is built (admin → AccessAll, mirrors ListLinked).
//     For N-N this ACL gate applies ONLY to the far entity — the rel_<a>_<b> link table carries
//     no entity-level ACL concept of its own (dynrepo.JoinSpec.NoRowScope on that hop).
func (s *RecordService) resolveReportJoin(ctx context.Context, scope RecordScope, tenantID, baseEntity string, joins []AggregateJoin) (*resolvedReportJoin, error) {
	if len(joins) == 0 {
		// Defensive (S3): the sole caller guards len>0, but never index joins[0] unguarded.
		return nil, fmt.Errorf("services.RecordService.resolveReportJoin: no join to resolve")
	}
	if err := checkJoinChainShape(joins); err != nil {
		return nil, err
	}
	// Chain: hop k starts from the entity hop k-1 reached, and its ON clause chains off that hop's
	// far alias ("" = the base). EVERY hop is authorized on its own far entity — a hidden middle
	// table is refused exactly like a hidden last one.
	out := &resolvedReportJoin{}
	from, fromAlias := baseEntity, ""
	for k, jn := range joins {
		hop, specs, isNN, err := s.resolveOneHop(ctx, scope, tenantID, from, fromAlias, k, jn)
		if err != nil {
			return nil, err
		}
		out.hops = append(out.hops, hop)
		out.joinSpecs = append(out.joinSpecs, specs...)
		out.isNN = out.isNN || isNN
		from, fromAlias = hop.relatedEntity, hop.alias
	}
	return out, nil
}

// checkJoinChainShape is the registry-free shape check of a join list — ONE body for the run path
// (resolveReportJoin), the config-time check (ValidateReportJoinConfig) and the report/widget
// normalizers: 1..maxReportJoinHops hops, each with a relationship name, no relationship twice
// (the name is the "<rel>.<field>" namespace, so a repeat would make a token ambiguous).
func checkJoinChainShape(joins []AggregateJoin) error {
	if len(joins) > maxReportJoinHops {
		return invalid("REPORT_JOIN_TOO_MANY",
			"report chỉ ghép được tối đa %d bước quan hệ (nhận %d)", maxReportJoinHops, len(joins))
	}
	seen := make(map[string]bool, len(joins))
	for _, j := range joins {
		name := strings.TrimSpace(j.Relationship)
		if name == "" {
			return invalid("REPORT_JOIN_RELATION_UNKNOWN", "joins[].relationship là bắt buộc")
		}
		if seen[name] {
			return invalid("REPORT_JOIN_ENTITY_MISMATCH",
				"quan hệ %q xuất hiện hai lần — mỗi quan hệ chỉ ghép một lần", name)
		}
		seen[name] = true
	}
	return nil
}

// resolveOneHop resolves + authorizes hop k of the chain, starting from baseEntity (here: the entity
// the previous hop reached, whose table is aliased fromAlias; "" = the base, which joinSQL defaults to). Returns the hop and its JoinSpecs
// (one for FK-on-child, two for N-N).
func (s *RecordService) resolveOneHop(ctx context.Context, scope RecordScope, tenantID, baseEntity, fromAlias string, k int, jn AggregateJoin) (resolvedHop, []dynrepo.JoinSpec, bool, error) {
	relName := strings.TrimSpace(jn.Relationship)
	if relName == "" {
		return resolvedHop{}, nil, false, invalid("REPORT_JOIN_RELATION_UNKNOWN", "joins[].relationship là bắt buộc")
	}
	if s.rollupMeta == nil {
		// Fail closed: without the relationship resolver wired (WithRollupMeta) the hop cannot
		// be verified — never guess a join. Production always wires *metadata.Repository.
		return resolvedHop{}, nil, false, fmt.Errorf("services.RecordService.resolveReportJoin: relationship resolver not wired (WithRollupMeta)")
	}
	rel, err := s.rollupMeta.GetRelationshipByName(ctx, tenantID, relName)
	if err != nil {
		if errors.Is(err, metadata.ErrRelationshipNotFound) {
			return resolvedHop{}, nil, false, invalid("REPORT_JOIN_RELATION_UNKNOWN",
				"relationship %q không tồn tại trong tenant", relName)
		}
		return resolvedHop{}, nil, false, fmt.Errorf("services.RecordService.resolveReportJoin: %w", err)
	}
	// Direction resolution (FK-on-child vs N-N) shares ONE body with the config-time validator
	// (ValidateReportJoinConfig) so a report that VALIDATES on save cannot then fail this check at
	// run time — the "save-then-always-die" gap. This resolves only registry facts (no roleID), so
	// both paths can run it.
	relatedEntity, leftCol, rightCol, isNN, derr := resolveJoinEdge(rel, baseEntity, relName)
	if derr != nil {
		return resolvedHop{}, nil, false, derr
	}

	// Resolve the related (far) entity + its live field specs the SAME way the base resolved
	// (ErrEntityNotFound → 404, no existence leak).
	relE, relSpecs, err := s.resolve(ctx, tenantID, relatedEntity)
	if err != nil {
		return resolvedHop{}, nil, false, err
	}

	// Per-hop authorization (fail-closed, spec §2.2): the caller must hold entity-level "view"
	// on the related (far) entity — the SAME authorizer ?expand / ListLinked use. Precedence
	// mirrors ListLinked's target-scope derivation: admin bypasses to AccessAll (exactly what
	// RecordScope.toRowScope does via RowScope.IsAdmin); denied → 403 (the spec chooses an
	// explicit REPORT_JOIN_FORBIDDEN over an empty result — a report silently missing a hop
	// would be a wrong number, not a smaller list); nil authz (unit tests without ACL wiring)
	// falls back to the caller's own scope, the codebase-wide nil-authz convention.
	var relScope dynrepo.RowScope
	switch {
	case scope.IsAdmin:
		relScope = dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessAll, IsAdmin: true}
	case s.authz != nil:
		allowed, level, aerr := s.authz.Authorize(ctx, tenantID, scope.RoleID, relE.Name, "view")
		if aerr != nil {
			return resolvedHop{}, nil, false, fmt.Errorf("services.RecordService.resolveReportJoin: authorize %q: %w", relE.Name, aerr)
		}
		if !allowed {
			return resolvedHop{}, nil, false, invalid("REPORT_JOIN_FORBIDDEN",
				"bạn không có quyền xem entity %q — không thể chạy report join qua nó", relE.Name)
		}
		// IncludeShares is inherited from the request scope (view-resolved callers only) and, together
		// with JoinSpec.Entity below, lets the hop reach rows manually shared with the caller (#225).
		relScope = dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessLevel(level), IncludeShares: scope.IncludeShares}
	default:
		// FAIL-CLOSED (security review): unlike ?expand/ListLinked (which fall back to the base
		// scope when no authorizer is wired), a report JOIN surfaces a RELATED entity's rows into
		// the result — inheriting the base access level here would widen it (base "all" over a
		// related grant of "owner"/"none"). Production always wires WithACLAuthorizer, so this
		// path is only reachable in a misconfigured build or a unit test that forgot the fake
		// authorizer; either way, seeing NOTHING is the safe default for a cross-object hop.
		relScope = dynrepo.RowScope{UserID: scope.UserID, Level: dynrepo.AccessNone}
	}

	relTable, err := dynrepo.TableFor(relE.Name)
	if err != nil {
		return resolvedHop{}, nil, false, fmt.Errorf("services.RecordService.resolveReportJoin: %w", err)
	}
	byName := make(map[string]FieldSpec, len(relSpecs))
	for _, f := range relSpecs {
		byName[f.Name] = f
	}

	var joinSpecs []dynrepo.JoinSpec
	if isNN {
		// N-N: TWO chained hops. Hop 1 (base → rel_<a>_<b> link table) carries ONLY the
		// mandatory tenant filter (RM-28-shaped concern closed HERE, in the join predicate, not
		// merely in application code) — the link table has no deleted_at/team_set_id, so
		// NoSoftDelete+NoRowScope skip those predicates entirely (dynrepo.JoinSpec doc). Hop 2
		// (link → far entity) carries the SAME tenant + A5 row-ACL predicate the FK-on-child hop
		// emits (rowFilterFor), chained off hop 1's alias via FromAlias.
		linkTable, colA, colB, jerr := ddl.JoinParts(baseEntity, relatedEntity)
		if jerr != nil {
			return resolvedHop{}, nil, false, fmt.Errorf("services.RecordService.resolveReportJoin: %w", jerr)
		}
		baseCol, farCol := colA, colB
		if colA != baseEntity+"_id" {
			baseCol, farCol = colB, colA
		}
		joinSpecs = []dynrepo.JoinSpec{
			{
				Alias: hopLinkAlias(k), RelatedTable: linkTable, FromAlias: fromAlias,
				LeftCol: "id", RightCol: baseCol,
				NoSoftDelete: true, NoRowScope: true,
			},
			{
				Alias: hopAlias(k), RelatedTable: relTable, Entity: relE.Name, FromAlias: hopLinkAlias(k),
				Optional: jn.Optional, Scope: relScope,
				LeftCol: farCol, RightCol: "id",
			},
		}
	} else {
		joinSpecs = []dynrepo.JoinSpec{{
			Alias:        hopAlias(k),
			FromAlias:    fromAlias,
			RelatedTable: relTable,
			Entity:       relE.Name,
			Optional:     jn.Optional,
			Scope:        relScope,
			LeftCol:      leftCol,
			RightCol:     rightCol,
		}}
	}

	return resolvedHop{
		name: relName, relatedEntity: relE.Name, alias: hopAlias(k), specs: relSpecs, byName: byName,
	}, joinSpecs, isNN, nil
}

// resolveJoinEdge dispatches to the FK-on-child or N-N edge resolver based on the relationship's
// registered type/JoinTableName. Shared by BOTH resolveReportJoin (run-time) and
// ValidateReportJoinConfig (config-time save) so a report that validates on save cannot fail
// these same registry checks at run (the "save-then-always-die" gap this codebase's REPORT-JOIN
// pattern always closes). leftCol/rightCol are empty (unused) for the N-N branch — REPORT-JOIN-NN
// derives its two hop columns separately (ddl.JoinParts, which needs BOTH entity names).
func resolveJoinEdge(rel *metadata.RelationshipDefinition, baseEntity, relName string) (relatedEntity, leftCol, rightCol string, isNN bool, err error) {
	if rel.RelationshipType == "N-N" || rel.JoinTableName != nil {
		re, derr := resolveNNJoinEdge(rel, baseEntity, relName)
		return re, "", "", true, derr
	}
	re, lc, rc, derr := resolveJoinDirection(rel, baseEntity, relName)
	return re, lc, rc, false, derr
}

// resolveNNJoinEdge resolves the FAR entity of an N-N relationship edge for baseEntity. N-N rows
// store the two entities in ParentEntityName/ChildEntityName without directional meaning (see
// dynrepo.LinkRecords / ddl.JoinParts — the physical link table sorts them alphabetically,
// independent of registration order), so either matching side yields the other as "far".
func resolveNNJoinEdge(rel *metadata.RelationshipDefinition, baseEntity, relName string) (relatedEntity string, err error) {
	switch baseEntity {
	case rel.ParentEntityName:
		return rel.ChildEntityName, nil
	case rel.ChildEntityName:
		return rel.ParentEntityName, nil
	default:
		return "", invalid("REPORT_JOIN_ENTITY_MISMATCH",
			"entity %q không thuộc quan hệ %q (%s ↔ %s)",
			baseEntity, relName, rel.ParentEntityName, rel.ChildEntityName)
	}
}

// resolveJoinDirection resolves a report join's related entity + FK-on-child join columns from a
// relationship row and the report's base entity. It rejects N-N (dispatched to resolveNNJoinEdge
// by resolveJoinEdge instead — this defensive guard here should never actually trigger via the
// dispatcher, kept as defense-in-depth for any direct caller) and a base entity that is not an
// edge of the relationship. It touches ONLY registry facts (no roleID / no field specs), so BOTH
// the run path (via resolveJoinEdge) and the config-time check (ValidateReportJoinConfig) share
// it — a report that validates on save cannot then fail these same checks at run time (the
// reviewer's "save-then-always-die" gap).
//
// Direction (FK-on-child): base is the CHILD (holds the FK) → hop to the PARENT (j0.id = base.fk);
// base is the PARENT → fan-out 1-N to the CHILD (j0.fk = base.id). A self-referential relation
// (parent==child) matches the CHILD edge first, so only the follow-the-FK direction is reachable —
// harmless, but noted so a builder of a self-relation ("parent_task") isn't surprised.
func resolveJoinDirection(rel *metadata.RelationshipDefinition, baseEntity, relName string) (relatedEntity, leftCol, rightCol string, err error) {
	if rel.RelationshipType == "N-N" || rel.JoinTableName != nil {
		return "", "", "", invalid("REPORT_JOIN_NN_UNSUPPORTED",
			"quan hệ %q là N-N — hàm này chỉ xử lý FK-on-child (dùng resolveJoinEdge để dispatch đúng)", relName)
	}
	fkCol := strings.TrimSpace(rel.ChildFKColumn)
	if fkCol == "" {
		// Defensive: every 1-1/1-N registry row carries child_fk_column (migration 000028); a row
		// without one cannot emit a join key — fail closed instead of a broken ON clause.
		return "", "", "", invalid("REPORT_JOIN_RELATION_UNKNOWN",
			"quan hệ %q không có cột FK đăng ký — không join được", relName)
	}
	switch baseEntity {
	case rel.ChildEntityName:
		return rel.ParentEntityName, fkCol, "id", nil
	case rel.ParentEntityName:
		return rel.ChildEntityName, "id", fkCol, nil
	default:
		return "", "", "", invalid("REPORT_JOIN_ENTITY_MISMATCH",
			"entity %q không thuộc quan hệ %q (%s ↔ %s)",
			baseEntity, relName, rel.ParentEntityName, rel.ChildEntityName)
	}
}

// validateNNFanoutSafety rejects a metric that would silently double-count across an N-N hop's
// fan-out. Package-level (no receiver) so BOTH the config-time save check
// (ValidateReportJoinConfig) and the run-time Aggregate engine share ONE body — a report that
// validates on save can never silently compute a wrong number at run (mirrors
// resolveJoinDirection's shared-body rationale).
//
// N-N is SYMMETRIC — unlike a 1-N FK-on-child hop, EITHER side can fan out: one base row can link
// to many far rows, AND one far row can link to many base rows (that is the defining shape of a
// many-to-many relation). So SUM/AVG double-counts regardless of which side the aggregated field
// lives on: a base field repeats once per linked far row; a far field repeats once per linked
// base row. Concretely — one far row (score=10) linked to 2 base rows joins into 2 (base,far)
// pairs, so SUM(far.score) would read 20, not the true value 10. There is no "safe side" to
// exempt (an earlier version of this gate allowed sum/avg of a namespaced far-entity field — that
// was WRONG under N-N symmetry; it is correct ONLY for the asymmetric FK-on-child 1-N case, which
// never reaches this function since validateNNFanoutSafety no-ops when !isNN).
//
// A SUM(DISTINCT ...) is NOT a fix: it dedups by VALUE, so two distinct rows on the far/base side
// that happen to share a numeric value would wrongly collapse into one. The only correct rule is
// an unconditional reject for sum/avg over any N-N hop.
//
// Final allow/reject matrix (REPORT-JOIN-NN):
//   - count       → ALLOW always. An N-N hop's COUNT(*) counts (base,far) PAIRS — i.e. "how many
//     linked rows per group". This is the SAME fan-out-row-count semantic the existing FK-on-child
//     v1 join already has for a parent→child (1-N) hop (see TestReportJoin_DirectionAndLeftInner:
//     COUNT already counts JOINED rows, not base rows). N-N introduces no NEW ambiguity here.
//   - sum/avg     → REJECT, 422 AGGREGATE_NN_FANOUT_UNSAFE, UNCONDITIONALLY (base field OR
//     "<rel>.<field>" far field) — both sides of an N-N hop can fan out, so there is no field
//     placement that is safe. Mirrors AGGREGATE_REQUIRES_INDEXED: refuse rather than silently
//     compute garbage.
//   - min/max     → ALLOW (not gated here), regardless of field side. MIN/MAX are non-additive —
//     a duplicated value never changes the extremum, so fan-out (from either side) cannot distort
//     the result.
//   - groupBy     → always ALLOWED — partitioning is unaffected by fan-out.
func validateNNFanoutSafety(isNN bool, metric AggregateMetric) error {
	if !isNN || (metric != AggMetricSum && metric != AggMetricAvg) {
		return nil
	}
	return invalid("AGGREGATE_NN_FANOUT_UNSAFE",
		"metric %q qua quan hệ N-N sẽ đếm trùng (fan-out đối xứng — cả 2 phía đều có thể nhân bản) — chỉ count/min/max được hỗ trợ trên join N-N",
		metric)
}

// ValidateReportJoinConfig runs the registry-only report-join checks that DON'T need the caller's
// roleID (relationship exists in the tenant, direction/edge resolves — FK-on-child or N-N — and
// the base entity is one of its edges), PLUS the N-N fan-out safety check (which only needs the
// resolved relationship type, not the caller's roleID), so ReportService can reject a bad join at
// SAVE time — matching what run time enforces via resolveReportJoin + Aggregate. It is a no-op
// when no relationship resolver is wired (mirrors the optional entity-existence checker); run
// time re-validates regardless. Per-hop ACL + field existence stay at run time (they need the
// caller's roleID / live field specs). metric is the report config's raw (trimmed by the caller)
// metric token — required to evaluate the fan-out rule (which, for N-N, depends ONLY on the
// metric, not on which side the aggregated field lives — see validateNNFanoutSafety's doc).
func (s *RecordService) ValidateReportJoinConfig(ctx context.Context, tenantID, baseEntity string, joins []AggregateJoin, metric AggregateMetric) error {
	if len(joins) == 0 || s.rollupMeta == nil {
		return nil
	}
	if err := checkJoinChainShape(joins); err != nil {
		return err
	}
	from, anyNN := strings.TrimSpace(baseEntity), false
	for _, jn := range joins {
		relName := strings.TrimSpace(jn.Relationship)
		rel, err := s.rollupMeta.GetRelationshipByName(ctx, tenantID, relName)
		if err != nil {
			if errors.Is(err, metadata.ErrRelationshipNotFound) {
				return invalid("REPORT_JOIN_RELATION_UNKNOWN",
					"relationship %q không tồn tại trong tenant", relName)
			}
			return fmt.Errorf("services.RecordService.ValidateReportJoinConfig: %w", err)
		}
		// Each hop starts from the entity the previous one reached (same chain as the run path).
		far, _, _, isNN, derr := resolveJoinEdge(rel, from, relName)
		if derr != nil {
			return derr
		}
		anyNN = anyNN || isNN
		from = far
	}
	return validateNNFanoutSafety(anyNN, metric)
}

// resolveAggregatePlanJoined is resolveAggregatePlan for a joined aggregate: groupBy and/or the
// metric field may be namespaced "<rel>.<field>" onto the join's related entity; a namespaced
// token resolves against the RELATED entity's specs, a bare token against the BASE — both
// through the SAME extracted helper bodies (resolveAggMetric / resolveMetricField /
// resolveGroupByIndexed / resolveAggBucket), so validation codes/messages can never drift from
// the single-table path. A prefix that does not name THE configured join →
// REPORT_JOIN_ENTITY_MISMATCH.
func resolveAggregatePlanJoined(metric AggregateMetric, field, groupBy, bucket string,
	specs []FieldSpec, byName map[string]FieldSpec, join *resolvedReportJoin) (aggregatePlan, error) {

	var plan aggregatePlan
	repoMetric, err := resolveAggMetric(metric)
	if err != nil {
		return plan, err
	}
	plan.metric = repoMetric

	// Metric field axis (COUNT needs none — a namespaced token is ignored exactly like a bare
	// one is on the single-table path).
	if repoMetric != dynrepo.MetricCount {
		fieldName := strings.TrimSpace(field)
		axisByName := byName
		if pre, bare, namespaced := splitJoinedToken(fieldName); namespaced {
			hop, ok := join.hopNamed(pre)
			if !ok {
				return plan, invalid("REPORT_JOIN_ENTITY_MISMATCH",
					"field %q tham chiếu %q — không khớp join %q", fieldName, pre, join.hopNames())
			}
			fieldName, axisByName = bare, hop.byName
			plan.fieldAlias = hop.alias
		}
		col, subPath, ccy, ferr := resolveMetricField(metric, fieldName, axisByName)
		if ferr != nil {
			return plan, ferr
		}
		plan.fieldColumn, plan.fieldSubPath, plan.currencyCode = col, subPath, ccy
	}

	// GroupBy axis — the indexed-physical-column rule (AGGREGATE_REQUIRES_INDEXED) applies on
	// whichever entity the token resolves on (spec §2.4); the W4 bucket rides that same axis.
	// ONE body with the single-table path and every grid level (resolveGroupAxis).
	ax, gerr := resolveGroupAxis(groupBy, bucket, specs, byName, join)
	if gerr != nil {
		return plan, gerr
	}
	plan.groupByPhysical, plan.groupByField, plan.groupByAlias, plan.bucket = ax.physical, ax.field, ax.alias, ax.bucket
	return plan, nil
}
