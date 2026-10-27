package main

import (
	// The embedded IANA timezone database. The runtime image is alpine WITHOUT tzdata
	// (Dockerfile stage 2 installs only ca-certificates) and the binary is CGO_ENABLED=0, so
	// without this import time.LoadLocation("Asia/Ho_Chi_Minh") fails IN PRODUCTION while
	// passing on every developer machine. That failure is silent and actively misleading: a
	// stored business calendar whose timezone will not load is treated as NO calendar
	// (services.WorkflowScheduleResolver), so every business_hours deadline degrades to
	// wall-clock and the warning says "no business calendar for this tenant" — while the admin
	// is looking at the calendar in the UI. Verified by running the shipped image shape:
	// `unknown time zone Asia/Ho_Chi_Minh` before this import, resolved after.
	// Preferred over `apk add tzdata` because it survives a base-image change.
	_ "time/tzdata"

	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nexus/platform/internal/ai"
	"nexus/platform/internal/appmeta"
	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
	"nexus/platform/internal/bpm"
	"nexus/platform/internal/calmeta"
	"nexus/platform/internal/config"
	"nexus/platform/internal/credential"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/db"
	"nexus/platform/internal/doctor"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/emaillog"
	"nexus/platform/internal/files"
	"nexus/platform/internal/handlers"
	"nexus/platform/internal/health"
	"nexus/platform/internal/idempotency"
	"nexus/platform/internal/inboundhook"
	"nexus/platform/internal/janitor"
	"nexus/platform/internal/jobs"
	"nexus/platform/internal/mcpserver"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/metaversion"
	"nexus/platform/internal/metrics"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/notes"
	"nexus/platform/internal/notification"
	"nexus/platform/internal/notify"
	"nexus/platform/internal/packinstall"
	"nexus/platform/internal/project"
	"nexus/platform/internal/report"
	"nexus/platform/internal/services"
	"nexus/platform/internal/services/vertical_pack"
	"nexus/platform/internal/shares"
	"nexus/platform/internal/storage"
	"nexus/platform/internal/tenantbackup"
	"nexus/platform/internal/usage"
	"nexus/platform/internal/view"
	"nexus/platform/internal/workflow"
	"nexus/platform/internal/workqueue"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"go.uber.org/zap"
)

// aiDraftFieldReader is the AI Builder's metadata reader: the registry repository, except that field
// lists come through the draft layer (specs/063 S7-8).
type aiDraftFieldReader struct {
	*metadata.Repository
	fields *services.DraftFieldRepo
}

func (r aiDraftFieldReader) ListFields(ctx context.Context, tenantID, entityID string) ([]metadata.FieldDefinition, error) {
	return r.fields.ListFields(ctx, tenantID, entityID)
}

func main() {
	// Initialize structured logging
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("failed to initialize logger: %v", err)
	}
	defer logger.Sync()

	logger.Info("Starting Nexus Platform server...")

	// Default gin to release mode in production (MED-3): debug mode dumps the full route table
	// and per-request debug logs to stdout. An explicit GIN_MODE (e.g. debug for local dev) is
	// honoured — gin already applies it at package init, so we only set the release default when
	// the operator has not chosen a mode. Set in main() only, never in test setup, so tests keep
	// gin's default (debug/test) mode.
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode)
	}

	// Load configuration
	cfg, err := config.LoadConfig()
	if err != nil {
		logger.Fatal("Failed to load config", zap.Error(err))
	}
	// TRUSTED_PROXY_CIDRS entries that failed to parse are dropped (degrade-to-default, never fail
	// boot); surface each one so a typo in the trusted-proxy list is visible instead of silently
	// leaving XFF untrusted from that block.
	for _, bad := range cfg.TrustedProxyCIDRsSkipped {
		logger.Warn("Ignoring invalid TRUSTED_PROXY_CIDRS entry — X-Forwarded-For will not be trusted from it",
			zap.String("cidr", bad))
	}

	// Initialize Database connection
	database, err := db.InitDB(cfg)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	logger.Info("Database connection established successfully")

	// sqlx connection for the dynamic-record path (obj_* tables). System metadata uses
	// the GORM connection above; dynamic CRUD uses sqlx + parameterized queries.
	sqlxDB, err := sqlx.Connect("mysql", cfg.DBDSN)
	if err != nil {
		logger.Fatal("Failed to open sqlx connection", zap.Error(err))
	}
	defer sqlxDB.Close()

	// Apply connection-pool limits to the sqlx pool (RM-20). The GORM pool is configured
	// inside db.InitDB; the sqlx pool is configured here because sqlx.Connect returns the
	// *sqlx.DB handle directly (no separate InitDB wrapper). Both pools share the same
	// env-driven budget so the combined open-conn ceiling stays well under MySQL's default
	// max_connections=151.
	sqlxDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	sqlxDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	sqlxDB.SetConnMaxLifetime(cfg.DBConnMaxLifetime)
	logger.Info("DB connection pools configured",
		zap.Int("maxOpenConns", cfg.DBMaxOpenConns),
		zap.Int("maxIdleConns", cfg.DBMaxIdleConns),
		zap.Duration("connMaxLifetime", cfg.DBConnMaxLifetime),
	)

	// Security-alert dispatcher (spec §6 / ../specs/014-security-alerts/spec.md §5).
	// LoadAlertsConfig fails fast when ALERTS_ENABLED=true but SMTP_HOST is missing,
	// matching the JWT_SECRET boot-guard pattern. When disabled, a NoopSender is wired
	// and the dispatcher is still built so WithNotifier injection is always safe.
	alertsCfg, err := config.LoadAlertsConfig()
	if err != nil {
		logger.Fatal("Failed to load alerts config", zap.Error(err))
	}

	var alertSender notify.EmailSender
	var alertRepo notify.AlertRepository
	if alertsCfg.Enabled {
		alertSender = notify.NewSMTPSender(notify.SMTPConfig{
			Host:     alertsCfg.SMTPHost,
			Port:     alertsCfg.SMTPPort,
			Username: alertsCfg.SMTPUser,
			Password: alertsCfg.SMTPPass,
			From:     alertsCfg.SMTPFrom,
		})
		// Audit rows are only written when alerts are actually enabled; a noop path with
		// repo=nil avoids writing pending→sent rows for sends that never happen.
		alertRepo = notify.NewSQLAlertRepository(sqlxDB)
	} else {
		alertSender = notify.NewNoopSender()
		// repo stays nil — dispatcher skips audit rows when alerts are disabled (spec §4/§5).
	}

	dispatcher := notify.NewAsyncDispatcher(notify.DispatcherConfig{
		Buffer:  alertsCfg.Buffer,
		Workers: alertsCfg.Workers,
	}, alertSender, alertRepo, logger)

	// RM-19 audit trail: a separate AsyncDispatcher writes one append-only audit_log row per
	// committed mutation (record CRUD + admin entity/field/role). It is fully fail-open — a full
	// buffer or a DB write error logs a warning and never blocks/slows/breaks the business flow
	// (the mutation has already committed). The writer is the sqlx-backed audit repository; the
	// buffer/workers reuse the alert dispatcher's sizing knobs (the dispatcher clamps to >=1).
	auditRepo := audit.NewSQLRepository(sqlxDB)
	auditDispatcher := audit.NewAsyncDispatcher(audit.DispatcherConfig{
		// The buffer must hold the largest burst the app is allowed to produce in one call.
		// It did not: the alert default is 256 while ONE bulk-update may carry MAX_BULK_OPS
		// (500) rows, each emitting its own event — so a mass edit could silently drop half its
		// before-images, the only thing that makes such an edit reconstructible. BOTH bulk doors
		// are passed: their caps live in different files behind different env vars, and flooring
		// on only the currently-larger one would re-open the gap the moment the other is raised.
		Buffer:  audit.BufferFor(alertsCfg.Buffer, services.MassOpMaxIDs(), services.BulkMaxRows()),
		Workers: alertsCfg.Workers,
	}, auditRepo, logger)

	// JOBS-1 durable job queue (../specs/021-durable-job-queue/spec.md — engine-depth primitive #0): a
	// sys_jobs row per unit of background work, claimed atomically (FOR UPDATE SKIP LOCKED) by a
	// small worker pool, retried with capped exponential backoff, and reaped back to 'queued'
	// when a worker crashes mid-run. This is the durable substrate the timer/deadline engine and
	// campaign send will stand on. First real producer: the workflow email_alert action enqueues
	// here (see the workflow wiring below) so an alert survives a crash and retries a down SMTP.
	jobsCfg := config.LoadJobsConfig()
	userRepo := auth.NewUserRepository(database)
	// specs/068 slice B: ONE freeze checker for every gate (HTTP writes, inbound webhooks, the
	// cross-tenant operator target, the job runner, the schedulers).
	tenantFreeze := services.NewTenantFreeze(userRepo, logger)
	// The checker already folds a read error into its answer (a known live freeze stays frozen), so the
	// gates below use only the verdict. Built before the queue and the schedulers so none of them
	// starts without it.
	frozenForSweep := func(ctx context.Context, tenantID string) bool {
		_, frozen, _ := tenantFreeze.FrozenUntil(ctx, tenantID)
		return frozen
	}
	jobsStore := jobs.NewSQLStore(sqlxDB, jobsCfg.MaxAttempts)
	// Queue #95: an email_alert payload carries the rendered recipient (possibly a sensitive
	// field) — drop it once the delivery is done or has given up, instead of keeping it for
	// JOBS_RETENTION_DAYS. Retries keep it.
	// A parked bpm_arm event (specs/043 S9a) carries the record's field values — same treatment.
	if err := jobsStore.ScrubPayloadOnFinish(services.EmailAlertJobType, bpm.ArmJobType); err != nil {
		logger.Fatal("Failed to configure job payload scrub", zap.Error(err))
	}
	jobsRegistry := jobs.NewRegistry()
	// email_alert's handler is registered below, next to outbound_webhook's: it writes a delivery
	// the queue gave up on back onto the workflow run (specs/059), so it needs workflowRepo.
	// NOTE: the manager is constructed + STARTED further down (after the workflow/calendar
	// wiring) — every handler must be registered before a worker can claim its job type, and the
	// TIMER-1 workflow_timer handler needs the workflow engine + dynrepo built below. A claim on
	// an unregistered type fails the job permanently (by design), so Start must come last.
	jobsManager := jobs.NewManager(jobs.ManagerConfig{
		Workers:      jobsCfg.Workers,
		PollInterval: jobsCfg.PollInterval,
		Lease:        jobsCfg.Lease,
		// outbound_webhook gets its own lane: one attempt against a receiver that swallows packets
		// holds a worker for the whole HTTP timeout, and in the shared FIFO pool a flood of those
		// would delay every tenant's email_alert and workflow_timer jobs.
		Dedicated: map[string]int{services.OutboundWebhookJobType: jobsCfg.WebhookWorkers},
	}, jobsStore, jobsRegistry, logger).
		// specs/068 slice B: a frozen tenant's job waits out the freeze (Postpone gives the attempt back).
		WithTenantGate(func(ctx context.Context, tenantID string) (time.Time, bool) {
			until, frozen, _ := tenantFreeze.FrozenUntil(ctx, tenantID)
			return until, frozen
		})

	// Wire the metadata engine: repositories -> services -> handlers.
	metaRepo := metadata.NewRepository(database)
	layoutRepo := metadata.NewLayoutRepository(database)
	dynRepo := dynrepo.New(sqlxDB)
	// O3: RecordService reads entity/field metadata THROUGH the in-process cache (it runs on every
	// dynamic-record request); the entity/field write services invalidate it after commit so cached
	// reads never go stale. Admin reads stay on the raw repo (cold path). Tier-1 in-proc; later a
	// Redis L2 swaps only NewMetaCache's backing (search-cache-longterm.md).
	metaCache := services.NewMetaCache(metaRepo, services.DefaultMetaTTL)
	entitySvc := services.NewEntityService(dynRepo, metaRepo).WithMetaCache(metaCache).WithAuditor(auditDispatcher)
	fieldSvc := services.NewFieldService(dynRepo, metaRepo).WithMetaCache(metaCache).WithAuditor(auditDispatcher)

	// E9 AI Builder (../specs/020-e9-ai-builder/spec.md): the conversational NL→metadata agent.
	// Model A — Nexus owns the loop + the tenant-safe tools; the OpenAI-compatible LLM gateway only relays
	// messages. Absent AI_* vars leave the agent inert (Enabled=false), and the route still mounts
	// to answer 503 AI_DISABLED (honest-disabled); only a bad AI_GATEWAY_MODE/AI_GATEWAY_CASSETTE
	// fails boot. The gateway base URL is a localhost-pinned CONFIG value (never a request) → no
	// SSRF. metaRepo satisfies the tenant-scoped MetadataReader the read tools use; the sqlx-backed
	// run repo writes the sys_ai_runs ledger (prompt HASHED, per-turn token/latency/outcome).
	aiCfg, err := config.LoadAIConfig()
	if err != nil {
		logger.Fatal("Failed to load AI config", zap.Error(err))
	}
	// The live gateway, or record/replay through AI_GATEWAY_CASSETTE (demos, CI, evals without a model).
	aiGateway, err := ai.NewCompleter(*aiCfg)
	if err != nil {
		logger.Fatal("Failed to set up the AI gateway", zap.Error(err))
	}
	aiRunRepo := ai.NewSQLRunRepository(sqlxDB)
	// aiAgent itself is constructed LATER (before the ⌘K assistant block): explain_access needs
	// AccessProbes over userRepo/roleRepo/aclSvc/fieldACLRepo/recordSvc, none of which exist yet.
	// M3 sessions (../specs/020-e9-ai-builder/spec.md §11): the sqlx-backed session store keeps the
	// RAW conversation (tenant's own data, scoped per tenant+user) so a builder can resume a chat.
	// This is a DIFFERENT store from the hash-only sys_ai_runs ledger above (two stores, two
	// purposes — resume vs audit/cost). The ledger stays hash-only and unchanged.
	aiSessionStore := ai.NewSessionStore(sqlxDB)
	// M4 (spec §12, MED-1 hard gate): the per-tenant budget/rate/concurrency guard around every
	// chat turn. It reads the daily token spend from the SAME sqlx run repo that writes the
	// ledger (budget READ fails closed — 500, unlike the fail-open ledger WRITE). Production
	// ALWAYS wires this guard; a handler without one gates nothing (test-only shape).
	aiBudgetGuard := ai.NewBudgetGuard(*aiCfg, aiRunRepo, logger)
	if aiCfg.Enabled {
		logger.Info("AI Builder enabled", zap.String("ai_model", aiCfg.Model), zap.String("ai_gateway", aiCfg.BaseURL),
			zap.String("ai_gateway_mode", aiCfg.GatewayMode))
	} else {
		logger.Info("AI Builder disabled (AI_ENABLED not \"true\") — route answers 503 AI_DISABLED")
	}

	// EPIC 4 Workflow/Automation engine: a separate AsyncDispatcher fires admin-registered
	// workflow rules AFTER a record mutation commits (RecordService Create/Update/SoftDelete).
	// It is async + FAIL-OPEN exactly like the audit dispatcher — a load/condition/action error
	// logs a warning and NEVER blocks/slows/breaks the already-committed mutation. The loader
	// reads active rules of the mutated entity through a TTL cache (workflowCache, invalidated by
	// the admin write service after a rule change); the field_update action patches the same
	// record under an admin scope; the in_app_notification action writes a sys_notifications row.
	workflowRepo := workflow.NewSQLRepository(sqlxDB)
	workflowCache := workflow.NewCachedLoader(workflowRepo, workflow.DefaultWorkflowTTL)
	// WF-01 execution-time guard: the mutator validates the field_update target against
	// sys_fields (through the TTL'd metadata cache) so a pre-existing bad rule can never
	// overwrite a computed/assigned field (auto_number corruption is permanent).
	// WithAuditor: the mutator bypasses RecordService, so without this every field_update / assign /
	// set_deadline write leaves no audit row (queue #86) — and every unit test still passes.
	workflowMutator := services.NewWorkflowRecordMutator(dynRepo).WithCatalog(metaCache).WithAuditor(auditDispatcher)
	// ASSIGN-1 (engine-depth primitive #4): the assign action's round-robin resolver. It advances
	// a per-rule cursor (sys_assignment_cursors, mig 000049) under a FOR UPDATE row lock so two
	// records firing the same rule concurrently get DIFFERENT team members, and writes the picked
	// userId through the SAME field_update mutator path (system scope). Shared with the scheduler
	// via the engine's deps() (a scheduled rule with an assign action rotates per matched record).
	assignmentSvc := services.NewAssignmentService(sqlxDB, logger)
	// EPIC 4 M2: load scheduler + outbound-webhook tuning (env-driven, fail-fast on bad values).
	workflowCfg, err := config.LoadWorkflowConfig()
	if err != nil {
		logger.Fatal("Failed to load workflow config", zap.Error(err))
	}
	// M2 email_alert: with the durable queue ON (JOBS_ENABLED, default), the action ENQUEUES an
	// email_alert job and a queue worker delivers it through the SAME provider-agnostic transport
	// the security-alert dispatcher uses (alertSender: SMTP when ALERTS_ENABLED, else noop) — so
	// an SMTP failure retries with backoff and survives a restart (JOBS-1 first real producer).
	// With the queue OFF the legacy inline sender is wired instead (no silent black hole: jobs
	// would otherwise be enqueued and never run).
	//
	// M2 outbound_webhook, same rule (DAILY-QUEUE #26): with the queue ON the action parks an
	// "outbound_webhook" job (tenant-pinned; rule id + action slot + frozen body, never the
	// secret) and a queue worker makes ONE signed POST per claim through the SAME HTTP sender
	// (Deliver) — the queue owns retry/backoff/dead-letter, so a receiver outage or a restart
	// mid-backoff no longer loses the event. With the queue OFF the webhook queue stays a nil
	// INTERFACE (never a typed-nil pointer) and the sender's in-process retry (WEBHOOK_MAX_RETRIES
	// × WEBHOOK_BACKOFF_MS) posts inline as before.
	// specs/066: both senders write the tenant's email log (sys_email_log), fail-open.
	emailLogRepo := emaillog.NewRepository(sqlxDB)
	var workflowEmailSender workflow.EmailSender = services.NewWorkflowEmailSender(alertSender).WithEmailLog(emailLogRepo, logger)
	var workflowWebhookQueue workflow.WebhookQueue
	if jobsCfg.Enabled {
		workflowEmailSender = services.NewEnqueuingWorkflowEmailSender(jobsStore).WithEmailLog(emailLogRepo, logger)
		workflowWebhookQueue = services.NewEnqueuingWebhookQueue(jobsStore)
	}
	// specs/050 S1a: named-credential vault master keyring — a DEDICATED key (not SSO/MFA's).
	// Present-but-malformed CREDENTIAL_ENC_* fails boot here, naming the variable. ABSENT boots
	// normally with the vault inert: credentialKeyring stays nil, which the vault service (S1b)
	// answers with 503 CREDENTIALS_NOT_CONFIGURED and the webhook resolver (S2b) with "send
	// nothing" — never an unauthenticated call.
	credentialCfg, err := config.LoadCredentialConfig()
	if err != nil {
		logger.Fatal("Failed to load named-credential config", zap.Error(err))
	}
	var credentialKeyring *crypto.Keyring
	if credentialCfg.Enabled {
		credentialKeyring, err = crypto.NewKeyring(credentialCfg.KeyID, credentialCfg.Key, credentialCfg.Retired)
		if err != nil {
			logger.Fatal("Failed to init named-credential keyring", zap.Error(err))
		}
		logger.Info("Named-credential vault enabled",
			zap.String("credential_key_id", credentialKeyring.ActiveID()),
			zap.Int("credential_retired_keys", len(credentialCfg.Retired)))
	} else {
		logger.Warn("Named-credential vault disabled (CREDENTIAL_ENC_KEY not configured) — partner keys cannot be stored and webhooks that use one send nothing")
	}
	if credentialCfg.AllowInsecureHTTP {
		logger.Warn("CREDENTIAL_ALLOW_INSECURE_HTTP=true — named credentials accept plain-http partner URLs; dev / internal network only, never in production")
	}
	// specs/050 S1b: the vault admin API (/admin/credentials on adminGated). A nil keyring is the
	// inert vault — every call answers 503 CREDENTIALS_NOT_CONFIGURED.
	// specs/050 S2b: built HERE, before the webhook job handler and the workflow engine, because
	// both resolve a {credentialId, path} webhook's key through it at send time (ResolveAuth) — and
	// NewScheduler below snapshots the engine's deps(), so a resolver attached later would leave
	// the scheduled path sending nothing.
	credentialSvc := services.NewCredentialService(credential.NewRepository(database), credentialKeyring, credentialCfg.AllowInsecureHTTP).
		WithAuditor(auditDispatcher).
		WithLogger(logger)

	workflowWebhookSender := workflow.NewHTTPWebhookSender(
		workflowCfg.WebhookTimeout, workflowCfg.WebhookMaxRetries, workflowCfg.WebhookBackoff)
	if err := jobsRegistry.Register(services.OutboundWebhookJobType,
		services.NewOutboundWebhookJobHandler(workflowRepo, credentialSvc, workflowWebhookSender)); err != nil {
		logger.Fatal("Failed to register outbound_webhook job handler", zap.Error(err))
	}
	// specs/059: a delivery the queue gives up on (budget spent, or no mail configured) turns its
	// workflow run failed, so the builder sees it in the rule's history (workflowRepo = the runs).
	if err := jobsRegistry.Register(services.EmailAlertJobType,
		services.NewEmailAlertJobHandler(alertSender, workflowRepo, emailLogRepo, logger)); err != nil {
		logger.Fatal("Failed to register email_alert job handler", zap.Error(err))
	}
	// W6: WithRunRecorder records every (event × workflow) run into sys_workflow_runs INSIDE the
	// async worker (zero sync latency; fail-open on insert error). It must be attached BEFORE
	// NewScheduler below — the scheduler snapshots the engine's recorder at construction.
	// CALENDAR-1 store, built here (not at its service below) because the workflow engine needs the
	// SAME lookup for a business_hours set_deadline offset, and it must be attached BEFORE
	// NewScheduler snapshots deps() — a resolver injected later would leave the scheduled path
	// computing against no calendar.
	calendarRepo := calmeta.NewSQLRepository(sqlxDB)
	workflowScheduleResolver := services.NewWorkflowScheduleResolver(calendarRepo, logger)
	// specs/059: an email_alert's {{relation.field}} / lookup placeholders — the patient of an
	// appointment, the customer of a ticket. ONE instance for the engine AND the dry-run below, so
	// "Chạy thử" previews exactly the address the send uses. Before NewScheduler (deps() snapshot).
	workflowTemplateResolver := services.NewWorkflowTemplateResolver(metaCache, dynRepo)
	workflowEngine := workflow.NewAsyncDispatcher(workflow.DispatcherConfig{
		Buffer:  alertsCfg.Buffer,
		Workers: alertsCfg.Workers,
	}, workflowCache, workflowMutator, workflowRepo, logger).
		WithEmailSender(workflowEmailSender).
		WithTemplateResolver(workflowTemplateResolver).
		WithWebhookSender(workflowWebhookSender).
		WithWebhookQueue(workflowWebhookQueue).
		WithCredentialResolver(credentialSvc).
		WithAssignmentResolver(assignmentSvc).
		WithRecipientResolver(services.NewWorkflowRecipientResolver(sqlxDB)).
		WithScheduleResolver(workflowScheduleResolver).
		WithRunRecorder(workflowRepo)
	// specs/048 S3: the before rules run INSIDE a save, so the run each one leaves (blocked, or
	// skipped for an exempt system write) is written by one background worker into the same
	// sys_workflow_runs — never a DB round trip on the save. Drained on shutdown below.
	beforeRunSink := workflow.NewRunSink(workflowRepo, logger)
	// The workflow admin CRUD service validates rules + invalidates the engine cache after a write
	// commit so a new/edited/removed rule takes effect promptly. It checks the target entity exists
	// via the metadata cache (reject a rule pointing at a non-existent entity — and, WF-01, a
	// field_update pointing at a computed/assigned or unknown field). WithRunReader serves the W6
	// run-history API; WithRecordReader backs the side-effect-free dry-run endpoint.
	// specs/063-draft-publish: the admin workflow service gets the DRAFT-AWARE repo. The engine's loader
	// (workflowCache above), the scheduler, the timer and webhook handlers keep the raw workflowRepo, so a
	// draft is never executed by construction (FR-016) and the customer's hot path gains no query.
	metaRegistry := services.NewMetaRegistry()
	draftRepo := metaversion.NewDraftRepo(sqlxDB, metaRegistry)
	versionRepo := metaversion.NewVersionRepo(sqlxDB)
	draftGate := services.NewDraftGate(versionRepo)
	// specs/063 S7-5: changes that apply at once on a delivered workspace still leave a "direct" version entry.
	directRec := services.NewVersionRecorder(sqlxDB, versionRepo, draftGate, logger)
	// specs/063 S7-8: a field's edits (and the required flag of a new one) are drafts on a delivered workspace.
	draftFieldRepo := services.NewDraftFieldRepo(metaRepo, draftRepo, draftGate)
	fieldSvc.WithDraftFields(draftFieldRepo)
	entitySvc.WithDraftFields(draftFieldRepo)
	draftWorkflowRepo := services.NewDraftWorkflowRepo(workflowRepo, draftRepo, draftGate).WithRecorder(directRec, services.NewWorkflowDocAdapter())
	publishSvc := services.NewPublishService(sqlxDB, draftRepo, versionRepo, metaRegistry, draftGate, auditDispatcher, logger).
		WithInvalidator(services.DocTypeWorkflow, workflowCache.Invalidate).
		// FieldDocAdapter.Entities returns entity IDS: drop the cached field list the record engine validates against.
		WithInvalidator(services.DocTypeField, metaCache.InvalidateFields).
		// EntityDocAdapter.Entities returns the table NAME: a rollback that un-deletes a table drops its cached definition.
		WithInvalidator(services.DocTypeEntity, metaCache.InvalidateEntityDef).
		WithSchemaOps(fieldSvc, entitySvc) // S7-9: a field / table delete runs through the existing services after the publish commits
	// WithLiveRepo gets the SAME raw workflowRepo the engine's loader reads (never draftWorkflowRepo):
	// "Tắt ngay cho khách" must switch the live rule off whatever the draft layer says.
	workflowSvc := services.NewWorkflowService(draftWorkflowRepo).WithLiveRepo(workflowRepo).WithRecorder(directRec, services.NewWorkflowDocAdapter()).WithEntityChecker(metaCache).WithCache(workflowCache).
		WithRunReader(workflowRepo).WithRecordReader(dynRepo).WithScheduleResolver(workflowScheduleResolver).
		WithTemplateResolver(workflowTemplateResolver).
		WithCredentialChecker(credentialSvc). // specs/050 S2b: a webhook's key must be a live key of the tenant
		// specs/062 L4: "workflow" is a draft type, so the gate suppresses the row for a draft-only save.
		WithAuditor(auditDispatcher, draftGate)

	// EPIC 5 BPM / Approval / SLA engine. The arming engine (bpm.AsyncEngine) is async + fail-open
	// exactly like the workflow dispatcher: after a record mutation commits, it loads the active
	// approval processes of the entity, matches the criteria, and opens an approval instance on the
	// first step (idempotent — a record already under approval is not double-armed). The Decider is
	// the SYNCHRONOUS approve/reject path. The SLA scheduler is a separate ticker that escalates
	// instances whose current step is past its SLA deadline (at-most-once per step via an atomic
	// CAS). Approval decisions land in the SAME append-only audit_log as record mutations.
	bpmRepo := bpm.NewSQLRepository(sqlxDB, logger)
	bpmAudit := services.NewApprovalAuditAdapter(auditDispatcher)
	// specs/043 S1 (#39): approval step deadlines walk the tenant's working calendar through the
	// SAME resolver the workflow timer and set_deadline use. All three places that open a step
	// (arm, decide, submit) get it; RequireDeadlines below refuses to boot if one is missed.
	bpmDeadlines, err := services.NewBPMDeadlineCalculator(workflowScheduleResolver, logger)
	if err != nil {
		logger.Fatal("Failed to build the approval deadline calculator", zap.Error(err))
	}
	// specs/043 S6a: a step whose holder is "the person in this field" / "the submitter" is resolved
	// when an instance enters it — on ALL three paths (arm, decide, submit); RequireRouting below
	// refuses to boot if one is missed.
	bpmRouter := services.NewBPMRouter(dynRepo, bpmRepo, logger).WithTeams(assignmentSvc)
	bpmEngine := bpm.NewAsyncEngine(bpm.EngineConfig{
		Buffer:  alertsCfg.Buffer,
		Workers: alertsCfg.Workers,
		// ONE chain cap for automation and approvals (specs/043 FR-042) — the same knob the
		// workflow dispatcher bounds create_record with (WithRecordCreator below).
		MaxChainDepth: workflowCfg.MaxChainDepth,
	}, bpmRepo, bpmRepo, bpmRepo, logger).WithDeadlines(bpmDeadlines).WithRouter(bpmRouter)
	bpmDecider := bpm.NewDecider(bpmRepo, bpmRepo, bpmRepo, bpmAudit, logger).WithDeadlines(bpmDeadlines).WithRouter(bpmRouter)
	workflowSvc.WithApprovalProcessReader(bpmRepo) // specs/043 S9b: start_approval names a live manual process
	// APPROVAL-OUTCOME: a terminal decision writes the process's configured field back onto the
	// record it decided about. Wired later (recordSvc is built below) — see WithRecordWriter.
	// specs/043 S2 (#40): "took N working hours" on GET /approvals/:id is measured on the SAME
	// calendar the deadlines above walk — one calculator, so the two can never disagree.
	approvalSvc := services.NewApprovalService(bpmRepo, bpmRepo, bpmDecider).WithEntityChecker(metaCache).
		WithWorkingTime(bpmDeadlines).
		// specs/043 S6a: a step's user / role / team assignee (and the fallback) must exist in the
		// tenant; a team seat is held by the members the directory names.
		WithHolderChecker(bpmRepo.HolderExists).
		WithTeamDirectory(bpmRepo).
		// specs/043 S12a: warn when a wait node's deadline field is also written by a set_deadline rule.
		WithDueWriters(workflowRepo)
	// SLA breach scheduler: reuses the workflow scheduler's cadence env (a single time-based knob).
	bpmSLAScheduler := bpm.NewSLAScheduler(
		bpm.SLASchedulerConfig{Interval: workflowCfg.SchedulerInterval}, bpmRepo, bpmRepo, bpmAudit, logger).
		// specs/043 S12a: a wait node has no seat — its missed deadline is told to sla.notify, which
		// may name a record field (assigned_to) or the submitter, resolved by the same router.
		WithRouter(bpmRouter).
		WithTenantGate(frozenForSweep) // specs/068 slice B: skip a write-frozen tenant's breaches
	// specs/043 S3c: each step with a deadline parks a durable bpm_sla_due job in the same
	// transaction as the write that set it, so the breach bell no longer depends on the ticker
	// below. Only with the queue ON (no job nobody runs); the ticker stays as the fallback.
	if jobsCfg.Enabled {
		bpmRepo.WithSLADueJobs(jobsStore)
		if err := jobsRegistry.Register(bpm.SLADueJobType, bpmSLAScheduler.NewSLADueJobHandler(bpmRepo)); err != nil {
			logger.Fatal("Failed to register bpm_sla_due job handler", zap.Error(err))
		}
	}
	if workflowCfg.SchedulerEnabled() {
		bpmSLAScheduler.Start()
		logger.Info("BPM SLA scheduler started", zap.Duration("interval", workflowCfg.SchedulerInterval))
	} else {
		logger.Info("BPM SLA scheduler disabled (WORKFLOW_SCHEDULER_INTERVAL_SEC=0)")
	}

	// Retention janitor (RUNS-JANITOR + TOKEN-JANITOR + JOBS-1): one background goroutine sweeps
	// seven cross-tenant SYSTEM tables that otherwise grow unbounded — sys_workflow_runs and
	// sys_hook_deliveries older than WORKFLOW_RUNS_RETENTION_DAYS; mfa_pending_tokens,
	// password_reset_tokens and sso_handoff_tokens whose expiry is past a grace day; done/failed
	// sys_jobs older than JOBS_RETENTION_DAYS; sys_idempotency_keys past their expires_at. The
	// authoritative list is janitor.Sweep. Each DELETE is a bounded LIMIT batch so a huge backlog
	// cannot lock a table; each sweep logs per-table counts (no PII). Retention is a GLOBAL policy
	// on system tables, so these deletes are intentionally not tenant-pinned. Disabled when
	// JANITOR_INTERVAL_SEC=0.
	janitorCfg, err := config.LoadJanitorConfig()
	if err != nil {
		logger.Fatal("Failed to load janitor config", zap.Error(err))
	}
	// specs/066 L4: per-day usage figures (sys_usage_daily). The roller below writes them; the janitor
	// sweeps them at USAGE_RETENTION_DAYS (2 years by default, longer than the sources it counts).
	usageCfg, err := config.LoadUsageConfig()
	if err != nil {
		logger.Fatal("Failed to load usage config", zap.Error(err))
	}
	usageRepo := usage.NewRepository(sqlxDB)
	// specs/066 L5: the instant block (active users, rows per table, attachment bytes); table names
	// come from the tenant's own metadata.
	usageGauges := &services.UsageGaugeSource{Store: usageRepo, Entities: metaRepo, Logger: logger}
	// specs/066 L6: operating health of one customer. The Doctor snapshot comes from the project
	// registry (counts only); the same service feeds health_json.ops at "Khám lại" (ProjectService.WithOps).
	healthSvc := services.NewHealthService(jobsStore, workflowRepo, emailLogRepo, inboundhook.New(database),
		services.NewProjectDoctorSnapshot(project.NewRepository(database)), jobsCfg.Enabled)
	retentionJanitor := janitor.New(janitor.Config{
		Interval:       janitorCfg.Interval,
		RunsRetention:  janitorCfg.RunsRetention,
		TokenGrace:     janitorCfg.TokenGrace,
		JobsRetention:  jobsCfg.Retention, // JOBS-1: done/failed sys_jobs older than JOBS_RETENTION_DAYS
		UsageRetention: usageCfg.Retention,
	}, janitor.NewSQLSweeper(sqlxDB), logger)
	if janitorCfg.Enabled() {
		retentionJanitor.Start()
		logger.Info("Retention janitor started",
			zap.Duration("interval", janitorCfg.Interval),
			zap.Duration("runs_retention", janitorCfg.RunsRetention))
	} else {
		logger.Info("Retention janitor disabled (JANITOR_INTERVAL_SEC=0)")
	}

	// WithFieldValueFetcher/WithRollupMeta/WithRollupLinked wire ENG-COMPUTED #2/#3 (read-time
	// lookup + ?rollup=): dynRepo backs both BatchFetchField (lookup) and ListLinked (N-N
	// rollup pivot id-set); metaRepo backs the cross-entity metadata reads (child entity +
	// fields, N-N relationship lookup) rollup resolution needs.
	// WithSequenceClaimer wires ENG #6 (auto_number): dynRepo backs ClaimSequence (sys_sequences,
	// migration 000029) — the Create-only seam that assigns a per-tenant, per-entity, per-field
	// sequence value.
	// WithBulkInserter wires ENG-BATCH #1 (bulk create): dynRepo backs InsertRecordsAtomic, the
	// one-transaction multi-row insert primitive CreateBulk's mode=atomic Phase 2 needs.
	recordSvc := services.NewRecordService(metaCache, dynRepo).
		WithLabelFetcher(dynRepo).
		WithFieldValueFetcher(dynRepo).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dynRepo).
		WithSequenceClaimer(dynRepo).
		WithBulkInserter(dynRepo).
		WithAuditor(auditDispatcher).
		WithEngine(workflowEngine).
		WithApprovalEngine(bpmEngine).
		// Handing someone a record changed whose list it is and told them nothing. Same inbox the
		// automation engine writes to, so an assignment notice and a rule's notice look alike.
		WithInbox(workflowRepo, logger).
		// Idempotency-Key on POST /:entity (specs/047): a resend after a lost connection replays
		// the first record instead of creating a second one. Claims live in sys_idempotency_keys.
		WithIdempotencyStore(idempotency.NewSQLRepository(sqlxDB)).
		// specs/048: before_save / before_delete rules veto a save or delete before it is written.
		// The SAME CachedLoader the dispatcher reads, which WorkflowService invalidates on every rule
		// edit — a rule switched off stops vetoing on the next save.
		WithBeforeHooks(workflowCache, beforeRunSink).
		// specs/054: on create, a required field the edit form hides by a visibleWhen condition
		// is not demanded. Read only when a required field is actually missing.
		WithLayoutReader(layoutRepo).
		// S7-8 on the data path (issue #2): a table born after the last publish is absent for the
		// customer's reads and writes too, not only from the metadata reads (same DraftGate decision).
		WithSchemaCutoff(draftGate)
	// specs/083: stored roll-up totals follow automation writes, and a new/"Tính lại" stored roll-up
	// is backfilled over the rows that already exist. Both closures need recordSvc, built after them.
	workflowMutator.WithAfterWrite(recordSvc.RefreshStoredRollups)
	// #330: a related-record field_update fires the target entity's after_update rules (depth-bounded).
	workflowMutator.WithEmitter(workflowEngine.Emit)
	fieldSvc.WithStoredRollupSweep(recordSvc.RecomputeStoredRollup)

	// specs/043 S7: the walk through gateway / auto / end nodes of an approval graph. Built HERE, now
	// that recordSvc exists and before the job queue starts, so a bpm_continue job a crash left
	// behind finds a runner on the first poll. The arming engine, the decider (and, further down, the
	// requester) all hand a machine node to it; the job is the safety net behind the synchronous walk.
	bpmRecords := services.NewApprovalOutcomeWriter(recordSvc)
	bpmRunner := bpm.NewRunner(bpmRepo, services.NewApprovalRecordReader(recordSvc), bpmRecords, bpmDeadlines, logger).
		WithRecordWriter(bpmRecords).
		WithInbox(bpmRepo).
		WithRouter(bpmRouter)
	bpmEngine.WithRunner(bpmRunner)
	bpmDecider.WithRunner(bpmRunner)
	if jobsCfg.Enabled {
		if err := jobsRegistry.Register(bpm.ContinueJobType, bpm.NewContinueJobHandler(bpmRepo, bpmRunner)); err != nil {
			logger.Fatal("Failed to register bpm_continue job handler", zap.Error(err))
		}
		// specs/043 S9a: a start event that met a full buffer is parked as a bpm_arm job, not dropped.
		bpmEngine.WithArmJobs(jobsStore)
		if err := jobsRegistry.Register(bpm.ArmJobType, bpm.NewArmJobHandler(bpmEngine)); err != nil {
			logger.Fatal("Failed to register bpm_arm job handler", zap.Error(err))
		}
	}

	// CREATE-RECORD: wire the create_record action's full-create seam over the now-built record
	// service (it wraps RecordService.Create), plus the max automation chain depth that bounds
	// recursion (WORKFLOW_MAX_CHAIN_DEPTH). This is done AFTER recordSvc is built — breaking the
	// construction cycle RecordService→WithEngine→creator→RecordService — and BEFORE NewScheduler
	// below, so the scheduler's deps() snapshot includes the creator (a scheduled create_record works
	// too). The nested Create carries depth+1 via ctx (WithChainDepth); emitCreateEvents stamps it on
	// the created record's after_create event so the guard terminates a self-referential chain.
	workflowEngine.WithRecordCreator(services.NewWorkflowRecordCreator(recordSvc), workflowCfg.MaxChainDepth)
	// specs/043 S9b: the start_approval action. Bound to the approval requester further down (it needs
	// recordSvc and the runner, built later) — but handed to the engine HERE so the scheduler's deps()
	// snapshot below carries it: a scheduled / date_offset rule can open a hồ sơ too.
	workflowApprovalStarter := services.NewWorkflowApprovalStarter()
	workflowEngine.WithApprovalStarter(workflowApprovalStarter)

	// EPIC 4 M2 background scheduler: a single goroutine ticks every WORKFLOW_SCHEDULER_INTERVAL_SEC
	// (disabled when 0), scans the due 'scheduled' workflows across all tenants, claims each run via
	// an atomic CAS (no double-fire), lists the rule's entity records, and runs the actions on every
	// record matching the condition. It shares the engine's action senders (email/webhook/mutator/
	// notification/creator) so there is exactly one set of adapters. Built AFTER WithRecordCreator so
	// its deps() snapshot carries the create_record seam. Started after wiring, stopped on signal.
	workflowLister := services.NewWorkflowRecordLister(dynRepo)
	workflowScheduler := workflow.NewScheduler(
		workflow.SchedulerConfig{Interval: workflowCfg.SchedulerInterval},
		workflowRepo, workflowLister, workflowEngine, logger).
		WithTenantGate(frozenForSweep) // specs/068 slice B: a frozen tenant's due rules stay due
	if workflowCfg.SchedulerEnabled() {
		workflowScheduler.Start()
		logger.Info("Workflow scheduler started", zap.Duration("interval", workflowCfg.SchedulerInterval))
	} else {
		logger.Info("Workflow scheduler disabled (WORKFLOW_SCHEDULER_INTERVAL_SEC=0)")
	}

	// specs/062 L3 + specs/063 S7-3: layout writes leave a __layout__ audit row, except a builder's
	// draft-only save (draftGate = the docWriteModer). LayoutService gets the DRAFT-AWARE repo; the
	// raw layoutRepo stays with pack export, Doctor and every other reader (live only).
	layoutSvc := services.NewLayoutService(metaRepo, services.NewDraftLayoutRepo(layoutRepo, draftRepo, draftGate), logger).WithAuditor(auditDispatcher, draftGate).
		WithSchemaCutoff(draftGate) // S7-8: the customer's default form omits fields born after the last publish

	// File-upload M1: the file subsystem catalogs uploaded blobs in sys_files (file repo) and
	// stores the bytes on a pluggable StorageBackend (local filesystem under FILE_STORAGE_DIR;
	// swappable for S3 later). FileService authorizes every upload/download/delete against the
	// parent record's tenant + A5 row scope via recordSvc, enforces the size cap + mime whitelist,
	// and keeps the record's file field [{id,name}] reference array in sync. A storage-init failure
	// is fatal — file uploads would otherwise silently 500.
	fileCfg := config.LoadFileConfig()
	fileBackend, err := storage.NewLocalBackend(fileCfg.StorageDir)
	if err != nil {
		logger.Fatal("Failed to init file storage backend", zap.Error(err))
	}
	// Deletes purged attachments' bytes after the catalog rows are gone — ONE closure for both
	// purges (tenant: WithBlobRemover below; single record from the recycle bin: specs/056). Per id
	// (Backend.Delete is idempotent) because the Backend interface has no prefix sweep; the key
	// convention comes from files.StorageKey, never re-spelled here.
	removeBlobs := func(ctx context.Context, tenantID string, ids []string) (int, error) {
		removed := 0
		var firstErr error
		for _, id := range ids {
			if err := fileBackend.Delete(ctx, files.StorageKey(tenantID, id)); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			removed++
		}
		return removed, firstErr
	}
	// specs/068 per-customer backups: the zips live on a SECOND LocalBackend (TENANT_BACKUP_DIR,
	// its own volume in prod) — never under FILE_STORAGE_DIR, so an attachment key can never name a
	// backup and the two stores can be sized, backed up and purged independently. The purge gets a
	// closure that removes a customer's backup files after its transaction commits (same shape as
	// removeBlobs above); tenantProvisionSvc is built here so the backup store can borrow its
	// schema-shape guard instead of carrying a second copy of that SQL.
	tbCfg := config.LoadTenantBackupConfig()
	tenantBackupBackend, err := storage.NewLocalBackend(tbCfg.Dir)
	if err != nil {
		logger.Fatal("Failed to init tenant backup storage backend", zap.Error(err))
	}
	tenantProvisionSvc := services.NewTenantProvisionService(database).WithLogger(logger).WithAuditor(auditDispatcher).
		// A purge that removes sys_files rows but leaves the bytes tells a customer their
		// documents are gone while they sit on disk — removeBlobs (shared with the record purge).
		WithBlobRemover(removeBlobs).
		// …and the same for the customer's own backup zips (FR-060): removed after the commit,
		// directory swept, never silent.
		WithBackupRemover(tenantbackup.NewRemover(tenantBackupBackend, tbCfg.Dir))
	tenantBackupRunner := tenantbackup.NewRunner(logger)
	tenantBackupStore := tenantbackup.NewGormStore(database, sqlxDB, tenantProvisionSvc.CheckSchemaShape)
	tenantBackupSvc := tenantbackup.NewService(
		tenantBackupStore,
		tenantbackup.NewSqlxSnapshots(sqlxDB),
		fileBackend, tenantBackupBackend, tenantBackupRunner, auditDispatcher,
		tenantbackup.Config{Dir: tbCfg.Dir, MinFreeBytes: tbCfg.MinFreeBytes}, logger).
		WithFreezer(tenantFreeze) // specs/068 slice B: POST/DELETE /admin/tenants/:id/maintenance
	tenantBackupSvc.WithPlan(tenantBackupStore, tbCfg.RestoreMaxRows) // read side of "Kiểm tra khôi phục"
	tenantBackupReaperCtx, stopTenantBackupReaper := context.WithCancel(context.Background())
	// specs/068 slice E: nightly + after-deploy backup of every customer, retention, manual delete.
	// The ticker also reaps abandoned running backups each tick; replicas never double up (uq_tbk_slot).
	tenantBackupSvc.WithSchedule(tenantBackupStore, tenantbackup.ScheduleConfig{
		HourLocal: tbCfg.HourLocal, KeepPreRestore: tbCfg.KeepPreRestore, FailedRetentionDays: tbCfg.FailedRetentionDays,
	})
	tenantBackupScheduler := tenantbackup.NewScheduler(tenantBackupSvc, tbCfg.ScheduleInterval, logger)
	if tbCfg.ScheduleEnabled {
		tenantBackupScheduler.Start()
	}

	// specs/056: the purge reads what depends on a record from the relationship registry (raw repo —
	// an operator action wants the current registry, not a cached one).
	recordSvc.WithBlobRemover(removeBlobs).WithRelationshipLister(metaRepo)
	fileSvc := files.NewService(
		files.NewRepository(sqlxDB),
		fileBackend,
		recordSvc,
		files.Limits{MaxBytes: fileCfg.MaxUploadBytes, MimeOK: fileCfg.MimeAllowed},
		nil, // default UUID id generator
	)
	logger.Info("File storage initialized",
		zap.String("dir", fileCfg.StorageDir),
		zap.Int64("max_upload_bytes", fileCfg.MaxUploadBytes),
		zap.Int("mime_whitelist", len(fileCfg.MimeWhitelist)))

	// #54 (specs/036 §2.5): digest attachments that have no sha256 yet — uploaded before migration
	// 000068, or by an older image after it (deploy.sh rolls code back without migrating down) — so
	// restore-drill.sh can compare them byte-for-byte. Once per start, in the background: idempotent
	// (NULL rows only), and a failure leaves those rows on the drill's size-only path, never blocks
	// serving. Unreadable = blobs already gone from storage; the drill names them MISSING too.
	go func() {
		st, err := fileSvc.BackfillHashes(context.Background())
		if err != nil {
			logger.Error("sys_files sha256 backfill failed", zap.Error(err), zap.Int("hashed", st.Hashed))
			return
		}
		if len(st.Unreadable) > 0 {
			logger.Warn("sys_files sha256 backfill: blobs unreadable, rows left without a digest",
				zap.Int("hashed", st.Hashed), zap.Strings("storage_keys", st.Unreadable))
		} else if st.Hashed > 0 {
			logger.Info("sys_files sha256 backfill done", zap.Int("hashed", st.Hashed))
		}
	}()

	// specs/050 S7: master-key rotation. With retired keys configured, re-seal every vault row still
	// sealed under one of them, tenant by tenant, in the background (idempotent; a row that cannot
	// be opened is left for Doctor C12). Never blocks serving.
	if credentialKeyring != nil && len(credentialCfg.Retired) > 0 {
		go func() {
			n, err := services.ResealCredentials(context.Background(), credential.NewRepository(database), credentialKeyring, logger)
			if err != nil {
				logger.Error("named-credential reseal failed", zap.Error(err), zap.Int("resealed", n))
				return
			}
			logger.Info("named-credential reseal done", zap.Int("resealed", n))
		}()
	}

	// EPIC 6 M2 saved reports: the report admin CRUD service validates a saved-report definition
	// (tabular/summary/matrix shape) + checks the target entity exists, and the RUN endpoint
	// replays the saved config through the SAME aggregate/list engine on recordSvc — so a report
	// reflects live, tenant + A5 row-scoped records and never stores materialized data.
	reportRepo := report.NewSQLRepository(sqlxDB)
	reportSvc := services.NewReportService(services.NewDraftReportRepo(reportRepo, draftRepo, draftGate).WithRecorder(directRec, services.NewReportDocAdapter()), recordSvc).WithEntityChecker(metaCache).
		WithAuditor(auditDispatcher, draftGate) // specs/062 L4: "report" is a draft type — a draft-only save leaves no row

	// EPIC 6 Dashboard M2: the dashboard admin CRUD service validates a saved-dashboard definition
	// (widget type/metric/entity shape) and the RUN endpoint executes each widget through the SAME
	// aggregate engine on recordSvc — so a dashboard reflects live, tenant + A5 row-scoped records
	// and never stores materialized data. Per-widget errors are captured (200 overall).
	dashboardRepo := dashboard.NewSQLRepository(sqlxDB)
	draftDashboardRepo := services.NewDraftDashboardRepo(dashboardRepo, draftRepo, draftGate).WithRecorder(directRec, services.NewDashboardDocAdapter()) // also the app menu validator (S7-4): sees draft-only dashboards
	dashboardSvc := services.NewDashboardService(draftDashboardRepo, recordSvc, logger).
		WithAuditor(auditDispatcher, draftGate) // specs/062 L4: "dashboard" is a draft type

	// W9b saved views: per-user saved list-view presets (filters/sort/columns/density) against a
	// dynamic entity. The service validates the definition + enforces the shared/personal ownership
	// model; metaCache backs the target-entity existence check (same checker the report service uses).
	viewRepo := view.NewSQLRepository(sqlxDB)
	viewSvc := services.NewViewService(viewRepo, metaCache, logger)

	// N-N relation service: uses the metadata repo for relationship definitions and the
	// dynRepo for join-table DDL/DML. Logger injected via functional option.
	// WithRelationACLAuthorizer is injected below (after aclSvc is constructed).
	relationSvc := services.NewRelationService(dynRepo, metaRepo, services.WithRelationLogger(logger))

	// W2 relationship lifecycle (impact / patch / delete by sys_relationships id): reuses
	// fieldSvc.DeleteField for the 1-N/1-1 path (same flow as DELETE /admin/fields/:id) and
	// the metadata repo's tombstoned soft-delete for N-N rows.
	relationLifecycleSvc := services.NewRelationLifecycleService(
		metaRepo, dynRepo, fieldSvc, services.WithRelationLifecycleLogger(logger))

	// CSV import M1 service: maps + validates rows against entity metadata and batch-inserts via
	// dynRepo (partial-success, tenant-scoped). Logger for structured per-batch diagnostics.
	// WithSequenceClaimer wires IMPORT-AUTONUM: dynRepo backs ClaimSequence (sys_sequences), so a
	// CSV-imported record gets its auto_number assigned through the SAME atomic claim the Create
	// write-path uses (ENG #6) instead of a permanently-blank value.
	// WithBeforeHooks (specs/048 S2): every row is judged by the entity's before_save rules through
	// the SAME workflowCache RecordService reads — an import is not a way around a data rule.
	// WithIdempotencyStore: Idempotency-Key on POST /:entity/import (specs/047 FR-12) — a resend of
	// the same file replays the first summary instead of inserting the rows again.
	importSvc := services.NewImportService(metaRepo, dynRepo, logger).WithSequenceClaimer(dynRepo).
		WithSchemaCutoff(draftGate). // issue #2: a table born after the last publish is absent for the customer's import too
		WithBeforeHooks(workflowCache, beforeRunSink).
		WithIdempotencyStore(idempotency.NewSQLRepository(sqlxDB)).
		WithAfterInsert(recordSvc.RefreshStoredRollups) // specs/083: stored roll-up totals follow an import
	// specs/048 S7a: with the queue ON, every imported row gets its "Tạo" history line through the
	// record_import_events job (registered below). Queue OFF = no history job (nobody would run it).
	if jobsCfg.Enabled {
		importSvc = importSvc.WithEventJobs(jobsStore)
	}

	// Wire the A1 auth foundation: JWT config (fail-fast on missing/placeholder secret,
	// spec §5) -> token service -> user repository -> login service -> handler.
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		logger.Fatal("Failed to load JWT config", zap.Error(err))
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		logger.Fatal("Failed to init token service", zap.Error(err))
	}
	refreshRepo := auth.NewRefreshTokenRepository(database)

	// ENG-AUTH2 #1: forgot/reset-password config (RESET_URL_BASE + RESET_MAIL_FROM/SMTP_FROM).
	// Fail-fast mirrors LoadAlertsConfig: required only when ALERTS_ENABLED=true (the flag that
	// makes outbound email real — see config.LoadResetConfig doc), so dev/test boot with alerts
	// disabled is unaffected.
	resetCfg, err := config.LoadResetConfig(alertsCfg.Enabled)
	if err != nil {
		logger.Fatal("Failed to load reset-password config", zap.Error(err))
	}
	resetRepo := auth.NewPasswordResetTokenRepository(database)

	// A7: inject the alert dispatcher into AuthService so E5 (refresh-token reuse detection)
	// can enqueue a refresh_reuse alert. WithLogger ensures recovered panics are visible.
	// WithResetRepo/WithResetMailer wire ENG-AUTH2 #1: the reset-token repo plus the SAME
	// provider-agnostic email transport the security-alert dispatcher uses (alertSender: SMTP
	// when ALERTS_ENABLED, else noop) — reset mail is sent synchronously (not through the async
	// dispatcher) because it is the one sanctioned token-bearing render path (notify.reset_mail.go).
	// specs/055 FR-008: ONE ended-session guard shared by the auth service (EndSession / logout-all
	// invalidate it, /auth/me reads it) and the live Auth stage (handlers.WithSessionGuard below),
	// so an ended session's access token is refused on its very next request. 30 s TTL = backstop
	// for another replica's end, like the user-row and ACL caches.
	sessionGuard := services.NewCachedSessionGuard(refreshRepo, services.DefaultSessionGuardTTL)
	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc).
		WithSessionGuard(sessionGuard).
		WithNotifier(dispatcher).
		WithAuditor(auditDispatcher).
		WithLogger(logger).
		WithResetRepo(resetRepo).
		WithResetMailer(alertSender, resetCfg.From, resetCfg.URLBase).
		// specs/066 L3: the reset mail and the admin-issued link are lines of the tenant's email log,
		// written inside the detached send goroutine (never on the request).
		WithEmailLog(emailLogRepo).
		// Persistent lockout (prod-ops slice b): AUTH_LOCKOUT_MAX_FAILURES wrong passwords
		// → account locked for AUTH_LOCKOUT_DURATION_MIN (generic 401, anti-enumeration).
		WithLockoutPolicy(cfg.AuthLockoutMaxFailures, cfg.AuthLockoutDuration)

	// ENG-AUTH2 #7: MFA/TOTP. LoadMFAConfig fails fast ONLY when MFA_SECRET_ENC_KEY (or
	// MFA_FAIL_OPEN) is PRESENT but malformed — see config.LoadMFAConfig doc. An ABSENT key
	// boots normally with FULL MFA support left inert: WithMFA is skipped below, so every MFA
	// endpoint (enroll/activate/disable/verify) answers 503 MFA_UNAVAILABLE
	// (services.ErrMFAUnavailable).
	//
	// MFA-ENFORCE: the ENROLLMENT READER is wired UNCONDITIONALLY (it needs only the DB — the
	// user_mfa.enabled check never touches the sealed secret), so Login can still see "this
	// account is enrolled" when the key is gone and FAIL CLOSED (503 MFA_UNAVAILABLE) instead
	// of silently downgrading the account to one-step login. MFA_FAIL_OPEN=true is the
	// INFRA-EMERGENCY override (one-step login allowed, zap.Error on every such login).
	mfaCfg, err := config.LoadMFAConfig()
	if err != nil {
		logger.Fatal("Failed to load MFA config", zap.Error(err))
	}
	mfaStore := auth.NewUserMFARepository(database)
	authSvc = authSvc.WithMFAEnrollmentReader(mfaStore, mfaCfg.FailOpen)
	// MFA-ENFORCE MED-1: the fail-closed guarantee rests entirely on the reader above being
	// wired. A future refactor that drops this line would silently reopen the fail-open hole
	// (every fixture wires its own reader, so no test catches it). Assert it at boot.
	if !authSvc.MFAGateWired() {
		logger.Fatal("MFA enrollment gate not wired — MFA-enrolled accounts would silently downgrade to one-step login if MFA_SECRET_ENC_KEY is lost")
	}
	if mfaCfg.Enabled {
		mfaSealer, err := crypto.New(mfaCfg.KeyID, mfaCfg.Key)
		if err != nil {
			logger.Fatal("Failed to init MFA secret-at-rest sealer", zap.Error(err))
		}
		authSvc = authSvc.WithMFA(
			mfaStore,
			auth.NewMFABackupCodeRepository(database),
			auth.NewMFAPendingTokenRepository(database),
			mfaSealer,
		)
		logger.Info("MFA/TOTP support enabled", zap.String("mfa_key_id", mfaCfg.KeyID))
	} else {
		logger.Info("MFA/TOTP support disabled (MFA_SECRET_ENC_KEY not configured) — enrolled accounts FAIL CLOSED at login (MFA-ENFORCE)",
			zap.Bool("mfa_fail_open", mfaCfg.FailOpen))
		if mfaCfg.FailOpen {
			logger.Error("MFA_FAIL_OPEN=true — MFA-enrolled accounts will log in WITHOUT their second factor; restore MFA_SECRET_ENC_KEY and unset this flag as soon as the incident is over")
		}
	}

	// ENG-AUTH2 #8: SSO / OIDC. LoadSSOConfig fails fast ONLY when SSO_SECRET_ENC_KEY is PRESENT
	// but malformed, OR present with SSO_REDIRECT_BASE left empty (a half-configured deployment
	// that could never build a valid redirect_uri) — see config.LoadSSOConfig doc. An ABSENT key
	// boots normally with SSO support left entirely inert: WithSealer is skipped below, so every
	// SSO endpoint (start/callback/admin CRUD) answers 503 SSO_NOT_CONFIGURED
	// (services.ErrSSOUnavailable). ssoSvc is always constructed (never nil) — the handler is
	// unconditionally wired and each use case internally gates on the sealer, mirroring how MFA
	// gates on mfaRepo != nil rather than leaving the handler itself nil.
	ssoCfg, err := config.LoadSSOConfig()
	if err != nil {
		logger.Fatal("Failed to load SSO config", zap.Error(err))
	}
	ssoSvc := services.NewSSOService(authSvc, auth.NewSSOProviderRepository(database), auth.NewSSOIdentityRepository(database)).
		WithAuditor(auditDispatcher). // SSO-ADMIN-AUDIT: provider create/policy-flip/delete → RM-19 trail
		// SSO-FE-REDIRECT: one-time handoff codes bridging redirect-mode /callback → the
		// frontend's POST /auth/sso/exchange. Wired unconditionally (needs only the DB) —
		// the service gates on the sealer anyway while SSO is disabled.
		WithHandoffStore(auth.NewSSOHandoffTokenRepository(database))
	if ssoCfg.Enabled {
		ssoSealer, err := crypto.New(ssoCfg.KeyID, ssoCfg.Key)
		if err != nil {
			logger.Fatal("Failed to init SSO secret-at-rest sealer", zap.Error(err))
		}
		ssoSvc = ssoSvc.WithSealer(ssoSealer, ssoCfg.RedirectBase)
		ssoBootFields := []zap.Field{zap.String("sso_key_id", ssoCfg.KeyID), zap.String("sso_redirect_base", ssoCfg.RedirectBase)}
		if ssoCfg.FrontendBase != "" {
			// SSO-FE-REDIRECT: /callback runs in redirect mode (302 → frontend) instead of JSON.
			ssoBootFields = append(ssoBootFields, zap.String("sso_frontend_base", ssoCfg.FrontendBase))
		}
		logger.Info("SSO/OIDC support enabled", ssoBootFields...)
	} else {
		logger.Info("SSO/OIDC support disabled (SSO_SECRET_ENC_KEY not configured)")
	}

	// A4 RBAC: the role repository backs both /admin/roles CRUD (role service) and the per-action
	// ACL decision on the dynamic /:entity surface (ACL service → RequireACL middleware). It also
	// powers the user service's cross-tenant role guard (validates an assigned role_id is in-tenant).
	roleRepo := auth.NewRoleRepository(database)

	// W1 (claim-freshness, ../specs/001-auth-security/spec.md §5): the user service revokes a user's refresh
	// tokens when an admin changes that user's role_id / is_admin or deletes the account, so the
	// holder can no longer mint a fresh access token carrying the stale (over-)privilege.
	// 🔒 R3 cross-tenant role guard: WithRoleValidator(roleRepo) makes CreateUser / UpdateUser
	// reject a role_id that belongs to another tenant (422 ROLE_INVALID), so an admin can never
	// attach a foreign tenant's role to a user.
	// A7: inject the alert dispatcher into UserService so E1–E4 events are emitted.
	// RM-19: WithAuditor(auditDispatcher) — every committed user create/update/delete writes a
	// __user__ audit row (actor from auditContext; password changes appear only as "[redacted]").
	// #14 claim-freshness (../specs/001-auth-security/spec.md §5): the Auth stage rebuilds every
	// protected request's Subject from the caller's LIVE user row read through this cache
	// (WithLiveSubject below), and the user service drops the row on every committed user
	// create/update/delete — so a demotion / role change / disable / delete lands on the
	// user's very next request; the 30 s TTL is only the backstop for a missed invalidation.
	userClaims := services.NewCachedUserLoader(userRepo, services.DefaultUserClaimTTL)
	// specs/065 lát D: an owner-tier switch drops the live-row cache and ends the sessions of the
	// accounts it converts, exactly like a user edit does.
	tenantProvisionSvc.WithClaimCache(userClaims).WithTokenRevoker(refreshRepo)
	userSvc := services.NewUserService(userRepo).
		WithTokenRevoker(refreshRepo).
		WithSessionEnder(authSvc). // #310: an admin-set password ends every session at once
		WithClaimCache(userClaims).
		WithRoleValidator(roleRepo).
		WithRoleLister(roleRepo). // specs/065: the owner's role picker
		WithNotifier(dispatcher).
		WithLogger(logger).
		WithAuditor(auditDispatcher)

	// O3: ACLService reads role grants THROUGH the in-process cache (it runs on every non-admin
	// request); RoleService invalidates a role's entry after a write commit so a changed/revoked
	// grant takes effect at once. ACL TTL is short (30s) — the fail-safe backstop for a security
	// decision, not a perf knob (search-cache-longterm.md).
	aclCache := services.NewCachedACLLoader(roleRepo, services.DefaultACLTTL)
	roleSvc := services.NewRoleService(roleRepo).WithRecorder(directRec, services.NewRoleFootprint(sqlxDB)).WithACLCache(aclCache).WithAuditor(auditDispatcher).
		WithUserFieldChecker(services.CatalogUserFields{Catalog: metaRepo}) // #283 "field:<name>" grants
	aclSvc := services.NewACLService(aclCache)
	// Issue #2: a table delete drops every role's grants on it and a rollback that brings the table back puts
	// them back; both refresh the cache at once, not after the backstop.
	entitySvc.WithACLCache(aclCache)
	publishSvc.WithACLCache(aclCache)

	// FILE-ACL: arm per-attachment authorization now that the ACL service exists. Download and
	// delete live at /files/:id, a path with no :entity segment, so the route-level ACL stage
	// cannot run on them — the file service resolves the parent entity's grants itself, after it
	// has read the row. Without this every non-admin got 404 on every attachment while UPLOAD
	// worked, because upload rides the record group and had its level resolved.
	fileSvc.WithACL(aclSvc)

	// VIEC-HOM-NAY: role-based work queues (../specs/012-work-queues/spec.md). Validation shares the record
	// engine's own filter/sort validators; end-user counts run under the caller's ACL scope.
	workQueueRepo := workqueue.New(database)
	// specs/043 S5: a queue with the "claim" verb counts the pool seats waiting to be claimed.
	workQueueSvc := services.NewWorkQueueService(workQueueRepo, recordSvc, aclSvc, roleRepo, logger).WithSeatCounter(bpmRepo)

	// APIKEY-1 machine identity: sys_api_keys repo + service. roleRepo doubles as the in-tenant
	// role guard on issue (same guard the user service uses). The service also powers the
	// composite Bearer verifier below — a presented `nxk_` credential mints
	// Claims{tenant, role, IsAdmin:false} and rides the SAME middleware chain as a JWT.
	apiKeyRepo := auth.NewAPIKeyRepository(database)
	apiKeySvc := services.NewAPIKeyService(apiKeyRepo, roleRepo).
		WithAuditor(auditDispatcher).
		WithLogger(logger).
		// #356: a client IP that keeps presenting `nxk_` strings that read the table and fail is
		// refused without the read (per process, like the other limiters).
		WithProbeBrake(cfg.APIKeyProbeMaxPerMin)
	// specs/064 S9-1 preview-as-role: a builder opens a read-only, short-lived session as one role.
	// PREVIEW_TTL_MINUTES is range-checked at boot (1..60, default 30 — fail fast, it bounds a token
	// carrying another role). The service re-checks every request, uncached, against its
	// sys_preview_sessions row and the builder's live login session (WorkspaceSessionRepository.
	// ActorLive — the method specs/062 L1a shares); the composite verifier below is its only door.
	previewCfg, err := config.LoadPreviewConfig()
	if err != nil {
		logger.Fatal("Failed to load preview config", zap.Error(err))
	}
	workspaceSessionRepo := auth.NewWorkspaceSessionRepository(database)
	previewSvc := services.NewPreviewService(auth.NewPreviewSessionRepository(database),
		workspaceSessionRepo, roleRepo, tokenSvc, previewCfg.TTL).
		WithDrafts(draftGate, draftRepo). // S9-9: the draft flag needs "delivered" + "has drafts"
		WithAuditor(auditDispatcher).
		WithLogger(logger).
		// S9-8: a preview opened inside a customer's workspace also dies with the builder's workspace
		// session (GetLive, uncached) — the same repository specs/062 resolves its own tokens with.
		WithWorkspaceSessions(workspaceSessionRepo)
	// specs/064 S9-2: GET /auth/me answers a preview token with the synthetic role profile.
	authSvc.WithPreview(previewSvc)
	// specs/062-customer-workspace: a builder works directly inside a customer's workspace with a
	// short-lived session token. The service is the resolver the Auth stage calls for EVERY request
	// carrying act/att/wsid (uncached: GetLive + ActorLive), wired below as WithWorkspaceResolver.
	workspaceSessionSvc := services.NewWorkspaceSessionService(workspaceSessionRepo, userRepo, tokenSvc).
		WithAuditor(auditDispatcher).
		WithLogger(logger)
	// GET /auth/me answers a workspace-session token through the same resolver (before the home
	// session guard, which would ask the customer tenant about the builder's login).
	authSvc = authSvc.WithWorkspaceResolver(workspaceSessionSvc)
	// W5-A2: gate ?expand by the caller's ACL on each relation's TARGET entity, so a label
	// (e.g. a related person's name) cannot be harvested across an entity the caller may not
	// view. aclSvc is built after recordSvc, so we inject it here. Reuses the O3-cached ACL
	// loader → the per-target view-check is cheap (no extra DB round-trip on the hot path).
	recordSvc.WithACLAuthorizer(aclSvc)
	// NN-01 fix: gate ListLinked by the TARGET entity's ACL, not the source entity's scope.
	// Without this, a caller with view:all on the source but view:owner on the target would
	// see every linked target record — records they'd get 403/404 for on a direct GET.
	// aclSvc must be constructed before this call (see above).
	relationSvc.WithRelationACLAuthorizer(aclSvc)

	// RECORD-NOTES (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục 1 "Trao đổi trên hồ sơ"): an
	// ENGINE capability for every entity — the notes service sits on TWO already-wired deciders:
	// recordSvc (ResolveNoteTarget re-authorizes the parent record's VIEW grant regardless of the
	// route's own action mapping — record_note.go) and aclSvc (the delete rule's "note author OR
	// entity delete-grant" override). Both must be constructed first (see above).
	noteSvc := notes.NewService(notes.NewRepository(sqlxDB), recordSvc, aclSvc)
	// Manual per-record READ shares (specs/051-record-manual-share): recordSvc is the gate
	// (ResolveShareTarget re-resolves the EDIT grant), dynRepo the grantee probe (active user of the
	// same tenant), auditDispatcher the share/unshare trail on the record's History tab.
	shareSvc := shares.NewService(shares.NewRepository(sqlxDB), recordSvc, dynRepo, auditDispatcher)

	// App container (../specs/010-app-container/spec.md Phase 1): sys_apps + sys_app_nav_items — the
	// sidebar App Switcher's lens layer over tenant-global entities/dashboards. The service
	// validates nav refs against metaRepo/dashboardRepo (tenant-pinned) and filters the nav
	// read through aclSvc.CanViewEntity — the SAME decider the ⌘K assistant uses (INV-3).
	appRepo := appmeta.NewSQLRepository(sqlxDB)
	// TRỤ 2 mục 4 item 5: readiness reuses the already-wired role/user/entity readers — no new
	// repositories, just a 3-way structural fan-in (RolesGrantingView / ListUsers / ListFields).
	appSvc := services.NewAppService(services.NewDraftAppStore(appRepo, draftRepo, draftGate), metaRepo, draftDashboardRepo, aclSvc, logger).
		WithReadiness(roleRepo, userRepo, metaRepo).WithRecorder(directRec).
		// APP-DELETE-ORPHAN-ROLES (WAVE 6): deleting an app takes its own auto-seeded roles with
		// it (kept when users still hold them — see cleanupSeededRoles).
		WithRoleCleanup(roleRepo).
		WithHomeRefs(workQueueRepo, roleRepo)
	// TRỤ 2 mục 4 items 1-4: seeds an app's two default roles (idempotent) + grants a freshly
	// created, app-scoped entity into them — see docs/NEXUS-MOT-XUONG-2026-07-29.md and
	// internal/services/app_acl_seed.go. Shares the SAME role repo + O3 ACL cache as roleSvc/
	// aclSvc so a seeded grant is authoritative and cache-fresh immediately.
	appACLSeedSvc := services.NewAppACLSeedService(appRepo, roleRepo).WithRecorder(directRec).WithACLCache(aclCache).WithLogger(logger)
	// TRỤ 2 mục 4 gap-closer (POST /admin/apps/:id/acl-backfill): appSvc.ACLBackfill composes
	// over the SAME single-entity seeder CreateEntity uses, for every entity already in an
	// app's nav. Auditor mirrors every other admin-mutation service below.
	// "app" is a draft type (S7-4): a draft-only update/nav save leaves no audit row; create/delete apply at once and always do.
	appSvc.WithACLSeeder(appACLSeedSvc).WithAuditor(auditDispatcher, draftGate)

	// Business-hours calendars (CALENDAR-1, engine-depth primitive #1a): per-tenant
	// working-hours + holidays + timezone rows (sys_business_calendars, mig 000047) that the
	// TIMER-1 date_offset trigger consumes through internal/calendar (see below). A tenant with
	// no calendar → wall-clock fallback (documented contract in docs/api-conventions.md §17).
	calendarSvc := services.NewBusinessCalendarService(calendarRepo, logger)
	// TIMER-1: the workflow service rejects a date_offset rule pinning an unknown calendarId.
	workflowSvc.WithCalendarChecker(calendarRepo)

	// TIMER-1 (engine-depth primitive #1b — date_offset workflow trigger on JOBS-1 + CALENDAR-1):
	// register the wake-up consumer, attach the producer seam to the dispatcher, then START the
	// queue manager (constructed above; Start deliberately deferred until every job type is
	// registered — a claim on an unregistered type fails the job permanently).
	if err := jobsRegistry.Register(services.WorkflowTimerJobType,
		// jobsStore is passed so a RECURRING date_offset rule can park its next occurrence at fire
		// time (ACTIVITY-CORE #3b). Without it a recurrence would fire once and log that it could
		// not repeat — never pretend it did.
		services.NewWorkflowTimerJobHandler(workflowRepo, dynRepo, workflowEngine, jobsStore, logger)); err != nil {
		logger.Fatal("Failed to register workflow_timer job handler", zap.Error(err))
	}
	// specs/048 S7a: the CSV import's history job writes audit synchronously (auditRepo, not the
	// async dispatcher — a full buffer would drop lines of a big file).
	if err := jobsRegistry.Register(services.ImportEventsJobType,
		// S7b: runAutomations imports feed the record's after_create to the SAME workflow dispatcher
		// and BPM engine a hand-made create uses, through their blocking EmitWait.
		services.NewImportEventsJobHandler(metaRepo, dynRepo, auditRepo, logger,
			services.ImportAutomation{Workflow: workflowEngine, BPM: bpmEngine})); err != nil {
		logger.Fatal("Failed to register record_import_events job handler", zap.Error(err))
	}
	if jobsCfg.Enabled {
		// Producer only with the queue ON: otherwise date_offset rules would park wake-ups
		// nobody runs (the same no-black-hole rule as the email_alert sender fallback).
		workflowEngine.WithTimerScheduler(services.NewWorkflowTimerScheduler(jobsStore, calendarRepo, dynRepo))
		jobsManager.Start()
		logger.Info("Durable job queue started",
			zap.Int("workers", jobsCfg.Workers),
			zap.Int("webhook_workers", jobsCfg.WebhookWorkers),
			zap.Duration("poll_interval", jobsCfg.PollInterval),
			zap.Duration("lease", jobsCfg.Lease))
	} else {
		logger.Info("Durable job queue disabled (JOBS_ENABLED=false) — email_alert and outbound_webhook send inline; date_offset workflows schedule nothing")
	}

	// Nexus Doctor (../specs/008-nexus-doctor/spec.md): drift scanner + targeted fixer. DOCTOR-CORE =
	// four soft-metadata checks (B1/B2/C1/C2); DOCTOR-SCHEMA = five information_schema checks
	// (A1/A2a/A5/A6/B4) whose DDL fixes execute through dynRepo (the SAME ddl.Build* executors
	// the entity/field services use — one statement body, never a mirror); DOCTOR-OPS = C4
	// zombie-workflow disable + report-only A3/B5/C8 + the flush-caches action. Detect queries
	// ride the sqlx store (read-only, tenant-pinned, batched IN-list information_schema probes);
	// soft-metadata fixes reuse the existing repair helpers (metaRepo relationship soft-delete,
	// layoutRepo persist/hard-delete, appmeta nav purge). Metadata reads use the RAW metaRepo — a
	// doctor must see DB truth, never the cache — while the MetaCache is attached only for the
	// post-fix blanket invalidation. WithOpsCaches wires the ACL + workflow caches (and
	// WithUserClaimCache the #14 user-row cache) so flush-caches clears all four in-proc caches
	// and the C4 fix invalidates the disabled rule's entry. Every fix writes an RM-19 audit row
	// via auditDispatcher.
	doctorStore := doctor.NewSQLStore(sqlxDB)
	doctorSvc := services.NewDoctorService(doctorStore, dynRepo, metaRepo, metaRepo, layoutRepo, auditDispatcher, logger).
		WithMetaCache(metaCache).
		WithOpsCaches(aclCache, workflowCache).
		// #14 claim-freshness: the live user-row cache is the fourth in-proc cache flush-caches clears.
		WithUserClaimCache(userClaims).
		// specs/043 S3a2 D3: pending approvals the inbox still reads through the projection fallback.
		WithApprovalSeats(bpmRepo).
		// specs/043 S9a D4: records with two PENDING approvals — the pairs Migration B refuses.
		WithApprovalDuplicates(bpmRepo).
		// queue #143 C9: field-ACL rows that leave a role unable to create (the create path no
		// longer names a hidden required field to that role, so the admin reads it here).
		WithFieldRestrictions(auth.NewFieldACLRepository(database)).
		// specs/050 S3b C10: inline webhook keys of rules saved before the vault move into it
		// (confirm-gated, one-way). Registered even without CREDENTIAL_ENC_KEY: the scan needs no
		// master key, the fix refuses per rule with CREDENTIALS_NOT_CONFIGURED. The queue probe keeps a
		// rule whose deliveries are still parked in sys_jobs from moving under them.
		WithInlineSecretMigration(credentialSvc, workflowRepo, services.NewEnqueuingWebhookQueue(jobsStore)).
		// specs/050 S7 C11/C12 (report-only): rows still on a retired master key / rows no held key opens.
		// Registered even with the vault inert — a DB restored where the key is absent must say so.
		WithCredentialKeyChecks(credentialSvc)

	// specs/068 slice D: the restore engine needs the Doctor (repair + cache flush) and the write
	// freeze, so it is wired here, after both exist — and the reaper starts only now, so it never
	// runs without the restore half: a backup that was running when the previous process died has no
	// heartbeat; fail it and remove its partial file now and then every 10 minutes (FR-021), and
	// finish a restore that died after COMMIT (FR-071).
	tenantBackupSvc.WithRestore(tenantbackup.RestoreDeps{
		Store: tenantBackupStore, Freezer: tenantFreeze, Doctor: services.NewTenantRestoreDoctor(doctorSvc),
		TxTimeout: tbCfg.RestoreTxTimeout,
	})
	go tenantBackupSvc.RunReaper(tenantBackupReaperCtx, 10*time.Minute)

	// DOCTOR-OPS scheduled scan: every DOCTOR_SCAN_INTERVAL_SEC (default 0 = OFF, opt-in) the
	// scheduler runs a READ-ONLY doctor scan per active tenant and notifies each tenant's
	// admins in-app when findings exist (title links to /admin/doctor). It NEVER auto-fixes
	// (spec §3: "tự khám định kỳ, người chỉ duyệt" — enforced by its Scan-only dependency).
	// The notification write reuses the workflow repo's InsertNotification (one insert body).
	doctorCfg := config.LoadDoctorConfig()
	doctorScheduler := services.NewDoctorScheduler(
		services.DoctorSchedulerConfig{Interval: doctorCfg.ScanInterval},
		doctorSvc, doctorStore, workflowRepo, logger).
		WithTenantGate(frozenForSweep) // specs/068 slice B: no scan of a tenant mid-restore
	if doctorCfg.Enabled() {
		doctorScheduler.Start()
		logger.Info("Doctor scheduled scan started", zap.Duration("interval", doctorCfg.ScanInterval))
	} else {
		logger.Info("Doctor scheduled scan disabled (DOCTOR_SCAN_INTERVAL_SEC=0)")
	}

	// specs/066 L4: roll each tenant's per-day usage into sys_usage_daily (same tenant universe as the
	// Doctor sweep). USAGE_ROLLUP_INTERVAL_SEC=0 turns it off; today is still counted live on GET.
	usageRoller := services.NewUsageRoller(usageCfg.RollupInterval, usageRepo, doctorStore, logger).WithGauges(usageGauges)
	if usageCfg.Enabled() {
		usageRoller.Start()
		logger.Info("Usage roller started", zap.Duration("interval", usageCfg.RollupInterval))
	} else {
		logger.Info("Usage roller disabled (USAGE_ROLLUP_INTERVAL_SEC=0)")
	}

	// RM-42 / A6 field-level security: a per-field view/edit overlay (sys_field_acl) on top of the
	// A4 module ACL and A5 row scope. The record service strips fields the caller's role may not
	// view from every read (get/list/aggregate/export) and rejects a write that touches a field
	// the role may not edit (403 FIELD_FORBIDDEN); is_admin bypasses. Default-allow: a field with
	// no sys_field_acl row is fully visible+editable, so existing entities work with no backfill.
	fieldACLRepo := auth.NewFieldACLRepository(database)
	// Every enforcer instance shares the cached catalog so a formula over a hidden field is hidden
	// with it (queue #316) — one constructor, so no surface can be wired without the derivation.
	newFieldACL := func() *services.FieldACLService {
		return services.NewFieldACLService(fieldACLRepo).WithCatalog(metaCache)
	}
	recordSvc.WithFieldACL(newFieldACL())
	// queue #142: PUT /admin/fields/:id refuses to make a field required-without-fill while a role
	// has it read-only or hidden (the reverse door to the PutGrants guard). Same repo, same rows.
	fieldSvc.WithFieldRestrictions(fieldACLRepo)
	// queue #259 / specs/054 FR-6: the same door for a required flip on a field the edit layout
	// hides behind a visibleWhen condition (else the next layout save 422s, far from its cause).
	fieldSvc.WithLayoutReader(layoutRepo)

	// APPROVAL-REQUEST: arm the human verbs (gửi duyệt / thu hồi / uỷ quyền). Wired HERE rather
	// than beside the decider because Submit needs recordSvc — it resolves the record under the
	// caller's row scope both to authorize the request and to get the values the process criteria
	// are evaluated against, and recordSvc is only fully configured (field-ACL included) by now.
	bpmRequester := bpm.NewRequester(bpmRepo, bpmRepo, bpmRepo, bpmRepo, logger).WithDeadlines(bpmDeadlines).WithRouter(bpmRouter).WithRunner(bpmRunner)
	// D-8: unwired on any one path, a "4 working hours" step would quietly be timed on the wall
	// clock there — refuse to start instead.
	if err := bpm.RequireDeadlines(bpmEngine, bpmDecider, bpmRequester); err != nil {
		logger.Fatal("Approval deadlines not wired", zap.Error(err))
	}
	if err := bpm.RequireRouting(bpmEngine, bpmDecider, bpmRequester); err != nil {
		logger.Fatal("Approval routing not wired", zap.Error(err))
	}
	workflowApprovalStarter.Use(bpmRequester)
	approvalSvc.WithRequestFlow(bpmRequester, recordSvc).
		// specs/043 S7: "Thử lại" on a hồ sơ stalled on an automatic step.
		WithRetrier(bpmRunner).
		// specs/043 S5: queue seats on the approval routes obey the same entity-view rule as /work-items.
		WithQueueACL(aclSvc).
		// specs/043 S5: a graph node's work-queue assignee must be a live, ACTIVE queue of the tenant
		// (else its seat could never be claimed).
		WithQueueChecker(func(ctx context.Context, tenantID, queueID string) (bool, error) {
			q, err := workQueueRepo.GetByID(ctx, tenantID, queueID)
			if err != nil {
				if errors.Is(err, workqueue.ErrNotFound) {
					return false, nil
				}
				return false, err
			}
			return q.IsActive, nil
		}).
		// Delegate target must be a real user in this tenant — else the step ghost-parks forever.
		WithUserChecker(func(ctx context.Context, tenantID, userID string) (bool, error) {
			if _, err := userRepo.GetUserByID(ctx, tenantID, userID); err != nil {
				if errors.Is(err, auth.ErrUserNotFound) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		})
	// The decision's write-back. Without it "approved" is a terminal state nothing reads: the
	// record keeps whatever status it had and no rule or report can react.
	bpmDecider.WithRecordWriter(services.NewApprovalOutcomeWriter(recordSvc))
	// A rejection that returns the record to its submitter (specs/043 S8) needs a live person to hand it to.
	bpmDecider.WithSubmitterResolver(services.NewApprovalSubmitterResolver(bpmRepo, recordSvc))

	// E9 builder agent — constructed HERE, not beside aiCfg above, because explain_access needs
	// AccessProbes over services that only exist by now (security audit 2026-09-01, specs/020 §Nợ:
	// ReaderWithAccessProbes had ZERO production call-sites, so the tool always answered
	// ACCESS_EXPLAIN_UNAVAILABLE — the "đoạn nối không ai viết" bug-class). The probes are the SAME
	// tenant-scoped services the rest of the platform enforces with; NewAIBuilderAgent now warns at
	// startup if a future refactor hands it a probe-less reader again.
	// One variable feeds BOTH the agent's tools and the context resolver (specs/067 plan §3/§9) so a
	// later wrapper (draft overlay) changes a single spot and the two can never read different sources.
	// specs/063 S7-8 (T123a): the AI reads fields through draftFieldRepo so it sees the builder's draft labels;
	// ReaderWithAccessProbes wraps THIS reader (inside), never the other way round, or explain_access loses its probes.
	var aiMeta ai.MetadataReader = aiDraftFieldReader{Repository: metaRepo, fields: draftFieldRepo}
	aiAgent := ai.NewAIBuilderAgent(*aiCfg, aiGateway, ai.ReaderWithAccessProbes(aiMeta, &ai.AccessProbes{
		Users:    userRepo,
		Roles:    roleRepo,
		ACL:      aclSvc,
		FieldACL: fieldACLRepo,
		Records:  recordSvc,
		// queue #319: propose_edit_field judges FIELD_REQUIRED_WHILE_LOCKED at propose time.
		FieldLocks: fieldACLRepo,
		// issue #2: propose_access grants only on a table the customer does not have yet.
		Published: draftGate,
		// issue #42: propose_field refuses a relation/user field whose <name>_id column is taken, as AddField does.
		Columns: dynRepo,
		// issue #42: propose_records resolves a "@ref" of an applied parent card the way Apply does.
		Refs: recordSvc,
		// issue #30 item 4: the delete card names the packs that ship the table and counts its records.
		// The ledger repo is stateless; the installer's packInstallRepo below reads the same sys_pack_items.
		Packs:  packinstall.New(database),
		Counts: dynRepo,
	}), &ai.DocReaders{Layouts: layoutSvc, Workflows: workflowSvc, Reports: reportSvc, Apps: appSvc}, aiRunRepo, logger).
		WithDelivery(draftGate) // specs/067 L10-5: one grounding line once the project is delivered
	aiContextResolver := ai.NewContextResolver(aiMeta, appSvc, userRepo)
	// Issue #3: MCP tokens let an outside agent call the builder agent's read and propose tools at /mcp,
	// as the builder who issued them; proposals land in that builder's thread (aiSessionStore).
	mcpTokenSvc := services.NewMCPTokenService(auth.NewMCPTokenRepository(database), aiSessionStore, userRepo).
		WithAuditor(auditDispatcher).WithLogger(logger)
	// A credential-level event (logout-all, a password change or reset, the reuse alarm) ends the user's
	// agent keys with their sessions: a key minted from a stolen session must not outlive them.
	authSvc.WithAgentKeys(mcpTokenSvc)

	// ⌘K end-user data assistant (read-only ask-data): reuses the E9 plumbing (gateway, ledger,
	// session store) with its OWN tool registry — query/aggregate/list only, ZERO propose_*. The
	// adapter routes every read through recordSvc (tenant + A5 row scope + field-ACL) and gates
	// entity visibility per caller via aclSvc — never the raw metadata repo (trap #2/#4). It is
	// wired AFTER recordSvc's ACL/field-ACL injection so the assistant reads through the fully
	// armed record engine.
	// A3 semantic-layer tools: reportSvc lets ⌘K list + run SAVED reports (reviewed metric
	// definitions) so the assistant's numbers match the Reports screen instead of re-deriving
	// grain/filters ad hoc. RunReport does its own AuthorizeView (fail-closed).
	assistantReader := ai.NewRecordReadAdapter(recordSvc, aclSvc, metaRepo, reportSvc)
	assistantAgent := ai.NewAssistantAgent(*aiCfg, aiGateway, assistantReader, aiRunRepo, logger)
	// A SEPARATE guard INSTANCE from the builder's: assistant turns must not eat the builder's
	// in-memory per-tenant rate/concurrency window; the daily token budget is naturally shared
	// anyway through the one sys_ai_runs ledger (TokensSince sums all features per tenant).
	// ponytail: guard keys per TENANT (M4) — the assistant is every-user, so one user can burn
	// the tenant's window/budget; a per-USER rate limit is deferred backlog before wide rollout.
	assistantGuard := ai.NewBudgetGuard(*aiCfg, aiRunRepo, logger)
	// W7 governance: the ADMIN config surface over the same sys_field_acl rows the enforcement
	// above reads. Role existence is validated tenant-scoped via roleRepo; entity/field names
	// via the MetaCache catalog (cold admin path). The enforcement loader is cache-less
	// (per-request DB read), so a save here is live on the next record request — no
	// invalidation step exists or is needed.
	fieldACLAdminSvc := services.NewFieldACLAdminService(fieldACLRepo, roleRepo, metaCache).
		WithAuditor(auditDispatcher).WithRecorder(directRec)
	// specs/063 S7-7b: "Hoàn tác" of an applied-at-once role / field-permission / app entry goes back through these services.
	roleSvc.WithRecreator(roleRepo)
	publishSvc.WithReverters(roleSvc, fieldACLAdminSvc, appSvc)
	// A6 parity: CSV import must enforce the same field-level write ACL as POST create.
	// A fresh FieldACLService instance is fine — both share the same underlying repo.
	importSvc.WithFieldACL(newFieldACL())
	// B3 fix: wire relation-FK validator so CSV import rejects dangling FKs the same way
	// RecordService.Create does via validateRelations. dynRepo already implements BatchRecordExists.
	importSvc.WithRelationChecker(dynRepo)
	// specs/061 FR-015: a relation cell may name its target by value only if the caller may VIEW
	// the target entity — the same gate ?expand uses. The value lookup itself rides dynRepo above.
	importSvc.WithACLAuthorizer(aclSvc)
	// FU-1 fix: strip A6-hidden fields from ?expand results in ListLinked, mirroring the
	// FilterReadFields call in RecordService.Get. Uses a fresh FieldACLService — same repo.
	relationSvc.WithRelationFieldACL(newFieldACL())

	// Vertical seed-pack service: export the current tenant's metadata
	// (entities/fields/relationships/layouts/roles) as a reusable JSON pack, and install a pack
	// (uploaded or a named built-in: CRM/Clinic) into the current tenant. Install reuses the
	// SAME entity/field/layout/role services as the hand-built admin endpoints (entitySvc /
	// fieldSvc / layoutSvc / roleSvc) so every install goes through their validation, DDL,
	// tenant-isolation and audit path — there is no weaker second write path. Export reads
	// through the raw repos (cold admin path).
	// RelationWriter / RecordWriter / Linker enable Pass 2 (N-N join tables), Pass 3 (seed
	// records), and Pass 4 (N-N link rows) of Install. dynRepo satisfies TargetChecker and
	// RecordFetcher for the Link source-gate — the same repo used by RelationService itself.
	// W8-BE-1: the install ledger (sys_pack_installs) records one row per successful install for
	// the Studio "installed versions" view; RelationshipReader lets Export emit N-N from the
	// registry and RecordLister backs the optional ?includeData=true sample export.
	packInstallRepo := packinstall.New(database)
	// A5-extra team repository. Constructed here (before verticalSvc) so PACK-TEAMS can seed a team
	// + membership through it; it also backs /admin/teams CRUD (teamSvc) and the ASSIGN-1 checker.
	teamRepo := auth.NewTeamRepository(database)
	shareSvc.WithTeams(teamRepo) // DAILY-QUEUE #224: a record can be shared with a whole team
	// Hoisted above verticalSvc so the pack installer can seed a pack's inbound hooks through the
	// SAME validated Create path the admin API uses (PACK-HOOKS) — one write path, one validator.
	// specs/062 L4: WithAuditor records __inbound_hook__ rows (secret never in the diff); WithUserReader
	// is D-22 — a hook whose creator is not a user of the hook's tenant runs as inboundhook.SystemActorID.
	inboundHookSvc := services.NewInboundHookService(inboundhook.New(database), recordSvc, userRepo, logger).
		WithAuditor(auditDispatcher).WithUserReader(userRepo).WithTenantFreeze(tenantFreeze)

	verticalSvc := vertical_pack.NewService(vertical_pack.Deps{
		EntityReader:             metaRepo,
		LayoutReader:             layoutRepo,
		RoleReader:               roleRepo,
		EntityWriter:             entitySvc,
		EntityExists:             metaRepo,
		FieldWriter:              fieldSvc,
		LayoutWriter:             layoutSvc,
		RoleWriter:               roleSvc,
		RelationWriter:           relationSvc,
		RelationViewConfigWriter: relationLifecycleSvc, // queue #720
		CalendarStore:            calendarSvc,          // queue #720: calendars travel with the pack
		ViewStore:                viewSvc,              // queue #720: shared saved views travel with the pack
		RecordWriter:             recordSvc,
		Linker:                   relationSvc,
		RecordExists:             dynRepo,
		RecordFetcher:            dynRepo,
		DashboardWriter:          dashboardSvc,
		WorkflowWriter:           workflowSvc,    // PACK-WORKFLOWS: seed pack automation rules (SLA timers, routing)
		CredentialVault:          credentialSvc,  // specs/050 S6: vault keys travel in a pack by name, no secret
		ReportWriter:             reportSvc,      // PACK-REPORTS: seed pack saved reports through the validated CreateReport path
		WorkQueueWriter:          workQueueSvc,   // PACK-QUEUES: seed "Việc Hôm Nay" queues through the validated Create path
		ApprovalWriter:           approvalSvc,    // PACK-APPROVALS: seed approval processes through the validated CreateProcess path
		InboundHookWriter:        inboundHookSvc, // PACK-HOOKS: seed receiving endpoints; the secret is MINTED per tenant, never carried in the pack
		// PACK-FIELD-ACL: field-level security must SURVIVE the tenant boundary. Both directions go
		// through the same FieldACLAdminService the admin screen uses, so a pack can never express a
		// restriction the UI could not — and handover stops being the one path that drops it.
		FieldACLReader:     fieldACLAdminSvc,
		FieldACLWriter:     fieldACLAdminSvc,
		TeamWriter:         teamRepo, // PACK-TEAMS: seed a team + membership so a pack can ship round-robin routing
		RelationshipReader: metaRepo,
		RecordLister:       dynRepo,
		InstallStore:       packInstallRepo,
		ObjProber:          dynRepo, // W8-BE-2: read-only unique-slot conflict probe for dry-run
		// DAILY-QUEUE #82: warn in the install result when a pack's business_hours rules land in a
		// tenant with no usable default calendar — asked through the SAME resolver the engine uses.
		ScheduleResolver: workflowScheduleResolver,
		Logger:           logger,
	})
	// Apps Phase 4 (../specs/010-app-container/spec.md §0.2): a successful pack install mints ONE sys_app
	// (sidebar container) named by the manifest slug, nav = pack entities + dashboards.
	// Optional dep, strictly fail-open — a mint failure never fails the committed install.
	verticalSvc.WithAppService(appSvc)
	// KHUON-SONG-P1: per-item pack provenance (sys_pack_items) — the install writes each item's
	// as-shipped content (fail-open) and the diff endpoint reads it back. Same repo as the ledger.
	verticalSvc.WithItemStore(packInstallRepo)
	// specs/063 FR-009: no pack install into a delivered workspace (changes go through drafts only).
	verticalSvc.WithDeliveryGate(draftGate)
	// specs/069 L7: a delivered project's upgrade plan marks items that already have a draft.
	verticalSvc.WithDraftReader(draftRepo)

	// A5-extra: team management. teamRepo (constructed above for PACK-TEAMS) backs /admin/teams CRUD
	// + member add/remove (team service). Teams + memberships feed the A5 row-level filter (a
	// record's team_set_id resolves to its creator's teams; access_level=team viewers see records on
	// a shared team).
	teamSvc := services.NewTeamService(teamRepo).WithAuditor(auditDispatcher)
	// ASSIGN-1: the workflow service rejects an assign action naming an unknown/cross-tenant team.
	// Wired here (after teamRepo exists) via the same in-place pointer mutation as the calendar
	// checker above — visible to the already-registered workflow handlers.
	workflowSvc.WithTeamChecker(teamRepo)

	// P0 FE-binding: notification read service. The sqlx-backed repository reads sys_notifications
	// (written by the workflow/bpm engines). All reads are pinned to (tenant_id, user_id).
	notificationRepo := notification.New(sqlxDB)
	notificationSvc := services.NewNotificationService(notificationRepo, logger)

	// Set up router (OBS-1, RM-35/37/38): gin.New() instead of gin.Default() — the default
	// stdout Logger/Recovery are replaced by zap-structured equivalents so every line lands in
	// the same pipeline the rest of the app logs to. Onion order (outermost first):
	//
	//   RequestID → AccessLog → Metrics → Recovery → CORS → … → handlers
	//
	//   - RequestID FIRST: everything downstream (access log, metrics, recovery, handler error
	//     logs) reads the correlation id from context.
	//   - AccessLog and Metrics sit OUTSIDE Recovery: a panic is converted to a 500 by the inner
	//     Recovery, so both still record the request with status=500 (and the in-flight gauge
	//     can never leak on a panic).
	//   - Recovery wraps everything after it (CORS included) — any panic below is logged via
	//     zap with the request id and answered with a clean 500 JSON envelope.
	r := gin.New()
	// Trusted proxies (MED-3): gin trusts ALL proxies by default, so c.ClientIP() (used by the
	// access log) would read an attacker-spoofable X-Forwarded-For. Restrict XFF trust to the
	// configured reverse-proxy networks; with none configured, pass nil so gin trusts NO proxy and
	// c.ClientIP() == RemoteAddr (safe default). The strings come from the already-validated CIDRs
	// (each is a *net.IPNet.String()), so SetTrustedProxies cannot realistically fail — but a
	// parse error here would silently reopen the spoof hole, so treat it as fatal.
	var trustedProxyStrs []string
	for _, n := range cfg.TrustedProxyCIDRs {
		trustedProxyStrs = append(trustedProxyStrs, n.String())
	}
	if err := r.SetTrustedProxies(trustedProxyStrs); err != nil {
		logger.Fatal("Failed to set gin trusted proxies", zap.Error(err))
	}
	r.Use(middleware.RequestID())
	r.Use(middleware.AccessLog(logger))

	// Prometheus instrumentation (RM-38): HTTP histogram/in-flight gauge plus DBStats gauges
	// for BOTH pools (RM-20 sized them; this makes both visible), distinguished by db_name.
	appMetrics := metrics.New()
	if gormSQLDB, dbErr := database.DB(); dbErr != nil {
		// Same hygiene posture as db.InitDB: DB() never errors on a successfully-opened driver.
		logger.Fatal("Failed to get GORM sql.DB for metrics", zap.Error(dbErr))
	} else {
		appMetrics.RegisterDBPool(gormSQLDB, "gorm_sys")
	}
	appMetrics.RegisterDBPool(sqlxDB.DB, "sqlx_obj")
	r.Use(appMetrics.Middleware())

	r.Use(middleware.Recovery(logger))

	// CORS must be registered at the engine level BEFORE any route group so that preflight
	// OPTIONS requests (which have no registered handler) are still processed by the
	// middleware rather than hitting gin's NoRoute 404 path (RM-04). AllowCredentials=true
	// requires specific origins — never "*" — enforced by BuildCORSConfig.
	r.Use(cors.New(config.BuildCORSConfig(cfg.CORSAllowedOrigins)))

	// Inject shared dependencies (DB, logger) into the request context so that
	// downstream dynamic metadata/CRUD handlers can resolve them per request.
	r.Use(func(c *gin.Context) {
		c.Set("db", database)
		c.Set("logger", logger)
		c.Next()
	})

	// Health probes (W5-E2). /health/live is a static 200 (process up). /health/ready pings the
	// DB under cfg.DBQueryTimeout via the sqlx pool → 200 ready / 503 DB_NOT_READY when MySQL is
	// slow/down, so an orchestrator/LB takes a wedged node out of rotation instead of stalling on
	// it. The legacy /health alias is preserved as a liveness 200 for existing probes/dashboards.
	healthH := health.NewHandler(sqlxDB, cfg.DBQueryTimeout, logger)
	healthH.RegisterRoutes(r)

	// Prometheus scrape endpoint (RM-38): engine root, OUTSIDE auth (a scraper has no JWT) but
	// minimally gated by METRICS_BEARER_TOKEN — empty (default) answers 404 (fail-closed OFF),
	// set requires `Authorization: Bearer <token>`. See docs/api-conventions.md §2.3.
	r.GET("/metrics", appMetrics.Handler(cfg.MetricsBearerToken))
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"status":  "healthy",
			"version": "1.0.0",
		})
	})
	// MCP (issue #3): agents propose, humans dispose. Outside /api/v1: its own bearer (nxm_ tokens
	// only, refused everywhere else) and the SDK's stateless streamable HTTP transport. Issue #22: the
	// API's brakes in front of it — an IP's lookups of tokens not yet seen working get the API-key
	// brake's budget, each working token the data plane's per-key budget.
	mcpLimiter := middleware.MCPRateLimit(middleware.MCPRateLimitConfig{
		ProbeMax:          cfg.APIKeyProbeMaxPerMin,
		KeyMax:            cfg.DataRateLimitPerMin,
		Window:            time.Minute,
		TrustedProxyCIDRs: cfg.TrustedProxyCIDRs,
	})
	r.Any("/mcp", mcpLimiter, gin.WrapH(mcpserver.New(mcpTokenSvc, aiAgent, aiSessionStore, tenantFreeze, logger)))

	// API v1 routes. RegisterAPIRoutes is the single wiring point shared with the integration
	// tests: it mounts the PUBLIC /auth/* endpoints, then a PROTECTED sub-group fronted by
	// AuthMiddleware → RequireTenant (../specs/001-auth-security/spec.md §4 order: Auth → Tenant → ACL →
	// Business Logic) carrying the metadata/admin/dynamic-record handlers. Every protected route
	// derives its tenant strictly from the verified JWT claim; tokenSvc is the access-token verifier.
	//
	// Hardening options (RM-16, RM-15):
	//   - WithBodySizeLimit: caps every request body at cfg.MaxRequestBodyBytes before the JSON
	//     decoder runs, preventing OOM from oversized payloads (→ 413 REQUEST_TOO_LARGE).
	//   - WithAuthRateLimiter: throttles the public /auth/* group to cfg.AuthRateLimitMaxAttempts
	//     per cfg.AuthRateLimitWindow, keyed by source IP (→ 429 RATE_LIMITED with Retry-After).
	//   - WithSSORateLimiter (SSO-RATELIMIT-NAT): the public SSO endpoints (/auth/sso/:tenant/start,
	//     /auth/sso/callback) get a DEDICATED, larger bucket (cfg.SSORateLimitMaxAttempts per
	//     cfg.SSORateLimitWindow — default 60/60 s) keyed per (ip,tenant,provider) on /start and per
	//     ip on /callback, so an enterprise behind NAT doing an SSO login burst never exhausts the
	//     strict /auth credential bucket (which would lock the company out of password login too).
	authLimiter := middleware.AuthRateLimit(middleware.RateLimitConfig{
		MaxAttempts:       cfg.AuthRateLimitMaxAttempts,
		IPMaxAttempts:     cfg.AuthRateLimitIPMaxAttempts,
		Window:            cfg.AuthRateLimitWindow,
		TrustedProxyCIDRs: cfg.TrustedProxyCIDRs,
	})
	ssoLimiter := middleware.SSORateLimit(middleware.SSORateLimitConfig{
		MaxAttempts:       cfg.SSORateLimitMaxAttempts,
		Window:            cfg.SSORateLimitWindow,
		TrustedProxyCIDRs: cfg.TrustedProxyCIDRs,
	})
	// Per-user BURST cap on the ⌘K assistant, IN FRONT of its per-tenant budget guard: one
	// enthusiastic user can't monopolize the tenant's SHARED turn window (default per-user 5 <
	// tenant 10, so each user keeps a bounded fair-share). Window reused from AI_RATELIMIT so both
	// gates measure the same span. (Does not bound a user's daily token total — separate open item.)
	assistantLimiter := middleware.AssistantRateLimit(middleware.AssistantRateLimitConfig{
		MaxAttempts: aiCfg.AssistantRateLimitPerUser,
		Window:      time.Duration(aiCfg.RateLimitWindowSec) * time.Second,
	})
	// #52 per-API-KEY data-plane throttle (specs/045-api-key-throttle): a per-minute budget PLUS
	// an in-flight cap, so one customer integration retrying in a loop holds at most
	// DataMaxInFlightPerKey pooled connections at once (the budget alone would admit its whole
	// window in one burst). Only `nxk_` principals are counted; browser (JWT) sessions pass through.
	// Per key and in-memory ⇒ per process; several runaway keys together can still fill the pool.
	dataLimiter := middleware.DataRateLimit(middleware.DataRateLimitConfig{
		MaxAttempts: cfg.DataRateLimitPerMin,
		Window:      time.Minute,
		MaxInFlight: cfg.DataMaxInFlightPerKey,
	})
	api := r.Group("/api/v1")
	// APIKEY-1: the Bearer verifier is now the COMPOSITE — `nxk_` credentials verify as API
	// keys (DB-backed, IsAdmin hard-floored false), everything else falls through to the JWT
	// TokenService verbatim. Same middleware chain, zero downstream changes.
	// specs/064 S9-1: …and `typ=preview` JWTs (refused by the access verifier as a wrong type) go to
	// the preview service — the ONE door opened for them; every other door still answers 401.
	bearerVerifier := services.NewCompositeVerifier(tokenSvc, apiKeySvc).WithPreview(previewSvc)
	handlers.RegisterAPIRoutes(api, bearerVerifier, aclSvc, handlers.APIHandlers{
		Auth: handlers.NewAuthHandler(authSvc, logger),
		// WithEntityACL: GET /metadata/entities must not name tables the caller cannot view
		// (METADATA-ACL-FILTER). aclSvc is the SAME cached decider RequireACL uses, so 19 checks
		// collapse to one DB load (acl_cache single-flight, 30s TTL).
		// WithFieldACL: GET /metadata/fields/:entity must not name a field the caller's role
		// cannot view (A6 "ẩn" = not served) — the SAME enforcer the record read strips with.
		// WithFieldLocks: and must mark `readOnly` the fields the role may view but not edit
		// (field-lock-signal) — the SAME enforcer, over the SAME fieldACLRepo, that the write
		// guard refuses with, so "hidden" and "locked" can never disagree.
		Metadata: handlers.NewMetadataHandler(database, logger).
			WithRoleActionsLoader(roleRepo).
			WithEntityACL(aclSvc).
			WithSchemaCutoff(draftGate).     // S7-8: new tables / fields / relations stay hidden from the customer until Publish
			WithFieldLister(draftFieldRepo). // ...and the builder's admin read carries its drafts
			WithEntityMarks(draftFieldRepo). // S7-9: ...and marks the tables going away at the next Publish
			WithFieldACL(newFieldACL()).
			WithFieldLocks(newFieldACL()),
		Admin: handlers.NewAdminEntityHandler(entitySvc, fieldSvc, logger).WithACLSeeder(appACLSeedSvc),
		// WithEntityACL: the layout read was the only metadata endpoint with no view check, so a
		// user with no grant on an entity could still read its section titles and field names.
		// WithFieldACL: the served layout must not place a hidden field either — otherwise the
		// edit form rendered it empty and resent it as null, and every save came back 403.
		Layout: handlers.NewAdminLayoutHandler(layoutSvc, logger).
			WithEntityACL(aclSvc).
			WithFieldACL(newFieldACL()),
		// W7 invite flow: the user handler mounts POST /admin/users/:id/reset-password-link
		// through the SAME AuthService reset machinery as forgot-password (ENG-AUTH2 #1).
		// specs/055 US3: GET /admin/users/:id/sessions reads the same login-session store.
		User: handlers.NewAdminUserHandler(userSvc, logger).WithResetLinkIssuer(authSvc).WithSessionManager(authSvc),
		// specs/065 lát B: the customer owner's Cài đặt › Người dùng (/org-admin/*), gated by WithUserManagerGate.
		OrgAdmin: handlers.NewOrgAdminHandler(userSvc, logger).WithResetLinkIssuer(authSvc).WithSessionManager(authSvc),
		Role:     handlers.NewAdminRoleHandler(roleSvc, logger),
		Team:     handlers.NewAdminTeamHandler(teamSvc, logger),
		Record:   handlers.NewDynamicRecordHandler(recordSvc, logger),
		// W7 audit v2: the list reader plus the CSV exporter (same filters, csvGuard-ed cells).
		Audit: handlers.NewAuditHandler(auditRepo, logger).
			WithExporter(services.NewAuditExportService(auditRepo)).
			// Per-record history for non-admins: the SAME record service the record screen reads
			// through (so row visibility cannot drift) and the SAME field-ACL enforcer as the
			// record read (so history is not a way around field-level security).
			WithRecordScope(recordSvc, newFieldACL()),
		FieldACL: handlers.NewAdminFieldACLHandler(fieldACLAdminSvc, logger),
		Workflow: handlers.NewAdminWorkflowHandler(workflowSvc, logger).WithSchedulerStatus(workflowCfg.SchedulerInterval),
		// F7-4: metaCache + dynRepo let /approvals/pending decorate rows with display labels.
		// WithFieldACL: the pending-list label decoration reads a field VALUE off another entity's
		// rows, so it must honour the same hidden-field config every record read does.
		Approval: handlers.NewApprovalHandler(approvalSvc, logger).
			WithRecordLabels(metaCache, dynRepo).
			WithFieldACL(newFieldACL()),
		// WithEntityACL: GET /reports must not list (nor name the entity of) a report the caller
		// cannot open — same decider as the metadata filter (REPORT-CATALOG-ACL, WAVE 6).
		Report:    handlers.NewAdminReportHandler(reportSvc, logger).WithEntityACL(aclSvc),
		Dashboard: handlers.NewAdminDashboardHandler(dashboardSvc, logger),
		// MS-C (../specs/027-handover-wizard/spec.md §3.1): ?targetTenant=<slug> cross-tenant install — userRepo
		// serves BOTH the live operator-flag read (same repo RequirePlatformOperator uses) and the
		// slug→active-tenant resolution; the audit dispatcher receives the source-tenant
		// __handover__ event.
		Vertical: handlers.NewAdminVerticalHandler(verticalSvc, logger).
			WithCrossTenantInstall(userRepo, userRepo, auditDispatcher).WithTenantFreeze(tenantFreeze),
		Tenant: handlers.NewAdminTenantHandler(tenantProvisionSvc, userRepo, logger),
		// specs/062 L5: the project registry. "Delivered" is read from the publish history
		// (metaversion.DeliveryRepo, one GROUP BY) - never stored on the profile.
		Project: handlers.NewAdminProjectHandler(
			services.NewProjectService(project.NewRepository(database), tenantProvisionSvc, doctorSvc, appSvc, auditDispatcher, logger).
				WithDelivery(metaversion.NewDeliveryRepo(sqlxDB)).WithOps(healthSvc).
				// specs/062 L6: POST /admin/projects - Provision (+ profile, one tx), then the built-in
				// pack through the same Install the handover wizard uses, then the builder session.
				WithCreate(tenantProvisionSvc, vertical_pack.NewStarter(verticalSvc), workspaceSessionSvc),
			userRepo, logger),
		TenantBackup: handlers.NewAdminTenantBackupHandler(tenantBackupSvc, userRepo, tbCfg.DownloadDeadline, logger),
		Notification: handlers.NewNotificationHandler(notificationSvc, logger),
		WorkQueue:    handlers.NewWorkQueueHandler(workQueueSvc, logger),
		// specs/043 S5: "Việc của tôi" — the inbox read off the work seats + claim / release / complete.
		// Labels and the hidden-field check are the approvals inbox's own, so a record label cannot
		// leak a field the caller's role may not view.
		WorkItem: handlers.NewWorkItemHandler(services.NewWorkItemService(bpmRepo, bpmDecider, recordSvc, aclSvc, logger).WithTeamDirectory(bpmRepo), logger).
			WithRecordLabels(metaCache, dynRepo).
			WithFieldACL(newFieldACL()),
		Search: handlers.NewSearchHandler(services.NewSearchService(metaRepo, aclSvc, recordSvc), logger),
		// GET /users/directory (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b): any authenticated user's
		// "who can I assign this to" lookup — reuses the SAME userRepo as the admin User handler,
		// but through the narrower directoryRepo slice (ListDirectory), never the admin CRUD one.
		UserDirectory: handlers.NewUserDirectoryHandler(services.NewUserDirectoryService(userRepo), logger).WithTeams(teamRepo),
		Branding:      handlers.NewBrandingHandler(services.NewTenantBrandingService(services.NewDraftBrandingStore(userRepo, draftRepo, draftGate).WithRecorder(directRec, services.NewBrandingDocAdapter())).WithAuditor(auditDispatcher, draftGate), logger),
		InboundHook:   handlers.NewInboundHookHandler(inboundHookSvc, logger),
		Credential:    handlers.NewAdminCredentialHandler(credentialSvc, logger),
		Payment:       handlers.NewPaymentHandler(services.NewTenantPaymentService(userRepo, recordSvc).WithAuditor(auditDispatcher), logger),
		File:          handlers.NewFileHandler(fileSvc, fileCfg.MaxUploadBytes, logger),
		// dynRepo also implements NoteUserLabelFetcher (BatchFetchUserLabels) — same batch
		// primitive the `user` field type's ?expand= uses, reused here for note author names.
		Note:          handlers.NewNoteHandler(noteSvc, logger).WithUserLabels(dynRepo),
		RecordShare:   handlers.NewRecordShareHandler(shareSvc, logger).WithUserLabels(dynRepo).WithTeams(teamRepo),
		Relation:      handlers.NewRelationHandler(relationSvc, dynRepo, aclSvc, logger),
		AdminRelation: handlers.NewAdminRelationHandler(relationSvc, relationLifecycleSvc, logger),
		Import:        handlers.NewImportHandler(importSvc, logger),
		SSO:           handlers.NewSSOHandler(ssoSvc, ssoCfg.RedirectBase, logger).WithFrontendBase(ssoCfg.FrontendBase),
		SavedView:     handlers.NewSavedViewHandler(viewSvc, logger),
		// Write deadline covers a worst-case turn (MaxSteps × per-call timeout) + 15s slack so the
		// shared 30s server WriteTimeout never truncates a valid slow AI response.
		// M4: the budget guard gates the chat route per tenant (rate + concurrency + daily
		// token budget — MED-1); session list/get/delete stay ungated (no LLM spend). guard is now
		// a REQUIRED constructor param (post-review MED-2 — WithBudgetGuard made it forgettable).
		AI: handlers.NewAIBuilderHandler(aiAgent, aiSessionStore, aiBudgetGuard, aiContextResolver, logger).
			WithWriteDeadline(time.Duration(aiCfg.MaxSteps*aiCfg.TimeoutSec)*time.Second + 15*time.Second),
		// ⌘K assistant: NOT admin-gated (router mounts it on the protected group) — per-caller
		// ACL lives inside the agent's tools. Shares the builder's session store (same tenant+
		// user ownership guard); rides its own guard instance (see assistantGuard above).
		Assistant: handlers.NewAssistantHandler(assistantAgent, aiSessionStore, assistantGuard, logger).
			WithWriteDeadline(time.Duration(aiCfg.MaxSteps*aiCfg.TimeoutSec)*time.Second + 15*time.Second),
		// App container: admin CRUD on /admin/apps + App Switcher reads (GET /apps, /apps/:name/nav).
		// WithCrossTenantRepair: an operator can repair a CUSTOMER workspace's grants without a
		// database session (?targetTenant=<slug>), through the same gate the install uses.
		App: handlers.NewAdminAppHandler(appSvc, logger).WithCrossTenantRepair(userRepo, userRepo).WithTenantFreeze(tenantFreeze),
		// Nexus Doctor: POST /admin/doctor/scan + POST /admin/doctor/fix (admin-gated).
		Doctor: handlers.NewAdminDoctorHandler(doctorSvc, logger),
		// Machine identity (APIKEY-1): /admin/api-keys — issue (secret shown once), list, revoke.
		APIKey: handlers.NewAdminAPIKeyHandler(apiKeySvc, logger),
		// "Connect an agent (MCP)" (issue #3): /admin/mcp-tokens — issue (token shown once), list, revoke.
		MCPToken: handlers.NewAdminMCPTokenHandler(mcpTokenSvc, logger),
		// Business-hours calendars (CALENDAR-1): /admin/business-calendars CRUD (admin-gated).
		Calendar: handlers.NewAdminCalendarHandler(calendarSvc, logger),
		// "Việc nền" (DAILY-QUEUE #31/#32): /admin/jobs — the tenant's background-job log over
		// the same sys_jobs store the queue runs on (tenant-admin gate, not the operator one), and
		// "Gửi lại" (POST /admin/jobs/:id/retry): each resend is audited (__job__); with the queue
		// off it answers 503 instead of parking a row nothing will run.
		Jobs: handlers.NewAdminJobsHandler(services.NewBackgroundJobService(jobsStore).
			WithAuditor(auditDispatcher).WithQueueEnabled(jobsCfg.Enabled), logger),
		// "Nhật ký email" (specs/066 L1b): GET /admin/email-log — the tenant's log of the mail its
		// rules sent or failed to send (adminGated; masked recipients only, never rendered data).
		// L2: POST /admin/email-log/:id/resend rebuilds a failed mail from the LIVE rule (workflowSvc holds
		// the raw workflowRepo via WithLiveRepo — never the draft layer) and sends it through the SAME
		// sender instance the engine uses (queue or direct), so the new row is logged like any rule mail.
		EmailLog: handlers.NewAdminEmailLogHandler(services.NewEmailLogService(emailLogRepo).
			WithMailConfigured(alertsCfg.Enabled).WithQueueEnabled(jobsCfg.Enabled).
			WithRenderer(workflowSvc).WithSender(workflowEmailSender).WithAuditor(auditDispatcher), logger),
		// "Mức dùng" (specs/066 L4): GET /admin/usage — per-day rule runs, mail and webhooks (adminGated).
		Usage: handlers.NewAdminUsageHandler(services.NewUsageService(usageRepo, usageCfg.Enabled()).WithGauges(usageGauges), logger),
		// "Sức khoẻ" (specs/066 L6): GET /admin/health — signals gathered live (healthSvc above).
		Health: handlers.NewAdminHealthHandler(healthSvc, logger),
		// specs/064 S9-1: POST /admin/preview-sessions (builderGated) + DELETE /preview-sessions/current.
		Preview: handlers.NewPreviewHandler(previewSvc, logger),
		// specs/063-draft-publish: /admin/publish/* (builderGated) — state, changes, first publish, publish, discard.
		Publish: handlers.NewAdminPublishHandler(publishSvc, logger),
		// specs/062 L1b: POST /admin/workspace-sessions + DELETE …/current (builderGated).
		WorkspaceSession: handlers.NewAdminWorkspaceSessionHandler(workspaceSessionSvc, logger),
		// specs/064 S9-4/S9-5: GET /admin/sample-data + POST …/records/:entity + POST …/generate (builderGated).
		SampleData: handlers.NewSampleDataHandler(
			// S9-7: wipe = RecordService.PurgeSample (the 056 hard delete), pinned defaults read from metaRepo's entity list.
			// S9-6: WithPackSeeder — source "pack"/"auto" re-seeds a built-in pack's samples.
			services.NewSampleDataService(dynRepo, metaCache, recordSvc).WithAuditor(auditDispatcher).
				WithPackSeeder(verticalSvc).WithWipe(recordSvc, metaRepo).WithLogger(logger), logger),
	}, logger,
		// main_wiring_test.go reads these lines: dropping an option (or hard-coding a limiter
		// setting) fails the build gate. A new hardening option belongs in its table too.
		handlers.WithBodySizeLimit(cfg.MaxRequestBodyBytes),
		handlers.WithAuthRateLimiter(authLimiter),
		handlers.WithSSORateLimiter(ssoLimiter),
		handlers.WithAssistantRateLimiter(assistantLimiter),
		handlers.WithDataRateLimiter(dataLimiter),
		// EASE Nước 6: two-tier /admin — builder surfaces (schema/layouts/relationships/packs/
		// apps/doctor/api-keys/AI-builder) require the platform-operator flag (DB-fresh read,
		// same gate as /admin/tenants); a customer's tenant-admin keeps operational governance
		// only. Pre-flight: both Nexus accounts carry is_platform_operator=1 (2026-07-25).
		// specs/062: RequireBuilderTier = the same live read for a home token, and lets a confirmed
		// builder workspace-session token through (RequirePlatformOperator itself refuses it: it reads
		// the user row by the token's tenant, which in a session is the customer).
		handlers.WithOperatorTier(middleware.RequireBuilderTier(userRepo, logger)),
		// specs/065: the customer-owner gate (live read of users.can_manage_users) in front of /org-admin/*.
		handlers.WithUserManagerGate(middleware.RequireUserManager(userRepo, logger)),
		// specs/062: the delegation branch of the Auth stage — every request carrying act/att/wsid is
		// re-resolved (session row + live builder), uncached; without this option such a token is 401.
		handlers.WithWorkspaceResolver(workspaceSessionSvc),
		// specs/068 slice B: the per-tenant write freeze (503 TENANT_MAINTENANCE) on the protected chain.
		handlers.WithTenantFreeze(tenantFreeze),
		// #356: the Auth stage hands the API-key verifier the client IP — the SAME address, behind the
		// same trusted proxies, the login limiter keys on.
		handlers.WithTrustedProxies(cfg.TrustedProxyCIDRs),
		// #14 claim-freshness: LIVE auth stage — role/is_admin/status from the user row (via the
		// cache the user service invalidates), not from the token. `nxk_` keys are not re-read.
		handlers.WithLiveSubject(userClaims),
		// specs/055: …and refuses a token whose login session was ended (same guard as authSvc).
		handlers.WithSessionGuard(sessionGuard),
	)

	// Graceful shutdown: capture SIGINT / SIGTERM so in-flight HTTP requests and the
	// alert dispatcher's buffered channel are drained before the process exits.
	// We wrap gin in a net/http.Server so we can call Shutdown() on it (r.Run() blocks
	// and never returns on signal, making defer dispatcher.Shutdown unreachable).
	//
	// Timeouts (RM-16): ReadHeaderTimeout prevents Slowloris; ReadTimeout / WriteTimeout
	// prevent hanging connections from exhausting goroutines; IdleTimeout closes keep-alive
	// connections faster than the default 3 min so FDs are reclaimed promptly.
	srv := &http.Server{
		Addr:              ":" + cfg.ServerPort,
		Handler:           r,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// Listen for OS signals in a separate goroutine.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("Server listening on port", zap.String("port", cfg.ServerPort))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("Server ListenAndServe error", zap.Error(err))
		}
	}()

	// Block until a signal arrives.
	<-quit
	logger.Info("Shutdown signal received, draining...")

	// Give in-flight HTTP requests up to 10 s to finish.
	httpCtx, httpCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer httpCancel()
	if err := srv.Shutdown(httpCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	}

	// Stop the per-customer backup reaper and runner (specs/068). A backup still writing is
	// cancelled and records itself failed (BACKUP_CANCELLED) with its partial file removed — never
	// left "running" for the next boot's reaper to find. Before the audit dispatcher drains below.
	stopTenantBackupReaper()
	tbCtx, tbCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer tbCancel()
	if err := tenantBackupScheduler.Stop(tbCtx); err != nil {
		logger.Warn("Tenant backup scheduler did not stop cleanly within deadline", zap.Error(err))
	}
	if err := tenantBackupRunner.Stop(tbCtx); err != nil {
		logger.Warn("Tenant backup runner did not stop cleanly within deadline (stale rows will be reaped)", zap.Error(err))
	}

	// Drain the alert dispatcher's buffered channel (spec §6 graceful drain). Give it
	// 5 s; any alerts still in the buffer after the deadline are logged by the dispatcher.
	dispatchCtx, dispatchCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dispatchCancel()
	if err := dispatcher.Shutdown(dispatchCtx); err != nil {
		logger.Warn("Alert dispatcher did not drain cleanly within deadline", zap.Error(err))
	}

	// Drain the EPIC 4 workflow engine's buffered channel (graceful drain). Give it 5 s; any
	// events still buffered after the deadline are logged by the dispatcher (fail-open).
	wfCtx, wfCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wfCancel()
	if err := workflowEngine.Shutdown(wfCtx); err != nil {
		logger.Warn("Workflow engine did not drain cleanly within deadline", zap.Error(err))
	}

	// Stop the EPIC 4 M2 background scheduler (graceful): signal the ticker goroutine to exit
	// and wait up to 5 s for the in-flight tick to finish. Only stop when it was actually
	// started (enabled) — calling Stop on a never-started scheduler would block on its done
	// channel (the run goroutine that closes it never launched) until the deadline, producing
	// a spurious "did not stop cleanly" warning.
	if workflowCfg.SchedulerEnabled() {
		schedCtx, schedCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer schedCancel()
		if err := workflowScheduler.Stop(schedCtx); err != nil {
			logger.Warn("Workflow scheduler did not stop cleanly within deadline", zap.Error(err))
		}
	}

	// Stop the EPIC 5 BPM SLA scheduler (graceful) + drain the arming engine. Same enabled-guard
	// as the workflow scheduler. The arming engine drains its buffered approval events.
	if workflowCfg.SchedulerEnabled() {
		slaCtx, slaCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer slaCancel()
		if err := bpmSLAScheduler.Stop(slaCtx); err != nil {
			logger.Warn("BPM SLA scheduler did not stop cleanly within deadline", zap.Error(err))
		}
	}

	// Stop the retention janitor (graceful). Same enabled-guard: only Stop when it was Started, so
	// Stop never blocks on a done channel whose run goroutine never launched.
	if janitorCfg.Enabled() {
		janCtx, janCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer janCancel()
		if err := retentionJanitor.Stop(janCtx); err != nil {
			logger.Warn("Retention janitor did not stop cleanly within deadline", zap.Error(err))
		}
	}

	// Stop the JOBS-1 worker pool (graceful): a worker finishes its in-flight job before exiting.
	// Same enabled-guard as the janitor. A job still running past the deadline is NOT lost — it
	// stays 'running' and the reaper of the next boot reclaims it after the lease (durability).
	if jobsCfg.Enabled {
		jobsCtx, jobsCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer jobsCancel()
		if err := jobsManager.Stop(jobsCtx); err != nil {
			logger.Warn("Job queue did not drain cleanly within deadline (in-flight jobs will be reaped)", zap.Error(err))
		}
	}

	// Stop the DOCTOR-OPS scheduled scan (graceful). Same enabled-guard as the janitor.
	if doctorCfg.Enabled() {
		docCtx, docCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer docCancel()
		if err := doctorScheduler.Stop(docCtx); err != nil {
			logger.Warn("Doctor scheduler did not stop cleanly within deadline", zap.Error(err))
		}
	}
	// Stop the usage roller (graceful), same enabled-guard.
	if usageCfg.Enabled() {
		usageCtx, usageCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer usageCancel()
		if err := usageRoller.Stop(usageCtx); err != nil {
			logger.Warn("Usage roller did not stop cleanly within deadline", zap.Error(err))
		}
	}
	bpmCtx, bpmCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer bpmCancel()
	if err := bpmEngine.Shutdown(bpmCtx); err != nil {
		logger.Warn("BPM approval engine did not drain cleanly within deadline", zap.Error(err))
	}

	// Drain the before-rule run sink (specs/048 S3) after everything above that writes records —
	// HTTP requests, a rule's create_record, a timer job, an approval's outcome write-back all judge
	// before rules and may leave a run. Runs still buffered past 5 s are lost (fail-open).
	runSinkCtx, runSinkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer runSinkCancel()
	if err := beforeRunSink.Close(runSinkCtx); err != nil {
		logger.Warn("Before-rule run sink did not drain cleanly within deadline", zap.Error(err))
	}

	// Drain the RM-19 audit dispatcher's buffered channel (graceful drain) LAST: the workflow
	// engine, the scheduler, the job queue (timer fires) and the BPM engine above all PRODUCE audit
	// rows while they drain — a machine field_update/assign writes one (queue #86) — so closing the
	// audit sink before them dropped exactly those rows. Give it 5 s; any audit rows still buffered
	// after the deadline are logged by the dispatcher (fail-open).
	auditCtx, auditCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer auditCancel()
	if err := auditDispatcher.Shutdown(auditCtx); err != nil {
		logger.Warn("Audit dispatcher did not drain cleanly within deadline", zap.Error(err))
	}

	logger.Info("Server exited cleanly")
}
