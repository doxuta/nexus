//go:build integration

package handlers

// Queue #157 — PUT /admin/fields/:id must not change a currency field's configured currencyCode
// while live records still store the old code: that edit relabelled every such record (USD 10
// read as "VND 10" in every total) and is how the dev DB came to hold 9 USD rows under a VND
// config. Real MySQL, real HTTP stack, real JSON predicate.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"nexus/platform/internal/dynrepo"
)

func TestCurrencyConfigChangeGuard_Integration(t *testing.T) {
	r := setupW3Entity(t) // `price` is configured USD, precision 2
	_, sx := newFullRouter(t)

	var fieldID string
	if err := sx.Get(&fieldID, `SELECT f.id FROM sys_fields f JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = 'price' AND f.deleted_at IS NULL`, testTenant, w3Entity); err != nil {
		t.Fatalf("resolve price field id: %v", err)
	}
	create := func(email string) string {
		t.Helper()
		w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, map[string]any{
			"email":    email,
			"fullname": map[string]any{"first": "A"},
			"price":    map[string]any{"amount": 10.0, "currencyCode": "USD"},
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: status=%d — %s", w.Code, w.Body.String())
		}
		var got struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("parse create: %v", err)
		}
		id, _ := got.Data["id"].(string)
		return id
	}
	live := create("q157-live@example.com")
	gone := create("q157-gone@example.com")
	if w := do(t, r, http.MethodDelete, "/api/v1/"+w3Entity+"/"+gone, nil); w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("soft-delete: status=%d — %s", w.Code, w.Body.String())
	}

	toVND := map[string]any{"options": map[string]any{"currencyCode": "VND", "precision": float64(2)}}
	storedCode := func() string {
		t.Helper()
		var code string
		if err := sx.Get(&code, `SELECT JSON_UNQUOTE(JSON_EXTRACT(options, '$.currencyCode')) FROM sys_fields WHERE id = ?`, fieldID); err != nil {
			t.Fatalf("read field options: %v", err)
		}
		return code
	}

	// One live USD record plus one in the recycle bin → refused, config untouched. The trashed row
	// counts: POST /:entity/:id/restore brings it back unchanged, still stored in USD.
	w := do(t, r, http.MethodPut, "/api/v1/admin/fields/"+fieldID, toVND)
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "FIELD_CURRENCY_CODE_IN_USE" {
		t.Fatalf("USD→VND with a USD record: status=%d code=%s; want 422 FIELD_CURRENCY_CODE_IN_USE — %s", w.Code, bodyCode(t, w), w.Body.String())
	}
	if d := bodyDetails(t, w); d["records"] != 2.0 || d["recycleBin"] != 1.0 || d["from"] != "USD" || d["to"] != "VND" {
		t.Errorf("details = %v; want records=2 recycleBin=1 from=USD to=VND", d)
	}
	if got := storedCode(); got != "USD" {
		t.Fatalf("configured code after a refused edit = %q; want USD", got)
	}

	// The count is tenant-pinned: the other tenant has no rows in this table.
	repo := dynrepo.New(sx)
	if live, trashed, err := repo.CountCurrencyCodeRows(context.Background(), w3Entity, otherTenant, "price", "USD"); err != nil || live != 0 || trashed != 0 {
		t.Errorf("other tenant count = %d live + %d trashed, %v; want 0, 0, nil", live, trashed, err)
	}

	// A relabel of the field (same code resent, as Studio does) still saves.
	w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+fieldID, map[string]any{
		"label": "Giá", "options": map[string]any{"currencyCode": "USD", "precision": float64(2)},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("relabel with the same code: status=%d; want 200 — %s", w.Code, w.Body.String())
	}

	// The recycle-bin bypass (review of #157): with the live row really in VND, the soft-deleted
	// USD row alone must still block the change — otherwise "delete, change the code, restore"
	// brings back USD 10 read as VND 10 in every total.
	plantVND := func(id string) {
		t.Helper()
		if _, err := sx.Exec("UPDATE `obj_"+w3Entity+"` SET `data` = JSON_SET(`data`, '$.price.currencyCode', 'VND') WHERE `tenant_id` = ? AND `id` = ?", testTenant, id); err != nil {
			t.Fatalf("plant VND row: %v", err)
		}
	}
	plantVND(live)
	w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+fieldID, toVND)
	if w.Code != http.StatusUnprocessableEntity || bodyCode(t, w) != "FIELD_CURRENCY_CODE_IN_USE" {
		t.Fatalf("USD→VND with the only USD record in the recycle bin: status=%d code=%s; want 422 FIELD_CURRENCY_CODE_IN_USE — %s", w.Code, bodyCode(t, w), w.Body.String())
	}
	if d := bodyDetails(t, w); d["records"] != 1.0 || d["recycleBin"] != 1.0 {
		t.Errorf("details = %v; want records=1 recycleBin=1", d)
	}
	if got := storedCode(); got != "USD" {
		t.Fatalf("configured code after a refused edit = %q; want USD", got)
	}
	// Restoring it now is harmless: it comes back in USD under the USD config it was stored under.
	if w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity+"/"+gone+"/restore", nil); w.Code != http.StatusOK {
		t.Fatalf("restore: status=%d — %s", w.Code, w.Body.String())
	}

	// Once no record is left in USD, live or trashed (here: both really hold VND — the fix
	// direction), the change goes through.
	plantVND(gone)
	w = do(t, r, http.MethodPut, "/api/v1/admin/fields/"+fieldID, toVND)
	if w.Code != http.StatusOK {
		t.Fatalf("USD→VND with no USD record left: status=%d; want 200 — %s", w.Code, w.Body.String())
	}
	if got := storedCode(); got != "VND" {
		t.Fatalf("configured code = %q; want VND", got)
	}
}
