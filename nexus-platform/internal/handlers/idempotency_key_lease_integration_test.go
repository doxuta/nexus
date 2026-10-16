//go:build integration

// Lease for the Idempotency-Key of an atomic bulk and an import — EMPIRICAL acceptance against real
// MySQL 8 (specs/047 FR-13, DAILY-QUEUE #355). Same fixture as idempotency_key_integration_test.go.
// "The process died mid-batch" is the state it leaves: the claim in processing, aged past the lease,
// with or without the batch's rows in obj_*.
//
// Run with: go test -tags=integration ./internal/handlers/ -race -run TestIdempotencyKey_Lease
package handlers

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// leaseClaim reads the claim of key: status, base id and whether a response is stored.
func (f *idemFixture) leaseClaim(t *testing.T, key string) (status, baseID string, hasResponse bool) {
	t.Helper()
	var row struct {
		Status   string  `db:"status"`
		RecordID *string `db:"record_id"`
		Response []byte  `db:"response"`
	}
	if err := f.sx.Get(&row, `SELECT status, record_id, response FROM sys_idempotency_keys WHERE tenant_id = ? AND principal_id = ? AND idem_key = ?`,
		f.tenant, f.principal, key); err != nil {
		t.Fatalf("read claim %s: %v", key, err)
	}
	if row.RecordID != nil {
		baseID = *row.RecordID
	}
	return row.Status, baseID, len(row.Response) > 0
}

// crash puts the claim back as a dead process leaves it: processing, written `ago` ago, naming
// baseID (the batch's base id; "" keeps the one it has). keepResponse keeps the saved plan.
func (f *idemFixture) crash(t *testing.T, key string, ago time.Duration, baseID string, keepResponse bool) {
	t.Helper()
	q := `UPDATE sys_idempotency_keys SET status = 'processing', expires_at = ?`
	args := []any{time.Now().Add(-ago).Add(24 * time.Hour).UTC()}
	if baseID != "" {
		q += `, record_id = ?`
		args = append(args, baseID)
	}
	if !keepResponse {
		q += `, response = NULL`
	}
	q += ` WHERE tenant_id = ? AND principal_id = ? AND idem_key = ?`
	args = append(args, f.tenant, f.principal, key)
	if _, err := f.sx.Exec(q, args...); err != nil {
		t.Fatalf("crash claim: %v", err)
	}
}

func (f *idemFixture) rowExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := f.sx.Get(&n, "SELECT COUNT(*) FROM `obj_"+f.entity+"` WHERE tenant_id = ? AND id = ? AND deleted_at IS NULL", f.tenant, id); err != nil {
		t.Fatalf("probe row %s: %v", id, err)
	}
	return n > 0
}

func TestIdempotencyKey_LeaseAtomicBulk(t *testing.T) {
	f := newIdemFixture(t, nil)
	path := "/api/v1/" + f.entity + "/bulk?mode=atomic"
	batch := map[string]any{"records": []map[string]any{{"memo": "Phiếu 1", "amount": 10}, {"memo": "Phiếu 2", "amount": 20}, {"memo": "Phiếu 3"}}}

	first := f.post(t, path, "lease-bulk", batch)
	if first.Code != http.StatusCreated {
		t.Fatalf("first bulk = %d %s", first.Code, first.Body.String())
	}
	_, base, _ := f.leaseClaim(t, "lease-bulk")
	if base == "" || !f.rowExists(t, base) {
		t.Fatalf("claim base id %q is not row 0 of the batch in obj_*", base)
	}

	// The batch committed; only the completion was lost. Inside the lease: still the first's.
	f.crash(t, "lease-bulk", time.Minute, "", false)
	if w := f.post(t, path, "lease-bulk", batch); w.Code != http.StatusConflict || decodeCode(w.Body.Bytes()) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("resend inside the lease = %d %s, want 409 IDEMPOTENCY_IN_PROGRESS", w.Code, w.Body.String())
	}
	// Past the lease: the rebuilt response is the first response, byte for byte, and nothing is written.
	f.crash(t, "lease-bulk", 4*time.Minute, "", false)
	w := f.post(t, path, "lease-bulk", batch)
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("resend past the lease = %d %s, want 200 Idempotent-Replay", w.Code, w.Body.String())
	}
	if !bytes.Equal(compactJSON(t, dataOf(t, w)), compactJSON(t, dataOf(t, first))) {
		t.Fatalf("rebuilt response = %s, want the first response %s", dataOf(t, w), dataOf(t, first))
	}
	if n := f.rows(t); n != 3 {
		t.Fatalf("obj rows = %d, want 3 (the batch was written twice)", n)
	}
	if status, _, has := f.leaseClaim(t, "lease-bulk"); status != "completed" || !has {
		t.Fatalf("reconciled claim = (%s, response %v), want completed with the response", status, has)
	}

	// The process died before the batch's COMMIT: no row under the claim's base id ⇒ take over.
	if w := f.post(t, path, "lease-bulk-2", batch); w.Code != http.StatusOK && w.Code != http.StatusCreated {
		// a different key is another request: the same rows are simply created again
		t.Fatalf("second key = %d %s", w.Code, w.Body.String())
	}
	ghost := uuid.NewString()
	f.crash(t, "lease-bulk-2", 4*time.Minute, ghost, false)
	rowsBefore := f.rows(t)
	w = f.post(t, path, "lease-bulk-2", batch)
	if w.Code != http.StatusCreated {
		t.Fatalf("resend on a batch that never committed = %d %s, want 201", w.Code, w.Body.String())
	}
	if n := f.rows(t); n != rowsBefore+3 {
		t.Fatalf("obj rows = %d, want %d (take-over writes the batch once)", n, rowsBefore+3)
	}
	if _, nb, _ := f.leaseClaim(t, "lease-bulk-2"); nb == ghost || !f.rowExists(t, nb) {
		t.Fatalf("taken-over claim base id %q: ghost=%v exists=%v", nb, nb == ghost, f.rowExists(t, nb))
	}
}

func TestIdempotencyKey_LeaseImport(t *testing.T) {
	f := newIdemFixture(t, nil)
	// Row 3 has no required "Nội dung": refused, so the summary carries a per-row error that the
	// database cannot give back once the valid rows are in.
	csv := "Nội dung,Số tiền\nThu 1,100\n,5\nThu 2,200\n"

	first := f.postImport(t, "lease-imp", csv)
	if first.Code != http.StatusOK {
		t.Fatalf("first import = %d %s", first.Code, first.Body.String())
	}
	status, base, hasPlan := f.leaseClaim(t, "lease-imp")
	if status != "completed" || base == "" || !hasPlan || !f.rowExists(t, base) {
		t.Fatalf("claim = (%s, base %q, response %v, row0 exists %v), want completed with a base id that is row 0", status, base, hasPlan, f.rowExists(t, base))
	}

	// The import committed; the process died before completing the claim — the saved plan survives.
	f.crash(t, "lease-imp", time.Minute, "", true)
	if w := f.postImport(t, "lease-imp", csv); w.Code != http.StatusConflict || decodeCode(w.Body.Bytes()) != "IDEMPOTENCY_IN_PROGRESS" {
		t.Fatalf("resend inside the lease = %d %s, want 409 IDEMPOTENCY_IN_PROGRESS", w.Code, w.Body.String())
	}
	f.crash(t, "lease-imp", 4*time.Minute, "", true)
	w := f.postImport(t, "lease-imp", csv)
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("resend past the lease = %d %s, want 200 Idempotent-Replay", w.Code, w.Body.String())
	}
	if !bytes.Equal(compactJSON(t, dataOf(t, w)), compactJSON(t, dataOf(t, first))) {
		t.Fatalf("replayed summary = %s, want the first summary %s", dataOf(t, w), dataOf(t, first))
	}
	if n := f.rows(t); n != 2 {
		t.Fatalf("obj rows = %d, want 2 (the file was imported twice)", n)
	}

	// The process died before the COMMIT: the plan was saved, no row exists under the base id.
	if w := f.postImport(t, "lease-imp-2", csv); w.Code != http.StatusOK {
		t.Fatalf("second key = %d %s", w.Code, w.Body.String())
	}
	f.crash(t, "lease-imp-2", 4*time.Minute, uuid.NewString(), true)
	before := f.rows(t)
	w = f.postImport(t, "lease-imp-2", csv)
	if w.Code != http.StatusOK || w.Header().Get("Idempotent-Replay") == "true" {
		t.Fatalf("resend on an import that never committed = %d replay=%q %s, want a fresh import", w.Code, w.Header().Get("Idempotent-Replay"), w.Body.String())
	}
	if n := f.rows(t); n != before+2 {
		t.Fatalf("obj rows = %d, want %d (take-over imports the file once)", n, before+2)
	}
}
