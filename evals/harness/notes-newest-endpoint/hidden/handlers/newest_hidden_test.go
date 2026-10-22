package handlers

// Held out of the run; regrade.sh adds it to the finished workspace. GET /api/v1/:entity/:id/notes/newest answers
// {"data": <the newest live note of the caller's tenant on that record>}, {"data": null} when there is none, and the
// parent record's 404 when the caller cannot see the record. Besides the right note, the table holds, all newer: a
// note of another tenant on the same record id, a note of the same tenant on another record, a soft-deleted note on
// the record. The rows are stored out of order, and the right note has the smallest id. So a query without the
// tenant, the record, `deleted_at IS NULL` or `ORDER BY created_at DESC` returns another note.

import (
	"database/sql/driver"
	"net/http"
	"testing"
	"time"
)

func TestHidden_TheNewestNote(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	traps := func() []map[string]driver.Value {
		otherTenant := hiddenNote("x9", "another workspace", at.Add(4*time.Hour))
		otherTenant["tenant_id"] = "t2"
		otherRecord := hiddenNote("x8", "another record", at.Add(3*time.Hour))
		otherRecord["record_id"] = "r3"
		deleted := hiddenNote("x7", "deleted", at.Add(2*time.Hour))
		deleted["deleted_at"] = at.Add(5 * time.Hour)
		return []map[string]driver.Value{otherTenant, otherRecord, deleted}
	}

	code, body, raw := hiddenGet(t, traps()...)("/api/v1/crm_accounts/r1/notes/newest")
	if code != http.StatusOK {
		t.Fatalf("no live note of this tenant on r1: status %d, want 200: %s", code, raw)
	}
	if v, ok := body["data"]; !ok || v != nil {
		t.Errorf(`no live note of this tenant on r1: want {"data": null}, got %s`, raw)
	}

	rows := append([]map[string]driver.Value{hiddenNote("z9", "older", at)}, traps()...)
	rows = append(rows, hiddenNote("a1", "newest", at.Add(time.Hour)))
	get := hiddenGet(t, rows...)
	code, body, raw = get("/api/v1/crm_accounts/r1/notes/newest")
	if code != http.StatusOK {
		t.Fatalf("two live notes: status %d, want 200: %s", code, raw)
	}
	note, _ := body["data"].(map[string]any)
	if note == nil || note["id"] != "a1" || note["body"] != "newest" {
		t.Errorf(`two live notes: want {"data": {"id": "a1", "body": "newest", …}}, got %s`, raw)
	}

	if code, _, raw = get("/api/v1/crm_accounts/r2/notes/newest"); code != http.StatusNotFound {
		t.Errorf("a record the caller cannot see: status %d, want 404: %s", code, raw)
	}
}
