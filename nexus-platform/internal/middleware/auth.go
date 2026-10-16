// Package middleware holds the HTTP middleware chain that fronts the API: Auth → Tenant
// → ACL → Business Logic (../specs/001-auth-security/spec.md §4). This file implements the Auth stage:
// AuthMiddleware verifies the Bearer access token and lifts the verified claims into the
// request context so downstream stages read the tenant/role from a TRUSTED source — never
// from a client-controlled header or query (spec §5 tenant-from-claim).
package middleware

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"nexus/platform/internal/auth"
)

// Context keys under which AuthMiddleware stores the verified principal. Downstream code
// must read these only via the typed accessors below (ClaimsFromContext / TenantFromContext
// / SubjectFromContext) so the key strings stay a private contract of this package.
//
// ctxKeyTenantID is intentionally the same "tenantID" string the legacy
// auth.GetContextTenantID reader already expects, so handlers wired before this middleware
// landed keep working once it is mounted (../specs/001-auth-security/spec.md §4).
const (
	ctxKeyClaims   = "authClaims"
	ctxKeyTenantID = "tenantID"
)

// tokenVerifier is the subset of auth.TokenService the middleware needs: verifying that a
// presented string is a valid, unexpired ACCESS token and returning its claims. Declaring
// it consumer-side keeps the middleware unit-testable with a fake and inverts the dependency
// so this package does not require a concrete TokenService to be constructed in tests.
type tokenVerifier interface {
	VerifyAccessToken(tok string) (*auth.Claims, error)
}

// contextTokenVerifier is the OPTIONAL context-aware upgrade of tokenVerifier (APIKEY-1). A
// verifier that also needs I/O — the composite JWT+API-key verifier does a DB point-read for
// `nxk_` credentials — implements this too, and verifyBearer prefers it so the lookup honors
// the request's cancellation/deadline. Pure verifiers (auth.TokenService) stay untouched: the
// upgrade is detected by type assertion, keeping every existing wiring and test valid.
type contextTokenVerifier interface {
	VerifyAccessTokenContext(ctx context.Context, tok string) (*auth.Claims, error)
}

// LiveUserReader reads the caller's CURRENT user row for the claim-freshness re-read
// (AuthMiddlewareLive). Same shape as platformOperatorRepo — satisfied by *auth.UserRepository
// directly, and in production by services.CachedUserLoader (30 s backstop TTL, invalidated by
// the user service on every committed user mutation). Exported, like ACLChecker, so the route
// builder can name it without importing a concrete type.
type LiveUserReader interface {
	GetUserByID(ctx context.Context, tenantID, id string) (*auth.User, error)
}

// LiveSessionChecker answers "was this login session ENDED?" for the session-freshness check
// (../specs/055-session-list-kill/spec.md FR-008). Satisfied in production by
// services.CachedSessionGuard (30 s backstop TTL, invalidated the moment a session is ended).
// Exported, like LiveUserReader, so the route builder can name it.
type LiveSessionChecker interface {
	SessionEnded(ctx context.Context, tenantID, userID, sessionID string) (bool, error)
}

// WorkspaceResolver answers "is this builder workspace-session token still good, right now?" for the
// delegation branch of the Auth stage (specs/062-customer-workspace). Satisfied in production by
// services.WorkspaceSessionService.Resolve, which asks auth.WorkspaceSessionRepository.GetLive (the
// session row) and ActorLive (the builder behind it) — two uncached reads, so every revocation
// lands on the very next request on every replica. It returns auth.ErrWorkspaceSessionNotLive /
// auth.ErrActorNotLive when the token no longer stands (the Auth stage answers the uniform 401);
// any other error is a DB failure and the request fails closed (500).
type WorkspaceResolver interface {
	Resolve(ctx context.Context, claims *auth.Claims) (*auth.ResolvedWorkspace, error)
}

// AuthOption tunes the Auth stage without changing the signature every existing caller uses.
type AuthOption func(*authOptions)

type authOptions struct {
	workspace WorkspaceResolver
	// trusted are the reverse-proxy networks whose X-Forwarded-For may be trusted for the client IP
	// handed to the verifier (same set as the per-IP limiters). Empty ⇒ the direct peer.
	trusted []*net.IPNet
}

// WithTrustedProxies sets the reverse-proxy CIDRs behind which the client IP is read from
// X-Forwarded-For — the SAME set (TRUSTED_PROXY_CIDRS) the per-IP limiters use, so the API-key brake
// and the login limiter agree on who "this IP" is. Absent ⇒ the direct peer (XFF never trusted).
func WithTrustedProxies(cidrs []*net.IPNet) AuthOption {
	return func(o *authOptions) { o.trusted = cidrs }
}

// apiKeyThrottleRetryAfter is the Retry-After of the API-key brake's 429: the service's window.
const apiKeyThrottleRetryAfter = time.Minute

// WithWorkspaceResolver wires the resolver of the delegation branch. Absent ⇒ a token that carries
// delegation claims is refused with 401 (fail closed): the harness variants never accept one.
func WithWorkspaceResolver(r WorkspaceResolver) AuthOption {
	return func(o *authOptions) { o.workspace = r }
}

// errSessionEnded marks a token whose login session was ended (DELETE /auth/sessions/:id, the
// admin twin, logout-all): refused with the same 401 as any other auth failure.
var errSessionEnded = errors.New("middleware: login session ended")

// userStatusActive is the only users.status under which a verified token may act. Mirrors
// services.statusActive; kept local so this package stays free of the services import.
const userStatusActive = "active"

// errUserInactive marks a row that exists but whose status left "active" (disabled / suspended):
// the principal is refused exactly like a deleted one (same 401), the reason logged server-side.
var errUserInactive = errors.New("middleware: user is not active")

// AuthMiddleware returns a gin handler that enforces the Auth stage. It is a thin HTTP
// adapter over verifyBearer (the testable unit that holds the real logic): it pulls the
// Authorization header, asks verifyBearer to extract+verify the token, and on success
// stores the claims in context for downstream handlers. On ANY failure it aborts the
// request with a single, structured 401 that never reveals which check failed (missing
// header vs expired vs bad signature) so the response cannot be used to probe tokens
// (../specs/001-auth-security/spec.md §5). verifier must be non-nil; logger may be nil (no-op).
//
// This variant trusts the token's role/admin claims verbatim for the token's lifetime —
// production mounts AuthMiddlewareLive instead (queue #14); this one remains for harnesses
// that verify claims without a user table.
func AuthMiddleware(verifier tokenVerifier, logger *zap.Logger, opts ...AuthOption) gin.HandlerFunc {
	return authMiddleware(verifier, nil, nil, logger, opts...)
}

// AuthMiddlewareLive is AuthMiddleware plus the claim-freshness re-read (queue #14, spec §5):
// after the Bearer verifies, the caller's user row is read through users and the Subject the
// request runs under is REBUILT from that row — role_id and is_admin come from the database,
// not from the (up to access-TTL-old) token claims, and a row that is disabled or gone refuses
// the request with the same 401 as any other auth failure. Because every downstream reader
// (RequireAdmin, RequireACL, the 28 SubjectFromContext callers) goes through the context this
// stage fills, one seam fixes them all: a demotion, role change, disable or delete takes effect
// on the user's VERY NEXT request — the cache TTL is only the backstop for a missed
// invalidation, never the mechanism.
//
// API-key principals (auth.TokenTypeAPIKey, the `nxk_` path) are NOT re-read: they are machine
// identities with no user row, and their verifier already reads the key row per request
// (revocation is immediate there by construction). Reading a user row for them would 401 every
// key. A reader error fails CLOSED (500) — stale token claims are never a fallback authority.
//
// sessions (specs/055 FR-008, may be nil) adds the SESSION half of the same idea: a token whose
// `sid` names a login session that was ended (DELETE /auth/sessions/:id, the admin twin,
// logout-all) is refused with the uniform 401 on its very next request instead of living out its
// access TTL. A token without `sid` (minted before 055) is not checked — it can only expire.
func AuthMiddlewareLive(verifier tokenVerifier, users LiveUserReader, sessions LiveSessionChecker, logger *zap.Logger, opts ...AuthOption) gin.HandlerFunc {
	return authMiddleware(verifier, users, sessions, logger, opts...)
}

// authMiddleware is the shared core: verify the Bearer, optionally refresh the Subject from the
// live user row (users != nil) and refuse an ended session (sessions != nil), then publish the
// claims + tenant into the gin context.
func authMiddleware(verifier tokenVerifier, users LiveUserReader, sessions LiveSessionChecker, logger *zap.Logger, opts ...AuthOption) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	var o authOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(c *gin.Context) {
		// The verifier gets the client IP (per-IP brake on failed API-key lookups, queue #356).
		vctx := auth.WithClientIP(c.Request.Context(), clientIP(c.Request, o.trusted))
		claims, err := verifyBearer(vctx, verifier, c.GetHeader("Authorization"))
		if errors.Is(err, auth.ErrAPIKeyThrottled) {
			// The IP spent its budget of failed key lookups: the ONE canonical 429, no log line (a
			// probing storm must not become a log storm), nothing about whether the string was a key.
			writeRateLimited429(c, apiKeyThrottleRetryAfter)
			return
		}
		if err != nil {
			// Log the real reason server-side (never the token itself) for ops/debugging,
			// but return an undifferentiated 401 to the client.
			logger.Warn("middleware.AuthMiddleware: rejected request", zap.Error(err))
			abortUnauthorized(c)
			return
		}
		// Preview-as-role (specs/064-preview-as-role-sample-data plan A4): this branch MUST stay
		// FIRST, right after verifyBearer. The verifier (services.PreviewService.Verify) has already
		// re-checked the session row and the live builder, uncached, and rebuilt the Subject with
		// IsAdmin=false. Letting it fall through would be wrong three ways: refreshSubject has no
		// users row for the synthetic principal (401) — and if it ever found one it would make the
		// preview ADMIN; the `sid` guard would read the preview-session id as a login session; and
		// the delegation branch specs/062-customer-workspace adds below (act/att/wsid, typ=access
		// only) would 401 a token whose act ≠ uid by design. Lanes landing later keep this order;
		// internal/middleware/auth_preview_test.go pins it.
		if claims.TokenType == auth.TokenTypePreview {
			c.Set(ctxKeyClaims, claims)
			c.Set(ctxKeyTenantID, claims.TenantID)
			c.Next()
			return
		}
		// Builder workspace session (specs/062-customer-workspace plan §2): ONLY a typ=access token that
		// carries any delegation claim (act/att/wsid) enters this branch — a preview token returned
		// above, API-key principals never carry the claims, and a normal token falls straight through
		// to the old path untouched. The branch is the whole authority: the Subject is rebuilt from the
		// resolution (customer tenant, admin, builder flag), never from the token's own adm claim, and
		// neither the home-tenant user reader nor the home session guard is consulted (they would look
		// for the builder in the CUSTOMER tenant).
		if claims.TokenType == auth.TokenTypeAccess && claims.HasDelegationClaims() {
			ws, ok := resolveWorkspaceToken(c, o.workspace, claims, logger)
			if !ok {
				return
			}
			c.Set(ctxKeyClaims, ws)
			c.Set(ctxKeyTenantID, ws.TenantID)
			c.Next()
			return
		}
		if users != nil && claims.TokenType != auth.TokenTypeAPIKey {
			live, err := refreshSubject(c.Request.Context(), users, claims)
			switch {
			case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, errUserInactive):
				// Deleted or disabled since the token was minted: expected sentinels, refused
				// with the SAME 401 as a bad token (anti-enumeration). The FE's 401 recovery
				// then tries /auth/refresh, which the W1 revoke already cut → /login.
				logger.Warn("middleware.AuthMiddleware: live user refused",
					zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID), zap.Error(err))
				abortUnauthorized(c)
				return
			case err != nil:
				logger.Error("middleware.AuthMiddleware: load live user failed (fail-closed)",
					zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID), zap.Error(err))
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
				return
			}
			claims = live
			if sessions != nil && claims.SessionID != "" {
				ended, err := sessions.SessionEnded(c.Request.Context(), claims.TenantID, claims.UserID, claims.SessionID)
				switch {
				case err != nil:
					logger.Error("middleware.AuthMiddleware: session check failed (fail-closed)",
						zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID), zap.Error(err))
					c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
					return
				case ended:
					logger.Warn("middleware.AuthMiddleware: ended session refused",
						zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID),
						zap.String("session_id", claims.SessionID), zap.Error(errSessionEnded))
					abortUnauthorized(c)
					return
				}
			}
		}
		// Success: place the full claims and the tenant id (the most-read value) into the
		// gin context. Downstream stages/handlers read them via the accessors below.
		c.Set(ctxKeyClaims, claims)
		c.Set(ctxKeyTenantID, claims.TenantID)
		c.Next()
	}
}

// resolveWorkspaceToken validates the shape of a delegation token and re-resolves it uncached. On
// success it returns a COPY of the claims rebuilt for the request (TenantID = the customer, admin,
// no role, builder flag set); on any failure it has already written the response and reports false.
func resolveWorkspaceToken(c *gin.Context, resolver WorkspaceResolver, claims *auth.Claims, logger *zap.Logger) (*auth.Claims, bool) {
	if resolver == nil || !claims.DelegationShapeOK() {
		logger.Warn("middleware.AuthMiddleware: malformed workspace session token refused",
			zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID),
			zap.Bool("resolver_wired", resolver != nil))
		abortUnauthorized(c)
		return nil, false
	}
	if _, err := resolver.Resolve(c.Request.Context(), claims); err != nil {
		if errors.Is(err, auth.ErrWorkspaceSessionNotLive) || errors.Is(err, auth.ErrActorNotLive) {
			logger.Warn("middleware.AuthMiddleware: workspace session no longer live",
				zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID),
				zap.String("workspace_session_id", claims.WorkspaceSessionID), zap.Error(err))
			abortUnauthorized(c)
			return nil, false
		}
		logger.Error("middleware.AuthMiddleware: resolve workspace session failed (fail-closed)",
			zap.String("tenant_id", claims.TenantID), zap.String("user_id", claims.UserID),
			zap.String("workspace_session_id", claims.WorkspaceSessionID), zap.Error(err))
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "Lỗi hệ thống", "code": "INTERNAL"})
		return nil, false
	}
	live := *claims
	live.IsAdmin = true
	live.RoleID = ""
	live.IsPlatformOperator = true
	return &live, true
}

// refreshSubject returns a COPY of claims whose RoleID / IsAdmin come from the live user row
// pinned to the token's (tenant, user). UserID and TenantID are the lookup key and are never
// rewritten from the row. It never mutates claims in place: a verifier may hand out a shared
// pointer. A row whose status is not active is errUserInactive; a missing row propagates the
// reader's auth.ErrUserNotFound.
func refreshSubject(ctx context.Context, users LiveUserReader, claims *auth.Claims) (*auth.Claims, error) {
	u, err := users.GetUserByID(ctx, claims.TenantID, claims.UserID)
	if err != nil {
		return nil, err
	}
	if u.Status != userStatusActive {
		return nil, errUserInactive
	}
	live := *claims
	live.IsAdmin = u.IsAdmin
	// The builder flag comes from the live row only (json:"-": never a claim); metaversion.IsBuilder
	// = IsAdmin && IsPlatformOperator is the single question every draft-layer stage asks.
	live.IsPlatformOperator = u.IsPlatformOperator
	live.RoleID = ""
	if u.RoleID != nil {
		live.RoleID = *u.RoleID
	}
	return &live, nil
}

// abortUnauthorized writes the single structured 401 the Auth stage uses for EVERY refusal.
func abortUnauthorized(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": "Yêu cầu xác thực không hợp lệ",
		"code":  "UNAUTHORIZED",
	})
}

// RequireTenant returns a gin handler that enforces the Tenant stage (../specs/001-auth-security/spec.md
// §4): it asserts the verified claims carry a non-empty tenant id before any business handler
// runs. AuthMiddleware already aborts on a missing/invalid token and lifts the tenant claim
// into context, so in normal operation this guard never fires — it is defense-in-depth that
// fails closed if a token ever verifies with an EMPTY tenant claim (a malformed/legacy token),
// guaranteeing no downstream repository can ever run a query with a blank tenant_id. It must be
// mounted AFTER AuthMiddleware. On failure it aborts with the same structured 401 as the Auth
// stage so the two are indistinguishable to a client (anti-enumeration). logger may be nil.
func RequireTenant(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		if _, ok := TenantFromContext(c); !ok {
			// Reaching here means the request either bypassed AuthMiddleware (mounting bug)
			// or carried a verified token with no tenant claim. Either way, refuse: a
			// tenant-less request must never reach a tenant-scoped query.
			logger.Warn("middleware.RequireTenant: rejected request with no tenant claim")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "Yêu cầu xác thực không hợp lệ",
				"code":  "UNAUTHORIZED",
			})
			return
		}
		c.Next()
	}
}

// RequireAdmin returns a gin handler that enforces the admin gate on the /admin/* surface
// (../specs/001-auth-security/spec.md §4: "/admin/* → yêu cầu is_admin"). It must be mounted AFTER
// AuthMiddleware + RequireTenant so the verified is_admin claim is already in context. A
// principal whose token does NOT carry is_admin is refused with a structured 403 (distinct
// from the 401 auth gate: the caller IS authenticated, just not authorized). This is the
// single coarse gate for every /admin route (entities, fields, users); finer per-action RBAC
// arrives with A4. logger may be nil.
func RequireAdmin(logger *zap.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		if !IsAdminFromContext(c) {
			// Authenticated but not an admin: refuse with 403 (not 401). The request reached
			// here only after AuthMiddleware + RequireTenant verified the token, so identity
			// is established — what is missing is the admin privilege.
			logger.Warn("middleware.RequireAdmin: rejected non-admin request on /admin")
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "Yêu cầu quyền quản trị viên",
				"code":  "FORBIDDEN",
			})
			return
		}
		c.Next()
	}
}

// verifyBearer is the testable core of the Auth stage, free of any gin/HTTP plumbing. It
// extracts the credential from a raw "Authorization: Bearer <token>" header value, verifies
// it is a valid access token via verifier, and returns the parsed claims. It returns a typed
// auth sentinel on failure (auth.ErrTokenMalformed for a missing/garbled header, or whatever
// verifier reports) so callers — and tests — can branch on errors.Is without string matching.
// A verifier that implements the contextTokenVerifier upgrade is preferred (ctx threads the
// request's cancellation into I/O-backed verification, e.g. the API-key DB lookup).
func verifyBearer(ctx context.Context, verifier tokenVerifier, authHeader string) (*auth.Claims, error) {
	token, ok := extractBearer(authHeader)
	if !ok {
		// A missing or malformed Authorization header is treated as a malformed token: there
		// is no credential to verify. Folding it into the same error family keeps the HTTP
		// layer's response identical to the bad-token cases (anti-enumeration).
		return nil, auth.ErrTokenMalformed
	}
	var claims *auth.Claims
	var err error
	if cv, ok := verifier.(contextTokenVerifier); ok {
		claims, err = cv.VerifyAccessTokenContext(ctx, token)
	} else {
		claims, err = verifier.VerifyAccessToken(token)
	}
	if err != nil {
		// Propagate the verifier's typed sentinel (expired / invalid / wrong-type) verbatim;
		// the HTTP layer collapses all of them to one 401.
		return nil, err
	}
	return claims, nil
}

// extractBearer parses an "Authorization: Bearer <token>" header value, returning the bare
// token. The scheme match is case-insensitive (RFC 7235). A missing header, a non-Bearer
// scheme, or an empty token after the scheme yields ("", false). It never logs the token.
func extractBearer(header string) (string, bool) {
	const prefix = "bearer "
	h := strings.TrimSpace(header)
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// ---- typed context accessors ------------------------------------------------
//
// Downstream handlers MUST use these to read the authenticated principal. They return a
// safe (zero, false) when the value is absent or has an unexpected type, so a handler that
// forgets to sit behind AuthMiddleware fails closed (no tenant) rather than panicking.

// ClaimsFromContext returns the verified JWT claims AuthMiddleware stored, or (nil, false)
// if the request did not pass through the middleware (or the stored value is the wrong type).
func ClaimsFromContext(c *gin.Context) (*auth.Claims, bool) {
	v, exists := c.Get(ctxKeyClaims)
	if !exists {
		return nil, false
	}
	claims, ok := v.(*auth.Claims)
	if !ok || claims == nil {
		return nil, false
	}
	return claims, true
}

// SubjectFromContext returns just the application Subject (user/tenant/role/admin) from the
// verified claims — the slice most callers need without the registered JWT fields.
func SubjectFromContext(c *gin.Context) (auth.Subject, bool) {
	claims, ok := ClaimsFromContext(c)
	if !ok {
		return auth.Subject{}, false
	}
	return claims.Subject, true
}

// TenantFromContext returns the verified tenant id — the trust-boundary value every
// downstream query must filter on. It is sourced from the access-token claim, never from a
// client header/query (../specs/001-auth-security/spec.md §5). Returns ("", false) when absent/empty.
func TenantFromContext(c *gin.Context) (string, bool) {
	v, exists := c.Get(ctxKeyTenantID)
	if !exists {
		return "", false
	}
	id, ok := v.(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

// IsAdminFromContext reports whether the authenticated principal carries the admin flag in
// its verified claims. It is a convenience over SubjectFromContext for the /admin/* gate
// (spec §4); a request that did not pass AuthMiddleware reports false.
func IsAdminFromContext(c *gin.Context) bool {
	sub, ok := SubjectFromContext(c)
	if !ok {
		return false
	}
	return sub.IsAdmin
}
