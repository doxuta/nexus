package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dashboard"
)

// dashboardRepo is the consumer-side slice of the dashboard store the service needs. Declared here
// so the service is unit-testable with an in-memory fake and does not import a concrete DB type.
// EVERY method is tenant-scoped in the real implementation (sys_dashboards.tenant_id), so no use
// case can read, run, or mutate another tenant's dashboard.
type dashboardRepo interface {
	Create(ctx context.Context, in dashboard.CreateInput) (string, error)
	Get(ctx context.Context, tenantID, id string) (*dashboard.Record, error)
	List(ctx context.Context, tenantID string) ([]dashboard.Record, error)
	Update(ctx context.Context, tenantID, id string, in dashboard.UpdateInput) error
	Delete(ctx context.Context, tenantID, id string) error
}

// WidgetConfig is one widget in a dashboard definition. Type is a frontend render hint (kpi, bar,
// line, table) — the backend stores it verbatim and does NOT branch on it at run time. GroupBy
// presence decides scalar vs grouped: absent/empty → scalar (KPI); present → grouped.
type WidgetConfig struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Type    string `json:"type"`              // render hint: kpi|bar|line|table|funnel
	Entity  string `json:"entity"`            // dynamic entity name, e.g. "accounts"
	Metric  string `json:"metric"`            // count|sum|avg|min|max
	Field   string `json:"field,omitempty"`   // optional field for sum/avg/min/max
	GroupBy string `json:"groupBy,omitempty"` // optional: presence → grouped result
	Bucket  string `json:"bucket,omitempty"`  // W4: day|week|month|quarter|year over a date GroupBy
	// DASH-WIDGET-SCHEMA: react-grid-layout coordinates. Persisted + returned so the FE stores a
	// dashboard's grid layout SERVER-side instead of per-browser localStorage. Each is bounded to
	// [0, widgetCoordMax]; an out-of-bounds value is rejected (WIDGET_LAYOUT_INVALID). Emitted
	// unconditionally (no omitempty) so a widget at the origin (x=0/y=0) still round-trips its
	// position rather than dropping the keys.
	X       int                 `json:"x"`
	Y       int                 `json:"y"`
	W       int                 `json:"w"`
	H       int                 `json:"h"`
	Filters []ReportFilterInput `json:"filters,omitempty"` // reuse same filter dialect as report/list
	// Joins (queue #294) carries a report's relationship chain onto the card, so GroupBy / Field may
	// be "<relationship>.<field>" tokens exactly as in the report (same engine, same per-hop ACL).
	Joins []ReportJoinInput `json:"joins,omitempty"`
}

// validWidgetTypes is the set of accepted widget type tokens. Backend stores, does not branch on them.
var validWidgetTypes = map[string]struct{}{
	"kpi":    {},
	"bar":    {},
	"line":   {},
	"table":  {},
	"funnel": {},
}

// widgetCoordMax is the inclusive upper bound for a widget's grid coordinates (x/y/w/h). The lower
// bound is 0. 48 comfortably covers a react-grid-layout board (typ. 12 cols, arbitrary rows)
// while rejecting absurd/negative values that would break the FE grid renderer.
const widgetCoordMax = 48

// DashboardService implements the EPIC 6 Dashboard M2 admin CRUD over /admin/dashboards plus the
// run endpoint (GET /api/v1/dashboards/:id/run). Every method takes the verified tenant id and pins
// all repository calls to it. Config validation ensures each widget has a valid type + metric +
// non-empty entity at write time; deep semantic checks (entity exists? field indexed?) happen at
// run time via the underlying aggregate engine so a metadata change after save is caught honestly.
//
// RunDashboard executes every widget through RecordService.Aggregate — NEVER directly via dynrepo —
// so the tenant + A5 row-isolation contract is preserved. Per-widget errors are captured and
// returned in the response body; a bad widget does NOT abort the whole run (200 overall).
type DashboardService struct {
	repo    dashboardRepo
	records *RecordService // executes each widget through the aggregate engine
	logger  *zap.Logger    // server-side log for per-widget failures (raw error never sent to client)
	// auditor receives one __dashboard__ row per committed write (specs/062 L4); noop until
	// WithAuditor. draftMode skips the row for a write that only reached the builder's draft (FR-022).
	auditor   audit.Auditor
	draftMode docWriteModer
}

// NewDashboardService wires the dashboard service over a dashboard repository and the record
// service that runs each widget's config.
func NewDashboardService(repo dashboardRepo, records *RecordService, logger *zap.Logger) *DashboardService {
	return &DashboardService{repo: repo, records: records, logger: logger, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for dashboard create / update / delete and the draft mode that
// suppresses the row for a draft-only write ("dashboard" is a draft type, so main.go passes the
// DraftGate). Chainable; a nil auditor keeps the noop.
func (s *DashboardService) WithAuditor(a audit.Auditor, mode docWriteModer) *DashboardService {
	if a != nil {
		s.auditor = a
	}
	s.draftMode = mode
	return s
}

// CreateDashboardInput is the payload to create a dashboard.
type CreateDashboardInput struct {
	Name        string
	Description string
	Widgets     []WidgetConfig
	ActorID     string
}

// UpdateDashboardInput is the full-replace payload to update a dashboard.
type UpdateDashboardInput struct {
	Name        string
	Description string
	Widgets     []WidgetConfig
}

// ListDashboards returns every dashboard in the tenant (admin list page).
func (s *DashboardService) ListDashboards(ctx context.Context, tenantID string) ([]dashboard.Record, error) {
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.DashboardService.ListDashboards: %w", err)
	}
	return rows, nil
}

// ListVisibleDashboards is ListDashboards for the open discovery list (GET /dashboards): a dashboard
// whose widgets all read entities the caller cannot view is dropped (ACL by construction — it would
// only render a wall of FORBIDDEN). A dashboard with no widgets, or with at least one readable
// widget, stays; each distinct entity is authorized once. An ACL infrastructure fault fails closed.
func (s *DashboardService) ListVisibleDashboards(ctx context.Context, scope RecordScope, tenantID string) ([]dashboard.Record, error) {
	rows, err := s.ListDashboards(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if scope.IsAdmin || s.records == nil {
		return rows, nil
	}
	readable := map[string]bool{}
	canRead := func(entity string) (bool, error) {
		if ok, seen := readable[entity]; seen {
			return ok, nil
		}
		_, err := s.records.AuthorizeRead(ctx, tenantID, scope, entity)
		var ve *ValidationError
		if err != nil && !(errors.As(err, &ve) && ve.Code == "FORBIDDEN") {
			return false, err
		}
		readable[entity] = err == nil
		return err == nil, nil
	}
	out := make([]dashboard.Record, 0, len(rows))
	for _, r := range rows {
		var ws []WidgetConfig
		if len(r.Widgets) > 0 {
			if err := json.Unmarshal(r.Widgets, &ws); err != nil {
				return nil, fmt.Errorf("services.DashboardService.ListVisibleDashboards: decode widgets of %s: %w", r.ID, err)
			}
		}
		keep := len(ws) == 0
		for _, w := range ws {
			ok, err := canRead(w.Entity)
			if err != nil {
				return nil, fmt.Errorf("services.DashboardService.ListVisibleDashboards: %w", err)
			}
			if ok {
				keep = true
				break
			}
		}
		if keep {
			out = append(out, r)
		}
	}
	return out, nil
}

// GetDashboard returns one tenant-scoped dashboard, or DASHBOARD_NOT_FOUND for an unknown id.
func (s *DashboardService) GetDashboard(ctx context.Context, tenantID, id string) (*dashboard.Record, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("DASHBOARD_ID_REQUIRED", "id dashboard là bắt buộc")
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, dashboard.ErrDashboardNotFound) {
			return nil, invalid("DASHBOARD_NOT_FOUND", "không tìm thấy dashboard")
		}
		return nil, fmt.Errorf("services.DashboardService.GetDashboard: %w", err)
	}
	return rec, nil
}

// CreateDashboard validates and inserts a dashboard. name is required; each widget must have a
// valid type, metric, and non-empty entity. Returns the created row.
func (s *DashboardService) CreateDashboard(ctx context.Context, tenantID string, in CreateDashboardInput) (*dashboard.Record, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("DASHBOARD_NAME_REQUIRED", "name dashboard là bắt buộc")
	}
	widgetsJSON, err := ValidateDashboardWidgets(in.Widgets)
	if err != nil {
		return nil, err
	}
	if err := s.checkWidgetJoinsDeep(ctx, tenantID, in.Widgets); err != nil {
		return nil, err
	}
	id, err := s.repo.Create(ctx, dashboard.CreateInput{
		TenantID:    tenantID,
		Name:        name,
		Description: strings.TrimSpace(in.Description),
		Widgets:     widgetsJSON,
		CreatedBy:   strings.TrimSpace(in.ActorID),
	})
	if err != nil {
		return nil, fmt.Errorf("services.DashboardService.CreateDashboard: %w", err)
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.DashboardMeta, id, audit.ActionCreate, nil, dashboardAuditSnapshot(rec))
	return rec, err
}

// UpdateDashboard re-validates and replaces a tenant-scoped dashboard. Unknown id → DASHBOARD_NOT_FOUND.
func (s *DashboardService) UpdateDashboard(ctx context.Context, tenantID, id string, in UpdateDashboardInput) (*dashboard.Record, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("DASHBOARD_ID_REQUIRED", "id dashboard là bắt buộc")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("DASHBOARD_NAME_REQUIRED", "name dashboard là bắt buộc")
	}
	widgetsJSON, err := ValidateDashboardWidgets(in.Widgets)
	if err != nil {
		return nil, err
	}
	if err := s.checkWidgetJoinsDeep(ctx, tenantID, in.Widgets); err != nil {
		return nil, err
	}
	// Snapshot before the write (a repository may hand back the very row it then mutates).
	beforeSnap := dashboardAuditSnapshot(priorDoc(func() (*dashboard.Record, error) { return s.repo.Get(ctx, tenantID, id) }))
	if err := s.repo.Update(ctx, tenantID, id, dashboard.UpdateInput{
		Name:        name,
		Description: strings.TrimSpace(in.Description),
		Widgets:     widgetsJSON,
	}); err != nil {
		if errors.Is(err, dashboard.ErrDashboardNotFound) {
			return nil, invalid("DASHBOARD_NOT_FOUND", "không tìm thấy dashboard")
		}
		return nil, fmt.Errorf("services.DashboardService.UpdateDashboard: %w", err)
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.DashboardMeta, id, audit.ActionUpdate, beforeSnap, dashboardAuditSnapshot(rec))
	return rec, err
}

// DeleteDashboard soft-deletes a tenant-scoped dashboard. Unknown id → DASHBOARD_NOT_FOUND.
func (s *DashboardService) DeleteDashboard(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("DASHBOARD_ID_REQUIRED", "id dashboard là bắt buộc")
	}
	// Say what was deleted: the row is gone after the write.
	beforeSnap := dashboardAuditSnapshot(priorDoc(func() (*dashboard.Record, error) { return s.repo.Get(ctx, tenantID, id) }))
	if err := s.repo.Delete(ctx, tenantID, id); err != nil {
		if errors.Is(err, dashboard.ErrDashboardNotFound) {
			return invalid("DASHBOARD_NOT_FOUND", "không tìm thấy dashboard")
		}
		return fmt.Errorf("services.DashboardService.DeleteDashboard: %w", err)
	}
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.DashboardMeta, id, audit.ActionDelete, beforeSnap, nil)
	return nil
}

// DashboardRunResult is the complete result of running a saved dashboard: the dashboard definition
// plus a per-widget result slice (one entry per widget in order).
type DashboardRunResult struct {
	Dashboard *dashboard.Record
	Widgets   []WidgetRunResult
}

// WidgetRunResult is the run outcome for one widget. On success, Result is non-nil and Error/Code
// are empty. On failure, Result is nil and Error/Code carry the error details. The overall run
// always returns 200; a per-widget failure does not abort the other widgets.
type WidgetRunResult struct {
	WidgetID string
	Title    string
	Type     string           // render hint forwarded from config
	Result   *AggregateResult // nil on error
	Error    string           // human-readable error, empty on success
	Code     string           // machine-readable error code, empty on success
}

// RunDashboard loads a tenant-scoped dashboard and executes each widget through RecordService.Aggregate.
// Per-widget errors are captured and returned inside WidgetRunResult; they do NOT abort the run.
// The caller receives a 200 overall even when some widgets fail; each failure carries Error + Code.
//
// overlay (W4, nil = none) is the request-time created_at/created_by filter overlay applied to
// EVERY widget's underlying aggregate — it narrows what the widgets count/sum without touching
// the saved config (persist-nothing). created_at/created_by are mandatory system columns on
// every obj_* table, so no widget can lack them (no skip/error case exists for the overlay).
//
// CRITICAL: this method MUST enter at s.records.Aggregate — never at dynrepo directly — to
// preserve the tenant + A5 row-isolation contract (the same guarantee as a live aggregate request).
func (s *DashboardService) RunDashboard(ctx context.Context, scope RecordScope, tenantID, id string, overlay *RunOverlay) (DashboardRunResult, error) {
	rec, err := s.GetDashboard(ctx, tenantID, id)
	if err != nil {
		return DashboardRunResult{}, err
	}

	// Decode stored widget array from the raw JSON in the record.
	var widgets []WidgetConfig
	if len(rec.Widgets) > 0 && string(rec.Widgets) != "null" && string(rec.Widgets) != "[]" {
		if err := json.Unmarshal(rec.Widgets, &widgets); err != nil {
			return DashboardRunResult{}, fmt.Errorf("services.DashboardService.RunDashboard: decode widgets: %w", err)
		}
	}

	results := make([]WidgetRunResult, 0, len(widgets))
	for _, w := range widgets {
		wr := runWidget(ctx, s.records, s.logger, scope, tenantID, w, overlay)
		results = append(results, wr)
	}

	return DashboardRunResult{
		Dashboard: rec,
		Widgets:   results,
	}, nil
}

// runWidget executes a single widget through the aggregate engine, capturing any error without
// propagating it — the caller continues regardless of per-widget success or failure. overlay
// (W4) is ANDed onto the widget's saved filters via the aggregate engine's overlay path; a
// widget whose bucket config no longer matches its metadata (e.g. groupBy field type changed)
// surfaces as a per-widget AGGREGATE_BUCKET_* error, not an aborted run.
func runWidget(ctx context.Context, records *RecordService, logger *zap.Logger, scope RecordScope, tenantID string, w WidgetConfig, overlay *RunOverlay) WidgetRunResult {
	base := WidgetRunResult{
		WidgetID: w.ID,
		Title:    w.Title,
		Type:     w.Type,
	}

	q := AggregateQuery{
		Metric:  AggregateMetric(strings.ToLower(strings.TrimSpace(w.Metric))),
		Field:   strings.TrimSpace(w.Field),
		GroupBy: strings.TrimSpace(w.GroupBy),
		Bucket:  strings.TrimSpace(w.Bucket),
		Filters: toFilterInputs(w.Filters),
		Overlay: overlay,
		Joins:   toAggregateJoins(w.Joins),
	}

	// DASHBOARD-RUN-ACL (parity with REPORT-RUN-ACL): the run endpoint is mounted OUTSIDE the
	// /:entity RequireACL middleware, so scopeOf fails closed to AccessNone → 0 rows for non-admins.
	// Resolve the caller's "view" access level on THIS widget's entity here (mirrors AuthorizeView
	// in the report run path). A widget whose entity the caller cannot view surfaces as a PER-WIDGET
	// FORBIDDEN error (it must not kill sibling widgets); an allowed widget runs with the RESOLVED
	// A5 level; admin is unchanged. The resolved scope still applies tenant + A5 row-scoping.
	resolvedScope, err := records.AuthorizeRead(ctx, tenantID, scope, w.Entity)
	// Enter through RecordService.Aggregate — preserves tenant + A5 row-isolation contract.
	var res AggregateResult
	if err == nil {
		res, err = records.Aggregate(ctx, resolvedScope, tenantID, w.Entity, q)
	}
	if err != nil {
		// Extract a structured code when available (ValidationError), otherwise treat as internal.
		var ve *ValidationError
		if errors.As(err, &ve) {
			base.Error = ve.Msg
			base.Code = ve.Code
		} else if errors.Is(err, ErrEntityNotFound) {
			base.Error = "entity không tồn tại"
			base.Code = "ENTITY_NOT_FOUND"
		} else {
			// Do NOT echo the raw DB error to the client — MySQL driver text can carry obj_* table
			// and generated-column names (info disclosure, OWASP A09). Log it server-side and return
			// a generic message, mirroring DynamicRecordHandler.respondError's hygiene.
			if logger != nil {
				logger.Warn("dashboard widget aggregate failed",
					zap.String("widgetId", w.ID), zap.String("entity", w.Entity), zap.Error(err))
			}
			base.Error = "lỗi tính toán widget"
			base.Code = "WIDGET_RUN_FAILED"
		}
		return base
	}

	base.Result = &res
	return base
}

// ValidateDashboardWidgets is the exported config-time validator for a dashboard's widget list —
// ONE body with the apply path: CreateDashboard and UpdateDashboard run THIS function
// (validator-parity), and the AI Builder's propose_dashboard tool runs it too, so a draft that
// buffers is exactly what POST /admin/dashboards accepts. It normalizes in place (bucket token
// lowercased, like the report config) and returns the marshaled widgets JSON the repository
// persists. Validation is config-time shape only by design; deep semantic checks (entity exists?
// field indexed?) stay at RUN time via the aggregate engine so a metadata change after save is
// caught honestly.
func ValidateDashboardWidgets(widgets []WidgetConfig) ([]byte, error) {
	return validateAndMarshalWidgets(widgets)
}

// validateAndMarshalWidgets validates each widget's required fields and marshals the slice to JSON
// for storage. Validation is config-time shape only; deep semantic checks (entity exists? field
// indexed?) happen at run time via the aggregate engine.
func validateAndMarshalWidgets(widgets []WidgetConfig) ([]byte, error) {
	// Normalize to empty slice so storage never sees null.
	if widgets == nil {
		widgets = []WidgetConfig{}
	}
	for i, w := range widgets {
		if _, ok := validWidgetTypes[strings.ToLower(w.Type)]; !ok {
			return nil, invalid("WIDGET_TYPE_INVALID",
				"widget[%d] type không hợp lệ: %q (chỉ kpi/bar/line/table/funnel)", i, w.Type)
		}
		// DASH-WIDGET-SCHEMA: grid coordinates must be within [0, widgetCoordMax]. Checked in a
		// fixed order (x,y,w,h) so the reported field is deterministic.
		if err := checkWidgetLayout(i, w); err != nil {
			return nil, err
		}
		metric := strings.ToLower(strings.TrimSpace(w.Metric))
		if _, ok := validAggMetrics[AggregateMetric(metric)]; !ok {
			return nil, invalid("WIDGET_METRIC_INVALID",
				"widget[%d] metric không hợp lệ: %q (chỉ count/sum/avg/min/max)", i, w.Metric)
		}
		if strings.TrimSpace(w.Entity) == "" {
			return nil, invalid("WIDGET_ENTITY_REQUIRED",
				"widget[%d] entity là bắt buộc", i)
		}
		// W4 bucket (B6 config-time parity with the run-time aggregate validation): the token
		// must be a valid bucket and requires a groupBy. Whether that groupBy is actually a
		// date/datetime field is a metadata question re-checked honestly at RUN time
		// (AGGREGATE_BUCKET_FIELD_NOT_DATE per widget), like every other deep semantic check.
		if b := strings.ToLower(strings.TrimSpace(w.Bucket)); b != "" {
			if _, ok := validAggBuckets[b]; !ok {
				return nil, invalid("WIDGET_BUCKET_INVALID",
					"widget[%d] bucket không hợp lệ: %q (chỉ day/week/month/quarter/year)", i, w.Bucket)
			}
			if strings.TrimSpace(w.GroupBy) == "" {
				return nil, invalid("WIDGET_BUCKET_REQUIRES_GROUPBY",
					"widget[%d] bucket cần groupBy (field date/datetime hoặc created_at/updated_at)", i)
			}
			// Persist the normalized token (report config does the same) so FE readers
			// never have to case-fold; run-time lowercases anyway.
			widgets[i].Bucket = b
		}
		if err := checkWidgetJoins(i, &widgets[i]); err != nil {
			return nil, err
		}
	}
	b, err := json.Marshal(widgets)
	if err != nil {
		return nil, fmt.Errorf("services.DashboardService: marshal widgets: %w", err)
	}
	return b, nil
}

// checkWidgetJoins is the config-time shape check of a widget's joins (queue #294) — the SAME
// body the report config runs (checkJoinChainShape) plus the two widget-side invariants: every
// "<rel>." token prefix on groupBy/field names one of the widget's joins, and filters stay
// BASE-only. Relationship existence / direction / per-hop ACL stay run-time (and are re-checked at
// save by DashboardService.checkWidgetJoinsDeep). Normalizes the relationship names in place.
func checkWidgetJoins(i int, w *WidgetConfig) error {
	if len(w.Joins) == 0 {
		return nil
	}
	if err := checkJoinChainShape(toAggregateJoins(w.Joins)); err != nil {
		return err
	}
	names := make(map[string]bool, len(w.Joins))
	for k, j := range w.Joins {
		w.Joins[k] = ReportJoinInput{Relationship: strings.TrimSpace(j.Relationship), Optional: j.Optional}
		names[w.Joins[k].Relationship] = true
	}
	for _, tok := range []string{strings.TrimSpace(w.GroupBy), strings.TrimSpace(w.Field)} {
		if pre, _, ns := splitJoinedToken(tok); ns && !names[pre] {
			return invalid("REPORT_JOIN_ENTITY_MISMATCH",
				"widget[%d] field %q tham chiếu %q — không khớp quan hệ nào của widget", i, tok, pre)
		}
	}
	for _, f := range w.Filters {
		if _, _, ns := splitJoinedToken(strings.TrimSpace(f.Field)); ns {
			return invalid("REPORT_JOIN_FILTER_UNSUPPORTED",
				"widget[%d] lọc theo field liên quan (%q) chưa hỗ trợ — filter chỉ áp trên entity gốc", i, f.Field)
		}
	}
	return nil
}

// checkWidgetJoinsDeep runs the registry-backed report-join check (relationship exists in this
// tenant, each hop starts from the entity the previous one reached, N-N fan-out) for every joined
// widget at SAVE time — a widget that saves cannot be one whose join always dies at run.
func (s *DashboardService) checkWidgetJoinsDeep(ctx context.Context, tenantID string, widgets []WidgetConfig) error {
	if s.records == nil {
		return nil
	}
	for _, w := range widgets {
		if len(w.Joins) == 0 {
			continue
		}
		if err := s.records.ValidateReportJoinConfig(ctx, tenantID, w.Entity, toAggregateJoins(w.Joins),
			AggregateMetric(strings.ToLower(strings.TrimSpace(w.Metric)))); err != nil {
			return err
		}
	}
	return nil
}

// checkWidgetLayout validates a widget's grid coordinates (DASH-WIDGET-SCHEMA). X/Y are grid
// OFFSETS valid from origin 0 ([0, widgetCoordMax]). W/H are EXTENTS with a SENTINEL: both 0 means
// "layout not yet set" (the FE auto-places/sizes the widget — the common case for a freshly-added
// widget, and the shape existing dashboards persist), and both ≥1 means an explicitly sized tile.
// A SINGLE zero extent (w=0,h=5 or w=6,h=0) is a degenerate zero-area tile that would render
// invisible/collapsed → rejected. Coords checked in a fixed order so the offending name is
// deterministic.
func checkWidgetLayout(i int, w WidgetConfig) error {
	coords := []struct {
		name string
		val  int
	}{{"x", w.X}, {"y", w.Y}, {"w", w.W}, {"h", w.H}}
	for _, c := range coords {
		if c.val < 0 || c.val > widgetCoordMax {
			return invalid("WIDGET_LAYOUT_INVALID",
				"widget[%d] %s=%d ngoài khoảng cho phép [0, %d]", i, c.name, c.val, widgetCoordMax)
		}
	}
	// Zero-area guard: extents must be BOTH unset (0,0) or BOTH positive — never one-sided.
	if (w.W == 0) != (w.H == 0) {
		return invalid("WIDGET_LAYOUT_INVALID",
			"widget[%d] w=%d h=%d: kích thước phải cùng =0 (chưa đặt layout) hoặc cùng >0 (đã đặt)", i, w.W, w.H)
	}
	return nil
}
