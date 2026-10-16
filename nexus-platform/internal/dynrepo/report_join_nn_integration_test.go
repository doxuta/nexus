//go:build integration

// Integration tests for REPORT-JOIN-NN — the two-hop (base → rel_<a>_<b> link table → far entity)
// aggregate JOIN. These are the DISCRIMINATING tests the brief mandates: they run against MySQL
// and prove (a) the JOIN mechanics are correct (no cartesian leak — an unlinked row never enters
// the aggregate), (b) N-N's SYMMETRIC fan-out (a far row can link to MANY base rows, exactly as a
// base row can link to many far rows) makes a raw SUM/AVG of EITHER side's field wrong — the
// reason services.validateNNFanoutSafety rejects sum/avg over any N-N hop unconditionally
// (services/report_join_test.go proves the actual 422; dynrepo has no policy layer, so this file
// proves the underlying SQL DANGER the policy layer exists to prevent), (c) MIN/MAX/COUNT stay
// correct despite fan-out (dynrepo runs them with no gate because the engine allows them), (d)
// per-hop tenant isolation closes the RM-28-shaped leak at the join predicate, and (e) the far
// entity's A5 row-level scope is honoured on hop 2.
//
// Run with:  go test -tags=integration ./internal/dynrepo/
package dynrepo

import (
	"context"
	"testing"

	"nexus/platform/internal/ddl"
)

// seedNNFixture builds a base entity + a far entity + their rel_<a>_<b> N-N link table, and
// returns the repo. Far.score is a plain JSON field (no index needed — AggregateParams.FieldColumn
// reads it via JSON_EXTRACT regardless of indexing).
func seedNNFixture(t *testing.T) (*Repository, context.Context, func()) {
	t.Helper()
	db := testDB(t)
	repo := New(db)
	ctx := context.Background()

	linkTable, _, _, err := ddl.JoinParts("it_rn_base", "it_rn_far")
	if err != nil {
		t.Fatalf("JoinParts: %v", err)
	}
	_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+linkTable+"`")
	_ = repo.DropTable(ctx, "it_rn_base")
	_ = repo.DropTable(ctx, "it_rn_far")
	if err := repo.CreateTable(ctx, "it_rn_base", nil); err != nil {
		t.Fatalf("CreateTable base: %v", err)
	}
	if err := repo.CreateTable(ctx, "it_rn_far", []ddl.FieldDef{
		{Name: "score", FieldType: "number"},
	}); err != nil {
		t.Fatalf("CreateTable far: %v", err)
	}
	if err := repo.CreateJoinTable(ctx, "it_rn_base", "it_rn_far"); err != nil {
		t.Fatalf("CreateJoinTable: %v", err)
	}
	cleanup := func() {
		_, _ = db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+linkTable+"`")
		_ = repo.DropTable(ctx, "it_rn_base")
		_ = repo.DropTable(ctx, "it_rn_far")
	}
	return repo, ctx, cleanup
}

const (
	rjnnTenantA = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	rjnnTenantB = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	rjnnUser1   = "33333333-3333-3333-3333-333333333333"
	rjnnUser2   = "44444444-4444-4444-4444-444444444444"
)

// nnJoinSpecs builds the two chained JoinSpecs (base→link, link→far) exactly as
// services.RecordService.resolveReportJoin does for an N-N hop (record_aggregate_join.go), so
// this test proves the REAL SQL shape, not a simplified stand-in.
func nnJoinSpecs(farScope RowScope) []JoinSpec {
	linkTable, colA, colB, _ := ddl.JoinParts("it_rn_base", "it_rn_far")
	baseCol, farCol := colA, colB
	if colA != "it_rn_base_id" {
		baseCol, farCol = colB, colA
	}
	farTable, _ := TableFor("it_rn_far")
	return []JoinSpec{
		{
			Alias: "lt", RelatedTable: linkTable,
			LeftCol: "id", RightCol: baseCol,
			NoSoftDelete: true, NoRowScope: true,
		},
		{
			Alias: "j0", RelatedTable: farTable, FromAlias: "lt",
			LeftCol: farCol, RightCol: "id",
			Scope: farScope,
		},
	}
}

// TestReportJoinNN_FanoutFixture proves the 2-hop JOIN's mechanics are correct for the
// base-fans-out-to-far shape: one base record links to TWO far records with known values (100,
// 50) — SUM/AVG/COUNT of the FAR field must reflect EXACTLY those two rows, not more. A THIRD,
// unlinked far record (score=999) proves the join predicate genuinely narrows to LINKED rows (a
// cartesian-join bug would pull it in, giving 100+50+999=1149 instead of 150 — a visibly
// different, easy-to-spot-wrong number).
//
// ⚠️ This test does NOT prove "sum of a far field is safe over N-N" as a general rule — it only
// proves the SQL is correct for THIS data shape, where each far row happens to link to exactly
// ONE base row. N-N is SYMMETRIC: a far row CAN link to many base rows too (that is the defining
// shape of a many-to-many relation), and when it does, SUM/AVG of that far field DOES
// double-count — see TestReportJoinNN_FarSideFanout_SumWouldDoubleCount right below, which is why
// services.validateNNFanoutSafety rejects sum/avg over ANY N-N hop unconditionally, regardless of
// which side the aggregated field lives on.
func TestReportJoinNN_FanoutFixture(t *testing.T) {
	repo, ctx, cleanup := seedNNFixture(t)
	defer cleanup()

	base1, err := repo.InsertRecord(ctx, "it_rn_base", rjnnTenantA, rjnnUser1, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("insert base1: %v", err)
	}
	far1, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 100}, nil)
	if err != nil {
		t.Fatalf("insert far1: %v", err)
	}
	far2, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 50}, nil)
	if err != nil {
		t.Fatalf("insert far2: %v", err)
	}
	// far3 is NEVER linked to base1 — a cartesian-join bug would still pull it into base1's
	// aggregate; a correct join must exclude it.
	if _, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 999}, nil); err != nil {
		t.Fatalf("insert far3 (unlinked): %v", err)
	}

	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", base1, "it_rn_far", far1); err != nil {
		t.Fatalf("link base1-far1: %v", err)
	}
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", base1, "it_rn_far", far2); err != nil {
		t.Fatalf("link base1-far2: %v", err)
	}

	params := AggregateParams{
		Metric: MetricSum, FieldColumn: "score", FieldAlias: "j0",
		Joins: nnJoinSpecs(AdminScope()),
	}
	sc, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), params)
	if err != nil {
		t.Fatalf("AggregateScalar sum: %v", err)
	}
	if sc.Value == nil || !decEq(*sc.Value, "150") {
		t.Fatalf("SUM(far.score) = %v, want 150 (100+50, NOT double-counted, NOT including unlinked far3=999)", strOrNil(sc.Value))
	}
	if sc.Count != 2 {
		t.Fatalf("COUNT = %d, want 2 (exactly the 2 linked far rows — a fan-out/cartesian bug would give a different count)", sc.Count)
	}

	avgParams := AggregateParams{Metric: MetricAvg, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(AdminScope())}
	avg, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), avgParams)
	if err != nil {
		t.Fatalf("AggregateScalar avg: %v", err)
	}
	if avg.Value == nil || !decEq(*avg.Value, "75") {
		t.Fatalf("AVG(far.score) = %v, want 75 (150/2, not 383=1149/3 which is what a cartesian leak of far3 would give)", strOrNil(avg.Value))
	}

	countParams := AggregateParams{Metric: MetricCount, Joins: nnJoinSpecs(AdminScope())}
	cnt, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), countParams)
	if err != nil {
		t.Fatalf("AggregateScalar count: %v", err)
	}
	if cnt.Count != 2 {
		t.Fatalf("COUNT(*) over N-N join = %d, want 2 (base1,far1)+(base1,far2) pairs", cnt.Count)
	}
}

// TestReportJoinNN_FarSideFanout_SumWouldDoubleCount is THE mandatory discriminating test for the
// reviewer-caught bug: N-N is SYMMETRIC — a FAR row can link to MANY base rows too, not just the
// other way around. This fixture is structurally blind in TestReportJoinNN_FanoutFixture (every
// far there links to exactly 1 base); here farZ (score=10) is linked to TWO base rows (baseX,
// baseY), plus farW (score=100) linked to only baseX — the join therefore yields 3 (base,far)
// pairs: (baseX,farZ)=10, (baseY,farZ)=10, (baseX,farW)=100.
//
// A raw SUM/AVG over this join is WRONG: SUM = 10+10+100 = 120 (the true distinct-far sum is
// farZ+farW = 10+100 = 110); AVG = 120/3 = 40 (the true distinct-far average is 110/2 = 55) — both
// visibly different from the correct distinct-far numbers, proving farZ's value was counted TWICE
// (once per linked base row). dynrepo has NO policy/business-rule layer (AggregateScalar just
// executes whatever AggregateParams it is given) — this SQL-level proof is exactly why the
// SERVICE layer's validateNNFanoutSafety rejects sum/avg over ANY N-N hop unconditionally with
// 422 AGGREGATE_NN_FANOUT_UNSAFE, BEFORE this SQL is ever built
// (services/report_join_test.go's TestAggregate_NNFanoutGate proves the actual 422 rejection —
// mocked, no DB needed, since the gate runs before any query).
//
// MIN/MAX and COUNT remain CORRECT despite the same fan-out: MIN/MAX are non-additive (a
// duplicated value never changes the extremum: min=10, max=100 — the single correct values, not
// doubled), and COUNT(*) correctly reports the PAIR count (3) — the documented "how many linked
// rows" semantic, not a claim about distinct far rows.
func TestReportJoinNN_FarSideFanout_SumWouldDoubleCount(t *testing.T) {
	repo, ctx, cleanup := seedNNFixture(t)
	defer cleanup()

	baseX, err := repo.InsertRecord(ctx, "it_rn_base", rjnnTenantA, rjnnUser1, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("insert baseX: %v", err)
	}
	baseY, err := repo.InsertRecord(ctx, "it_rn_base", rjnnTenantA, rjnnUser1, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("insert baseY: %v", err)
	}
	farZ, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 10}, nil)
	if err != nil {
		t.Fatalf("insert farZ: %v", err)
	}
	farW, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 100}, nil)
	if err != nil {
		t.Fatalf("insert farW: %v", err)
	}

	// farZ linked to BOTH base rows — the far-side fan-out.
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", baseX, "it_rn_far", farZ); err != nil {
		t.Fatalf("link baseX-farZ: %v", err)
	}
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", baseY, "it_rn_far", farZ); err != nil {
		t.Fatalf("link baseY-farZ: %v", err)
	}
	// farW linked to only baseX.
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", baseX, "it_rn_far", farW); err != nil {
		t.Fatalf("link baseX-farW: %v", err)
	}

	sumParams := AggregateParams{Metric: MetricSum, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(AdminScope())}
	sum, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), sumParams)
	if err != nil {
		t.Fatalf("AggregateScalar sum: %v", err)
	}
	if sum.Value == nil || !decEq(*sum.Value, "120") {
		t.Fatalf("SUM(far.score) = %v, want 120 (the RAW join double-counts farZ=10 once per linked base row: 10+10+100 — the true distinct-far sum is 110; this WRONG number is exactly why the service layer rejects sum/avg over N-N unconditionally)", strOrNil(sum.Value))
	}

	avgParams := AggregateParams{Metric: MetricAvg, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(AdminScope())}
	avg, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), avgParams)
	if err != nil {
		t.Fatalf("AggregateScalar avg: %v", err)
	}
	if avg.Value == nil || !decEq(*avg.Value, "40") {
		t.Fatalf("AVG(far.score) = %v, want 40 (120/3 pairs — the true distinct-far average is 110/2=55, ALSO wrong)", strOrNil(avg.Value))
	}

	minParams := AggregateParams{Metric: MetricMin, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(AdminScope())}
	mn, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), minParams)
	if err != nil {
		t.Fatalf("AggregateScalar min: %v", err)
	}
	if mn.Value == nil || !decEq(*mn.Value, "10") {
		t.Fatalf("MIN(far.score) = %v, want 10 — the SINGLE correct value, unaffected by farZ's duplication", strOrNil(mn.Value))
	}

	maxParams := AggregateParams{Metric: MetricMax, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(AdminScope())}
	mx, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), maxParams)
	if err != nil {
		t.Fatalf("AggregateScalar max: %v", err)
	}
	if mx.Value == nil || !decEq(*mx.Value, "100") {
		t.Fatalf("MAX(far.score) = %v, want 100 — the SINGLE correct value, unaffected by duplication", strOrNil(mx.Value))
	}

	countParams := AggregateParams{Metric: MetricCount, Joins: nnJoinSpecs(AdminScope())}
	cnt, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), countParams)
	if err != nil {
		t.Fatalf("AggregateScalar count: %v", err)
	}
	if cnt.Count != 3 {
		t.Fatalf("COUNT(*) = %d, want 3 (the correct PAIR count: (baseX,farZ)+(baseY,farZ)+(baseX,farW))", cnt.Count)
	}
}

// TestReportJoinNN_CrossTenantIsolation is THE bug-class gate for the N-N hop: a malicious/
// mistaken link row in tenant A's link-table space points at a far record that actually belongs
// to tenant B. Hop 2's own `far.tenant_id = ?` predicate must exclude it — tenant A's aggregate
// must never see tenant B's far value, even though the shared rel_/obj_ tables physically hold
// both tenants' rows.
func TestReportJoinNN_CrossTenantIsolation(t *testing.T) {
	repo, ctx, cleanup := seedNNFixture(t)
	defer cleanup()

	baseA, err := repo.InsertRecord(ctx, "it_rn_base", rjnnTenantA, rjnnUser1, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("insert baseA: %v", err)
	}
	farA, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 10}, nil)
	if err != nil {
		t.Fatalf("insert farA: %v", err)
	}
	farB, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantB, rjnnUser1, map[string]any{"score": 777}, nil)
	if err != nil {
		t.Fatalf("insert farB: %v", err)
	}

	// Legit link within tenant A.
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", baseA, "it_rn_far", farA); err != nil {
		t.Fatalf("link baseA-farA: %v", err)
	}
	// The collision: a link row STAMPED tenant A but pointing at tenant B's far record id.
	// LinkRecords does not itself verify cross-tenant ownership of the target id (a known
	// simplification) — the per-hop tenant filter in the JOIN's ON clause is what must close this.
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", baseA, "it_rn_far", farB); err != nil {
		t.Fatalf("link baseA-farB (cross-tenant collision): %v", err)
	}

	params := AggregateParams{Metric: MetricSum, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(AdminScope())}
	sc, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), params)
	if err != nil {
		t.Fatalf("AggregateScalar: %v", err)
	}
	if sc.Value == nil || !decEq(*sc.Value, "10") {
		t.Fatalf("CROSS-TENANT LEAK: tenant A's SUM = %v, want 10 (only farA) — tenant B's farB=777 must be excluded by the per-hop tenant filter", strOrNil(sc.Value))
	}
	if sc.Count != 1 {
		t.Fatalf("CROSS-TENANT LEAK: tenant A's COUNT = %d, want 1 (the cross-tenant link row must not surface farB)", sc.Count)
	}
}

// TestReportJoinNN_RowLevelScopeOnFarEntity proves hop 2 honours A5 row-level access on the FAR
// entity: an owner-scoped hop excludes far rows the scope's user does not own, even though the
// BASE row and the link rows are all visible under an admin base scope.
func TestReportJoinNN_RowLevelScopeOnFarEntity(t *testing.T) {
	repo, ctx, cleanup := seedNNFixture(t)
	defer cleanup()

	base1, err := repo.InsertRecord(ctx, "it_rn_base", rjnnTenantA, rjnnUser1, map[string]any{}, nil)
	if err != nil {
		t.Fatalf("insert base1: %v", err)
	}
	farOwn, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser1, map[string]any{"score": 5}, nil)
	if err != nil {
		t.Fatalf("insert farOwn (owned by user1): %v", err)
	}
	farOther, err := repo.InsertRecord(ctx, "it_rn_far", rjnnTenantA, rjnnUser2, map[string]any{"score": 500}, nil)
	if err != nil {
		t.Fatalf("insert farOther (owned by user2): %v", err)
	}
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", base1, "it_rn_far", farOwn); err != nil {
		t.Fatalf("link base1-farOwn: %v", err)
	}
	if _, err := repo.LinkRecords(ctx, rjnnTenantA, rjnnUser1, "it_rn_base", base1, "it_rn_far", farOther); err != nil {
		t.Fatalf("link base1-farOther: %v", err)
	}

	ownerScope := RowScope{Level: AccessOwner, UserID: rjnnUser1}
	params := AggregateParams{Metric: MetricSum, FieldColumn: "score", FieldAlias: "j0", Joins: nnJoinSpecs(ownerScope)}
	sc, err := repo.AggregateScalar(ctx, "it_rn_base", rjnnTenantA, AdminScope(), params)
	if err != nil {
		t.Fatalf("AggregateScalar: %v", err)
	}
	if sc.Value == nil || !decEq(*sc.Value, "5") {
		t.Fatalf("ROW-LEVEL LEAK: owner-scoped SUM = %v, want 5 (only farOwn) — farOther (user2's) must be excluded", strOrNil(sc.Value))
	}
	if sc.Count != 1 {
		t.Fatalf("ROW-LEVEL LEAK: owner-scoped COUNT = %d, want 1", sc.Count)
	}
}
