package services

import (
	"context"
	"encoding/csv"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/audit"
)

// fakeAuditQuerier serves canned pages keyed by the After cursor and records the filters it
// was called with, so the tests can prove the export (a) loops the cursor chain, (b) forwards
// the caller's filters unchanged, (c) stops at the cap.
type fakeAuditQuerier struct {
	pages map[string]struct {
		rows []audit.AuditRow
		next string
	}
	calls []audit.QueryFilter
	err   error
}

func (f *fakeAuditQuerier) Query(_ context.Context, _ string, q audit.QueryFilter) ([]audit.AuditRow, string, error) {
	f.calls = append(f.calls, q)
	if f.err != nil {
		return nil, "", f.err
	}
	p, ok := f.pages[q.After]
	if !ok {
		return nil, "", nil
	}
	// Honor the requested limit like the real repository (needed by the cap test).
	rows := p.rows
	next := p.next
	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}
	return rows, next, nil
}

func auditRowAt(id, entity, action string, diff string, at time.Time) audit.AuditRow {
	var d audit.RawJSON
	if diff != "" {
		d = audit.RawJSON(diff)
	}
	return audit.AuditRow{ID: id, ActorUserID: "actor-1", EntityName: entity, Action: action, Diff: d, At: at}
}

// TestAuditExportCSV_PagesFiltersAndGuard drives the export across two cursor pages and
// asserts: header row, page-loop follow-through, filter passthrough, csvGuard on hostile
// cells, and NULL diff rendering as an empty cell (not "null").
func TestAuditExportCSV_PagesFiltersAndGuard(t *testing.T) {
	at := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	fq := &fakeAuditQuerier{pages: map[string]struct {
		rows []audit.AuditRow
		next string
	}{
		"": {rows: []audit.AuditRow{
			auditRowAt("r1", "accounts", "update", `{"after":{"x":1}}`, at),
			// Hostile entity name: must be csvGuard-quoted so a spreadsheet won't execute it.
			auditRowAt("r2", `=HYPERLINK("http://evil")`, "create", "", at.Add(-time.Minute)),
		}, next: "cur-2"},
		"cur-2": {rows: []audit.AuditRow{
			auditRowAt("r3", "tickets", "delete", `{"before":{"y":2}}`, at.Add(-2*time.Minute)),
		}, next: ""},
	}}

	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	var sb strings.Builder
	n, err := NewAuditExportService(fq).ExportCSV(context.Background(), "tenant-a", audit.QueryFilter{
		EntityName: "ignored-by-fake", ActorUserID: "actor-1", From: &from,
	}, &sb)
	if err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows written = %d; want 3", n)
	}

	// The download opens in Excel, so the stream must start with a UTF-8 BOM — without it the
	// Vietnamese record labels and user names in the trail arrive as mojibake.
	if !strings.HasPrefix(sb.String(), "\xEF\xBB\xBF") {
		t.Fatalf("audit export does not start with a UTF-8 BOM: %q", sb.String()[:min(6, sb.Len())])
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(sb.String(), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("parse produced CSV: %v", err)
	}
	if len(recs) != 4 { // header + 3 rows
		t.Fatalf("csv line count = %d; want 4", len(recs))
	}
	if got, want := strings.Join(recs[0], ","), "id,at,actorUserId,entityName,recordId,action,diff,actorLabel"; got != want {
		t.Fatalf("header = %q; want %q", got, want)
	}
	// Formula-injection guard: leading '=' must be neutralized with a quote prefix.
	if got := recs[2][3]; !strings.HasPrefix(got, "'=") {
		t.Fatalf("hostile entityName cell = %q; want csvGuard \"'=\" prefix", got)
	}
	// NULL diff (nil RawJSON) renders as an empty cell.
	if got := recs[2][6]; got != "" {
		t.Fatalf("nil diff cell = %q; want empty", got)
	}
	// Filter passthrough: every page call must carry the caller's filters.
	if len(fq.calls) != 2 {
		t.Fatalf("query calls = %d; want 2 (two pages)", len(fq.calls))
	}
	for i, call := range fq.calls {
		if call.ActorUserID != "actor-1" || call.From == nil || !call.From.Equal(from) {
			t.Fatalf("page %d lost filters: %+v", i, call)
		}
	}
	if fq.calls[1].After != "cur-2" {
		t.Fatalf("page 2 cursor = %q; want cur-2", fq.calls[1].After)
	}
}

// TestAuditExportCSV_ErrorAndCap covers the error wrap (a failing reader aborts with a wrapped
// error) and the hard row cap (the loop must stop at auditExportMaxRows even when the reader
// keeps advertising more pages).
func TestAuditExportCSV_ErrorAndCap(t *testing.T) {
	t.Run("reader error is wrapped", func(t *testing.T) {
		boom := errors.New("db down")
		fq := &fakeAuditQuerier{err: boom}
		var sb strings.Builder
		_, err := NewAuditExportService(fq).ExportCSV(context.Background(), "t", audit.QueryFilter{}, &sb)
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v; want wrapped %v", err, boom)
		}
	})

	t.Run("hard cap stops an endless cursor chain", func(t *testing.T) {
		// One "page" that always advertises another page: 200 rows, next always set.
		rows := make([]audit.AuditRow, auditExportPageSize)
		at := time.Now().UTC()
		for i := range rows {
			rows[i] = auditRowAt("r", "e", "create", "", at)
		}
		fq := &fakeAuditQuerier{pages: map[string]struct {
			rows []audit.AuditRow
			next string
		}{
			"":      {rows: rows, next: "again"},
			"again": {rows: rows, next: "again"},
		}}
		var sb strings.Builder
		n, err := NewAuditExportService(fq).ExportCSV(context.Background(), "t", audit.QueryFilter{}, &sb)
		if err != nil {
			t.Fatalf("ExportCSV: %v", err)
		}
		if n != auditExportMaxRows {
			t.Fatalf("rows written = %d; want cap %d", n, auditExportMaxRows)
		}
	})
}

// specs/062 L3 T054a — the builder's name rides in a trailing `actorLabel` column: old columns keep
// their positions, the label (from a session OR from diff.after.by — the repository decides) goes
// through csvGuard because it is a person's typed name, and the truncation marker keeps the row
// width.
func TestAuditExportCSV_ActorLabelColumn(t *testing.T) {
	at := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	label := "An Nguyễn (Nexus)"
	hostile := "=cmd|' /C calc'!A0"
	r1 := auditRowAt("r1", "__layout__", "update", `{"after":{"x":1}}`, at)
	r1.ActorLabel = &label
	r2 := auditRowAt("r2", "__tenant_backup__", "create", `{"after":{"by":"Bình (Xưởng)"}}`, at)
	by := "Bình (Xưởng)"
	r2.ActorLabel = &by
	r3 := auditRowAt("r3", "leads", "update", "", at)
	r4 := auditRowAt("r4", "__layout__", "update", "", at)
	r4.ActorLabel = &hostile

	fq := &fakeAuditQuerier{pages: map[string]struct {
		rows []audit.AuditRow
		next string
	}{"": {rows: []audit.AuditRow{r1, r2, r3, r4}}}}
	var sb strings.Builder
	if _, err := NewAuditExportService(fq).ExportCSV(context.Background(), "t", audit.QueryFilter{}, &sb); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(sb.String(), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	const labelCol = 7
	for i, want := range []string{label, by, "", "'" + hostile} {
		if len(recs[i+1]) != len(auditCSVHeader) {
			t.Fatalf("row %d width = %d; want %d", i+1, len(recs[i+1]), len(auditCSVHeader))
		}
		if got := recs[i+1][labelCol]; got != want {
			t.Errorf("row %d actorLabel = %q; want %q", i+1, got, want)
		}
	}
	// Old columns keep their positions.
	if recs[1][0] != "r1" || recs[1][3] != "__layout__" || recs[1][6] != `{"after":{"x":1}}` {
		t.Errorf("old columns moved: %v", recs[1])
	}
}

// The truncation marker row must be as wide as the header.
func TestAuditExportCSV_TruncatedMarkerWidth(t *testing.T) {
	rows := make([]audit.AuditRow, auditExportPageSize)
	at := time.Now().UTC()
	for i := range rows {
		rows[i] = auditRowAt("r", "e", "create", "", at)
	}
	fq := &fakeAuditQuerier{pages: map[string]struct {
		rows []audit.AuditRow
		next string
	}{"": {rows: rows, next: "again"}, "again": {rows: rows, next: "again"}}}
	var sb strings.Builder
	if _, err := NewAuditExportService(fq).ExportCSV(context.Background(), "t", audit.QueryFilter{}, &sb); err != nil {
		t.Fatalf("ExportCSV: %v", err)
	}
	recs, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(sb.String(), "\xEF\xBB\xBF"))).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	last := recs[len(recs)-1]
	if last[0] != "__TRUNCATED__" || len(last) != len(auditCSVHeader) {
		t.Fatalf("marker = %v (width %d); want __TRUNCATED__ with %d cells", last, len(last), len(auditCSVHeader))
	}
}
