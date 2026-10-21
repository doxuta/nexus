package notes

// Held out of the run (evals/harness/notes-pinned-field/hidden); regrade.py adds it to the finished workspace.
// A note carries a pinned flag read from its row, and a record's notes list pinned ones first.

import (
	"context"
	"reflect"
	"regexp"
	"testing"
)

func TestHidden_PinnedNotesComeFirst(t *testing.T) {
	db, rec := newFakeDB(t)
	repo := NewRepository(db)
	if _, err := repo.ListByRecord(context.Background(), "tenant-a", "crm_accounts", "rec-1", 20, 0); err != nil {
		t.Fatalf("ListByRecord: %v", err)
	}
	q := rec.lastQuery(t).query
	if !regexp.MustCompile(`(?is)SELECT\b.*\bpinned\b.*\bFROM\b`).MatchString(q) {
		t.Errorf("ListByRecord does not read pinned: %q", q)
	}
	if !regexp.MustCompile(`(?i)ORDER BY\s+pinned\s+DESC`).MatchString(q) {
		t.Errorf("ListByRecord does not list pinned notes first: %q", q)
	}
	found := false
	for _, f := range reflect.VisibleFields(reflect.TypeOf(Note{})) {
		if f.Tag.Get("db") == "pinned" && f.Type.Kind() == reflect.Bool {
			found = true
		}
	}
	if !found {
		t.Error(`Note has no bool field tagged db:"pinned"`)
	}
}
