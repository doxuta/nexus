package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/db"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// filterOpSeparator is the bracket notation used to encode a filter operator in a query
// parameter key. The syntax ?field[op]=value is parsed so field and op are extracted:
// "age[gte]" → field="age", op="gte"; "status" (no brackets) → field="status", op="" (eq).
const filterOpSeparator = "["

// DynamicRecordHandler is the single dispatcher serving CRUD for EVERY entity. It resolves
// :entity from sys_entities at request time rather than registering routes per entity.
type DynamicRecordHandler struct {
	svc    *services.RecordService
	logger *zap.Logger
}

// NewDynamicRecordHandler wires the dispatcher over the record service.
func NewDynamicRecordHandler(svc *services.RecordService, logger *zap.Logger) *DynamicRecordHandler {
	return &DynamicRecordHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts the dynamic record routes. These coexist with static routes like
// /metadata/* and /admin/* under the same group.
func (h *DynamicRecordHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/:entity", h.List)
	// EPIC 6 M1: the aggregate endpoint is a STATIC child of /:entity, registered alongside
	// the /:entity/:id param route. gin's tree resolves the static "aggregate" segment ahead
	// of the :id wildcard, so GET /:entity/aggregate never shadows GET /:entity/<id> and an
	// entity record whose id literally equals "aggregate" is unreachable by id (acceptable —
	// ids are server-generated UUIDs, never the literal "aggregate").
	rg.GET("/:entity/aggregate", h.Aggregate)
	// EPIC 6 M2: CSV export is another STATIC child of /:entity (like /aggregate), so gin's
	// tree resolves the literal "export.csv" segment ahead of the :id wildcard. A record whose
	// id literally equals "export.csv" is unreachable by id (acceptable — ids are UUIDs).
	rg.GET("/:entity/export.csv", h.ExportCSV)
	rg.POST("/:entity", h.Create)
	// ENG-BATCH #1: bulk (multi-row) create. A static POST child of /:entity — no route
	// conflict with GET/PUT/DELETE's /:entity/:id (different HTTP method, and gin resolves the
	// static "bulk" segment ahead of any wildcard on the same method regardless).
	rg.POST("/:entity/bulk", h.CreateBulk)
	// W9b recycle bin: GET /:entity/deleted lists the tenant's soft-deleted records of the entity.
	// It is a STATIC child of /:entity (like /aggregate and /export.csv), so gin's tree resolves
	// the literal "deleted" segment ahead of the :id wildcard; a record whose id literally equals
	// "deleted" is unreachable by id (acceptable — ids are server-generated UUIDs). It rides the
	// SAME RequireACL gate as List (GET→view), so the recycle bin has the identical entity-level
	// permission and A5 row-scope as the live list.
	rg.GET("/:entity/deleted", h.ListDeleted)
	rg.GET("/:entity/:id", h.Get)
	rg.PUT("/:entity/:id", h.Update)
	rg.DELETE("/:entity/:id", h.Delete)
	// TRỤ 2 mục 2 "Giao việc & đổi chủ": PUT /:entity/:id/owner reassigns a record's current
	// owner. It is a STATIC child of /:entity/:id (a sibling of PUT /:entity/:id, deeper by one
	// segment — same coexistence precedent as POST /:entity/:id/restore), so it rides the SAME
	// recordGroup ACL gate as every other route here — PUT's default method→action mapping is
	// already "edit" (middleware/acl.go methodAction), exactly the grant this needs; the
	// STRICTER "must be access_level=all or admin" check lives in RecordService.SetOwner, since
	// RequireACL alone would also let a mere owner-level editor through.
	rg.PUT("/:entity/:id/owner", h.SetOwner)
}

// RegisterRestoreRoutes mounts POST /:entity/:id/restore (W9b recycle-bin restore) on the given
// group. It is registered SEPARATELY from RegisterRoutes because it must NOT ride the default
// method→action ACL (POST→"create"): restore is the inverse of delete and requires the "delete"
// grant. The caller mounts this group behind middleware.RequireACLAction(checker, "delete") so the
// permission mirrors DELETE /:entity/:id exactly. It is a STATIC child of /:entity/:id (a sibling
// of POST /:entity/:id/files), so it never conflicts with the record routes.
func (h *DynamicRecordHandler) RegisterRestoreRoutes(rg *gin.RouterGroup) {
	rg.POST("/:entity/:id/restore", h.Restore)
}

// RegisterPurgeRoute mounts POST /:entity/:id/purge (specs/056 — remove a binned record for good).
// The caller mounts it on a group behind RequireAdmin → RequirePlatformOperator and ONLY when the
// operator tier is wired (router.go) — never on the ACL-gated record group, because an entity
// "delete" grant is exactly what a customer's staff hold and purge is a Nexus-operator act.
func (h *DynamicRecordHandler) RegisterPurgeRoute(rg *gin.RouterGroup) {
	rg.POST("/:entity/:id/purge", h.Purge)
}

// RegisterBulkUpdateRoute mounts POST /:entity/bulk-update (ENG-MASSOP mass update). Registered
// SEPARATELY from RegisterRoutes for the same reason as restore: the default POST→"create"
// mapping is wrong — a mass update must require the "edit" grant, exactly like PUT /:entity/:id.
// The caller mounts this group behind middleware.RequireACLAction(checker, "edit"). The static
// "bulk-update" segment resolves ahead of any :id wildcard (same precedent as /bulk).
func (h *DynamicRecordHandler) RegisterBulkUpdateRoute(rg *gin.RouterGroup) {
	rg.POST("/:entity/bulk-update", h.UpdateBulk)
}

// RegisterBulkDeleteRoute mounts POST /:entity/bulk-delete (ENG-MASSOP mass delete) — same
// fixed-action story: it must require the "delete" grant (like DELETE /:entity/:id), so the
// caller mounts it on the SAME RequireACLAction("delete") group that carries /restore.
func (h *DynamicRecordHandler) RegisterBulkDeleteRoute(rg *gin.RouterGroup) {
	rg.POST("/:entity/bulk-delete", h.DeleteBulk)
}

// tenantOf reads the tenant id from the verified JWT claim that AuthMiddleware placed in the
// request context (../specs/001-auth-security/spec.md §4 tenant-from-claim). It NEVER consults a
// client-controlled header or query param, so a caller cannot forge a tenant. A request that
// somehow reaches here without a tenant claim (a mounting bug — every record route sits behind
// AuthMiddleware + RequireTenant) is refused with a generic 401, identical to the auth gate.
func tenantOf(c *gin.Context) (string, bool) {
	tenantID, err := auth.GetContextTenantID(c)
	if err != nil || strings.TrimSpace(tenantID) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Yêu cầu xác thực không hợp lệ", "code": "UNAUTHORIZED"})
		return "", false
	}
	return tenantID, true
}

// scopeOf builds the A5 row-level access scope for the request from the VERIFIED principal. The
// user id comes from the verified JWT subject (created_by / owner-filter source) — never from a
// client value. The access_level is the one the RequireACL stage resolved and stashed in
// context (none/owner/team/all); the verified user id stashed alongside it is preferred so the
// scope is built from exactly what the ACL stage authorized.
//
// When NO ACL stage ran (a route mounted without an aclChecker — e.g. some integration harnesses),
// there is no resolved access_level in context. A request reaching a record handler without an ACL
// stage is a mounting error, so the scope fails CLOSED to AccessNone (see no rows) rather than
// assuming AccessAll — matching the documented contract of AccessLevelFromContext (acl.go) and the
// fail-closed policy in dynrepo.normalizeAccessLevel. Tenant isolation still holds via tenantOf, and
// the admin flag is honored either way: admins short-circuit to AccessAll in effectiveLevel(), so a
// verified admin still sees every row in the tenant even on a no-ACL surface. Production always
// mounts the ACL stage, so a real non-admin request always carries a resolved level.
func scopeOf(c *gin.Context) services.RecordScope {
	sub, _ := middleware.SubjectFromContext(c)

	userID := sub.UserID
	if uid, ok := middleware.ACLUserIDFromContext(c); ok {
		userID = uid
	}

	level, ok := middleware.AccessLevelFromContext(c)
	if !ok {
		// No ACL stage resolved an access_level → fail CLOSED (deny all rows), per the
		// AccessLevelFromContext contract; never assume "all". Admins are unaffected because
		// effectiveLevel() short-circuits IsAdmin to AccessAll regardless of this level.
		level = string(dynrepo.AccessNone)
	}

	// specs/051-record-manual-share: a manual share is read-only, so it widens the scope only when
	// the ACL stage authorized THIS request as "view". Any other action — or no ACL stage — leaves it
	// off (and dynrepo never emits the share clause on a write path anyway).
	action, _ := middleware.ACLActionFromContext(c)

	return services.RecordScope{UserID: userID, AccessLevel: level, IsAdmin: sub.IsAdmin, RoleID: sub.RoleID, IncludeShares: action == "view"}
}

// Create handles POST /:entity.
//
// Optional `Idempotency-Key` header (specs/047-idempotency-key-on-create): a resend with the same
// key and body answers 200 + `Idempotent-Replay: true` carrying the FIRST record's id instead of
// creating a second one; see RecordService.CreateIdempotent for the claim rules. Without the header
// the create is unchanged.
func (h *DynamicRecordHandler) Create(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	key, hasKey, kerr := idempotencyKeyOf(c)
	if kerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": kerr.Error(), "code": "INVALID_IDEMPOTENCY_KEY"})
		return
	}
	var payload map[string]any
	if !bindJSON(c, &payload) {
		return
	}
	scope := scopeOf(c)
	if !hasKey {
		id, err := h.svc.Create(c.Request.Context(), scope, tenantID, scope.UserID, c.Param("entity"), payload)
		if err != nil {
			h.respondError(c, err, "Create")
			return
		}
		c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": id}})
		return
	}
	id, replayed, err := h.svc.CreateIdempotent(c.Request.Context(), scope, tenantID, scope.UserID, c.Param("entity"), payload, key)
	if err != nil {
		h.respondError(c, err, "Create")
		return
	}
	if replayed {
		c.Header("Idempotent-Replay", "true")
		c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": id}})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": gin.H{"id": id}})
}

// idempotencyKeyMaxLen caps the Idempotency-Key header (Stripe's limit; the column is VARCHAR(255)).
const idempotencyKeyMaxLen = 255

// idempotencyKeyOf reads the optional Idempotency-Key header: ("", false, nil) when absent; an
// error when it is sent more than once, or is not 1..255 visible ASCII characters (0x21-0x7E — no
// spaces, no control or non-ASCII bytes, so the stored ascii_bin key is exactly what was sent).
func idempotencyKeyOf(c *gin.Context) (string, bool, error) {
	return idempotencyKeyOfMax(c, idempotencyKeyMaxLen)
}

// idempotencyKeyOfMax is idempotencyKeyOf for a column narrower than 255 (approval send: 128).
func idempotencyKeyOfMax(c *gin.Context, maxLen int) (string, bool, error) {
	vals := c.Request.Header.Values("Idempotency-Key")
	if len(vals) == 0 {
		return "", false, nil
	}
	if len(vals) > 1 {
		return "", false, errors.New("chỉ gửi MỘT header Idempotency-Key")
	}
	key := strings.TrimSpace(vals[0])
	if key == "" || len(key) > maxLen {
		return "", false, fmt.Errorf("Idempotency-Key phải dài 1-%d ký tự", maxLen)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return "", false, errors.New("Idempotency-Key chỉ gồm ký tự ASCII nhìn thấy được (không dấu cách, không dấu tiếng Việt)")
		}
	}
	return key, true, nil
}

// CreateBulk handles POST /:entity/bulk (ENG-BATCH #1) — multi-row create in one request. Body:
// {"records":[{...},{...}]}. Query ?mode=atomic (default) | ?mode=partial. See
// ../specs/009-bulk-create/spec.md §2 for the full response envelope.
//
// Status: 201 when mode=atomic and every row was created; 422 when mode=atomic and ANY row
// failed (nothing was written — see BulkCreateResult.Results for the per-row cause); 200 for
// mode=partial regardless of per-row outcome (a mixed created/failed result IS the successful
// multi-status response for that mode).
//
// Optional `Idempotency-Key` header (specs/047 FR-12): a resend of the same batch with the same key
// answers 200 + `Idempotent-Replay: true` carrying the FIRST response verbatim instead of writing the
// rows again; see RecordService.CreateBulkIdempotent.
func (h *DynamicRecordHandler) CreateBulk(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	key, hasKey, kerr := idempotencyKeyOf(c)
	if kerr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": kerr.Error(), "code": "INVALID_IDEMPOTENCY_KEY"})
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
	scope := scopeOf(c)
	var (
		result services.BulkCreateResult
		replay json.RawMessage
		err    error
	)
	if hasKey {
		result, replay, err = h.svc.CreateBulkIdempotent(c.Request.Context(), scope, tenantID, scope.UserID, c.Param("entity"), body.Records, mode, key)
	} else {
		result, err = h.svc.CreateBulk(c.Request.Context(), scope, tenantID, scope.UserID, c.Param("entity"), body.Records, mode)
	}
	if err != nil {
		h.respondError(c, err, "CreateBulk")
		return
	}
	if replay != nil {
		writeIdempotentReplay(c, replay)
		return
	}
	c.JSON(statusForBulkResult(mode, result), gin.H{"data": result})
}

// writeIdempotentReplay answers a resend whose Idempotency-Key already completed with the stored
// response of the first call: 200 + `Idempotent-Replay: true`, body `{"data": <first response>}`.
func writeIdempotentReplay(c *gin.Context, stored json.RawMessage) {
	c.Header("Idempotent-Replay", "true")
	c.JSON(http.StatusOK, gin.H{"data": stored})
}

// statusForBulkResult resolves the HTTP status for a bulk-create response (../specs/009-bulk-create/spec.md
// §2): mode=partial is always 200 (a mixed created/failed result is itself the successful
// response); mode=atomic is 201 when every row was created and 422 when the batch was aborted
// (created is always 0 in that case — nothing was written).
func statusForBulkResult(mode services.BulkMode, result services.BulkCreateResult) int {
	if mode == services.BulkModePartial {
		return http.StatusOK
	}
	if result.Failed == 0 {
		return http.StatusCreated
	}
	return http.StatusUnprocessableEntity
}

// UpdateBulk handles POST /:entity/bulk-update (ENG-MASSOP): apply ONE shared values patch to
// 1..MAX_BULK_OPS (default 500) ids via the single-record Update path per row (partial
// semantics — a failing row never aborts its siblings; a values-level defect fails each row
// with the same code a single PUT returns). Body: {"ids":[...], "values":{field:value,...}}.
// Always 200 when the batch executed (per-row errors inside data.results); 422 on a
// request-level defect (BULK_EMPTY / BULK_TOO_MANY / BULK_VALUES_EMPTY); 400 INVALID_BODY on
// malformed JSON like every endpoint.
func (h *DynamicRecordHandler) UpdateBulk(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var body struct {
		IDs    []string       `json:"ids"`
		Values map[string]any `json:"values"`
	}
	if !bindJSON(c, &body) {
		return
	}
	result, err := h.svc.UpdateBulk(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), body.IDs, body.Values)
	if err != nil {
		h.respondError(c, err, "UpdateBulk")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// DeleteBulk handles POST /:entity/bulk-delete (ENG-MASSOP): soft-delete 1..MAX_BULK_OPS
// (default 500) ids via the single-record Delete path per row (partial semantics). Body:
// {"ids":[...]}. Always 200 when the batch executed; 422 on BULK_EMPTY / BULK_TOO_MANY.
func (h *DynamicRecordHandler) DeleteBulk(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if !bindJSON(c, &body) {
		return
	}
	result, err := h.svc.DeleteBulk(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), body.IDs)
	if err != nil {
		h.respondError(c, err, "DeleteBulk")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}

// Get handles GET /:entity/:id. The optional ?expand=f1,f2 query resolves relation fields
// to {id,label} objects (depth=1 only). Unknown expand fields are silently ignored (RM-07).
// The optional ?rollup=f1,f2 query resolves `rollup` fields (ENG-COMPUTED #3, SUM/COUNT/AVG/
// MIN/MAX over child records) — gated behind this explicit param so a plain GET never pays for
// an aggregate query it did not ask for. The response carries the record's optimistic-lock
// version both in the body (data.version) and as a strong ETag header (RM-24), so a client can
// echo it back via If-Match on PUT.
func (h *DynamicRecordHandler) Get(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	expand := parseExpand(c.Query("expand"))
	rollup := parseExpand(c.Query("rollup"))
	rec, err := h.svc.Get(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id"), expand, rollup)
	if err != nil {
		h.respondError(c, err, "Get")
		return
	}
	// RM-24: expose the version as a strong ETag (RFC 7232) so a conditional PUT can send it
	// back via If-Match. The body already carries data.version (Record.Version json tag).
	c.Header("ETag", strconv.Quote(strconv.Itoa(rec.Version)))
	c.JSON(http.StatusOK, gin.H{"data": rec})
}

// reservedListParams are query keys consumed by pagination/sort/search/expand/rollup and
// therefore NOT treated as ?field[op]=value record filters. "after" is the D2 cursor param and
// "paginate" its mode selector (CURSOR-BOOTSTRAP #42) — left out, ?paginate=cursor would fall into
// the filter parser as paginate[eq]=cursor and fail as LIST_UNKNOWN_FILTER.
// "rollup" is the ENG-COMPUTED #3 explicit rollup-resolution request (mirrors "expand").
var reservedListParams = map[string]struct{}{
	"page": {}, "limit": {}, "sort": {}, "order": {}, "q": {}, "expand": {}, "after": {}, "paginate": {}, "rollup": {},
}

// reservedAggregateParams are the query keys the aggregate endpoint consumes itself (metric,
// field, groupBy, bucket) and therefore must NOT be parsed as ?field[op]=value record filters.
var reservedAggregateParams = map[string]struct{}{
	"metric": {}, "field": {}, "groupBy": {}, "groupby": {}, "bucket": {},
}

// parseQueryFilters turns the raw query params into FilterInput values, skipping the supplied
// reserved keys. It implements the shared ?field[op]=value syntax (bare ?field=value == eq;
// comma-split for in/between; no value for isnull/notnull) used by both List and Aggregate.
func parseQueryFilters(c *gin.Context, reserved map[string]struct{}) []services.FilterInput {
	var filters []services.FilterInput
	for key, vals := range c.Request.URL.Query() {
		if _, skip := reserved[key]; skip {
			continue
		}
		if len(vals) == 0 {
			continue
		}
		field, opStr := parseFilterKey(key)
		if field == "" {
			continue
		}
		op := dynrepo.FilterOperator(opStr) // empty string == eq (dynrepo default)
		rawVal := vals[len(vals)-1]
		var values []string
		switch op {
		case dynrepo.FilterIn, dynrepo.FilterBetween:
			for _, part := range strings.Split(rawVal, ",") {
				if t := strings.TrimSpace(part); t != "" {
					values = append(values, t)
				}
			}
		case dynrepo.FilterIsNull, dynrepo.FilterIsNotNull:
			values = nil
		default:
			values = []string{rawVal}
		}
		filters = append(filters, services.FilterInput{
			Field:    field,
			Operator: op,
			Values:   values,
		})
	}
	return filters
}

// parseFilterKey parses a query key of the form "field" or "field[op]" into its field
// name and operator components. "status" → ("status", ""); "age[gte]" → ("age", "gte").
// Malformed brackets (missing ']', empty field) produce ("", "") so the caller can skip.
func parseFilterKey(key string) (field, op string) {
	idx := strings.Index(key, filterOpSeparator)
	if idx < 0 {
		// Plain key — no operator bracket.
		return key, ""
	}
	field = key[:idx]
	if field == "" {
		return "", ""
	}
	rest := key[idx+1:]
	if !strings.HasSuffix(rest, "]") {
		return "", ""
	}
	op = rest[:len(rest)-1]
	return field, op
}

// List handles GET /:entity with pagination, ?field[op]=value filters and ?q substring
// search. The operator bracket syntax ?field[op]=value is backward-compatible: a bare
// ?field=value is treated as ?field[eq]=value. Multiple filters on the same field with
// different operators (e.g. ?age[gte]=18&age[lte]=65) are all collected. The special
// "in" and "between" operators accept comma-separated values: ?status[in]=open,pending.
//
// HIDDEN-COL-SIGNAL: when the caller's field-ACL hides ≥1 declared field of the entity
// (can_view=0 — the same set stripped from every row), the envelope gains an ADDITIVE
// `meta: {hiddenColumns: <int>}` key so the FE can render an honest "N cột ẩn bởi quyền"
// chip (a hidden column is otherwise indistinguishable from an empty one). The key is
// OMITTED entirely when the count is 0 — notably for admins, who bypass field-ACL and see
// every column, so a chip driven by `meta` presence can never be a 100%-false-positive for
// them. data/pagination shapes are unchanged (backward-compatible).
func (h *DynamicRecordHandler) List(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	// Parse ?field[op]=value filters (shared parser; skips pagination/sort/search/expand keys).
	filters := parseQueryFilters(c, reservedListParams)
	// Keyset mode is ?paginate=cursor (first page, no after) or a non-empty ?after=; an empty
	// after alone stays offset. The service resolves the mode and refuses ?page= in keyset mode,
	// which is why whether ?page= was SENT at all is passed along (Page itself defaults to 1).
	_, pageExplicit := c.GetQuery("page")

	query := services.ListQuery{
		Page:         atoiDefault(c.Query("page"), 1),
		Limit:        atoiDefault(c.Query("limit"), 20),
		Sort:         c.Query("sort"),
		Order:        c.Query("order"),
		After:        c.Query("after"),
		Paginate:     c.Query("paginate"),
		PageExplicit: pageExplicit,
		Expand:       parseExpand(c.Query("expand")),
		Rollup:       parseExpand(c.Query("rollup")),
		Filters:      filters,
		Search:       c.Query("q"),
	}
	result, err := h.svc.List(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), query)
	if err != nil {
		h.respondError(c, err, "List")
		return
	}

	// Cursor mode: return {limit, nextCursor} — no total/totalPages (O(1), no COUNT).
	// Offset mode: return {page, limit, total, totalPages} (backward-compatible envelope).
	if result.CursorMode {
		_, limit := dynrepo.ClampPageLimit(1, query.Limit)
		var nextCursor *string
		if result.NextCursor != "" {
			nextCursor = &result.NextCursor
		}
		resp := gin.H{
			"data": result.Records,
			"pagination": gin.H{
				"limit":      limit,
				"nextCursor": nextCursor,
			},
		}
		if result.HiddenColumns > 0 {
			resp["meta"] = gin.H{"hiddenColumns": result.HiddenColumns}
		}
		c.JSON(http.StatusOK, resp)
		return
	}

	page, limit := dynrepo.ClampPageLimit(query.Page, query.Limit)
	resp := gin.H{
		"data": result.Records,
		"pagination": gin.H{
			"page":       page,
			"limit":      limit,
			"total":      result.Total,
			"totalPages": (result.Total + limit - 1) / limit,
		},
	}
	if result.HiddenColumns > 0 {
		resp["meta"] = gin.H{"hiddenColumns": result.HiddenColumns}
	}
	c.JSON(http.StatusOK, resp)
}

// Update handles PUT /:entity/:id.
//
// RM-24 optimistic locking (OPT-IN): the caller may pin the version it is editing via the
// `If-Match: "<n>"` header (RFC 7232, preferred) OR a `version` key in the JSON body (fallback).
// When present, a version mismatch is rejected 409 RECORD_VERSION_CONFLICT (with currentVersion).
// When neither is sent, the update is unconditional last-writer-wins (backward-compatible). The
// `version` body key — when used — is stripped before validation so it is not mistaken for an
// unknown record field.
func (h *DynamicRecordHandler) Update(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var payload map[string]any
	if !bindJSON(c, &payload) {
		return
	}
	expectedVersion, perr := parseExpectedVersion(c, payload)
	if perr != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": perr.Error(), "code": "INVALID_IF_MATCH"})
		return
	}
	// Strip the version sentinel from the body so ValidatePartialRecord does not see it as an
	// unknown field. (If-Match-only callers leave the body untouched.)
	delete(payload, "version")
	err := h.svc.Update(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id"), payload, expectedVersion)
	if err != nil {
		h.respondError(c, err, "Update")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": c.Param("id")}})
}

// parseExpectedVersion resolves the RM-24 optimistic-lock expected version from the request,
// preferring the `If-Match` header over a `version` key in the JSON body. It returns:
//   - (nil, nil)   when neither is supplied → opt-in skip (unconditional update).
//   - (&n,  nil)   when a valid non-negative integer version was found.
//   - (nil, err)   when a value was supplied but is malformed (→ 400 INVALID_IF_MATCH).
//
// If-Match syntax: a quoted token per RFC 7232, e.g. `If-Match: "3"`. The surrounding quotes
// (and an optional weak `W/` prefix, which we treat as equivalent here) are stripped before
// parsing. The wildcard `If-Match: *` means "any current version" → opt-in skip (nil, nil).
// Body fallback: a JSON number `version` (float64 after decode) that must be a whole number.
func parseExpectedVersion(c *gin.Context, payload map[string]any) (*int, error) {
	if raw := strings.TrimSpace(c.GetHeader("If-Match")); raw != "" {
		if raw == "*" {
			return nil, nil // wildcard: no version pin
		}
		// Strip an optional weak-validator prefix and the surrounding double quotes.
		raw = strings.TrimPrefix(raw, "W/")
		unquoted := strings.Trim(raw, `"`)
		n, err := strconv.Atoi(strings.TrimSpace(unquoted))
		if err != nil || n < 0 {
			return nil, fmt.Errorf("If-Match phải là phiên bản số nguyên không âm trong dấu nháy, ví dụ \"3\"")
		}
		return &n, nil
	}
	// Body fallback: a numeric `version` key (JSON decodes numbers to float64).
	if v, present := payload["version"]; present {
		switch t := v.(type) {
		case float64:
			if t < 0 || t != float64(int(t)) {
				return nil, fmt.Errorf("body.version phải là số nguyên không âm")
			}
			n := int(t)
			return &n, nil
		case nil:
			return nil, nil // explicit null → no pin
		default:
			return nil, fmt.Errorf("body.version phải là số nguyên")
		}
	}
	return nil, nil
}

// setOwnerRequest is the PUT /:entity/:id/owner body.
type setOwnerRequest struct {
	UserID string `json:"userId"`
}

// SetOwner handles PUT /:entity/:id/owner (TRỤ 2 mục 2 "Giao việc & đổi chủ"): reassigns the
// record's current owner. Authorization beyond the route's "edit" ACL gate (manager-only, or
// admin) and userId validity are enforced in RecordService.SetOwner; see its doc comment for the
// full contract (403 FORBIDDEN / 422 RECORD_OWNER_INVALID / 404 RECORD_NOT_FOUND).
func (h *DynamicRecordHandler) SetOwner(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req setOwnerRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.svc.SetOwner(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id"), req.UserID); err != nil {
		h.respondError(c, err, "SetOwner")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": c.Param("id"), "ownerId": req.UserID}})
}

// Delete handles DELETE /:entity/:id (soft delete).
func (h *DynamicRecordHandler) Delete(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id")); err != nil {
		h.respondError(c, err, "Delete")
		return
	}
	c.Status(http.StatusNoContent)
}

// ListDeleted handles GET /:entity/deleted — the W9b recycle bin. It lists the tenant's
// soft-deleted records of the entity (deleted_at IS NOT NULL), OFFSET-paginated, reusing the SAME
// service.List pipeline as the live list with OnlyDeleted=true: identical tenant filter, A5
// row-scope, field-ACL strip, ?field[op]=value filters and ?q search. A user therefore only sees
// deleted rows they could have seen live, with hidden fields stripped exactly as on the live list.
// It is OFFSET-only (?after / ?paginate=cursor → 422 PAGINATION_MODE_CONFLICT; no ?expand/?rollup)
// — the recycle bin is a shallow admin view, not a deep-paginated data surface.
func (h *DynamicRecordHandler) ListDeleted(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	// Reuse the shared ?field[op]=value parser (skips pagination/sort/search/cursor/expand keys).
	filters := parseQueryFilters(c, reservedListParams)
	query := services.ListQuery{
		Page:  atoiDefault(c.Query("page"), 1),
		Limit: atoiDefault(c.Query("limit"), 20),
		Sort:  c.Query("sort"),
		Order: c.Query("order"),
		// Passed through only so the service can REFUSE them (offset-only bin → 422
		// PAGINATION_MODE_CONFLICT) — both keys are reserved, so dropping them would be silent.
		After:       c.Query("after"),
		Paginate:    c.Query("paginate"),
		Filters:     filters,
		Search:      c.Query("q"),
		OnlyDeleted: true, // recycle bin: deleted_at IS NOT NULL
	}
	result, err := h.svc.List(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), query)
	if err != nil {
		h.respondError(c, err, "ListDeleted")
		return
	}
	page, limit := dynrepo.ClampPageLimit(query.Page, query.Limit)
	c.JSON(http.StatusOK, gin.H{
		"data": result.Records,
		"pagination": gin.H{
			"page":       page,
			"limit":      limit,
			"total":      result.Total,
			"totalPages": (result.Total + limit - 1) / limit,
		},
	})
}

// Restore handles POST /:entity/:id/restore — the W9b recycle-bin restore. It clears the record's
// deleted_at (tenant-pinned + A5 row-scoped), authorized with the "delete" grant via the
// RequireACLAction("delete") middleware on its route group (see RegisterRestoreRoutes). A record
// that is not soft-deleted / does not exist / is cross-tenant / out-of-scope surfaces as 404
// RECORD_NOT_FOUND; a unique value that now collides with a live row created after the delete
// surfaces as 409 RECORD_DUPLICATE (details.field) — both mapped by the shared respondError.
func (h *DynamicRecordHandler) Restore(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	if err := h.svc.Restore(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id")); err != nil {
		h.respondError(c, err, "Restore")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"id": c.Param("id")}})
}

// purgeRecordRequest is the optional POST body of /purge. DryRun is a POINTER so an absent key (or
// no body at all) means "dry run": the safe reading of silence on an irreversible endpoint is
// "tell me what would go, delete nothing" — {"dryRun": false} is a deliberate act (the same
// contract as POST /admin/tenants/:id/purge).
type purgeRecordRequest struct {
	DryRun *bool `json:"dryRun"`
}

// Purge handles POST /:entity/:id/purge — permanently remove ONE record that is already in the
// recycle bin (specs/056). 200 {data: report} in both modes: a dry run is a real answer the
// operator reads before deciding. 409 RECORD_NOT_IN_RECYCLE_BIN for a live record, 404 for an
// unknown / other-tenant id — mapped by the shared respondError.
func (h *DynamicRecordHandler) Purge(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req purgeRecordRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Body không hợp lệ", "code": "INVALID_BODY"})
		return
	}
	dryRun := req.DryRun == nil || *req.DryRun
	report, err := h.svc.Purge(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), c.Param("id"), dryRun)
	if err != nil {
		h.respondError(c, err, "Purge")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": report})
}

// Aggregate handles GET /:entity/aggregate — the EPIC 6 M1 aggregation endpoint (the P0
// reporting gap: SUM/COUNT/AVG/MIN/MAX, optionally grouped). Query params:
//
//	?metric=sum|count|avg|min|max   (required)
//	?field=<fieldName>              (required for sum/avg/min/max; ignored for count)
//	?groupBy=<fieldName>            (optional; must be an indexed field/system column)
//	?bucket=day|week|month|quarter|year (optional, W4; requires groupBy on a date/datetime
//	                                 field or created_at/updated_at — groups become calendar
//	                                 buckets in CHRONOLOGICAL order; empty buckets omitted)
//	?<field>[op]=value              (optional filters; same syntax as List)
//
// Response:
//   - scalar (no groupBy): {"data":{"value":<string|null>,"count":<int>}}
//   - grouped:             {"data":{"groups":[{"key":<string|null>,"value":<string|null>,"count":<int>}],"groupBy":"<field>"}}
//     (bucketed adds "bucket":"<token>" and keys like "2026-01" / "2026-Q1" / "2026-W05")
//
// value is MySQL's exact DECIMAL text (e.g. "1234.5600") for sum/avg/min/max so a currency
// total never drifts through float64; for count it is the integer count as a string. null
// surfaces when the metric is undefined over the matched set (e.g. AVG of zero rows) or for a
// NULL group key. Tenant + A5 row-scope isolation is identical to List: an aggregate only
// covers rows the caller may read.
func (h *DynamicRecordHandler) Aggregate(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	// metric/field/groupBy/bucket are consumed here; every other ?key is a record filter.
	filters := parseQueryFilters(c, reservedAggregateParams)
	// groupBy accepts either spelling; the camelCase form wins when both are present.
	groupBy := c.Query("groupBy")
	if groupBy == "" {
		groupBy = c.Query("groupby")
	}
	bucket := strings.ToLower(strings.TrimSpace(c.Query("bucket")))
	query := services.AggregateQuery{
		Metric:  services.AggregateMetric(strings.ToLower(strings.TrimSpace(c.Query("metric")))),
		Field:   c.Query("field"),
		GroupBy: groupBy,
		Bucket:  bucket,
		Filters: filters,
	}
	result, err := h.svc.Aggregate(c.Request.Context(), scopeOf(c), tenantID, c.Param("entity"), query)
	if err != nil {
		h.respondError(c, err, "Aggregate")
		return
	}

	if result.Grouped {
		groups := make([]gin.H, 0, len(result.Groups))
		for _, g := range result.Groups {
			groups = append(groups, gin.H{"key": g.Key, "value": g.Value, "count": g.Count})
		}
		data := gin.H{"groups": groups, "groupBy": groupBy, "currencyCode": result.CurrencyCode}
		if bucket != "" {
			data["bucket"] = bucket
		}
		c.JSON(http.StatusOK, gin.H{"data": data})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{
		"value":        result.Scalar.Value,
		"count":        result.Scalar.Count,
		"currencyCode": result.CurrencyCode,
	}})
}

// ExportCSV handles GET /:entity/export.csv — the EPIC 6 M2 streaming CSV export. It reuses the
// SAME ?field[op]=value filter syntax as List (every query param is a filter), and streams every
// matching row (tenant + A5 row-scoped) as RFC 4180 CSV with a header row of the entity's declared
// field names (column names from metadata). The body is streamed page-by-page via keyset cursor
// pagination so a large export never buffers the whole set in memory.
//
// Headers: Content-Type text/csv; charset=utf-8 and a Content-Disposition attachment filename so a
// browser downloads it as <entity>.csv. Errors that occur BEFORE the first byte (entity unknown,
// invalid filter) surface as the normal JSON error envelope; an error mid-stream is logged (the
// status line is already sent) and the connection is closed.
func (h *DynamicRecordHandler) ExportCSV(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	entity := c.Param("entity")
	// Every query param is a record filter (same parser/syntax as List; no reserved keys here).
	filters := parseQueryFilters(c, nil)

	// Resolve up front (entity existence + filter validity) so a bad request is a clean JSON
	// error BEFORE we commit to a 200 streaming response. We do a 1-row probe via the service's
	// own validation path by streaming straight away — but to keep error→JSON clean, the service
	// returns the first error (resolve/filter) before writing any row, and ExportCSV writes the
	// header only after resolve succeeds. We therefore buffer nothing here and let the service's
	// early returns drive the pre-stream error; once headers are set we are committed.
	//
	// To distinguish a pre-stream error (→ JSON) from a mid-stream error (→ logged), we stream to
	// a small buffered checkpoint: set the headers only on the first successful service call. The
	// service writes the header row first, so if resolve/filter fails it returns before any write.
	c.Header("Content-Type", "text/csv; charset=utf-8")
	c.Header("Content-Disposition", "attachment; filename=\""+exportFilename(entity)+"\"")
	c.Header("X-Content-Type-Options", "nosniff")

	if err := h.svc.ExportCSV(c.Request.Context(), scopeOf(c), tenantID, entity, services.ExportCSVOptions{Filters: filters}, c.Writer); err != nil {
		// If nothing was written yet, the header map is still mutable and gin can emit a JSON
		// error with the right status. If bytes were already flushed, the status is committed —
		// we can only log. We detect "already wrote" via the ResponseWriter's Written() flag.
		if !c.Writer.Written() {
			// Reset the CSV content-type to JSON for the error envelope.
			c.Writer.Header().Del("Content-Disposition")
			c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			h.respondError(c, err, "ExportCSV")
			return
		}
		h.logger.Error("handlers.DynamicRecord.ExportCSV: mid-stream", append(logFields(c), zap.Error(err))...)
	}
}

// exportFilename builds a safe download filename for an entity export. The entity name is a
// validated identifier (letters/digits/underscore), so it is safe to embed directly.
func exportFilename(entity string) string {
	e := strings.TrimSpace(entity)
	if e == "" {
		e = "export"
	}
	return e + ".csv"
}

// logFields builds the RM-36 (OBS-1) log-context fields for this handler's error logs so a
// 500/timeout on the highest-traffic surface is diagnosable per tenant/entity/record and
// correlatable with the access log via request_id. Values come from trusted sources only:
// tenant from the verified JWT claim, entity/id from the route params (templates, not bodies),
// request id from the RequestID middleware.
func logFields(c *gin.Context) []zap.Field {
	fields := make([]zap.Field, 0, 4)
	if tenantID, err := auth.GetContextTenantID(c); err == nil && tenantID != "" {
		fields = append(fields, zap.String("tenant_id", tenantID))
	}
	if entity := c.Param("entity"); entity != "" {
		fields = append(fields, zap.String("entity", entity))
	}
	if recordID := c.Param("id"); recordID != "" {
		fields = append(fields, zap.String("record_id", recordID))
	}
	if reqID, ok := middleware.RequestIDFromContext(c); ok {
		fields = append(fields, zap.String("request_id", reqID))
	}
	return fields
}

// respondError maps service errors to HTTP: not-found -> 404, validation -> 422, else 500.
// ValidationError.Details is forwarded in the response envelope so field-level errors
// (e.g. RECORD_REQUIRED_MISSING) carry {"field":"<name>"} to the client (RM-06).
// INVALID_CURSOR -> 400 (client sent a bad token, not a server error and not 422).
// PAGE_TOO_DEEP  -> 422 (handled by the default statusForCode fallback).
// CURSOR_REQUIRES_INDEXED_SORT -> 422 (handled by the default statusForCode fallback).
// DB statement/connection timeout -> 503 DB_TIMEOUT (W5-E2): a slow/hung MySQL surfaces as a
// driver i/o-timeout (DSN readTimeout/writeTimeout) or a context deadline; we fail fast with a
// clean 503 instead of a generic 500 or a hang, so the caller can retry and an LB can shed load.
func (h *DynamicRecordHandler) respondError(c *gin.Context, err error, op string) {
	switch {
	case errors.Is(err, services.ErrEntityNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Entity không tồn tại", "code": "ENTITY_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "Bản ghi không tồn tại", "code": "RECORD_NOT_FOUND"})
	case errors.Is(err, services.ErrRecordHooksUnavailable):
		// specs/048 FR-021: the entity's before rules (or the record they judge) could not be read,
		// so the write was refused rather than let through unjudged. Checked before DB_TIMEOUT so a
		// timed-out rule load still answers with this code. Nothing was written — safe to retry.
		h.logger.Warn("handlers.DynamicRecord."+op+": before rules unavailable", append(logFields(c), zap.Error(err))...)
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Hệ thống bận, thử lại sau ít giây", "code": "RECORD_HOOKS_UNAVAILABLE"})
	case db.IsTimeout(err):
		// Checked before the ValidationError/500 branches: a wire/dial timeout is an
		// availability failure, not a client validation error. 503 + Retry-After lets the
		// caller back off; the request returns immediately rather than hanging the goroutine.
		h.logger.Warn("handlers.DynamicRecord."+op+": DB timeout", append(logFields(c), zap.Error(err))...)
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Cơ sở dữ liệu phản hồi chậm, vui lòng thử lại", "code": "DB_TIMEOUT"})
	case errors.Is(err, services.ErrWriteConflict):
		// RM-28: a transient MySQL deadlock/lock-wait (1213/1205) on the obj_ write or a pivot
		// statement. MySQL already rolled the tx back, so the SAME request is safe to retry —
		// 503 + Retry-After, mirroring DB_TIMEOUT, instead of an opaque 500.
		h.logger.Warn("handlers.DynamicRecord."+op+": write conflict", append(logFields(c), zap.Error(err))...)
		c.Header("Retry-After", "1")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Ghi dữ liệu bị xung đột tạm thời, vui lòng thử lại", "code": "RECORD_WRITE_CONFLICT"})
	default:
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			status := statusForCode(ve.Code)
			// INVALID_CURSOR is a malformed client token — HTTP 400, not 422.
			if ve.Code == "INVALID_CURSOR" {
				status = http.StatusBadRequest
			}
			c.JSON(status, gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers.DynamicRecord."+op, append(logFields(c), zap.Error(err))...)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Lỗi xử lý bản ghi", "code": "RECORD_OP_FAILED"})
	}
}

// parseExpand splits a comma-separated ?expand=f1,f2 query value into a trimmed slice.
// An empty or blank string returns nil (no expand). Blank tokens are dropped.
func parseExpand(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// atoiDefault parses a query int, falling back to def on empty/invalid input.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
