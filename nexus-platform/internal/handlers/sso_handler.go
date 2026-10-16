package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// ssoStateCookieName is the browser-session bind cookie (SSO-STATE-COOKIE, #8 post-review
// LOW-5): set at /start with the flow's nonce, required back at /callback to match the sealed
// state's nonce — so a `state` an ATTACKER minted via their own /start cannot complete in a
// VICTIM's browser (login-CSRF: victim silently logged into the attacker's account). The nonce
// is browser-visible by design (it rides the IdP redirect URL), so the cookie leaks nothing;
// what matters is that ONLY the initiating browser carries it back.
const ssoStateCookieName = "sso_state"

// ssoLoginNotAuthorizedMsg is the SINGLE client-facing message shared by the two SSO
// enumeration-sensitive callback outcomes (reviewer FIX 5, LOW — see the
// ErrSSOAccountLinkDenied/ErrSSOUserNotProvisioned cases in writeSSOError below): deny-link
// (email matches an existing local user who never linked this provider/sub) and not-provisioned
// (no local match at all, JIT off) must be indistinguishable to the CLIENT, or a caller could
// probe arbitrary emails through an IdP it controls to learn which ones have a local account in
// this tenant.
const ssoLoginNotAuthorizedMsg = "Không thể đăng nhập qua SSO với tài khoản này. Vui lòng liên hệ quản trị viên"

// ssoService is the subset of services.SSOService the handler needs. Consumer-side interface so
// this file stays unit-testable with a fake and does not force a concrete services import cycle
// beyond the DTOs already shared with the rest of this package.
type ssoService interface {
	CreateProvider(ctx context.Context, tenantID string, in services.CreateSSOProviderInput) (*auth.SSOProvider, error)
	ListProviders(ctx context.Context, tenantID string) ([]*auth.SSOProvider, error)
	// UpdateProviderPolicy partially updates the only two mutable flags (enabled, trustIdpMfa) —
	// a nil pointer leaves that flag unchanged.
	UpdateProviderPolicy(ctx context.Context, tenantID, id string, enabled, trustIdpMfa *bool) (*auth.SSOProvider, error)
	DeleteProvider(ctx context.Context, tenantID, id string) error
	// PublicProviders lists ENABLED providers for the pre-auth login page
	// (SSO-PROVIDER-PUBLIC-LIST); unknown tenant folds into an empty list.
	PublicProviders(ctx context.Context, tenantSlug string) ([]services.PublicSSOProvider, error)
	// StartLogin returns (redirectURL, nonce) — the nonce doubles as the sso_state cookie value.
	StartLogin(ctx context.Context, tenantSlug, providerParam string) (string, string, error)
	// HandleCallback's browserNonce is the sso_state cookie value ("" when the cookie is absent).
	HandleCallback(ctx context.Context, code, state, browserNonce string) (*services.LoginOutput, error)
	// StartHandoff (SSO-FE-REDIRECT) runs the same verification pipeline as HandleCallback but,
	// instead of minting a session, stores a single-use handoff row and returns the RAW one-time
	// code for the 302 redirect to the frontend.
	StartHandoff(ctx context.Context, code, state, browserNonce string) (string, error)
	// ExchangeHandoff (SSO-FE-REDIRECT) redeems a one-time handoff code (POST /auth/sso/exchange)
	// and completes the login — same LoginOutput shape as HandleCallback.
	ExchangeHandoff(ctx context.Context, rawCode string) (*services.LoginOutput, error)
}

// SSOHandler serves the ENG-AUTH2 #8 SSO/OIDC surface (../specs/004-sso-oidc/spec.md §"#8"):
//   - PUBLIC: GET /auth/sso/:tenant/start, GET /auth/sso/callback (pre-auth entry points, like
//     login — mounted on the same public group).
//   - ADMIN (adminGated): POST/GET /admin/sso/providers, PATCH /admin/sso/providers/:id,
//     DELETE /admin/sso/providers/:id.
type SSOHandler struct {
	svc    ssoService
	logger *zap.Logger
	// stateCookiePath / stateCookieSecure are derived ONCE from SSO_REDIRECT_BASE (the backend's
	// own public base URL — the single honest signal for both "what path does the browser see the
	// callback at" and "is this deployment https"). Path-scoping the cookie to the callback route
	// keeps it off every other request; Secure follows the base's scheme so local http dev still
	// works without ever hardcoding Secure=false for production.
	stateCookiePath   string
	stateCookieSecure bool
	// frontendBase (SSO_FRONTEND_BASE, SSO-FE-REDIRECT) switches /callback into redirect mode:
	// non-empty -> 302 to "<frontendBase>/auth/sso/complete?code=<one-time handoff code>" on
	// success / "<frontendBase>/login?ssoError=<coarse code>" on failure. Empty (the default)
	// keeps the legacy JSON LoginOutput response — fully backward compatible.
	frontendBase string
}

// NewSSOHandler wires the SSO handler over the SSO service. redirectBase is SSO_REDIRECT_BASE
// (config.SSOConfig.RedirectBase; "" when SSO is disabled — the cookie attrs then fall back to
// defaults that are never exercised, since /start answers 503 before any cookie is set).
func NewSSOHandler(svc ssoService, redirectBase string, logger *zap.Logger) *SSOHandler {
	cookiePath, cookieSecure := ssoStateCookieAttrs(redirectBase)
	return &SSOHandler{svc: svc, logger: logger, stateCookiePath: cookiePath, stateCookieSecure: cookieSecure}
}

// WithFrontendBase enables redirect-mode /callback (SSO-FE-REDIRECT). base is
// config.SSOConfig.FrontendBase (already boot-validated as an absolute http(s) URL); an empty
// base is a no-op that leaves the handler in legacy JSON mode. Trailing slashes are trimmed so
// path joins below never produce "//".
func (h *SSOHandler) WithFrontendBase(base string) *SSOHandler {
	h.frontendBase = strings.TrimRight(strings.TrimSpace(base), "/")
	return h
}

// ssoStateCookieAttrs derives the sso_state cookie's Path and Secure flag from
// SSO_REDIRECT_BASE. The Path mirrors services.SSOService.callbackRedirectURI's construction
// ("<base>/auth/sso/callback") so the browser sends the cookie on exactly the callback request
// and nothing else. Secure=true iff the base is https — see NewSSOHandler's doc.
func ssoStateCookieAttrs(redirectBase string) (path string, secure bool) {
	path, secure = "/auth/sso/callback", false
	u, err := url.Parse(strings.TrimSpace(redirectBase))
	if err != nil || u == nil {
		return path, secure
	}
	return strings.TrimRight(u.Path, "/") + "/auth/sso/callback", u.Scheme == "https"
}

// RegisterRoutes mounts the PUBLIC start/callback endpoints. The caller has, by definition, no
// valid token yet at either of these — /start is the pre-auth entry point (mirrors /auth/login)
// and /callback is the IdP's own redirect back to us, so both sit on the public group.
func (h *SSOHandler) RegisterRoutes(rg *gin.RouterGroup) {
	// SSO-PROVIDER-PUBLIC-LIST: the login page asks which enabled IdPs it can offer as
	// branded buttons. Public by design (pre-auth), throttled by the same SSO limiter
	// (":tenant" param → the per-(ip,tenant) start bucket family).
	rg.GET("/auth/sso/:tenant/providers", h.PublicProviders)
	rg.GET("/auth/sso/:tenant/start", h.Start)
	rg.GET("/auth/sso/callback", h.Callback)
	// SSO-FE-REDIRECT: the frontend redeems the one-time handoff code minted by redirect-mode
	// /callback. Same public group -> same dedicated SSO limiter (SSO-RATELIMIT-NAT; no :tenant
	// param, so it shares the per-IP "sso:cb:" bucket with /callback).
	rg.POST("/auth/sso/exchange", h.Exchange)
}

// RegisterAdminRoutes mounts the admin provider CRUD. Must be called on a group that already
// has RequireAdmin in its middleware chain (mirrors AdminRelationHandler).
func (h *SSOHandler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	rg.POST("/admin/sso/providers", h.CreateProvider)
	rg.GET("/admin/sso/providers", h.ListProviders)
	rg.PATCH("/admin/sso/providers/:id", h.UpdateProvider)
	rg.DELETE("/admin/sso/providers/:id", h.DeleteProvider)
}

// PublicProviders handles GET /auth/sso/:tenant/providers — the pre-auth login page asks
// which ENABLED IdPs it can offer as branded buttons (SSO-PROVIDER-PUBLIC-LIST). Unknown
// tenant folds into the same empty list a provider-less tenant returns (anti-enumeration,
// mirrors StartLogin); SSO-disabled deployments answer the same 503 the other SSO routes do.
func (h *SSOHandler) PublicProviders(c *gin.Context) {
	providers, err := h.svc.PublicProviders(c.Request.Context(), c.Param("tenant"))
	if err != nil {
		h.writeSSOError(c, "SSOPublicProviders", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"providers": providers}})
}

// Start handles GET /auth/sso/:tenant/start?provider=<id-or-issuer>. The tenant is FIXED here
// from the path slug (../specs/004-sso-oidc/spec.md §"#8" MUST-FIX) — it is carried forward opaquely
// inside the signed state the service builds; /callback never reads a tenant from its own query
// string. On success this is a 302 redirect to the IdP's authorization endpoint, and the
// sso_state bind cookie (SSO-STATE-COOKIE) is set on the SAME response so the initiating browser
// — and only it — can complete /callback.
func (h *SSOHandler) Start(c *gin.Context) {
	tenantSlug := c.Param("tenant")
	providerParam := c.Query("provider")

	redirectURL, nonce, err := h.svc.StartLogin(c.Request.Context(), tenantSlug, providerParam)
	if err != nil {
		h.writeSSOError(c, "SSOStart", err)
		return
	}
	h.setStateCookie(c, nonce)
	c.Redirect(http.StatusFound, redirectURL)
}

// setStateCookie sets the sso_state bind cookie: HttpOnly (no script access), SameSite=Lax
// (still sent on the IdP's top-level GET redirect back to /callback — Strict would break the
// round-trip; None would defeat the CSRF bind), Path-scoped to the callback route, Max-Age tied
// to the sealed state's own TTL (services.SSOStateTTL), Secure per the deployment's scheme.
func (h *SSOHandler) setStateCookie(c *gin.Context, nonce string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     ssoStateCookieName,
		Value:    nonce,
		Path:     h.stateCookiePath,
		MaxAge:   int(services.SSOStateTTL.Seconds()),
		Secure:   h.stateCookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearStateCookie expires the sso_state cookie (Max-Age<0 serializes as Max-Age=0 — immediate
// deletion). Called unconditionally at /callback — success AND failure — so the bind cookie is
// strictly one-shot: a second /callback with a replayed state finds no cookie to match.
func (h *SSOHandler) clearStateCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     ssoStateCookieName,
		Value:    "",
		Path:     h.stateCookiePath,
		MaxAge:   -1,
		Secure:   h.stateCookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// Callback handles GET /auth/sso/callback?code=...&state=.... It deliberately reads NO tenant
// from its query string (even if a caller supplies ?tenant=..., it is ignored) — the tenant is
// resolved server-side strictly from the signed `state` value (../specs/004-sso-oidc/spec.md §"#8"
// MUST-FIX). Two response modes (SSO-FE-REDIRECT):
//   - Legacy JSON (frontendBase == ""): on success the response is the SAME envelope shape as
//     POST /auth/login ({"data":{"accessToken","refreshToken","user"}}) — a normal one-step
//     login and a completed SSO login are indistinguishable to a client by response shape alone.
//   - Redirect (frontendBase set): identity is resolved, a single-use handoff code is stored,
//     and the browser is 302'd to "<frontendBase>/auth/sso/complete?code=<raw>" — the frontend
//     redeems it via POST /auth/sso/exchange. Session tokens NEVER appear in the URL; on
//     failure only the coarse machine code rides "<frontendBase>/login?ssoError=<code>" (the
//     detailed message stays out of browser history/Referer/access logs, and the
//     anti-enumeration folding of ssoPublicError is preserved as-is).
func (h *SSOHandler) Callback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	// SSO-STATE-COOKIE: the bind cookie set at /start. http.ErrNoCookie (the only error
	// c.Cookie returns) deliberately maps to "" — the service rejects an empty browserNonce
	// with ErrSSOStateMismatch, so a cookieless caller is refused, not silently accepted.
	browserNonce, cookieErr := c.Cookie(ssoStateCookieName)
	if cookieErr != nil {
		browserNonce = ""
	}
	// Clear BEFORE writing any response body (headers flush with the first body byte) and on
	// every outcome — the bind cookie is one-shot by design.
	h.clearStateCookie(c)

	if h.frontendBase == "" {
		out, err := h.svc.HandleCallback(deviceContext(c), code, state, browserNonce)
		if err != nil {
			h.writeSSOError(c, "SSOCallback", err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
		return
	}

	raw, err := h.svc.StartHandoff(c.Request.Context(), code, state, browserNonce)
	if err != nil {
		// Same server-side logging discipline as the JSON path (Warn on state-mismatch /
		// discovery / enumeration-sensitive arms, Error on internal faults), but the browser
		// only ever carries the coarse machine code — never the message, never a token.
		_, errCode, _, logFn := ssoPublicError(err)
		if logFn != nil {
			logFn(h.logger, "SSOCallback")
		}
		c.Redirect(http.StatusFound, h.frontendBase+"/login?ssoError="+url.QueryEscape(errCode))
		return
	}
	c.Redirect(http.StatusFound, h.frontendBase+"/auth/sso/complete?code="+url.QueryEscape(raw))
}

// ssoExchangeRequest is the POST /auth/sso/exchange body (SSO-FE-REDIRECT).
type ssoExchangeRequest struct {
	Code string `json:"code"`
}

// Exchange handles POST /auth/sso/exchange (SSO-FE-REDIRECT): the frontend redeems the one-time
// handoff code from redirect-mode /callback for a real session. Success is the SAME envelope
// shape as POST /auth/login and JSON-mode /callback ({"data":{...}} — including the
// {mfaPending,pendingToken} arm). Every "this code buys nothing" outcome (unknown/used/expired/
// user-disabled/provider-disabled) is the ONE opaque 401 SSO_HANDOFF_INVALID — anti-probing,
// mirrors the service's auth.ErrSSOHandoffInvalid folding.
func (h *SSOHandler) Exchange(c *gin.Context) {
	var req ssoExchangeRequest
	if !bindJSON(c, &req) {
		return
	}

	out, err := h.svc.ExchangeHandoff(deviceContext(c), req.Code)
	if err != nil {
		if errors.Is(err, auth.ErrSSOHandoffInvalid) {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": "Mã đăng nhập SSO không hợp lệ hoặc đã hết hạn — vui lòng đăng nhập lại",
				"code":  "SSO_HANDOFF_INVALID",
			})
			return
		}
		// ErrSSOUnavailable -> 503 SSO_NOT_CONFIGURED, ErrMFAEnrolledButUnavailable -> 503
		// MFA_UNAVAILABLE, anything else -> logged 500 — all already mapped by writeSSOError.
		h.writeSSOError(c, "SSOExchange", err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// createSSOProviderRequest is the POST /admin/sso/providers body.
type createSSOProviderRequest struct {
	Issuer          string `json:"issuer"`
	ClientID        string `json:"clientId"`
	ClientSecret    string `json:"clientSecret"`
	DisplayName     string `json:"displayName"`
	Enabled         *bool  `json:"enabled"`
	JitProvisioning bool   `json:"jitProvisioning"`
}

// CreateProvider handles POST /admin/sso/providers (admin-only). The client secret is sealed
// (AES-256-GCM) before persisting and is NEVER echoed back — the response's SSOProvider carries
// json:"-" on ClientSecretEnc, so even an accidental serialization change here cannot leak it.
func (h *SSOHandler) CreateProvider(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	var req createSSOProviderRequest
	if !bindJSON(c, &req) {
		return
	}

	p, err := h.svc.CreateProvider(auditContext(c), tenantID, services.CreateSSOProviderInput{
		Issuer:          req.Issuer,
		ClientID:        req.ClientID,
		ClientSecret:    req.ClientSecret,
		DisplayName:     req.DisplayName,
		Enabled:         req.Enabled,
		JITProvisioning: req.JitProvisioning,
	})
	if err != nil {
		var ve *services.ValidationError
		if errors.As(err, &ve) {
			c.JSON(statusForCode(ve.Code), gin.H{"error": ve.Msg, "code": ve.Code, "details": ve.Details})
			return
		}
		if errors.Is(err, services.ErrSSOUnavailable) {
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error": "SSO/OIDC chưa được cấu hình trên hệ thống", "code": "SSO_NOT_CONFIGURED",
			})
			return
		}
		h.logger.Error("handlers.SSO.CreateProvider", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể tạo SSO provider", "code": "SSO_PROVIDER_CREATE_FAILED"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"data": p})
}

// ListProviders handles GET /admin/sso/providers (admin-only). Never exposes client secrets.
func (h *SSOHandler) ListProviders(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	list, err := h.svc.ListProviders(c.Request.Context(), tenantID)
	if err != nil {
		h.logger.Error("handlers.SSO.ListProviders", zap.String("tenant_id", tenantID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể lấy danh sách SSO provider", "code": "SSO_PROVIDER_LIST_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": list})
}

// updateProviderRequest is the PATCH /admin/sso/providers/:id body — `enabled` (reviewer FIX 1)
// and `trustIdpMfa` (SSO-MFA-POLICY) are the ONLY mutable fields this endpoint exposes (a
// deliberately narrow surface; issuer/clientId/clientSecret changes go through delete+recreate,
// mirrors FIELD_TYPE_IMMUTABLE-style narrow PATCH contracts elsewhere in this codebase rather
// than a general partial-update). Both fields are optional pointers: an absent field leaves the
// stored flag unchanged (partial update); at least one must be present.
type updateProviderRequest struct {
	Enabled     *bool `json:"enabled"`
	TrustIdpMfa *bool `json:"trustIdpMfa"`
}

// UpdateProvider handles PATCH /admin/sso/providers/:id (admin-only): partially updates the two
// mutable provider flags — `enabled` (re-enable/disable a provider — reviewer FIX 1, closing the
// gap api-conventions.md §3c.4 flagged: "List hiện disabled để admin BẬT LẠI" had no backing
// endpoint) and `trustIdpMfa` (SSO-MFA-POLICY: the per-provider opt-in that lets found-identity
// logins through this provider SKIP local MFA, trusting the IdP's own MFA — BLANKET trust, no
// per-login AMR/acr inspection; see api-conventions.md §3c.8). trustIdpMfa is deliberately NOT
// settable at create — an admin must flip it here, as an explicit second decision.
func (h *SSOHandler) UpdateProvider(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := c.Param("id")
	var req updateProviderRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Enabled == nil && req.TrustIdpMfa == nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": "Cần ít nhất một trong hai trường 'enabled' hoặc 'trustIdpMfa'", "code": "SSO_PROVIDER_FIELD_REQUIRED",
			"details": gin.H{"fields": []string{"enabled", "trustIdpMfa"}},
		})
		return
	}

	p, err := h.svc.UpdateProviderPolicy(auditContext(c), tenantID, id, req.Enabled, req.TrustIdpMfa)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy SSO provider", "code": "SSO_PROVIDER_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.SSO.UpdateProvider", zap.String("tenant_id", tenantID), zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể cập nhật SSO provider", "code": "SSO_PROVIDER_UPDATE_FAILED"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": p})
}

// DeleteProvider handles DELETE /admin/sso/providers/:id (admin-only). Hard-deletes the
// provider; FK CASCADE removes every identity linked to it (revoking that provider's sessions).
func (h *SSOHandler) DeleteProvider(c *gin.Context) {
	tenantID, ok := tenantOf(c)
	if !ok {
		return
	}
	id := c.Param("id")

	if err := h.svc.DeleteProvider(auditContext(c), tenantID, id); err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Không tìm thấy SSO provider", "code": "SSO_PROVIDER_NOT_FOUND"})
			return
		}
		h.logger.Error("handlers.SSO.DeleteProvider", zap.String("tenant_id", tenantID), zap.String("id", id), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Không thể xoá SSO provider", "code": "SSO_PROVIDER_DELETE_FAILED"})
		return
	}
	c.Status(http.StatusNoContent)
}

// writeSSOError maps the ENG-AUTH2 #8 sentinel errors to their HTTP status + stable machine
// code (docs/api-conventions.md §3c) via ssoPublicError, preserving the pre-refactor behavior
// byte-for-byte (JSON body AND server-side Warn/Error lines). Any error not matched there is an
// internal fault, logged with full context and mapped to a generic 500.
func (h *SSOHandler) writeSSOError(c *gin.Context, op string, err error) {
	status, code, msg, logFn := ssoPublicError(err)
	if logFn != nil {
		logFn(h.logger, op)
	}
	c.JSON(status, gin.H{"error": msg, "code": code})
}

// ssoPublicError is the pure err -> (status, machine code, client message) classification shared
// by the JSON error writer (writeSSOError) and redirect-mode /callback (which puts ONLY the
// coarse code on the ?ssoError= redirect — SSO-FE-REDIRECT). The optional logFn carries each
// arm's server-side log line (nil = nothing to log) so both paths keep the exact same logging
// discipline without the mapping ever touching gin.
func ssoPublicError(err error) (status int, code, msg string, logFn func(l *zap.Logger, op string)) {
	switch {
	case errors.Is(err, services.ErrSSOUnavailable):
		return http.StatusServiceUnavailable, "SSO_NOT_CONFIGURED",
			"SSO/OIDC chưa được cấu hình trên hệ thống", nil
	case errors.Is(err, auth.ErrSSOProviderNotFound):
		return http.StatusNotFound, "SSO_PROVIDER_NOT_FOUND",
			"Không tìm thấy SSO provider", nil
	case errors.Is(err, services.ErrSSOProviderDiscoveryFailed):
		// FIX 6: distinct from the generic 500 default below — a typo'd/unreachable/malformed
		// issuer's discovery document is an actionable, specific signal, not an internal fault.
		return http.StatusBadGateway, "SSO_PROVIDER_DISCOVERY_FAILED",
			"Không thể kết nối tới nhà cung cấp SSO (discovery thất bại)",
			func(l *zap.Logger, op string) {
				l.Warn("handlers."+op+": oidc discovery failed", zap.String("op", op), zap.Error(err))
			}
	case errors.Is(err, services.ErrSSOStateInvalid):
		return http.StatusBadRequest, "SSO_STATE_INVALID",
			"Phiên đăng nhập SSO không hợp lệ hoặc đã hết hạn", nil
	case errors.Is(err, services.ErrSSOStateMismatch):
		// SSO-STATE-COOKIE (login-CSRF bind): the sealed state opened fine but this browser
		// session did not initiate it (sso_state cookie missing or not matching the state's
		// nonce) — a refusal about WHO is presenting the state, not its shape, hence 403 (mirrors
		// the SSO_LOGIN_NOT_AUTHORIZED family) rather than folding into the 400 above. Logged
		// server-side: a spike here is the login-CSRF-attempt signature worth spotting.
		return http.StatusForbidden, "SSO_STATE_MISMATCH",
			"Phiên đăng nhập SSO không được khởi tạo từ trình duyệt này — vui lòng bắt đầu đăng nhập lại",
			func(l *zap.Logger, op string) {
				l.Warn("handlers."+op+": state not bound to this browser session", zap.String("op", op))
			}
	case errors.Is(err, services.ErrSSOExchangeFailed):
		return http.StatusBadRequest, "SSO_EXCHANGE_FAILED",
			"Không thể đổi mã xác thực với nhà cung cấp SSO", nil
	case errors.Is(err, services.ErrSSOIDTokenInvalid):
		return http.StatusUnauthorized, "SSO_ID_TOKEN_INVALID",
			"Token định danh SSO không hợp lệ", nil
	case errors.Is(err, services.ErrMFAEnrolledButUnavailable):
		// MFA-ENFORCE fail-closed, SSO parity: a found-identity SSO login for an MFA-enrolled
		// account is blocked with the SAME 503 MFA_UNAVAILABLE the password login returns when
		// the server is missing MFA_SECRET_ENC_KEY — otherwise SSO would be the bypass. The
		// service already logged zap.Error with tenant_id/user_id.
		return http.StatusServiceUnavailable, "MFA_UNAVAILABLE",
			"Tài khoản này bật MFA nhưng hệ thống đang thiếu khóa MFA — liên hệ quản trị; đăng nhập bị chặn để bảo vệ tài khoản", nil
	case errors.Is(err, services.ErrSSOAccountLinkDenied):
		// FIX 5 (LOW, enumeration oracle): the CLIENT-FACING response for this case (email
		// matches an existing local user who never linked this provider/sub) and the
		// ErrSSOUserNotProvisioned case below (no local match at all) used to be two
		// DISTINGUISHABLE outcomes (different `code` AND different `error` text) — a caller could
		// probe an arbitrary email through an IdP it controls and learn from the response alone
		// whether that email has a local account in this tenant. Both arms now return the SAME
		// status/code/error text (status parity already held — both were 403). Server-side
		// diagnosability is preserved: the two zap.Warn lines below stay distinct so an admin
		// reading logs can still tell "denied-link attempt" apart from "unprovisioned attempt" —
		// only what reaches the CLIENT is unified. zap fields deliberately carry no tenant/user
		// identity: at this layer HandleCallback's error return gives us none (tenant only ever
		// lived inside the already-consumed signed state, never surfaced) — logging "op" is
		// enough to spot a spike of denied-link attempts in the audit trail without adding a
		// client-un-verifiable identity claim to the log.
		return http.StatusForbidden, "SSO_LOGIN_NOT_AUTHORIZED", ssoLoginNotAuthorizedMsg,
			func(l *zap.Logger, op string) {
				l.Warn("handlers."+op+": account link denied", zap.String("op", op))
			}
	case errors.Is(err, services.ErrSSOUserNotProvisioned):
		// FIX 5 — see the ErrSSOAccountLinkDenied case above: SAME client-facing status/code/text
		// as that arm, distinct server-side zap.Warn line.
		return http.StatusForbidden, "SSO_LOGIN_NOT_AUTHORIZED", ssoLoginNotAuthorizedMsg,
			func(l *zap.Logger, op string) {
				l.Warn("handlers."+op+": user not provisioned", zap.String("op", op))
			}
	default:
		return http.StatusInternalServerError, "SSO_OPERATION_FAILED",
			"Không thể xử lý yêu cầu SSO",
			func(l *zap.Logger, op string) {
				l.Error("handlers."+op, zap.Error(err))
			}
	}
}
