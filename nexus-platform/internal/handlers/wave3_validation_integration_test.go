//go:build integration

package handlers

// Wave 3 / Validation-completeness integration tests (RM-29 through RM-33).
// These tests use a real MySQL database (nexus-mysql container, throwaway DB migrated to
// head) and verify the full HTTP stack: create entity + fields → POST/PUT records that
// violate each Wave 3 rule → assert correct HTTP status + error code + details.field.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

const w3Entity = "it_wave3"

func cleanupW3(t *testing.T) {
	t.Helper()
	_, sx := newFullRouter(t)
	cleanupNamed(sx, w3Entity)
}

// bodyCode unmarshals a response body and returns the "code" field.
func bodyCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	s, _ := m["code"].(string)
	return s
}

// bodyDetails unmarshals a response body and returns the "details" map.
func bodyDetails(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	d, _ := m["details"].(map[string]any)
	return d
}

// setupW3Entity creates a test entity with fields that exercise all Wave 3 rules and
// returns the router for further calls. The entity is cleaned up via t.Cleanup.
func setupW3Entity(t *testing.T) *gin.Engine {
	t.Helper()
	r, sx := newFullRouter(t)
	cleanupNamed(sx, w3Entity)
	t.Cleanup(func() { cleanupNamed(sx, w3Entity) })

	body := map[string]any{
		"name":  w3Entity,
		"label": "Wave3 Test",
		"fields": []map[string]any{
			// RM-29 number with range + step
			{
				"name": "score", "label": "Score", "fieldType": "number",
				"options": map[string]any{"min": float64(0), "max": float64(100), "step": float64(1)},
			},
			// RM-29 text with maxLength + pattern
			{
				"name": "code", "label": "Code", "fieldType": "text",
				"options": map[string]any{"maxLength": float64(5), "pattern": `[A-Z]+`},
			},
			// RM-30 unique field → creates idx_obj_it_wave3_email index (non-unique post RM-28 Slice B; enforced by the sys_unique_index pivot)
			{
				"name": "email", "label": "Email", "fieldType": "email",
				"isUnique": true,
			},
			// RM-31 currency with precision=2 — NO min: negatives allowed by default (refund/credit).
			{
				"name": "price", "label": "Price", "fieldType": "currency",
				"options": map[string]any{"currencyCode": "USD", "precision": float64(2)},
			},
			// RM-31 (revised) currency with opt-in floor min:0 — proves options.min survives the
			// metadata round-trip (sys_fields JSON → runtime validation) and forbids negatives.
			{
				"name": "nnprice", "label": "Non-negative Price", "fieldType": "currency",
				"options": map[string]any{"currencyCode": "USD", "precision": float64(2), "min": float64(0)},
			},
			// RM-32 composite with required sub-field
			{
				"name": "fullname", "label": "Full Name", "fieldType": "composite",
				"options": map[string]any{
					"subFields": []map[string]any{
						{"name": "first", "fieldType": "text", "required": true},
						{"name": "last", "fieldType": "text"},
					},
				},
			},
		},
	}
	w := do(t, r, http.MethodPost, "/api/v1/admin/entities", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("setupW3Entity: %d %s", w.Code, w.Body.String())
	}
	return r
}

// ── RM-29: number range ───────────────────────────────────────────────────────

func TestRM29_NumberRange_Integration(t *testing.T) {
	r := setupW3Entity(t)
	base := map[string]any{
		"email":    "rm29num@example.com",
		"fullname": map[string]any{"first": "A"},
	}

	cases := []struct {
		name     string
		score    any
		wantCode int
		wantErr  string
	}{
		{"valid: score=50", float64(50), http.StatusCreated, ""},
		{"below min: score=-1", float64(-1), http.StatusUnprocessableEntity, "RECORD_INVALID_RANGE"},
		{"above max: score=101", float64(101), http.StatusUnprocessableEntity, "RECORD_INVALID_RANGE"},
		{"fractional step: score=1.5", 1.5, http.StatusUnprocessableEntity, "RECORD_INVALID_RANGE"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := copyMap(base)
			payload["score"] = tc.score
			// use unique email per sub-test
			payload["email"] = "rm29num" + string(rune('a'+i)) + "@example.com"
			w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, payload)
			if w.Code != tc.wantCode {
				t.Fatalf("status=%d; want %d — %s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr != "" {
				if got := bodyCode(t, w); got != tc.wantErr {
					t.Fatalf("code=%q; want %q — %s", got, tc.wantErr, w.Body.String())
				}
			}
		})
	}
}

// ── RM-29: text maxLength + pattern ─────────────────────────────────────────

func TestRM29_TextConstraints_Integration(t *testing.T) {
	r := setupW3Entity(t)
	base := map[string]any{
		"fullname": map[string]any{"first": "A"},
	}

	cases := []struct {
		name     string
		email    string
		code     string
		wantCode int
		wantErr  string
	}{
		{"valid: code=ABC", "rm29txt1@example.com", "ABC", http.StatusCreated, ""},
		{"too long: code=ABCDEF (6 chars > maxLength 5)", "rm29txt2@example.com", "ABCDEF", http.StatusUnprocessableEntity, "RECORD_TOO_LONG"},
		{"pattern mismatch: code=abc (lowercase)", "rm29txt3@example.com", "abc", http.StatusUnprocessableEntity, "RECORD_INVALID_PATTERN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := copyMap(base)
			payload["email"] = tc.email
			if tc.code != "" {
				payload["code"] = tc.code
			}
			w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, payload)
			if w.Code != tc.wantCode {
				t.Fatalf("status=%d; want %d — %s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr != "" {
				if got := bodyCode(t, w); got != tc.wantErr {
					t.Fatalf("code=%q; want %q — %s", got, tc.wantErr, w.Body.String())
				}
			}
		})
	}
}

// ── RM-30: RECORD_DUPLICATE details.field ────────────────────────────────────

func TestRM30_DuplicateFieldName_Integration(t *testing.T) {
	r := setupW3Entity(t)
	base := map[string]any{
		"email":    "dup@example.com",
		"fullname": map[string]any{"first": "A"},
	}

	// First insert: must succeed.
	if w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, base); w.Code != http.StatusCreated {
		t.Fatalf("first insert: %d %s", w.Code, w.Body.String())
	}

	// Second insert with same email: must return 409 RECORD_DUPLICATE with details.field=email.
	w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, base)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate insert status=%d; want 409 — %s", w.Code, w.Body.String())
	}
	if got := bodyCode(t, w); got != "RECORD_DUPLICATE" {
		t.Fatalf("code=%q; want RECORD_DUPLICATE — %s", got, w.Body.String())
	}
	details := bodyDetails(t, w)
	if details["field"] != "email" {
		t.Fatalf("details.field=%q; want %q — %s", details["field"], "email", w.Body.String())
	}
}

// ── RM-31: currency precision + non-negative ─────────────────────────────────

func TestRM31_CurrencyPrecision_Integration(t *testing.T) {
	r := setupW3Entity(t)
	base := map[string]any{
		"fullname": map[string]any{"first": "A"},
	}

	cases := []struct {
		name     string
		email    string
		field    string // which currency field the amount goes into (price = no min, nnprice = min:0)
		price    map[string]any
		wantCode int
		wantErr  string
	}{
		{
			"valid: 19.99 USD", "rm31a@example.com", "price",
			map[string]any{"amount": 19.99, "currencyCode": "USD"},
			http.StatusCreated, "",
		},
		{
			// RM-31 (revised): negative on a field with no min is a valid refund/credit → accepted.
			"negative amount, no min: accepted (refund/credit)", "rm31b@example.com", "price",
			map[string]any{"amount": -1.0, "currencyCode": "USD"},
			http.StatusCreated, "",
		},
		{
			// RM-31 (revised): the opt-in floor min:0 (carried through metadata) forbids negatives.
			"negative amount, field min:0: rejected", "rm31bn@example.com", "nnprice",
			map[string]any{"amount": -1.0, "currencyCode": "USD"},
			http.StatusUnprocessableEntity, "RECORD_INVALID_RANGE",
		},
		{
			"exceeds precision=2: 1.999", "rm31c@example.com", "price",
			map[string]any{"amount": 1.999, "currencyCode": "USD"},
			http.StatusUnprocessableEntity, "RECORD_INVALID_PRECISION",
		},
		{
			"zero precision: 5.0 OK", "rm31d@example.com", "price",
			// price field has precision=2, so 5.00 is fine
			map[string]any{"amount": 5.00, "currencyCode": "USD"},
			http.StatusCreated, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := copyMap(base)
			payload["email"] = tc.email
			field := tc.field
			if field == "" {
				field = "price"
			}
			payload[field] = tc.price
			w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, payload)
			if w.Code != tc.wantCode {
				t.Fatalf("status=%d; want %d — %s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr != "" {
				if got := bodyCode(t, w); got != tc.wantErr {
					t.Fatalf("code=%q; want %q — %s", got, tc.wantErr, w.Body.String())
				}
			}
		})
	}
}

// ── Queue #7 (Q7): currency code must match the field's configured code ──────

// TestQ7_CurrencyCodeMismatch_Integration proves the guard on the DIRECT write paths through the
// full HTTP stack with the field's options round-tripped through sys_fields: `price` is
// configured USD, so a VND row is rejected on POST (create) and on PUT (update) with 422
// RECORD_CURRENCY_MISMATCH + details.field, while a USD row is created and the stored code is
// the one the client sent (never relabelled).
func TestQ7_CurrencyCodeMismatch_Integration(t *testing.T) {
	r := setupW3Entity(t)

	// Create: VND into a USD field → 422, field named.
	w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, map[string]any{
		"email":    "q7a@example.com",
		"fullname": map[string]any{"first": "A"},
		"price":    map[string]any{"amount": 10.0, "currencyCode": "VND"},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create mismatch: status=%d; want 422 — %s", w.Code, w.Body.String())
	}
	if got := bodyCode(t, w); got != "RECORD_CURRENCY_MISMATCH" {
		t.Fatalf("create mismatch: code=%q; want RECORD_CURRENCY_MISMATCH — %s", got, w.Body.String())
	}
	if d := bodyDetails(t, w); d["field"] != "price" {
		t.Fatalf("create mismatch: details.field=%v; want price — %s", d["field"], w.Body.String())
	}

	// Create: USD into the USD field → 201, stored verbatim.
	w = do(t, r, http.MethodPost, "/api/v1/"+w3Entity, map[string]any{
		"email":    "q7b@example.com",
		"fullname": map[string]any{"first": "B"},
		"price":    map[string]any{"amount": 10.0, "currencyCode": "USD"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create match: status=%d; want 201 — %s", w.Code, w.Body.String())
	}
	var created struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("parse create response: %v — %s", err, w.Body.String())
	}
	id, _ := created.Data["id"].(string)
	if id == "" {
		t.Fatalf("create match: no id in response — %s", w.Body.String())
	}

	// Update: VND into the USD field → 422 through ValidatePartialRecord (same shared guard).
	w = do(t, r, http.MethodPut, "/api/v1/"+w3Entity+"/"+id, map[string]any{
		"price": map[string]any{"amount": 5.0, "currencyCode": "VND"},
	})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("update mismatch: status=%d; want 422 — %s", w.Code, w.Body.String())
	}
	if got := bodyCode(t, w); got != "RECORD_CURRENCY_MISMATCH" {
		t.Fatalf("update mismatch: code=%q; want RECORD_CURRENCY_MISMATCH — %s", got, w.Body.String())
	}

	// Read back: the stored code is still USD (no rewrite on either path).
	w = do(t, r, http.MethodGet, "/api/v1/"+w3Entity+"/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get: status=%d — %s", w.Code, w.Body.String())
	}
	var got struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	price, _ := got.Data.Data["price"].(map[string]any)
	if price["currencyCode"] != "USD" || price["amount"] != 10.0 {
		t.Fatalf("stored price = %v; want USD 10 verbatim — %s", price, w.Body.String())
	}
}

// ── RM-32: composite sub-field required ──────────────────────────────────────

func TestRM32_CompositeSubRequired_Integration(t *testing.T) {
	r := setupW3Entity(t)

	cases := []struct {
		name      string
		email     string
		fullname  map[string]any
		wantCode  int
		wantErr   string
		wantField string
	}{
		{
			"valid: first present", "rm32a@example.com",
			map[string]any{"first": "Alice", "last": "Smith"},
			http.StatusCreated, "", "",
		},
		{
			"required first absent", "rm32b@example.com",
			map[string]any{"last": "Smith"},
			http.StatusUnprocessableEntity, "RECORD_REQUIRED_MISSING", "fullname.first",
		},
		{
			"required first empty", "rm32c@example.com",
			map[string]any{"first": "  ", "last": "Smith"},
			http.StatusUnprocessableEntity, "RECORD_REQUIRED_MISSING", "fullname.first",
		},
		{
			"optional last absent: OK", "rm32d@example.com",
			map[string]any{"first": "Bob"},
			http.StatusCreated, "", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"email":    tc.email,
				"fullname": tc.fullname,
			}
			w := do(t, r, http.MethodPost, "/api/v1/"+w3Entity, payload)
			if w.Code != tc.wantCode {
				t.Fatalf("status=%d; want %d — %s", w.Code, tc.wantCode, w.Body.String())
			}
			if tc.wantErr != "" {
				if got := bodyCode(t, w); got != tc.wantErr {
					t.Fatalf("code=%q; want %q — %s", got, tc.wantErr, w.Body.String())
				}
			}
			if tc.wantField != "" {
				details := bodyDetails(t, w)
				if details["field"] != tc.wantField {
					t.Fatalf("details.field=%q; want %q — %s", details["field"], tc.wantField, w.Body.String())
				}
			}
		})
	}
}

// ── RM-33: currency default rejected at EditField time ───────────────────────

func TestRM33_CurrencyDefaultRejected_Integration(t *testing.T) {
	r, sx := newFullRouter(t)
	entity := "it_wave3_rm33"
	cleanupNamed(sx, entity)
	t.Cleanup(func() { cleanupNamed(sx, entity) })

	// Create entity with a currency field.
	if w := do(t, r, http.MethodPost, "/api/v1/admin/entities", map[string]any{
		"name":  entity,
		"label": "RM33 Test",
		"fields": []map[string]any{
			{
				"name": "amount", "label": "Amount", "fieldType": "currency",
				"options": map[string]any{"currencyCode": "USD"},
			},
		},
	}); w.Code != http.StatusCreated {
		t.Fatalf("create entity: %d %s", w.Code, w.Body.String())
	}

	// Resolve the field id directly from sys_fields (no GET entity-by-name admin route).
	var fieldID string
	if err := sx.Get(&fieldID, `SELECT f.id FROM sys_fields f
		JOIN sys_entities e ON e.id = f.entity_id
		WHERE e.tenant_id = ? AND e.name = ? AND f.name = ? AND f.deleted_at IS NULL`,
		testTenant, entity, "amount"); err != nil {
		t.Fatalf("resolve field id: %v", err)
	}

	// Attempt to set a default value on the currency field — must be rejected (RM-33a).
	editBody := map[string]any{"defaultValue": "100"}
	w := do(t, r, http.MethodPut, "/api/v1/admin/fields/"+fieldID, editBody)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("edit currency default status=%d; want 422 — %s", w.Code, w.Body.String())
	}
	if got := bodyCode(t, w); got != "FIELD_DEFAULT_NOT_SUPPORTED" {
		t.Fatalf("code=%q; want FIELD_DEFAULT_NOT_SUPPORTED — %s", got, w.Body.String())
	}
}

// copyMap makes a shallow copy of a map[string]any so sub-tests don't share state.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
