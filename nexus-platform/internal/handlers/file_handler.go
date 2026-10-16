package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/files"
	"nexus/platform/internal/services"
)

// FileUploader is the consumer-side view of the file service the handler needs. Declared here so
// the handler depends on an interface (the real implementation is *files.Service) and tests can
// substitute a fake.
type FileUploader interface {
	Upload(ctx context.Context, scope services.RecordScope, in files.UploadInput) (*files.UploadResult, error)
	Open(ctx context.Context, scope services.RecordScope, tenantID, id string) (io.ReadCloser, *files.File, error)
	Delete(ctx context.Context, scope services.RecordScope, tenantID, id string) error
}

// FileHandler serves the File-upload M1 surface:
//
//	POST   /:entity/:id/files   (multipart) — upload an attachment to a record's file field
//	GET    /files/:id           — download a file (tenant + A5 scoped)
//	DELETE /files/:id           — soft-delete a file (tenant + A5 scoped)
//
// The upload route is entity-scoped and rides the same ACL stage as the dynamic record surface
// (a caller needs edit access to the entity). The /files/:id routes are mounted on the protected
// group BEFORE the dynamic dispatcher so the static "files" segment resolves ahead of the
// catch-all /:entity param route; their authorization is enforced inside the service against the
// parent record's tenant + A5 scope. The tenant ALWAYS comes from the verified JWT claim.
type FileHandler struct {
	svc      FileUploader
	maxBytes int64
	logger   *zap.Logger
}

// NewFileHandler wires the handler over the file service. maxBytes is the per-request body cap
// applied to the multipart upload (a value ≤ 0 disables the handler-level cap; the service still
// enforces its own limit). logger may be nil.
func NewFileHandler(svc FileUploader, maxBytes int64, logger *zap.Logger) *FileHandler {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &FileHandler{svc: svc, maxBytes: maxBytes, logger: logger}
}

// RegisterEntityRoutes mounts the entity-scoped upload route. It is registered on the dynamic
// record group (behind the ACL stage) so the same per-action RBAC governs who may attach a file.
// The static "files" child segment resolves ahead of the /:entity/:id param route in gin's tree.
func (h *FileHandler) RegisterEntityRoutes(rg *gin.RouterGroup) {
	rg.POST("/:entity/:id/files", h.Upload)
}

// RegisterFileRoutes mounts the file download/delete routes. These must be registered on the
// protected group BEFORE the dynamic dispatcher so the literal "files" segment is not shadowed
// by the /:entity catch-all.
func (h *FileHandler) RegisterFileRoutes(rg *gin.RouterGroup) {
	rg.GET("/files/:id", h.Download)
	rg.DELETE("/files/:id", h.Delete)
}

// uploadFormField is the multipart form field name the upload endpoint reads the file from.
const uploadFormField = "file"

// uploadFieldQueryParam names the record field (file/image) the attachment belongs to. It is
// read from the ?field= query (or a "field" form value as fallback) since a multipart upload
// targets a specific declared field on the record.
const uploadFieldQueryParam = "field"

// Upload handles POST /:entity/:id/files (multipart/form-data). The file part is "file"; the
// target record field is ?field=<name>. It enforces the size cap, then delegates to the service
// which authorizes the record + field and stores the blob.
func (h *FileHandler) Upload(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	fieldName := c.Query(uploadFieldQueryParam)
	if fieldName == "" {
		fieldName = c.PostForm(uploadFieldQueryParam)
	}
	if fieldName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "thiếu tham số field (tên field file/image)", "code": "FILE_FIELD_REQUIRED"})
		return
	}

	// Cap the request body so a huge upload cannot exhaust memory/disk before the size check.
	if h.maxBytes > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, h.maxBytes)
	}

	fileHeader, err := c.FormFile(uploadFormField)
	if err != nil {
		// http.MaxBytesReader surfaces an over-limit body as an error during multipart parse.
		if isBodyTooLarge(err) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file vượt quá giới hạn dung lượng", "code": "FILE_TOO_LARGE"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "thiếu file upload (multipart field 'file')", "code": "FILE_MISSING"})
		return
	}

	src, err := fileHeader.Open()
	if err != nil {
		h.logger.Error("handlers.File.Upload: open multipart", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "không đọc được file upload", "code": "FILE_READ_FAILED"})
		return
	}
	defer src.Close()

	mime := fileHeader.Header.Get("Content-Type")
	res, err := h.svc.Upload(c.Request.Context(), scopeOf(c), files.UploadInput{
		TenantID:   tenantID,
		EntityName: c.Param("entity"),
		RecordID:   c.Param("id"),
		FieldName:  fieldName,
		Filename:   fileHeader.Filename,
		Mime:       mime,
		Size:       fileHeader.Size,
		Content:    src,
	})
	if err != nil {
		h.respondFileError(c, err, "Upload")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": res})
}

// Download handles GET /files/:id. It streams the blob with its original filename and content
// type. Authorization (tenant + A5 against the parent record) is enforced inside the service.
func (h *FileHandler) Download(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	rc, f, err := h.svc.Open(c.Request.Context(), scopeOf(c), tenantID, c.Param("id"))
	if err != nil {
		h.respondFileError(c, err, "Download")
		return
	}
	defer rc.Close()

	if f.Mime != "" {
		c.Header("Content-Type", f.Mime)
	} else {
		c.Header("Content-Type", "application/octet-stream")
	}
	if f.Size > 0 {
		c.Header("Content-Length", strconv.FormatInt(f.Size, 10))
	}
	// Content-Disposition with the original display name. nosniff prevents the browser from
	// re-interpreting the bytes as a different (active) content type.
	c.Header("Content-Disposition", "attachment; filename=\""+f.Name+"\"")
	c.Header("X-Content-Type-Options", "nosniff")

	if _, err := io.Copy(c.Writer, rc); err != nil {
		// Headers/status already committed — can only log a mid-stream failure.
		h.logger.Error("handlers.File.Download: mid-stream", zap.String("tenant_id", tenantID), zap.Error(err))
	}
}

// Delete handles DELETE /files/:id (soft delete + blob removal + record-field re-sync).
func (h *FileHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), scopeOf(c), tenantID, c.Param("id")); err != nil {
		h.respondFileError(c, err, "Delete")
		return
	}
	c.Status(http.StatusNoContent)
}

// respondFileError maps file/service errors to HTTP responses. Authorization-shaped errors
// (entity/record/file not found) → 404; field-not-file → 422; policy rejections → 413/415;
// before rules unreadable → 503; ValidationError carries its own code/status; everything else → 500.
func (h *FileHandler) respondFileError(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
	case errors.Is(err, files.ErrFileNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "File không tồn tại", "code": "FILE_NOT_FOUND"})
	case errors.Is(err, files.ErrFileTooLarge):
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "file vượt quá giới hạn dung lượng", "code": "FILE_TOO_LARGE"})
	case errors.Is(err, files.ErrMimeNotAllowed):
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "loại file không được phép", "code": "FILE_MIME_NOT_ALLOWED"})
	case errors.Is(err, files.ErrEmptyFile):
		c.JSON(http.StatusBadRequest, gin.H{"error": "file rỗng", "code": "FILE_EMPTY"})
	case errors.Is(err, services.ErrRecordHooksUnavailable):
		// specs/048 FR-021, same envelope as the record routes: the record's before rules could not
		// be read, so the ref-array write was refused and the service undid the file change —
		// nothing was written, the retry is safe.
		h.logger.Warn("handlers.File."+op+": before rules unavailable", zap.Error(err))
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Hệ thống bận, thử lại sau ít giây", "code": "RECORD_HOOKS_UNAVAILABLE"})
	default:
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.File."+op, zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi xử lý file", "code": "FILE_OP_FAILED"})
	}
}

// isBodyTooLarge reports whether err is the "http: request body too large" error
// http.MaxBytesReader produces when the multipart body exceeds the cap.
func isBodyTooLarge(err error) bool {
	if err == nil {
		return false
	}
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return true
	}
	return err.Error() == "http: request body too large"
}
