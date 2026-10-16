package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// AdminDashboardHandler exposes the EPIC 6 Dashboard M2 endpoints. The CRUD surface
// (/admin/dashboards) sits behind the AuthMiddleware → RequireTenant → RequireAdmin chain (an admin
// builds saved dashboards); the RUN surface (/dashboards/:id/run) sits on the protected (non-admin)
// group because running a dashboard returns tenant + A5 row-scoped DATA the caller may read — it is
// a read for any authenticated user, governed by the same row-scope as an aggregate request, not
// the coarse admin gate.
type AdminDashboardHandler struct {
	svc    *services.DashboardService
	logger *zap.Logger
}

// NewAdminDashboardHandler wires the handler over the dashboard service.
func NewAdminDashboardHandler(svc *services.DashboardService, logger *zap.Logger) *AdminDashboardHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminDashboardHandler{svc: svc, logger: logger}
}

// RegisterAdminRoutes mounts the dashboard CRUD routes under the given (admin-gated) group.
func (h *AdminDashboardHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/dashboards", h.List)
	rg.GET("/admin/dashboards/:id", h.Get)
	rg.POST("/admin/dashboards", h.Create)
	rg.PUT("/admin/dashboards/:id", h.Update)
	rg.DELETE("/admin/dashboards/:id", h.Delete)
}

// RegisterRunRoutes mounts the dashboard RUN route under the given (protected, non-admin) group so
// a dashboard can be run by any authenticated user with the data scoped to what they may read. It
// must be registered BEFORE the dynamic /:entity dispatcher so the static "dashboards" segment
// resolves ahead of the catch-all param route.
func (h *AdminDashboardHandler) RegisterRunRoutes(rg *gin.RouterGroup) {
	rg.GET("/dashboards/:id/run", h.Run)
}

// RegisterCatalogRoutes mounts the non-admin dashboard catalog read under the given protected group.
// GET /dashboards lists all dashboards in the tenant so any authenticated user can discover and
// navigate to them. It is NOT admin-gated — dashboards are viewable by any tenant member. Must be
// registered BEFORE the dynamic /:entity dispatcher so the static "dashboards" segment resolves
// ahead of the catch-all param route.
func (h *AdminDashboardHandler) RegisterCatalogRoutes(rg *gin.RouterGroup) {
	rg.GET("/dashboards", h.Catalog)
}

// Catalog handles GET /dashboards: returns the list of all tenant dashboards (id, name, description)
// so any authenticated user can discover available dashboards. Data is tenant-scoped.
func (h *AdminDashboardHandler) Catalog(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListVisibleDashboards(c.Request.Context(), scopeOf(c), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminDashboard.Catalog", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê dashboard", "code": "DASHBOARD_LIST_FAILED"})
		return
	}
	// PROJECT to exactly what the doc comment above always promised. The handler used to return the
	// repository row whole, and that row carries `widgets` — every widget's entity name, metric and
	// filters. This route is open to any authenticated user by design (discovery), so a grantless
	// employee could read the shape of tables they have no grant on, from a list endpoint whose
	// stated contract is "id, name, description". Projecting here fixes the leak at its source and
	// costs nothing: the detail route (GET /dashboards/:id/run) is where widgets legitimately
	// travel, and it is the one that resolves each widget's entity under the caller's ACL.
	out := make([]dashboardCatalogItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, dashboardCatalogItem{
			ID: r.ID, Name: r.Name, Description: r.Description,
			Widgets: catalogTiles(r.Widgets), DraftState: r.DraftState,
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"dashboards": out}})
}

// dashboardCatalogItem is the safe projection of a dashboard row for the OPEN discovery list
// (GET /dashboards is authenticated but ungranted by design, so any employee can find a dashboard).
//
// The row it replaces carried `widgets` WHOLE: each widget's entity, metric, field, groupBy and
// filters. That let a caller with no grant on those tables read their names and the shape of what
// is measured on them, from a list endpoint whose own doc comment promised "id, name, description".
//
// It is a PROJECTION, not a removal, because the customer-facing /dashboards page legitimately
// renders a widget count and a type preview — cutting the array outright would have shown every
// dashboard as empty. So each widget keeps only what that page draws (id + type) and loses
// everything that names the data underneath.
type dashboardCatalogItem struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Description *string                `json:"description,omitempty"`
	Widgets     []dashboardCatalogTile `json:"widgets"`
	// DraftState: only on the builder's draft-overlay read (specs/063 S7-2); omitted for everyone else.
	DraftState string `json:"draftState,omitempty"`
}

// dashboardCatalogTile is a widget with the schema stripped out: enough to count and preview,
// nothing that names an entity, a metric or a filter.
type dashboardCatalogTile struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// catalogTiles strips a stored widgets array down to {id, type}. A malformed/absent blob yields an
// empty list rather than an error: discovery must not fail because one dashboard has odd config.
func catalogTiles(raw json.RawMessage) []dashboardCatalogTile {
	if len(raw) == 0 {
		return []dashboardCatalogTile{}
	}
	var all []dashboardCatalogTile
	if err := json.Unmarshal(raw, &all); err != nil {
		return []dashboardCatalogTile{}
	}
	return all
}

// dashboardFilterRequest is one filter predicate in a widget config body.
type dashboardFilterRequest struct {
	Field  string   `json:"field"`
	Op     string   `json:"op,omitempty"`
	Values []string `json:"values,omitempty"`
}

// widgetRequest is one widget in the dashboard body. Type is stored as-is (render hint for FE).
type widgetRequest struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`
	Entity  string `json:"entity"`
	Metric  string `json:"metric"`
	Field   string `json:"field,omitempty"`
	GroupBy string `json:"groupBy,omitempty"`
	Bucket  string `json:"bucket,omitempty"` // W4: day|week|month|quarter|year over a date GroupBy
	// DASH-WIDGET-SCHEMA: react-grid-layout coordinates (bounds [0,48], validated in the service).
	// Persisted + returned so the FE moves dashboard grid layout off per-browser localStorage.
	X       int                      `json:"x"`
	Y       int                      `json:"y"`
	W       int                      `json:"w"`
	H       int                      `json:"h"`
	Filters []dashboardFilterRequest `json:"filters,omitempty"`
	Joins   []reportJoinRequest      `json:"joins,omitempty"` // queue #294: same shape as a report's config.joins
}

// dashboardRequest is the POST/PUT body for a dashboard.
type dashboardRequest struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Widgets     []widgetRequest `json:"widgets"`
}

// toServiceWidgets maps the request widget slice to the service WidgetConfig slice.
func toServiceWidgets(ws []widgetRequest) []services.WidgetConfig {
	if len(ws) == 0 {
		return []services.WidgetConfig{}
	}
	out := make([]services.WidgetConfig, 0, len(ws))
	for _, w := range ws {
		wc := services.WidgetConfig{
			ID:      w.ID,
			Title:   w.Title,
			Type:    w.Type,
			Entity:  w.Entity,
			Metric:  w.Metric,
			Field:   w.Field,
			GroupBy: w.GroupBy,
			Bucket:  w.Bucket,
			X:       w.X,
			Y:       w.Y,
			W:       w.W,
			H:       w.H,
		}
		for _, j := range w.Joins {
			wc.Joins = append(wc.Joins, services.ReportJoinInput{Relationship: j.Relationship, Optional: j.Optional})
		}
		for _, f := range w.Filters {
			wc.Filters = append(wc.Filters, services.ReportFilterInput{
				Field:  f.Field,
				Op:     f.Op,
				Values: f.Values,
			})
		}
		out = append(out, wc)
	}
	return out
}

// List handles GET /admin/dashboards: returns every dashboard in the tenant.
func (h *AdminDashboardHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rows, err := h.svc.ListDashboards(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.AdminDashboard.List", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể liệt kê dashboard", "code": "DASHBOARD_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"dashboards": rows}})
}

// Get handles GET /admin/dashboards/:id: returns one tenant-scoped dashboard, 404 for unknown.
func (h *AdminDashboardHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	rec, err := h.svc.GetDashboard(c.Request.Context(), tenantID, id)
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"dashboard": rec}})
}

// Create handles POST /admin/dashboards: validates and saves a dashboard. Invalid widget type/metric
// or a missing name is 422. The created dashboard is returned with 201.
func (h *AdminDashboardHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req dashboardRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.svc.CreateDashboard(auditContext(c), tenantID, services.CreateDashboardInput{
		Name:        req.Name,
		Description: req.Description,
		Widgets:     toServiceWidgets(req.Widgets),
		ActorID:     actorOf(c),
	})
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	jsonWV(c, http.StatusCreated, gin.H{"data": gin.H{"dashboard": rec}})
}

// Update handles PUT /admin/dashboards/:id: full-replace of a dashboard. Unknown id is 404; invalid 422.
func (h *AdminDashboardHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	var req dashboardRequest
	if !bindJSON(c, &req) {
		return
	}
	rec, err := h.svc.UpdateDashboard(auditContext(c), tenantID, id, services.UpdateDashboardInput{
		Name:        req.Name,
		Description: req.Description,
		Widgets:     toServiceWidgets(req.Widgets),
	})
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	jsonWV(c, http.StatusOK, gin.H{"data": gin.H{"dashboard": rec}})
}

// Delete handles DELETE /admin/dashboards/:id: soft-deletes the dashboard. Unknown id is 404.
func (h *AdminDashboardHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if err := h.svc.DeleteDashboard(auditContext(c), tenantID, id); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	noContentWV(c)
}

// Run handles GET /dashboards/:id/run: executes each widget of the dashboard against LIVE records,
// scoped by tenant + A5 row-level access. Per-widget errors are returned in the response body;
// the overall HTTP status is always 200 when the dashboard itself was found.
//
// Optional W4 query params (persist-nothing overlay ANDed onto EVERY widget's filters):
//
//	?dateFrom=YYYY-MM-DD   created_at >= dateFrom 00:00:00 (inclusive)
//	?dateTo=YYYY-MM-DD     created_at <  dateTo+1d (inclusive whole day)
//	?ownerId=<userId>      created_by = ownerId (exact)
//
// Invalid dates → 422 RUN_DATE_INVALID; dateFrom > dateTo → 422 RUN_DATE_RANGE_INVALID. All
// params absent → behavior identical to a plain run (backward compatible). Response shape:
//
//	{"data": {"dashboard": {...}, "widgets": [{"widgetId":"..","title":"..","type":"..","result":{...}|null,"error":"..","code":".."}],
//	          "appliedFilters": {"dateFrom":"..","dateTo":"..","ownerId":".."}}}  // only when params were sent
func (h *AdminDashboardHandler) Run(c *gin.Context) {
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
	res, err := h.svc.RunDashboard(c.Request.Context(), scopeOf(c), tenantID, id, overlay)
	if err != nil {
		h.respondRunError(c, err, tenantID)
		return
	}

	// Build per-widget response items. result is null (omitted) on error.
	widgetItems := make([]gin.H, 0, len(res.Widgets))
	for _, w := range res.Widgets {
		item := gin.H{
			"widgetId": w.WidgetID,
			"title":    w.Title,
			"type":     w.Type,
		}
		if w.Result != nil {
			// Shape the aggregate result inline: grouped or scalar.
			agg := w.Result
			if agg.Grouped {
				groups := make([]gin.H, 0, len(agg.Groups))
				for _, g := range agg.Groups {
					groups = append(groups, gin.H{"key": g.Key, "value": g.Value, "count": g.Count})
				}
				item["result"] = gin.H{"grouped": true, "groups": groups, "currencyCode": agg.CurrencyCode}
			} else {
				item["result"] = gin.H{"grouped": false, "value": agg.Scalar.Value, "count": agg.Scalar.Count, "currencyCode": agg.CurrencyCode}
			}
		} else {
			item["result"] = nil
			item["error"] = w.Error
			item["code"] = w.Code
		}
		widgetItems = append(widgetItems, item)
	}

	data := gin.H{
		"dashboard": res.Dashboard,
		"widgets":   widgetItems,
	}
	if af := appliedFiltersOf(overlay); af != nil {
		data["appliedFilters"] = af
	}
	c.JSON(http.StatusOK, gin.H{"data": data})
}

// appliedFiltersOf echoes the validated overlay back to the client (only the params that were
// actually sent) so the FE can render honest filter state. nil overlay → nil (key omitted).
func appliedFiltersOf(o *services.RunOverlay) gin.H {
	if o == nil {
		return nil
	}
	af := gin.H{}
	if o.DateFrom != "" {
		af["dateFrom"] = o.DateFrom
	}
	if o.DateTo != "" {
		af["dateTo"] = o.DateTo
	}
	if o.OwnerID != "" {
		af["ownerId"] = o.OwnerID
	}
	return af
}

// respondServiceError maps a dashboard-service error to the right HTTP status. A ValidationError
// with code DASHBOARD_NOT_FOUND → 404; anything else → statusForCode (422/409/etc.) or logged 500.
func (h *AdminDashboardHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		// DASHBOARD_NOT_FOUND is not in statusForCode's explicit list; handle it here.
		if ve.Code == "DASHBOARD_NOT_FOUND" {
			c.JSON(http.StatusNotFound, gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	h.logger.Error("handlers.AdminDashboard."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu dashboard", "code": "DASHBOARD_OP_FAILED"})
}

// respondRunError maps a RunDashboard error. The dashboard itself can be NOT_FOUND (dashboard
// service); ENTITY_NOT_FOUND from the underlying aggregate engine → 404. Per-widget failures live
// in the response body (200), not here; only dashboard-level errors reach this path.
func (h *AdminDashboardHandler) respondRunError(c *gin.Context, err error, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		if ve.Code == "DASHBOARD_NOT_FOUND" {
			c.JSON(http.StatusNotFound, gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	if errors.Is(err, services.ErrEntityNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
		return
	}
	h.logger.Error("handlers.AdminDashboard.Run", zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể chạy dashboard", "code": "DASHBOARD_RUN_FAILED"})
}
