package services

// preview_service.go — specs/064-preview-as-role-sample-data S9-1 (DAILY-QUEUE #393, #298 part 1):
// a builder previews the customer app AS ONE ROLE. The token is its own signed type
// (auth.TokenTypePreview), acts as a synthetic principal UUIDv5(tenant|role) with no users row and
// IsAdmin=false, and is re-checked on EVERY request against its sys_preview_sessions row and the
// builder's live login session (auth.WorkspaceSessionRepository.ActorLive) — no cache anywhere, so
// logout, ending the session, a password change, the E5 reuse alarm, a demotion, a lock or the
// operator flag removed by CLI all kill it on the very next request.
//
// ponytail: no cache of the session row or of ActorLive — two key reads under 1 ms each; a cache
// would only delay revocation. No refresh token, no "extend": when it expires the builder reopens it.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// previewNamespace is the fixed UUIDv5 namespace of preview principals. A v5 id differs from the
// v4 ids of real users in its version nibble, so a principal can never equal a real user id.
var previewNamespace = uuid.MustParse("6a3f2c1e-8b4d-4e7a-9c0f-5d2b1a7e3c64")

// PreviewPrincipal is the synthetic subject of a preview session: fixed per (tenant, role),
// different across tenants and roles.
func PreviewPrincipal(tenantID, roleID string) string {
	return uuid.NewSHA1(previewNamespace, []byte(tenantID+"|"+roleID)).String()
}

// PreviewEndReasons are the only end_reason values End accepts: the builder pressed "Thoát"
// (exit) or opened Studio in the preview tab (left).
var PreviewEndReasons = map[string]bool{"exit": true, "left": true}

var (
	// ErrPreviewForbidden: the caller may not open a preview session — a token without `sid`, or a
	// builder whose login session / admin / operator flag is no longer live (403 FORBIDDEN).
	ErrPreviewForbidden = errors.New("services: preview session forbidden")
	// ErrPreviewUnauthorized: a preview token that no longer stands (row gone/ended/expired/
	// mismatched, builder not live). The auth stage answers the uniform 401.
	ErrPreviewUnauthorized = errors.New("services: preview token not valid")
)

// previewSessionStore is the persistence PreviewService needs (auth.PreviewSessionRepository).
type previewSessionStore interface {
	Create(ctx context.Context, s *auth.PreviewSession) error
	GetByID(ctx context.Context, tenantID, id string) (*auth.PreviewSession, error)
	End(ctx context.Context, tenantID, id, reason string, now time.Time) (bool, error)
}

// previewActorChecker is auth.WorkspaceSessionRepository.ActorLive (specs/062 shared method).
type previewActorChecker interface {
	ActorLive(ctx context.Context, att, act, sid string, now time.Time) (*auth.LiveActor, error)
}

// previewWorkspaceChecker is auth.WorkspaceSessionRepository.GetLive (specs/062 L1): "is this builder
// workspace session still open, inside its hard deadline, with the customer tenant active?".
type previewWorkspaceChecker interface {
	GetLive(ctx context.Context, tenantID, id, act, att, sid string, now time.Time) (*auth.WorkspaceSessionLive, error)
}

// previewRoleReader resolves the (tenant, role) being previewed (auth.RoleRepository).
type previewRoleReader interface {
	GetRoleByID(ctx context.Context, tenantID, id string) (*auth.Role, error)
}

// previewTokens mints and verifies the preview JWT (auth.TokenService).
type previewTokens interface {
	IssuePreviewToken(sub auth.Subject, exp time.Time) (string, error)
	VerifyPreviewToken(tok string) (*auth.Claims, error)
}

// previewDeliveredChecker / previewDraftCounter are the two facts S9-9 needs: is the workspace
// delivered (*DraftGate.Delivered) and does it have pending drafts (*metaversion.DraftRepo.Count).
type previewDeliveredChecker interface {
	Delivered(ctx context.Context, tenantID string) (bool, error)
}
type previewDraftCounter interface {
	Count(ctx context.Context, tenantID string) (int, error)
}

// PreviewService opens, verifies and ends preview-as-role sessions.
type PreviewService struct {
	sessions previewSessionStore
	actors   previewActorChecker
	// workspaces is nil until WithWorkspaceSessions; a row that names a workspace session while it is
	// nil is refused (fail closed) — never previewed without the check.
	workspaces previewWorkspaceChecker
	roles      previewRoleReader
	tokens     previewTokens
	delivery   previewDeliveredChecker // S9-9; nil = no draft layer wired, a draft preview is never available
	drafts     previewDraftCounter
	ttl        time.Duration
	auditor    audit.Auditor
	logger     *zap.Logger
	now        func() time.Time
}

// NewPreviewService wires the service. ttl is PREVIEW_TTL_MINUTES (config.LoadPreviewConfig).
func NewPreviewService(sessions previewSessionStore, actors previewActorChecker, roles previewRoleReader, tokens previewTokens, ttl time.Duration) *PreviewService {
	return &PreviewService{
		sessions: sessions, actors: actors, roles: roles, tokens: tokens, ttl: ttl,
		auditor: audit.NewNoopAuditor(), logger: zap.NewNop(), now: time.Now,
	}
}

// WithAuditor sets the audit sink for the start/end rows (__preview__, async, fail-open).
func (s *PreviewService) WithAuditor(a audit.Auditor) *PreviewService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithWorkspaceSessions wires the workspace-session check of S9-8 (preview inside a customer's
// workspace): Start and every Verify of a session opened there ask GetLive, uncached.
func (s *PreviewService) WithWorkspaceSessions(w previewWorkspaceChecker) *PreviewService {
	s.workspaces = w
	return s
}

// WithDrafts wires the draft layer probe (S9-9): a session may preview the draft only when the
// tenant is delivered AND has pending drafts.
func (s *PreviewService) WithDrafts(d previewDeliveredChecker, c previewDraftCounter) *PreviewService {
	s.delivery, s.drafts = d, c
	return s
}

// WithLogger sets the structured logger.
func (s *PreviewService) WithLogger(l *zap.Logger) *PreviewService {
	if l != nil {
		s.logger = l
	}
	return s
}

// PreviewStartInput is the POST /admin/preview-sessions body after binding.
type PreviewStartInput struct {
	RoleID string
	AppID  string
	// Draft: nil = default (on when the workspace is delivered and has drafts); true = must be
	// available else 422 PREVIEW_DRAFT_UNAVAILABLE; false = the running version.
	Draft *bool
}

// PreviewRole names the role the session previews.
type PreviewRole struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Label string `json:"label"`
}

// PreviewStartResult is the 201 body of POST /admin/preview-sessions.
type PreviewStartResult struct {
	SessionID string      `json:"sessionId"`
	Token     string      `json:"token"`
	ExpiresAt time.Time   `json:"expiresAt"`
	Role      PreviewRole `json:"role"`
	AppID     *string     `json:"appId"`
	Draft     bool        `json:"draft"`
}

// Start opens a preview session for one role of the builder's tenant and mints its token.
//
// Order: the builder must be LIVE first (a token with no `sid` or a dead login session is refused
// with ErrPreviewForbidden — the route gate already required admin + operator, this re-checks it
// in the service so a harness without the operator tier is still safe); then the input; then the
// role, pinned to the tenant. The row is written synchronously BEFORE the token is signed, so every
// token that exists has its trail. A signing failure leaves an orphan row that expires on its own.
func (s *PreviewService) Start(ctx context.Context, builder auth.Subject, in PreviewStartInput) (*PreviewStartResult, error) {
	if builder.SessionID == "" {
		return nil, ErrPreviewForbidden
	}
	now := s.now()
	// Inside a customer's workspace (S9-8) the token is a workspace-session token: tid is the CUSTOMER,
	// the builder's own workshop is att and the session is wsid. The builder is then checked in att, not
	// in tid, and the workspace session itself is checked too (the Auth stage just did, this request —
	// the service re-checks so it never depends on the route gate alone). Any partial set of the
	// delegation claims is not a workspace session at all.
	actorTenant := builder.TenantID
	var wsID *string
	if builder.HasDelegationClaims() {
		if !builder.DelegationShapeOK() || s.workspaces == nil {
			return nil, ErrPreviewForbidden
		}
		actorTenant = builder.ActorTenantID
		wsID = &builder.WorkspaceSessionID
	}
	if _, err := s.actors.ActorLive(ctx, actorTenant, builder.UserID, builder.SessionID, now); err != nil {
		if errors.Is(err, auth.ErrActorNotLive) {
			return nil, ErrPreviewForbidden
		}
		return nil, fmt.Errorf("services.PreviewService.Start: %w", err)
	}
	if wsID != nil {
		if _, err := s.workspaces.GetLive(ctx, builder.TenantID, *wsID, builder.UserID, actorTenant, builder.SessionID, now); err != nil {
			if errors.Is(err, auth.ErrWorkspaceSessionNotLive) {
				return nil, ErrPreviewForbidden
			}
			return nil, fmt.Errorf("services.PreviewService.Start: %w", err)
		}
	}
	var appID *string
	if in.AppID != "" {
		if _, err := uuid.Parse(in.AppID); err != nil {
			return nil, invalid("INVALID_BODY", "appId không hợp lệ")
		}
		appID = &in.AppID
	}
	draft, err := s.resolveDraft(ctx, builder.TenantID, in.Draft)
	if err != nil {
		return nil, err
	}
	role, err := s.roles.GetRoleByID(ctx, builder.TenantID, in.RoleID)
	if errors.Is(err, auth.ErrRoleNotFound) {
		return nil, invalid("PREVIEW_ROLE_NOT_FOUND", "Không tìm thấy vai này trong không gian hiện tại")
	}
	if err != nil {
		return nil, fmt.Errorf("services.PreviewService.Start: %w", err)
	}

	row := &auth.PreviewSession{
		ID:             uuid.NewString(),
		TenantID:       builder.TenantID,
		RoleID:         role.ID,
		PrincipalID:    PreviewPrincipal(builder.TenantID, role.ID),
		ActorUserID:    builder.UserID,
		ActorTenantID:  actorTenant,
		ActorSessionID: builder.SessionID,
		// Named only on the row, never on the token: the preview Subject stays free of wsid.
		WorkspaceSessionID: wsID,
		AppID:              appID,
		Draft:              draft,
		ExpiresAt:          now.Add(s.ttl),
		CreatedBy:          builder.UserID,
	}
	if err := s.sessions.Create(ctx, row); err != nil {
		return nil, fmt.Errorf("services.PreviewService.Start: %w", err)
	}
	tok, err := s.tokens.IssuePreviewToken(auth.Subject{
		UserID:        row.PrincipalID,
		TenantID:      row.TenantID,
		RoleID:        row.RoleID,
		SessionID:     row.ID,
		ActorUserID:   row.ActorUserID,
		ActorTenantID: row.ActorTenantID,
		PreviewDraft:  draft,
	}, row.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("services.PreviewService.Start: %w", err)
	}

	s.auditor.Record(ctx, audit.Event{
		TenantID:    row.TenantID,
		ActorUserID: builder.UserID,
		EntityName:  audit.PreviewMeta,
		RecordID:    row.ID,
		Action:      audit.ActionCreate,
		Diff: &audit.Diff{After: map[string]any{
			"roleId": role.ID, "roleName": previewRoleName(role), "expiresAt": row.ExpiresAt.UTC().Format(time.RFC3339),
			"appId": appID, "draft": draft,
		}},
		At: now,
	})
	s.logger.Info("services.PreviewService.Start: preview session opened",
		zap.String("tenant_id", row.TenantID), zap.String("user_id", builder.UserID),
		zap.String("role_id", role.ID), zap.String("preview_session_id", row.ID))

	return &PreviewStartResult{
		SessionID: row.ID,
		Token:     tok,
		ExpiresAt: row.ExpiresAt.UTC(),
		Role:      PreviewRole{ID: role.ID, Name: role.Name, Label: role.Label},
		AppID:     appID,
		Draft:     draft,
	}, nil
}

// resolveDraft decides the draft flag of a new session for the builder's tenant (specs/064 S9-9,
// FR-019). Explicit false never touches the draft layer. Otherwise the workspace must be delivered
// and have pending drafts: then nil defaults to ON; when it is not available nil defaults to OFF and
// an explicit true is refused with 422 PREVIEW_DRAFT_UNAVAILABLE. A probe failure is an error, never
// a silent "no draft".
func (s *PreviewService) resolveDraft(ctx context.Context, tenantID string, ask *bool) (bool, error) {
	if ask != nil && !*ask {
		return false, nil
	}
	available := false
	if s.delivery != nil && s.drafts != nil {
		delivered, err := s.delivery.Delivered(ctx, tenantID)
		if err != nil {
			return false, fmt.Errorf("services.PreviewService.Start: %w", err)
		}
		if delivered {
			n, err := s.drafts.Count(ctx, tenantID)
			if err != nil {
				return false, fmt.Errorf("services.PreviewService.Start: %w", err)
			}
			available = n > 0
		}
	}
	if !available && ask != nil {
		return false, invalid("PREVIEW_DRAFT_UNAVAILABLE", "Dự án chưa có bản nháp để xem")
	}
	return available, nil
}

// previewRoleName is the human name of the role (label, falling back to the machine name).
func previewRoleName(r *auth.Role) string {
	if r.Label != "" {
		return r.Label
	}
	return r.Name
}

// Verify is the preview half of the Bearer verifier (services.CompositeVerifier.WithPreview). It
// returns claims REBUILT from the session row — IsAdmin=false and IsPlatformOperator=false are
// set here and nowhere else, WorkspaceSessionID is never set — after checking, uncached:
//
//  1. signature, expiry and typ=preview;
//  2. the (tenant, sid) row exists, is not ended, not expired, and agrees with the claims
//     (role, principal, builder, builder tenant);
//  3. the builder is live: ActorLive(actor_tenant_id, actor_user_id, actor_session_id) — the
//     builder's LOGIN sid from the row, never the token's own sid claim (= the preview-session id).
//
// Any failure is an error (the auth stage answers the uniform 401); a DB error is returned wrapped
// and never lets the request through.
func (s *PreviewService) Verify(ctx context.Context, tok string) (*auth.Claims, error) {
	claims, _, _, err := s.verify(ctx, tok)
	return claims, err
}

// verify is Verify plus the session row and the live builder it already loaded, so Profile reuses
// them instead of a second read (and cannot drift from the request-path checks).
func (s *PreviewService) verify(ctx context.Context, tok string) (*auth.Claims, *auth.PreviewSession, *auth.LiveActor, error) {
	claims, err := s.tokens.VerifyPreviewToken(tok)
	if err != nil {
		return nil, nil, nil, err
	}
	row, err := s.sessions.GetByID(ctx, claims.TenantID, claims.SessionID)
	if errors.Is(err, auth.ErrPreviewSessionNotFound) {
		return nil, nil, nil, fmt.Errorf("%w: session not found", ErrPreviewUnauthorized)
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("services.PreviewService.Verify: %w", err)
	}
	now := s.now()
	switch {
	case row.EndedAt != nil:
		return nil, nil, nil, fmt.Errorf("%w: session ended", ErrPreviewUnauthorized)
	case !row.ExpiresAt.After(now):
		return nil, nil, nil, fmt.Errorf("%w: session expired", ErrPreviewUnauthorized)
	case row.RoleID != claims.RoleID, row.PrincipalID != claims.UserID,
		row.ActorUserID != claims.ActorUserID, row.ActorTenantID != claims.ActorTenantID,
		row.Draft != claims.PreviewDraft:
		return nil, nil, nil, fmt.Errorf("%w: claims do not match the session", ErrPreviewUnauthorized)
	}
	actor, err := s.actors.ActorLive(ctx, row.ActorTenantID, row.ActorUserID, row.ActorSessionID, now)
	if err != nil {
		if errors.Is(err, auth.ErrActorNotLive) {
			return nil, nil, nil, fmt.Errorf("%w: builder not live", ErrPreviewUnauthorized)
		}
		return nil, nil, nil, fmt.Errorf("services.PreviewService.Verify: %w", err)
	}
	if row.WorkspaceSessionID != nil {
		// Opened inside a customer's workspace: the builder's workspace session must still be open too
		// ("Về Nexus", its hard deadline, a suspended customer all end the preview on the next request).
		if s.workspaces == nil {
			return nil, nil, nil, fmt.Errorf("%w: workspace check not wired", ErrPreviewUnauthorized)
		}
		if _, err := s.workspaces.GetLive(ctx, row.TenantID, *row.WorkspaceSessionID, row.ActorUserID, row.ActorTenantID, row.ActorSessionID, now); err != nil {
			if errors.Is(err, auth.ErrWorkspaceSessionNotLive) {
				return nil, nil, nil, fmt.Errorf("%w: workspace session not live", ErrPreviewUnauthorized)
			}
			return nil, nil, nil, fmt.Errorf("services.PreviewService.Verify: %w", err)
		}
	}
	return &auth.Claims{
		Subject: auth.Subject{
			UserID:        row.PrincipalID,
			TenantID:      row.TenantID,
			RoleID:        row.RoleID,
			IsAdmin:       false,
			SessionID:     row.ID,
			ActorUserID:   row.ActorUserID,
			ActorTenantID: row.ActorTenantID,
			PreviewDraft:  row.Draft,
		},
		TokenType:        auth.TokenTypePreview,
		RegisteredClaims: claims.RegisteredClaims,
	}, row, actor, nil
}

// PreviewInfo is the `preview` block of GET /auth/me for a preview token (specs/064 S9-2): what the
// banner shows ("Đang xem như <vai>", builder name, minutes left) and what the FE needs to end it.
type PreviewInfo struct {
	SessionID string    `json:"sessionId"`
	RoleID    string    `json:"roleId"`
	RoleName  string    `json:"roleName"`
	ActorName string    `json:"actorName"`
	ExpiresAt time.Time `json:"expiresAt"`
	Draft     bool      `json:"draft"`
}

// Profile answers GET /auth/me for a preview token: a synthetic, non-admin profile of the previewed
// role. It runs the SAME uncached checks as Verify (one shared code path), so an ended / expired /
// builder-no-longer-live session is a 401 here exactly as on a data request. Any "this token does
// not stand" outcome is ErrPreviewUnauthorized; a DB error is returned wrapped (500, never a pass).
func (s *PreviewService) Profile(ctx context.Context, tok string) (*LoginUser, error) {
	claims, row, actor, err := s.verify(ctx, tok)
	if err != nil {
		if errors.Is(err, auth.ErrTokenMalformed) || errors.Is(err, auth.ErrTokenExpired) ||
			errors.Is(err, auth.ErrTokenInvalid) || errors.Is(err, auth.ErrWrongTokenType) {
			return nil, fmt.Errorf("%w: %v", ErrPreviewUnauthorized, err)
		}
		return nil, err
	}
	role, err := s.roles.GetRoleByID(ctx, row.TenantID, row.RoleID)
	if errors.Is(err, auth.ErrRoleNotFound) {
		return nil, fmt.Errorf("%w: role no longer exists", ErrPreviewUnauthorized)
	}
	if err != nil {
		return nil, fmt.Errorf("services.PreviewService.Profile: %w", err)
	}
	name := previewRoleName(role)
	actorName := actor.FullName
	if actorName == "" {
		actorName = actor.Email
	}
	op := false
	return &LoginUser{
		ID:                 claims.UserID,
		TenantID:           claims.TenantID,
		FullName:           "Xem như: " + name,
		IsAdmin:            false,
		RoleID:             role.ID,
		IsPlatformOperator: &op,
		Preview: &PreviewInfo{
			SessionID: row.ID, RoleID: role.ID, RoleName: name, ActorName: actorName,
			ExpiresAt: row.ExpiresAt.UTC(), Draft: row.Draft,
		},
	}, nil
}

// End closes the caller's own preview session (DELETE /preview-sessions/current). sub is the
// verified preview Subject; the audit row carries the REAL builder (sub.ActorUserID). Idempotent:
// ending an already-ended session writes nothing and returns nil. reason must be one of
// PreviewEndReasons (the handler validates it at the trust boundary).
func (s *PreviewService) End(ctx context.Context, sub auth.Subject, reason string) error {
	now := s.now()
	ended, err := s.sessions.End(ctx, sub.TenantID, sub.SessionID, reason, now)
	if err != nil {
		return fmt.Errorf("services.PreviewService.End: %w", err)
	}
	if !ended {
		return nil
	}
	s.auditor.Record(ctx, audit.Event{
		TenantID:    sub.TenantID,
		ActorUserID: sub.ActorUserID,
		EntityName:  audit.PreviewMeta,
		RecordID:    sub.SessionID,
		Action:      audit.ActionDelete,
		Diff:        &audit.Diff{After: map[string]any{"reason": reason, "roleId": sub.RoleID}},
		At:          now,
	})
	s.logger.Info("services.PreviewService.End: preview session ended",
		zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.ActorUserID),
		zap.String("preview_session_id", sub.SessionID), zap.String("reason", reason))
	return nil
}
