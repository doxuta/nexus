package handlers

// Held out of the run; regrade.sh adds it to the finished workspace. GET /api/v1/:entity/:id/notes/newest
// answers {"data": null} for a record with no notes, and {"data": <the newest note>} otherwise.

import (
	"net/http"
	"testing"
	"time"
)

func TestHidden_TheNewestNote(t *testing.T) {
	code, body, raw := hiddenGet(t)("/api/v1/crm_accounts/r1/notes/newest")
	if code != http.StatusOK {
		t.Fatalf("no notes: status %d, want 200: %s", code, raw)
	}
	if v, ok := body["data"]; !ok || v != nil {
		t.Errorf(`no notes: want {"data": null}, got %s`, raw)
	}

	at := time.Unix(1700000000, 0).UTC()
	code, body, raw = hiddenGet(t, hiddenNote("n2", "newer", at.Add(time.Hour)), hiddenNote("n1", "older", at))("/api/v1/crm_accounts/r1/notes/newest")
	if code != http.StatusOK {
		t.Fatalf("two notes: status %d, want 200: %s", code, raw)
	}
	note, _ := body["data"].(map[string]any)
	if note == nil || note["id"] != "n2" || note["body"] != "newer" {
		t.Errorf(`two notes: want {"data": {"id": "n2", "body": "newer", …}}, got %s`, raw)
	}
}
