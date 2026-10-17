package handlers

import (
	"net"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
)

// TokenVerifier is the subset of auth.TokenService the protected-route chain needs to verify a
// presented Bearer access token. Declaring it here (consumer-side) lets RegisterAPIRoutes wire
// the AuthMiddleware without importing a concrete TokenService and keeps the builder testable.
type TokenVerifier interface {
	VerifyAccessToken(tok string) (*auth.Claims, error)
}

// APIHandlers bundles the per-resource HTTP handlers RegisterAPIRoutes mounts. Each is built
// by the caller (main.go / tests) from its services so this builder stays wiring-only.
type APIHandlers struct {
	Auth      *AuthHandler
	Metadata  *MetadataHandler
	Admin     *AdminEntityHandler
	Layout    *AdminLayoutHandler
	User      *AdminUserHandler
	Role      *AdminRoleHandler
	Team      *AdminTeamHandler
	Record    *DynamicRecordHandler
	Audit     *AuditHandler
	Workflow  *AdminWorkflowHandler
	Approval  *ApprovalHandler
	Report    *AdminReportHandler
	Dashboard *AdminDashboardHandler
	Vertical  *AdminVerticalHandler
	// Tenant is the operator-only provisioning handler (TENANT-PROVISION): POST/GET /admin/tenants,
	// mounted behind Auth → Tenant → RequireAdmin → RequirePlatformOperator. nil disables it.
	Tenant *AdminTenantHandler
	// TenantBackup serves the per-customer backup routes (specs/068-tenant-backup-restore):
	// /admin/tenants/:id/backups[...] behind the same operator gate as Tenant. nil disables it.
	TenantBackup *AdminTenantBackupHandler
	// Project is the project registry (specs/062 L5): GET/PATCH /admin/projects*, operator-only. nil disables it.
	Project *AdminProjectHandler
	// Notification serves the P0 notification-read surface (list, unread-count, mark-read,
	// mark-all-read). Mounted on the protected (non-admin) group, user-scoped. nil disables it.
	Notification *NotificationHandler
	// WorkQueue serves VIEC-HOM-NAY: GET /work-queues (any user — home-screen cards with live
	// counts) + /admin/work-queues CRUD. nil disables it.
	WorkQueue *WorkQueueHandler
	// WorkItem serves "Việc của tôi" (specs/043 S5): GET /work-items[/counts] and POST
	// /work-items/:id/claim|release|complete, on the protected (non-admin) group. nil disables it.
	WorkItem *WorkItemHandler
	// Branding serves AO-THUONG-HIEU p1: public GET /auth/branding/:tenant (login page,
	// unthrottled like /auth/me) + /admin/tenant/branding GET/PUT. nil disables it.
	Branding *BrandingHandler
	// InboundHook serves CONG-TIN-HIEU: public POST /hooks/:tenant/:hook (HMAC-signed, no
	// JWT) + /admin/inbound-hooks CRUD/rotate/deliveries. nil disables it.
	InboundHook *InboundHookHandler
	// Credential serves the named-credential vault (../specs/050-named-credentials):
	// /admin/credentials[/:id] on adminGated — the customer's own admin stores partner keys,
	// sealed and never read back. nil disables it.
	Credential *AdminCredentialHandler
	// Payment serves DONG-TIEN-KHEP-KIN p1: GET /:entity/:id/payment-qr (on the ACL record
	// group) + /admin/tenant/payment GET/PUT. nil disables it.
	Payment *PaymentHandler
	// Search serves SEARCH-UNIFIED (3.5): GET /search?q= — server-side tenant+ACL fan-out over
	// every viewable entity for ⌘K. Mounted on the protected group. nil disables it.
	Search *SearchHandler
	// File serves the File-upload M1 surface: POST /:entity/:id/files (entity-scoped, ACL-gated)
	// and GET/DELETE /files/:id (tenant + A5 scoped inside the service). nil disables it.
	File *FileHandler
	// Note serves the RECORD-NOTES engine capability (docs/NEXUS-MOT-XUONG-2026-07-29.md §3d mục
	// 1): GET/POST /:entity/:id/notes + DELETE /:entity/:id/notes/:noteId. nil disables it.
	Note *NoteHandler
	// RecordShare serves manual per-record READ shares (specs/051-record-manual-share):
	// GET/POST /:entity/:id/shares + DELETE /:entity/:id/shares/:userId. nil disables it.
	RecordShare *RecordShareHandler
	// Relation is the N-N link/unlink/list handler (M4). It is mounted on the recordGroup so
	// the same RequireACL gate that governs /:entity CRUD also governs relationship operations.
	Relation *RelationHandler
	// AdminRelation is the admin-only N-N registration handler: POST /admin/relationships.
	AdminRelation *AdminRelationHandler
	// Import is the CSV import M1 handler: POST /:entity/import (map+validate+batch). Mounted on
	// the recordGroup BEFORE the dynamic dispatcher so the static /import segment resolves first.
	Import *ImportHandler
	// SSO is the ENG-AUTH2 #8 OIDC handler: PUBLIC GET /auth/sso/:tenant/start + GET
	// /auth/sso/callback (mounted on the same public group as Auth), and ADMIN-gated
	// POST/GET /admin/sso/providers + DELETE /admin/sso/providers/:id. nil disables it.
	SSO *SSOHandler
	// FieldACL is the W7 governance config surface for A6 field-level security:
	// GET/PUT/DELETE /admin/field-acl (admin-gated). nil disables it.
	FieldACL *AdminFieldACLHandler
	// SavedView is the W9b saved-view CRUD surface: GET/POST /:entity/views, PUT/DELETE
	// /:entity/views/:id. Entity-scoped, gated by RequireACLAction("view"); the shared-vs-personal
	// ownership gate is enforced in-service (ViewService). nil disables it.
	SavedView *SavedViewHandler
	// AI is the E9 conversational AI-Builder handler: POST /admin/ai/builder/chat (admin-only).
	// The route mounts even when AI is disabled (AI_ENABLED=false) — the agent then answers 503
	// AI_DISABLED (honest-disabled). nil disables the route entirely.
	AI *AIBuilderHandler
	// Assistant is the ⌘K end-user data assistant: POST /assistant/ask. Mounted on the protected
	// (non-admin) group — every read inside it runs through the caller's own RecordScope + ACL,
	// so any authenticated user may ask about the data they can already browse. Like AI, the
	// route mounts even when AI is disabled (503 AI_DISABLED). nil disables it.
	Assistant *AssistantHandler
	// App is the App-container handler (../specs/010-app-container/spec.md): admin CRUD + nav writes on
	// /admin/apps (admin-gated) plus the App Switcher reads GET /apps and GET /apps/:name/nav on
	// the protected group (any authenticated user; nav is ACL-filtered in-service, INV-3).
	// nil disables it.
	App *AdminAppHandler
	// Doctor is the Nexus Doctor handler (../specs/008-nexus-doctor/spec.md, DOCTOR-CORE): POST
	// /admin/doctor/scan (read-only drift scan) + POST /admin/doctor/fix (targeted repairs),
	// admin-gated like every other /admin resource. nil disables it.
	Doctor *AdminDoctorHandler
	// APIKey is the machine-identity admin surface (APIKEY-1): POST/GET /admin/api-keys +
	// DELETE /admin/api-keys/:id, admin-gated. The keys it issues authenticate on the SAME
	// Bearer chain via the composite verifier passed to RegisterAPIRoutes. nil disables it.
	APIKey *AdminAPIKeyHandler
	// MCPToken is "Connect an agent (MCP)" (issue #3): POST/GET /admin/mcp-tokens + DELETE
	// /admin/mcp-tokens/:id on builderGated. The tokens it issues work on /mcp only. nil disables it.
	MCPToken *AdminMCPTokenHandler
	// Calendar is the business-hours calendar admin CRUD (CALENDAR-1):
	// /admin/business-calendars[/:id], admin-gated. The rows it manages feed the SLA/timer
	// engine (slice 1b) through internal/calendar. nil disables it.
	Calendar *AdminCalendarHandler
	// Jobs serves "Việc nền" (DAILY-QUEUE #31/#32, specs/021 §3): the tenant's background-job log
	// (GET /admin/jobs). Mounted on adminGated — the customer's own tenant-admin must reach it.
	// nil disables it.
	Jobs *AdminJobsHandler
	// OrgAdmin serves the customer owner's "Cài đặt › Người dùng" (specs/065 lát B): /org-admin/users[/:id],
	// /org-admin/roles. Mounted on the protected group behind the owner gate (WithUserManagerGate) — NOT under
	// /admin — and fail-closed: without the gate option the routes are not mounted at all. nil disables it.
	OrgAdmin *OrgAdminHandler
	// EmailLog serves "Nhật ký email" (specs/066 L1b): the tenant's log of the mail its rules sent
	// (GET /admin/email-log). Mounted on adminGated — the customer's own tenant-admin must reach
	// it, never builderGated. nil disables it.
	EmailLog *AdminEmailLogHandler
	// Usage serves "Mức dùng" (specs/066 L4): GET /admin/usage — the tenant's per-day rule runs,
	// mail and webhooks. adminGated, like EmailLog. nil disables it.
	Usage *AdminUsageHandler
	// Health serves "Sức khoẻ" (specs/066 L6): GET /admin/health — the tenant's operating signals and
	// overall label. adminGated, like Usage. nil disables it.
	Health *AdminHealthHandler
	// UserDirectory serves GET /users/directory (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b — the
	// owner-change FE-gap): a minimal {id,fullName,email} listing of the tenant's ACTIVE users,
	// available to ANY authenticated user (not admin-gated, unlike Admin's /admin/users). nil
	// disables it.
	UserDirectory *UserDirectoryHandler
	// Preview serves "preview as role" (specs/064-preview-as-role-sample-data S9-1): POST
	// /admin/preview-sessions on builderGated (a builder opens a read-only session as one role) and
	// DELETE /preview-sessions/current on the protected group (the preview token leaves). nil
	// disables both.
	Preview *PreviewHandler
	// Publish serves /admin/publish/* (specs/063-draft-publish): state, change list, first publish,
	// publish and discard of a delivered workspace's drafts. Mounted on builderGated. nil disables it.
	Publish *AdminPublishHandler
	// WorkspaceSession serves the builder workspace session of specs/062-customer-workspace L1b:
	// POST /admin/workspace-sessions (home token only) and DELETE /admin/workspace-sessions/current
	// (session token), both on builderGated. nil disables both.
	WorkspaceSession *AdminWorkspaceSessionHandler
	// SampleData serves GET /admin/sample-data and POST /admin/sample-data/records/:entity on
	// builderGated (specs/064-preview-as-role-sample-data S9-4).
	SampleData *SampleDataHandler
}

// ACLChecker is the permission-lookup the ACL middleware needs on the dynamic record surface:
// given a verified (tenant, role) it reports whether that role grants `action` on `entity`
// (honoring the entity_name='*' wildcard internally, with strict default-deny). Declared here
// (as an alias of middleware.ACLChecker) so the route builder can wire middleware.RequireACL
// without importing a concrete type; the real implementation is *services.ACLService (backed by
// auth.RoleRepository). A nil checker disables the ACL stage (the dynamic dispatcher then runs
// without per-action RBAC) — production always passes the real checker.
type ACLChecker = middleware.ACLChecker

// routeConfig holds optional configuration applied by RegisterAPIRoutes.
type routeConfig struct {
	// trustedProxies feeds middleware.WithTrustedProxies on the Auth stage (see WithTrustedProxies).
	trustedProxies []*net.IPNet
	// bodySizeLimit is the maximum request body size in bytes (RM-16). ≤0 means no limit.
	bodySizeLimit int64
	// authRateLimiter, when non-nil, is mounted on the public /auth/* group (RM-15).
	authRateLimiter gin.HandlerFunc
	// ssoRateLimiter, when non-nil, is mounted on the public SSO group instead of the strict
	// /auth limiter (SSO-RATELIMIT-NAT). When nil, the SSO group falls back to authRateLimiter
	// so callers that never wire the SSO option keep the pre-slice behaviour (throttled, never
	// silently unthrottled).
	ssoRateLimiter gin.HandlerFunc
	// assistantRateLimiter, when non-nil, is mounted on POST /assistant/ask as a PER-USER cap in
	// front of the endpoint's per-tenant budget guard (one user can't exhaust the tenant). When
	// nil the endpoint keeps only the per-tenant budget guard (test-only / pre-slice shape).
	assistantRateLimiter gin.HandlerFunc
	// dataRateLimiter (DAILY-QUEUE #52, specs/045-api-key-throttle), when non-nil, is mounted on
	// the PROTECTED group right after RequireTenant — before every protected route and sub-group
	// is registered — so it fronts the whole data plane (/:entity included). It counts API-key
	// principals only (per key); JWT browser sessions pass through untouched. nil = no data-plane
	// throttle (test harnesses / pre-slice shape).
	dataRateLimiter gin.HandlerFunc
	// operatorTier (EASE Nước 6, ../specs/034-customer-admin-tier/spec.md §2), when non-nil, is mounted on the
	// BUILDER subset of /admin/* (entities/fields/layouts/relationships/verticals/apps/doctor/
	// api-keys/ai-builder) as a stage AFTER RequireAdmin — creating the two-tier split: platform
	// operators keep the factory, a customer's tenant-admin keeps ONLY operational governance
	// (users/roles/teams/field-acl/audit/sso/automation/analytics). When nil (test harnesses,
	// pre-slice callers) every /admin route keeps the single coarse RequireAdmin gate.
	operatorTier gin.HandlerFunc
	// userManagerGate (specs/065), when non-nil, is the owner gate (middleware.RequireUserManager) that fronts
	// /org-admin/*. Absent ⇒ those routes are NOT mounted (fail closed: never reachable without their gate).
	userManagerGate gin.HandlerFunc
	// liveUsers (queue #14 claim-freshness, ../specs/001-auth-security/spec.md §5), when non-nil,
	// switches the Auth stage to middleware.AuthMiddlewareLive: every protected request runs under
	// the role/is_admin/status of the caller's user ROW (read through this reader — the cached
	// loader in production) instead of the token claims, so a demotion / disable / delete lands
	// on the very next request. When nil (test harnesses, pre-slice callers) the Auth stage
	// trusts the token claims for the token's lifetime — the pre-#14 shape.
	liveUsers middleware.LiveUserReader
	// liveSessions (specs/055 FR-008), when non-nil alongside liveUsers, makes the live Auth stage
	// also refuse an access token whose login session was ENDED (DELETE /auth/sessions/:id, the
	// admin twin, logout-all) — on the very next request, not at token expiry.
	liveSessions middleware.LiveSessionChecker
	// workspaceResolver (specs/062-customer-workspace L1a), when non-nil, opens the delegation branch
	// of the Auth stage: a typ=access token carrying act/att/wsid (a builder's workspace-session token)
	// is re-resolved through it on EVERY request. nil ⇒ such a token is refused with 401 (fail closed).
	workspaceResolver middleware.WorkspaceResolver
	// tenantFreeze (specs/068 slice B), when non-nil, mounts RequireTenantWritable on the protected
	// group (503 TENANT_MAINTENANCE for the write methods of a frozen tenant).
	tenantFreeze middleware.TenantFreezeChecker
}

// RouteOption is a functional option for RegisterAPIRoutes, allowing callers to add
// optional hardening middleware (body-size limit, rate limiter) without changing the
// function signature in a way that breaks existing test harnesses.
type RouteOption func(*routeConfig)

// WithBodySizeLimit returns a RouteOption that caps request bodies at limit bytes (RM-16).
// A value ≤ 0 is ignored (no limit applied).
func WithBodySizeLimit(limit int64) RouteOption {
	return func(c *routeConfig) { c.bodySizeLimit = limit }
}

// WithAuthRateLimiter returns a RouteOption that mounts h as a gin middleware on the
// public /auth/* sub-group (RM-15). h should be the result of middleware.AuthRateLimit.
func WithAuthRateLimiter(h gin.HandlerFunc) RouteOption {
	return func(c *routeConfig) { c.authRateLimiter = h }
}

// WithSSORateLimiter returns a RouteOption that mounts h as the DEDICATED limiter on the
// public SSO sub-group (GET /auth/sso/:tenant/start, GET /auth/sso/callback) instead of the
// strict /auth/* limiter (SSO-RATELIMIT-NAT: an enterprise behind NAT must not exhaust the
// credential-guessing bucket during an SSO login burst). h should be the result of
// middleware.SSORateLimit. When this option is absent, the SSO routes fall back to the
// /auth/* limiter (pre-slice behaviour) — never to no limiter at all.
func WithSSORateLimiter(h gin.HandlerFunc) RouteOption {
	return func(c *routeConfig) { c.ssoRateLimiter = h }
}

// WithAssistantRateLimiter returns a RouteOption that mounts h as a PER-USER BURST limiter in front
// of POST /assistant/ask (the ⌘K end-user assistant). The endpoint's own guard caps spend per
// TENANT; this adds a per-(tenant,user) ceiling BELOW the tenant turn cap so one user can't
// monopolize the shared turn window. h should be the result of middleware.AssistantRateLimit. When
// absent, the route keeps only its per-tenant guard (test-only / pre-slice shape).
func WithAssistantRateLimiter(h gin.HandlerFunc) RouteOption {
	return func(c *routeConfig) { c.assistantRateLimiter = h }
}

// WithDataRateLimiter returns a RouteOption that mounts h (the result of middleware.DataRateLimit)
// on the protected group, in front of every authenticated route: a per-API-key budget + in-flight
// cap, so one customer integration stuck in a retry loop holds at most a bounded share of the
// shared connection pool instead of all of it. Browser (JWT) sessions are never counted.
func WithDataRateLimiter(h gin.HandlerFunc) RouteOption {
	return func(c *routeConfig) { c.dataRateLimiter = h }
}

// WithOperatorTier returns a RouteOption that mounts h (the result of
// middleware.RequirePlatformOperator) on the BUILDER subset of /admin/* — the EASE Nước 6
// two-tier split. Absent ⇒ legacy single-tier RequireAdmin everywhere (fail-open by DESIGN
// here is safe: the routes are still admin-gated; the option only ever NARROWS access).
func WithOperatorTier(h gin.HandlerFunc) RouteOption {
	return func(c *routeConfig) { c.operatorTier = h }
}

// WithUserManagerGate returns a RouteOption that mounts h (the result of middleware.RequireUserManager)
// in front of /org-admin/* (specs/065). Absent ⇒ /org-admin/* is not registered at all.
func WithUserManagerGate(h gin.HandlerFunc) RouteOption {
	return func(c *routeConfig) { c.userManagerGate = h }
}

// WithLiveSubject returns a RouteOption that mounts the LIVE Auth stage
// (middleware.AuthMiddlewareLive) in place of the claims-only one: users is the reader every
// protected request re-reads the caller's row through — in production the
// services.CachedUserLoader the user service invalidates on every committed user mutation
// (queue #14 claim-freshness: a demotion / role change / disable / delete takes effect on the
// user's very next request; the cache TTL is only the backstop). API-key principals are never
// re-read (machine identity, no user row). Absent ⇒ pre-#14 behaviour (token claims verbatim).
func WithLiveSubject(users middleware.LiveUserReader) RouteOption {
	return func(c *routeConfig) { c.liveUsers = users }
}

// WithSessionGuard returns a RouteOption that adds the ended-session check to the LIVE Auth stage
// (specs/055 FR-008): sessions is asked, per request carrying a `sid` claim, whether that login
// session was ended — in production the services.CachedSessionGuard the auth service invalidates
// the moment a session ends. Only effective together with WithLiveSubject.
func WithSessionGuard(sessions middleware.LiveSessionChecker) RouteOption {
	return func(c *routeConfig) { c.liveSessions = sessions }
}

// WithWorkspaceResolver returns a RouteOption that wires the resolver of the builder workspace-session
// token into the Auth stage (specs/062-customer-workspace L1a): production passes
// services.WorkspaceSessionService. Needed alongside WithLiveSubject for the full chain, but the
// delegation branch is independent of the live user reader (it never consults it). Absent ⇒ every
// delegation token answers 401.
func WithWorkspaceResolver(r middleware.WorkspaceResolver) RouteOption {
	return func(c *routeConfig) { c.workspaceResolver = r }
}

// WithTenantFreeze wires the per-tenant write-freeze gate (specs/068 slice B): production passes
// *services.TenantFreeze. Absent ⇒ no freeze is enforced on the HTTP write path.
func WithTenantFreeze(f middleware.TenantFreezeChecker) RouteOption {
	return func(c *routeConfig) { c.tenantFreeze = f }
}

// WithTrustedProxies returns a RouteOption telling the Auth stage which reverse-proxy networks may
// vouch for X-Forwarded-For when it derives the client IP it hands the API-key verifier (the per-IP
// brake on failed key lookups, queue #356). Production passes the TRUSTED_PROXY_CIDRS set the login
// limiter uses; absent ⇒ the direct peer (XFF never trusted).
func WithTrustedProxies(cidrs []*net.IPNet) RouteOption {
	return func(c *routeConfig) { c.trustedProxies = cidrs }
}

// RegisterAPIRoutes wires the complete /api/v1 surface onto rg with the middleware chain in the
// SPEC-MANDATED ORDER (../specs/001-auth-security/spec.md §4: Auth → Tenant → ACL → Business Logic). It is the
// single source of truth for route mounting, shared by the production server (cmd/server/main.go)
// and the integration tests, so the order is verified structurally rather than duplicated.
//
// Two sub-groups are created on rg:
//
//   - PUBLIC group: the pre-auth auth endpoints (login/refresh are the credential entry points;
//     logout/me self-verify the presented token in the A1 contract). These sit BEFORE AuthMiddleware.
//   - PROTECTED group: AuthMiddleware (verify Bearer → lift verified claims, incl. tenant, into
//     context) then RequireTenant (fail closed if a verified token carries no tenant claim) then
//     the metadata / admin / dynamic-record handlers. EVERY business route therefore derives its
//     tenant strictly from the verified claim — never from a header/query/body — and a request with
//     no valid token (or no tenant claim) is rejected with a 401 before any handler runs.
//
// verifier is the access-token verifier (the real auth.TokenService in production); aclChecker is
// the per-action RBAC decider mounted on the dynamic /:entity surface (A4) — a nil checker skips
// the ACL stage. bodySizeLimit is the maximum request body in bytes for the global cap (RM-16);
// a value ≤ 0 disables the cap. logger may be nil.
func RegisterAPIRoutes(rg *gin.RouterGroup, verifier TokenVerifier, aclChecker ACLChecker, h APIHandlers, logger *zap.Logger, opts ...RouteOption) {
	if logger == nil {
		logger = zap.NewNop()
	}

	// Apply optional route options (e.g. body-size limit, rate limiting).
	var cfg routeConfig
	for _, o := range opts {
		o(&cfg)
	}

	// Global body-size cap (RM-16): mounted first so every route — including /auth/login —
	// is protected. A ≤0 limit is a no-op (see middleware.MaxBodySize).
	if cfg.bodySizeLimit > 0 {
		rg.Use(middleware.MaxBodySize(cfg.bodySizeLimit))
	}

	// --- PUBLIC: no auth gate (credential entry points). -------------------------------------
	// Mounted on rg directly so /auth/* stays reachable without a token.
	authGroup := rg.Group("")
	if cfg.authRateLimiter != nil {
		authGroup.Use(cfg.authRateLimiter)
	}
	if h.Auth != nil {
		// Auth rate-limiter (RM-15): applied only to the public /auth/* group so it does
		// not add overhead to authenticated routes. The limiter is keyed per
		// (tenant + email + source IP) and is separate from the global body-size cap above.
		// AUTH-ME-RATELIMIT (catalog B11): the brute-forceable credential endpoints go on the
		// rate-limited authGroup, but GET /auth/me is mounted on the UNLIMITED public group
		// (rg) — it is an authenticated read every FE navigation calls, so counting it against
		// the /auth/* limiter self-locked login on fast navigation. It still self-verifies the
		// Bearer token in the handler (401 on an invalid one), so exempting it opens no hole.
		h.Auth.RegisterThrottledRoutes(authGroup)
		h.Auth.RegisterMeRoute(rg)
	}
	if h.Branding != nil {
		// Public branding read rides the UNLIMITED group for the same reason as /auth/me
		// (see AUTH-ME-RATELIMIT above); the admin surface is mounted with the other admin
		// routes further down.
		h.Branding.RegisterPublicRoute(rg)
	}
	if h.InboundHook != nil {
		// CONG-TIN-HIEU receive endpoint: pre-auth like SSO, but its gate is the per-hook
		// HMAC signature (uniform 401 for every rejection). Body cap + timeouts still apply.
		h.InboundHook.RegisterPublicRoute(rg)
	}
	// ENG-AUTH2 #8 + SSO-RATELIMIT-NAT: SSO public entry points (/start, /callback) are pre-auth
	// like login, but ride a DEDICATED group with their OWN limiter. The RM-15 /auth bucket is
	// sized for credential guessing (default 10/60 s per (ip,tenant,email) — and SSO GETs carry
	// no JSON body, so they all collapsed onto the bare per-IP key); an enterprise behind NAT
	// (whole workforce on one egress IP) doing an SSO login burst would exhaust that strict
	// bucket and lock the company out of ALL /auth endpoints, password login included. The SSO
	// limiter (middleware.SSORateLimit) keys /start per (ip, tenant, provider) and /callback per
	// ip, with a larger default budget (60/60 s) that never touches the /auth budget. Fallback:
	// when no SSO limiter option is wired, the SSO group reuses the /auth limiter — the
	// pre-slice behaviour — so a caller upgrading nothing keeps SSO throttled (fail-secure),
	// never silently unthrottled.
	if h.SSO != nil {
		ssoGroup := rg.Group("")
		switch {
		case cfg.ssoRateLimiter != nil:
			ssoGroup.Use(cfg.ssoRateLimiter)
		case cfg.authRateLimiter != nil:
			ssoGroup.Use(cfg.authRateLimiter)
		}
		h.SSO.RegisterRoutes(ssoGroup)
	}

	// --- PROTECTED: Auth → Tenant → (ACL, A4) → Business Logic. -------------------------------
	// A fresh sub-group inherits rg's prefix but adds the auth chain so it cannot leak onto the
	// public routes above. Ordering here IS the security contract: AuthMiddleware must run before
	// RequireTenant, and both before any resource handler.
	protected := rg.Group("")
	authOpts := []middleware.AuthOption{middleware.WithTrustedProxies(cfg.trustedProxies)}
	if cfg.workspaceResolver != nil {
		authOpts = append(authOpts, middleware.WithWorkspaceResolver(cfg.workspaceResolver))
	}
	if cfg.liveUsers != nil {
		// #14 claim-freshness: the Subject every downstream stage reads is rebuilt from the
		// caller's live user row (role/is_admin/status from the DB, not the token).
		protected.Use(middleware.AuthMiddlewareLive(verifier, cfg.liveUsers, cfg.liveSessions, logger, authOpts...))
	} else {
		protected.Use(middleware.AuthMiddleware(verifier, logger, authOpts...))
	}
	protected.Use(middleware.RequireTenant(logger))
	// specs/064 S9-1 PreviewReadOnly: a preview-as-role token may read and may leave, nothing else
	// (403 PREVIEW_READ_ONLY / PREVIEW_FORBIDDEN). Same placement constraint as the throttle below:
	// directly here, before any route or sub-group. Chain order (plan A5, pinned by
	// TestPreviewRouteOrder_ProtectedChain): Auth → RequireTenant → PreviewReadOnly →
	// RequireTenantWritable (specs/068) → DraftCaller (specs/063) → data rate limiter → sub-groups.
	protected.Use(middleware.PreviewReadOnly(protected.BasePath(), logger))
	// specs/068 slice B RequireTenantWritable: a write-frozen tenant answers 503 TENANT_MAINTENANCE to
	// POST/PUT/PATCH/DELETE (reads pass; the closed freezeExempt list excepted). MUST sit directly here —
	// after PreviewReadOnly (its 403 reads no DB), before DraftCaller, the throttle and every sub-group:
	// gin snapshots a group's handlers at registration time, so a later Use() would leave adminGated and
	// the /:entity dispatcher writable while frozen (pinned by TestRouterFreeze_*).
	if cfg.tenantFreeze != nil {
		protected.Use(middleware.RequireTenantWritable(cfg.tenantFreeze, protected.BasePath(), logger))
	}
	// specs/063 DraftCaller: resolves the draft-layer Caller onto c.Request.Context() — never refuses. Same
	// placement constraint (before the throttle and every sub-group, plan §2); pinned by
	// TestDraftCallerRouteOrder.
	protected.Use(middleware.DraftCaller(protected.BasePath()))
	// #52 per-API-key throttle: MUST stay directly here, before any route or sub-group
	// (adminGated, the record group…) is created — gin snapshots a group's handlers at
	// registration time, so a later Use() would leave /:entity silently unthrottled.
	if cfg.dataRateLimiter != nil {
		protected.Use(cfg.dataRateLimiter)
	}

	if h.Metadata != nil {
		h.Metadata.RegisterRoutes(protected)
	}
	// GET /metadata/layouts/:entity — accessible to any authenticated user (not admin-gated).
	if h.Layout != nil {
		h.Layout.RegisterMetadataRoutes(protected)
	}

	// --- ADMIN: Auth → Tenant → RequireAdmin → admin handlers. --------------------------------
	// A2 deferred the /admin/* gate to here (A3), so EVERY /admin route — entities, fields, AND
	// users — sits behind a single is_admin gate. A non-admin principal hitting any /admin path
	// is refused with 403 before the handler runs (../specs/001-auth-security/spec.md §4). The handlers still
	// register their absolute /admin/* paths; mounting them on this gated sub-group (which shares
	// rg's prefix) keeps the paths identical while inserting RequireAdmin into their chain only.
	adminGated := protected.Group("")
	adminGated.Use(middleware.RequireAdmin(logger))
	// EASE Nước 6 (../specs/034-customer-admin-tier/spec.md §2): the BUILDER subset of /admin/* mounts on
	// builderGated — adminGated + the operator tier when wired (WithOperatorTier). Schema,
	// layouts, relationships, packs/install, app composition, doctor, api-keys and the
	// AI-Builder are FACTORY surfaces: a customer's tenant-admin must be able to run their
	// governance (users/roles/teams/field-acl/audit/sso) without being handed the workshop.
	builderGated := adminGated.Group("")
	if cfg.operatorTier != nil {
		builderGated.Use(cfg.operatorTier)
	}
	if h.Admin != nil {
		h.Admin.RegisterRoutes(builderGated)
	}
	// POST/PUT/DELETE /admin/layouts — operator tier (builder surface; IDOR re-pins tenant_id).
	if h.Layout != nil {
		h.Layout.RegisterAdminRoutes(builderGated)
	}
	if h.User != nil {
		h.User.RegisterRoutes(adminGated)
		// specs/065: the operator's owner switch is factory tooling (builderGated).
		h.User.RegisterBuilderRoutes(builderGated)
	}
	// specs/065 lát B: the customer owner's own surface. NOT under adminGated (an owner is not an admin): a
	// sibling group on protected carrying the owner gate. Registered here, BEFORE the dynamic /:entity
	// dispatcher, and only when the gate is wired (fail closed).
	if h.OrgAdmin != nil && cfg.userManagerGate != nil {
		orgGated := protected.Group("")
		orgGated.Use(cfg.userManagerGate)
		h.OrgAdmin.RegisterRoutes(orgGated)
	}
	if h.Role != nil {
		h.Role.RegisterRoutes(adminGated)
	}
	if h.Team != nil {
		h.Team.RegisterRoutes(adminGated)
	}
	// RM-19 audit-log read API (admin-only). Mounted on the admin-gated group so /audit-log[/..]
	// sits behind Auth → Tenant → RequireAdmin like the other admin resources. It is registered
	// BEFORE the dynamic dispatcher so its static /audit-log segments resolve ahead of the
	// catch-all /:entity param routes.
	if h.Audit != nil {
		h.Audit.RegisterRoutes(adminGated)
		// The PER-RECORD history is not an admin surface: whoever may open a record may ask who
		// changed it. Mounted on its own view-ACL sub-group so RequireACLAction resolves the
		// row-level access_level the handler needs (it then re-checks the specific row against
		// the record service and redacts field values the role may not read). Without an
		// aclChecker there is no resolved level, so the handler's scope fails closed — the route
		// still mounts rather than vanishing, which would read as a 404 wiring bug.
		auditRecordGroup := protected
		if aclChecker != nil {
			auditRecordGroup = protected.Group("")
			auditRecordGroup.Use(middleware.RequireACLAction(aclChecker, logger, "view"))
		}
		h.Audit.RegisterRecordRoutes(auditRecordGroup)
	}
	// EPIC 4 workflow admin CRUD (admin-only). Mounted on the admin-gated group so
	// /admin/workflows[/:id] sits behind Auth → Tenant → RequireAdmin like the other admin
	// resources, and BEFORE the dynamic dispatcher so its static segments resolve ahead of the
	// catch-all /:entity param routes.
	if h.Workflow != nil {
		h.Workflow.RegisterRoutes(adminGated)
	}
	// EPIC 6 M2 report admin CRUD (admin-only). Mounted on the admin-gated group so
	// /admin/reports[/:id] sits behind Auth → Tenant → RequireAdmin like the other admin
	// resources, and BEFORE the dynamic dispatcher so its static segments resolve ahead of the
	// catch-all /:entity param routes.
	if h.Report != nil {
		h.Report.RegisterAdminRoutes(adminGated)
	}
	// EPIC 6 Dashboard M2 admin CRUD (admin-only). Mounted on the admin-gated group so
	// /admin/dashboards[/:id] sits behind Auth → Tenant → RequireAdmin like the other admin
	// resources, and BEFORE the dynamic dispatcher so its static segments resolve ahead of the
	// catch-all /:entity param routes.
	if h.Dashboard != nil {
		h.Dashboard.RegisterAdminRoutes(adminGated)
	}
	// Vertical seed-pack admin endpoints (export/install). Mounted on the admin-gated group so
	// /admin/verticals/* sits behind Auth → Tenant → RequireAdmin like the other admin resources,
	// and BEFORE the dynamic dispatcher so its static segments resolve ahead of the catch-all
	// /:entity param routes.
	if h.Vertical != nil {
		h.Vertical.RegisterRoutes(builderGated)
	}
	// TENANT-PROVISION operator-only routes (POST/GET /admin/tenants). RegisterRoutes wraps them in
	// its own RequirePlatformOperator subgroup, so this sits behind Auth → Tenant → RequireAdmin →
	// RequirePlatformOperator — a strictly higher tier than the rest of /admin/*.
	if h.TenantBackup != nil {
		h.TenantBackup.RegisterRoutes(adminGated)
	}
	if h.Tenant != nil {
		h.Tenant.RegisterRoutes(adminGated)
	}
	// specs/062 L5: the project registry (GET/PATCH /admin/projects*), same operator-only tier.
	if h.Project != nil {
		h.Project.RegisterRoutes(adminGated)
	}
	// EPIC 5 BPM approval-process admin CRUD (admin-only). Mounted on the admin-gated group so
	// /admin/approval-processes[/:id] sits behind Auth → Tenant → RequireAdmin like the other
	// admin resources, and BEFORE the dynamic dispatcher so its static segments resolve ahead of
	// the catch-all /:entity param routes.
	if h.Approval != nil {
		h.Approval.RegisterAdminRoutes(adminGated)
	}
	// VIEC-HOM-NAY admin CRUD (/admin/work-queues) — full config-time validation inside.
	if h.WorkQueue != nil {
		h.WorkQueue.RegisterAdminRoutes(adminGated)
	}
	// AO-THUONG-HIEU admin surface (/admin/tenant/branding) — caller tenant only.
	if h.Branding != nil {
		h.Branding.RegisterAdminRoutes(adminGated)
	}
	// CONG-TIN-HIEU admin surface (/admin/inbound-hooks) — full config-time validation inside.
	if h.InboundHook != nil {
		h.InboundHook.RegisterAdminRoutes(adminGated)
	}
	// specs/050 named-credential vault (/admin/credentials) — adminGated, NOT builderGated (plan
	// D-4): the customer admin enters and rotates partner keys without Nexus seeing them.
	if h.Credential != nil {
		h.Credential.RegisterAdminRoutes(adminGated)
	}
	// DONG-TIEN-KHEP-KIN admin surface (/admin/tenant/payment) — caller tenant only.
	if h.Payment != nil {
		h.Payment.RegisterAdminRoutes(adminGated)
	}
	if h.AdminRelation != nil {
		h.AdminRelation.RegisterRoutes(builderGated)
	}
	// ENG-AUTH2 #8: SSO admin provider CRUD (admin-only). Mounted on the admin-gated group so
	// /admin/sso/providers[/:id] sits behind Auth → Tenant → RequireAdmin like the other admin
	// resources.
	if h.SSO != nil {
		h.SSO.RegisterAdminRoutes(adminGated)
	}
	// W7 governance: field-level-security config (GET/PUT/DELETE /admin/field-acl). Admin-gated
	// like every other /admin resource; the rows it manages drive the A6 enforcement on the
	// dynamic record surface.
	if h.FieldACL != nil {
		h.FieldACL.RegisterRoutes(adminGated)
	}
	// E9 AI Builder: POST /admin/ai/builder/chat. Admin-gated like every other /admin resource
	// (a non-admin is refused 403 by RequireAdmin before the agent runs). The route mounts even
	// when AI is disabled — the agent answers 503 AI_DISABLED so the FE keeps an honest-disabled
	// banner rather than a missing route.
	if h.AI != nil {
		h.AI.RegisterRoutes(builderGated)
	}
	// App container admin CRUD + nav writes (/admin/apps[/:id[/nav]]). Mounted on builderGated
	// so it sits behind Auth → Tenant → RequireAdmin → RequirePlatformOperator (EASE Nước 6 —
	// app composition is factory tooling; customer tenant-admins get 403), and BEFORE the
	// dynamic dispatcher so its static segments resolve ahead of the catch-all /:entity routes.
	if h.App != nil {
		h.App.RegisterAdminRoutes(builderGated)
	}
	// Nexus Doctor scan/fix (/admin/doctor/*). Mounted on builderGated so it sits behind
	// Auth → Tenant → RequireAdmin → RequirePlatformOperator (EASE Nước 6, 2026-07-25 — factory
	// tooling is operator-only; customer tenant-admins get 403), and BEFORE the dynamic
	// dispatcher so its static
	// segments resolve ahead of the catch-all /:entity param routes.
	if h.Doctor != nil {
		h.Doctor.RegisterRoutes(builderGated)
	}
	// Machine identity (APIKEY-1): /admin/api-keys CRUD. Admin-gated like every other /admin
	// resource — and note the coarse RequireAdmin gate is exactly why an API key can never
	// manage API keys: keys mint IsAdmin=false by construction.
	if h.APIKey != nil {
		h.APIKey.RegisterRoutes(builderGated)
	}
	// Issue #3: /admin/mcp-tokens, the Studio's "Connect an agent (MCP)": the AI builder's gate.
	if h.MCPToken != nil {
		h.MCPToken.RegisterRoutes(builderGated)
	}
	// Business-hours calendars (CALENDAR-1): /admin/business-calendars CRUD. Admin-gated
	// like every other /admin resource, and BEFORE the dynamic dispatcher so its static
	// segments resolve ahead of the catch-all /:entity param routes.
	if h.Calendar != nil {
		h.Calendar.RegisterRoutes(adminGated)
	}
	// "Việc nền" (/admin/jobs, DAILY-QUEUE #31/#32): adminGated, NEVER builderGated — the persona
	// is the customer's own tenant-admin (not an operator), who would get 403 on the builder tier
	// (pinned by TestAdminJobsList_GateAndTenantPin).
	if h.Jobs != nil {
		h.Jobs.RegisterRoutes(adminGated)
	}
	// "Nhật ký email" (/admin/email-log, specs/066 L1b): same persona, same gate as "Việc nền" —
	// adminGated, NOT builderGated (pinned by TestAdminEmailLogList_GateAndTenantPin).
	if h.EmailLog != nil {
		h.EmailLog.RegisterRoutes(adminGated)
	}
	if h.Usage != nil {
		h.Usage.RegisterRoutes(adminGated)
	}
	if h.Health != nil {
		h.Health.RegisterRoutes(adminGated)
	}
	// specs/064 S9-4: seeding / counting sample data is factory tooling — builderGated.
	if h.SampleData != nil {
		h.SampleData.RegisterAdminRoutes(builderGated)
	}
	// specs/064 S9-1: opening a preview-as-role session is factory tooling — builderGated.
	if h.Preview != nil {
		h.Preview.RegisterAdminRoutes(builderGated)
	}
	// specs/063-draft-publish: the builder's draft -> publish lifecycle — factory tooling, builderGated.
	if h.Publish != nil {
		h.Publish.RegisterRoutes(builderGated)
	}
	// specs/062 L1b: open / close the builder's workspace session inside a customer. builderGated
	// (a customer-side admin / owner holding a plain token gets the same 403 as every builder route —
	// no distinguishable 404), registered BEFORE the dynamic /:entity dispatcher.
	if h.WorkspaceSession != nil {
		h.WorkspaceSession.RegisterRoutes(builderGated)
	}

	// The dynamic dispatcher (/:entity[/:id]) is mounted LAST on the (non-admin-gated) protected
	// group so its catch-all param routes do not shadow the static /metadata and /admin paths
	// registered above (gin resolves the static segments first; registering them ahead of the
	// param routes keeps that resolution stable). It is NOT under RequireAdmin: dynamic-record
	// CRUD is governed by per-action RBAC (A4), not the coarse admin gate.
	//
	// EPIC 6 M2 report RUN + export endpoints (GET /reports/:id/run, /reports/:id/export.csv):
	// mounted on the protected (non-admin) group BEFORE the dynamic dispatcher so its static
	// "reports" segment resolves ahead of the catch-all /:entity param route. Running a report
	// returns tenant + A5 row-scoped DATA, so it is available to any authenticated user (not
	// admin-gated). RequireACL can NOT run here (it reads the :entity URL param, but a report's
	// entity is stored on the report record, not the URL), so the report SERVICE resolves the
	// caller's "view" ACL on the report entity itself (REPORT-RUN-ACL, RecordService.AuthorizeView —
	// mirroring the ⌘K assistant's ACL-by-construction): a caller WITHOUT "view" on the entity gets
	// 403 FORBIDDEN before any data query, an admin sees all rows, and a scoped user sees only their
	// A5 rows (owner/team/all).
	if h.Report != nil {
		h.Report.RegisterRunRoutes(protected)
	}
	// EPIC 6 Dashboard M2 RUN endpoint (GET /dashboards/:id/run): mounted on the protected
	// (non-admin) group BEFORE the dynamic dispatcher so its static "dashboards" segment resolves
	// ahead of the catch-all /:entity param route. Available to any authenticated user (not
	// admin-gated). Like the report run route it is OUTSIDE /:entity RequireACL, so the dashboard
	// service resolves EACH widget's entity "view" ACL itself (RecordService.AuthorizeView,
	// per-widget — DASHBOARD-RUN-ACL): an admin sees every widget, a scoped user sees its A5 rows,
	// and a widget whose entity the caller cannot view returns a per-widget FORBIDDEN (it does not
	// kill sibling widgets; the dashboard HTTP status stays 200 — per-widget errors are body-level).
	if h.Dashboard != nil {
		h.Dashboard.RegisterRunRoutes(protected)
	}

	// File-upload M1 download/delete (GET/DELETE /files/:id): mounted on the protected (non-admin)
	// group BEFORE the dynamic dispatcher so the static "files" segment resolves ahead of the
	// catch-all /:entity param route. Authorization (tenant + A5 against the parent record) is
	// enforced inside the file service, so these are available to any authenticated user — a
	// caller who cannot read the parent record gets a 404, exactly like the record GET.
	if h.File != nil {
		h.File.RegisterFileRoutes(protected)
	}

	// EPIC 5 BPM user approval surface (GET /approvals/pending, POST /approvals/:id/approve|reject):
	// mounted on the protected (non-admin) group BEFORE the dynamic dispatcher so the static
	// "approvals" segment resolves ahead of the catch-all /:entity param route. Available to any
	// authenticated user — the approval service enforces that the caller is the current step's
	// approver (a non-approver is refused inside the service, not by the coarse admin gate).
	if h.Approval != nil {
		h.Approval.RegisterUserRoutes(protected)
	}

	// P0 FE-binding: auth protected routes (PUT /auth/me/password). Mounted on the protected group
	// so the Auth → Tenant chain populates the verified caller identity before the handler runs.
	// Must be BEFORE the dynamic dispatcher so /auth/me/password is not swallowed by /:entity.
	if h.Auth != nil {
		h.Auth.RegisterProtectedRoutes(protected)
	}
	// specs/064 S9-1: DELETE /preview-sessions/current — the preview token leaves its session. On the
	// protected group (a preview token is not admin) BEFORE the dynamic dispatcher so the static
	// segment is not read as /:entity/:id.
	if h.Preview != nil {
		h.Preview.RegisterRoutes(protected)
	}

	// P0 FE-binding: notification read surface (GET /notifications, GET /notifications/unread-count,
	// POST /notifications/:id/read, POST /notifications/read-all). Mounted on the protected (non-admin)
	// group so any authenticated user can reach their own notifications. BEFORE the dynamic dispatcher
	// so the static "notifications" segment resolves ahead of the /:entity catch-all.
	if h.Notification != nil {
		h.Notification.RegisterRoutes(protected)
	}

	// VIEC-HOM-NAY: end-user queue cards (GET /work-queues). Protected (non-admin) group, BEFORE
	// the dynamic dispatcher so the static segment resolves ahead of /:entity. Per-queue entity
	// ACL + row scope are enforced inside the service (ACL-by-construction).
	if h.WorkQueue != nil {
		h.WorkQueue.RegisterUserRoutes(protected)
	}
	// specs/043 S5: the work-item inbox. Static "work-items" segment on the protected group BEFORE
	// the /:entity dispatcher (the hyphen also keeps it clear of any tenant entity named "tasks").
	// Who may see or act on an item is the work seat's rule, judged inside the service (a stranger
	// gets 404 WORK_ITEM_NOT_FOUND).
	if h.WorkItem != nil {
		h.WorkItem.RegisterUserRoutes(protected)
	}
	// SEARCH-UNIFIED (3.5): GET /search on the protected group BEFORE the /:entity dispatcher
	// (static segment first). Per-entity ACL + row scope are enforced inside the service.
	if h.Search != nil {
		h.Search.RegisterRoutes(protected)
	}

	// GET /users/directory (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b): mounted on the protected
	// (non-admin) group BEFORE the dynamic dispatcher so the static "users" segment resolves
	// ahead of the /:entity catch-all (same precedent as /notifications, /search, /apps). Any
	// authenticated user of the tenant may call it — tenant scoping alone is the boundary, no ACL.
	if h.UserDirectory != nil {
		h.UserDirectory.RegisterRoutes(protected)
	}

	// GET /tenant/payment-status: same reasoning as /users/directory — a static segment on the
	// protected group, ahead of the /:entity catch-all, tenant-scoped with no ACL. It lets a
	// NON-admin find out whether the QR button should appear; the config itself stays admin-only.
	if h.Payment != nil {
		h.Payment.RegisterStatusRoute(protected)
	}

	// P0 FE-binding: non-admin dashboard catalog (GET /dashboards). Mounted on the protected group
	// BEFORE the dynamic dispatcher; complements GET /dashboards/:id/run already registered above.
	if h.Dashboard != nil {
		h.Dashboard.RegisterCatalogRoutes(protected)
	}

	// ⌘K end-user data assistant (POST /assistant/ask): mounted on the protected (non-admin)
	// group BEFORE the dynamic dispatcher so the static "assistant" segment resolves ahead of the
	// /:entity catch-all (same precedent as /notifications, /reports). Deliberately NOT behind
	// RequireAdmin: the agent's tools enforce the caller's own entity ACL + A5 row scope on every
	// read, so the endpoint exposes exactly what normal browsing already exposes.
	if h.Assistant != nil {
		// Thread the per-user limiter only when wired (nil-safe: a nil handler is dropped so the
		// route isn't mounted behind a no-op that would still count as middleware).
		var assistantMW []gin.HandlerFunc
		if cfg.assistantRateLimiter != nil {
			assistantMW = append(assistantMW, cfg.assistantRateLimiter)
		}
		h.Assistant.RegisterRoutes(protected, assistantMW...)
	}

	// P0 FE-binding: non-admin report catalog (GET /reports). Mounted on the protected group
	// BEFORE the dynamic dispatcher; complements GET /reports/:id/run already registered above.
	if h.Report != nil {
		h.Report.RegisterCatalogRoutes(protected)
	}

	// App Switcher reads (GET /apps, GET /apps/:name/nav): mounted on the protected (non-admin)
	// group BEFORE the dynamic dispatcher so the static "apps" segment resolves ahead of the
	// /:entity catch-all (same precedent as /dashboards, /reports). Not admin-gated — every
	// authenticated user needs the Switcher; the nav read filters entity items through the
	// caller's own entity ACL inside the service (INV-3).
	if h.App != nil {
		h.App.RegisterReadRoutes(protected)
	}

	// W9b saved views (GET/POST /:entity/views, PUT/DELETE /:entity/views/:id). Entity-scoped, so
	// they ride a DEDICATED group forcing action="view": a caller needs the SAME entity-level view
	// grant as the list surface a view is applied to (GET→"view" is the natural mapping, but the
	// routes carry mixed methods POST/PUT/DELETE that would otherwise map to create/edit/delete — a
	// saved UI preset must not require record-write permission, so the action is pinned to "view").
	// The shared-vs-personal ownership gate is enforced on top, inside ViewService. When no
	// aclChecker is wired (some test harnesses), the group is the plain protected group — identical
	// to how recordGroup/restoreGroup fall back. Registered BEFORE the dynamic dispatcher so the
	// static "views" segment resolves ahead of the /:entity/:id catch-all.
	if h.SavedView != nil {
		viewGroup := protected
		if aclChecker != nil {
			viewGroup = protected.Group("")
			viewGroup.Use(middleware.RequireACLAction(aclChecker, logger, "view"))
		}
		h.SavedView.RegisterRoutes(viewGroup)
	}

	// A4 ACL STAGE: when an aclChecker is supplied, a dedicated sub-group inserts RequireACL
	// (Auth → Tenant → ACL → Business Logic, spec §4) so every /:entity request is authorized per
	// (method→action) against the role's grants with strict default-deny (is_admin bypasses).
	// /admin/* is intentionally NOT under this stage — it keeps its coarse RequireAdmin gate.
	if h.Record != nil || h.Import != nil {
		recordGroup := protected
		// editGroup pins action="edit" for the POST routes that act on an EXISTING record — the
		// default POST→"create" of recordGroup is the wrong question for them. Same fallback as
		// recordGroup when no aclChecker is wired (some test harnesses): the plain protected group,
		// where scopeOf still fails closed to AccessNone for non-admins.
		editGroup := protected
		if aclChecker != nil {
			recordGroup = protected.Group("")
			recordGroup.Use(middleware.RequireACL(aclChecker, logger))
			editGroup = protected.Group("")
			editGroup.Use(middleware.RequireACLAction(aclChecker, logger, "edit"))
		}
		// Import routes must be registered BEFORE the dynamic record dispatcher so gin sees
		// the static "/import" segment before the /:id wildcard. (Gin already resolves static
		// before param, but registration order is the explicit contract here.)
		if h.Import != nil {
			h.Import.RegisterRoutes(recordGroup)
		}
		// Record dispatcher is now nil-guarded: the outer block also fires when only Import is
		// set, so an unguarded h.Record call would panic when Record is nil.
		if h.Record != nil {
			h.Record.RegisterRoutes(recordGroup)
			// DONG-TIEN-KHEP-KIN: payment QR rides the SAME view-gated group — only a caller
			// who can see the record can mint its QR.
			if h.Payment != nil {
				h.Payment.RegisterRecordRoute(recordGroup)
			}
			// W9b recycle-bin restore (POST /:entity/:id/restore): mounted on a DEDICATED group
			// forcing action="delete" — restore is the inverse of delete and must require the
			// SAME grant, NOT the default POST→"create" the recordGroup's RequireACL would apply.
			// GET /:entity/deleted stays on recordGroup (GET→"view", mirroring the live list); only
			// this POST needs the action override. When no aclChecker is wired (some test harnesses),
			// the restore group is the plain protected group — identical to how recordGroup falls
			// back — so the record handler's scopeOf still fails closed to AccessNone for non-admins.
			restoreGroup := protected
			if aclChecker != nil {
				restoreGroup = protected.Group("")
				restoreGroup.Use(middleware.RequireACLAction(aclChecker, logger, "delete"))
			}
			h.Record.RegisterRestoreRoutes(restoreGroup)
			// specs/056 recycle-bin PURGE (POST /:entity/:id/purge) — permanent, so it is NOT on
			// restoreGroup: an entity "delete" grant is what a customer's staff hold, and purge is a
			// Nexus-operator act (Q5b 2026-09-05). Auth → Tenant → RequireAdmin → operator tier.
			// FAIL-CLOSED: without WithOperatorTier the route is not registered at all (404) — it never
			// degrades to "any admin may purge" the way the builder subset degrades to RequireAdmin.
			if cfg.operatorTier != nil {
				purgeGroup := protected.Group("")
				purgeGroup.Use(middleware.RequireAdmin(logger), cfg.operatorTier)
				h.Record.RegisterPurgeRoute(purgeGroup)
			}
			// ENG-MASSOP mass delete (POST /:entity/bulk-delete) requires the SAME "delete"
			// grant as DELETE /:entity/:id, so it rides the restore group's fixed action.
			h.Record.RegisterBulkDeleteRoute(restoreGroup)
			// ENG-MASSOP mass update (POST /:entity/bulk-update): the default POST→"create"
			// mapping is wrong here — it must require the "edit" grant, exactly like
			// PUT /:entity/:id — so it rides editGroup.
			h.Record.RegisterBulkUpdateRoute(editGroup)
			// specs/051 manual record shares: list/share/revoke all require the "edit" grant — Q4a,
			// whoever may EDIT the record manages who else may VIEW it — so they ride the SAME
			// edit-pinned group as bulk-update (GET included: the list names colleagues, a read-only
			// viewer has no need for it). shares.Service re-resolves "edit" and reads the record
			// under the caller's edit scope anyway, so this mount is defense in depth.
			if h.RecordShare != nil {
				h.RecordShare.RegisterRoutes(editGroup)
			}
		}
		// File-upload M1 upload (POST /:entity/:id/files) rides editGroup: attaching a file writes
		// the parent record's file-ref array, so the gate is "may this person change this record",
		// not POST→"create". specs/060 (#269): on recordGroup it asked for create here and the
		// service asked for edit, so a technician with edit (own) and no create got 403 on every
		// photo, and granting create to unblock it also let them open new orders. The service still
		// re-resolves "edit" + the row scope itself (files.Service.Upload step 0) — keep both.
		if h.File != nil {
			h.File.RegisterEntityRoutes(editGroup)
		}
		// N-N relationship endpoints (the source entity is the same :entity param). Link (POST) and
		// unlink (DELETE) write the SOURCE record's relationships, so the gate is "may this person
		// change this record": they ride editGroup, NOT the method-mapped recordGroup (POST→create,
		// DELETE→delete). specs/060 (#351): on recordGroup a role holding create:all + edit:own reached
		// RelationService.Link with row scope "all" and could attach links to a colleague's record.
		// The service re-resolves "edit" + the row scope itself (RelationService.sourceEditScope) —
		// keep both. The read (GET → "view") stays on recordGroup.
		if h.Relation != nil {
			h.Relation.RegisterRelationReadRoutes(recordGroup)
			h.Relation.RegisterRelationWriteRoutes(editGroup)
		}
		// P0 FE-binding: per-record approval history (GET /:entity/:id/approvals). Shares the
		// same RequireACL gate (view on :entity) as the record GET, so only users with read
		// access to the entity may inspect its approval instances.
		if h.Approval != nil {
			h.Approval.RegisterEntityRoutes(recordGroup)
			// "Gửi duyệt" rides editGroup, not the method-mapped recordGroup: RequireACL maps
			// POST→"create", which is the wrong question here. Submitting is an act on an EXISTING
			// record's lifecycle, so the right floor is "may this person change this record" — a
			// read-only viewer must not be able to start a workflow on someone else's work. The
			// service then re-checks the specific ROW through RecordService.Get, because
			// entity-level edit is not row-level permission.
			h.Approval.RegisterSubmitRoute(editGroup)
		}
		// RECORD-NOTES: list, create and delete ALL ride ONE group pinned to "view" — the floor is
		// "whoever can read the record can comment on it" (specs/060 #349, Founder 2026-10-09), NOT
		// the method-mapped recordGroup: there POST→"create" asked "may this person open a NEW
		// record", so a technician with view/edit (own) and no create could not note their own work
		// order. Delete needs the same weaker floor (rule "note author OR entity delete-grant" lives
		// in notes.Service.Delete; DELETE→"delete" would block an author holding only view). The
		// load-bearing checks are in the service, which re-resolves view + row scope via
		// ResolveNoteTarget (internal/services/record_note.go) and, because a "view" scope carries
		// IncludeShares, refuses a write from a caller who reaches the record ONLY through a manual
		// share (specs/051) — so this mount is defense in depth. Falls back to the plain protected
		// group when no aclChecker is wired, identical to restoreGroup/editGroup above.
		if h.Note != nil {
			noteGroup := protected
			if aclChecker != nil {
				noteGroup = protected.Group("")
				noteGroup.Use(middleware.RequireACLAction(aclChecker, logger, "view"))
			}
			h.Note.RegisterEntityRoutes(noteGroup)
			h.Note.RegisterDeleteRoute(noteGroup)
		}
	}
}
