package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// APIKEY-1 — machine identity (production-ops slice c, docs/api-conventions.md §3e).
//
// An API key authenticates a SERVER as a (tenant, role) principal. VerifyKey mints
// auth.Claims{Subject{TenantID, RoleID, IsAdmin:false}} — the SAME shape a JWT login produces —
// so the whole existing authorization stack (RequireTenant → per-action ACL → field-ACL →
// tenant-pinned repos) governs machine calls with ZERO downstream changes. Security invariants:
//
//   - IsAdmin is HARD-FLOORED to false at mint time, unconditionally: acl_roles carries no admin
//     bit (is_admin lives on users), so there is no admin role to refuse — but even if a future
//     role model grew one, this constructor never reads it. RequireAdmin therefore rejects every
//     API-key request: /admin/* is structurally unreachable with a key.
//   - Only the SHA-256 of the raw key is stored; the raw secret is returned ONCE from
//     IssueAPIKey and never again (list/get answer prefix + metadata only).
//   - Every verify failure (unknown / revoked / expired) folds into the ONE opaque
//     auth.ErrAPIKeyInvalid; the auth middleware collapses it into the same undifferentiated
//     401 a bad JWT gets (anti-enumeration — a prober cannot learn "valid format, revoked").
//   - The key row carries its tenant_id; minted Claims use THAT value, so a key can only ever
//     act inside its own tenant. Claims.UserID is set to the KEY's id (a machine principal has
//     no user): audit rows and created_by attribute to the key, and an `owner`-scoped grant
//     resolves to records the key itself created — both fail-closed properties.
const (
	// apiKeyPrefix is the credential namespace. A JWT can never collide with it: compact JWTs
	// always start with the base64url of `{"alg"` (i.e. "eyJ..."), so prefix-routing in the
	// composite verifier can never shadow a valid JWT (fall-through proof).
	apiKeyPrefix = "nxk_"
	// apiKeySecretBytes is the CSPRNG budget: 32 bytes = 256 bits, encoded base64url (43 chars).
	// The first apiKeyPrefixChars of the encoding become the PUBLIC key_prefix (shown in the
	// admin list), so the effective secret entropy is 256 − 12×6 = 184 bits — still far above
	// the 128-bit brute-force bar even with the prefix fully disclosed.
	apiKeySecretBytes = 32
	// apiKeyPrefixChars is how many chars of the random part form the public identification
	// prefix (fits the key_prefix VARCHAR(16) column).
	apiKeyPrefixChars = 12
	// apiKeyNameMaxRunes caps the admin-facing label (column VARCHAR(100), utf8mb4 = 100 chars).
	apiKeyNameMaxRunes = 100
	// apiKeyTouchInterval throttles the last_used_at write-behind: at most one UPDATE per key
	// per interval per process (see touch below).
	apiKeyTouchInterval = 5 * time.Minute
	// apiKeyTouchTimeout bounds the async write so a slow DB can never pile up goroutines
	// beyond (keys × in-flight-window).
	apiKeyTouchTimeout = 5 * time.Second
)

// apiKeyRepo is the persistence surface the service needs — satisfied by
// auth.APIKeyRepository, faked in unit tests.
type apiKeyRepo interface {
	Create(ctx context.Context, k *auth.APIKey) error
	FindByHash(ctx context.Context, keyHash string) (*auth.APIKey, error)
	List(ctx context.Context, tenantID string) ([]auth.APIKey, error)
	GetByID(ctx context.Context, tenantID, id string) (*auth.APIKey, error)
	Revoke(ctx context.Context, tenantID, id string, at time.Time) (bool, error)
	TouchLastUsed(ctx context.Context, tenantID, id string, at time.Time) error
}

// APIKeyService implements issue / list / revoke (admin surface) and VerifyKey (the hot-path
// credential check the composite verifier calls). repo and roles are REQUIRED constructor
// params — the in-tenant role guard on a new credential must not be forgettable (the same
// lesson as the AI budget guard: an optional WithX for a security dep is a foot-gun).
type APIKeyService struct {
	repo    apiKeyRepo
	roles   roleValidator // in-tenant role guard (auth.RoleRepository in production)
	auditor audit.Auditor
	logger  *zap.Logger
	now     func() time.Time
	genID   func() string

	// lastTouch throttles the last_used_at write-behind per key id (in-memory, per-process —
	// same acceptance as the RM-15 rate limiters). Entry count is bounded by the number of
	// distinct keys actively used in this process.
	lastTouch sync.Map // key id (string) → time.Time of last scheduled touch
	// touchDone, when non-nil (tests), receives one signal after each async touch completes,
	// making the fire-and-forget write observable without sleeps.
	touchDone chan struct{}

	// deadMu guards dead: the key hashes VerifyKey has refused as unknown / revoked / expired.
	deadMu sync.Mutex
	dead   map[string]struct{}

	// Per-IP brake on lookups that FAILED (queue #356, specs/045 FR-9): probeMax is the budget of
	// failed lookups one client IP may spend per apiKeyProbeWindow; 0 = no brake (WithProbeBrake).
	probeMax int
	probeMu  sync.Mutex
	probes   map[string]probeWindow // client IP → its current fixed window
	// liveSeen holds the hash of every key that verified live in this process. A braked IP may still
	// present those (a real integration behind the same NAT); only never-seen strings are refused.
	liveSeen sync.Map // hash (string) → struct{}
}

// probeWindow is one IP's count of failed lookups since start (a fixed window).
type probeWindow struct {
	n     int
	start time.Time
}

// apiKeyDeadMax caps the dead-key memory (~100 bytes an entry).
const apiKeyDeadMax = 10000

const (
	// apiKeyProbeWindow is the span one IP's failed-lookup budget covers (fixed window; the 429
	// carries it as Retry-After).
	apiKeyProbeWindow = time.Minute
	// apiKeyProbeIPsMax caps the per-IP counters (~60 bytes an entry).
	apiKeyProbeIPsMax = 10000
)

// isDead reports whether hash was already refused for good (see markDead).
func (s *APIKeyService) isDead(hash string) bool {
	s.deadMu.Lock()
	defer s.deadMu.Unlock()
	_, ok := s.dead[hash]
	return ok
}

// markDead remembers a hash VerifyKey refused as unknown, revoked or expired, so an integration
// retrying that string no longer costs a sys_api_keys read and a WARN line per request (queue #155,
// specs/045 §5 — measured: a 64-worker storm of such a key tripled other users' latency compared
// with a storm of garbage JWTs, which never touch the DB). Remembering can never refuse a key that
// would verify, because each refusal is final: revoke has no undo, expires_at is fixed at issue,
// and an unknown string cannot become a key (the secret is 256 CSPRNG bits minted server-side).
// A live key is never remembered, so a revoke still lands on its next request.
func (s *APIKeyService) markDead(hash string) {
	s.deadMu.Lock()
	defer s.deadMu.Unlock()
	// ponytail: wholesale reset when full. Only a flood of ever-new strings fills it — that flood
	// is not brakeable by hash anyway (per-IP is the upgrade path) — and a real dead-key storm is
	// back in the map on its next request.
	if s.dead == nil || len(s.dead) >= apiKeyDeadMax {
		s.dead = make(map[string]struct{})
	}
	s.dead[hash] = struct{}{}
}

// probeBraked reports whether ip has spent its budget of failed lookups in the current window. A
// window that has passed counts as unspent. Only called with probeMax > 0 and a non-empty ip.
func (s *APIKeyService) probeBraked(ip string, now time.Time) bool {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	w, ok := s.probes[ip]
	return ok && now.Sub(w.start) < apiKeyProbeWindow && w.n >= s.probeMax
}

// chargeProbe is probeFailed behind the "brake armed and IP known" test.
func (s *APIKeyService) chargeProbe(ip string) {
	if s.probeMax > 0 && ip != "" {
		s.probeFailed(ip, s.now())
	}
}

// probeFailed charges ip one failed lookup (a read of sys_api_keys that found nothing usable).
func (s *APIKeyService) probeFailed(ip string, now time.Time) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	w, ok := s.probes[ip]
	if !ok || now.Sub(w.start) >= apiKeyProbeWindow {
		w = probeWindow{start: now}
	}
	w.n++
	// ponytail: wholesale reset when full, like markDead. Only a flood from > apiKeyProbeIPsMax
	// distinct addresses fills it; a per-IP brake cannot stop that anyway. Fixed window, so a prober
	// can spend up to 2x the budget across a window edge.
	if s.probes == nil || (!ok && len(s.probes) >= apiKeyProbeIPsMax) {
		s.probes = make(map[string]probeWindow)
	}
	s.probes[ip] = w
}

// deadKeyCount is the size of the dead-key memory (tests).
func (s *APIKeyService) deadKeyCount() int {
	s.deadMu.Lock()
	defer s.deadMu.Unlock()
	return len(s.dead)
}

// NewAPIKeyService wires the service. repo and roles must be non-nil; auditor defaults to a
// noop and logger to a nop until injected via WithAuditor / WithLogger.
func NewAPIKeyService(repo apiKeyRepo, roles roleValidator) *APIKeyService {
	return &APIKeyService{
		repo:    repo,
		roles:   roles,
		auditor: audit.NewNoopAuditor(),
		logger:  zap.NewNop(),
		now:     time.Now,
		genID:   uuid.NewString,
	}
}

// WithProbeBrake arms the per-IP brake on failed key lookups: after maxPerWindow lookups that read
// sys_api_keys and found nothing usable, the same client IP is refused (auth.ErrAPIKeyThrottled, no
// read) for any `nxk_` string that has not verified live before, until the window passes. <= 0 = off.
func (s *APIKeyService) WithProbeBrake(maxPerWindow int) *APIKeyService {
	s.probeMax = maxPerWindow
	return s
}

// WithAuditor injects the RM-19 audit dispatcher so issue/revoke emit meta-audit rows.
func (s *APIKeyService) WithAuditor(a audit.Auditor) *APIKeyService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithLogger injects the structured logger. Returns the service for fluent wiring.
func (s *APIKeyService) WithLogger(l *zap.Logger) *APIKeyService {
	if l != nil {
		s.logger = l
	}
	return s
}

// IssueAPIKeyInput is the service-side create payload. ActorID is the verified admin caller
// (recorded as created_by); ExpiresAt nil = the key never expires.
type IssueAPIKeyInput struct {
	Name      string
	RoleID    string
	ExpiresAt *time.Time
	ActorID   string
}

// IssueAPIKey validates, mints and persists a new key, returning the stored row plus the RAW
// secret — the ONLY time the secret ever exists outside the caller's hands (only its SHA-256
// is stored). Validation: name required (≤100 runes), role must exist IN the caller's tenant
// (the tenant-pinned lookup makes a foreign tenant's role a miss → 422 APIKEY_ROLE_INVALID),
// expiry (when given) must be in the future.
func (s *APIKeyService) IssueAPIKey(ctx context.Context, tenantID string, in IssueAPIKeyInput) (*auth.APIKey, string, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, "", invalid("APIKEY_NAME_REQUIRED", "Tên khoá API là bắt buộc")
	}
	if utf8.RuneCountInString(name) > apiKeyNameMaxRunes {
		return nil, "", invalid("APIKEY_NAME_TOO_LONG", "Tên khoá API tối đa %d ký tự", apiKeyNameMaxRunes)
	}
	roleID := strings.TrimSpace(in.RoleID)
	if roleID == "" {
		return nil, "", invalid("APIKEY_ROLE_INVALID", "roleId là bắt buộc — khoá API hành động theo quyền của một vai trò")
	}
	if _, err := s.roles.GetRoleByID(ctx, tenantID, roleID); err != nil {
		if errors.Is(err, auth.ErrRoleNotFound) {
			return nil, "", invalid("APIKEY_ROLE_INVALID", "roleId không hợp lệ hoặc không thuộc tổ chức")
		}
		return nil, "", fmt.Errorf("services.APIKeyService.IssueAPIKey: validate role: %w", err)
	}
	now := s.now()
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return nil, "", invalid("APIKEY_EXPIRY_INVALID", "expiresAt phải ở tương lai")
	}

	raw, prefix, err := newSecret(apiKeyPrefix)
	if err != nil {
		return nil, "", fmt.Errorf("services.APIKeyService.IssueAPIKey: generate secret: %w", err)
	}
	k := &auth.APIKey{
		ID:        s.genID(),
		TenantID:  tenantID,
		RoleID:    roleID,
		Name:      name,
		KeyPrefix: prefix,
		KeyHash:   auth.HashRefreshToken(raw), // SHA-256 hex — the shared hash-only-storage helper
		CreatedBy: in.ActorID,
		ExpiresAt: in.ExpiresAt,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, k); err != nil {
		return nil, "", fmt.Errorf("services.APIKeyService.IssueAPIKey: %w", err)
	}
	// RM-19 audit: after-image only (create). KeyHash is json:"-" so StructToMap drops it —
	// neither the secret nor its hash can ever reach the audit diff (same mechanism that keeps
	// the SSO client_secret out).
	emitMetaAudit(ctx, s.auditor, tenantID, "__api_key__", "create", nil, k)
	s.logger.Info("services.APIKeyService: api key issued",
		zap.String("tenant_id", tenantID), zap.String("api_key_id", k.ID),
		zap.String("role_id", roleID), zap.String("actor_user_id", in.ActorID))
	return k, raw, nil
}

// ListAPIKeys returns every key of the tenant — metadata only (the entity carries no secret
// and its hash is json:"-"), revoked rows included for audit visibility.
func (s *APIKeyService) ListAPIKeys(ctx context.Context, tenantID string) ([]auth.APIKey, error) {
	keys, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.APIKeyService.ListAPIKeys: %w", err)
	}
	return keys, nil
}

// RevokeAPIKey soft-revokes a key (sets revoked_at; the row stays for audit). Unknown or
// cross-tenant id → 404 APIKEY_NOT_FOUND. Revoking an already-revoked key is an idempotent
// success (no second audit row). Takes effect on the NEXT verify — there is no verify cache.
func (s *APIKeyService) RevokeAPIKey(ctx context.Context, tenantID, id string) error {
	k, err := s.repo.GetByID(ctx, tenantID, strings.TrimSpace(id))
	if err != nil {
		if errors.Is(err, auth.ErrAPIKeyNotFound) {
			return invalid("APIKEY_NOT_FOUND", "Không tìm thấy khoá API")
		}
		return fmt.Errorf("services.APIKeyService.RevokeAPIKey: %w", err)
	}
	flipped, err := s.repo.Revoke(ctx, tenantID, k.ID, s.now())
	if err != nil {
		return fmt.Errorf("services.APIKeyService.RevokeAPIKey: %w", err)
	}
	if flipped {
		after := *k
		at := s.now()
		after.RevokedAt = &at
		emitMetaAudit(ctx, s.auditor, tenantID, "__api_key__", "revoke", k, &after)
		s.logger.Info("services.APIKeyService: api key revoked",
			zap.String("tenant_id", tenantID), zap.String("api_key_id", k.ID))
	}
	return nil
}

// VerifyKey is the hot-path credential check: SHA-256 the presented raw key, point-read the row
// by its unique hash index (constant work — the comparison happens on the digest, so there is
// no timing oracle over the raw secret), enforce not-revoked + not-expired fail-closed, then
// mint the Claims. All failures collapse into auth.ErrAPIKeyInvalid (the middleware turns any
// error into the same 401 the JWT path produces); the REAL reason is logged server-side only.
// Revocation freshness is immediate: every verify hits the DB row (no cache by design — a
// revoked credential must die on the next request, mirroring the W1 claim-freshness rule).
func (s *APIKeyService) VerifyKey(ctx context.Context, raw string) (*auth.Claims, error) {
	if !strings.HasPrefix(raw, apiKeyPrefix) {
		// Defense-in-depth: the composite verifier only routes nxk_ credentials here, but a
		// direct caller must still never get a non-key string looked up.
		return nil, auth.ErrAPIKeyInvalid
	}
	hash := auth.HashRefreshToken(raw)
	if s.isDead(hash) {
		return nil, auth.ErrAPIKeyInvalid
	}
	// Per-IP brake (#356): a client that keeps presenting strings that read the table and fail — a
	// prober changing the string every request, which the dead-key memory above cannot catch — is
	// refused WITHOUT the read once its budget is spent. A string that verified live before is exempt
	// (a real integration behind the same NAT address) and still takes the full fresh read below, so a
	// revoke lands on its next request exactly as before.
	ip := auth.ClientIPFrom(ctx)
	if s.probeMax > 0 && ip != "" {
		if _, known := s.liveSeen.Load(hash); !known && s.probeBraked(ip, s.now()) {
			return nil, auth.ErrAPIKeyThrottled
		}
	}
	k, err := s.repo.FindByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, auth.ErrAPIKeyInvalid) {
			s.markDead(hash)
			s.chargeProbe(ip)
			return nil, auth.ErrAPIKeyInvalid
		}
		// Infrastructure failure — fail closed (the middleware answers 401). Wrapped so the
		// server-side log keeps the real cause.
		return nil, fmt.Errorf("services.APIKeyService.VerifyKey: %w", err)
	}
	now := s.now()
	if k.RevokedAt != nil {
		s.logger.Warn("services.APIKeyService: revoked api key presented",
			zap.String("tenant_id", k.TenantID), zap.String("api_key_id", k.ID))
		s.markDead(hash)
		s.chargeProbe(ip)
		return nil, auth.ErrAPIKeyInvalid
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		s.logger.Warn("services.APIKeyService: expired api key presented",
			zap.String("tenant_id", k.TenantID), zap.String("api_key_id", k.ID))
		s.markDead(hash)
		s.chargeProbe(ip)
		return nil, auth.ErrAPIKeyInvalid
	}
	if s.probeMax > 0 {
		s.liveSeen.Store(hash, struct{}{})
	}
	s.touch(k, now)
	return &auth.Claims{
		Subject: auth.Subject{
			// UserID = the key's own id: machine principal, attributed as itself (see file doc).
			UserID:   k.ID,
			TenantID: k.TenantID, // ALWAYS the row's tenant — never anything client-supplied
			RoleID:   k.RoleID,
			IsAdmin:  false, // hard privilege floor, regardless of anything in the row/role
		},
		TokenType: auth.TokenTypeAPIKey,
	}, nil
}

// touch schedules the last_used_at write-behind: THROTTLED (at most one write per key per
// apiKeyTouchInterval per process) and ASYNC (fire-and-forget goroutine with its own bounded
// timeout, detached from the request context) so the hot path never pays a synchronous UPDATE
// per request (write-amplification guard). Two concurrent requests racing the throttle window
// may both schedule a write — benign (the UPDATE is idempotent) and self-limiting. A failed
// write only logs: last_used_at is best-effort telemetry, never an auth decision.
func (s *APIKeyService) touch(k *auth.APIKey, now time.Time) {
	if v, ok := s.lastTouch.Load(k.ID); ok {
		if last, ok := v.(time.Time); ok && now.Sub(last) < apiKeyTouchInterval {
			return
		}
	}
	s.lastTouch.Store(k.ID, now)
	tenantID, id := k.TenantID, k.ID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), apiKeyTouchTimeout)
		defer cancel()
		if err := s.repo.TouchLastUsed(ctx, tenantID, id, now); err != nil {
			s.logger.Warn("services.APIKeyService.touch: last_used_at write failed",
				zap.String("tenant_id", tenantID), zap.String("api_key_id", id), zap.Error(err))
		}
		if s.touchDone != nil {
			s.touchDone <- struct{}{}
		}
	}()
}

// newSecret mints a raw credential `<namespace><43 base64url chars>` (nxk_ API keys, nxm_ MCP
// tokens) from crypto/rand and returns it with its public identification prefix (the first
// apiKeyPrefixChars of the random part). A CSPRNG failure aborts issuance — never a weak key.
func newSecret(namespace string) (raw, prefix string, err error) {
	buf := make([]byte, apiKeySecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("crypto/rand read: %w", err)
	}
	random := base64.RawURLEncoding.EncodeToString(buf)
	return namespace + random, random[:apiKeyPrefixChars], nil
}

// ---- composite Bearer verifier ------------------------------------------------------------

// jwtAccessVerifier is the JWT half of the composite — satisfied by *auth.TokenService.
type jwtAccessVerifier interface {
	VerifyAccessToken(tok string) (*auth.Claims, error)
}

// CompositeVerifier routes a presented Bearer credential by namespace: `nxk_...` → API-key
// verify (DB-backed), anything else → the existing JWT VerifyAccessToken, UNCHANGED. It
// implements both the legacy context-free interface the middleware/router already accept and
// the context-aware upgrade (VerifyAccessTokenContext) the middleware prefers, so wiring it in
// place of *auth.TokenService requires no signature changes anywhere.
//
// Fall-through proof: a compact JWT is three base64url segments whose first is the encoding of
// `{"alg"` — it always starts "eyJ", never "nxk_". The prefix routing therefore cannot divert
// a real JWT into the key path, and a real key (which is not a JWT) could only ever fail JWT
// verification anyway — the two credential spaces are disjoint by construction.
type CompositeVerifier struct {
	jwt  jwtAccessVerifier
	keys *APIKeyService
	// previews, when set (WithPreview), is the ONE deliberately opened door for preview-as-role
	// tokens (specs/064 S9-1): a JWT the access verifier rejects as ErrWrongTokenType is handed to
	// it, and it accepts typ=preview only. nil ⇒ preview tokens 401 like any wrong-type JWT.
	previews previewVerifier
}

// previewVerifier is the preview half of the Bearer verifier (*PreviewService).
type previewVerifier interface {
	Verify(ctx context.Context, tok string) (*auth.Claims, error)
}

// WithPreview opens the Bearer chain to preview-as-role tokens (specs/064-preview-as-role-sample-data
// S9-1). Only a token the JWT access verifier refused with auth.ErrWrongTokenType is routed there,
// so access tokens and API keys keep their exact paths.
func (v *CompositeVerifier) WithPreview(p previewVerifier) *CompositeVerifier {
	v.previews = p
	return v
}

// NewCompositeVerifier wires the composite. Both halves are required.
func NewCompositeVerifier(jwt jwtAccessVerifier, keys *APIKeyService) *CompositeVerifier {
	return &CompositeVerifier{jwt: jwt, keys: keys}
}

// VerifyAccessToken satisfies the legacy context-free verifier interface. The API-key lookup
// needs a context; callers that can supply the request context use VerifyAccessTokenContext
// (the auth middleware does) — this fallback uses Background, bounded by the repo call's own
// DB timeouts.
func (v *CompositeVerifier) VerifyAccessToken(tok string) (*auth.Claims, error) {
	return v.VerifyAccessTokenContext(context.Background(), tok)
}

// VerifyAccessTokenContext is the context-aware verify the auth middleware prefers (it passes
// the request context so the key lookup honors cancellation/deadlines).
func (v *CompositeVerifier) VerifyAccessTokenContext(ctx context.Context, tok string) (*auth.Claims, error) {
	if strings.HasPrefix(tok, MCPTokenPrefix) {
		// An MCP token is valid on /mcp only (issue #3): on every API route it is a bad token.
		return nil, auth.ErrTokenInvalid
	}
	if strings.HasPrefix(tok, apiKeyPrefix) {
		return v.keys.VerifyKey(ctx, tok)
	}
	claims, err := v.jwt.VerifyAccessToken(tok)
	if err != nil && v.previews != nil && errors.Is(err, auth.ErrWrongTokenType) {
		return v.previews.Verify(ctx, tok)
	}
	return claims, err
}
