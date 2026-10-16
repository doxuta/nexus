package services

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"time"

	"nexus/platform/internal/audit"
)

// auditQuerier is the subset of the audit read repository the CSV export needs — the SAME
// tenant-scoped, cursor-paginated Query the GET /audit-log list uses, so the export honors
// exactly the filters the list honors (one query path, no drift). Consumer-side interface;
// the production implementation is *audit.SQLRepository.
type auditQuerier interface {
	Query(ctx context.Context, tenantID string, f audit.QueryFilter) ([]audit.AuditRow, string, error)
}

const (
	// auditExportMaxRows caps one CSV export so a huge trail cannot stream unbounded. Deep
	// archives should narrow with the date-range filters (the same ones the list uses).
	auditExportMaxRows = 10000
	// auditExportPageSize is the internal cursor page size the export loops with. It stays at
	// the repository's max page so the loop is ceil(N/200) round-trips.
	auditExportPageSize = 200
)

// AuditExportMaxRows exposes the cap for the HTTP layer's cap-hit observability log —
// single source of truth (B-catalog: no dual-source constants).
const AuditExportMaxRows = auditExportMaxRows

// auditCSVHeader is the fixed column order of the export. Mirrors the JSON list envelope
// fields (camelCase) so a row in the CSV lines up 1:1 with a row in the API. actorLabel (specs/062
// L3) is APPENDED so the older columns keep their positions for anyone already parsing the file.
var auditCSVHeader = []string{"id", "at", "actorUserId", "entityName", "recordId", "action", "diff", "actorLabel"}

// AuditExportService streams a tenant's audit trail as CSV (W7 audit v2 — plan §3.17 "Export
// CSV cùng filters"). It reuses the list's Query (filters + keyset pagination) page by page,
// so memory stays O(page) regardless of trail size, and every text cell passes csvGuard
// (OWASP CSV-injection neutralization — audit diffs carry raw tenant data, and the file's
// audience is an admin's spreadsheet).
type AuditExportService struct {
	reader auditQuerier
}

// NewAuditExportService wires the export over the audit read repository.
func NewAuditExportService(reader auditQuerier) *AuditExportService {
	return &AuditExportService{reader: reader}
}

// ExportCSV writes the filtered, tenant-scoped audit trail to w as CSV (newest-first, the
// list's order) and returns the number of data rows written. The caller controls f's filters
// (entity/record/actor/action/date range); Limit/After are OWNED by the export loop and
// overwritten. Output is capped at auditExportMaxRows (narrow with date filters for more).
//
// Multi-tenant: tenantID pins every page query (the repository enforces it); rows of another
// tenant can never appear. Errors are wrapped; a mid-stream error leaves a truncated file —
// the HTTP layer has already committed the 200 header, which is the standard streaming
// trade-off (same as the record CSV export).
func (s *AuditExportService) ExportCSV(ctx context.Context, tenantID string, f audit.QueryFilter, w io.Writer) (int, error) {
	// UTF-8 BOM: the audit trail carries Vietnamese record labels and user names, and the file's
	// audience opens it by double-clicking it in Excel — without the BOM every one of them is
	// mojibake. Safe at the top of the stream here, unlike RecordService.ExportCSV: the audit
	// handler has no `!c.Writer.Written()` reset branch (a mid-stream error is logged and the
	// stream stops), so there is no pre-stream JSON envelope for the BOM to spoil.
	if _, err := io.WriteString(w, utf8BOM); err != nil {
		return 0, fmt.Errorf("services.AuditExportService.ExportCSV: write BOM: %w", err)
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(auditCSVHeader); err != nil {
		return 0, fmt.Errorf("services.AuditExportService.ExportCSV: header: %w", err)
	}

	written := 0
	moreRows := false
	f.After = ""
	for written < auditExportMaxRows {
		f.Limit = auditExportPageSize
		if remaining := auditExportMaxRows - written; remaining < f.Limit {
			f.Limit = remaining
		}
		rows, next, err := s.reader.Query(ctx, tenantID, f)
		if err != nil {
			return written, fmt.Errorf("services.AuditExportService.ExportCSV: %w", err)
		}
		for i := range rows {
			if err := cw.Write(auditCSVRecord(&rows[i])); err != nil {
				return written, fmt.Errorf("services.AuditExportService.ExportCSV: row: %w", err)
			}
			written++
		}
		moreRows = next != ""
		if next == "" {
			break
		}
		f.After = next
	}

	// Truncation marker (security W-3): if the cap fired while more rows remained, say so IN
	// the file — headers are already committed mid-stream, and a silent cut reads as "that was
	// everything" to an investigating admin.
	if moreRows && written >= auditExportMaxRows {
		if err := cw.Write([]string{"__TRUNCATED__", "", "", "", "", "",
			fmt.Sprintf("export chạm trần %d dòng — thu hẹp khoảng ngày để lấy phần còn lại", auditExportMaxRows), ""}); err != nil {
			return written, fmt.Errorf("services.AuditExportService.ExportCSV: truncation marker: %w", err)
		}
	}

	cw.Flush()
	if err := cw.Error(); err != nil {
		return written, fmt.Errorf("services.AuditExportService.ExportCSV: flush: %w", err)
	}
	return written, nil
}

// auditCSVRecord renders one audit row into the fixed column order, csvGuard-ing every cell
// that carries tenant-controlled text (entity/record/actor/action/diff). `at` is RFC3339 with
// millisecond precision (the column's TIMESTAMP(3) resolution), UTC.
func auditCSVRecord(r *audit.AuditRow) []string {
	recordID := ""
	if r.RecordID != nil {
		recordID = *r.RecordID
	}
	label := ""
	if r.ActorLabel != nil {
		label = *r.ActorLabel
	}
	return []string{
		csvGuard(r.ID),
		r.At.UTC().Format(time.RFC3339Nano),
		csvGuard(r.ActorUserID),
		csvGuard(r.EntityName),
		csvGuard(recordID),
		csvGuard(r.Action),
		csvGuard(string(r.Diff)),
		csvGuard(label), // a person's typed name (workspace session) — formula-injection guard
	}
}
