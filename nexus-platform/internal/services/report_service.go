package services

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
)

// reportRepo is the consumer-side slice of the report store the service needs. Declared here so
// the service is unit-testable with an in-memory fake and does not import a concrete DB type.
// EVERY method is tenant-scoped in the real implementation (sys_reports.tenant_id), so no use
// case can read, run, or mutate another tenant's report.
type reportRepo interface {
	Create(ctx context.Context, in report.CreateInput) (string, error)
	Get(ctx context.Context, tenantID, id string) (*report.Record, error)
	List(ctx context.Context, tenantID string) ([]report.Record, error)
	Update(ctx context.Context, tenantID, id string, in report.UpdateInput) error
	Delete(ctx context.Context, tenantID, id string) error
}

// reportEntityChecker reports whether an entity name resolves to a real (non-deleted) entity in
// the tenant. Satisfied by *MetaCache / the metadata repository (GetEntityByName). The report
// service uses it to reject a report pointing at an entity that does not exist, so a dead report
// is never persisted. Optional: a nil checker skips the existence check (unit tests).
type reportEntityChecker interface {
	GetEntityByName(ctx context.Context, tenantID, name string) (*metadata.EntityDefinition, error)
}

// ReportFilterInput is one filter predicate in a report config. Field + Op + Values mirror the
// list/aggregate ?field[op]=value vocabulary exactly, so a saved report reuses the SAME filter
// engine as a live list/aggregate query (no second filter dialect).
type ReportFilterInput struct {
	Field  string   `json:"field"`
	Op     string   `json:"op,omitempty"`     // empty == eq
	Values []string `json:"values,omitempty"` // one (eq/neq/...) or more (in/between) raw string values
}

// ReportSortInput is the optional sort of a tabular report.
type ReportSortInput struct {
	Field string `json:"field"`
	Dir   string `json:"dir,omitempty"` // asc|desc (default desc)
}

// ReportJoinInput is one requested cross-object hop of a saved report (REPORT-JOIN v1: at most
// ONE, FK-on-child 1-1/1-N, summary/matrix only). Relationship names a sys_relationships row
// (tenant-scoped); Optional selects LEFT JOIN (true — keep base rows with no related match,
// group key NULL "—") vs INNER (false, default). GroupBy / Field may then reference a related
// field as "<relationship>.<field>"; filters stay BASE-only in v1.
type ReportJoinInput struct {
	Relationship string `json:"relationship"`
	Optional     bool   `json:"optional,omitempty"`
}

// ReportConfig is the validated, decoded report definition. A single struct covers all three
// report types; the service validates that the fields required by the report TYPE are present:
//   - tabular:  Columns (the projection) — required; Filters/Sort optional.
//   - summary:  GroupBy + Metric (+ Field for sum/avg/min/max) — required; Filters optional;
//     SubGroups optional (specs/049 S3): ≤ 2 further levels, run as a tree with sub-totals.
//   - matrix:   same as summary, plus an optional ColumnGroup (specs/049 S2b). With it the report
//     runs as a cross-tab (rows = GroupBy, columns = ColumnGroup); without it — every matrix saved
//     before S2b — it runs as the single-axis summary it always was.
type ReportConfig struct {
	Columns []string            `json:"columns,omitempty"`
	Filters []ReportFilterInput `json:"filters,omitempty"`
	GroupBy string              `json:"groupBy,omitempty"`
	Metric  string              `json:"metric,omitempty"`
	Field   string              `json:"field,omitempty"`
	Bucket  string              `json:"bucket,omitempty"` // W4: day|week|month|quarter|year over a date GroupBy (summary/matrix only)
	Sort    *ReportSortInput    `json:"sort,omitempty"`
	Joins   []ReportJoinInput   `json:"joins,omitempty"` // REPORT-JOIN v1: ≤1 hop, summary/matrix only
	// ColumnGroup is the column axis of a matrix (specs/049 S2b): with it the matrix runs as a
	// cross-tab (GroupBy = rows); without it, the single-axis summary it always was.
	ColumnGroup *ReportGroupInput `json:"columnGroup,omitempty"`
	// SubGroups are the next grouping levels of a summary after GroupBy (specs/049 S3, ≤ 2): with
	// them the summary runs as a tree with sub-totals; without them, the one-level summary it was.
	SubGroups []ReportGroupInput `json:"subGroups,omitempty"`
	// Having is the threshold on the LEAF groups of a summary/matrix (specs/049 S5): only the leaf
	// groups passing it are kept, and every total adds the kept leaves only.
	Having *ReportHavingInput `json:"having,omitempty"`
}

// ReportGroupInput is one extra GROUP BY axis of a report config: a field token (a base field, or
// "<rel>.<field>" on the configured join) plus an optional day|week|month|quarter|year bucket.
type ReportGroupInput struct {
	Field  string `json:"field"`
	Bucket string `json:"bucket,omitempty"`
}

// ReportHavingInput is a report threshold (specs/049 S5): On "value" (the metric) or "count" (the
// row count), Op gt|gte|lt|lte|eq|neq|between, and 1 decimal value (2 for between).
type ReportHavingInput struct {
	On     string   `json:"on"`
	Op     string   `json:"op"`
	Values []string `json:"values"`
}

// ReportService implements the EPIC 6 M2 admin CRUD over /admin/reports plus the run endpoint
// (GET /api/v1/reports/:id/run). Every method takes the verified tenant id and pins all
// repository calls to it, so no use case can touch another tenant's report. It validates the
// type/config shape BEFORE any write so a stored report always carries a runnable definition,
// and it RUNS a report by replaying its config through the existing aggregate (summary/matrix)
// or list (tabular) engine on RecordService — so a report reflects live, tenant + A5 row-scoped
// records and never references a field that vanished since it was saved (run-time re-validation).
type ReportService struct {
	repo     reportRepo
	records  *RecordService      // executes the saved config (aggregate/list)
	entities reportEntityChecker // optional; nil → skip entity-existence check on write
	// auditor receives one __report__ row per committed write (specs/062 L4); noop until WithAuditor.
	// draftMode skips the row for a write that only reached the builder's draft (FR-022).
	auditor   audit.Auditor
	draftMode docWriteModer
}

// NewReportService wires the report service over a report repository and the record service that
// runs the saved config.
func NewReportService(repo reportRepo, records *RecordService) *ReportService {
	return &ReportService{repo: repo, records: records, auditor: audit.NewNoopAuditor()}
}

// WithAuditor injects the audit sink for report create / update / delete and the draft mode that
// suppresses the row for a draft-only write ("report" is a draft type, so main.go passes the
// DraftGate). Chainable; a nil auditor keeps the noop.
func (s *ReportService) WithAuditor(a audit.Auditor, mode docWriteModer) *ReportService {
	if a != nil {
		s.auditor = a
	}
	s.draftMode = mode
	return s
}

// WithEntityChecker injects the metadata entity-existence checker so create/update reject a
// report targeting an unknown entity. Chainable.
func (s *ReportService) WithEntityChecker(c reportEntityChecker) *ReportService {
	s.entities = c
	return s
}

// CreateReportInput is the payload to create a report.
type CreateReportInput struct {
	Name       string
	EntityName string
	Type       string
	Config     ReportConfig
	IsActive   *bool // defaults to true when nil
	ActorID    string
}

// UpdateReportInput is the full-replace payload to update a report.
type UpdateReportInput struct {
	Name       string
	EntityName string
	Type       string
	Config     ReportConfig
	IsActive   *bool
	ActorID    string
}

// ListReports returns every report in the tenant (admin list page).
func (s *ReportService) ListReports(ctx context.Context, tenantID string) ([]report.Record, error) {
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.ReportService.ListReports: %w", err)
	}
	return rows, nil
}

// GetReport returns one tenant-scoped report, or REPORT_NOT_FOUND for an unknown id.
func (s *ReportService) GetReport(ctx context.Context, tenantID, id string) (*report.Record, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("REPORT_ID_REQUIRED", "id report là bắt buộc")
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, report.ErrReportNotFound) {
			return nil, invalid("REPORT_NOT_FOUND", "không tìm thấy report")
		}
		return nil, fmt.Errorf("services.ReportService.GetReport: %w", err)
	}
	return rec, nil
}

// CreateReport validates and inserts a report. name/entityName/type are required; the type must
// be one of tabular/summary/matrix; the entity must exist; the config must satisfy the type's
// shape. Returns the created row.
func (s *ReportService) CreateReport(ctx context.Context, tenantID string, in CreateReportInput) (*report.Record, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("REPORT_NAME_REQUIRED", "name report là bắt buộc")
	}
	// AI-APPLY-IDEMPOTENT (3.1): sys_reports has no UNIQUE(tenant,name) — a repeated Apply
	// silently duplicated the saved report. App-level list-then-check → 409 (parity with the
	// other draft kinds); "same name = update" callers use PUT on the existing id.
	existing, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.ReportService.CreateReport: dup check: %w", err)
	}
	for i := range existing {
		// A report the builder marked "to delete" in the draft overlay is as good as gone (specs/063 S7-2).
		if existing[i].DraftState != draftStateDeleted && strings.TrimSpace(existing[i].Name) == name {
			return nil, invalid("REPORT_ALREADY_EXISTS", "report %q đã tồn tại — sửa báo cáo cũ thay vì tạo trùng", name)
		}
	}
	entity, err := s.validateEntity(ctx, tenantID, in.EntityName)
	if err != nil {
		return nil, err
	}
	// nil liveFields → shape-only validation; deep semantic checks (field existence, numeric-ness,
	// indexed groupBy, date-typed bucket target) stay deferred to RUN time by design — see
	// ValidateReportConfig's doc.
	typ, configJSON, err := ValidateReportConfig(in.Type, in.Config, nil)
	if err != nil {
		return nil, err
	}
	// REPORT-JOIN: reject at SAVE time a join whose relationship is unknown / not an edge of the
	// report entity, or (REPORT-JOIN-NN) a SUM/AVG metric across an N-N hop (symmetric fan-out —
	// EITHER side, base or far, can double-count) — the registry-only checks that don't need the
	// caller's roleID. Keeps a stored report always runnable (no "save-then-always-die") and keeps
	// spec §4 truthful.
	if err := s.records.ValidateReportJoinConfig(ctx, tenantID, entity, toAggregateJoins(in.Config.Joins),
		AggregateMetric(strings.ToLower(strings.TrimSpace(in.Config.Metric)))); err != nil {
		return nil, err
	}
	if err := s.validateResolvedAxes(ctx, tenantID, entity, typ, in.Config); err != nil {
		return nil, err
	}
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	id, err := s.repo.Create(ctx, report.CreateInput{
		TenantID:   tenantID,
		Name:       name,
		EntityName: entity,
		Type:       typ,
		Config:     configJSON,
		IsActive:   isActive,
		CreatedBy:  strings.TrimSpace(in.ActorID),
	})
	if err != nil {
		return nil, fmt.Errorf("services.ReportService.CreateReport: %w", err)
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.ReportMeta, id, audit.ActionCreate, nil, reportAuditSnapshot(rec))
	return rec, err
}

// validateResolvedAxes rejects at SAVE time a multi-axis report whose axes name one physical column
// under different tokens (#261) — the metadata-backed check the shape-only validator cannot do.
func (s *ReportService) validateResolvedAxes(ctx context.Context, tenantID, entity string, typ report.Type, cfg ReportConfig) error {
	if typ != report.TypeSummary && typ != report.TypeMatrix {
		return nil
	}
	axes, matrix := reportGridAxes(typ, cfg)
	return s.records.ValidateReportAxes(ctx, tenantID, entity, axes, matrix)
}

// UpdateReport re-validates and replaces a tenant-scoped report. Unknown id → REPORT_NOT_FOUND.
func (s *ReportService) UpdateReport(ctx context.Context, tenantID, id string, in UpdateReportInput) (*report.Record, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, invalid("REPORT_ID_REQUIRED", "id report là bắt buộc")
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, invalid("REPORT_NAME_REQUIRED", "name report là bắt buộc")
	}
	entity, err := s.validateEntity(ctx, tenantID, in.EntityName)
	if err != nil {
		return nil, err
	}
	// Same shape-only validation as CreateReport — ONE exported body (ValidateReportConfig).
	typ, configJSON, err := ValidateReportConfig(in.Type, in.Config, nil)
	if err != nil {
		return nil, err
	}
	// REPORT-JOIN save-time relationship + N-N fan-out check (parity with CreateReport).
	if err := s.records.ValidateReportJoinConfig(ctx, tenantID, entity, toAggregateJoins(in.Config.Joins),
		AggregateMetric(strings.ToLower(strings.TrimSpace(in.Config.Metric)))); err != nil {
		return nil, err
	}
	if err := s.validateResolvedAxes(ctx, tenantID, entity, typ, in.Config); err != nil {
		return nil, err
	}
	isActive := true
	if in.IsActive != nil {
		isActive = *in.IsActive
	}
	// Snapshot before the write (a repository may hand back the very row it then mutates).
	beforeSnap := reportAuditSnapshot(priorDoc(func() (*report.Record, error) { return s.repo.Get(ctx, tenantID, id) }))
	if err := s.repo.Update(ctx, tenantID, id, report.UpdateInput{
		Name:       name,
		EntityName: entity,
		Type:       typ,
		Config:     configJSON,
		IsActive:   isActive,
		UpdatedBy:  strings.TrimSpace(in.ActorID),
	}); err != nil {
		if errors.Is(err, report.ErrReportNotFound) {
			return nil, invalid("REPORT_NOT_FOUND", "không tìm thấy report")
		}
		return nil, fmt.Errorf("services.ReportService.UpdateReport: %w", err)
	}
	rec, err := s.repo.Get(ctx, tenantID, id)
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.ReportMeta, id, audit.ActionUpdate, beforeSnap, reportAuditSnapshot(rec))
	return rec, err
}

// DeleteReport soft-deletes a tenant-scoped report. Unknown id → REPORT_NOT_FOUND.
func (s *ReportService) DeleteReport(ctx context.Context, tenantID, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return invalid("REPORT_ID_REQUIRED", "id report là bắt buộc")
	}
	// Say what was deleted: the row is gone after the write.
	beforeSnap := reportAuditSnapshot(priorDoc(func() (*report.Record, error) { return s.repo.Get(ctx, tenantID, id) }))
	if err := s.repo.Delete(ctx, tenantID, id); err != nil {
		if errors.Is(err, report.ErrReportNotFound) {
			return invalid("REPORT_NOT_FOUND", "không tìm thấy report")
		}
		return fmt.Errorf("services.ReportService.DeleteReport: %w", err)
	}
	emitDocAudit(ctx, s.auditor, s.draftMode, tenantID, audit.ReportMeta, id, audit.ActionDelete, beforeSnap, nil)
	return nil
}

// ReportRunResult is the result of running a saved report. Exactly one of Tabular / Aggregate /
// Grid is populated:
//   - tabular                      → Tabular set (the projected record page + the column list used).
//   - summary / matrix, one axis   → Aggregate set (the grouped/scalar aggregate), Type summary.
//   - matrix with a column axis    → Grid set (a matrix GridRunResult), Type matrix; Axes holds the
//     saved row axis then the column axis, so a caller can name them without re-reading the config.
//   - summary with sub-levels      → Grid set (a tree GridRunResult), Type summary; Axes holds the
//     levels in order (groupBy, then subGroups) — specs/049 S3.
//   - summary / matrix with a threshold but no second axis → Grid set (a ONE-level tree), Type
//     summary (specs/049 S5). Having echoes the threshold a grid run applied (nil = none).
type ReportRunResult struct {
	Type      report.Type
	Tabular   *TabularRunResult
	Aggregate *AggregateResult
	Grid      *GridRunResult
	Axes      []ReportGroupInput
	Having    *ReportHavingInput
	// DraftState is "live" | "edited" | "new" | "deleted" when the builder runs the report over the
	// draft overlay (specs/063 S7-2); empty for everyone else.
	DraftState string
}

// TabularRunResult is a tabular report's output: the chosen columns plus the matching record page
// (each record's full data map; the handler/caller projects to Columns). Total is the offset-mode
// row count over the scoped/filtered set.
type TabularRunResult struct {
	Columns []string
	// ColumnLabels maps a column name to the display label of the field behind it
	// (sys_fields.label). It exists so a caller rendering a heading does not have to re-resolve
	// metadata, and is kept SEPARATE from Columns on purpose: Columns stays a []string because
	// the AI assistant's run_report tool (internal/ai/assistant_tools.go) walks it as map keys.
	// A column with no matching field (a saved report survives its field being deleted) has NO
	// entry here — callers fall back to the column name rather than dropping the column.
	ColumnLabels map[string]string
	Records      []ReportRow
	Total        int
}

// ReportRow is one row of a tabular report: the record id plus its data map. The handler projects
// Data to the report's Columns when shaping the response/CSV.
type ReportRow struct {
	ID   string
	Data map[string]any
}

// RunReport loads a tenant-scoped report and executes its saved config against LIVE records,
// scoped by tenant + A5 row-level access (identical to a direct list/aggregate request — a report
// only ever covers rows the caller may read). The config is re-validated against the entity's
// current metadata by the underlying list/aggregate engine, so a field removed since the report
// was saved surfaces as a clean 422 rather than stale data.
//
// overlay (W4, nil = none) is the request-time created_at/created_by filter overlay ANDed onto
// the saved filters — persist-nothing: the stored config is never mutated. ExportReportCSV
// applies the SAME overlay so the export always matches the on-screen run.
//
// Dispatch by type:
//   - tabular           → RecordService.List (filters + sort + first page), projected to Columns.
//   - summary / matrix  → RecordService.Aggregate (groupBy + metric + field + bucket + filters);
//     a matrix with a column axis, a summary with sub-levels, or either with a threshold →
//     RecordService.AggregateGrid (specs/049 S2b / S3 / S5).
func (s *ReportService) RunReport(ctx context.Context, scope RecordScope, tenantID, id string, overlay *RunOverlay) (ReportRunResult, error) {
	rec, cfg, err := s.loadRunnable(ctx, tenantID, id)
	if err != nil {
		return ReportRunResult{}, err
	}
	// REPORT-RUN-ACL: this endpoint is mounted OUTSIDE the /:entity RequireACL middleware (a report's
	// entity lives on the record, not in the URL), so resolve the caller's "view" access level on the
	// report's entity HERE — mirroring resolveReportJoin / the ⌘K assistant. A denied caller is refused
	// 403 BEFORE any data query; an allowed caller runs with the RESOLVED A5 level (admin unchanged).
	// Without this the handler-supplied scope fails closed to AccessNone → 0 rows for every non-admin.
	scope, err = s.records.AuthorizeRead(ctx, tenantID, scope, rec.EntityName)
	if err != nil {
		return ReportRunResult{}, err
	}

	var res ReportRunResult
	switch report.Type(rec.Type) {
	case report.TypeTabular:
		res, err = s.runTabular(ctx, scope, tenantID, rec.EntityName, cfg, overlay)
	case report.TypeSummary, report.TypeMatrix:
		res, err = s.runAggregate(ctx, scope, tenantID, rec.EntityName, report.Type(rec.Type), cfg, overlay)
	default:
		// Defensive: the type column is an ENUM validated on write, so this is unreachable.
		return ReportRunResult{}, invalid("REPORT_TYPE_INVALID", "type report không hợp lệ: %q", rec.Type)
	}
	res.DraftState = rec.DraftState // set only when the builder runs the report over the draft overlay
	return res, err
}

// loadRunnable loads a tenant-scoped report, rejects an inactive one, and decodes its config —
// the shared preamble of RunReport and ExportReportCSV (kept identical so run and export can
// never diverge on which report/config they execute).
func (s *ReportService) loadRunnable(ctx context.Context, tenantID, id string) (*report.Record, ReportConfig, error) {
	rec, err := s.GetReport(ctx, tenantID, id)
	if err != nil {
		return nil, ReportConfig{}, err
	}
	if !rec.IsActive {
		return nil, ReportConfig{}, invalid("REPORT_INACTIVE", "report đang tắt (isActive=false), không thể chạy")
	}
	var cfg ReportConfig
	if len(rec.Config) > 0 && string(rec.Config) != "null" {
		if err := json.Unmarshal(rec.Config, &cfg); err != nil {
			return nil, ReportConfig{}, fmt.Errorf("services.ReportService.loadRunnable: decode config: %w", err)
		}
	}
	return rec, cfg, nil
}

// reportRunPageLimit caps how many rows a single tabular report run returns. A report is a
// summary surface, not a bulk export (CSV export is the unbounded-stream path); a hard cap keeps
// the run response bounded. The list engine clamps internally too.
const reportRunPageLimit = 500

// runTabular executes a tabular report through the list engine: the saved filters (plus the W4
// overlay) narrow the set, the saved sort orders it, and the first page (≤ reportRunPageLimit)
// is returned. Columns are echoed so the caller projects each record's data map.
func (s *ReportService) runTabular(ctx context.Context, scope RecordScope, tenantID, entity string, cfg ReportConfig, overlay *RunOverlay) (ReportRunResult, error) {
	sort, order := "", ""
	if cfg.Sort != nil {
		sort = strings.TrimSpace(cfg.Sort.Field)
		order = normalizeSortDir(cfg.Sort.Dir)
	}
	// ponytail: one extra metadata resolve per run (both reads are already cached behind the
	// same catalog the List below goes through). The specs serve two purposes — which projected
	// columns are relation/user fields (→ ListQuery.Expand, REPORT-RELATION-LABELS) and the
	// column labels after the run. resolve() is the single existing entry point and returns the
	// whole entity; a narrower lookup would duplicate it for no measurable gain at report-sized
	// column counts.
	//
	// A resolve error is deliberately NOT fatal here: the List below runs against the same entity
	// and surfaces the real error. Losing the specs only degrades headings to field names and
	// leaves relation ids unresolved — never worth failing a report over.
	var specs []FieldSpec
	if _, sp, err := s.records.resolve(ctx, tenantID, entity); err == nil {
		specs = sp
	}
	q := ListQuery{
		Page:    1,
		Limit:   reportRunPageLimit,
		Sort:    sort,
		Order:   order,
		Filters: toFilterInputs(cfg.Filters),
		Overlay: overlay,
		// REPORT-RELATION-LABELS: a projected relation/user column is resolved to {id,label} by
		// the SAME applyExpand the record grid uses (batch per target entity, W5-A2 entity-view
		// gate, EXP-01 field-ACL gate on the labelField) — a report that lists customers must
		// name them, not print 36-char UUIDs. ExportReportCSV sends the same set.
		Expand: expandableColumns(specs, cfg.Columns),
	}
	res, err := s.records.List(ctx, scope, tenantID, entity, q)
	if err != nil {
		return ReportRunResult{}, err
	}
	rows := make([]ReportRow, 0, len(res.Records))
	for _, r := range res.Records {
		rows = append(rows, ReportRow{ID: r.ID, Data: r.Data})
	}
	// FIELD-ACL (RM-42/A6): a field the caller's field-ACL hides gets NO label, so its heading
	// falls back to the field name. This is not cosmetic. The name of every configured column
	// was already on the wire before labels existed (record_export.go:127-129 documents that as
	// deliberate — a hidden column exports as empty cells under its name), but an admin-authored
	// label carries meaning a machine name hides, AND `label != name` would otherwise be an
	// oracle separating "this field exists but is hidden from you" from "this column's field was
	// deleted" — precisely the distinction the FLS policy suppresses elsewhere
	// (handlers/fls_hidden_metadata_integration_test.go). Both cases must look identical.
	// The hidden set comes from computedHiddenFields, the SAME field-ACL path List just ran —
	// no second ACL road to drift.
	labels := map[string]string{}
	if specs != nil {
		hidden, hErr := s.records.computedHiddenFields(ctx, scope, tenantID, entity)
		if hErr == nil {
			want := make(map[string]struct{}, len(cfg.Columns))
			for _, c := range cfg.Columns {
				want[c] = struct{}{}
			}
			for _, f := range specs {
				if _, isHidden := hidden[f.Name]; isHidden {
					continue
				}
				if _, ok := want[f.Name]; ok && f.Label != "" {
					labels[f.Name] = f.Label
				}
			}
		}
		// hErr != nil leaves labels empty: fail-closed in the disclosure direction (every
		// heading degrades to its field name) rather than emitting labels we could not verify.
	}
	return ReportRunResult{
		Type: report.TypeTabular,
		Tabular: &TabularRunResult{
			Columns:      cfg.Columns,
			ColumnLabels: labels,
			Records:      rows,
			Total:        res.Total,
		},
	}, nil
}

// expandableColumns returns the projected columns whose field is a `relation` or `user` — the
// ones whose stored value is a bare id and must go through the list engine's expand to become a
// human label (REPORT-RELATION-LABELS). Mirrors the record grid, which sends the same set through
// ?expand= (nexus-web LayoutList relationColumns → lib/metadata isExpandableType). Only PROJECTED
// columns qualify: a relation the report does not show is neither fetched nor leaked. A column
// with no field (deleted since the report was saved) is skipped — the run returns null for it.
func expandableColumns(specs []FieldSpec, columns []string) []string {
	byName := make(map[string]string, len(specs))
	for _, f := range specs {
		byName[f.Name] = f.FieldType
	}
	var out []string
	for _, c := range columns {
		if t := byName[c]; t == "relation" || t == "user" {
			out = append(out, c)
		}
	}
	return out
}

// runAggregate executes a summary/matrix report through the aggregate engine (the saved groupBy +
// metric + field + bucket + filters, plus the W4 overlay). The aggregate engine validates
// metric/field/groupBy/bucket against the entity's live metadata (numeric-field, indexed-groupBy,
// date-typed bucket target), so an invalid saved config surfaces as a clean 422 at run time.
//
// A matrix carrying a column axis (specs/049 S2b), a summary carrying sub-levels (S3), or either
// carrying a threshold (S5) runs the grid engine instead — same metric, filters, overlay, join and the SAME preflight (resolve,
// per-hop ACL, field-ACL on every axis); anything else takes the single-axis path below untouched
// (FR-016: no byte changes).
func (s *ReportService) runAggregate(ctx context.Context, scope RecordScope, tenantID, entity string, typ report.Type, cfg ReportConfig, overlay *RunOverlay) (ReportRunResult, error) {
	// A threshold (S5) takes the grid path even over one axis: only there can the total add the
	// kept groups alone, and more than 1000 groups be refused instead of cut.
	if axes, matrix := reportGridAxes(typ, cfg); len(axes) > 1 || cfg.Having != nil {
		grid, err := s.records.AggregateGrid(ctx, scope, tenantID, entity, GridQuery{
			Metric:  AggregateMetric(strings.ToLower(strings.TrimSpace(cfg.Metric))),
			Field:   strings.TrimSpace(cfg.Field),
			Axes:    axes,
			Matrix:  matrix,
			Filters: toFilterInputs(cfg.Filters),
			Overlay: overlay,
			Joins:   toAggregateJoins(cfg.Joins),
			Having:  cfg.Having,
		})
		if err != nil {
			return ReportRunResult{}, err
		}
		levels := make([]ReportGroupInput, len(axes))
		for i, ax := range axes {
			levels[i] = ReportGroupInput(ax)
		}
		out := ReportRunResult{Type: report.TypeSummary, Grid: &grid, Axes: levels, Having: cfg.Having}
		if matrix {
			out.Type = report.TypeMatrix
		}
		if err := s.labelAxes(ctx, scope, tenantID, entity, cfg.Joins, axes, &out); err != nil {
			return ReportRunResult{}, err
		}
		return out, nil
	}
	q := AggregateQuery{
		Metric:  AggregateMetric(strings.ToLower(strings.TrimSpace(cfg.Metric))),
		Field:   strings.TrimSpace(cfg.Field),
		GroupBy: strings.TrimSpace(cfg.GroupBy),
		Bucket:  strings.TrimSpace(cfg.Bucket),
		Filters: toFilterInputs(cfg.Filters),
		Overlay: overlay,
		Joins:   toAggregateJoins(cfg.Joins),
	}
	res, err := s.records.Aggregate(ctx, scope, tenantID, entity, q)
	if err != nil {
		return ReportRunResult{}, err
	}
	out := ReportRunResult{Type: report.TypeSummary, Aggregate: &res}
	if err := s.labelAxes(ctx, scope, tenantID, entity, cfg.Joins, []GridAxisInput{{Field: q.GroupBy, Bucket: q.Bucket}}, &out); err != nil {
		return ReportRunResult{}, err
	}
	return out, nil
}

// labelAxes names the group keys of a summary/matrix run (specs/049 S6, FR-022) — axes in run
// order (groupBy, then the column axis or the sub-levels). Per axis:
//   - picklist → its options.valueLabels (the axis already passed field-ACL in the run);
//   - relation, `user`, and the owner (a reserved key, run as a `user` spec like its filter) → the
//     name, through the SAME applyExpand the tabular report's relation columns use: entity-view
//     on the relation's target (W5-A2) and field-ACL on its labelField (EXP-01). A caller who may
//     not see it gets no label and the name is never fetched. Only the name is kept (a `user`
//     expand's email is dropped);
//   - a time-bucketed axis, any other type, a token with no field → no label.
//
// A joined "<rel>.<field>" axis resolves on the related entity's specs. Labels are additive: key,
// value, count and order are never touched, and a key with no label reads as the key everywhere
// (screen, grid CSV). Runs inside runAggregate so /run and export.csv carry the same labels; the
// one-level CSV ignores them (FR-016 byte freeze).
//
// ponytail: re-resolves the entity (and the join) the run already resolved — both reads sit behind
// the metadata cache; threading them out of RecordService would widen its API for one caller.
func (s *ReportService) labelAxes(ctx context.Context, scope RecordScope, tenantID, entity string, joins []ReportJoinInput, axes []GridAxisInput, res *ReportRunResult) error {
	slots := axisGroups(res, len(axes))
	var specs []FieldSpec
	var join *resolvedReportJoin
	for i, ax := range axes {
		if ax.Bucket != "" || ax.Field == "" || len(slots[i]) == 0 {
			continue
		}
		if specs == nil {
			_, sp, err := s.records.resolve(ctx, tenantID, entity)
			if err != nil {
				return fmt.Errorf("services.ReportService.labelAxes: %w", err)
			}
			specs = sp
		}
		axisSpecs, name := specs, ax.Field
		if pre, bare, namespaced := splitJoinedToken(ax.Field); namespaced && len(joins) > 0 {
			if join == nil {
				j, err := s.records.resolveReportJoin(ctx, scope, tenantID, entity, toAggregateJoins(joins))
				if err != nil {
					return fmt.Errorf("services.ReportService.labelAxes: join: %w", err)
				}
				join = j
			}
			if hop, ok := join.hopNamed(pre); ok {
				axisSpecs, name = hop.specs, bare
			}
		}
		spec, ok := findFieldSpec(axisSpecs, name)
		if _, owner := ownerReservedKeys[name]; !ok && owner {
			spec, ok = ownerFilterSpec(), true
		}
		if !ok {
			continue
		}
		labels, err := s.axisKeyLabels(ctx, scope, tenantID, spec, slots[i])
		if err != nil {
			return fmt.Errorf("services.ReportService.labelAxes: axis %q: %w", ax.Field, err)
		}
		for _, g := range slots[i] {
			if g.Key != nil {
				if l, ok := labels[*g.Key]; ok && l != "" {
					g.Label = &l
				}
			}
		}
	}
	return nil
}

// axisKeyLabels maps the keys of one axis to their names (see labelAxes); a key with no name is
// absent from the map.
func (s *ReportService) axisKeyLabels(ctx context.Context, scope RecordScope, tenantID string, spec FieldSpec, groups []*AggregateGroupResult) (map[string]string, error) {
	out := map[string]string{}
	switch spec.FieldType {
	case "picklist":
		vl, _ := spec.Options["valueLabels"].(map[string]any)
		for k, v := range vl {
			if l, ok := v.(string); ok {
				out[k] = l
			}
		}
	case "relation", "user":
		if s.records.fetcher == nil {
			return out, nil
		}
		// One synthetic record per distinct key, expanded exactly like a record page's column.
		recs := make([]dynrepo.Record, 0, len(groups))
		seen := map[string]struct{}{}
		for _, g := range groups {
			if g.Key == nil {
				continue
			}
			if _, dup := seen[*g.Key]; dup {
				continue
			}
			seen[*g.Key] = struct{}{}
			recs = append(recs, dynrepo.Record{Data: map[string]any{spec.Name: *g.Key}})
		}
		r := s.records
		if err := r.expandRecords(ctx, scope, tenantID, recs, []FieldSpec{spec}, []string{spec.Name}); err != nil {
			return nil, err
		}
		for _, rec := range recs {
			switch v := rec.Data[spec.Name].(type) {
			case expandedValue:
				out[v.ID] = v.Label
			case expandedUserValue:
				out[v.ID] = v.Label
			}
		}
	}
	return out, nil
}

// axisGroups lists, per axis, the groups a run holds on it: a one-level summary's groups (axis
// 0), a matrix's rows (axis 0) and columns (axis 1), a tree's nodes by depth.
func axisGroups(res *ReportRunResult, n int) [][]*AggregateGroupResult {
	out := make([][]*AggregateGroupResult, n)
	switch {
	case res.Aggregate != nil:
		for i := range res.Aggregate.Groups {
			out[0] = append(out[0], &res.Aggregate.Groups[i])
		}
	case res.Grid != nil && res.Grid.Kind == GridKindMatrix && n == 2:
		for i := range res.Grid.Rows {
			out[0] = append(out[0], &res.Grid.Rows[i])
		}
		for i := range res.Grid.Columns {
			out[1] = append(out[1], &res.Grid.Columns[i])
		}
	case res.Grid != nil:
		var walk func(nodes []GridNode, depth int)
		walk = func(nodes []GridNode, depth int) {
			if depth >= n {
				return
			}
			for i := range nodes {
				out[depth] = append(out[depth], &nodes[i].AggregateGroupResult)
				walk(nodes[i].Children, depth+1)
			}
		}
		walk(res.Grid.Groups, 0)
	}
	return out
}

// findFieldSpec returns the spec of the named field.
func findFieldSpec(specs []FieldSpec, name string) (FieldSpec, bool) {
	for _, f := range specs {
		if f.Name == name {
			return f, true
		}
	}
	return FieldSpec{}, false
}

// reportGridAxes lists a summary/matrix report's GROUP BY axes in run order — groupBy, then the
// matrix column axis or the summary sub-levels — trimmed like the single-axis path. One axis is
// the single-axis report; matrix = the second axis is a column axis (a cross-tab, not a tree).
// Run and the deep config check both read it, so they can never disagree on a report's axes.
func reportGridAxes(typ report.Type, cfg ReportConfig) ([]GridAxisInput, bool) {
	axes := []GridAxisInput{{Field: strings.TrimSpace(cfg.GroupBy), Bucket: strings.TrimSpace(cfg.Bucket)}}
	if typ == report.TypeMatrix && cfg.ColumnGroup != nil {
		return append(axes, GridAxisInput{Field: strings.TrimSpace(cfg.ColumnGroup.Field), Bucket: strings.TrimSpace(cfg.ColumnGroup.Bucket)}), true
	}
	if typ == report.TypeSummary {
		for _, g := range cfg.SubGroups {
			axes = append(axes, GridAxisInput{Field: strings.TrimSpace(g.Field), Bucket: strings.TrimSpace(g.Bucket)})
		}
	}
	return axes, false
}

// ExportReportCSV streams a saved report's result as RFC 4180 CSV, honoring the SAME overlay as
// RunReport (W4 parity guarantee: run and export share loadRunnable + the same filter path, so
// what you see on screen is exactly what you export — same rows, no divergence). By type:
//
//   - tabular: streams EVERY matching row (keyset id ASC — NOT capped at reportRunPageLimit; the
//     export is the bulk path), projected to the report's configured columns. Row ORDER may
//     differ from the on-screen sort (the stream walks id ASC for O(1) paging); the row SET is
//     identical.
//   - summary / matrix: writes the grouped aggregate as `key,value,count` rows (same group cap
//     and ordering as the run — chronological when bucketed, value DESC otherwise). A matrix with
//     a column axis or a summary with sub-levels writes the grid on screen instead — the
//     cross-tab or the tree with its sub-totals (writeGridCSV, specs/049 S4).
//
// Errors before the first row (unknown report, inactive, invalid config) return before any byte
// is written so the handler can still emit a JSON error envelope.
func (s *ReportService) ExportReportCSV(ctx context.Context, scope RecordScope, tenantID, id string, overlay *RunOverlay, w io.Writer) error {
	rec, cfg, err := s.loadRunnable(ctx, tenantID, id)
	if err != nil {
		return err
	}
	// REPORT-RUN-ACL (parity with RunReport): resolve the caller's "view" ACL on the report entity
	// BEFORE the first byte is written, so a denied caller gets a clean 403 (the handler resets to a
	// JSON envelope while nothing is streamed yet) and an allowed caller streams only their A5 rows.
	scope, err = s.records.AuthorizeRead(ctx, tenantID, scope, rec.EntityName)
	if err != nil {
		return err
	}

	switch report.Type(rec.Type) {
	case report.TypeTabular:
		// REPORT-RELATION-LABELS: the file must name the same customers/people the screen does.
		// The specs decide which projected columns are relation/user fields; this resolve is
		// pre-stream, so a failure stays a clean JSON envelope (ExportCSV re-resolves the same
		// entity first thing anyway).
		_, specs, err := s.records.resolve(ctx, tenantID, rec.EntityName)
		if err != nil {
			return err
		}
		return s.records.ExportCSV(ctx, scope, tenantID, rec.EntityName, ExportCSVOptions{
			Filters: toFilterInputs(cfg.Filters),
			Overlay: overlay,
			Columns: cfg.Columns,
			Expand:  expandableColumns(specs, cfg.Columns),
		}, w)
	case report.TypeSummary, report.TypeMatrix:
		res, err := s.runAggregate(ctx, scope, tenantID, rec.EntityName, report.Type(rec.Type), cfg, overlay)
		if err != nil {
			return err
		}
		// specs/049 S4 (FR-020): a grid exports as the grid on screen, never as the three-column
		// key,value,count file (#51). The axis headings are the field labels under the SAME
		// field-ACL policy as the tabular export (every axis already passed the field-ACL gate of
		// the run, so none is hidden); a token with no field — a system column, a joined
		// "<rel>.<field>" — prints as itself, as the tabular export does. Everything that can fail
		// runs before the first byte, so an error is still a JSON envelope.
		if res.Grid != nil {
			_, specs, err := s.records.resolve(ctx, tenantID, rec.EntityName)
			if err != nil {
				return err
			}
			labels := s.records.exportHeaderLabels(ctx, scope, tenantID, rec.EntityName, specs)
			titles := make([]string, len(res.Axes))
			for i, ax := range res.Axes {
				titles[i] = ax.Field
				if l := labels[ax.Field]; l != "" {
					titles[i] = l
				}
			}
			return writeGridCSV(res.Grid, titles, w)
		}
		return writeAggregateCSV(res.Aggregate, w)
	default:
		return invalid("REPORT_TYPE_INVALID", "type report không hợp lệ: %q", rec.Type)
	}
}

// writeAggregateCSV renders a SINGLE-AXIS aggregate result as CSV: grouped → header key,value,count
// with one row per group (NULL key/value → empty cell); scalar (defensive — summary configs always
// carry a groupBy) → header value,count with a single row. Its bytes are frozen (specs/049 FR-016);
// a grid result goes through writeGridCSV.
func writeAggregateCSV(agg *AggregateResult, w io.Writer) error {
	if agg == nil {
		return fmt.Errorf("services.writeAggregateCSV: nil aggregate result")
	}
	// Same Excel contract as the record export: a downloaded .csv opens as UTF-8 only when it
	// starts with a BOM. Safe at the top here — runAggregate has already completed, so no
	// pre-stream error can still need a JSON envelope.
	if _, err := io.WriteString(w, utf8BOM); err != nil {
		return fmt.Errorf("services.writeAggregateCSV: write BOM: %w", err)
	}
	cw := csv.NewWriter(w)
	if agg.Grouped {
		if err := cw.Write([]string{"key", "value", "count"}); err != nil {
			return fmt.Errorf("services.writeAggregateCSV: header: %w", err)
		}
		for _, g := range agg.Groups {
			// Group keys can carry tenant text (picklist/indexed text values) — same
			// formula-injection guard as record cells. Values are DECIMAL text (safe,
			// and csvGuard leaves plain numbers untouched).
			if err := cw.Write([]string{csvGuard(strOrEmpty(g.Key)), csvGuard(strOrEmpty(g.Value)), strconv.FormatInt(g.Count, 10)}); err != nil {
				return fmt.Errorf("services.writeAggregateCSV: row: %w", err)
			}
		}
	} else {
		if err := cw.Write([]string{"value", "count"}); err != nil {
			return fmt.Errorf("services.writeAggregateCSV: header: %w", err)
		}
		if err := cw.Write([]string{strOrEmpty(agg.Scalar.Value), strconv.FormatInt(agg.Scalar.Count, 10)}); err != nil {
			return fmt.Errorf("services.writeAggregateCSV: row: %w", err)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("services.writeAggregateCSV: flush: %w", err)
	}
	return nil
}

// gridBlankKey is how the NULL group of an axis (records with no value on it — a real group, not a
// sub-total) prints in the grid CSV, so its "Tổng (trống)" line reads as a group, never as a blank.
const gridBlankKey = "(trống)"

// writeGridCSV renders a grid run as the file the screen draws (specs/049 FR-020), titles being the
// axis headings in run order (2 for a matrix, one per level for a tree):
//
//   - matrix: "<rows> \ <columns>", the column keys, "Tổng"; per row its key, a cell per column
//     (empty where the row has no data there) and the row total; a last "Tổng" line with the
//     column totals and the grand total.
//   - tree: one column per level, then "Giá trị,Số lượng"; depth first, a leaf line repeating its
//     ancestors' keys; each branch closed by a "Tổng <key>" line in the branch's own column; the
//     "Tổng cộng" line last.
//
// A "key" is the group's label when the run named it (S6: tên khách hàng, "Báo giá"), else the key
// itself — the same text the screen draws. Same rows, order and totals as /run — no second
// computation, the ROLLUP numbers are printed as they came (NULL value → empty cell). Every cell
// goes through csvGuard (keys, labels and headings are tenant text; decimals pass unchanged). The file is composed in memory and written in one go, so
// any error — even a malformed grid — leaves w untouched and the caller still answers with a JSON
// envelope.
//
// ponytail: buffered, not streamed — the grid is bounded by the Q3 cap (≤ 1000 cells / leaf
// groups), so the whole file is a few tens of KB at most.
func writeGridCSV(g *GridRunResult, titles []string, w io.Writer) error {
	if g == nil {
		return fmt.Errorf("services.writeGridCSV: nil grid result")
	}
	if g.Kind == GridKindMatrix && len(titles) != 2 || g.Kind != GridKindMatrix && len(titles) == 0 {
		return fmt.Errorf("services.writeGridCSV: %d axis titles for a %s", len(titles), g.Kind)
	}
	var buf bytes.Buffer
	buf.WriteString(utf8BOM)
	cw := csv.NewWriter(&buf)
	write := func(cells []string) error {
		for i, c := range cells {
			cells[i] = csvGuard(c)
		}
		return cw.Write(cells)
	}
	// A group prints as its label (S6 — the name /run carries), else its key, the NULL group as
	// "(trống)": the file names the rows and columns exactly as the screen does.
	name := func(gr AggregateGroupResult) string {
		switch {
		case gr.Label != nil:
			return *gr.Label
		case gr.Key == nil:
			return gridBlankKey
		}
		return *gr.Key
	}
	var err error
	if g.Kind == GridKindMatrix {
		err = writeMatrixLines(g, titles, name, write)
	} else {
		err = writeTreeLines(g, titles, name, write)
	}
	if err != nil {
		return fmt.Errorf("services.writeGridCSV: %w", err)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("services.writeGridCSV: flush: %w", err)
	}
	if _, err := buf.WriteTo(w); err != nil {
		return fmt.Errorf("services.writeGridCSV: write: %w", err)
	}
	return nil
}

// writeMatrixLines writes the cross-tab: the dense rows × columns grid (≤ the Q3 cap, so held in
// memory), filled from the sparse cells.
func writeMatrixLines(g *GridRunResult, titles []string, name func(AggregateGroupResult) string, write func([]string) error) error {
	n := len(g.Columns)
	head := make([]string, 0, n+2)
	last := make([]string, 0, n+2)
	head, last = append(head, titles[0]+` \ `+titles[1]), append(last, "Tổng")
	for _, c := range g.Columns {
		head, last = append(head, name(c)), append(last, strOrEmpty(c.Value))
	}
	lines := make([][]string, len(g.Rows))
	for i, r := range g.Rows {
		lines[i] = make([]string, n+2)
		lines[i][0], lines[i][n+1] = name(r), strOrEmpty(r.Value)
	}
	for _, c := range g.Cells {
		if c.R < 0 || c.R >= len(lines) || c.C < 0 || c.C >= n {
			return fmt.Errorf("cell (%d,%d) outside the %d×%d grid", c.R, c.C, len(lines), n)
		}
		lines[c.R][1+c.C] = strOrEmpty(c.Value)
	}
	if err := write(append(head, "Tổng")); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	for _, l := range lines {
		if err := write(l); err != nil {
			return fmt.Errorf("row: %w", err)
		}
	}
	if err := write(append(last, strOrEmpty(g.Total.Value))); err != nil {
		return fmt.Errorf("total: %w", err)
	}
	return nil
}

// writeTreeLines writes the tree depth first, a branch's sub-total line after its children.
func writeTreeLines(g *GridRunResult, titles []string, name func(AggregateGroupResult) string, write func([]string) error) error {
	n := len(titles)
	line := func(path []string, label string, value *string, count int64) []string {
		l := make([]string, n+2)
		copy(l, path)
		l[len(path)], l[n], l[n+1] = label, strOrEmpty(value), strconv.FormatInt(count, 10)
		return l
	}
	var walk func(nodes []GridNode, path []string) error
	walk = func(nodes []GridNode, path []string) error {
		if len(path) >= n {
			return fmt.Errorf("tree deeper than its %d levels", n)
		}
		for _, nd := range nodes {
			k := name(nd.AggregateGroupResult)
			if len(nd.Children) == 0 {
				if err := write(line(path, k, nd.Value, nd.Count)); err != nil {
					return fmt.Errorf("row: %w", err)
				}
				continue
			}
			if err := walk(nd.Children, append(path[:len(path):len(path)], k)); err != nil {
				return err
			}
			if err := write(line(path, "Tổng "+k, nd.Value, nd.Count)); err != nil {
				return fmt.Errorf("sub-total: %w", err)
			}
		}
		return nil
	}
	if err := write(append(append([]string{}, titles...), "Giá trị", "Số lượng")); err != nil {
		return fmt.Errorf("header: %w", err)
	}
	if err := walk(g.Groups, nil); err != nil {
		return err
	}
	if err := write(line(nil, "Tổng cộng", g.Total.Value, g.Total.Count)); err != nil {
		return fmt.Errorf("total: %w", err)
	}
	return nil
}

// strOrEmpty dereferences a nullable string cell for CSV rendering (NULL → empty cell).
func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// toAggregateJoins maps the report config joins to the aggregate-engine vocabulary (REPORT-JOIN).
// Relationship names are trimmed; existence/direction/ACL resolve at RUN time in
// RecordService.resolveReportJoin (the caller's roleID is a run-time input — spec §4).
func toAggregateJoins(in []ReportJoinInput) []AggregateJoin {
	if len(in) == 0 {
		return nil
	}
	out := make([]AggregateJoin, 0, len(in))
	for _, j := range in {
		out = append(out, AggregateJoin{Relationship: strings.TrimSpace(j.Relationship), Optional: j.Optional})
	}
	return out
}

// toFilterInputs maps the report config filters to the list/aggregate FilterInput vocabulary.
func toFilterInputs(in []ReportFilterInput) []FilterInput {
	if len(in) == 0 {
		return nil
	}
	out := make([]FilterInput, 0, len(in))
	for _, f := range in {
		// An empty op decodes to dynrepo's empty FilterOperator, which List/Aggregate treat as eq
		// (backward-compatible), mirroring the ?field=value handler default.
		out = append(out, FilterInput{
			Field:    strings.TrimSpace(f.Field),
			Operator: dynrepo.FilterOperator(strings.TrimSpace(f.Op)),
			Values:   f.Values,
		})
	}
	return out
}

// validateEntity trims + checks the entity name is non-empty and (when a checker is wired) refers
// to a real entity in the tenant. Returns the trimmed entity name.
func (s *ReportService) validateEntity(ctx context.Context, tenantID, raw string) (string, error) {
	entity := strings.TrimSpace(raw)
	if entity == "" {
		return "", invalid("REPORT_ENTITY_REQUIRED", "entityName của report là bắt buộc")
	}
	if s.entities != nil {
		if _, err := s.entities.GetEntityByName(ctx, tenantID, entity); err != nil {
			if errors.Is(err, metadata.ErrEntityNotFound) {
				return "", invalid("REPORT_ENTITY_NOT_FOUND", "entity %q không tồn tại", entity)
			}
			return "", fmt.Errorf("services.ReportService.validateEntity: %w", err)
		}
	}
	return entity, nil
}

// ValidateReportConfig is the EXPORTED config-time report validator — the ONE validation body
// behind the apply path (CreateReport/UpdateReport call it) and the AI Builder's propose_report
// tool (internal/ai, M8 — spec ../specs/020-e9-ai-builder/spec.md §15), the M6 ValidateLayoutConfig /
// M7 ValidateWorkflowConfig pattern: no hand-mirrored rule logic that could drift. It normalizes
// + validates the report type (REPORT_TYPE_INVALID) and the type-shape invariants
// (REPORT_CONFIG_INVALID via validateReportConfig), returning the normalized type and the
// marshaled config JSON ready for storage.
//
// liveFields controls the DEPTH of validation (mirrors ValidateWorkflowConfig's nil-fieldTypes
// convention):
//   - nil → shape-only. This is the APPLY path's mode: deep semantic checks (does the field
//     exist? is it numeric? is groupBy indexed? is the bucket target a date?) stay deferred to
//     RUN time, so a metadata change AFTER save is caught honestly by the aggregate/list engine.
//   - non-nil (the tool resolves them via its tenant-scoped reader.ListFields) → ADDITIONALLY
//     resolves the summary/matrix aggregate surface (metric/field/groupBy/bucket) through
//     resolveAggregatePlan and the filters through resolveFiltersForAggregate — the SAME single
//     bodies the run-time engine executes — so an off-spec proposal surfaces the run-time code
//     (AGGREGATE_* / LIST_UNKNOWN_FILTER / FILTER_*) at propose time for model self-correction,
//     instead of applying cleanly and dying on first run. The tool is thus deliberately STRICTER
//     than apply (M7 precedent); tabular columns stay unchecked on both paths (an unknown column
//     projects as null at run — not an error anywhere).
//
// Every violation returns a *ValidationError (typed for the AI tool's validationOf()).
func ValidateReportConfig(reportType string, cfg ReportConfig, liveFields []metadata.FieldDefinition) (report.Type, []byte, error) {
	typ, err := validateReportType(reportType)
	if err != nil {
		return "", nil, err
	}
	configJSON, err := validateReportConfig(typ, cfg)
	if err != nil {
		return "", nil, err
	}
	if liveFields != nil {
		specs, err := toFieldSpecs(liveFields)
		if err != nil {
			return "", nil, fmt.Errorf("services.ValidateReportConfig: %w", err)
		}
		byName := make(map[string]FieldSpec, len(specs))
		for _, f := range specs {
			byName[f.Name] = f
		}
		// Deep-check the aggregate surface with the EXACT normalization runAggregate applies to
		// a saved config, so propose-time and run-time see identical tokens.
		// REPORT-JOIN: a namespaced "<rel>.<field>" token resolves against the RELATED entity's
		// live fields and the hop needs the CALLER's ACL — both run-time inputs (spec §4), so a
		// joined report's aggregate surface stays run-time-checked; the structural join checks
		// (shape, ≤1, tabular reject, prefix-match) already ran in validateReportConfig. (The AI
		// tool strips joins outright, so deep mode never actually sees one today.)
		if typ == report.TypeSummary || typ == report.TypeMatrix {
			// The axes in run order; a matrix's column axis (specs/049 S2b) and a summary's
			// sub-levels (S3) are resolved by the SAME axis body as the groupBy, and — like the
			// grid run — their errors name the axis.
			axes, matrix := reportGridAxes(typ, cfg)
			var axisNames []string // nil = the single-axis report: errors exactly as before
			if len(axes) > 1 {
				if axisNames, err = gridAxisNames(GridQuery{Axes: axes, Matrix: matrix}); err != nil {
					return "", nil, err
				}
			}
			_, _, joined := splitJoinedToken(strings.TrimSpace(cfg.Field))
			for _, ax := range axes {
				_, _, ns := splitJoinedToken(ax.Field)
				joined = joined || ns
			}
			// Skip ONLY for a configured join whose token is actually namespaced — a dotted
			// token WITHOUT a join keeps the old strict deep-check (it would die at run too).
			if len(cfg.Joins) == 0 || !joined {
				metric := AggregateMetric(strings.ToLower(strings.TrimSpace(cfg.Metric)))
				// resolveAggregatePlan with an empty axis is metric → field; the axes follow in
				// order — the same sequence (and codes) as the single-axis plan and the run.
				if _, err := resolveAggregatePlan(metric, strings.TrimSpace(cfg.Field), "", "", specs, byName); err != nil {
					return "", nil, err
				}
				for i, ax := range axes {
					if _, err := resolveGroupAxis(ax.Field, ax.Bucket, specs, byName, nil); err != nil {
						return "", nil, tagAxis(err, axisNames, i)
					}
				}
				if len(axes) > 1 {
					if err := rejectResolvedDuplicateAxes(axes, axisNames, specs, byName); err != nil {
						return "", nil, err
					}
				}
			}
		}
		// Deep-check the filters for BOTH report types. For summary/matrix this is literally the
		// run body (Aggregate → resolveFiltersForAggregate); for tabular it is the same factored
		// body the export path runs and the equivalent of the List path's validation — at worst
		// the tool is marginally stricter, never looser.
		if _, err := resolveFiltersForAggregate(byName, toFilterInputs(cfg.Filters)); err != nil {
			return "", nil, err
		}
	}
	return typ, configJSON, nil
}

// validateReportType normalizes + validates the report type against the three supported shapes.
func validateReportType(raw string) (report.Type, error) {
	t := report.Type(strings.TrimSpace(strings.ToLower(raw)))
	if !report.ValidType(t) {
		return "", invalid("REPORT_TYPE_INVALID",
			"type không hợp lệ: %q (chỉ tabular/summary/matrix)", raw)
	}
	return t, nil
}

// validateReportConfig validates the config against the report type's required shape and marshals
// it to JSON for storage. The DEEP semantic validation (does the field exist? is it numeric? is
// groupBy indexed?) is deferred to RUN time (the list/aggregate engine) so a metadata change after
// save is caught honestly; here we enforce only the type-shape invariants a report cannot run
// without:
//   - tabular: at least one column (an empty projection is meaningless).
//   - summary/matrix: a groupBy and a metric; sum/avg/min/max additionally need a field.
func validateReportConfig(typ report.Type, cfg ReportConfig) ([]byte, error) {
	switch typ {
	case report.TypeTabular:
		cols := make([]string, 0, len(cfg.Columns))
		for _, c := range cfg.Columns {
			if t := strings.TrimSpace(c); t != "" {
				cols = append(cols, t)
			}
		}
		if len(cols) == 0 {
			return nil, invalid("REPORT_CONFIG_INVALID", "report tabular cần ít nhất 1 cột (config.columns)")
		}
		cfg.Columns = cols
		// W4: bucket is a summary/matrix concept — a tabular report has no GROUP BY to bucket.
		// Reject explicitly instead of silently ignoring (B6: a mistyped config must not
		// fall through).
		if strings.TrimSpace(cfg.Bucket) != "" {
			return nil, invalid("REPORT_CONFIG_INVALID",
				"config.bucket chỉ dùng cho report summary/matrix (tabular không có groupBy)")
		}
		// specs/049: the column axis is a matrix concept, sub-levels a summary one.
		if cfg.ColumnGroup != nil {
			return nil, invalid("REPORT_CONFIG_INVALID", "config.columnGroup chỉ dùng cho report matrix")
		}
		if len(cfg.SubGroups) > 0 {
			return nil, invalid("REPORT_CONFIG_INVALID", "config.subGroups chỉ dùng cho report summary")
		}
		// specs/049 S5: a threshold filters groups — a tabular report has none.
		if cfg.Having != nil {
			return nil, invalid("REPORT_CONFIG_INVALID", "config.having chỉ dùng cho report summary/matrix")
		}
		// REPORT-JOIN v1: tabular projection through a join is the deferred #2b slice.
		if len(cfg.Joins) > 0 {
			return nil, invalid("REPORT_JOIN_TABULAR_UNSUPPORTED",
				"config.joins chỉ dùng cho report summary/matrix ở v1 (tabular qua join là lát sau)")
		}
	case report.TypeSummary, report.TypeMatrix:
		if strings.TrimSpace(cfg.GroupBy) == "" {
			return nil, invalid("REPORT_CONFIG_INVALID", "report %s cần config.groupBy", typ)
		}
		metric := strings.TrimSpace(strings.ToLower(cfg.Metric))
		if metric == "" {
			return nil, invalid("REPORT_CONFIG_INVALID", "report %s cần config.metric (count/sum/avg/min/max)", typ)
		}
		if _, ok := validAggMetrics[AggregateMetric(metric)]; !ok {
			return nil, invalid("REPORT_CONFIG_INVALID",
				"config.metric không hợp lệ: %q (count/sum/avg/min/max)", cfg.Metric)
		}
		if metric != string(AggMetricCount) && strings.TrimSpace(cfg.Field) == "" {
			return nil, invalid("REPORT_CONFIG_INVALID",
				"report %s với metric %q cần config.field", typ, metric)
		}
		cfg.Metric = metric
		// W4 bucket (B6 config-time parity): the token must be one of the run-time set. The
		// deep date-type check on groupBy is re-validated at run time against live metadata
		// (AGGREGATE_BUCKET_FIELD_NOT_DATE), like metric/field/groupBy semantics.
		if b := strings.ToLower(strings.TrimSpace(cfg.Bucket)); b != "" {
			if _, ok := validAggBuckets[b]; !ok {
				return nil, invalid("REPORT_CONFIG_INVALID",
					"config.bucket không hợp lệ: %q (day/week/month/quarter/year)", cfg.Bucket)
			}
			cfg.Bucket = b
		} else {
			cfg.Bucket = ""
		}
		// specs/049 S2b column axis: a matrix only; a field; a bucket of the run-time set; and not
		// the row axis again (same field AND same bucket — "close_date by year × by month" is two
		// axes). Normalized like bucket above. Deep checks (indexed, date) stay run-time.
		if cfg.ColumnGroup != nil {
			if typ != report.TypeMatrix {
				return nil, invalid("REPORT_CONFIG_INVALID", "config.columnGroup chỉ dùng cho report matrix")
			}
			col := ReportGroupInput{
				Field:  strings.TrimSpace(cfg.ColumnGroup.Field),
				Bucket: strings.ToLower(strings.TrimSpace(cfg.ColumnGroup.Bucket)),
			}
			if col.Field == "" {
				return nil, invalid("REPORT_CONFIG_INVALID", "config.columnGroup cần field")
			}
			if col.Bucket != "" {
				if _, ok := validAggBuckets[col.Bucket]; !ok {
					return nil, invalid("REPORT_CONFIG_INVALID",
						"config.columnGroup.bucket không hợp lệ: %q (day/week/month/quarter/year)", cfg.ColumnGroup.Bucket)
				}
			}
			if col.Field == strings.TrimSpace(cfg.GroupBy) && col.Bucket == cfg.Bucket {
				return nil, invalid("REPORT_CONFIG_INVALID",
					"trục cột trùng trục hàng (cùng trường, cùng cách gom) — chọn trường khác hoặc cách gom khác")
			}
			cfg.ColumnGroup = &col
		}
		// specs/049 S3 sub-levels: a summary only; at most 2 (3 levels in all, FR-001); each a field
		// and a bucket of the run-time set; no level repeating an earlier one (same field AND same
		// bucket). Errors name the level (details.axis). Normalized like the column axis; an empty
		// list is stored as no key, so a one-level summary stays byte-identical.
		if len(cfg.SubGroups) > 0 {
			if typ != report.TypeSummary {
				return nil, invalid("REPORT_CONFIG_INVALID", "config.subGroups chỉ dùng cho report summary")
			}
			if len(cfg.SubGroups) > maxGridLevels-1 {
				return nil, invalid("REPORT_CONFIG_INVALID",
					"config.subGroups tối đa %d cấp (nhận %d)", maxGridLevels-1, len(cfg.SubGroups))
			}
			seen := map[ReportGroupInput]bool{{Field: strings.TrimSpace(cfg.GroupBy), Bucket: cfg.Bucket}: true}
			subs := make([]ReportGroupInput, 0, len(cfg.SubGroups))
			for i, g := range cfg.SubGroups {
				axis := []string{fmt.Sprintf("subGroups[%d]", i)}
				sub := ReportGroupInput{Field: strings.TrimSpace(g.Field), Bucket: strings.ToLower(strings.TrimSpace(g.Bucket))}
				if sub.Field == "" {
					return nil, tagAxis(invalid("REPORT_CONFIG_INVALID", "cấp nhóm thứ %d chưa chọn trường", i+2), axis, 0)
				}
				if _, ok := validAggBuckets[sub.Bucket]; sub.Bucket != "" && !ok {
					return nil, tagAxis(invalid("REPORT_CONFIG_INVALID",
						"cách gom thời gian của cấp nhóm thứ %d không hợp lệ: %q (day/week/month/quarter/year)", i+2, g.Bucket), axis, 0)
				}
				if seen[sub] {
					return nil, tagAxis(invalid("REPORT_CONFIG_INVALID",
						"cấp nhóm thứ %d trùng một cấp trước (cùng trường, cùng cách gom) — chọn trường khác hoặc cách gom khác", i+2), axis, 0)
				}
				seen[sub] = true
				subs = append(subs, sub)
			}
			cfg.SubGroups = subs
		} else {
			cfg.SubGroups = nil
		}
		// specs/049 S5 threshold on the leaf groups: subject, operator, value count, decimal values,
		// ordered bounds — the same body RecordService.AggregateGrid runs; stored normalized.
		h, err := normalizeHaving(cfg.Having)
		if err != nil {
			return nil, err
		}
		cfg.Having = h
		// REPORT-JOIN v1 shape invariants (B6 — a mistyped join config must not save-then-die
		// at run): ≤1 join with a non-empty relationship name; a namespaced groupBy/field must
		// reference THE configured join; filters stay BASE-only. Relationship existence,
		// direction, and the per-hop ACL are RUN-time checks (they need live registry state +
		// the caller's roleID — spec §4), enforced by resolveReportJoin.
		if len(cfg.Joins) > 0 {
			if err := checkJoinChainShape(toAggregateJoins(cfg.Joins)); err != nil {
				return nil, err
			}
			hopNames := make(map[string]bool, len(cfg.Joins))
			for i, j := range cfg.Joins {
				cfg.Joins[i] = ReportJoinInput{Relationship: strings.TrimSpace(j.Relationship), Optional: j.Optional}
				hopNames[cfg.Joins[i].Relationship] = true
			}
			tokens := []string{strings.TrimSpace(cfg.GroupBy), strings.TrimSpace(cfg.Field)}
			if cfg.ColumnGroup != nil {
				tokens = append(tokens, cfg.ColumnGroup.Field)
			}
			for _, g := range cfg.SubGroups {
				tokens = append(tokens, g.Field)
			}
			for _, tok := range tokens {
				if pre, _, namespaced := splitJoinedToken(tok); namespaced && !hopNames[pre] {
					return nil, invalid("REPORT_JOIN_ENTITY_MISMATCH",
						"field %q tham chiếu %q — không khớp join nào của báo cáo", tok, pre)
				}
			}
			for _, f := range cfg.Filters {
				if _, _, namespaced := splitJoinedToken(strings.TrimSpace(f.Field)); namespaced {
					return nil, invalid("REPORT_JOIN_FILTER_UNSUPPORTED",
						"lọc theo field liên quan (%q) chưa hỗ trợ ở v1 — filter chỉ áp trên entity gốc", f.Field)
				}
			}
		}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("services.validateReportConfig: %w", err)
	}
	return b, nil
}

// normalizeSortDir maps a report sort dir to the list-query order token (asc|desc; default desc).
func normalizeSortDir(dir string) string {
	if strings.EqualFold(strings.TrimSpace(dir), "asc") {
		return "asc"
	}
	return "desc"
}
