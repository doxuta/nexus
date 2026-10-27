package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// MCP tokens (issue #3): a builder issues one so an outside agent can call the builder agent's read
// and propose tools at /mcp. The token is valid on /mcp only (CompositeVerifier refuses it on the
// API), lives at most MCPTokenMaxTTL, and every call runs as the issuing builder: a token whose
// builder was disabled, demoted or lost the builder flag, or whose workspace was retired or
// suspended, stops working at its next request.
const (
	// MCPTokenPrefix is the token namespace, disjoint from JWTs ("eyJ…") and API keys (nxk_).
	MCPTokenPrefix = "nxm_"
	// MCPTokenMaxTTL bounds a token's life: also its default.
	MCPTokenMaxTTL = 8 * time.Hour
)

// MCPPrincipal is who a verified token acts as, and the thread its proposals land in.
type MCPPrincipal struct {
	TokenID   string
	TenantID  string
	UserID    string
	SessionID string
}

type mcpTokenRepo interface {
	Create(ctx context.Context, t *auth.MCPToken) error
	FindByHash(ctx context.Context, hash string) (*auth.MCPToken, error)
	ListLive(ctx context.Context, tenantID, userID string, now time.Time) ([]auth.MCPToken, error)
	Revoke(ctx context.Context, tenantID, userID, id string, at time.Time) error
	RevokeAllForUser(ctx context.Context, tenantID, userID string, at time.Time) (int64, error)
	Touch(ctx context.Context, tenantID, id string, at time.Time) error
}

// mcpThreads opens the token's assistant thread (ai.SessionStore.AppendTurn: the one write path into
// a thread, which creates it when sessionID is "").
type mcpThreads interface {
	AppendTurn(ctx context.Context, tenantID, userID, sessionID, newTitle, userContent, assistantContent string, assistantDraft json.RawMessage, outcome string) (string, error)
}

// mcpLiveReader reads the issuing builder's live row and their workspace (auth.UserRepository:
// GetTenantByID answers auth.ErrTenantNotFound for a retired or suspended workspace, as the refresh
// flow relies on).
type mcpLiveReader interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
	GetTenantByID(ctx context.Context, id string) (*auth.Tenant, error)
}

// MCPTokenService issues, lists, revokes and verifies MCP tokens.
type MCPTokenService struct {
	repo    mcpTokenRepo
	threads mcpThreads
	users   mcpLiveReader
	auditor audit.Auditor
	logger  *zap.Logger
	now     func() time.Time
}

// NewMCPTokenService wires the service; every dependency is required.
func NewMCPTokenService(repo mcpTokenRepo, threads mcpThreads, users mcpLiveReader) *MCPTokenService {
	return &MCPTokenService{repo: repo, threads: threads, users: users, auditor: audit.NewNoopAuditor(), logger: zap.NewNop(), now: time.Now}
}

// WithAuditor records issue and revoke in the audit log (__mcp_token__ rows; never the secret).
func (s *MCPTokenService) WithAuditor(a audit.Auditor) *MCPTokenService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithLogger injects the structured logger.
func (s *MCPTokenService) WithLogger(l *zap.Logger) *MCPTokenService {
	if l != nil {
		s.logger = l
	}
	return s
}

// Issue mints a token for the calling builder and opens the assistant thread its proposals land in.
// It returns the stored row and the raw token, the only time the raw token exists outside the
// caller's hands. expiresAt nil = MCPTokenMaxTTL from now; otherwise it must be in (now, now+8h].
// A builder working inside a customer workspace (workspace-session token) is refused: the token
// would act as a user that does not exist in that tenant.
func (s *MCPTokenService) Issue(ctx context.Context, sub auth.Subject, expiresAt *time.Time) (*auth.MCPToken, string, error) {
	if sub.WorkspaceSessionID != "" {
		return nil, "", invalid("MCP_TOKEN_WORKSPACE_UNSUPPORTED", "Không tạo được token MCP trong phiên dựng của khách — mở dự án từ workspace của chính bạn")
	}
	now := s.now()
	exp := now.Add(MCPTokenMaxTTL)
	if expiresAt != nil {
		if !expiresAt.After(now) || expiresAt.After(exp) {
			return nil, "", invalid("MCP_TOKEN_EXPIRY_INVALID", "expiresAt phải ở tương lai và không quá 8 giờ nữa")
		}
		exp = *expiresAt
	}
	raw, prefix, err := newSecret(MCPTokenPrefix)
	if err != nil {
		return nil, "", fmt.Errorf("services.MCPTokenService.Issue: generate secret: %w", err)
	}
	shown := MCPTokenPrefix + prefix + "…"
	sessionID, err := s.threads.AppendTurn(ctx, sub.TenantID, sub.UserID, "",
		"Agent qua MCP · "+shown,
		"[MCP] Kết nối agent bên ngoài",
		fmt.Sprintf("Đề xuất của agent kết nối qua MCP bằng token %s (hết hạn %s UTC) hiện ở đây thành thẻ. Agent chỉ đề xuất: bạn xem từng thẻ, Áp dụng vào bản nháp rồi Xuất bản.",
			shown, exp.UTC().Format("2006-01-02 15:04")),
		nil, "")
	if err != nil {
		return nil, "", fmt.Errorf("services.MCPTokenService.Issue: open thread: %w", err)
	}
	t := &auth.MCPToken{
		ID: uuid.NewString(), TenantID: sub.TenantID, UserID: sub.UserID, SessionID: sessionID,
		TokenPrefix: prefix, TokenHash: auth.HashRefreshToken(raw), ExpiresAt: exp, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, t); err != nil {
		return nil, "", fmt.Errorf("services.MCPTokenService.Issue: %w", err)
	}
	emitMetaAudit(ctx, s.auditor, sub.TenantID, "__mcp_token__", "create", nil, t)
	s.logger.Info("services.MCPTokenService: mcp token issued",
		zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID), zap.String("mcp_token_id", t.ID))
	return t, raw, nil
}

// List returns the caller's tokens that still work, newest first.
func (s *MCPTokenService) List(ctx context.Context, tenantID, userID string) ([]auth.MCPToken, error) {
	rows, err := s.repo.ListLive(ctx, tenantID, userID, s.now())
	if err != nil {
		return nil, fmt.Errorf("services.MCPTokenService.List: %w", err)
	}
	return rows, nil
}

// Revoke soft-revokes one of the caller's tokens: its next request answers 401. An id that is not the
// caller's is MCP_TOKEN_NOT_FOUND; revoking twice is a success.
func (s *MCPTokenService) Revoke(ctx context.Context, tenantID, userID, id string) error {
	err := s.repo.Revoke(ctx, tenantID, userID, strings.TrimSpace(id), s.now())
	if errors.Is(err, auth.ErrMCPTokenNotFound) {
		return invalid("MCP_TOKEN_NOT_FOUND", "Không tìm thấy token MCP")
	}
	if err != nil {
		return fmt.Errorf("services.MCPTokenService.Revoke: %w", err)
	}
	emitMetaAudit(ctx, s.auditor, tenantID, "__mcp_token__", "revoke", map[string]any{"id": id}, nil)
	return nil
}

// RevokeAllForUser ends every live agent key of one builder. AuthService calls it on every
// credential-level event (logout-all, a password change or reset, the refresh-reuse alarm), so a key
// minted from a stolen session does not outlive the victim's incident response.
func (s *MCPTokenService) RevokeAllForUser(ctx context.Context, tenantID, userID string) error {
	n, err := s.repo.RevokeAllForUser(ctx, tenantID, userID, s.now())
	if err != nil {
		return fmt.Errorf("services.MCPTokenService.RevokeAllForUser: %w", err)
	}
	if n > 0 {
		emitMetaAudit(ctx, s.auditor, tenantID, "__mcp_token__", "revoke_all", map[string]any{"userId": userID, "count": n}, nil)
		s.logger.Info("services.MCPTokenService: agent keys revoked with the user's sessions",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.Int64("count", n))
	}
	return nil
}

// Verify resolves a presented raw token to its principal. Unknown, revoked, expired, a retired or
// suspended workspace, or a builder who is no longer an active admin with the builder flag:
// auth.ErrMCPTokenInvalid (the reason is logged, never answered). Any other error is an
// infrastructure failure.
func (s *MCPTokenService) Verify(ctx context.Context, raw string) (*MCPPrincipal, error) {
	if !strings.HasPrefix(raw, MCPTokenPrefix) {
		return nil, auth.ErrMCPTokenInvalid
	}
	t, err := s.repo.FindByHash(ctx, auth.HashRefreshToken(raw))
	if errors.Is(err, auth.ErrMCPTokenInvalid) {
		return nil, auth.ErrMCPTokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("services.MCPTokenService.Verify: %w", err)
	}
	now := s.now()
	why, err := s.refusal(ctx, t, now)
	if err != nil {
		return nil, err
	}
	if why != "" {
		s.logger.Warn("services.MCPTokenService: mcp token refused", zap.String("reason", why),
			zap.String("tenant_id", t.TenantID), zap.String("mcp_token_id", t.ID))
		return nil, auth.ErrMCPTokenInvalid
	}
	// ponytail: one UPDATE per verified request; MCP calls are a handful per agent session. A
	// throttled write-behind (APIKeyService.touch) is the upgrade if that ever shows up.
	if err := s.repo.Touch(ctx, t.TenantID, t.ID, now); err != nil {
		s.logger.Warn("services.MCPTokenService: last_used_at write failed",
			zap.String("tenant_id", t.TenantID), zap.String("mcp_token_id", t.ID), zap.Error(err))
	}
	return &MCPPrincipal{TokenID: t.ID, TenantID: t.TenantID, UserID: t.UserID, SessionID: t.SessionID}, nil
}

// refusal says why a stored token no longer works ("" = it does); an error is an infrastructure
// failure, never a verdict.
func (s *MCPTokenService) refusal(ctx context.Context, t *auth.MCPToken, now time.Time) (string, error) {
	switch {
	case t.RevokedAt != nil:
		return "revoked", nil
	case !t.ExpiresAt.After(now):
		return "expired", nil
	}
	if _, err := s.users.GetTenantByID(ctx, t.TenantID); errors.Is(err, auth.ErrTenantNotFound) {
		return "workspace retired or suspended", nil
	} else if err != nil {
		return "", fmt.Errorf("services.MCPTokenService.Verify: workspace: %w", err)
	}
	u, err := s.users.GetUserByID(ctx, t.TenantID, t.UserID)
	switch {
	case errors.Is(err, auth.ErrUserNotFound):
		return "builder gone", nil
	case err != nil:
		return "", fmt.Errorf("services.MCPTokenService.Verify: builder: %w", err)
	case u.Status != "active" || !u.IsAdmin || !u.IsPlatformOperator:
		return "no longer a builder", nil
	}
	return "", nil
}
