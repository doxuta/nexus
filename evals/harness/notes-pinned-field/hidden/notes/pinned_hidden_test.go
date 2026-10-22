package notes

// Held out of the run; regrade.sh adds it to the finished workspace. A note carries a pinned flag read from its
// row, and a record's notes list pinned ones first: an older pinned note comes before a newer unpinned one.

import (
	"context"
	"reflect"
	"regexp"
	"testing"
	"time"
)

func TestHidden_PinnedIsReadAndListedFirst(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	pinnedRow := hiddenNote("p1", "pinned, older", at)
	pinnedRow["pinned"] = true
	db, s := newHiddenDB(t, hiddenNote("u1", "unpinned, newer", at.Add(time.Hour)), pinnedRow)
	got, err := NewRepository(db).ListByRecord(context.Background(), "t1", "crm_accounts", "r1", 20, 0)
	if err != nil {
		t.Fatalf("ListByRecord: %v", err)
	}
	q := hiddenStrip(s.lastCall(t).query)
	if !regexp.MustCompile(`(?is)SELECT\b.*\b(?:\w+\.)?pinned\b.*\bFROM\b`).MatchString(q) {
		t.Errorf("ListByRecord does not read pinned (comments removed): %q", q)
	}
	if !regexp.MustCompile(`(?i)ORDER BY\s+(?:\w+\.)?pinned\s+DESC`).MatchString(q) {
		t.Errorf("ListByRecord does not order by pinned first (comments removed): %q", q)
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
	if len(got) != 2 || got[0].ID != "p1" || !reflect.ValueOf(got[0]).FieldByIndex(pinned.Index).Bool() {
		t.Errorf("want the older pinned note first, pinned, then the newer one: %+v", got)
	}
}
