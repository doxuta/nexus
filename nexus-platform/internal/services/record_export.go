package services

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"nexus/platform/internal/dynrepo"
)

// exportPageSize is the cursor page size used to stream a CSV export. Keyset (cursor) pagination
// is O(1) per page regardless of depth, so a large export walks the whole result set without the
// deep-offset O(n²) penalty and without ever buffering all rows in memory — each page is written
// to the io.Writer and discarded.
const exportPageSize = 100

// exportMaxRows is a hard ceiling on the number of rows a single CSV export streams. It guards
// against an unbounded export of a huge entity tying up a connection/goroutine; a report/export
// over this many rows should be narrowed by filters. The cap is generous (a real export use case
// stays well under it) but finite.
const exportMaxRows = 1_000_000

// ExportCSVOptions bundles the optional narrowing/projection of a CSV export:
//   - Filters: the same ?field[op]=value filters as a list query (validated identically).
//   - Overlay: the W4 request-time created_at/created_by overlay — the report export applies
//     the SAME overlay as the on-screen run so what you see is what you export.
//   - Columns: optional projection (report export): the header and cells cover exactly these
//     field names in order instead of every declared field. A column that no longer exists in
//     metadata exports as empty cells — mirroring the run endpoint, which returns null for it.
//   - Expand: relation / `user` column names whose stored FK id must export as the target's
//     human label (REPORT-RELATION-LABELS). Resolved per cursor page by the SAME applyExpand the
//     list engine runs behind ListQuery.Expand — one batch per target entity, entity-view ACL on
//     the target (W5-A2) and field-ACL on its labelField (EXP-01) included — so a report's
//     screen and its file can never disagree on who may read a name. Empty = FK ids export
//     verbatim; the whole-entity dump keeps them on purpose (it is keyed by `id` for
//     integrations and is not the file a manager reads).
type ExportCSVOptions struct {
	Filters []FilterInput
	Overlay *RunOverlay
	Columns []string
	Expand  []string
}

// ExportCSV streams the records of an entity as RFC 4180 CSV to w, scoped by tenant + A5
// row-level access and narrowed by the same ?field[op]=value filters as a list query (EPIC 6 M2)
// plus the optional W4 overlay. It walks the result set with keyset (cursor) pagination on id
// ASC — a stable, always-indexed order — so the export covers every matching row without
// deep-offset cost and without buffering the whole set in memory (each page is written then
// discarded).
//
// Columns: the data columns are the entity's declared fields in metadata declaration order (or
// opts.Columns when projected), plus the leading system column "id" — dropped in the projected
// (report) case, see below. The HEADER row carries each field's display LABEL (falling back to
// the field name when there is none, when the field no longer exists, or when field-ACL hides it
// from the caller), so the file reads like the screen it came from. The stream opens with a UTF-8
// BOM so Excel reads it as UTF-8. A value is rendered as its scalar text (strings verbatim; numbers/bools via fmt);
// a relation / `user` column named in opts.Expand is rendered as the target's human label (see
// csvCell); a JSON object/array value (composite, multi_select, currency object) is rendered as its
// compact JSON text so the cell is never blank. encoding/csv handles RFC 4180 quoting/escaping
// (embedded comma, quote, newline) for every field.
//
// Tenant + row-scope isolation is identical to List: the export only contains rows the caller may
// read, so it can never leak another tenant's / team's records.
func (s *RecordService) ExportCSV(ctx context.Context, scope RecordScope, tenantID, entityName string, opts ExportCSVOptions, w io.Writer) error {
	e, specs, err := s.resolve(ctx, tenantID, entityName)
	if err != nil {
		return err
	}

	byName := make(map[string]FieldSpec, len(specs))
	for _, f := range specs {
		byName[f.Name] = f
	}
	// Validate + resolve filters identically to List/Aggregate (field existence, operator
	// legality, physical-column rewrite, currency sub-path, date canonicalization).
	//
	// Queue #317: filter keys resolve against the fields THIS caller may see, so a hidden name reads
	// exactly like one that does not exist (LIST_UNKNOWN_FILTER), not a 403 that confirms it. A load
	// failure fails closed.
	hiddenForKeys, herr := s.computedHiddenFields(ctx, scope, tenantID, e.Name)
	if herr != nil {
		return fmt.Errorf("services.ExportCSV: field-acl: %w", herr)
	}
	resolvedFilters, ferr := resolveFiltersForAggregate(visibleByName(byName, hiddenForKeys), opts.Filters)
	if ferr != nil {
		return ferr
	}
	// W4 run overlay: same system-column predicates as the run path (created_at range /
	// created_by eq) so the exported set is EXACTLY the on-screen filtered set.
	resolvedFilters = append(resolvedFilters, overlayFilters(opts.Overlay)...)

	// RM-42 anti-oracle: block FILTERING on a field the role cannot view (a filtered export
	// reconstructs a hidden value from which rows appear). Reject with 403 FIELD_FORBIDDEN before
	// any byte is written. Admin bypasses. The per-page read strip below additionally removes
	// hidden field VALUES from the exported cells.
	//
	// The key is mapped through guardedFilterFields — the SAME helper List and Aggregate use.
	// Export used to pass the RAW key, and that was the whole hole: resolveFiltersForAggregate
	// (called above) deliberately ACCEPTS `<poly>_type` / `<poly>_id`, but AssertFieldsVisible only
	// rejects names in the `hidden` set, which is keyed by real sys_fields rows — a generated
	// sub-column is never one, so the assertion returned nil and a role with can_view=0 on a
	// polymorphic field could export filtered by exactly that field.
	if s.fieldACL != nil {
		if err := s.fieldACL.AssertFieldsVisible(ctx, tenantID, scope.RoleID, e.Name, scope.IsAdmin,
			guardedFilterFields(byName, opts.Filters)); err != nil {
			return err
		}
	}

	headerLabels := s.exportHeaderLabels(ctx, scope, tenantID, e.Name, specs)

	// Column order = declared field order from metadata, or the caller's projection (report
	// export) verbatim. Projected columns are CSV text only (never interpolated into SQL);
	// hidden-field values are stripped per page below, so a projected-but-hidden column exports
	// as empty cells.
	//
	// The leading "id" column: kept for the WHOLE-ENTITY export (it is the stable row key an
	// integration keys off, and the file is a data dump), DROPPED for the projected report
	// export — a projection IS the on-screen column set, and a report handed to a manager must
	// not open on a column of 36-char UUIDs that the screen never showed. Nothing round-trips on
	// it: CSV import is insert-only and maps columns through an explicit client-supplied mapping,
	// never by header text (record_import.go buildMapping).
	projected := len(opts.Columns) > 0
	var fieldNames []string
	if projected {
		fieldNames = append(fieldNames, opts.Columns...)
	} else {
		fieldNames = make([]string, 0, len(specs))
		for _, f := range specs {
			// A whole-entity export lists every field the caller may SEE. A hidden one used to stay in
			// as a column headed by its bare name with empty cells — the name itself is the leak
			// (queue #316/#317: nothing else serves it, api-conventions §4.4b). A projected export
			// (report columns, below) keeps the author's columns; see the doc above.
			if _, isHidden := hiddenForKeys[f.Name]; isHidden {
				continue
			}
			fieldNames = append(fieldNames, f.Name)
		}
	}

	header := make([]string, 0, len(fieldNames)+1)
	if !projected {
		header = append(header, "id")
	}
	for _, name := range fieldNames {
		// A label is the admin-authored display name — the same text the on-screen grid shows.
		// It falls back to the field name when the field has no label, no longer exists, or is
		// hidden from this caller (see exportHeaderLabels). csvGuard: a label is tenant-authored
		// text like any cell, so a label starting with '=' must be neutralized here too — the
		// header row is a spreadsheet row like any other.
		h := name
		if label := headerLabels[name]; label != "" {
			h = label
		}
		header = append(header, csvGuard(h))
	}

	// The caller's hidden-field set does not change between pages; load it once (see the
	// lookup resolution inside the loop). Nothing has been written yet, so a load failure is
	// still a JSON error envelope.
	hidden, err := s.computedHiddenFields(ctx, scope, tenantID, e.Name)
	if err != nil {
		return fmt.Errorf("services.ExportCSV: field-acl: %w", err)
	}

	cw := csv.NewWriter(w)
	after := ""
	written := 0
	headerWritten := false
	for {
		// Keyset page on id ASC — a stable, always-indexed order. The store returns a NextCursor
		// even on the first page (empty After), so the whole set is walked uniformly.
		cp := dynrepo.CursorParams{
			Limit:   exportPageSize,
			After:   after,
			Sort:    "id",
			Order:   "ASC",
			Filters: resolvedFilters,
		}
		records, nextCursor, cerr := s.store.ListRecordsCursor(ctx, e.Name, tenantID, scope.toRowScope(), cp)
		if cerr != nil {
			return fmt.Errorf("services.ExportCSV: %w", cerr)
		}
		// RM-42 read strip: drop fields the role may not view from each page BEFORE rendering
		// the CSV cells, so a hidden field's value never reaches the exported file. The header
		// row still lists every declared field name (a hidden column simply exports as empty
		// cells); an admin bypasses inside the enforcer.
		if err := s.stripReadFields(ctx, scope, tenantID, e.Name, records); err != nil {
			return fmt.Errorf("services.ExportCSV: field-acl: %w", err)
		}
		// ENG-COMPUTED #2: lookup fields are ALWAYS resolved (List does the same, unconditionally),
		// gated on the same hidden set as the strip, and BEFORE applyExpand — which mutates the
		// via-FK into an expandedValue the lookup reader cannot read. Without this the screen shows
		// the mirrored value and the file shows an empty cell.
		denied := map[string]struct{}{} // read-time operands the caller may not see (formula_derived.go)
		if err := applyLookups(ctx, s.lookupFetcher, s.authz, s.fieldACL, scope, tenantID, records, specs, hidden, denied); err != nil {
			return fmt.Errorf("services.ExportCSV: lookup: %w", err)
		}
		// queue #284: formulas over a lookup/rollup are computed at read, so the file must carry
		// them too (their rollup operands resolved first — export never asks for ?rollup=).
		if err := s.applyRollups(ctx, scope, tenantID, records, specs, derivedRollupRefs(specs, hidden), hidden, denied); err != nil {
			return fmt.Errorf("services.ExportCSV: rollup: %w", err)
		}
		applyDerivedFormulas(specs, records, hidden, denied)
		// REPORT-RELATION-LABELS: resolve the projected relation/user columns to their labels on
		// THIS page — after the read strip (a hidden relation field is never resolved; same order
		// as List) and before the BOM/header write below, so an expand failure on page 1 still
		// surfaces as a JSON error envelope rather than a 3-byte "download". A nil fetcher (no
		// label fetcher wired) leaves the ids verbatim, mirroring List.
		if s.fetcher != nil && len(opts.Expand) > 0 {
			if err := s.expandRecords(ctx, scope, tenantID, records, specs, opts.Expand); err != nil {
				return fmt.Errorf("services.ExportCSV: expand: %w", err)
			}
		}
		// Excel opens a bare .csv as the machine's legacy code page unless the file starts with a
		// UTF-8 BOM, turning every Vietnamese value into mojibake ("Nguyá»…n VÄƒn A"). The BOM is
		// the FIRST byte that reaches w, and it is written only once the first page has cleared
		// every error above — not at function entry, and not before the loop either: the handler
		// can reset to a JSON error envelope only while nothing has been written
		// (`!c.Writer.Written()`), and csv.Writer buffers until Flush, so before the BOM existed
		// that window covered the whole first page (a DB timeout or a field-ACL load failure on
		// page 1 still surfaced as 503/500 JSON). Writing the BOM earlier would turn those into a
		// 200 with a 3-byte body that Excel opens as an empty sheet.
		if !headerWritten {
			if _, err := io.WriteString(w, utf8BOM); err != nil {
				return fmt.Errorf("services.ExportCSV: write BOM: %w", err)
			}
			if err := cw.Write(header); err != nil {
				return fmt.Errorf("services.ExportCSV: write header: %w", err)
			}
			headerWritten = true
		}
		for i := range records {
			if written >= exportMaxRows {
				cw.Flush()
				return fmt.Errorf("services.ExportCSV: export vượt %d dòng; hãy thu hẹp bằng filter", exportMaxRows)
			}
			row := buildCSVRow(&records[i], fieldNames, !projected, byName)
			if err := cw.Write(row); err != nil {
				return fmt.Errorf("services.ExportCSV: write row: %w", err)
			}
			written++
		}
		// Flush per page so bytes reach the client incrementally (true streaming).
		cw.Flush()
		if err := cw.Error(); err != nil {
			return fmt.Errorf("services.ExportCSV: flush: %w", err)
		}
		if nextCursor == "" || len(records) == 0 {
			break // last page
		}
		after = nextCursor
	}
	return nil
}

// utf8BOM is the UTF-8 byte-order mark. Excel (Windows) only reads a double-clicked .csv as UTF-8
// when the file opens with it; without it every non-ASCII value renders as mojibake. Every CSV
// this platform streams to a browser download starts with it.
const utf8BOM = "\xEF\xBB\xBF"

// exportHeaderLabels maps field name → display label for the CSV header row, under the SAME
// field-ACL policy as the on-screen tabular run (report_service.go runTabular):
//
//   - a field the caller's role may not VIEW gets no label, so its heading falls back to the bare
//     field name — identical to a column whose field was deleted. Both cases must look the same,
//     or `label != name` becomes an oracle separating "this field exists but is hidden from you"
//     from "this column's field is gone", the exact distinction the FLS policy suppresses
//     elsewhere (handlers/fls_hidden_metadata_integration_test.go). The hidden column's VALUES are
//     already stripped per page by stripReadFields; this is only about the heading.
//   - if the hidden set cannot be loaded, NO labels are returned: fail-closed in the disclosure
//     direction (every heading degrades to its field name) rather than emitting labels whose
//     visibility we could not verify. An export that already has its rows is never failed over a
//     heading.
//
// Admin bypasses inside computedHiddenFields, so an admin export carries every real label.
func (s *RecordService) exportHeaderLabels(ctx context.Context, scope RecordScope, tenantID, entity string, specs []FieldSpec) map[string]string {
	hidden, err := s.computedHiddenFields(ctx, scope, tenantID, entity)
	if err != nil {
		return nil
	}
	labels := make(map[string]string, len(specs))
	for _, f := range specs {
		if _, isHidden := hidden[f.Name]; isHidden {
			continue
		}
		if f.Label != "" {
			labels[f.Name] = f.Label
		}
	}
	return labels
}

// buildCSVRow projects a record to a CSV row in the same column order as the header: the id (only
// when the header carries it — the projected report export drops it), then each declared field's
// value rendered as text. A missing field yields an empty cell.
func buildCSVRow(rec *dynrepo.Record, fieldNames []string, withID bool, byName map[string]FieldSpec) []string {
	row := make([]string, 0, len(fieldNames)+1)
	if withID {
		row = append(row, rec.ID)
	}
	for _, name := range fieldNames {
		row = append(row, csvCellFor(byName[name], rec.Data[name]))
	}
	return row
}

// csvCellFor renders a cell the way Nhập CSV reads it back (#359): a picklist code becomes its label
// — only when the importer would resolve that label to the SAME code (a label shared by two codes,
// or equal to another code, stays the code) — and a number with exactly three decimals gets a
// trailing zero so "12.345" is not read back as 12345 (#361; the zero keeps the value).
func csvCellFor(spec FieldSpec, v any) string {
	switch t := v.(type) {
	case string:
		if spec.FieldType == "picklist" {
			labels, _ := spec.Options["valueLabels"].(map[string]any)
			if label, _ := labels[t].(string); strings.TrimSpace(label) != "" {
				if code, found, _, _ := resolveChoice(spec.Options, label); found && code == t {
					return csvGuard(label)
				}
			}
		}
	case float64:
		if out := csvCell(t); threeDecimalsRe.MatchString(out) {
			return out + "0"
		}
	}
	return csvCell(v)
}

// threeDecimalsRe matches the machine numbers FR-003 would read as thousands: one dot, a 1–3 digit
// non-zero head, exactly three digits after it.
var threeDecimalsRe = regexp.MustCompile(`^-?[1-9]\d{0,2}\.\d{3}$`)

// csvCell renders a single dynamic value as a CSV cell string. Scalars become their natural text;
// a nil becomes "". A relation / `user` value resolved by applyExpand (the in-process structs,
// never a map) renders as its human label — the same tri-state FieldDisplay shows on screen —
// never the FK UUID and never the struct as JSON. A composite/array/object value is rendered as
// compact JSON so the cell carries the data rather than Go's map-print noise. encoding/csv
// applies RFC 4180 quoting on top, and csvGuard neutralizes spreadsheet formula injection on the
// way out — a resolved label is tenant-authored text like any other cell.
func csvCell(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return csvGuard(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// JSON numbers decode to float64; render without a trailing ".0" for whole numbers.
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case expandedValue:
		// Target's label; an empty label (the target has no name) falls back to the id so a cell
		// holding a real FK is never blank.
		if t.Label != "" {
			return csvGuard(t.Label)
		}
		return csvGuard(t.ID)
	case expandedUserValue:
		// Display name → email → id (USER-FIELD tri-state).
		switch {
		case t.Label != "":
			return csvGuard(t.Label)
		case t.Email != "":
			return csvGuard(t.Email)
		}
		return csvGuard(t.ID)
	case expandedIDOnly:
		// EXP-01: field-ACL denies the target's labelField → the caller's own stored FK, exactly
		// what the screen shows; the label was never fetched.
		return csvGuard(t.ID)
	case json.RawMessage:
		return csvGuard(string(t))
	default:
		// Objects/arrays (composite, multi_select, currency {amount,currency}) → compact JSON.
		b, err := json.Marshal(v)
		if err != nil {
			return csvGuard(strings.TrimSpace(fmt.Sprintf("%v", v)))
		}
		return csvGuard(string(b))
	}
}

// csvGuard neutralizes spreadsheet formula injection (OWASP CSV injection): a cell whose first
// NON-BLANK character is '=', '@', TAB or CR — or is '+'/'-' and the cell is not a plain number —
// is prefixed with a single quote so Excel/Sheets/LibreOffice render it as text instead of
// executing it (e.g. =HYPERLINK(...), =cmd|' /c calc'!A1). Purely numeric cells ("-100", "+3.5")
// pass through untouched so numeric columns keep sorting numerically. RFC 4180 quoting alone does
// NOT stop formula interpretation. Tenant data is untrusted; the CSV opener (often an admin) is
// the target.
//
// The leading-blank skip matters: the check used to read s[0], so " =HYPERLINK(...)" — one space —
// walked straight past it, and every importer that trims leading whitespace on load then evaluated
// the formula. A space is not a defence.
func csvGuard(s string) string {
	trimmed := strings.TrimLeft(s, " ")
	if trimmed == "" {
		return s
	}
	switch trimmed[0] {
	case '=', '@', '\t', '\r':
		return "'" + s
	case '+', '-':
		if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return s
		}
		return "'" + s
	}
	return s
}
