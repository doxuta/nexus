//go:build integration

package handlers

import (
	"encoding/json"
	"net/http"
	"testing"
)

// aggFormulaEntity backs TestAggregateFormulaFields (specs/070 #264).
const aggFormulaEntity = "it_agg_formula"

// TestAggregateFormulaFields drives the three aggregate surfaces a customer app uses — the live
// GET /:entity/aggregate, a saved summary report (/reports/:id/run) and a dashboard widget
// (/dashboards/:id/run) — over formula fields on real MySQL: a currency formula SUMs its stored
// .amount and reports its code, a number formula SUMs/AVGs as a number grouped by an indexed
// picklist, and a text formula is still refused with AGGREGATE_FIELD_NOT_NUMERIC.
func TestAggregateFormulaFields(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, aggFormulaEntity)
	t.Cleanup(func() {
		sx.Exec(`DELETE FROM sys_reports WHERE tenant_id = ? AND entity_name = ?`, testTenant, aggFormulaEntity)
		sx.Exec(`DELETE FROM sys_dashboards WHERE tenant_id = ? AND name = ?`, testTenant, "it formula dash")
		cleanupNamed(sx, aggFormulaEntity)
	})

	create := map[string]any{
		"name": aggFormulaEntity, "label": "Hoá đơn",
		"fields": []map[string]any{
			{"name": "stage", "label": "Trạng thái", "fieldType": "picklist",
				"options": map[string]any{"values": []string{"open", "paid"}, "indexed": true}},
			{"name": "qty", "label": "Số lượng", "fieldType": "number"},
			{"name": "price", "label": "Đơn giá", "fieldType": "currency", "options": map[string]any{"currencyCode": "VND"}},
			{"name": "paid", "label": "Đã trả", "fieldType": "number"},
			{"name": "line_total", "label": "Thành tiền", "fieldType": "formula",
				"options": map[string]any{"formula": "qty * price", "resultType": "currency", "currencyCode": "VND"}},
			{"name": "balance", "label": "Còn nợ", "fieldType": "formula",
				"options": map[string]any{"formula": "qty * price - paid"}},
			{"name": "tier", "label": "Hạng", "fieldType": "formula",
				"options": map[string]any{"formula": `IF(qty >= 3, "Lớn", "Nhỏ")`, "resultType": "text"}},
		},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", create); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	for _, row := range []map[string]any{
		// line_total 2*100.5=201, balance 201-1=200
		{"stage": "open", "qty": 2, "price": map[string]any{"amount": 100.5, "currencyCode": "VND"}, "paid": 1},
		// line_total 3*1000=3000, balance 3000-500=2500
		{"stage": "open", "qty": 3, "price": map[string]any{"amount": 1000, "currencyCode": "VND"}, "paid": 500},
		// line_total 1*50=50, balance 50-50=0
		{"stage": "paid", "qty": 1, "price": map[string]any{"amount": 50, "currencyCode": "VND"}, "paid": 50},
	} {
		if w := do(t, r, http.MethodPost, "/api/v1/"+aggFormulaEntity, row); w.Code != http.StatusCreated {
			t.Fatalf("create record %v: %d %s", row, w.Code, w.Body.String())
		}
	}

	// --- live aggregate endpoint ---
	get := func(q string) (int, []byte) {
		w := do(t, r, http.MethodGet, "/api/v1/"+aggFormulaEntity+"/aggregate?"+q, nil)
		return w.Code, w.Body.Bytes()
	}
	var scalar struct {
		Data struct {
			Value        *string `json:"value"`
			Count        int64   `json:"count"`
			CurrencyCode string  `json:"currencyCode"`
		} `json:"data"`
	}
	code, body := get("metric=sum&field=line_total")
	json.Unmarshal(body, &scalar)
	if code != http.StatusOK || scalar.Data.Value == nil || !decEqual(*scalar.Data.Value, "3251") || scalar.Data.CurrencyCode != "VND" {
		t.Fatalf("sum(line_total) = %d %s; want 200 value 3251 currencyCode VND", code, body)
	}
	code, body = get("metric=avg&field=balance")
	scalar.Data.Value = nil
	json.Unmarshal(body, &scalar)
	if code != http.StatusOK || scalar.Data.Value == nil || !decEqual(*scalar.Data.Value, "900") {
		t.Fatalf("avg(balance) = %d %s; want 200 value 900", code, body)
	}
	var grouped aggGroupResp
	code, body = get("metric=sum&field=balance&groupBy=stage")
	json.Unmarshal(body, &grouped)
	if code != http.StatusOK || len(grouped.Data.Groups) != 2 {
		t.Fatalf("sum(balance) by stage = %d %s; want 2 groups", code, body)
	}
	g := grouped.Data.Groups[0]
	if g.Key == nil || *g.Key != "open" || g.Value == nil || !decEqual(*g.Value, "2700") || g.Count != 2 {
		t.Fatalf("group[0] = %s; want open 2700 ×2", body)
	}
	code, body = get("metric=sum&field=tier")
	if code != http.StatusUnprocessableEntity || errCodeOf(body) != "AGGREGATE_FIELD_NOT_NUMERIC" {
		t.Fatalf("sum(text formula) = %d %s; want 422 AGGREGATE_FIELD_NOT_NUMERIC", code, body)
	}

	// --- saved summary report ---
	w := do(t, r, http.MethodPost, "/api/v1/admin/reports", map[string]any{
		"name": "Tổng còn nợ theo trạng thái", "entityName": aggFormulaEntity, "type": "summary",
		"config": map[string]any{"metric": "sum", "field": "line_total", "groupBy": "stage"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create report: %d %s", w.Code, w.Body.String())
	}
	var rep struct {
		Data struct {
			Report struct {
				ID string `json:"id"`
			} `json:"report"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &rep)
	w = do(t, r, http.MethodGet, "/api/v1/reports/"+rep.Data.Report.ID+"/run", nil)
	var run struct {
		Data struct {
			Groups []struct {
				Key   *string `json:"key"`
				Value *string `json:"value"`
			} `json:"groups"`
			CurrencyCode string `json:"currencyCode"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &run)
	if w.Code != http.StatusOK || len(run.Data.Groups) != 2 || run.Data.CurrencyCode != "VND" ||
		run.Data.Groups[0].Value == nil || !decEqual(*run.Data.Groups[0].Value, "3201") {
		t.Fatalf("report run = %d %s; want 200, open first = 3201, currencyCode VND", w.Code, w.Body.String())
	}

	// --- dashboard KPI widget ---
	w = do(t, r, http.MethodPost, "/api/v1/admin/dashboards", map[string]any{
		"name": "it formula dash",
		"widgets": []map[string]any{
			{"id": "w1", "title": "Còn nợ", "type": "kpi", "entity": aggFormulaEntity, "metric": "sum", "field": "balance"},
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create dashboard: %d %s", w.Code, w.Body.String())
	}
	var dash struct {
		Data struct {
			Dashboard struct {
				ID string `json:"id"`
			} `json:"dashboard"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &dash)
	w = do(t, r, http.MethodGet, "/api/v1/dashboards/"+dash.Data.Dashboard.ID+"/run", nil)
	var drun struct {
		Data struct {
			Widgets []struct {
				Result *struct {
					Value *string `json:"value"`
				} `json:"result"`
				Code string `json:"code"`
			} `json:"widgets"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &drun)
	if w.Code != http.StatusOK || len(drun.Data.Widgets) != 1 || drun.Data.Widgets[0].Result == nil ||
		drun.Data.Widgets[0].Result.Value == nil || !decEqual(*drun.Data.Widgets[0].Result.Value, "2700") {
		t.Fatalf("dashboard run = %d %s; want KPI value 2700", w.Code, w.Body.String())
	}
}

// aggFormulaLateEntity backs TestAggregateFormulaAddedAfterData.
const aggFormulaLateEntity = "it_agg_formula_late"

// TestAggregateFormulaAddedAfterData is the review finding on specs/070 #264: a formula is
// evaluated and STORED at write time, so a formula added to — or edited on — an entity that
// already holds rows used to leave those rows without a value (or with the old one). The report,
// the widget and ⌘K then summed only the rows written since: 200 with a wrong total, no warning.
// Adding or editing a formula now re-evaluates the tenant's existing rows.
func TestAggregateFormulaAddedAfterData(t *testing.T) {
	r, sx := newFullRouter(t)
	cleanupNamed(sx, aggFormulaLateEntity)
	t.Cleanup(func() { cleanupNamed(sx, aggFormulaLateEntity) })

	create := map[string]any{
		"name": aggFormulaLateEntity, "label": "Dòng hàng",
		"fields": []map[string]any{{"name": "qty", "label": "Số lượng", "fieldType": "number"}},
	}
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", create); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}
	addRow := func(qty int) {
		t.Helper()
		if w := do(t, r, http.MethodPost, "/api/v1/"+aggFormulaLateEntity, map[string]any{"qty": qty}); w.Code != http.StatusCreated {
			t.Fatalf("create record qty=%d: %d %s", qty, w.Code, w.Body.String())
		}
	}
	addRow(2)
	addRow(3)
	// Pin the rows' last-modified stamp in the past: the recompute must not move it.
	if _, err := sx.Exec("UPDATE `obj_"+aggFormulaLateEntity+"` SET `updated_at` = '2026-01-02 03:04:05' WHERE `tenant_id` = ?", testTenant); err != nil {
		t.Fatalf("pin updated_at: %v", err)
	}

	sum := func(step, want, wantCode string) {
		t.Helper()
		w := do(t, r, http.MethodGet, "/api/v1/"+aggFormulaLateEntity+"/aggregate?metric=sum&field=total", nil)
		var resp struct {
			Data struct {
				Value        *string `json:"value"`
				CurrencyCode string  `json:"currencyCode"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &resp)
		if w.Code != http.StatusOK || resp.Data.Value == nil || !decEqual(*resp.Data.Value, want) || resp.Data.CurrencyCode != wantCode {
			t.Fatalf("%s: sum(total) = %d %s; want 200 value %s currencyCode %q", step, w.Code, w.Body.String(), want, wantCode)
		}
	}

	w := do(t, r, http.MethodPost, "/api/v1/admin/fields", map[string]any{
		"entityName": aggFormulaLateEntity, "name": "total", "label": "Thành tiền", "fieldType": "formula",
		"options": map[string]any{"formula": "qty * 10"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("add formula: %d %s", w.Code, w.Body.String())
	}
	var added struct {
		Data struct {
			Field struct {
				ID string `json:"id"`
			} `json:"field"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &added)
	sum("formula added over 2 rows", "50", "")
	addRow(1)
	sum("a row written after the add", "60", "")

	// Editing the formula's result type re-stores every row in the new shape ({amount, code}).
	edit := func(step string, opts map[string]any) {
		t.Helper()
		w := do(t, r, http.MethodPut, "/api/v1/admin/fields/"+added.Data.Field.ID, map[string]any{
			"fieldType": "formula", "options": opts,
		})
		if w.Code != http.StatusOK {
			t.Fatalf("%s: edit formula: %d %s", step, w.Code, w.Body.String())
		}
	}
	edit("to currency", map[string]any{"formula": "qty * 10", "resultType": "currency", "currencyCode": "VND"})
	sum("resultType number -> currency", "60", "VND")
	edit("new expression", map[string]any{"formula": "qty * 100", "resultType": "currency", "currencyCode": "VND"})
	sum("expression edited", "600", "VND")

	// A derived refresh is not an edit: the two rows nobody touched keep version 0 and their
	// last-modified stamp, so an open form does not hit a false 409 and "Cập nhật lúc" stays true.
	var untouched int
	if err := sx.Get(&untouched, "SELECT COUNT(*) FROM `obj_"+aggFormulaLateEntity+"` WHERE `tenant_id` = ? "+
		"AND `version` = 0 AND `updated_at` = '2026-01-02 03:04:05'", testTenant); err != nil {
		t.Fatalf("read version/updated_at: %v", err)
	}
	if untouched != 2 {
		t.Fatalf("rows with version 0 and the pinned updated_at = %d; want 2 (the recompute must not count as an edit)", untouched)
	}
}
