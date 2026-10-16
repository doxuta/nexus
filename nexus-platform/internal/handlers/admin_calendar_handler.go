package handlers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/calendar"
	"nexus/platform/internal/services"
)

// AdminCalendarHandler exposes the business-hours calendar admin CRUD (CALENDAR-1):
// /admin/business-calendars behind Auth → Tenant → RequireAdmin, mirroring the
// AdminAppHandler wiring. Set-default rides the isDefault flag of POST/PUT (the repo
// clears the previous default in the same transaction) — no separate endpoint.
type AdminCalendarHandler struct {
	svc    *services.BusinessCalendarService
	logger *zap.Logger
}

// NewAdminCalendarHandler wires the handler over the calendar service.
func NewAdminCalendarHandler(svc *services.BusinessCalendarService, logger *zap.Logger) *AdminCalendarHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &AdminCalendarHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the calendar CRUD under the given (admin-gated) group.
func (h *AdminCalendarHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/business-calendars", h.List)
	rg.GET("/admin/business-calendars/:id", h.Get)
	rg.POST("/admin/business-calendars", h.Create)
	rg.PUT("/admin/business-calendars/:id", h.Update)
	rg.DELETE("/admin/business-calendars/:id", h.Delete)
}

// calendarRequest is the POST/PUT body. hours keys are mon..sun; times LOCAL "HH:MM"
// (end may be "24:00"); holidays "YYYY-MM-DD". Full-replace on PUT.
type calendarRequest struct {
	Name      string                         `json:"name"`
	Timezone  string                         `json:"timezone"`
	Hours     map[string][]calendar.Interval `json:"hours"`
	Holidays  []string                       `json:"holidays,omitempty"`
	IsDefault bool                           `json:"isDefault,omitempty"`
}

// input maps the request body plus the verified actor to the service payload.
func (r *calendarRequest) input(actorID string) services.CalendarInput {
	return services.CalendarInput{
		Name:      r.Name,
		Timezone:  r.Timezone,
		Hours:     r.Hours,
		Holidays:  r.Holidays,
		IsDefault: r.IsDefault,
		ActorID:   actorID,
	}
}

// List handles GET /admin/business-calendars.
func (h *AdminCalendarHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	cals, err := h.svc.ListCalendars(c.Request.Context(), tenantID)
	if err != nil {
		h.respondServiceError(c, err, "List", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"calendars": cals}})
}

// Get handles GET /admin/business-calendars/:id.
func (h *AdminCalendarHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	cal, err := h.svc.GetCalendar(c.Request.Context(), tenantID, c.Param("id"))
	if err != nil {
		h.respondServiceError(c, err, "Get", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"calendar": cal}})
}

// Create handles POST /admin/business-calendars → 201.
func (h *AdminCalendarHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req calendarRequest
	if !bindJSON(c, &req) {
		return
	}
	cal, err := h.svc.CreateCalendar(c.Request.Context(), tenantID, req.input(actorOf(c)))
	if err != nil {
		h.respondServiceError(c, err, "Create", tenantID)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"calendar": cal}})
}

// Update handles PUT /admin/business-calendars/:id (full-replace). Unknown id → 404.
func (h *AdminCalendarHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req calendarRequest
	if !bindJSON(c, &req) {
		return
	}
	cal, err := h.svc.UpdateCalendar(c.Request.Context(), tenantID, c.Param("id"), req.input(actorOf(c)))
	if err != nil {
		h.respondServiceError(c, err, "Update", tenantID)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"calendar": cal}})
}

// Delete handles DELETE /admin/business-calendars/:id → 204 (soft delete).
func (h *AdminCalendarHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.DeleteCalendar(c.Request.Context(), tenantID, c.Param("id")); err != nil {
		h.respondServiceError(c, err, "Delete", tenantID)
		return
	}
	c.Status(http.StatusNoContent)
}

// respondServiceError maps a calendar-service error to the right HTTP status: a
// ValidationError goes through the shared statusForCode (CALENDAR_NOT_FOUND → 404,
// CALENDAR_ALREADY_EXISTS → 409, CALENDAR_* validation → 422); anything else is a
// logged 500.
func (h *AdminCalendarHandler) respondServiceError(c *gin.Context, err error, op, tenantID string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.logger.Error("handlers.AdminCalendar."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý yêu cầu lịch làm việc", "code": "CALENDAR_OP_FAILED"})
}
