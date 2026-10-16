package services

// RM-42 / ACTIVITY-CORE #3a parity for the EXPORT path, plus the CSV header guard.
//
// ExportCSV was the one filter-accepting read path that handed AssertFieldsVisible the RAW filter
// key while List and Aggregate mapped it through polymorphicGuardField first. That mattered
// because resolveFiltersForAggregate — the body Export itself calls — deliberately ACCEPTS
// `<poly>_type` / `<poly>_id`, while FieldACLService.AssertFieldsVisible only rejects names in the
// `hidden` set, which is keyed by real sys_fields rows. A generated sub-column is never one, so a
// role with can_view=0 on a polymorphic field could export FILTERED by exactly that field: the
// per-page strip blanked the cell, but which ROWS came back reconstructed the hidden association.
//
// Reuses the rj* fakes from report_join_test.go and polyACLService from
// polymorphic_field_acl_test.go (same package).

import (
	"bytes"
	"context"
	"encoding/csv"
	"strings"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// exportStore answers the export's cursor walk with one empty page. Everything else falls through
// to rjStore, whose methods panic — so a test that reaches an unexpected store call says so loudly.
type exportStore struct {
	*rjStore
	cursorCalled bool
}

func (s *exportStore) ListRecordsCursor(context.Context, string, string, dynrepo.RowScope, dynrepo.CursorParams) ([]dynrepo.Record, string, error) {
	s.cursorCalled = true
	return nil, "", nil
}

func TestExportCSV_PolymorphicSubColumnFilter_FieldACL(t *testing.T) {
	ctx := context.Background()
	scope := RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all"}
	// The self-service shape: dynamic_record_handler builds export filters with
	// parseQueryFilters(c, nil) — "every query param is a record filter" — so any user with `view`
	// on the entity can supply `?linked_id[eq]=<uuid>`.
	opts := ExportCSVOptions{Filters: []FilterInput{{Field: "linked_id", Values: []string{"acct-1"}}}}

	t.Run("hidden polymorphic field → export filtered by its sub-column is FIELD_FORBIDDEN", func(t *testing.T) {
		store := &exportStore{rjStore: &rjStore{}}
		fACL := &rjFieldACL{hidden: map[string]bool{"events.linked": true}}
		var buf bytes.Buffer

		err := polyACLService(store, fACL).ExportCSV(ctx, scope, "t1", "events", opts, &buf)
		if codeOf(err) != "FIELD_FORBIDDEN" {
			t.Fatalf("code = %q, want FIELD_FORBIDDEN (err=%v)", codeOf(err), err)
		}
		if store.cursorCalled {
			t.Fatal("store was read after the field-ACL rejection — rows must never be fetched")
		}
		if buf.Len() != 0 {
			t.Fatalf("bytes were written before the rejection: %q", buf.String())
		}
		// Proves the guard asked about the BASE field, which is the half that was missing.
		var askedBase bool
		for _, k := range fACL.asked {
			if k == "events.linked" {
				askedBase = true
			}
			if k == "events.linked_id" {
				t.Fatal("field-ACL asked about the raw sub-column 'events.linked_id' — the guard mapping is gone")
			}
		}
		if !askedBase {
			t.Fatalf("field-ACL was never asked about events.linked; asked=%v", fACL.asked)
		}
	})

	t.Run("visible polymorphic field → export proceeds", func(t *testing.T) {
		store := &exportStore{rjStore: &rjStore{}}
		var buf bytes.Buffer
		if err := polyACLService(store, &rjFieldACL{hidden: map[string]bool{}}).
			ExportCSV(ctx, scope, "t1", "events", opts, &buf); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !store.cursorCalled {
			t.Fatal("the export never read the store once field-ACL passed")
		}
	})
}

// Every data cell goes through csvGuard (csvCell), but the header row — written first — did not.
// Declared field names cannot start with a formula character (ddl.SanitizeIdentifier), so the
// exposed path is a report's `config.columns`: validateReportConfig only trims blanks and
// deliberately defers field-existence to run time, so an admin-authored report puts arbitrary text
// into row 1 of a file a colleague opens in Excel.
func TestExportCSV_HeaderRowIsGuarded(t *testing.T) {
	store := &exportStore{rjStore: &rjStore{}}
	cat := &rjCatalog{
		entities: map[string]*metadata.EntityDefinition{"events": ent("events")},
		fields:   map[string][]metadata.FieldDefinition{"events": {{Name: "title", FieldType: "text"}}},
	}
	var buf bytes.Buffer
	err := NewRecordService(cat, store).ExportCSV(
		context.Background(),
		RecordScope{UserID: "u1", RoleID: "role1", AccessLevel: "all", IsAdmin: true},
		"t1", "events",
		// A tabular report's config.columns, verbatim.
		ExportCSVOptions{Columns: []string{"=cmd|'/c calc.exe'!A1", "title"}},
		&buf,
	)
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil || len(rows) == 0 {
		t.Fatalf("parse CSV: %v (out=%q)", err, buf.String())
	}
	header := rows[0]
	// A PROJECTED export (opts.Columns set) carries no leading "id" column. That was decided after
	// this branch forked, by the Excel-ready export slice: a projection IS the on-screen column
	// set, and a report handed to a manager must not open on a column of 36-char UUIDs the screen
	// never showed. The security property this test exists for is unaffected — it is about what
	// happens to a hostile column NAME, not about how many columns there are.
	if len(header) != 2 {
		t.Fatalf("header = %q; want 2 columns (projected export has no id column)", header)
	}
	// The first cell carries the UTF-8 BOM that makes Excel read the file as UTF-8; strip it
	// before comparing, or this assertion silently tests the BOM instead of the guard.
	got := strings.TrimPrefix(header[0], "\ufeff")
	if got != "'=cmd|'/c calc.exe'!A1" {
		t.Errorf("header[0] = %q; want the csvGuard \"'\" prefix so the spreadsheet renders it as text, not a formula", got)
	}
	if header[1] != "title" {
		t.Errorf("header[1] = %q; want an ordinary name to pass through untouched", header[1])
	}
}
