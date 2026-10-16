//go:build integration

package handlers

import (
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
)

// aggEntity is the entity the aggregation HTTP tests provision. `status` is an INDEXED picklist
// (groupBy-able), `amount` is a plain number, `note` is a plain (non-indexed) text field so a
// groupBy on it must be refused with 422 AGGREGATE_REQUIRES_INDEXED.
const aggEntity = "it_agg_orders"

// aggOtherTenant is a DIFFERENT tenant than testTenant, used to prove aggregate tenant isolation.
const aggOtherTenant = "abcdef00-0000-0000-0000-0000000000ff"

// decEqual compares two decimal-string representations numerically (MySQL DECIMAL text carries
// trailing zeros, e.g. "600.0000000000"); big.Rat parses exact decimal text.
func decEqual(got, want string) bool {
	g, ok1 := new(big.Rat).SetString(got)
	w, ok2 := new(big.Rat).SetString(want)
	if !ok1 || !ok2 {
		return false
	}
	return g.Cmp(w) == 0
}

// errCodeOf extracts the machine error code from a {"error":...,"code":...} envelope.
func errCodeOf(body []byte) string {
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Code
}

type aggScalarResp struct {
	Data struct {
		Value *string `json:"value"`
		Count int64   `json:"count"`
	} `json:"data"`
}

type aggGroupResp struct {
	Data struct {
		GroupBy string `json:"groupBy"`
		Groups  []struct {
			Key   *string `json:"key"`
			Value *string `json:"value"`
			Count int64   `json:"count"`
		} `json:"groups"`
	} `json:"data"`
}

// TestAggregateEndpoint drives GET /:entity/aggregate end-to-end through the real router
// (Auth → Tenant → handler → service → dynrepo → MySQL): scalar sum/count/avg, group-by an
// indexed column, the AGGREGATE_REQUIRES_INDEXED guard, metric/field validation, and tenant
// isolation. It is the EPIC 6 M1 acceptance test.
func TestAggregateEndpoint(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, aggEntity)
	t.Cleanup(func() { cleanupNamed(sx, aggEntity) })
	// The isolation case also registers aggEntity for aggOtherTenant — clean that tenant's
	// metadata rows too (cleanupNamed only targets testTenant). The shared obj_* table is
	// dropped by cleanupNamed above.
	t.Cleanup(func() {
		sx.Exec(`DELETE f FROM sys_fields f JOIN sys_entities e ON f.entity_id = e.id
		         WHERE e.tenant_id = ? AND e.name = ?`, aggOtherTenant, aggEntity)
		sx.Exec(`DELETE FROM sys_entities WHERE tenant_id = ? AND name = ?`, aggOtherTenant, aggEntity)
	})

	// Arrange: create the entity with an INDEXED picklist `status`, a number `amount`, and a
	// plain (non-indexed) text `note`.
	createBody := map[string]any{
		"name":  aggEntity,
		"label": "Đơn hàng",
		"fields": []map[string]any{
			{"name": "status", "label": "Trạng thái", "fieldType": "picklist",
				"options": map[string]any{"values": []string{"won", "lost", "open"}, "indexed": true}},
			{"name": "amount", "label": "Giá trị", "fieldType": "number"},
			{"name": "note", "label": "Ghi chú", "fieldType": "text"},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", createBody); w.Code != http.StatusCreated {
		t.Fatalf("setup create entity: %d %s", w.Code, w.Body.String())
	}

	mustCreate := func(data map[string]any) {
		if w := do(t, r, http.MethodPost, "/api/v1/"+aggEntity, data); w.Code != http.StatusCreated {
			t.Fatalf("create record %v: %d %s", data, w.Code, w.Body.String())
		}
	}
	// testTenant: won 100+200+300 = 600 (×3), lost 50+50 = 100 (×2).
	mustCreate(map[string]any{"status": "won", "amount": 100, "note": "a"})
	mustCreate(map[string]any{"status": "won", "amount": 200, "note": "b"})
	mustCreate(map[string]any{"status": "won", "amount": 300, "note": "c"})
	mustCreate(map[string]any{"status": "lost", "amount": 50, "note": "d"})
	mustCreate(map[string]any{"status": "lost", "amount": 50, "note": "e"})

	get := func(query string) *httptest.ResponseRecorder {
		return do(t, r, http.MethodGet, "/api/v1/"+aggEntity+"/aggregate?"+query, nil)
	}
	want422 := func(w *httptest.ResponseRecorder, code, label string) {
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d; want 422 (%s)", label, w.Code, w.Body.String())
		}
		if got := errCodeOf(w.Body.Bytes()); got != code {
			t.Fatalf("%s: code %q; want %q (%s)", label, got, code, w.Body.String())
		}
	}

	// --- Scalar SUM(amount) over all rows = 700, count 5 ---
	w := get("metric=sum&field=amount")
	if w.Code != http.StatusOK {
		t.Fatalf("sum: %d %s", w.Code, w.Body.String())
	}
	var sum aggScalarResp
	json.Unmarshal(w.Body.Bytes(), &sum)
	if sum.Data.Value == nil || !decEqual(*sum.Data.Value, "700") || sum.Data.Count != 5 {
		t.Fatalf("sum(amount) value=%v count=%d; want 700/5", sum.Data.Value, sum.Data.Count)
	}

	// --- COUNT(*) = 5 (no field) ---
	w = get("metric=count")
	var cnt aggScalarResp
	json.Unmarshal(w.Body.Bytes(), &cnt)
	if w.Code != http.StatusOK || cnt.Data.Count != 5 {
		t.Fatalf("count: %d count=%d; want 200/5", w.Code, cnt.Data.Count)
	}

	// --- AVG(amount) filtered status=won = (100+200+300)/3 = 200 ---
	w = get("metric=avg&field=amount&status=won")
	var avg aggScalarResp
	json.Unmarshal(w.Body.Bytes(), &avg)
	if w.Code != http.StatusOK || avg.Data.Value == nil || !decEqual(*avg.Data.Value, "200") || avg.Data.Count != 3 {
		t.Fatalf("avg won: %d value=%v count=%d; want 200/3", w.Code, avg.Data.Value, avg.Data.Count)
	}

	// --- GROUP BY status, SUM(amount): won=600/3, lost=100/2; ordered value DESC ---
	w = get("metric=sum&field=amount&groupBy=status")
	if w.Code != http.StatusOK {
		t.Fatalf("group sum: %d %s", w.Code, w.Body.String())
	}
	var grp aggGroupResp
	json.Unmarshal(w.Body.Bytes(), &grp)
	if grp.Data.GroupBy != "status" {
		t.Errorf("group response groupBy = %q; want status", grp.Data.GroupBy)
	}
	if len(grp.Data.Groups) != 2 {
		t.Fatalf("group sum got %d groups; want 2 (%s)", len(grp.Data.Groups), w.Body.String())
	}
	// First group is the largest value (won=600) per ORDER BY value DESC.
	g0 := grp.Data.Groups[0]
	if g0.Key == nil || *g0.Key != "won" || g0.Value == nil || !decEqual(*g0.Value, "600") || g0.Count != 3 {
		t.Fatalf("group[0] = %+v; want won/600/3", g0)
	}
	g1 := grp.Data.Groups[1]
	if g1.Key == nil || *g1.Key != "lost" || g1.Value == nil || !decEqual(*g1.Value, "100") || g1.Count != 2 {
		t.Fatalf("group[1] = %+v; want lost/100/2", g1)
	}

	// --- COUNT grouped by status (the kanban "count per column" case) ---
	w = get("metric=count&groupBy=status")
	var cgrp aggGroupResp
	json.Unmarshal(w.Body.Bytes(), &cgrp)
	if w.Code != http.StatusOK || len(cgrp.Data.Groups) != 2 {
		t.Fatalf("count grouped: %d groups=%d; want 200/2", w.Code, len(cgrp.Data.Groups))
	}

	// --- groupBy a NON-indexed field (note) → 422 AGGREGATE_REQUIRES_INDEXED ---
	want422(get("metric=count&groupBy=note"), "AGGREGATE_REQUIRES_INDEXED", "groupBy non-indexed")

	// --- SUM on a non-numeric field (status) → 422 AGGREGATE_FIELD_NOT_NUMERIC ---
	want422(get("metric=sum&field=status"), "AGGREGATE_FIELD_NOT_NUMERIC", "sum non-numeric")

	// --- SUM with no field → 422 AGGREGATE_FIELD_REQUIRED ---
	want422(get("metric=sum"), "AGGREGATE_FIELD_REQUIRED", "sum no field")

	// --- unknown field → 422 AGGREGATE_FIELD_UNKNOWN ---
	want422(get("metric=sum&field=ghost"), "AGGREGATE_FIELD_UNKNOWN", "sum unknown field")

	// --- invalid metric → 422 AGGREGATE_INVALID_METRIC ---
	want422(get("metric=median&field=amount"), "AGGREGATE_INVALID_METRIC", "invalid metric")

	// --- unknown entity → 404 ENTITY_NOT_FOUND ---
	w = do(t, r, http.MethodGet, "/api/v1/no_such_agg_entity/aggregate?metric=count", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown entity aggregate: %d; want 404", w.Code)
	}

	// --- TENANT ISOLATION: a DIFFERENT tenant with the SAME entity registered (shared obj_*
	// table) sees ONLY its own rows. It has none, so its aggregate is empty (count 0) — never
	// testTenant's 700. This proves the tenant_id filter on the shared physical table. ---
	if w := doAs(t, r, http.MethodPost, "/api/v1/admin/entities", aggOtherTenant, createBody); w.Code != http.StatusCreated && w.Code != http.StatusConflict {
		t.Fatalf("other-tenant register entity: %d %s", w.Code, w.Body.String())
	}
	wo := doAs(t, r, http.MethodGet, "/api/v1/"+aggEntity+"/aggregate?metric=sum&field=amount", aggOtherTenant, nil)
	if wo.Code != http.StatusOK {
		t.Fatalf("other-tenant sum: %d %s", wo.Code, wo.Body.String())
	}
	var other aggScalarResp
	json.Unmarshal(wo.Body.Bytes(), &other)
	if other.Data.Count != 0 {
		t.Fatalf("tenant isolation breach: other tenant count=%d; want 0", other.Data.Count)
	}
	if other.Data.Value != nil && decEqual(*other.Data.Value, "700") {
		t.Fatalf("tenant isolation breach: other tenant saw testTenant sum 700")
	}
	// testTenant still sees its full 700 after the other tenant joined the shared table.
	w = get("metric=sum&field=amount")
	var reSum aggScalarResp
	json.Unmarshal(w.Body.Bytes(), &reSum)
	if reSum.Data.Value == nil || !decEqual(*reSum.Data.Value, "700") || reSum.Data.Count != 5 {
		t.Fatalf("testTenant sum after co-tenant register = %v/%d; want 700/5", reSum.Data.Value, reSum.Data.Count)
	}
}
