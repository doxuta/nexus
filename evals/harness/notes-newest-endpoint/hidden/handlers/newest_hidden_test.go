package handlers

// Held out of the run; regrade.sh adds it to the finished workspace. GET /api/v1/:entity/:id/notes/newest answers
// {"data": <the newest note of the caller's tenant on that record>}, {"data": null} when there is none, and the
// parent record's 404 when the caller cannot see the record. The table holds a newer note of another tenant on the
// same record id, and stores the rows out of order, so a query without the tenant or without the order fails.

import (
	"net/http"
	"testing"
	"time"
)

func TestHidden_TheNewestNote(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	other := hiddenNote("x9", "another workspace", at.Add(2*time.Hour))
	other["tenant_id"] = "t2"

	code, body, raw := hiddenGet(t, other)("/api/v1/crm_accounts/r1/notes/newest")
	if code != http.StatusOK {
		t.Fatalf("no notes of this tenant: status %d, want 200: %s", code, raw)
	}
	if v, ok := body["data"]; !ok || v != nil {
		t.Errorf(`no notes of this tenant: want {"data": null}, got %s`, raw)
	}

	get := hiddenGet(t, hiddenNote("n1", "older", at), other, hiddenNote("n2", "newer", at.Add(time.Hour)))
	code, body, raw = get("/api/v1/crm_accounts/r1/notes/newest")
	if code != http.StatusOK {
		t.Fatalf("two notes: status %d, want 200: %s", code, raw)
	}
	note, _ := body["data"].(map[string]any)
	if note == nil || note["id"] != "n2" || note["body"] != "newer" {
		t.Errorf(`two notes: want {"data": {"id": "n2", "body": "newer", …}}, got %s`, raw)
	}

	if code, _, raw = get("/api/v1/crm_accounts/r2/notes/newest"); code != http.StatusNotFound {
		t.Errorf("a record the caller cannot see: status %d, want 404: %s", code, raw)
	}
}
