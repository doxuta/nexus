//go:build integration

package handlers

// Queue #63 — a legacy record holding a currency code different from the field's configured code
// is not silently relabelled by the edit form's save. The API has refused a mismatching code since
// queue #7, so the legacy row is planted the only way such rows ever came to exist: straight in
// the table. Then the exact request LayoutForm issues is replayed through the real HTTP stack.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestCurrencyLegacyMismatch_FormResendKeepsStoredValue_Integration(t *testing.T) {
	r := setupW3Entity(t) // `price` is configured USD, precision 2
	_, sx := newFullRouter(t)

	w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, map[string]any{
		"email":    "q63@example.com",
		"fullname": map[string]any{"first": "A"},
		"code":     "AB",
		"price":    map[string]any{"amount": 10.0, "currencyCode": "USD"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create: status=%d — %s", w.Code, w.Body.String())
	}
	var created struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("parse create: %v — %s", err, w.Body.String())
	}
	id, _ := created.Data["id"].(string)
	if id == "" {
		t.Fatalf("create: no id — %s", w.Body.String())
	}

	// Plant the legacy state: a VND row inside the USD-configured field (pre-Q7 data).
	res, err := sx.Exec("UPDATE `obj_"+w3Entity+"` SET `data` = JSON_SET(`data`, '$.price.currencyCode', 'VND') WHERE `tenant_id` = ? AND `id` = ?", testTenant, id)
	if err != nil {
		t.Fatalf("plant legacy row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("plant legacy row: %d rows affected; want 1", n)
	}

	read := func() map[string]any {
		t.Helper()
		w := do(t, r, http.MethodGet, "/api/v1/"+w3Entity+"/"+id, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("get: status=%d — %s", w.Code, w.Body.String())
		}
		var got struct {
			Data struct {
				Data map[string]any `json:"data"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("parse get: %v — %s", err, w.Body.String())
		}
		return got.Data.Data
	}

	// The LayoutForm update: every field resent, the configured code (USD) stamped over the stored
	// VND, the amount untouched — only `code` edited. Must save, and must NOT relabel.
	w = do(t, r, http.MethodPut, "/api/v1/"+w3Entity+"/"+id, map[string]any{
		"email":    "q63@example.com",
		"fullname": map[string]any{"first": "A"},
		"code":     "ABC",
		"price":    map[string]any{"amount": 10.0, "currencyCode": "USD"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("form resend: status=%d; want 200 — the rest of the save must go through — %s", w.Code, w.Body.String())
	}
	data := read()
	if data["code"] != "ABC" {
		t.Errorf("code = %v; want ABC (the edit the user actually made)", data["code"])
	}
	price, _ := data["price"].(map[string]any)
	if price["currencyCode"] != "VND" || price["amount"] != 10.0 {
		t.Fatalf("stored price = %v; want the legacy VND 10 verbatim — a same-amount resend was relabelled", price)
	}

	// A changed amount is the user's own edit: written with the configured code.
	w = do(t, r, http.MethodPut, "/api/v1/"+w3Entity+"/"+id, map[string]any{
		"price": map[string]any{"amount": 25.0, "currencyCode": "USD"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("new amount: status=%d — %s", w.Code, w.Body.String())
	}
	price, _ = read()["price"].(map[string]any)
	if price["currencyCode"] != "USD" || price["amount"] != 25.0 {
		t.Fatalf("stored price = %v; want USD 25 (a typed amount is a real edit)", price)
	}
}
