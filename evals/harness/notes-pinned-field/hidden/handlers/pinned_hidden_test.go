package handlers

// Held out of the run; regrade.sh adds it to the finished workspace. GET /:entity/:id/notes returns each note's
// pinned flag: a field declared on the response but never set fails here.

import (
	"net/http"
	"testing"
	"time"
)

func TestHidden_TheNotesListReturnsPinned(t *testing.T) {
	row := hiddenNote("n1", "pinned", time.Unix(1700000000, 0).UTC())
	row["pinned"] = true
	code, body, raw := hiddenGet(t, row)("/api/v1/crm_accounts/r1/notes")
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, raw)
	}
	items, _ := body["data"].([]any)
	if len(items) != 1 {
		t.Fatalf("want one note, got %s", raw)
	}
	if note, _ := items[0].(map[string]any); note["pinned"] != true {
		t.Errorf(`want "pinned": true on a pinned note, got %s`, raw)
	}
}
