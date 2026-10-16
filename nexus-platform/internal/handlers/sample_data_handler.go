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

// SampleDataHandler serves the builder's sample-data routes (specs/064-preview-as-role-sample-data
// S9-4), both mounted on builderGated (Auth → Tenant → PreviewReadOnly → RequireAdmin → operator
// tier), so a customer's staff and a legacy customer admin get 403:
//
//   - GET  /admin/sample-data?entities=a,b — how many sample rows each table holds.
//   - POST /admin/sample-data/records/:entity?mode=partial|atomic — POST /:entity/bulk's contract,
//     but the created rows are marked as sample data and trigger no workflow / approval.
//   - POST /admin/sample-data/generate — S9-5: Vietnamese sample rows for the chosen tables, built
//     from their field definitions (no AI), parents first.
//   - POST /admin/sample-data/wipe — S9-7: hard-delete every sample row (live or in the bin) that no
//     real data points at, behind a confirmed total; rows real data points at are kept and reported.
type SampleDataHandler struct {
	svc    *services.SampleDataService
	errs   *DynamicRecordHandler // borrowed only for respondError: one place maps record-write errors
	logger *zap.Logger
}

// NewSampleDataHandler wires the handler over the sample-data service.
func NewSampleDataHandler(svc *services.SampleDataService, logger *zap.Logger) *SampleDataHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &SampleDataHandler{svc: svc, errs: &DynamicRecordHandler{logger: logger}, logger: logger}
}

// RegisterAdminRoutes mounts the routes on the builder-gated group.
func (h *SampleDataHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.GET("/admin/sample-data", h.Summary)
	rg.POST("/admin/sample-data/records/:entity", h.CreateRecords)
	rg.POST("/admin/sample-data/generate", h.Generate)
	rg.POST("/admin/sample-data/wipe", h.Wipe)
}

// Wipe handles POST /admin/sample-data/wipe. Body {"entities":[…]?,"expectTotal":N} → 200
// {data:{deleted:[{entity,count}], kept:[{entity,recordId,reason,blockedBy}], blobsRemoved, blobsNote?,
// sequencesReset:[{entity,field}]}}. 422 SAMPLE_WIPE_CONFIRM_REQUIRED without expectTotal, 409
// SAMPLE_WIPE_STALE {details.current} when the sample rows are not exactly that many (nothing deleted),
// 409 SAMPLE_BUSY. POST rather than DELETE-with-body (precedent: POST /admin/tenants/:id/purge).
func (h *SampleDataHandler) Wipe(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	var body struct {
		Entities    []string `json:"entities"`
		ExpectTotal *int     `json:"expectTotal"`
	}
	if !bindJSON(c, &body) {
		return
	}
	scope := services.RecordScope{UserID: sub.UserID, AccessLevel: "all", IsAdmin: sub.IsAdmin, RoleID: sub.RoleID}
	res, err := h.svc.Wipe(c.Request.Context(), scope, tenantID, sub.UserID,
		services.WipeRequest{Entities: body.Entities, ExpectTotal: body.ExpectTotal})
	if err != nil {
		h.fail(c, err, "Wipe")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": res})
}

// Summary handles GET /admin/sample-data → 200 {data:{total, entities:[{entity,label,count,live,inBin,bySource}]}}.
// 422 SAMPLE_ENTITY_NOT_FOUND for a requested table that is not live.
func (h *SampleDataHandler) Summary(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var entities []string
	for _, e := range strings.Split(c.Query("entities"), ",") {
		if e = strings.TrimSpace(e); e != "" {
			entities = append(entities, e)
		}
	}
	out, err := h.svc.Summary(c.Request.Context(), tenantID, entities)
	if err != nil {
		h.fail(c, err, "Summary")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// CreateRecords handles POST /admin/sample-data/records/:entity. Body {"records":[…]}; the response
// and status mapping are exactly POST /:entity/bulk (201 / 422 atomic, 200 partial). 409 SAMPLE_BUSY
// while another sample batch of the tenant runs. No Idempotency-Key: a resend only seeds more samples.
func (h *SampleDataHandler) CreateRecords(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	var body struct {
		Records []map[string]any `json:"records"`
	}
	if !bindJSON(c, &body) {
		return
	}
	mode := services.BulkModeAtomic
	if raw := strings.TrimSpace(c.Query("mode")); raw != "" {
		mode = services.BulkMode(raw)
	}
	// builderGated admits only an admin, who bypasses row ACL: the scope states that outright
	// instead of leaning on a RequireACL stage this route does not run.
	scope := services.RecordScope{UserID: sub.UserID, AccessLevel: "all", IsAdmin: sub.IsAdmin, RoleID: sub.RoleID}
	res, err := h.svc.CreateRecords(c.Request.Context(), scope, tenantID, sub.UserID, c.Param("entity"), body.Records, mode)
	if err != nil {
		h.fail(c, err, "CreateRecords")
		return
	}
	c.JSON(statusForBulkResult(mode, res), gin.H{"data": res})
}

// Generate handles POST /admin/sample-data/generate. Body {"entities":[…],"perEntity":1..20,"source":
// "fields"|"pack"|"auto"} (absent = fields) → 201 {data:{batchId, created, failed, skipped, addedParents}}.
// 422 SAMPLE_PER_ENTITY_INVALID / SAMPLE_SOURCE_INVALID / SAMPLE_ENTITY_NOT_FOUND / SAMPLE_PACK_UNAVAILABLE,
// 409 SAMPLE_BUSY. A row the record layer refuses
// is reported under "failed", not as an HTTP error. Not idempotent on purpose: sending it again
// seeds more rows.
func (h *SampleDataHandler) Generate(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	var body struct {
		Entities  []string `json:"entities"`
		PerEntity int      `json:"perEntity"`
		Source    string   `json:"source"`
	}
	if !bindJSON(c, &body) {
		return
	}
	scope := services.RecordScope{UserID: sub.UserID, AccessLevel: "all", IsAdmin: sub.IsAdmin, RoleID: sub.RoleID}
	res, err := h.svc.Generate(c.Request.Context(), scope, tenantID, sub.UserID,
		services.GenerateRequest{Entities: body.Entities, PerEntity: body.PerEntity, Source: body.Source})
	if err != nil {
		h.fail(c, err, "Generate")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": res})
}

// fail maps a service error: the SAMPLE_* codes here, every record-write error through the shared
// dynamic-record mapping (validation 422, write conflict 503, DB timeout 503, otherwise 500).
func (h *SampleDataHandler) fail(c *gin.Context, err error, op string) {
	var ve *services.ValidationError
	if errors.As(err, &ve) && strings.HasPrefix(ve.Code, "SAMPLE_") {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
		return
	}
	h.errs.respondError(c, err, "SampleData."+op)
}
