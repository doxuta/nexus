package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/services"
)

// AuditReader is the consumer-side view the audit-log API needs: a tenant-scoped, cursor
// paginated query over audit_log plus a single-record history. Declared here so the handler
// depends on an interface (the real implementation is *audit.SQLRepository) and tests can
// substitute a fake.
type AuditReader interface {
	Query(ctx context.Context, tenantID string, f audit.QueryFilter) ([]audit.AuditRow, string, error)
}

// AuditExporter is the consumer-side view of the CSV export (W7 audit v2): stream the SAME
// filtered, tenant-scoped trail the list serves, as CSV, onto w. The real implementation is
// *services.AuditExportService.
type AuditExporter interface {
	ExportCSV(ctx context.Context, tenantID string, f audit.QueryFilter, w io.Writer) (int, error)
}

// recordVisibilityProbe reads ONE record under the caller's row scope. The record-history route
// uses it as the visibility question it cannot answer itself: "may this caller see this row?".
// Satisfied by *services.DynamicRecordService (the same Get the record screen uses), so the two
// answers can never drift.
type recordVisibilityProbe interface {
	Get(ctx context.Context, scope services.RecordScope, tenantID, entity, id string, expand, rollup []string) (*dynrepo.Record, error)
}

// fieldReadFilter drops field values the caller's role may not read. Satisfied by
// *services.FieldACLService — the same enforcement the record read applies, reused so history
// cannot become a side channel around field-level security.
type fieldReadFilter interface {
	FilterReadFieldsBatch(ctx context.Context, tenantID, roleID, entity string, isAdmin bool, datas []map[string]any) error
}

// AuditHandler serves the RM-19 read API: GET /audit-log (filterable, cursor paginated), GET
// /audit-log/export.csv (W7 — same filters, streamed CSV) and GET /audit-log/:entity/:recordId
// (history of one record).
//
// The two TENANT-WIDE surfaces stay admin-only: a filterable trail across every record is a
// governance/surveillance surface, and export.csv walks it in bulk. The PER-RECORD history is
// mounted on the record's own view-ACL gate instead. A user who may open a lead and edit it could
// not see who changed its status — the tab rendered "Yêu cầu quyền quản trị viên" for everyone but
// an admin, which is also why the gap survived: an admin testing it saw a working tab.
//
// Widening a read is only safe with the same two checks the record screen makes, so RecordHistory
// makes both: the row must be visible under the caller's scope (owner/team/all), and every value
// in a diff goes through field-level ACL. Both come from the services that already own those
// decisions — no second implementation to drift.
type AuditHandler struct {
	reader   AuditReader
	exporter AuditExporter
	logger   *zap.Logger

	// Record-history deps (WithRecordScope). All-or-nothing: while unwired, the per-record route
	// fails CLOSED for non-admins rather than serving an unscoped trail.
	records  recordVisibilityProbe
	fieldACL fieldReadFilter
}

// WithRecordScope arms the per-record history for non-admins: probe answers "is this row visible
// to the caller", fields drops values the caller's role may not read. Chainable; nil either side
// keeps the route admin-only in effect.
func (h *AuditHandler) WithRecordScope(probe recordVisibilityProbe, fields fieldReadFilter) *AuditHandler {
	h.records = probe
	h.fieldACL = fields
	return h
}

// NewAuditHandler wires the handler over the audit reader.
func NewAuditHandler(reader AuditReader, logger *zap.Logger) *AuditHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AuditHandler{reader: reader, logger: logger}
}

// WithExporter injects the CSV export service (W7). Without it the export route still mounts
// but answers 500 AUDIT_EXPORT_UNAVAILABLE — a wiring bug surfaced loudly, not a silent 404.
func (h *AuditHandler) WithExporter(e AuditExporter) *AuditHandler {
	h.exporter = e
	return h
}

// RegisterRoutes mounts the audit-log routes under the given group (admin-gated). The static
// /export.csv segment and the record-history params live at different depths, so gin resolves
// all three without conflict; registration order keeps the static segments explicit.
func (h *AuditHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/audit-log", h.List)
	rg.GET("/audit-log/export.csv", h.ExportCSV)
}

// RegisterRecordRoutes mounts ONLY the per-record history, on a group carrying the record
// view-ACL stage (RequireACLAction "view"), which resolves the row-level access_level scopeOf()
// reads. The param is named :entity because that is what middleware.requireACL authorizes on.
func (h *AuditHandler) RegisterRecordRoutes(rg *gin.RouterGroup) {
	rg.GET("/audit-log/:entity/:recordId", h.RecordHistory)
}

// List handles GET /api/v1/audit-log with optional filters
// ?entityName=&recordId=&actorUserId=&actor=builder&action=&from=&to=&dateFrom=&dateTo= and cursor pagination
// (?after=, ?limit=, sort `at` DESC). The tenant is taken from the verified claim (tenantOf).
// Returns {data:[...], pagination:{limit, nextCursor}} — limit echoes the clamped effective
// page size.
func (h *AuditHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}

	f, ok := h.parseAuditFilter(c)
	if !ok {
		return
	}
	f.After = c.Query("after")

	h.respondPage(c, tenantID, f)
}

// ExportCSV handles GET /api/v1/audit-log/export.csv (W7): the SAME filters as List (entity /
// record / actor / action / from / to / dateFrom / dateTo — one parser, no drift), streamed as
// text/csv with every text cell formula-guarded. limit/after are ignored: the export loops the
// cursor chain internally (capped server-side). A mid-stream failure after the header has been
// sent can only be logged — the standard streaming trade-off, same as the record CSV export.
func (h *AuditHandler) ExportCSV(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if h.exporter == nil {
		h.logger.Error("handlers.AuditHandler.ExportCSV: exporter not wired", zap.String("tenant_id", tenantID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Xuất audit-log chưa được cấu hình", "code": "AUDIT_EXPORT_UNAVAILABLE"})
		return
	}

	f, ok := h.parseAuditFilter(c)
	if !ok {
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", `attachment; filename="audit-log.csv"`)
	c.Status(http.StatusOK)

	written, err := h.exporter.ExportCSV(c.Request.Context(), tenantID, f, c.Writer)
	if err != nil {
		// Headers are already committed; log with tenant context and stop the stream.
		h.logger.Error("handlers.AuditHandler.ExportCSV", zap.String("tenant_id", tenantID), zap.Error(err))
		return
	}
	if written >= services.AuditExportMaxRows {
		// Cap-hit observability (review LOW-2): the file carries a __TRUNCATED__ marker row;
		// this log is the operator-side signal for compliance exports.
		h.logger.Warn("handlers.AuditHandler.ExportCSV: cap hit — export truncated",
			zap.String("tenant_id", tenantID), zap.Int("rows", written))
	}
}

// RecordHistory handles GET /api/v1/audit-log/:entityName/:recordId — the mutation timeline of
// one record (newest-first, cursor paginated). It is List pinned to a fixed (entityName,
// recordId) so a client can render a record's activity feed.
func (h *AuditHandler) RecordHistory(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	entity := strings.TrimSpace(c.Param("entity"))
	recordID := strings.TrimSpace(c.Param("recordId"))
	scope := scopeOf(c)

	// A non-admin reaching here has passed entity-level "view". That is not enough: with
	// view:own or view:team the row itself may be someone else's. Ask the record service the
	// same question the record screen asks, and let ITS answer stand — including the shape of
	// the refusal, so history cannot be used to probe for rows a listing would never show.
	sharedOnly := false
	if !scope.IsAdmin {
		if h.records == nil || h.fieldACL == nil {
			h.logger.Error("handlers.AuditHandler.RecordHistory: record scope not wired; refusing",
				zap.String("tenant_id", tenantID))
			c.JSON(http.StatusForbidden, gin.H{"error": "Yêu cầu quyền quản trị viên", "code": "FORBIDDEN"})
			return
		}
		rec, err := h.records.Get(c.Request.Context(), scope, tenantID, entity, recordID, nil, nil)
		if err != nil {
			h.respondRecordProbeError(c, err, tenantID, entity)
			return
		}
		sharedOnly = rec != nil && rec.SharedWithMe
	}

	rows, nextCursor, err := h.reader.Query(c.Request.Context(), tenantID, audit.QueryFilter{
		EntityName: entity,
		RecordID:   recordID,
		Limit:      atoiDefault(c.Query("limit"), 50),
		After:      c.Query("after"),
	})
	if err != nil {
		if errors.Is(err, dynrepo.ErrInvalidCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cursor không hợp lệ", "code": "INVALID_CURSOR"})
			return
		}
		h.logger.Error("handlers.AuditHandler.RecordHistory", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi đọc audit-log", "code": "AUDIT_QUERY_FAILED"})
		return
	}
	if sharedOnly {
		rows = dropShareRows(rows)
	}
	if !scope.IsAdmin {
		if ferr := h.redactDiffs(c.Request.Context(), tenantID, scope.RoleID, entity, rows); ferr != nil {
			h.logger.Error("handlers.AuditHandler.RecordHistory: field-ACL filter failed",
				zap.String("tenant_id", tenantID), zap.Error(ferr))
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi đọc audit-log", "code": "AUDIT_QUERY_FAILED"})
			return
		}
	}
	if rows == nil {
		rows = []audit.AuditRow{}
	}
	var next *string
	if nextCursor != "" {
		next = &nextCursor
	}
	c.JSON(http.StatusOK, gin.H{
		"data":       rows,
		"pagination": gin.H{"limit": audit.ClampLimit(atoiDefault(c.Query("limit"), 50)), "nextCursor": next},
	})
}

// dropShareRows removes the share/unshare entries (specs/051 §6, DAILY-QUEUE #226): their diff names
// the colleague the record was shared with, and the share list itself is pinned to `edit` so a
// read-only viewer never learns it. A caller who reaches the record only through a share (FR-009
// marker) cannot edit it by construction, so they do not see those rows.
func dropShareRows(rows []audit.AuditRow) []audit.AuditRow {
	kept := rows[:0]
	for _, r := range rows {
		if r.Action != audit.ActionShare && r.Action != audit.ActionUnshare {
			kept = append(kept, r)
		}
	}
	return kept
}

// respondRecordProbeError forwards the record service's refusal verbatim in shape. A denied or
// missing row must answer exactly as GET /:entity/:id does (404 with the same code) — a distinct
// 403 here would confirm the row exists, which is the enumeration leak the record surface already
// closed. A ValidationError carries its own code/status; anything else is a real failure.
func (h *AuditHandler) respondRecordProbeError(c *gin.Context, err error, tenantID, entity string) {
	// The two not-found sentinels are the whole point of the probe and they are NOT
	// ValidationErrors — matching only on ValidationError answered 500 for a record that simply
	// does not exist, which is both wrong and noisier than the leak it was meant to avoid.
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
		return
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
		return
	}
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AuditHandler.RecordHistory: visibility probe",
		zap.String("tenant_id", tenantID), zap.String("entity", entity), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi đọc audit-log", "code": "AUDIT_QUERY_FAILED"})
}

// redactDiffs runs every value in every entry's diff through field-level ACL, in place.
//
// Both halves matter: `after` is the new value, `before` the old one — a field the caller may not
// read is equally exposed by either, and showing only the key (with the value gone) still reveals
// that it changed and when. So a denied key is removed from both maps. The filter runs as ONE
// batch across all entries: field-ACL rows are loaded per (role, entity), and a per-entry call
// would reload them for every row on the page.
func (h *AuditHandler) redactDiffs(ctx context.Context, tenantID, roleID, entity string, rows []audit.AuditRow) error {
	var datas []map[string]any
	parsed := make([]map[string]map[string]any, len(rows))

	for i := range rows {
		if len(rows[i].Diff) == 0 {
			continue
		}
		var d map[string]map[string]any
		if err := json.Unmarshal(rows[i].Diff, &d); err != nil {
			// An unreadable diff is not a reason to leak it: blank the payload and keep the entry
			// (the fact that a change happened, by whom and when, is not field-scoped).
			rows[i].Diff = nil
			continue
		}
		parsed[i] = d
		for _, which := range []string{"before", "after"} {
			if m := d[which]; m != nil {
				// The filter mutates each map in place, so collecting them is enough — no
				// index bookkeeping is needed to write the results back.
				datas = append(datas, m)
			}
		}
	}
	if len(datas) == 0 {
		return nil
	}
	// isAdmin=false: this path only runs for non-admins (admins skip redaction entirely).
	if err := h.fieldACL.FilterReadFieldsBatch(ctx, tenantID, roleID, entity, false, datas); err != nil {
		return err
	}
	for i := range rows {
		if parsed[i] == nil {
			continue
		}
		re, err := json.Marshal(parsed[i])
		if err != nil {
			rows[i].Diff = nil
			continue
		}
		rows[i].Diff = re
	}
	return nil
}

// parseAuditFilter builds the shared list/export QueryFilter from the query string. Two date
// families are accepted (mutually exclusive per bound, mixing → 400 AUDIT_INVALID_TIME):
//   - from/to        — RFC3339 instants, inclusive (the RM-19 originals);
//   - dateFrom/dateTo — YYYY-MM-DD calendar days in UTC (W7, the FE DateRangePicker shape):
//     dateFrom ⇒ at >= 'D 00:00:00Z'; dateTo ⇒ at < 'D+1d 00:00:00Z' (whole-day inclusive,
//     mirroring the run-overlay semantics).
//
// On failure a 400 has already been written and ok=false is returned.
func (h *AuditHandler) parseAuditFilter(c *gin.Context) (audit.QueryFilter, bool) {
	f := audit.QueryFilter{
		EntityName:  strings.TrimSpace(c.Query("entityName")),
		RecordID:    strings.TrimSpace(c.Query("recordId")),
		ActorUserID: strings.TrimSpace(c.Query("actorUserId")),
		Action:      strings.TrimSpace(c.Query("action")),
		Limit:       atoiDefault(c.Query("limit"), 50),
	}
	// ?actor=builder = only rows a Nexus builder made in this workspace (specs/062 T071). Any other
	// non-empty value is a 400: silently ignoring it would show the whole trail under a filter label.
	switch strings.TrimSpace(c.Query("actor")) {
	case "":
	case "builder":
		f.BuilderOnly = true
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "tham số actor chỉ nhận giá trị builder", "code": "AUDIT_INVALID_ACTOR"})
		return f, false
	}

	from, ferr := parseAuditTime(c.Query("from"))
	if ferr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tham số from không phải thời gian hợp lệ (RFC3339)", "code": "AUDIT_INVALID_TIME"})
		return f, false
	}
	to, terr := parseAuditTime(c.Query("to"))
	if terr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "tham số to không phải thời gian hợp lệ (RFC3339)", "code": "AUDIT_INVALID_TIME"})
		return f, false
	}

	dateFrom := strings.TrimSpace(c.Query("dateFrom"))
	dateTo := strings.TrimSpace(c.Query("dateTo"))
	if (dateFrom != "" && from != nil) || (dateTo != "" && to != nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "không dùng đồng thời from/to (RFC3339) và dateFrom/dateTo (YYYY-MM-DD) cho cùng một biên", "code": "AUDIT_INVALID_TIME"})
		return f, false
	}
	if dateFrom != "" {
		d, err := time.ParseInLocation(auditDateLayout, dateFrom, time.UTC)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tham số dateFrom không hợp lệ (định dạng YYYY-MM-DD)", "code": "AUDIT_INVALID_TIME"})
			return f, false
		}
		from = &d
	}
	if dateTo != "" {
		d, err := time.ParseInLocation(auditDateLayout, dateTo, time.UTC)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "tham số dateTo không hợp lệ (định dạng YYYY-MM-DD)", "code": "AUDIT_INVALID_TIME"})
			return f, false
		}
		// Whole-day inclusive upper bound without the 23:59:59 gap: at < dateTo+1d 00:00:00.
		bound := d.AddDate(0, 0, 1)
		f.ToExclusive = &bound
	}
	if from != nil && f.ToExclusive != nil && !from.Before(*f.ToExclusive) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "dateFrom phải nhỏ hơn hoặc bằng dateTo", "code": "AUDIT_INVALID_TIME"})
		return f, false
	}

	f.From = from
	f.To = to
	return f, true
}

// auditDateLayout is the strict calendar-date layout ?dateFrom/?dateTo accept (rejects
// non-padded forms and datetime strings), interpreted in UTC — audit `at` is stored UTC.
const auditDateLayout = "2006-01-02"

// respondPage runs the query and writes the standard cursor-paginated envelope. A malformed
// cursor (ErrInvalidCursor) is a client error → 400; any other failure is a 500.
func (h *AuditHandler) respondPage(c *gin.Context, tenantID string, f audit.QueryFilter) {
	rows, nextCursor, err := h.reader.Query(c.Request.Context(), tenantID, f)
	if err != nil {
		if errors.Is(err, dynrepo.ErrInvalidCursor) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cursor không hợp lệ", "code": "INVALID_CURSOR"})
			return
		}
		h.logger.Error("handlers.AuditHandler.respondPage", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi đọc audit-log", "code": "AUDIT_QUERY_FAILED"})
		return
	}
	var next *string
	if nextCursor != "" {
		next = &nextCursor
	}
	// Always emit a non-null data array so the FE never has to null-check.
	if rows == nil {
		rows = []audit.AuditRow{}
	}
	c.JSON(http.StatusOK, gin.H{
		"data": rows,
		"pagination": gin.H{
			// Echo the EFFECTIVE (clamped) page size — the same clamp the repository applies —
			// so the FE's load-more knows the true page length (W7: the doc'd envelope shape).
			"limit":      audit.ClampLimit(f.Limit),
			"nextCursor": next,
		},
	})
}

// parseAuditTime parses an optional RFC3339 timestamp query param. An empty string yields
// (nil, nil) — the filter bound is simply not applied. A non-empty but malformed value yields
// an error so the handler can return 400 rather than silently ignoring a bad filter.
func parseAuditTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
