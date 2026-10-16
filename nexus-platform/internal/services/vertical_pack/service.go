package vertical_pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
	"nexus/platform/internal/view"
	"nexus/platform/internal/workflow"
	"nexus/platform/internal/workqueue"
)

// --- Reader interfaces (export): read a tenant's existing metadata. -------------------------
// Declared consumer-side so the service is unit-testable and depends on narrow capabilities,
// not concrete repositories. Every method is tenant-scoped in the real implementation.

// entityReader lists a tenant's entities and a single entity's live fields. Satisfied by
// *metadata.Repository (ListEntities is not ctx-aware; the rest are).
type entityReader interface {
	ListEntities(tenantID string) ([]metadata.EntityDefinition, error)
	ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error)
}

// layoutReader lists the stored layouts of one entity. Satisfied by *metadata.LayoutRepository.
type layoutReader interface {
	GetByEntity(ctx context.Context, tenantID, entityName string) ([]metadata.LayoutDefinition, error)
}

// roleReader lists a tenant's roles and a single role's grants. Satisfied by *auth.RoleRepository.
type roleReader interface {
	ListRoles(ctx context.Context, tenantID string) ([]auth.Role, error)
	ListActionsForRole(ctx context.Context, tenantID, roleID string) ([]auth.RoleAction, error)
}

// relationshipReader lists a tenant's relationship registry rows so Export can emit N-N
// relationships (RelationshipType == "N-N") from the registry rather than dropping them.
// Satisfied by *metadata.Repository.ListAllRelationships. Optional: when nil, Export skips N-N.
type relationshipReader interface {
	ListAllRelationships(ctx context.Context, tenantID string) ([]metadata.RelationshipDefinition, error)
}

// recordLister reads a tenant's records of one entity for the OPTIONAL sample export
// (?includeData=true). Satisfied by *dynrepo.Repository.ListRecords. Optional: when nil, Export
// never emits Samples (metadata-only pack).
type recordLister interface {
	ListRecords(ctx context.Context, entity, tenantID string, scope dynrepo.RowScope, p dynrepo.ListParams) ([]dynrepo.Record, int, error)
}

// linkReader reads a tenant's N-N join-table rows so Export can emit PackLinks alongside the
// Samples (?includeData=true) — PACK-EXPORT-FULL. Satisfied by *dynrepo.Repository.ListJoinRows.
// Optional: when nil, Export skips links and says so in its warnings.
type linkReader interface {
	ListJoinRows(ctx context.Context, entityA, entityB, tenantID string) ([]dynrepo.LinkRow, error)
}

// --- Writer interfaces (install): reuse the existing admin services. ------------------------
// Each call goes through the SAME validation / DDL / tenant-isolation / audit path as the
// hand-built admin endpoints, so install can never bypass a guard that a direct POST honors.

// entityWriter creates an entity (+ its initial non-relation fields) and soft-deletes one (for
// install's rollback). Satisfied by *services.EntityService. Idempotency existence checks are made
// in buildPlan via entityReader.ListEntities (W8-BE-2 refactor), not EntityExists.
type entityWriter interface {
	CreateEntity(ctx context.Context, tenantID string, in services.CreateEntityInput) (*metadata.EntityDefinition, []metadata.FieldDefinition, error)
	SoftDeleteEntity(ctx context.Context, tenantID, entityName string) error
}

// entityExistsChecker reports whether an active entity already exists for the tenant, backing
// install's idempotent skip. Satisfied by *metadata.Repository.EntityExists.
type entityExistsChecker interface {
	EntityExists(ctx context.Context, tenantID, name string) (bool, error)
}

// fieldWriter adds a single field to an existing entity. Satisfied by
// *services.FieldService.AddField. Used for the deferred relation-field pass.
type fieldWriter interface {
	AddField(ctx context.Context, tenantID string, in services.AddFieldInput) (*metadata.FieldDefinition, error)
}

// layoutWriter upserts a layout. Satisfied by *services.LayoutService.SaveLayout (an upsert:
// re-saving the same (entity,view) is a no-op-by-overwrite, which keeps install idempotent).
type layoutWriter interface {
	SaveLayout(ctx context.Context, tenantID string, in services.SaveLayoutInput) (*metadata.LayoutDefinition, error)
}

// roleWriter creates a role + its grants. Satisfied by *services.RoleService.CreateRole.
type roleWriter interface {
	ListRoles(ctx context.Context, tenantID string) ([]services.RoleView, error)
	CreateRole(ctx context.Context, tenantID string, in services.CreateRoleInput) (*services.RoleView, error)
	// GetRole/UpdateRole exist for the RE-handover path: a role that already exists used to be
	// skipped whole, so grants for entities shipped in a LATER delivery were never added and the
	// new tables were invisible to every non-admin in the customer tenant. Reconciling needs to
	// read the current grants (UpdateRole REPLACES the set, so a blind write would destroy any
	// the customer tuned by hand).
	GetRole(ctx context.Context, tenantID, id string) (*services.RoleView, error)
	UpdateRole(ctx context.Context, tenantID, id string, in services.UpdateRoleInput) (*services.RoleView, error)
}

// fieldACLWriter installs a pack's field-level restrictions. Satisfied by
// *services.FieldACLAdminService — the SAME body the admin screen writes through, so a pack cannot
// express a restriction the UI could not.
// fieldACLReader lists a role's field restrictions on one entity, for Export.
type fieldACLReader interface {
	ListGrants(ctx context.Context, tenantID, roleID, entity string) ([]services.FieldACLGrantView, error)
}

type fieldACLWriter interface {
	PutGrants(ctx context.Context, tenantID string, grants []services.FieldACLGrantInput) (*services.FieldACLPutResult, error)
}

// relationWriter registers an N-N relationship (creates join table + metadata row).
// Satisfied by *services.RelationService.RegisterRelationship. RELATIONSHIP_ALREADY_EXISTS
// is treated as an idempotent skip by Install.
type relationWriter interface {
	RegisterRelationship(ctx context.Context, tenantID string, input services.RegisterRelationshipInput) (*metadata.RelationshipDefinition, error)
}

// relationViewConfigWriter applies a relationship's related-list view config (queue #720).
// Satisfied by *services.RelationLifecycleService.SetViewConfig — the admin PUT's own validation.
type relationViewConfigWriter interface {
	SetViewConfig(ctx context.Context, tenantID, relID string, raw json.RawMessage) (map[string]any, error)
}

// viewStore lists and creates saved views (queue #720). Satisfied by *services.ViewService — the
// same validation and admin-only gate as POST /views.
type viewStore interface {
	List(ctx context.Context, tenantID, userID, entityName string) ([]view.Record, error)
	Create(ctx context.Context, tenantID, userID string, isAdmin bool, in services.CreateViewInput) (string, error)
}

// calendarStore lists and creates working calendars (queue #720). Satisfied by
// *services.BusinessCalendarService — the admin CRUD's own validation.
type calendarStore interface {
	ListCalendars(ctx context.Context, tenantID string) ([]calmeta.BusinessCalendar, error)
	CreateCalendar(ctx context.Context, tenantID string, in services.CalendarInput) (*calmeta.BusinessCalendar, error)
}

// dashboardWriter creates and lists saved dashboards so install can implement an idempotent
// skip-by-name pattern. sys_dashboards has no UNIQUE(tenant_id,name) constraint, so a naive
// re-install would silently duplicate the dashboard row. The interface is satisfied by
// *services.DashboardService: ListDashboards returns []dashboard.Record (each has a Name
// field) and CreateDashboard takes services.CreateDashboardInput returning *dashboard.Record.
type dashboardWriter interface {
	// ListDashboards returns all non-deleted dashboards for the tenant.
	ListDashboards(ctx context.Context, tenantID string) ([]dashboard.Record, error)
	// CreateDashboard validates and inserts one dashboard, returning the created row.
	CreateDashboard(ctx context.Context, tenantID string, in services.CreateDashboardInput) (*dashboard.Record, error)
}

// workflowWriter creates and lists workflow rules so install can implement an idempotent
// skip-by-name pattern (sys_workflows has no UNIQUE(tenant_id,name), so a naive re-install would
// silently duplicate the rule). CreateWorkflow runs the SAME full config-time validation as
// POST /admin/workflows (trigger/timer/condition/action + the WF-01 field-target guard), so a
// pack workflow referencing a missing field/team fails install with the real error code. The
// interface is satisfied by *services.WorkflowService: ListWorkflows returns []workflow.WorkflowRecord
// (each has a Name field) and CreateWorkflow takes services.CreateWorkflowInput.
type workflowWriter interface {
	// ListWorkflows returns every workflow rule for the tenant (backs the skip-by-name idempotency).
	ListWorkflows(ctx context.Context, tenantID string) ([]workflow.WorkflowRecord, error)
	// CreateWorkflow validates and inserts one workflow rule, returning the created row.
	CreateWorkflow(ctx context.Context, tenantID string, in services.CreateWorkflowInput) (*workflow.WorkflowRecord, error)
}

// credentialVault is the slice of the named-credential vault pack export/install needs (specs/050
// S6): list the tenant's keys (export: id → name + slot definition; install: reuse by name) and
// create an empty slot. Satisfied by *services.CredentialService. Configured is false when the
// server has no master key — a pack that needs the vault is then refused before any write.
type credentialVault interface {
	Configured() bool
	AllowsInsecureHTTP() bool
	List(ctx context.Context, tenantID string) ([]services.CredentialView, error)
	Create(ctx context.Context, tenantID string, in services.CredentialInput) (*services.CredentialView, string, error)
}

// reportWriter creates and lists saved reports so install can implement an idempotent skip-by-name
// pattern (sys_reports has no UNIQUE(tenant_id,name), so a naive re-install would silently duplicate
// the report row). CreateReport runs the SAME full config-time validation as POST /admin/reports
// (ValidateReportConfig: type/shape + entity existence), so a pack report with a malformed config
// fails install with the real error code — the contract that install reuses the admin write path,
// never a weaker one. The interface is satisfied by *services.ReportService: ListReports returns
// []report.Record (each has a Name field) and CreateReport takes services.CreateReportInput.
type reportWriter interface {
	// ListReports returns every saved report for the tenant (backs the skip-by-name idempotency).
	ListReports(ctx context.Context, tenantID string) ([]report.Record, error)
	// CreateReport validates and inserts one saved report, returning the created row.
	CreateReport(ctx context.Context, tenantID string, in services.CreateReportInput) (*report.Record, error)
}

// workQueueWriter creates and lists work queues so install can seed the pack's "Việc Hôm Nay"
// queues through the SAME validated path as POST /admin/work-queues (entity/field/operator/
// date-token/sort/role checks — PACK-QUEUES). ListAdmin backs both the skip-by-name idempotency
// (buildPlan) and the queue export. Satisfied by *services.WorkQueueService.
type workQueueWriter interface {
	// ListAdmin returns every queue for the tenant (backs skip-by-name idempotency + export).
	ListAdmin(ctx context.Context, tenantID string) ([]workqueue.Queue, error)
	// Create validates and inserts one queue, returning the created row.
	Create(ctx context.Context, tenantID string, in services.WorkQueueInput) (*workqueue.Queue, error)
}

// approvalWriter creates and lists approval processes so install can seed the pack's approval
// flows through the SAME validated path as POST /admin/approval-processes (PACK-APPROVALS —
// name/entity/trigger/criteria/steps validation lives in ApprovalService.CreateProcess, so
// validator-parity holds by construction: there is no second, weaker write path). ListProcesses
// backs both the skip-on-dup-by-(entity,name) idempotency (buildPlan) and the approval export.
// Satisfied by *services.ApprovalService.
// inboundHookWriter creates and lists inbound webhook hooks so install can seed a pack's hooks
// through the SAME validated path as POST /admin/inbound-hooks (mapping shape, regex, refField
// uniqueness and the entity references all live in InboundHookService — validator-parity by
// construction). ListHooks backs skip-by-name idempotency and the export. Satisfied by
// *services.InboundHookService.
//
// Note what is NOT in this port: any way to set a secret. Create mints its own, which is exactly
// how a pack stays free of live credentials (see Pack.InboundHooks).
type inboundHookWriter interface {
	// ListAdmin (not a bespoke name): the admin listing is exactly the set install must skip by
	// name, so reuse it rather than add a second read path.
	ListAdmin(ctx context.Context, tenantID string) ([]inboundhook.Hook, error)
	Create(ctx context.Context, tenantID, userID string, in services.InboundHookInput) (*inboundhook.Hook, error)
}

type approvalWriter interface {
	// ListProcesses returns every approval process for the tenant (backs idempotency + export).
	ListProcesses(ctx context.Context, tenantID string) ([]bpm.ProcessRecord, error)
	// CreateProcess validates and inserts one process, returning the created row.
	CreateProcess(ctx context.Context, tenantID string, in services.CreateApprovalProcessInput) (*bpm.ProcessRecord, error)
}

// teamWriter is the consumer-side slice of the team repository the PACK-TEAMS pass needs: (a)
// ListTeams to skip-by-name for idempotency AND to map a pre-existing team's name→id, (b)
// CreateTeam to seed a new team, (c) AddMember to add the install user as the team's first active
// member (so round-robin routing has ≥1 member immediately). Satisfied by *auth.TeamRepository —
// every method is tenant-scoped and parameterized (no hand-written team SQL). CreateTeam expects a
// server-minted id + tenant on the *auth.Team; AddMember expects a server-minted membership id.
// Optional: nil when no team repo is wired → the pack's teams are silently skipped (back-compat).
type teamWriter interface {
	ListTeams(ctx context.Context, tenantID string) ([]auth.Team, error)
	CreateTeam(ctx context.Context, team *auth.Team) error
	AddMember(ctx context.Context, tenantID, teamID, userID, membershipID string) error
}

// recordWriter creates a single dynamic record through the validated record service path.
// Satisfied by *services.RecordService.Create. Using the real service ensures every seed
// record goes through the same field-type validation, relation FK checks, and ACL that a
// hand-crafted POST would.
type recordWriter interface {
	Create(ctx context.Context, scope services.RecordScope, tenantID, userID, entityName string, payload map[string]any) (string, error)
}

// recordExistsChecker validates that a record ID exists in a given entity+tenant scope.
// Used by Install when calling Link (checker param). Satisfied by *dynrepo.Repository.
type recordExistsChecker interface {
	RecordExists(ctx context.Context, entity, tenantID, id string) (bool, error)
}

// objTableProber backs the dry-run's read-only conflict detection (trap B3): TableExists reports
// whether a shared obj_* table is already materialized, and LiveUniqueValueExists reports whether
// a LIVE row of THIS tenant already holds a unique value a sample would insert (which would 409
// RECORD_DUPLICATE at real-install time). Both are strictly read-only. Satisfied by
// *dynrepo.Repository. Optional: when nil, the dry-run simply reports no unique-slot conflicts.
type objTableProber interface {
	TableExists(ctx context.Context, entity string) (bool, error)
	LiveUniqueValueExists(ctx context.Context, tenantID, entity, field, value string) (bool, error)
}

// recordFetcher fetches a single record with a RowScope (for the Link source-gate).
// Satisfied by *dynrepo.Repository. Install calls Link with isAdmin=true + AccessAll scope
// so the fetch always succeeds for any record in the tenant.
type recordFetcher interface {
	GetRecord(ctx context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error)
}

// linker creates an N-N join-table row between two existing records.
// Satisfied by *services.RelationService.Link. Install passes isAdmin=true and nil
// aclChecker so ACL-on-target is bypassed during seed (the pack itself is the authority).
type linker interface {
	Link(
		ctx context.Context,
		tenantID, userID, roleID string,
		isAdmin bool,
		sourceEntity, sourceID string,
		relName, targetID string,
		checker services.TargetChecker,
		aclChecker services.TargetACLChecker,
		fetcher services.RecordFetcher,
		sourceScope dynrepo.RowScope,
	) (bool, error)
}

// Service exports a vertical pack from a tenant and installs one into a tenant. It is HTTP-
// agnostic; the handler maps its typed errors. Export is read-only; install reuses the admin
// write services so it shares their guards and is idempotent + best-effort-reversible.
type Service struct {
	// readers (export)
	entityRd   entityReader
	layoutRd   layoutReader
	roleRd     roleReader
	relationRd relationshipReader // optional: nil → Export skips N-N
	recordRd   recordLister       // optional: nil → Export never emits Samples
	linkRd     linkReader         // optional: nil → Export skips N-N link rows (warned)
	// writers (install)
	entityWr    entityWriter
	existsCk    entityExistsChecker // retained for Deps API stability (buildPlan uses ListEntities)
	fieldWr     fieldWriter
	layoutWr    layoutWriter
	roleWr      roleWriter
	relationWr  relationWriter
	viewWr      viewStore                // optional: nil → shared views neither exported nor installed
	calendarWr  calendarStore            // optional: nil → calendars neither exported nor installed (rules that name one are dropped / refused)
	relViewWr   relationViewConfigWriter // optional: nil → a pack's relationship viewConfig is skipped with a warning
	recordWr    recordWriter
	lnk         linker
	recExists   recordExistsChecker
	recFetch    recordFetcher
	dashboardWr dashboardWriter   // optional: nil when pack carries no dashboards
	workflowWr  workflowWriter    // optional: nil when pack carries no workflows
	vault       credentialVault   // optional: nil → vault-key rules are left out of an export / refused at install
	teamWr      teamWriter        // optional: nil when pack carries no teams (PACK-TEAMS)
	reportWr    reportWriter      // optional: nil when pack carries no reports (PACK-REPORTS)
	workQueueWr workQueueWriter   // optional: nil when pack carries no work queues (PACK-QUEUES)
	approvalWr  approvalWriter    // optional: nil when pack carries no approvals (PACK-APPROVALS)
	hookWr      inboundHookWriter // optional: nil when pack carries no inbound hooks (PACK-HOOKS)
	fieldACLWr  fieldACLWriter    // optional: nil → a pack's field restrictions WARN, never silently drop
	fieldACLRd  fieldACLReader    // optional: nil → export carries no field restrictions
	installs    installStore      // optional: nil → install recording is a no-op
	items       itemStore         // optional: nil → pack provenance is a no-op (KHUON-SONG-P1)
	objProber   objTableProber    // optional: nil → dry-run reports no unique-slot conflicts
	appMint     appMinter         // optional (WithAppService): nil → install mints no sidebar app
	// sched answers "does this tenant have a usable default working calendar" for a pack that ships
	// business_hours rules (DAILY-QUEUE #82). Optional: nil → no check, no warning.
	sched workflow.ScheduleResolver
	// delivery, when wired (WithDeliveryGate), refuses an install into a delivered workspace
	// (PROJECT_DELIVERED_INSTALL_BLOCKED, specs/063 FR-009). nil → no check (unit tests).
	delivery deliveryGate
	// drafts, when wired (WithDraftReader), lets the upgrade plan of a delivered project mark items
	// that already have a pending draft (draft_pending, specs/069 L7). nil → none.
	drafts draftLister
	logger *zap.Logger // never nil (defaults to Nop in NewService)
}

// deliveryGate answers "is this tenant delivered?". Satisfied by *services.DraftGate.
type deliveryGate interface {
	Delivered(ctx context.Context, tenantID string) (bool, error)
}

// WithDeliveryGate arms the delivered-workspace install block. Chainable.
func (s *Service) WithDeliveryGate(g deliveryGate) *Service {
	s.delivery = g
	return s
}

// Deps bundles the readers + writers the vertical-pack service needs. Built by the caller
// (main.go / tests) from the existing repositories and services.
type Deps struct {
	EntityReader   entityReader
	LayoutReader   layoutReader
	RoleReader     roleReader
	EntityWriter   entityWriter
	EntityExists   entityExistsChecker
	FieldWriter    fieldWriter
	LayoutWriter   layoutWriter
	RoleWriter     roleWriter
	RelationWriter relationWriter
	// ViewStore lets shared saved views travel with the pack (optional).
	ViewStore viewStore
	// CalendarStore lets calendars travel with the pack (optional).
	CalendarStore calendarStore
	// RelationViewConfigWriter carries relationship viewConfig on install (optional).
	RelationViewConfigWriter relationViewConfigWriter
	RecordWriter             recordWriter
	Linker                   linker
	RecordExists             recordExistsChecker
	RecordFetcher            recordFetcher
	// DashboardWriter enables Pass 7 (dashboard seeding). Optional: nil means no dashboards.
	// Satisfied by *services.DashboardService.
	DashboardWriter dashboardWriter
	// WorkflowWriter enables the workflows pass (seed automation rules AFTER samples + dashboards).
	// Optional: nil means the pack's workflows are silently skipped. Satisfied by *services.WorkflowService.
	WorkflowWriter workflowWriter
	// CredentialVault carries vault keys through packs by name (specs/050 S6). Optional: nil means
	// rules that use a key are left out of an export and a pack that declares keys is refused.
	// Satisfied by *services.CredentialService.
	CredentialVault credentialVault
	// TeamWriter enables the teams pass (PACK-TEAMS: seed teams AFTER roles, BEFORE workflows, so an
	// assign workflow's team NAME resolves to the seeded team's id). Optional: nil means the pack's
	// teams are silently skipped. Satisfied by *auth.TeamRepository.
	TeamWriter teamWriter
	// ReportWriter enables the reports pass (PACK-REPORTS: seed saved reports through the validated
	// CreateReport path). Optional: nil means the pack's reports are silently skipped. Satisfied by
	// *services.ReportService.
	ReportWriter reportWriter
	// WorkQueueWriter enables the work-queues pass (PACK-QUEUES: seed "Việc Hôm Nay" queues through
	// the validated Create path) and queue export. Optional: nil means the pack's queues are silently
	// skipped. Satisfied by *services.WorkQueueService.
	WorkQueueWriter workQueueWriter
	// ApprovalWriter enables the approvals pass (PACK-APPROVALS: seed approval processes through the
	// validated CreateProcess path) and approval export. Optional: nil means the pack's approvals are
	// silently skipped. Satisfied by *services.ApprovalService.
	ApprovalWriter    approvalWriter
	InboundHookWriter inboundHookWriter
	FieldACLWriter    fieldACLWriter
	FieldACLReader    fieldACLReader
	// RelationshipReader enables N-N export (Export reads the registry). Optional.
	// Satisfied by *metadata.Repository.
	RelationshipReader relationshipReader
	// RecordLister enables sample export (?includeData=true). Optional.
	// Satisfied by *dynrepo.Repository.
	RecordLister recordLister
	// LinkReader enables N-N link export alongside samples (PACK-EXPORT-FULL). Optional.
	// Satisfied by *dynrepo.Repository.
	LinkReader linkReader
	// InstallStore records + lists the install ledger. Optional (nil → no recording).
	// Satisfied by *packinstall.Repository.
	InstallStore installStore
	// ObjProber backs the dry-run's read-only unique-slot conflict probe. Optional (nil → the
	// dry-run reports no unique-slot conflicts). Satisfied by *dynrepo.Repository.
	ObjProber objTableProber
	// ScheduleResolver lets install warn when a pack's business_hours rules land in a tenant with no
	// usable default working calendar (DAILY-QUEUE #82) — the SAME resolver the workflow engine
	// computes deadlines with, so "no calendar" here means exactly what the run will do. Optional
	// (nil → no check). Satisfied by *services.WorkflowScheduleResolver.
	ScheduleResolver workflow.ScheduleResolver
	// Logger is used for fail-open install-recording warnings. Optional (nil → Nop).
	Logger *zap.Logger
}

// NewService wires the vertical-pack service over its readers and writers.
func NewService(d Deps) *Service {
	lg := d.Logger
	if lg == nil {
		lg = zap.NewNop()
	}
	return &Service{
		entityRd:    d.EntityReader,
		layoutRd:    d.LayoutReader,
		roleRd:      d.RoleReader,
		relationRd:  d.RelationshipReader,
		recordRd:    d.RecordLister,
		linkRd:      d.LinkReader,
		entityWr:    d.EntityWriter,
		existsCk:    d.EntityExists,
		fieldWr:     d.FieldWriter,
		layoutWr:    d.LayoutWriter,
		roleWr:      d.RoleWriter,
		relationWr:  d.RelationWriter,
		calendarWr:  d.CalendarStore,
		viewWr:      d.ViewStore,
		relViewWr:   d.RelationViewConfigWriter,
		recordWr:    d.RecordWriter,
		lnk:         d.Linker,
		recExists:   d.RecordExists,
		recFetch:    d.RecordFetcher,
		dashboardWr: d.DashboardWriter,
		workflowWr:  d.WorkflowWriter,
		vault:       d.CredentialVault,
		teamWr:      d.TeamWriter,
		reportWr:    d.ReportWriter,
		workQueueWr: d.WorkQueueWriter,
		approvalWr:  d.ApprovalWriter,
		hookWr:      d.InboundHookWriter,
		fieldACLWr:  d.FieldACLWriter,
		fieldACLRd:  d.FieldACLReader,
		installs:    d.InstallStore,
		objProber:   d.ObjProber,
		sched:       d.ScheduleResolver,
		logger:      lg,
	}
}

// ListInstalls returns the tenant's pack-install ledger (newest first) for the FE. Tenant-scoped.
// When no install store is wired it returns an empty slice (feature absent, not an error).
func (s *Service) ListInstalls(ctx context.Context, tenantID string) ([]InstallRecord, error) {
	if s.installs == nil {
		return []InstallRecord{}, nil
	}
	recs, err := s.installs.ListInstalls(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("vertical_pack.ListInstalls: %w", err)
	}
	if recs == nil {
		return []InstallRecord{}, nil // stable [] contract for the FE, never JSON null
	}
	return recs, nil
}

// validationError is the typed error install/export surface so the handler can map a code to an
// HTTP status, mirroring services.ValidationError (we reuse that exact type so statusForCode in
// the handlers package keeps working without a new case).
func invalid(code, format string, args ...any) error {
	return &services.ValidationError{Code: code, Msg: fmt.Sprintf(format, args...), Details: map[string]any{}}
}

// ErrPackEmpty is returned by Install when the pack carries no entities.
var ErrPackEmpty = errors.New("vertical pack has no entities")
