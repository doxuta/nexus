package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/middleware"
	"nexus/platform/internal/services"
)

// AuthHandler exposes the A1 authentication endpoints. For this slice it serves
// POST /auth/login (public): it authenticates {tenant, email, password} via the auth
// service and returns an access + refresh token pair. Tenant comes from the request body
// (slug) here precisely because this is the pre-auth entry point; every OTHER protected route
// takes the tenant from a verified claim via AuthMiddleware (../specs/001-auth-security/spec.md §4).
type AuthHandler struct {
	svc    *services.AuthService
	logger *zap.Logger
}

// NewAuthHandler wires the auth handler over the login service.
func NewAuthHandler(svc *services.AuthService, logger *zap.Logger) *AuthHandler {
	return &AuthHandler{svc: svc, logger: logger}
}

// RegisterRoutes mounts ALL public A1 auth routes under one group. Kept for tests + any caller
// that wants every public auth route on a single group. Production wiring (router.go) instead
// calls the two split methods below so the brute-forceable credential endpoints sit under the
// RM-15 rate limiter while GET /auth/me does NOT (see RegisterMeRoute).
func (h *AuthHandler) RegisterRoutes(rg *gin.RouterGroup) {
	h.RegisterThrottledRoutes(rg)
	h.RegisterMeRoute(rg)
}

// RegisterThrottledRoutes mounts the public auth endpoints that MUST sit under the /auth/*
// rate limiter (RM-15): the credential-proving / brute-forceable ones.
//
// login + refresh are PUBLIC (the caller proves identity with credentials or a refresh token,
// so they sit BEFORE any auth middleware). logout self-verifies the refresh token in its body.
// This self-verify is the A1 contract and A2 intentionally KEPT it: these handlers sit BEFORE
// AuthMiddleware on the public group because they need no tenant-from-claim, so verification
// stays in the handler rather than moving to the middleware (../specs/001-auth-security/spec.md §4).
func (h *AuthHandler) RegisterThrottledRoutes(rg *gin.RouterGroup) {
	rg.POST("/auth/login", h.Login)
	rg.POST("/auth/refresh", h.Refresh)
	rg.POST("/auth/logout", h.Logout)
	// ENG-AUTH2 #1: forgot/reset-password are pre-auth credential-recovery endpoints — the
	// caller has, by definition, no valid token yet — so they sit on the PUBLIC group exactly
	// like login/refresh (../specs/002-password-reset/spec.md §"#1").
	rg.POST("/auth/forgot-password", h.ForgotPassword)
	rg.POST("/auth/reset-password", h.ResetPassword)
	// ENG-AUTH2 #7: MFA/TOTP step 2 of the two-step login flow. This is PUBLIC (not behind
	// AuthMiddleware) because the caller does not yet hold a valid access token — the
	// pendingToken minted by Login's MFA branch IS the credential this endpoint verifies. It
	// sits on the SAME rate-limited authGroup as login/forgot-password (RM-15) so brute-forcing
	// a 6-digit TOTP or a 20-hex backup code against a captured pendingToken is throttled.
	rg.POST("/auth/mfa/verify", h.MFAVerify)
}

// RegisterMeRoute mounts GET /auth/me. It is DELIBERATELY separate from the throttled group
// (AUTH-ME-RATELIMIT / catalog B11): /auth/me is an authenticated read that self-verifies the
// Bearer access token — it is NOT a credential-guessing surface, so brute-force protection does
// not belong on it. Every FE navigation calls /auth/me, so counting it against the /auth/*
// limiter self-locked login for the whole window on fast navigation. router.go mounts this on a
// group WITHOUT the limiter; the handler still self-verifies the token (401 on an invalid one).
func (h *AuthHandler) RegisterMeRoute(rg *gin.RouterGroup) {
	rg.GET("/auth/me", h.Me)
}

// RegisterProtectedRoutes mounts auth routes that require a verified caller (Auth → Tenant chain).
// PUT /auth/me/password sits here rather than the public group because it reads the caller's
// identity from the verified JWT claim (injected by AuthMiddleware) — not from a body/param.
// ENG-AUTH2 #7's enroll/activate/disable also sit here: a user manages their OWN MFA — no
// admin gate — and the caller identity (tenant+user) always comes from the verified claim,
// never from a body/param (mirrors ChangePassword).
func (h *AuthHandler) RegisterProtectedRoutes(rg *gin.RouterGroup) {
	// specs/062-customer-workspace L1b: these seven routes manage the caller's OWN home account. A
	// builder workspace-session token is only a visit inside a customer (its tenant is the
	// customer), so it is refused here with 403 WORKSPACE_SESSION_NOT_ALLOWED — a sub-group so the
	// refusal wraps exactly these routes and nothing else on `protected`.
	self := rg.Group("")
	self.Use(middleware.RefuseActingSession())
	self.PUT("/auth/me/password", h.ChangePassword)
	self.POST("/auth/logout-all", h.LogoutEverywhere)
	// specs/055: the caller's own login sessions (identity from the verified claim only).
	self.GET("/auth/sessions", h.ListSessions)
	self.DELETE("/auth/sessions/:id", h.EndSession)
	self.POST("/auth/mfa/enroll", h.MFAEnroll)
	self.POST("/auth/mfa/activate", h.MFAActivate)
	self.POST("/auth/mfa/disable", h.MFADisable)
}

// loginRequest is the POST /auth/login body. All three fields are required; validation
// detail is enforced in the service so the handler stays a thin transport adapter.
type loginRequest struct {
	Tenant   string `json:"tenant"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Login handles POST /auth/login. On success it returns 200 with the token pair and the
// safe user view. On any authentication failure it returns a single generic 401 (no signal
// which of tenant/email/password was wrong, per §5 anti-enumeration). A malformed/empty body
// is a 400. The password is never logged or echoed.
func (h *AuthHandler) Login(c *gin.Context) {
	var req loginRequest
	if !bindJSON(c, &req) {
		return
	}

	out, err := h.svc.Login(deviceContext(c), services.LoginInput{
		Tenant:   req.Tenant,
		Email:    req.Email,
		Password: req.Password,
	})
	if err != nil {
		// Generic credential failure → 401, identical for every wrong-tenant/user/password.
		if errors.Is(err, services.ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Thông tin đăng nhập không đúng", "code": "INVALID_CREDENTIALS"})
			return
		}
		// MFA-ENFORCE fail-closed: the credentials were CORRECT but the account is MFA-enrolled
		// and the server has lost/never had MFA_SECRET_ENC_KEY — blocking is the only safe
		// answer (a silent one-step login here is a second-factor bypass). 503 (server
		// degraded), same MFA_UNAVAILABLE code family as /auth/mfa/*, honest message. The
		// service already logged zap.Error with tenant_id/user_id; no extra log here (the
		// handler only has unverified body strings, and the password must never be logged).
		if errors.Is(err, services.ErrMFAEnrolledButUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "Tài khoản này bật MFA nhưng hệ thống đang thiếu khóa MFA — liên hệ quản trị; đăng nhập bị chặn để bảo vệ tài khoản",
				"code":  "MFA_UNAVAILABLE",
			})
			return
		}
		// Missing/empty required field is a client error (well-formed JSON, bad values).
		// Use statusForCode so LOGIN_FIELD_REQUIRED → 422 (not 400): the body was valid JSON
		// but semantically incomplete, matching the api-conventions 422 rule (RM-06).
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		// Anything else is an internal fault. Log WITHOUT the password (tenant/email only).
		h.logger.Error("handlers.Login",
			zap.String("tenant", req.Tenant), zap.String("email", req.Email), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đăng nhập", "code": "LOGIN_FAILED"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": out})
}

// refreshRequest is the POST /auth/refresh and /auth/logout body. The refresh token may also
// be supplied in the request body for SPA/native clients that store it outside a cookie.
type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

// Refresh handles POST /auth/refresh. It exchanges a valid refresh token for a new access
// token and a rotated refresh token (the presented one is revoked). Any invalid/expired/
// revoked/wrong-type token returns a single generic 401 (no signal which); a malformed body
// is a 400. The tokens are never logged.
func (h *AuthHandler) Refresh(c *gin.Context) {
	var req refreshRequest
	if !bindJSON(c, &req) {
		return
	}

	out, err := h.svc.Refresh(deviceContext(c), req.RefreshToken)
	if err != nil {
		if errors.Is(err, services.ErrInvalidRefresh) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Refresh token không hợp lệ", "code": "INVALID_REFRESH"})
			return
		}
		h.logger.Error("handlers.Refresh", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể làm mới token", "code": "REFRESH_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// Logout handles POST /auth/logout. It ENDS the login session of the presented (live) refresh
// token (specs/055 FR-012): no more refresh, and that session's access tokens — this tab's and any
// other tab of the same browser — are refused on their next request. Logout is idempotent: an
// unknown/already-dead token still returns 204, so the endpoint cannot be used to probe token
// validity. A malformed body is a 400.
func (h *AuthHandler) Logout(c *gin.Context) {
	var req refreshRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := h.svc.Logout(c.Request.Context(), req.RefreshToken); err != nil {
		h.logger.Error("handlers.Logout", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đăng xuất", "code": "LOGOUT_FAILED"})
		return
	}
	c.Status(http.StatusNoContent)
}

// LogoutEverywhere handles POST /auth/logout-all — end every session of the CALLER, on every
// device. Identity comes from the verified claim (AuthMiddleware), so there is no body: a user
// can only end their own sessions.
//
// 204 on success, like /auth/logout. The response deliberately promises nothing about the
// caller's CURRENT access token: it stays valid until it expires, and the client is expected to
// drop its own tokens right after (the FE does, then routes to /login).
func (h *AuthHandler) LogoutEverywhere(c *gin.Context) {
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Token không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	if err := h.svc.LogoutEverywhere(c.Request.Context(), sub.TenantID, sub.UserID); err != nil {
		h.logger.Error("handlers.LogoutEverywhere", zap.String("tenant_id", sub.TenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đăng xuất các thiết bị", "code": "LOGOUT_FAILED"})
		return
	}
	h.logger.Info("handlers.LogoutEverywhere: all sessions revoked",
		zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID))
	c.Status(http.StatusNoContent)
}

// ListSessions handles GET /auth/sessions (specs/055 US1): the CALLER's live login sessions, one
// per device chain, with the device each came from. Tenant + user come from the verified claim —
// there is no parameter that could point it at someone else.
func (h *AuthHandler) ListSessions(c *gin.Context) {
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Token không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	views, err := h.svc.ListSessions(c.Request.Context(), sub.TenantID, sub.UserID, sub.SessionID)
	if err != nil {
		writeSessionError(c, h.logger, "Auth.ListSessions", sub.TenantID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": views})
}

// EndSession handles DELETE /auth/sessions/:id (specs/055 US2): end ONE of the caller's own login
// sessions — "đăng xuất thiết bị này". The session is pinned to the verified claim's tenant + user,
// so another user's session id is a 404 SESSION_NOT_FOUND like an unknown one (no existence
// leak). The ended device is refused on its very next request; the caller's other sessions,
// including the one making this call, are untouched (ending the current one is simply a logout).
func (h *AuthHandler) EndSession(c *gin.Context) {
	sub, ok := middleware.SubjectFromContext(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Token không hợp lệ", "code": "UNAUTHORIZED"})
		return
	}
	sessionID := strings.TrimSpace(c.Param("id"))
	if err := h.svc.EndSession(c.Request.Context(), sub.TenantID, sub.UserID, sessionID); err != nil {
		writeSessionError(c, h.logger, "Auth.EndSession", sub.TenantID, err)
		return
	}
	h.logger.Info("handlers.Auth.EndSession: session ended",
		zap.String("tenant_id", sub.TenantID), zap.String("user_id", sub.UserID),
		zap.String("session_id", sessionID), zap.Bool("own_current", sessionID == sub.SessionID))
	c.Status(http.StatusNoContent)
}

// writeSessionError maps a login-session service error: a ValidationError (USER_NOT_FOUND,
// SESSION_NOT_FOUND) to its status via statusForCode, anything else to a logged 500.
func writeSessionError(c *gin.Context, logger *zap.Logger, op, tenantID string, err error) {
	var ve *services.ValidationError
	if errors.As(err, &ve) {
		c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
		return
	}
	logger.Error("handlers."+op, zap.String("tenant_id", tenantID), zap.Error(err))
	c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xử lý phiên đăng nhập", "code": "SESSION_OP_FAILED"})
}

// deviceContext tags the request context with the calling device (User-Agent + client IP as gin
// resolves it — honouring TRUSTED_PROXY_CIDRS, never a spoofed X-Forwarded-For) so every token
// mint records which device a login session belongs to (specs/055 FR-002). Used by every handler
// that mints a token pair: login, refresh, MFA step 2, SSO callback / exchange.
func deviceContext(c *gin.Context) context.Context {
	return services.WithClientDevice(c.Request.Context(), services.ClientDevice{
		UserAgent: c.Request.UserAgent(),
		IP:        c.ClientIP(),
	})
}

// Me handles GET /auth/me. It returns the current user (resolved from the Bearer ACCESS
// token) as the safe public view — never the password hash. A missing/invalid/expired token
// is a 401. In this A1 slice the handler verifies the token itself; once AuthMiddleware (A2)
// lands, the user will be resolved from claims already placed in the request context.
func (h *AuthHandler) Me(c *gin.Context) {
	token, ok := bearerToken(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Thiếu hoặc sai Authorization header", "code": "UNAUTHORIZED"})
		return
	}

	user, err := h.svc.Me(c.Request.Context(), token)
	if err != nil {
		if errors.Is(err, services.ErrUnauthorized) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Token không hợp lệ", "code": "UNAUTHORIZED"})
			return
		}
		h.logger.Error("handlers.Me", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể lấy thông tin người dùng", "code": "ME_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": user})
}

// changePasswordRequest is the PUT /auth/me/password body.
type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// ChangePassword handles PUT /auth/me/password. It verifies the caller's current password
// against the stored bcrypt hash, enforces the min-length policy on the new password, replaces
// the hash, and ENDS every other login session of the caller (their devices get 401 on the next
// request; the calling device stays signed in).
// The calling user is always the verified JWT subject (never from the body) — this endpoint
// cannot change another user's password. Returns 401 on wrong current password, 422 on weak new.
func (h *AuthHandler) ChangePassword(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	var req changePasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	sub, _ := middleware.SubjectFromContext(c) // the caller's own login session stays signed in (#310)
	if err := h.svc.ChangePassword(c.Request.Context(), tenantID, userID, req.CurrentPassword, req.NewPassword, sub.SessionID); err != nil {
		if errors.Is(err, services.ErrInvalidCredentials) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Mật khẩu hiện tại không đúng", "code": "INVALID_CREDENTIALS"})
			return
		}
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.ChangePassword",
			zap.String("tenant_id", tenantID),
			zap.String("user_id", userID),
			zap.Error(err),
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đổi mật khẩu", "code": "PASSWORD_CHANGE_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// forgotPasswordRequest is the POST /auth/forgot-password body.
type forgotPasswordRequest struct {
	Tenant string `json:"tenant"`
	Email  string `json:"email"`
}

// ForgotPassword handles POST /auth/forgot-password. It ALWAYS returns 200 {"data":{"ok":true}}
// for a well-formed request regardless of whether the tenant/email exist — this is the
// anti-enumeration contract (../specs/002-password-reset/spec.md §"#1" MUST-FIX 7): the service internally
// no-ops (zero token rows, zero email) for an unknown tenant OR unknown email, and the handler
// has no signal to distinguish that from the success path. Only a malformed body (missing
// tenant/email) or a genuine infrastructure failure (DB/mailer down) produces a different status.
// Rate-limit (RM-15 shared /auth/* limiter): forgot's body carries {tenant,email}, so the
// middleware keys it per (ip,tenant,email) like login; RESET is the IP-only endpoint — its
// body {token,newPassword} has no identity to key on (see §"#1" MUST-FIX 7).
func (h *AuthHandler) ForgotPassword(c *gin.Context) {
	var req forgotPasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := h.svc.RequestPasswordReset(c.Request.Context(), req.Tenant, req.Email); err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		// Any other error is an infrastructure fault (DB/mailer) — NOT an enumeration signal,
		// because the anti-enumeration branches inside the service already returned nil above.
		h.logger.Error("handlers.ForgotPassword", zap.String("tenant", req.Tenant), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể gửi email đặt lại mật khẩu", "code": "FORGOT_PASSWORD_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// resetPasswordRequest is the POST /auth/reset-password body. Deliberately carries NO tenant
// field — the tenant is derived server-side from the token row (MUST-FIX 1); a client-supplied
// tenant here would be untrusted input and is not read even if present in a raw JSON body.
type resetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

// ResetPassword handles POST /auth/reset-password. An invalid/expired/already-used token maps
// to 400 RESET_TOKEN_INVALID (mirrors INVALID_CURSOR — a malformed/dead opaque credential on a
// public, non-Authorization route); a weak new password maps to 422 via statusForCode (the
// SAME RECORD_INVALID_TYPE code ChangePassword uses). The token is never logged.
func (h *AuthHandler) ResetPassword(c *gin.Context) {
	var req resetPasswordRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := h.svc.ResetPassword(c.Request.Context(), req.Token, req.NewPassword); err != nil {
		if errors.Is(err, auth.ErrResetTokenInvalid) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Đường dẫn đặt lại mật khẩu không hợp lệ hoặc đã hết hạn", "code": "RESET_TOKEN_INVALID"})
			return
		}
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code})
			return
		}
		h.logger.Error("handlers.ResetPassword", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể đặt lại mật khẩu", "code": "RESET_PASSWORD_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// bearerToken extracts the credential from an "Authorization: Bearer <token>" header. The
// scheme match is case-insensitive (RFC 7235); a missing header, wrong scheme, or empty
// token yields ("", false). It never logs the token.
func bearerToken(c *gin.Context) (string, bool) {
	const prefix = "bearer "
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// --- ENG-AUTH2 #7: MFA / TOTP ---------------------------------------------------------------

// mfaEnrollResponse is the POST /auth/mfa/enroll body: the raw base32 TOTP secret (for manual
// entry) plus an otpauth:// URL the frontend renders as a QR code. Neither value is persisted
// in the clear server-side (the secret is sealed via AES-256-GCM before it is stored) — this
// response is the ONLY time the raw secret ever leaves the server.
type mfaEnrollResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauthUrl"`
}

// mfaEnrollRequest is the POST /auth/mfa/enroll body (MFA-STEPUP): the caller's account
// password, required as a step-up re-authentication so a stolen access token alone cannot
// plant an attacker-known TOTP secret. The password is never logged or echoed.
type mfaEnrollRequest struct {
	Password string `json:"password"`
}

// MFAEnroll handles POST /auth/mfa/enroll (protected — caller identity from the verified JWT
// claim, never a body/param). Generates a fresh TOTP secret for the caller and returns it plus
// an otpauth:// URL; MFA is NOT yet active until POST /auth/mfa/activate confirms a code.
// MFA-STEPUP: requires the account password in the body (step-up re-auth) — missing → 422
// MFA_PASSWORD_REQUIRED, wrong → 401 INVALID_CREDENTIALS (counts toward the 5/15min lockout).
func (h *AuthHandler) MFAEnroll(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	var req mfaEnrollRequest
	if !bindJSON(c, &req) {
		return
	}

	secret, otpauthURL, err := h.svc.MFAEnroll(c.Request.Context(), tenantID, userID, req.Password)
	if err != nil {
		h.writeMFAError(c, "MFAEnroll", tenantID, userID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mfaEnrollResponse{Secret: secret, OtpauthURL: otpauthURL}})
}

// mfaCodeRequest is the shared POST /auth/mfa/{activate,disable} body. Password (MFA-STEPUP) is
// the caller's account password — a required step-up re-authentication on top of the TOTP/
// backup code, so a hijacked session cannot flip the second factor without knowing the password.
type mfaCodeRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

// mfaActivateResponse is the POST /auth/mfa/activate body on success: the ten single-use
// backup recovery codes in PLAINTEXT — the ONE time they are ever returned. The caller must
// show these to the user immediately; only their SHA-256 hashes are persisted.
type mfaActivateResponse struct {
	BackupCodes []string `json:"backupCodes"`
}

// MFAActivate handles POST /auth/mfa/activate (protected). Confirms the enrollment generated
// by MFAEnroll with a first valid TOTP code, flips the account to MFA-enabled, and returns the
// ten backup codes. A wrong/expired code is 401 MFA_CODE_INVALID; enrolling twice without
// disabling is 409 MFA_ALREADY_ENABLED; activating without a prior enroll is 422
// MFA_NOT_ENROLLED. MFA-STEPUP: also requires the account password — missing → 422
// MFA_PASSWORD_REQUIRED, wrong → 401 INVALID_CREDENTIALS (counts toward the 5/15min lockout).
func (h *AuthHandler) MFAActivate(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	var req mfaCodeRequest
	if !bindJSON(c, &req) {
		return
	}

	codes, err := h.svc.MFAActivate(c.Request.Context(), tenantID, userID, req.Password, req.Code)
	if err != nil {
		h.writeMFAError(c, "MFAActivate", tenantID, userID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": mfaActivateResponse{BackupCodes: codes}})
}

// MFADisable handles POST /auth/mfa/disable (protected). Verifies the presented code (a valid
// TOTP OR an unused backup code, either accepted) and, on success, permanently removes the
// caller's MFA enrollment and every backup code. A wrong/expired code is 401 MFA_CODE_INVALID;
// disabling an account with no enrollment is 422 MFA_NOT_ENROLLED. MFA-STEPUP: also requires
// the account password — missing → 422 MFA_PASSWORD_REQUIRED, wrong → 401 INVALID_CREDENTIALS
// (counts toward the 5/15min lockout).
func (h *AuthHandler) MFADisable(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	userID := actorOf(c)

	var req mfaCodeRequest
	if !bindJSON(c, &req) {
		return
	}

	if err := h.svc.MFADisable(c.Request.Context(), tenantID, userID, req.Password, req.Code); err != nil {
		h.writeMFAError(c, "MFADisable", tenantID, userID, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"ok": true}})
}

// mfaVerifyRequest is the POST /auth/mfa/verify body — step 2 of the two-step login flow.
// Deliberately carries NO tenant (like resetPasswordRequest): tenant is derived server-side
// from the pendingToken's own row, never trusted from the client.
type mfaVerifyRequest struct {
	PendingToken string `json:"pendingToken"`
	Code         string `json:"code"`
}

// MFAVerify handles POST /auth/mfa/verify (PUBLIC — see RegisterRoutes doc). It redeems a
// pendingToken minted by Login's MFA branch for a real access+refresh token pair, gated on a
// correct TOTP or backup code. On success the response body is IDENTICAL in shape to a normal
// POST /auth/login success ({"data":{"accessToken","refreshToken","user"}}) — the caller
// cannot distinguish a one-step login from a completed two-step login by response shape alone.
// A wrong/expired code is 401 MFA_CODE_INVALID; a missing/expired/already-used/forged
// pendingToken is 401 MFA_PENDING_INVALID (folded together, anti-probe).
func (h *AuthHandler) MFAVerify(c *gin.Context) {
	var req mfaVerifyRequest
	if !bindJSON(c, &req) {
		return
	}

	out, err := h.svc.CompleteMFALogin(deviceContext(c), req.PendingToken, req.Code)
	if err != nil {
		h.writeMFAError(c, "MFAVerify", "", "", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// writeMFAError maps the ENG-AUTH2 #7 sentinel errors to their HTTP status + stable machine
// code (docs/api-conventions.md §3b). tenantID/userID are log-only context and may be empty
// (MFAVerify runs pre-auth, before any verified identity exists). Any error not matched below
// is an internal fault, logged with full context and mapped to a generic 500 — never a
// half-specific MFA_* code for a case the client cannot meaningfully act on.
func (h *AuthHandler) writeMFAError(c *gin.Context, op, tenantID, userID string, err error) {
	switch {
	case errors.Is(err, services.ErrMFAUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "Xác thực hai lớp (MFA) chưa được cấu hình trên hệ thống", "code": "MFA_UNAVAILABLE",
		})
	case errors.Is(err, auth.ErrMFANotEnrolled):
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Tài khoản chưa đăng ký xác thực hai lớp (MFA)", "code": "MFA_NOT_ENROLLED",
		})
	case errors.Is(err, auth.ErrMFAAlreadyEnabled):
		c.JSON(http.StatusConflict, gin.H{
			"error": "Xác thực hai lớp (MFA) đã được bật cho tài khoản này", "code": "MFA_ALREADY_ENABLED",
		})
	case errors.Is(err, services.ErrMFACodeInvalid):
		// Reviewer LOW-1: a wrong/replayed/reused MFA code was previously not logged anywhere —
		// log it at Warn so repeated failures against one identity are visible in the audit
		// trail (brute-force / stolen-session signal). NEVER log the presented code itself.
		h.logger.Warn("handlers."+op+": mfa code invalid",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID))
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Mã xác thực không đúng hoặc đã được sử dụng", "code": "MFA_CODE_INVALID",
		})
	case errors.Is(err, auth.ErrMFAPendingInvalid):
		// Also logged (companion to the branch above): a forged/expired/reused pendingToken can
		// signal probing of the two-step-login bridge credential. tenantID/userID are commonly
		// empty here (MFAVerify runs pre-auth, before the pendingToken's owner is resolved) —
		// still worth a line with whatever identity context is available. NEVER log the token.
		h.logger.Warn("handlers."+op+": mfa pending token invalid",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID))
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Phiên đăng nhập MFA không hợp lệ hoặc đã hết hạn", "code": "MFA_PENDING_INVALID",
		})
	case errors.Is(err, services.ErrMFATooManyAttempts):
		// Guard trip (RM-15-style throttle scoped to the PROTECTED /auth/mfa/{enroll,activate,
		// disable} path — see services.mfaAttemptLimiter's doc). Retry-After tells the client how
		// long to back off; MFAAttemptLockoutWindow is the same window the limiter itself uses.
		c.Header("Retry-After", strconv.Itoa(int(services.MFAAttemptLockoutWindow.Seconds())))
		h.logger.Warn("handlers."+op+": mfa attempt limit exceeded",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "Quá nhiều lần thử mã MFA sai. Vui lòng thử lại sau", "code": "MFA_TOO_MANY_ATTEMPTS",
		})
	case errors.Is(err, services.ErrInvalidCredentials):
		// MFA-STEPUP: the step-up password re-authentication failed. Same code family as
		// Login/ChangePassword (401 INVALID_CREDENTIALS). Logged at Warn like a wrong MFA code —
		// a wrong password on an already-authenticated session is a strong stolen-token signal.
		// NEVER log the presented password.
		h.logger.Warn("handlers."+op+": mfa step-up password invalid",
			zap.String("tenant_id", tenantID), zap.String("user_id", userID))
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": "Mật khẩu không đúng", "code": "INVALID_CREDENTIALS",
		})
	default:
		// MFA-STEPUP: a missing/empty required body field (MFA_PASSWORD_REQUIRED) surfaces as a
		// services.ValidationError → statusForCode (422 family), mirroring Login's
		// LOGIN_FIELD_REQUIRED handling.
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		h.logger.Error("handlers."+op,
			zap.String("tenant_id", tenantID), zap.String("user_id", userID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Không thể xử lý yêu cầu MFA", "code": "MFA_OPERATION_FAILED",
		})
	}
}
