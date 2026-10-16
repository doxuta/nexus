//go:build integration

// Idempotency-Key on POST /:entity/bulk and /:entity/import — EMPIRICAL acceptance against real
// MySQL 8 (specs/047 FR-12, DAILY-QUEUE #174). Same fixture as idempotency_key_integration_test.go:
// the full stack over the handlers suite's throwaway DB, one fixed principal.
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestIdempotencyKey_(Bulk|Import)
package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

// postImport sends a multipart CSV import as the fixture's principal, with an optional key.
func (f *idemFixture) postImport(t *testing.T, idemKey, csv string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "data.csv")
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := fw.Write([]byte(csv)); err != nil {
		t.Fatalf("write csv: %v", err)
	}
	if err := w.WriteField("mapping", `{"Nội dung":"memo","Số tiền":"amount"}`); err != nil {
		t.Fatalf("mapping: %v", err)
	}
	w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/"+f.entity+"/import", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+f.token)
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	rec := httptest.NewRecorder()
	f.r.ServeHTTP(rec, req)
	return rec
}

// dataOf returns the raw `data` member of a response body.
func dataOf(t *testing.T, w *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || len(env.Data) == 0 {
		t.Fatalf("no data in %d %s", w.Code, w.Body.String())
	}
	return env.Data
}

func TestIdempotencyKey_BulkResendReplaysTheFirstResult(t *testing.T) {
	f := newIdemFixture(t, nil)
	for _, mode := range []string{"atomic", "partial"} {
		t.Run(mode, func(t *testing.T) {
			path := "/api/v1/" + f.entity + "/bulk?mode=" + mode
			key := "bulk-" + mode
			body := map[string]any{"records": []map[string]any{{"memo": "Phiếu 1", "amount": 10}, {"memo": "Phiếu 2", "amount": 20}}}
			before := f.rows(t)

			first := f.post(t, path, key, body)
			if first.Code != http.StatusCreated && first.Code != http.StatusOK {
				t.Fatalf("first bulk = %d %s", first.Code, first.Body.String())
			}
			if f.rows(t) != before+2 {
				t.Fatalf("obj rows = %d, want %d", f.rows(t), before+2)
			}
			// Same batch, other key order / spacing: a replay of the first result, nothing written.
			second := f.post(t, path, key, `{"records":[ {"amount":10.0, "memo":"Phiếu 1"}, {"amount":20,"memo":"Phiếu 2"} ]}`)
			if second.Code != http.StatusOK || second.Header().Get("Idempotent-Replay") != "true" {
				t.Fatalf("resend = %d replay=%q %s, want 200 Idempotent-Replay", second.Code, second.Header().Get("Idempotent-Replay"), second.Body.String())
			}
			if !bytes.Equal(compactJSON(t, dataOf(t, second)), compactJSON(t, dataOf(t, first))) {
				t.Fatalf("replay data = %s, want the first response %s", dataOf(t, second), dataOf(t, first))
			}
			if n := f.rows(t); n != before+2 {
				t.Fatalf("obj rows = %d after a resend, want %d", n, before+2)
			}
			// Another batch under the key is another request.
			other := f.post(t, path, key, map[string]any{"records": []map[string]any{{"memo": "Phiếu 3"}}})
			if other.Code != http.StatusUnprocessableEntity || decodeCode(other.Body.Bytes()) != "IDEMPOTENCY_KEY_REUSED" {
				t.Fatalf("other batch = %d %s, want 422 IDEMPOTENCY_KEY_REUSED", other.Code, other.Body.String())
			}
		})
	}

	// An atomic batch that aborts writes nothing and releases the key: the corrected batch runs.
	path := "/api/v1/" + f.entity + "/bulk"
	bad := f.post(t, path, "bulk-fix", map[string]any{"records": []map[string]any{{"memo": "ok"}, {"amount": 1}}})
	if bad.Code != http.StatusUnprocessableEntity {
		t.Fatalf("aborting batch = %d %s, want 422", bad.Code, bad.Body.String())
	}
	if w := f.post(t, path, "bulk-fix", map[string]any{"records": []map[string]any{{"memo": "ok"}, {"memo": "đã sửa", "amount": 1}}}); w.Code != http.StatusCreated {
		t.Fatalf("corrected batch under the same key = %d %s, want 201", w.Code, w.Body.String())
	}
	if w := f.post(t, path, "bad key", map[string]any{"records": []map[string]any{{"memo": "x"}}}); w.Code != http.StatusBadRequest || decodeCode(w.Body.Bytes()) != "INVALID_IDEMPOTENCY_KEY" {
		t.Fatalf("malformed key = %d %s, want 400 INVALID_IDEMPOTENCY_KEY", w.Code, w.Body.String())
	}
}

func TestIdempotencyKey_ImportResendReplaysTheFirstSummary(t *testing.T) {
	f := newIdemFixture(t, nil)
	csv := "Nội dung,Số tiền\nThu 1,100\nThu 2,200\n"
	first := f.postImport(t, "imp-1", csv)
	if first.Code != http.StatusOK {
		t.Fatalf("first import = %d %s", first.Code, first.Body.String())
	}
	if n := f.rows(t); n != 2 {
		t.Fatalf("obj rows = %d, want 2", n)
	}
	second := f.postImport(t, "imp-1", csv)
	if second.Code != http.StatusOK || second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("resend = %d replay=%q %s, want 200 Idempotent-Replay", second.Code, second.Header().Get("Idempotent-Replay"), second.Body.String())
	}
	if !bytes.Equal(compactJSON(t, dataOf(t, second)), compactJSON(t, dataOf(t, first))) {
		t.Fatalf("replay data = %s, want the first summary %s", dataOf(t, second), dataOf(t, first))
	}
	if n := f.rows(t); n != 2 {
		t.Fatalf("obj rows = %d after a resend, want 2 (the rows were imported twice)", n)
	}
	if w := f.postImport(t, "imp-1", "Nội dung,Số tiền\nThu 3,300\n"); w.Code != http.StatusUnprocessableEntity || decodeCode(w.Body.Bytes()) != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("another file under the key = %d %s, want 422 IDEMPOTENCY_KEY_REUSED", w.Code, w.Body.String())
	}
	if w := f.postImport(t, "khóa", csv); w.Code != http.StatusBadRequest || decodeCode(w.Body.Bytes()) != "INVALID_IDEMPOTENCY_KEY" {
		t.Fatalf("malformed key = %d %s, want 400 INVALID_IDEMPOTENCY_KEY", w.Code, w.Body.String())
	}
	// No header: the old behaviour, every import inserts.
	if w := f.postImport(t, "", csv); w.Code != http.StatusOK {
		t.Fatalf("no-key import = %d %s", w.Code, w.Body.String())
	}
	if n := f.rows(t); n != 4 {
		t.Fatalf("obj rows = %d, want 4", n)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.Bytes()
}
