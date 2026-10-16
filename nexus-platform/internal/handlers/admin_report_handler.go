package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// AdminReportHandler exposes the EPIC 6 M2 report-management endpoints. The CRUD surface
// (/admin/reports) sits behind the AuthMiddleware → RequireTenant → RequireAdmin chain (an admin
// builds saved reports); the RUN surface (/reports/:id/run) sits on the protected (non-admin)
// group because running a report returns tenant + A5 row-scoped DATA the caller may read — it is
// a read for any authenticated user, governed by the same row-scope as a list/aggregate request,
// not the coarse admin gate.
type AdminReportHandler struct {
	svc    *services.ReportService
	logger *zap.Logger
	// entityACL filters the non-admin catalog down to reports whose PRIMARY entity the caller may
	// view. nil = no filter (pre-slice behaviour, older test harnesses).
	entityACL entityViewChecker
}

// WithEntityACL wires the per-entity "view" decider used to filter GET /reports — the same
// consumer-side seam MetadataHandler uses (one decider, ACLService.CanViewEntity, everywhere).
func (h *AdminReportHandler) WithEntityACL(a entityViewChecker) *AdminReportHandler {
	h.entityACL = a
	return h
}

// NewAdminReportHandler wires the handler over the report service.
func NewAdminReportHandler(svc *services.ReportService, logger *zap.Logger) *AdminReportHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminReportHandler{svc: svc, logger: logger}
}

// RegisterAdminRoutes mounts the report CRUD routes under the given (admin-gated) group.
func (h *AdminReportHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/reports", h.List)
	rg.GET("/admin/reports/:id", h.Get)
	rg.POST("/admin/reports", h.Create)
	rg.PUT("/admin/reports/:id", h.Update)
	rg.DELETE("/admin/reports/:id", h.Delete)
}

// RegisterRunRoutes mounts the report RUN + EXPORT routes under the given (protected, non-admin)
// group so a report can be run/exported by any authenticated user with the data scoped to what
// they may read. It must be registered BEFORE the dynamic /:entity dispatcher so the static
// "reports" segment resolves ahead of the catch-all param route.
func (h *AdminReportHandler) RegisterRunRoutes(rg *gin.RouterGroup) {
	rg.GET("/reports/:id/run", h.Run)
	// W4: server-side CSV export of a saved report — honors the SAME ?dateFrom/?dateTo/?ownerId
	// overlay as /run so the export always matches the on-screen result.
	rg.GET("/reports/:id/export.csv", h.Export)
}

// RegisterCatalogRoutes mounts the non-admin report catalog read under the given protected group.
// GET /reports lists all active reports in the tenant so any authenticated user can discover and
// run them. It is NOT admin-gated — reports are visible to any tenant member. Must be registered
// BEFORE the dynamic /:entity dispatcher so the static "reports" segment resolves first.
func (h *AdminReportHandler) RegisterCatalogRoutes(rg *gin.RouterGroup) {
	rg.GET("/reports", h.Catalog)
}

// Catalog handles GET /reports: returns all tenant reports (id, name, entityName, type, isActive)
// so any authenticated user can discover available reports. Data is tenant-scoped.
func (h *AdminReportHandler) Catalog(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListReports(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminReport.Catalog", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê report", "code": "REPORT_LIST_FAILED"})
		return
	}
	// REPORT-CATALOG-ACL (WAVE 6): don't show a door the caller cannot open. A staff member used
	// to see every report of the tenant, click one over an entity they had no view grant on, and
	// hit FORBIDDEN — a dead end the catalog itself created, and the sibling of the
	// METADATA-ACL-FILTER leak (a report row also NAMES its entity). Filter on the PRIMARY entity
	// with the same decider RequireACL/GetNav/metadata use; the run endpoint keeps enforcing for
	// anything that slips through. Admin bypass + fail-closed follow canSeeEntity exactly.
	if h.entityACL != nil {
		sub, _ := middleware.SubjectFromContext(c)
		if !sub.IsAdmin {
			visible := rows[:0]
			for _, r := range rows {
				ok, aerr := h.entityACL.CanViewEntity(c.Request.Context(), tenantID, sub.RoleID, r.EntityName)
				if aerr != nil {
					h.logger.Error("handlers.AdminReport.Catalog: acl",
						zap.String("tenant_id", tenantID), zap.String("entity", r.EntityName), zap.Error(aerr))
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê report", "code": "REPORT_LIST_FAILED"})
					return
				}
				if ok {
					visible = append(visible, r)
				}
			}
			rows = visible
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"reports": rows}})
}

// reportFilterRequest is one filter predicate in a report config body.
type reportFilterRequest struct {
	Field  string   `json:"field"`
	Op     string   `json:"op,omitempty"`
	Values []string `json:"values,omitempty"`
}

// reportSortRequest is the optional sort of a tabular report config.
type reportSortRequest struct {
	Field string `json:"field"`
	Dir   string `json:"dir,omitempty"`
}

// reportJoinRequest is one cross-object hop of a report config body (REPORT-JOIN v1: ≤1,
// FK-on-child, summary/matrix only — validated in the service).
type reportJoinRequest struct {
	Relationship string `json:"relationship"`
	Optional     bool   `json:"optional,omitempty"`
}

// reportGroupRequest is one extra GROUP BY axis of a report config (specs/049).
type reportGroupRequest struct {
	Field  string `json:"field"`
	Bucket string `json:"bucket,omitempty"`
}

// reportHavingRequest is a report threshold (specs/049 S5): on value|count, an operator, 1–2 values.
type reportHavingRequest struct {
	On     string   `json:"on"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// reportConfigRequest is the config block of a report body (columns/filters/groupBy/metric/field/bucket/sort/joins/columnGroup/subGroups/having).
type reportConfigRequest struct {
	Columns     []string              `json:"columns,omitempty"`
	Filters     []reportFilterRequest `json:"filters,omitempty"`
	GroupBy     string                `json:"groupBy,omitempty"`
	Metric      string                `json:"metric,omitempty"`
	Field       string                `json:"field,omitempty"`
	Bucket      string                `json:"bucket,omitempty"` // W4: day|week|month|quarter|year (summary/matrix)
	Sort        *reportSortRequest    `json:"sort,omitempty"`
	Joins       []reportJoinRequest   `json:"joins,omitempty"`       // REPORT-JOIN v1
	ColumnGroup *reportGroupRequest   `json:"columnGroup,omitempty"` // specs/049 S2b: the matrix column axis
	SubGroups   []reportGroupRequest  `json:"subGroups,omitempty"`   // specs/049 S3: a summary's next levels (≤ 2)
	Having      *reportHavingRequest  `json:"having,omitempty"`      // specs/049 S5: threshold on the leaf groups
}

// reportRequest is the POST/PUT body for a report. isActive defaults to true when omitted.
type reportRequest struct {
	Name       string               `json:"name"`
	EntityName string               `json:"entityName"`
	Type       string               `json:"type"`
	Config     *reportConfigRequest `json:"config"`
	IsActive   *bool                `json:"isActive,omitempty"`
}

// toServiceConfig maps the request config block to the service input type.
func toServiceConfig(c *reportConfigRequest) services.ReportConfig {
	if c == nil {
		return services.ReportConfig{}
	}
	cfg := services.ReportConfig{
		Columns: c.Columns,
		GroupBy: c.GroupBy,
		Metric:  c.Metric,
		Field:   c.Field,
		Bucket:  c.Bucket,
	}
	for _, f := range c.Filters {
		cfg.Filters = append(cfg.Filters, services.ReportFilterInput{
			Field:  f.Field,
			Op:     f.Op,
			Values: f.Values,
		})
	}
	if c.Sort != nil {
		cfg.Sort = &services.ReportSortInput{Field: c.Sort.Field, Dir: c.Sort.Dir}
	}
	for _, j := range c.Joins {
		cfg.Joins = append(cfg.Joins, services.ReportJoinInput{Relationship: j.Relationship, Optional: j.Optional})
	}
	if c.ColumnGroup != nil {
		cfg.ColumnGroup = &services.ReportGroupInput{Field: c.ColumnGroup.Field, Bucket: c.ColumnGroup.Bucket}
	}
	for _, g := range c.SubGroups {
		cfg.SubGroups = append(cfg.SubGroups, services.ReportGroupInput{Field: g.Field, Bucket: g.Bucket})
	}
	if c.Having != nil {
		cfg.Having = &services.ReportHavingInput{On: c.Having.On, Op: c.Having.Op, Values: c.Having.Values}
	}
	return cfg
}

// List handles GET /admin/reports: returns every report in the tenant.
func (h *AdminReportHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListReports(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminReport.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê report", "code": "REPORT_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"reports": rows}})
}

// Get handles GET /admin/reports/:id: returns one tenant-scoped report, 404 unknown.
func (h *AdminReportHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	rec, err := h.svc.GetReport(c.Request.Context(), tenantID, id)
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"report": rec}})
}

// Create handles POST /admin/reports: validates and saves a report. Invalid type/config or a
// missing name/entity is 422; an unknown entity is 422. The created report is returned with 201.
func (h *AdminReportHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req reportRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.svc.CreateReport(auditContext(c), tenantID, services.CreateReportInput{
		Name:       req.Name,
		EntityName: req.EntityName,
		Type:       req.Type,
		Config:     toServiceConfig(req.Config),
		IsActive:   req.IsActive,
		ActorID:    actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	jsonWV(c, http.StatusCreated, gin.H{"data": gin.H{"report": rec}})
}

// Update handles PUT /admin/reports/:id: full-replace of a report. Unknown id is 404; invalid 422.
func (h *AdminReportHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req reportRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.svc.UpdateReport(auditContext(c), tenantID, id, services.UpdateReportInput{
		Name:       req.Name,
		EntityName: req.EntityName,
		Type:       req.Type,
		Config:     toServiceConfig(req.Config),
		IsActive:   req.IsActive,
		ActorID:    actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"report": rec}})
}

// Delete handles DELETE /admin/reports/:id: soft-deletes the report. Unknown id is 404.
func (h *AdminReportHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := h.svc.DeleteReport(auditContext(c), tenantID, id); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	noContentWV(c)
}

// Run handles GET /reports/:id/run: executes a saved report against LIVE records, scoped by
// tenant + A5 row-level access (same as a list/aggregate request).
//
// Optional W4 query params (persist-nothing overlay ANDed onto the saved filters — identical
// dialect to the dashboard run): ?dateFrom=YYYY-MM-DD, ?dateTo=YYYY-MM-DD, ?ownerId=<userId>.
// Invalid dates → 422 RUN_DATE_INVALID; dateFrom > dateTo → 422 RUN_DATE_RANGE_INVALID.
//
// The response shape depends on the report type:
//   - tabular:          {"data":{"type":"tabular","columns":[{"name":..,"label":..}],
//     "rows":[{"id":..,<col>:..}],"total":N}} — see api-conventions.md §4.4b: a column whose field
//     was deleted AND a column hidden by field-ACL both fall back to label = name, deliberately
//     indistinguishable.
//   - summary / matrix: {"data":{"type":"summary","groupBy":<field|null>,"groups":[...]}} (grouped)
//     or {"data":{"type":"summary","value":..,"count":..}} (scalar — no groupBy).
//   - matrix with config.columnGroup (specs/049 S2b): {"data":{"type":"matrix","rowAxis":{field,bucket},
//     "columnAxis":{field,bucket},"rows":[{key,value,count}],"columns":[...],"cells":[{r,c,value,count}],
//     "total":{value,count},"groups":<= rows>,"currencyCode":..}} — api-conventions.md §4.4e.
//   - summary with config.subGroups (specs/049 S3): {"data":{"type":"summary","levels":[{field,bucket}],
//     "groups":[{key,value,count,"children":[...]}],"total":{value,count},"currencyCode":..}} — the
//     level-1 groups keep the one-level shape; a leaf carries no "children".
//   - summary / matrix with config.having (specs/049 S5): the tree (a ONE-level tree when there is no
//     second axis) or the cross-tab of the kept leaf groups; both grid shapes echo "having" (null
//     when none).
//
// When overlay params were sent, data additionally carries "appliedFilters" echoing them.
// A config that no longer matches the entity's metadata (a field removed since save) surfaces as a
// clean 422 from the underlying list/aggregate engine.
func (h *AdminReportHandler) Run(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	overlay, err := services.ParseRunOverlay(c.Query("dateFrom"), c.Query("dateTo"), c.Query("ownerId"))
	if err != nil {
		h.respondRunError(c, err, tenantID)
		return
	}
	res, err := h.svc.RunReport(c.Request.Context(), scopeOf(c), tenantID, id, overlay)
	if err != nil {
		h.respondRunError(c, err, tenantID)
		return
	}

	var data gin.H
	switch {
	case res.Tabular != nil:
		rows := make([]gin.H, 0, len(res.Tabular.Records))
		for _, r := range res.Tabular.Records {
			row := gin.H{"id": r.ID}
			// Project to the report's columns; a missing column key yields null.
			for _, col := range res.Tabular.Columns {
				row[col] = r.Data[col]
			}
			rows = append(rows, row)
		}
		// The viewer reads `col.label || col.name` for the heading and `row[col.name]` for the
		// cell, so `columns` goes out as objects — a bare string leaves every heading blank and
		// every cell "—" on screen. A column whose field no longer exists keeps its slot with
		// label = name: dropping it would change the column count under the rendered rows.
		cols := make([]gin.H, 0, len(res.Tabular.Columns))
		for _, col := range res.Tabular.Columns {
			label := res.Tabular.ColumnLabels[col]
			if label == "" {
				label = col
			}
			cols = append(cols, gin.H{"name": col, "label": label})
		}
		data = gin.H{
			"type":    "tabular",
			"columns": cols,
			"rows":    rows,
			"total":   res.Tabular.Total,
		}
	case res.Grid != nil && res.Grid.Kind == services.GridKindMatrix && len(res.Axes) == 2:
		// specs/049 S2b: a matrix with a column axis — rows (row totals) and columns (column
		// totals) in display order, sparse cells addressed by index, the grand total. `groups`
		// repeats the rows so a single-axis reader (older client, chart) still reads the row totals.
		grid := res.Grid
		rows := groupRowsJSON(grid.Rows)
		cells := make([]gin.H, 0, len(grid.Cells))
		for _, c := range grid.Cells {
			cells = append(cells, gin.H{"r": c.R, "c": c.C, "value": c.Value, "count": c.Count})
		}
		data = gin.H{
			"type":         string(res.Type),
			"rowAxis":      gin.H{"field": res.Axes[0].Field, "bucket": res.Axes[0].Bucket},
			"columnAxis":   gin.H{"field": res.Axes[1].Field, "bucket": res.Axes[1].Bucket},
			"rows":         rows,
			"columns":      groupRowsJSON(grid.Columns),
			"cells":        cells,
			"total":        gin.H{"value": grid.Total.Value, "count": grid.Total.Count},
			"groups":       rows,
			"having":       res.Having,
			"currencyCode": grid.CurrencyCode,
		}
	case res.Grid != nil && res.Grid.Kind == services.GridKindTree:
		// specs/049 S3: a summary with sub-levels — the levels in order, the level-1 groups in the
		// one-level {key,value,count} shape (charts and older readers keep working) with their
		// sub-totals nested as children, and the grand total.
		levels := make([]gin.H, 0, len(res.Axes))
		for _, ax := range res.Axes {
			levels = append(levels, gin.H{"field": ax.Field, "bucket": ax.Bucket})
		}
		data = gin.H{
			"type":         string(res.Type),
			"levels":       levels,
			"groups":       treeGroupsJSON(res.Grid.Groups),
			"total":        gin.H{"value": res.Grid.Total.Value, "count": res.Grid.Total.Count},
			"having":       res.Having,
			"currencyCode": res.Grid.CurrencyCode,
		}
	case res.Aggregate != nil:
		agg := res.Aggregate
		if agg.Grouped {
			data = gin.H{
				"type":         string(res.Type),
				"groups":       groupRowsJSON(agg.Groups),
				"currencyCode": agg.CurrencyCode,
			}
		} else {
			data = gin.H{
				"type":         string(res.Type),
				"value":        agg.Scalar.Value,
				"count":        agg.Scalar.Count,
				"currencyCode": agg.CurrencyCode,
			}
		}
	default:
		// Defensive: a run always yields one of the two shapes.
		h.logger.Error("handlers.AdminReport.Run: empty result", zap.String("tenant_id", tenantID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể chạy report", "code": "REPORT_RUN_FAILED"})
		return
	}
	if af := appliedFiltersOf(overlay); af != nil {
		data["appliedFilters"] = af
	}
	if res.DraftState != "" {
		data["draftState"] = res.DraftState // the viewer says "bản nháp · khách chưa thấy" (specs/063 S7-2)
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// groupRowsJSON renders aggregate groups as the {key,value,count} objects of a run response — the
// summary groups and the matrix rows/columns share it.
func groupRowsJSON(gs []services.AggregateGroupResult) []gin.H {
	out := make([]gin.H, 0, len(gs))
	for _, g := range gs {
		out = append(out, groupJSON(g))
	}
	return out
}

// groupJSON is one group of a run response: {key,value,count}, plus "label" — the name to show
// for the key (specs/049 S6: tên khách hàng, "Báo giá") — only when the run named it. A group
// without a label has no label key at all, so a client reads `label ?? key` and filters by key.
func groupJSON(g services.AggregateGroupResult) gin.H {
	out := gin.H{"key": g.Key, "value": g.Value, "count": g.Count}
	if g.Label != nil {
		out["label"] = *g.Label
	}
	return out
}

// treeGroupsJSON renders tree groups as {key,value,count[,label]} objects, a node above the
// deepest level carrying its sub-groups as "children" (a leaf has no children key).
func treeGroupsJSON(ns []services.GridNode) []gin.H {
	out := make([]gin.H, 0, len(ns))
	for _, n := range ns {
		g := groupJSON(n.AggregateGroupResult)
		if len(n.Children) > 0 {
			g["children"] = treeGroupsJSON(n.Children)
		}
		out = append(out, g)
	}
	return out
}

// Export handles GET /reports/:id/export.csv (W4): streams the saved report's result as RFC 4180
// CSV, honoring the SAME ?dateFrom/?dateTo/?ownerId overlay as /run — the export is guaranteed to
// cover exactly the rows/groups the on-screen run shows (tabular: same row SET, streamed id ASC
// without the run's 500-row page cap; summary/matrix: the same groups as key,value,count rows; a
// matrix with a column axis / a summary with sub-levels: the cross-tab / tree itself — specs/049 S4).
//
// Errors BEFORE the first byte (unknown report REPORT_NOT_FOUND → 422 via statusForCode default —
// pre-existing asymmetry with dashboard-not-found's 404, pinned in report_export_integration_test;
// inactive 422, invalid overlay 422, invalid config 422, hidden-field filter 403) surface as the
// normal JSON envelope; a mid-stream error is logged and the connection closed (status already
// committed) — mirroring the entity export.
func (h *AdminReportHandler) Export(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	overlay, err := services.ParseRunOverlay(c.Query("dateFrom"), c.Query("dateTo"), c.Query("ownerId"))
	if err != nil {
		h.respondRunError(c, err, tenantID)
		return
	}
	// Pre-resolve the report so 404/tenant-isolation surfaces as clean JSON before any CSV
	// header is set, and so the download filename can carry the (validated-identifier) entity
	// name — the report display name may hold arbitrary text unsafe for a header.
	rep, err := h.svc.GetReport(c.Request.Context(), tenantID, id)
	if err != nil {
		h.respondRunError(c, err, tenantID)
		return
	}

	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=\""+rep.EntityName+"-report.csv\"")
	c.Header("X-Content-Type-Options", "nosniff")

	if err := h.svc.ExportReportCSV(c.Request.Context(), scopeOf(c), tenantID, id, overlay, c.Writer); err != nil {
		if !c.Writer.Written() {
			// Nothing streamed yet — reset to a JSON error envelope (inactive/config/ACL errors).
			c.Writer.Header().Del("Content-Disposition")
			c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			h.respondRunError(c, err, tenantID)
			return
		}
		h.logger.Error("handlers.AdminReport.Export: mid-stream", zap.String("tenant_id", tenantID), zap.Error(err))
	}
}

// respondServiceError maps a report-service error to the right HTTP status: a ValidationError →
// its code's status (404/422 via statusForCode); anything else → a logged 500.
func (h *AdminReportHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		body := gin.H{"error": ve.Msg, "code": ve.Code}
		// A field-bound config error names its field / axis (REPORT_HAVING_INVALID {field, reason},
		// details.axis) so the config panel can point at it; errors without details keep their bytes.
		if len(ve.Details) > 0 {
			body["details"] = ve.Details
		}
		c.JSON(statusForCode(ve.Code), body)
		return
	}
	h.logger.Error("handlers.AdminReport."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu report", "code": "REPORT_OP_FAILED"})
}

// respondRunError maps a RunReport error. The report itself can be NOT_FOUND/INACTIVE (report
// service), or the underlying list/aggregate engine can reject the saved config (ENTITY_NOT_FOUND
// 404, or an AGGREGATE_*/LIST_* validation 422). ValidationError carries the code; ErrEntityNotFound
// → 404.
func (h *AdminReportHandler) respondRunError(c *gin.Context, err error, tenantID string) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity của report không tồn tại", "code": "ENTITY_NOT_FOUND"})
	default:
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.AdminReport.Run", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể chạy report", "code": "REPORT_RUN_FAILED"})
	}
}
