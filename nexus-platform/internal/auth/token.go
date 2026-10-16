package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// TokenType distinguishes access tokens from refresh tokens. It is embedded in the
// signed claims so a token minted for one purpose cannot be replayed as the other
// (an access token must never pass refresh verification, and vice versa).
type TokenType string

const (
	TokenTypeAccess  TokenType = "access"
	TokenTypeRefresh TokenType = "refresh"
	// TokenTypeAPIKey marks Claims minted by the API-key verifier (APIKEY-1) rather than by a
	// signed JWT. It never appears inside a JWT payload — TokenService only issues/accepts
	// access|refresh — so an attacker cannot forge an "api_key"-typed JWT to impersonate the
	// machine path; the type exists so downstream code/logs can tell the principal kinds apart.
	TokenTypeAPIKey TokenType = "api_key"
	// TokenTypePreview marks a "preview as role" token (specs/064-preview-as-role-sample-data
	// S9-1): read-only, short-lived, minted by a builder for ONE role of the tenant. Read-only is a
	// property of this signed type, not a claim on an access token: VerifyAccessToken and
	// VerifyRefreshToken refuse it (typ mismatch), so every door that has not deliberately opened
	// for it — services.CompositeVerifier.WithPreview is the only one in S9-1 — answers 401.
	TokenTypePreview TokenType = "preview"
)

// Typed, sentinel errors for verification failures. Callers (the auth handlers /
// middleware in later slices) match these with errors.Is to map to HTTP statuses
// without string-matching, never leaking which secret/claim failed to the client.
var (
	// ErrTokenMalformed: the string is not a well-formed JWT (bad structure/base64).
	ErrTokenMalformed = errors.New("auth: token is malformed")
	// ErrTokenExpired: the token parsed and verified but its expiry has passed.
	ErrTokenExpired = errors.New("auth: token is expired")
	// ErrTokenInvalid: signature mismatch (tampered, wrong secret) or otherwise
	// unverifiable token. Also the catch-all for unexpected verification errors.
	ErrTokenInvalid = errors.New("auth: token is invalid")
	// ErrWrongTokenType: a valid token was presented for the wrong purpose
	// (e.g. a refresh token where an access token was required).
	ErrWrongTokenType = errors.New("auth: wrong token type")
)

// Subject is the authenticated principal carried inside a token. These values are
// populated at login (from the verified user row) and read back out on verify; the
// middleware (A2) lifts them into the request context.
type Subject struct {
	UserID   string `json:"uid"`
	TenantID string `json:"tid"`
	RoleID   string `json:"rid,omitempty"`
	IsAdmin  bool   `json:"adm,omitempty"`
	// SessionID is the login session (refresh_tokens.session_id) the token belongs to — the `sid`
	// claim (../specs/055-session-list-kill/spec.md FR-006). Set at every mint site and kept across
	// rotation, so the caller's own session is identifiable from its ACCESS token (GET
	// /auth/sessions "current") and an ended session's access tokens can be refused at once
	// (middleware.AuthMiddlewareLive). Empty on tokens minted before 055 and on API-key principals.
	SessionID string `json:"sid,omitempty"`
	// ActorUserID / ActorTenantID (`act` / `att`) name the REAL person behind a token whose uid is
	// not them, and that person's home tenant. A preview token (TokenTypePreview) carries the
	// builder here while uid is the synthetic role principal; specs/062-customer-workspace L1a
	// reuses the same two claims for the builder's workspace-session token. Empty on every other
	// token, so access/refresh payloads keep their exact pre-064 claim set (omitempty).
	ActorUserID   string `json:"act,omitempty"`
	ActorTenantID string `json:"att,omitempty"`
	// WorkspaceSessionID (`wsid`) is the builder-session id of specs/062-customer-workspace L1a.
	// A preview token never carries it (services.PreviewService.Verify rebuilds the Subject from
	// the session row and leaves it empty).
	WorkspaceSessionID string `json:"wsid,omitempty"`
	// PreviewDraft (`pdr`) asks a preview session to read the draft layer (S9-9); false until then.
	PreviewDraft bool `json:"pdr,omitempty"`
	// IsPlatformOperator is the builder flag. It is NEVER a claim (json:"-": no JWT payload can
	// set it); the auth stage sets it from a live DB read for home tokens (specs/063-draft-publish)
	// and workspace-session tokens (specs/062-customer-workspace). A preview Subject is always false.
	IsPlatformOperator bool `json:"-"`
}

// Claims is the full JWT payload: the application Subject plus the token type and the
// standard registered claims (exp, iat, ...). It is what IssueX signs and VerifyX returns.
type Claims struct {
	Subject
	TokenType TokenType `json:"typ"`
	jwt.RegisteredClaims
}

// TokenService issues and verifies signed JWTs using a single HMAC secret and the
// configured access/refresh TTLs. It holds no I/O state and is safe for concurrent use.
//
// signingMethod is fixed to HS256; verification additionally asserts the parsed token's
// method is HMAC (alg-confusion guard, spec §5/W1) so an attacker cannot downgrade to
// "none" or trick the keyfunc into treating the secret as an RSA public key.
type TokenService struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
	now        func() time.Time // injectable clock; defaults to time.Now
}

// NewTokenService builds a TokenService. The secret must be non-empty; TTL validation
// (positive, refresh >= access) is the config layer's responsibility (config.LoadJWTConfig),
// so a negative TTL is intentionally allowed here to support "already expired" test cases.
func NewTokenService(secret string, accessTTL, refreshTTL time.Duration) (*TokenService, error) {
	if secret == "" {
		return nil, fmt.Errorf("auth.NewTokenService: secret must not be empty")
	}
	return &TokenService{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		now:        time.Now,
	}, nil
}

// RefreshTTL reports the configured refresh-token lifetime. The auth service uses it to
// stamp expires_at on the persisted refresh_tokens row so the DB record expires in lockstep
// with the JWT's own exp claim (../specs/001-auth-security/spec.md §5).
func (s *TokenService) RefreshTTL() time.Duration { return s.refreshTTL }

// AccessTTL reports the configured access-token lifetime — the ceiling of every access token,
// including a workspace-session token (IssueAccessTokenUntil), so a caller can tell the client when
// that token will lapse.
func (s *TokenService) AccessTTL() time.Duration { return s.accessTTL }

// IssueAccessToken mints a short-lived access token for sub.
func (s *TokenService) IssueAccessToken(sub Subject) (string, error) {
	return s.issue(sub, TokenTypeAccess, s.accessTTL)
}

// IssueAccessTokenUntil mints the short-lived access token of a builder workspace session
// (specs/062-customer-workspace): a normal typ=access JWT whose lifetime is
// min(accessTTL, until − now), so the token can never outlive the session row's own hard deadline.
// There is no refresh token for it — when it lapses the builder resumes the SAME session from a
// live home token. until ≤ now is an error (the caller has an expired/closed session to refuse).
func (s *TokenService) IssueAccessTokenUntil(sub Subject, until time.Time) (string, error) {
	ttl := until.Sub(s.now())
	if ttl <= 0 {
		return "", fmt.Errorf("auth.TokenService.IssueAccessTokenUntil: deadline %s is not in the future", until.Format(time.RFC3339))
	}
	if ttl > s.accessTTL {
		ttl = s.accessTTL
	}
	return s.issue(sub, TokenTypeAccess, ttl)
}

// IssueRefreshToken mints a long-lived refresh token for sub.
func (s *TokenService) IssueRefreshToken(sub Subject) (string, error) {
	return s.issue(sub, TokenTypeRefresh, s.refreshTTL)
}

// IssuePreviewToken mints a preview-as-role token that expires exactly at exp — the expires_at
// of its sys_preview_sessions row, so the JWT and the row die together. There is no refresh
// token for this type: when it expires the builder opens a new session from Studio.
func (s *TokenService) IssuePreviewToken(sub Subject, exp time.Time) (string, error) {
	ttl := exp.Sub(s.now())
	if ttl <= 0 {
		return "", fmt.Errorf("auth.TokenService.IssuePreviewToken: expiry %s is not in the future", exp.Format(time.RFC3339))
	}
	return s.issue(sub, TokenTypePreview, ttl)
}

// VerifyPreviewToken verifies tok and requires it to be a preview token. Signature/expiry/type
// only — the caller (services.PreviewService.Verify) re-checks the session row and the builder.
func (s *TokenService) VerifyPreviewToken(tok string) (*Claims, error) {
	return s.verify(tok, TokenTypePreview)
}

// issue builds and signs a token of the given type with the given lifetime.
func (s *TokenService) issue(sub Subject, typ TokenType, ttl time.Duration) (string, error) {
	now := s.now()
	claims := &Claims{
		Subject:   sub,
		TokenType: typ,
		RegisteredClaims: jwt.RegisteredClaims{
			// A unique JWT ID (jti) per issuance guarantees two tokens minted for the same
			// subject within the same clock-second are still distinct strings — essential for
			// refresh-token rotation (the rotated token must differ) and for the SHA-256 hash
			// stored server-side to be unique per token (no hash collisions across rotations).
			ID:        uuid.NewString(),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Subject:   sub.UserID,
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("auth.TokenService.issue: %w", err)
	}
	return signed, nil
}

// VerifyAccessToken verifies tok and requires it to be an access token.
func (s *TokenService) VerifyAccessToken(tok string) (*Claims, error) {
	return s.verify(tok, TokenTypeAccess)
}

// VerifyRefreshToken verifies tok and requires it to be a refresh token.
func (s *TokenService) VerifyRefreshToken(tok string) (*Claims, error) {
	return s.verify(tok, TokenTypeRefresh)
}

// verify parses and validates tok (signature, expiry, structure), asserts the signing
// method is HMAC, and checks the embedded token type matches want. It returns typed
// sentinel errors so callers can branch without inspecting message text.
func (s *TokenService) verify(tok string, want TokenType) (*Claims, error) {
	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(s.now),
	)
	parsed, err := parser.ParseWithClaims(tok, claims, func(t *jwt.Token) (interface{}, error) {
		// Defense in depth alongside WithValidMethods: reject any non-HMAC method so
		// the secret is never handed to an RSA/ECDSA verification path (alg-confusion).
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %q", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, mapParseError(err)
	}
	if !parsed.Valid {
		return nil, ErrTokenInvalid
	}
	if claims.TokenType != want {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrWrongTokenType, claims.TokenType, want)
	}
	return claims, nil
}

// mapParseError translates jwt library errors into our typed sentinels.
func mapParseError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return fmt.Errorf("%w: %v", ErrTokenExpired, err)
	case errors.Is(err, jwt.ErrTokenMalformed):
		return fmt.Errorf("%w: %v", ErrTokenMalformed, err)
	case errors.Is(err, jwt.ErrTokenSignatureInvalid),
		errors.Is(err, jwt.ErrTokenUnverifiable),
		errors.Is(err, jwt.ErrSignatureInvalid):
		return fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	default:
		return fmt.Errorf("%w: %v", ErrTokenInvalid, err)
	}
}
