package notes

// Held out of the run; regrade.sh adds it to the finished workspace. A note carries a pinned flag read from its
// row, and a record's notes list pinned ones first.

import (
	"context"
	"reflect"
	"regexp"
	"testing"
	"time"
)

func TestHidden_PinnedIsReadAndListedFirst(t *testing.T) {
	row := hiddenNote("n1", "pinned", time.Unix(1700000000, 0).UTC())
	row["pinned"] = true
	db, s := newHiddenDB(t, row)
	got, err := NewRepository(db).ListByRecord(context.Background(), "t1", "crm_accounts", "r1", 20, 0)
	if err != nil {
		t.Fatalf("ListByRecord: %v", err)
	}
	q := s.lastCall(t).query
	if !regexp.MustCompile(`(?is)SELECT\b.*\bpinned\b.*\bFROM\b`).MatchString(q) {
		t.Errorf("ListByRecord does not read pinned: %q", q)
	}
	if !regexp.MustCompile(`(?i)ORDER BY\s+pinned\s+DESC`).MatchString(q) {
		t.Errorf("ListByRecord does not list pinned notes first: %q", q)
	}
	var pinned *reflect.StructField
	for _, f := range reflect.VisibleFields(reflect.TypeOf(Note{})) {
		if f.Tag.Get("db") == "pinned" && f.Type.Kind() == reflect.Bool {
			pinned = &f
		}
	}
	if pinned == nil {
		t.Fatal(`Note has no bool field tagged db:"pinned"`)
	}
	if len(got) != 1 || !reflect.ValueOf(got[0]).FieldByIndex(pinned.Index).Bool() {
		t.Errorf("a pinned row comes back unpinned: %+v", got)
	}
}
