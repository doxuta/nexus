package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/services"
)

// importServiceI is the interface the ImportHandler depends on, declared on the consumer
// side so the handler can be tested with a stub.
type importServiceI interface {
	Import(ctx context.Context, in services.ImportInput) (*services.ImportResult, error)
	ImportIdempotent(ctx context.Context, in services.ImportInput, key string) (*services.ImportResult, json.RawMessage, error)
}

// ImportHandler handles POST /:entity/import.
type ImportHandler struct {
	svc    importServiceI
	logger *zap.Logger
}

// NewImportHandler wires the handler over the import service.
func NewImportHandler(svc importServiceI, logger *zap.Logger) *ImportHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ImportHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts POST /:entity/import on the given group.
// This MUST be registered before DynamicRecordHandler.RegisterRoutes on the SAME group so
// gin sees the static "/import" segment before the /:id param route. In practice gin resolves
// static before param segments regardless of registration order, but mounting first is safer.
func (h *ImportHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.POST("/:entity/import", h.Import)
}

// Import handles POST /:entity/import.
//
// Request: multipart/form-data
//   - file: CSV file (required)
//   - mapping: JSON string {"csvHeader":"fieldName"} (required)
//   - dedupField: field name to use for deduplication (optional)
//   - dryRun: "true" = preview — every check runs, nothing is written (optional, specs/061)
//   - matchBy: JSON {"relationField":"targetField"} — what a relation column's cells name (optional)
//
// Optional `Idempotency-Key` header (specs/047 FR-12): a resend of the same file + mapping +
// dedupField with the same key answers 200 + `Idempotent-Replay: true` carrying the FIRST summary
// verbatim instead of inserting the rows again.
//
// Response 200: { "data": { "total": N, "inserted": M, "failed": K, "valid": V, "dryRun": B, "errors": [...] } }
func (h *ImportHandler) Import(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	key, hasKey, kerr := idempotencyKeyOf(c)
	if kerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": kerr.Error(), "code": "INVALID_IDEMPOTENCY_KEY"})
		return
	}
	scope := scopeOf(c)
	entityName := c.Param("entity")

	// --- Parse multipart form (limit to IMPORT_MAX_BYTES + small overhead for form fields). ---
	maxBytes := services.ImportMaxBytes()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes+4096)
	if err := c.Request.ParseMultipartForm(maxBytes); err != nil {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": "file vượt kích thước tối đa",
			"code":  "REQUEST_TOO_LARGE",
		})
		return
	}

	// --- File ---
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "thiếu file CSV (form field: file)",
			"code":  "IMPORT_MISSING_FILE",
		})
		return
	}
	if fh.Size > maxBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{
			"error": "file vượt kích thước tối đa",
			"code":  "REQUEST_TOO_LARGE",
		})
		return
	}
	f, err := fh.Open()
	if err != nil {
		h.logger.Error("import: open file", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "không mở được file", "code": "INTERNAL_ERROR"})
		return
	}
	defer func() { _ = f.Close() }()

	// --- Mapping ---
	rawMapping := c.PostForm("mapping")
	if rawMapping == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "thiếu mapping (form field: mapping)",
			"code":  "IMPORT_MISSING_MAPPING",
		})
		return
	}
	var mapping map[string]string
	if err := json.Unmarshal([]byte(rawMapping), &mapping); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "mapping không phải JSON hợp lệ",
			"code":  "IMPORT_INVALID_MAPPING",
		})
		return
	}

	// --- DedupField (optional) ---
	dedupField := c.PostForm("dedupField")

	// --- MatchBy (optional, specs/061 FR-012): relation field → target field its cells name ---
	var matchBy map[string]string
	if raw := c.PostForm("matchBy"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &matchBy); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "matchBy không phải JSON hợp lệ",
				"code":  "IMPORT_INVALID_MAPPING",
			})
			return
		}
	}

	// --- Call import service ---
	in := services.ImportInput{
		TenantID:   tenantID,
		UserID:     scope.UserID,
		RoleID:     scope.RoleID,
		IsAdmin:    scope.IsAdmin,
		EntityName: entityName,
		Mapping:    mapping,
		DedupField: dedupField,
		Reader:     io.Reader(f),
		FileSize:   fh.Size,
		// specs/061 FR-010: the "Xem trước" step — same checks, nothing written.
		DryRun:  c.PostForm("dryRun") == "true",
		MatchBy: matchBy,
		// specs/048 S7b: opt-in; a plain import fires nothing.
		RunAutomations: c.PostForm("runAutomations") == "true",
	}
	var (
		result *services.ImportResult
		replay json.RawMessage
	)
	if hasKey {
		result, replay, err = h.svc.ImportIdempotent(c.Request.Context(), in, key)
	} else {
		result, err = h.svc.Import(c.Request.Context(), in)
	}
	if err != nil {
		h.respondImportError(c, err)
		return
	}
	if replay != nil {
		writeIdempotentReplay(c, replay)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// respondImportError maps service errors to the appropriate HTTP response.
func (h *ImportHandler) respondImportError(c *gin.Context, err error) {
	// Check sentinel errors first.
	if errors.Is(err, services.ErrEntityNotFound) {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "entity không tồn tại",
			"code":  "ENTITY_NOT_FOUND",
		})
		return
	}
	// specs/048 S2: the entity's before rules could not be read, so the import was refused rather
	// than run unjudged — nothing was written, safe to retry. Same answer as the record routes.
	if errors.Is(err, services.ErrRecordHooksUnavailable) {
		h.logger.Warn("import: before rules unavailable", zap.String("entity_name", c.Param("entity")), zap.Error(err))
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Hệ thống bận, thử lại sau ít giây",
			"code":  "RECORD_HOOKS_UNAVAILABLE",
		})
		return
	}
	// Coded ValidationError.
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		switch ve.Code {
		case "REQUEST_TOO_LARGE":
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": ve.Msg,
				"code":  ve.Code,
			})
		case "IMPORT_TOO_LARGE", "IMPORT_AUTOMATIONS_UNAVAILABLE":
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error": ve.Msg,
				"code":  ve.Code,
			})
		case "IMPORT_REQUIRED_FIELD_NOT_MAPPED", "RECORD_UNKNOWN_FIELD", "IMPORT_MISSING_FILE",
			"IMPORT_MISSING_MAPPING", "IMPORT_INVALID_MAPPING", "IMPORT_INVALID_CSV":
			c.JSON(http.StatusBadRequest, gin.H{
				"error": ve.Msg,
				"code":  ve.Code,
			})
		case "FIELD_FORBIDDEN":
			// queue #318: authorization refusal — 403, same as POST /:entity (api-conventions).
			c.JSON(http.StatusForbidden, gin.H{
				"error": ve.Msg,
				"code":  ve.Code,
			})
		case "IMPORT_MATCH_FIELD_INVALID":
			// specs/061 FR-012: a "Khớp theo" choice the file cannot use; details.field = the relation.
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   ve.Msg,
				"code":    ve.Code,
				"details": ve.Details,
			})
		case "IMPORT_BATCH_DUPLICATE", "IDEMPOTENCY_IN_PROGRESS":
			// IMPORT_BATCH_DUPLICATE: the batch hit a unique constraint — entire tx rolled back, the
			// caller should use dedupField. IDEMPOTENCY_IN_PROGRESS: the first import with this
			// Idempotency-Key is still running (specs/047 FR-12).
			c.JSON(http.StatusConflict, gin.H{
				"error": ve.Msg,
				"code":  ve.Code,
			})
		default:
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error": ve.Msg,
				"code":  ve.Code,
			})
		}
		return
	}
	// Unexpected error.
	h.logger.Error("import: unexpected error", zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{
		"error": "lỗi hệ thống khi import",
		"code":  "INTERNAL_ERROR",
	})
}
