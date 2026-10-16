package services

// workspace_session_service.go — specs/062-customer-workspace. A Nexus builder works directly inside a
// customer's workspace with a short-lived token tied to a sys_workspace_sessions row.
//
//   - Resolve (L1a) is the per-request check the Auth stage runs for every delegation token.
//   - Open / Close (L1b) are the two routes behind "Mở Studio của khách" and "Về Nexus".
//
// ponytail: no cache anywhere — two indexed key reads per request of ONE builder, and a cache would
// only delay revocation (logout, ending the home session, removing the operator flag, suspending
// the customer all have to land on the very next request, on every replica).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// workspaceSessionMaxAge is the hard deadline of a workspace session (D-18: one constant, one builder).
const workspaceSessionMaxAge = 8 * time.Hour

// workspaceLabelMax is sys_workspace_sessions.actor_label's VARCHAR(191) in characters.
const workspaceLabelMax = 191

// Sentinels the handler maps onto the API's status codes (docs/api-error-codes.md).
var (
	// ErrWorkspaceNotAllowed: the caller already holds a workspace session token — no nested open (403).
	ErrWorkspaceNotAllowed = errors.New("services: not allowed inside a workspace session")
	// ErrWorkspaceSlugRequired: empty tenantSlug (422).
	ErrWorkspaceSlugRequired = errors.New("services: workspace tenant slug is required")
	// ErrWorkspaceUnauthorized: the builder's home login is not live any more, or the token predates
	// `sid` (401 — the FE refreshes the home token and retries).
	ErrWorkspaceUnauthorized = errors.New("services: builder login is not live")
	// ErrWorkspaceTenantNotFound: unknown, inactive or suspended slug — ONE answer (404).
	ErrWorkspaceTenantNotFound = errors.New("services: workspace tenant not found")
	// ErrWorkspaceIsHome: the target is the builder's own workshop (422).
	ErrWorkspaceIsHome = errors.New("services: target is the builder's home tenant")
	// ErrWorkspaceSessionEnded: the session asked to resume is closed, expired, foreign or unknown (404).
	ErrWorkspaceSessionEnded = errors.New("services: workspace session ended")
	// ErrWorkspaceSessionNotFound: a home token tried to close a session it does not hold (404).
	ErrWorkspaceSessionNotFound = errors.New("services: no workspace session")
)

// workspaceSessionStore is the persistence WorkspaceSessionService needs (auth.WorkspaceSessionRepository).
type workspaceSessionStore interface {
	GetLive(ctx context.Context, tenantID, id, act, att, sid string, now time.Time) (*auth.WorkspaceSessionLive, error)
	ActorLive(ctx context.Context, att, act, sid string, now time.Time) (*auth.LiveActor, error)
	Create(ctx context.Context, s *auth.WorkspaceSession) error
	FindOpen(ctx context.Context, tenantID, act, att, sid string, now time.Time) (*auth.WorkspaceSession, error)
	FindResumable(ctx context.Context, tenantID, id, act, att, sid string, now time.Time) (*auth.WorkspaceSession, error)
	Close(ctx context.Context, tenantID, id, act, reason string, now time.Time) (bool, error)
}

// workspaceTenantResolver maps a slug to its ACTIVE tenant (auth.UserRepository.ResolveTenantBySlug).
type workspaceTenantResolver interface {
	ResolveTenantBySlug(ctx context.Context, slug string) (*auth.Tenant, error)
}

// workspaceTokens mints the session token (auth.TokenService).
type workspaceTokens interface {
	IssueAccessTokenUntil(sub auth.Subject, until time.Time) (string, error)
	AccessTTL() time.Duration
}

// WorkspaceSessionService opens, resolves and closes builder workspace sessions.
type WorkspaceSessionService struct {
	sessions workspaceSessionStore
	tenants  workspaceTenantResolver
	tokens   workspaceTokens
	auditor  audit.Auditor
	logger   *zap.Logger
	now      func() time.Time
	newID    func() string
}

// NewWorkspaceSessionService wires the service. tenants/tokens may be nil for a caller that only
// resolves (tests of the Auth stage); Open then must not be called.
func NewWorkspaceSessionService(sessions workspaceSessionStore, tenants workspaceTenantResolver, tokens workspaceTokens) *WorkspaceSessionService {
	return &WorkspaceSessionService{
		sessions: sessions, tenants: tenants, tokens: tokens,
		auditor: audit.NewNoopAuditor(), logger: zap.NewNop(), now: time.Now, newID: uuid.NewString,
	}
}

// WithAuditor sets the audit sink for the open/close rows (__workspace_session__, async, fail-open).
func (s *WorkspaceSessionService) WithAuditor(a audit.Auditor) *WorkspaceSessionService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithLogger sets the structured logger.
func (s *WorkspaceSessionService) WithLogger(l *zap.Logger) *WorkspaceSessionService {
	if l != nil {
		s.logger = l
	}
	return s
}

// Resolve is the per-request check of a delegation token (middleware.WorkspaceResolver): the session
// row, then the builder behind it — both uncached, both pinned to the claims of the token. It returns
// auth.ErrWorkspaceSessionNotLive when the session no longer stands (closed, expired, customer
// suspended/deleted, claim mismatch) and auth.ErrActorNotLive when the builder no longer may act
// (not active / admin / operator, deleted, workshop inactive, home login session without a live
// refresh token); a DB error is returned wrapped — the Auth stage fails closed on it.
func (s *WorkspaceSessionService) Resolve(ctx context.Context, claims *auth.Claims) (*auth.ResolvedWorkspace, error) {
	now := s.now()
	live, err := s.sessions.GetLive(ctx, claims.TenantID, claims.WorkspaceSessionID, claims.ActorUserID, claims.ActorTenantID, claims.SessionID, now)
	if err != nil {
		return nil, fmt.Errorf("services.WorkspaceSessionService.Resolve: %w", err)
	}
	actor, err := s.sessions.ActorLive(ctx, claims.ActorTenantID, claims.ActorUserID, claims.SessionID, now)
	if err != nil {
		return nil, fmt.Errorf("services.WorkspaceSessionService.Resolve: %w", err)
	}
	return &auth.ResolvedWorkspace{Session: *live, Actor: *actor}, nil
}

// WorkspaceOpenInput is the POST /admin/workspace-sessions body after binding.
type WorkspaceOpenInput struct {
	TenantSlug      string
	ResumeSessionID string
}

// WorkspaceSessionView is the "session" object of the open response.
type WorkspaceSessionView struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenantId"`
	TenantSlug string    `json:"tenantSlug"`
	TenantName string    `json:"tenantName"`
	OpenedAt   time.Time `json:"openedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Resumed    bool      `json:"resumed"`
}

// WorkspaceOpenResult is the body of POST /admin/workspace-sessions. ExpiresAt is when the TOKEN
// lapses (the FE renews just before it); Session.ExpiresAt is the session's hard deadline.
// Created tells the handler 201 (a new row) from 200 (reused / resumed).
type WorkspaceOpenResult struct {
	AccessToken string               `json:"accessToken"`
	ExpiresAt   time.Time            `json:"expiresAt"`
	Session     WorkspaceSessionView `json:"session"`
	Created     bool                 `json:"-"`
}

// Open opens, reuses or resumes the builder's workspace session in the customer tenant `slug` and
// mints its token. home is the verified Subject of the builder's HOME token. Order (the route's
// gate has already refused non-operators BEFORE the slug is ever looked at — anti-enumeration):
// no nested open → slug present → home login live (ActorLive, which also supplies the label) →
// slug resolves to an active tenant → not the home tenant → resume | reuse | create.
//
// Resume NEVER creates a row: a dead id is ErrWorkspaceSessionEnded, so a tab whose session was
// closed elsewhere cannot silently reopen it. Audit (create, at both tenants) is written only by the
// call that created the row.
func (s *WorkspaceSessionService) Open(ctx context.Context, home auth.Subject, in WorkspaceOpenInput) (*WorkspaceOpenResult, error) {
	if home.WorkspaceSessionID != "" {
		return nil, ErrWorkspaceNotAllowed
	}
	slug := strings.TrimSpace(in.TenantSlug)
	if slug == "" {
		return nil, ErrWorkspaceSlugRequired
	}
	if home.SessionID == "" {
		return nil, ErrWorkspaceUnauthorized
	}
	now := s.now()
	actor, err := s.sessions.ActorLive(ctx, home.TenantID, home.UserID, home.SessionID, now)
	if errors.Is(err, auth.ErrActorNotLive) {
		return nil, ErrWorkspaceUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("services.WorkspaceSessionService.Open: %w", err)
	}
	cust, err := s.tenants.ResolveTenantBySlug(ctx, slug)
	if errors.Is(err, auth.ErrTenantNotFound) {
		return nil, ErrWorkspaceTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("services.WorkspaceSessionService.Open: %w", err)
	}
	if cust.ID == home.TenantID {
		return nil, ErrWorkspaceIsHome
	}

	var row *auth.WorkspaceSession
	created := false
	if in.ResumeSessionID != "" {
		row, err = s.sessions.FindResumable(ctx, cust.ID, in.ResumeSessionID, home.UserID, home.TenantID, home.SessionID, now)
		if errors.Is(err, auth.ErrWorkspaceSessionNotLive) {
			return nil, ErrWorkspaceSessionEnded
		}
		if err != nil {
			return nil, fmt.Errorf("services.WorkspaceSessionService.Open: %w", err)
		}
	} else {
		row, err = s.sessions.FindOpen(ctx, cust.ID, home.UserID, home.TenantID, home.SessionID, now)
		if err != nil && !errors.Is(err, auth.ErrWorkspaceSessionNotLive) {
			return nil, fmt.Errorf("services.WorkspaceSessionService.Open: %w", err)
		}
		if row == nil {
			row = &auth.WorkspaceSession{
				ID: s.newID(), TenantID: cust.ID,
				ActorUserID: home.UserID, ActorTenantID: home.TenantID, ActorSessionID: home.SessionID,
				ActorLabel: workspaceActorLabel(actor), ExpiresAt: now.Add(workspaceSessionMaxAge), CreatedBy: home.UserID,
			}
			// The row is written synchronously BEFORE the token is signed: every token that exists has its row.
			if err := s.sessions.Create(ctx, row); err != nil {
				return nil, fmt.Errorf("services.WorkspaceSessionService.Open: %w", err)
			}
			created = true
		}
	}

	tok, err := s.tokens.IssueAccessTokenUntil(auth.Subject{
		UserID: home.UserID, TenantID: cust.ID, IsAdmin: true, SessionID: home.SessionID,
		ActorUserID: home.UserID, ActorTenantID: home.TenantID, WorkspaceSessionID: row.ID,
	}, row.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("services.WorkspaceSessionService.Open: %w", err)
	}
	tokenExp := now.Add(s.tokens.AccessTTL())
	if row.ExpiresAt.Before(tokenExp) {
		tokenExp = row.ExpiresAt
	}

	if created {
		expires := row.ExpiresAt.UTC().Format(time.RFC3339)
		s.auditor.Record(ctx, audit.Event{
			TenantID: cust.ID, ActorUserID: home.UserID, EntityName: audit.WorkspaceSessionMeta, RecordID: row.ID,
			Action: audit.ActionCreate, Diff: &audit.Diff{After: map[string]any{"expiresAt": expires}}, At: now,
		})
		s.auditor.Record(ctx, audit.Event{
			TenantID: home.TenantID, ActorUserID: home.UserID, EntityName: audit.WorkspaceSessionMeta, RecordID: row.ID,
			Action: audit.ActionCreate,
			Diff:   &audit.Diff{After: map[string]any{"tenantSlug": cust.Slug, "tenantName": cust.Name, "expiresAt": expires}}, At: now,
		})
		s.logger.Info("services.WorkspaceSessionService.Open: workspace session opened",
			zap.String("tenant_id", cust.ID), zap.String("user_id", home.UserID),
			zap.String("home_tenant_id", home.TenantID), zap.String("workspace_session_id", row.ID))
	}
	return &WorkspaceOpenResult{
		AccessToken: tok,
		ExpiresAt:   tokenExp.UTC(),
		Session: WorkspaceSessionView{
			ID: row.ID, TenantID: cust.ID, TenantSlug: cust.Slug, TenantName: cust.Name,
			OpenedAt: openedAt(row, now), ExpiresAt: row.ExpiresAt.UTC(), Resumed: !created,
		},
		Created: created,
	}, nil
}

// openedAt is the row's creation time (a fresh row carries no CreatedAt until the DB stamps it).
func openedAt(row *auth.WorkspaceSession, now time.Time) time.Time {
	if row.CreatedAt.IsZero() {
		return now.UTC()
	}
	return row.CreatedAt.UTC()
}

// workspaceActorLabel is "Full name (Workshop)" — what the customer's audit trail shows. The full
// name of the builder falls back to "Người dựng"; the e-mail (the login of an account that carries a
// platform-wide flag) is NEVER used. Capped at the column's 191 characters.
func workspaceActorLabel(a *auth.LiveActor) string {
	name := strings.TrimSpace(a.FullName)
	if name == "" {
		name = "Người dựng"
	}
	label := name + " (" + a.TenantName + ")"
	if r := []rune(label); len(r) > workspaceLabelMax {
		label = string(r[:workspaceLabelMax])
	}
	return label
}

// Close ends the caller's own workspace session ("Về Nexus"). sub is the verified session Subject
// (customer tenant, wsid). Idempotent at this level: a close that matched nothing (the row was
// closed a moment ago by another tab that had already passed the Auth stage) is a nil error and
// writes no audit. Audit (delete, at both tenants) only by the call that actually closed the row.
func (s *WorkspaceSessionService) Close(ctx context.Context, sub auth.Subject) error {
	if sub.WorkspaceSessionID == "" {
		return ErrWorkspaceSessionNotFound
	}
	now := s.now()
	closed, err := s.sessions.Close(ctx, sub.TenantID, sub.WorkspaceSessionID, sub.ActorUserID, auth.WorkspaceClosedByBuilder, now)
	if err != nil {
		return fmt.Errorf("services.WorkspaceSessionService.Close: %w", err)
	}
	if !closed {
		return nil
	}
	diff := &audit.Diff{Before: map[string]any{"reason": auth.WorkspaceClosedByBuilder}}
	for _, tenantID := range []string{sub.TenantID, sub.ActorTenantID} {
		s.auditor.Record(ctx, audit.Event{
			TenantID: tenantID, ActorUserID: sub.ActorUserID, EntityName: audit.WorkspaceSessionMeta,
			RecordID: sub.WorkspaceSessionID, Action: audit.ActionDelete, Diff: diff, At: now,
		})
	}
	s.logger.Info("services.WorkspaceSessionService.Close: workspace session closed",
		zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.ActorUserID),
		zap.String("workspace_session_id", sub.WorkspaceSessionID))
	return nil
}
