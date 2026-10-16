package services

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/audit"
	"nexus/platform/internal/auth"
)

// ErrSSOUnavailable is returned by every SSO use case when the service was constructed without
// WithSealer (SSO_SECRET_ENC_KEY unset at boot — config.LoadSSOConfig().Enabled == false).
// Mirrors ErrMFAUnavailable / the resetRepo == nil branches elsewhere in this package.
var ErrSSOUnavailable = errors.New("services: sso support not configured")

// ErrSSOStateInvalid is the single, generic failure for the /callback `state` param: missing,
// malformed, tampered (GCM authentication failure), or expired. Folded into one error (mirrors
// ErrResetTokenInvalid / auth.ErrMFAPendingInvalid) so a probed/forged state reveals nothing
// about why it failed.
var ErrSSOStateInvalid = errors.New("services: sso state invalid")

// ErrSSOStateMismatch is the SSO-STATE-COOKIE (login-CSRF, #8 post-review LOW-5) rejection: the
// sealed `state` opened fine (not tampered, not expired) but it is NOT bound to the browser
// session presenting it — the `sso_state` cookie set at /start is missing, or its value does not
// match the state's nonce. Distinct from ErrSSOStateInvalid on purpose: a valid-but-unbound state
// is precisely the attacker-initiated-flow signature (someone else's /start, this browser's
// /callback), and folding it into the malformed-state error would hide that signal from both the
// server-side log and the FE ("please restart login from THIS browser" is the actionable cue).
var ErrSSOStateMismatch = errors.New("services: sso state not bound to this browser session")

// ErrSSOExchangeFailed is returned when the authorization-code-for-token exchange itself fails
// (a bad, expired, or already-redeemed `code`; the IdP's token endpoint rejecting the request).
var ErrSSOExchangeFailed = errors.New("services: sso token exchange failed")

// ErrSSOIDTokenInvalid is returned when the exchanged id_token fails verification: bad
// signature, wrong issuer/audience, expired, an empty `sub` claim, or a `nonce` claim that does
// not match the nonce embedded in the signed state (../specs/004-sso-oidc/spec.md §"#8" MUST-FIX:
// nonce must be checked against the STATE's nonce, not merely present).
var ErrSSOIDTokenInvalid = errors.New("services: sso id_token invalid")

// ErrSSOAccountLinkDenied is the SAFE-DEFAULT (../specs/004-sso-oidc/spec.md §"#8" CONFIRM WITH
// HUMAN item c): an IdP-asserted email matches an EXISTING local user that has never linked
// this (provider, sub) before. Auto-linking by email is refused — a rogue/misconfigured IdP
// asserting `email: admin@tenant` must not be able to take over an existing account. An admin
// must link the account explicitly (out of scope for this slice — see docs).
var ErrSSOAccountLinkDenied = errors.New("services: sso account link denied")

// ErrSSOProviderDiscoveryFailed is returned by StartLogin when the provider's OIDC discovery
// fetch itself fails (reviewer FIX 6, LOW): a typo'd-but-well-formed https:// issuer, an
// unreachable/misconfigured one, or one whose discovery document is malformed. Previously this
// fell through to the handler's generic default branch (500 SSO_OPERATION_FAILED),
// indistinguishable from a genuine internal fault — this gives the admin/caller a specific,
// actionable signal instead ("we could not reach/parse your IdP's discovery document").
var ErrSSOProviderDiscoveryFailed = errors.New("services: sso provider discovery failed")

// ErrSSOUserNotProvisioned is the SAFE-DEFAULT (../specs/004-sso-oidc/spec.md §"#8" CONFIRM WITH
// HUMAN item b): no local user matches the IdP-asserted email AND the provider's
// JITProvisioning flag is off (the default) — or the email claim itself is blank (never safe to
// resolve), or a previously-linked identity's local user is no longer active (orphaned link,
// fails clean rather than 500).
var ErrSSOUserNotProvisioned = errors.New("services: sso user not provisioned")

// errOIDCExchangeFailed / errOIDCIDTokenInvalid are the oidcClient-layer sentinels an
// implementation (real or fake) wraps its failures in, so HandleCallback can classify them into
// the two distinct public errors above via errors.Is without depending on go-oidc's own error
// types.
var errOIDCExchangeFailed = errors.New("oidc: token exchange failed")
var errOIDCIDTokenInvalid = errors.New("oidc: id_token invalid")

// ssoProviderRepo is the subset of auth.SSOProviderRepository the SSO flows need. Consumer-side
// interface (mirrors mfaRepo/resetRepo) so the service stays unit-testable with an in-memory
// fake.
type ssoProviderRepo interface {
	Create(ctx context.Context, p *auth.SSOProvider) error
	List(ctx context.Context, tenantID string) ([]*auth.SSOProvider, error)
	GetByID(ctx context.Context, tenantID, id string) (*auth.SSOProvider, error)
	GetEnabledByID(ctx context.Context, tenantID, id string) (*auth.SSOProvider, error)
	GetEnabledByIssuer(ctx context.Context, tenantID, issuer string) (*auth.SSOProvider, error)
	// SetEnabled flips ONLY the `enabled` column (reviewer FIX 1 — see
	// auth.SSOProviderRepository.SetEnabled's doc for why this must be a single-column update,
	// not a struct-based Updates call).
	SetEnabled(ctx context.Context, tenantID, id string, enabled bool) error
	// SetTrustIdpMfa flips ONLY the `trust_idp_mfa` column (SSO-MFA-POLICY — same
	// single-column-update discipline as SetEnabled; a struct-based Updates would make
	// "turn trust back OFF" a silent no-op via GORM's zero-value skip).
	SetTrustIdpMfa(ctx context.Context, tenantID, id string, trust bool) error
	Delete(ctx context.Context, tenantID, id string) error
}

// ssoIdentityRepo is the subset of auth.SSOIdentityRepository the SSO flows need.
type ssoIdentityRepo interface {
	FindBySubject(ctx context.Context, tenantID, providerID, subject string) (*auth.SSOIdentity, error)
	// CreateUserWithIdentity atomically JIT-provisions a local user AND its linking identity —
	// see auth.SSOIdentityRepository.CreateUserWithIdentity's doc for why this must be one
	// transaction, not two independent writes.
	CreateUserWithIdentity(ctx context.Context, u *auth.User, identity *auth.SSOIdentity) error
}

// oidcClaims is the minimal set of ID-token claims HandleCallback needs, extracted by the
// oidcClient after issuer/audience/expiry/signature verification has already passed. Nonce is
// the raw `nonce` claim as asserted by the IdP — HandleCallback (not the client) compares it
// against the signed state's nonce, which is what makes that binding check directly
// unit-testable with a fake client (sso_service_test.go).
type oidcClaims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Nonce         string
}

// oidcClient abstracts the OIDC authorization-code exchange so SSOService's business logic
// (deny-link, JIT on/off, tenant pin, nonce binding) is unit-testable with a fake
// (sso_service_test.go's fakeOIDCClient), while production wires the real go-oidc + oauth2
// implementation (sso_oidc_client.go) exercised end-to-end by the httptest-stubbed-IdP
// integration suite.
type oidcClient interface {
	// AuthCodeURL builds the IdP authorization-endpoint redirect URL for /start.
	AuthCodeURL(ctx context.Context, provider *auth.SSOProvider, clientSecret, redirectURI, state, nonce string) (string, error)
	// Exchange trades an authorization code for a verified ID token's claims. Implementations
	// wrap a pre-verification failure in errOIDCExchangeFailed and a post-exchange
	// signature/issuer/audience/expiry verification failure in errOIDCIDTokenInvalid so the
	// caller can classify them via errors.Is.
	Exchange(ctx context.Context, provider *auth.SSOProvider, clientSecret, redirectURI, code string) (*oidcClaims, error)
}

// SSOService implements the ENG-AUTH2 #8 OIDC authorization-code flow
// (../specs/004-sso-oidc/spec.md §"#8"): admin CRUD over per-tenant IdP registrations, and the
// public start/callback login flow that mints a session identical in shape to a password login
// (it reaches into authSvc's unexported issueSessionForSubject/users — same package, no new
// exported surface on AuthService). It holds no per-request state and is safe for concurrent use.
type SSOService struct {
	authSvc      *AuthService
	providers    ssoProviderRepo
	identities   ssoIdentityRepo
	sealer       ssoSealer // nil until WithSealer is called (SSO_SECRET_ENC_KEY configured)
	redirectBase string
	oidc         oidcClient
	newID        func() string
	now          func() time.Time
	auditor      audit.Auditor // RM-19 SSO-ADMIN-AUDIT; noop until WithAuditor (main.go) wires it
	// handoffs (SSO-FE-REDIRECT) backs the one-time handoff codes bridging /callback → the
	// frontend /exchange XHR. nil until WithHandoffStore (main.go wires it unconditionally —
	// it needs only the DB); StartHandoff/ExchangeHandoff answer ErrSSOUnavailable while nil.
	handoffs ssoHandoffRepo
}

// ssoHandoffRepo is the narrow persistence surface StartHandoff/ExchangeHandoff need
// (SSO-FE-REDIRECT) — satisfied by auth.SSOHandoffTokenRepository, faked in tests.
type ssoHandoffRepo interface {
	Store(ctx context.Context, t *auth.SSOHandoffToken) error
	FindByHash(ctx context.Context, tokenHash string) (*auth.SSOHandoffToken, error)
	ConsumeByHash(ctx context.Context, tenantID, tokenHash string) (bool, error)
}

// NewSSOService wires an SSOService over an AuthService (session issuance + user lookups) and
// the provider/identity repositories. SSO stays entirely inert (sealer == nil) until
// WithSealer is called — every method that needs the sealer returns ErrSSOUnavailable until then,
// mirroring MFA's WithMFA gating.
func NewSSOService(authSvc *AuthService, providers ssoProviderRepo, identities ssoIdentityRepo) *SSOService {
	return &SSOService{
		authSvc:    authSvc,
		providers:  providers,
		identities: identities,
		oidc:       newRealOIDCClient(),
		newID:      uuid.NewString,
		now:        time.Now,
		auditor:    audit.NewNoopAuditor(),
	}
}

// WithAuditor injects the RM-19 audit dispatcher so provider create/policy-flip/delete land in
// the same append-only trail as entity/field/role/field-ACL changes (SSO-ADMIN-AUDIT). main.go
// calls this unconditionally; a nil auditor is a no-op that leaves the noop default in place
// (every existing test fixture that skips it keeps recording to /dev/null, byte-identical).
func (s *SSOService) WithAuditor(a audit.Auditor) *SSOService {
	if a != nil {
		s.auditor = a
	}
	return s
}

// WithHandoffStore injects the sso_handoff_tokens repository (SSO-FE-REDIRECT). main.go calls
// this unconditionally — the store needs only the database, exactly like the MFA enrollment
// reader. A nil store is a no-op, keeping every existing test fixture byte-identical.
func (s *SSOService) WithHandoffStore(r ssoHandoffRepo) *SSOService {
	if r != nil {
		s.handoffs = r
	}
	return s
}

// WithSealer injects the AES-256-GCM sealer (client_secret-at-rest + signed state) and the
// backend's own redirect base URL. A nil sealer is a no-op (leaves SSO inert) — main.go calls
// this only when config.LoadSSOConfig().Enabled.
func (s *SSOService) WithSealer(sealer ssoSealer, redirectBase string) *SSOService {
	if sealer == nil {
		return s
	}
	s.sealer = sealer
	s.redirectBase = redirectBase
	return s
}

func (s *SSOService) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *SSOService) genID() string {
	if s.newID != nil {
		return s.newID()
	}
	return uuid.NewString()
}

// redirectURI builds the fixed OAuth2 redirect_uri every provider on this deployment shares:
// "<SSO_REDIRECT_BASE>/auth/sso/callback".
func (s *SSOService) callbackRedirectURI() string {
	return strings.TrimRight(s.redirectBase, "/") + "/auth/sso/callback"
}

// --- Admin CRUD (ENG-AUTH2 #8 item 6) -----------------------------------------------------

// CreateSSOProviderInput is the admin POST /admin/sso/providers payload.
type CreateSSOProviderInput struct {
	Issuer          string
	ClientID        string
	ClientSecret    string
	DisplayName     string
	Enabled         *bool // nil -> default true
	JITProvisioning bool
}

// CreateProvider registers a new OIDC provider for tenantID. The issuer must be a well-formed
// https:// URL — checked HERE at creation time (deliberately NOT via a live discovery fetch,
// which would make admin CRUD network-dependent and untestable without a reachable IdP;
// discovery is deferred to StartLogin/HandleCallback's first real use, per
// ../specs/004-sso-oidc/spec.md §"#8"). The client secret is sealed (AES-256-GCM) before
// persisting and is NEVER returned in the clear again (auth.SSOProvider.ClientSecretEnc carries
// json:"-").
func (s *SSOService) CreateProvider(ctx context.Context, tenantID string, in CreateSSOProviderInput) (*auth.SSOProvider, error) {
	if s.sealer == nil {
		return nil, ErrSSOUnavailable
	}
	issuer := strings.TrimSpace(in.Issuer)
	clientID := strings.TrimSpace(in.ClientID)
	if issuer == "" || clientID == "" || in.ClientSecret == "" {
		return nil, invalid("SSO_PROVIDER_FIELD_REQUIRED", "issuer, clientId, clientSecret là bắt buộc")
	}
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, invalid("SSO_PROVIDER_ISSUER_INVALID", "issuer phải là URL https hợp lệ")
	}

	sealedSecret, err := s.sealer.Seal([]byte(in.ClientSecret))
	if err != nil {
		return nil, fmt.Errorf("services.SSOService.CreateProvider: seal secret: %w", err)
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	p := &auth.SSOProvider{
		ID:              s.genID(),
		TenantID:        tenantID,
		Issuer:          issuer,
		ClientID:        clientID,
		ClientSecretEnc: sealedSecret,
		DisplayName:     strings.TrimSpace(in.DisplayName),
		Enabled:         enabled,
		JITProvisioning: in.JITProvisioning,
	}
	if err := s.providers.Create(ctx, p); err != nil {
		if errors.Is(err, auth.ErrSSOProviderAlreadyExists) {
			return nil, invalid("SSO_PROVIDER_ALREADY_EXISTS", "Provider cho issuer này đã tồn tại")
		}
		return nil, fmt.Errorf("services.SSOService.CreateProvider: %w", err)
	}
	// RM-19 (SSO-ADMIN-AUDIT): audit the committed registration. After-only diff; the client
	// secret is stripped by auth.SSOProvider's json:"-" (see emitMetaAudit → StructToMap).
	// Fail-open — the provider already persisted.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.SSOProviderMeta, audit.ActionCreate, nil, p)
	return p, nil
}

// ListProviders returns every provider registered for tenantID (enabled and disabled).
func (s *SSOService) ListProviders(ctx context.Context, tenantID string) ([]*auth.SSOProvider, error) {
	list, err := s.providers.List(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("services.SSOService.ListProviders: %w", err)
	}
	return list, nil
}

// UpdateProviderPolicy applies a partial update over the ONLY two mutable provider flags
// (narrow-surface contract, api-conventions.md §3c.4 — issuer/clientId/clientSecret stay
// delete+recreate): `enabled` (reviewer FIX 1 — admin re-enable/disable) and `trustIdpMfa`
// (SSO-MFA-POLICY — per-provider "trust the IdP's own MFA, skip local MFA" opt-in). A nil
// pointer leaves that flag untouched; each non-nil flag is written via its own single-column
// repository setter (never a struct-based Updates — GORM's zero-value skip would turn
// "set false" into a silent no-op, which on trustIdpMfa is a security bug). Returns the row as
// it now stands. Returns auth.ErrSSOProviderNotFound for an unknown id or one belonging to
// another tenant. The handler guarantees at least one flag is non-nil (422 otherwise), but a
// both-nil call is still safe here: it degrades to a tenant-pinned read.
func (s *SSOService) UpdateProviderPolicy(ctx context.Context, tenantID, id string, enabled, trustIdpMfa *bool) (*auth.SSOProvider, error) {
	// Snapshot the before-image for the RM-19 audit diff (SSO-ADMIN-AUDIT). Reading first also
	// makes NotFound uniform (previously it surfaced only from the first setter / the reload).
	before, err := s.providers.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return nil, auth.ErrSSOProviderNotFound
		}
		return nil, fmt.Errorf("services.SSOService.UpdateProviderPolicy: load before: %w", err)
	}
	if enabled != nil {
		if err := s.providers.SetEnabled(ctx, tenantID, id, *enabled); err != nil {
			if errors.Is(err, auth.ErrSSOProviderNotFound) {
				return nil, auth.ErrSSOProviderNotFound
			}
			return nil, fmt.Errorf("services.SSOService.UpdateProviderPolicy: set enabled: %w", err)
		}
	}
	if trustIdpMfa != nil {
		if err := s.providers.SetTrustIdpMfa(ctx, tenantID, id, *trustIdpMfa); err != nil {
			if errors.Is(err, auth.ErrSSOProviderNotFound) {
				return nil, auth.ErrSSOProviderNotFound
			}
			return nil, fmt.Errorf("services.SSOService.UpdateProviderPolicy: set trustIdpMfa: %w", err)
		}
		// SSO-MFA-POLICY security review LOW-1: the FLIP is rare + high-signal (unlike the
		// per-login skip which stays Info to avoid alert fatigue) — log it at Warn so a change to
		// "this IdP's MFA is trusted, local MFA skipped" surfaces in monitoring. Kept as a live
		// monitoring signal alongside the RM-19 audit row (the durable, queryable backstop),
		// which now lands below in this same method (SSO-ADMIN-AUDIT).
		s.authSvc.logger.Warn("services.SSOService.UpdateProviderPolicy: trust_idp_mfa changed — SSO logins via this provider will SKIP local MFA when true",
			zap.String("tenant_id", tenantID),
			zap.String("provider_id", id),
			zap.Bool("trust_idp_mfa", *trustIdpMfa),
		)
	}
	p, err := s.providers.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return nil, auth.ErrSSOProviderNotFound
		}
		return nil, fmt.Errorf("services.SSOService.UpdateProviderPolicy: reload: %w", err)
	}
	// RM-19 (SSO-ADMIN-AUDIT): audit the policy flip only when a flag was actually requested
	// (a both-nil call degrades to a read — nothing to record). ComputeDiff keeps only the keys
	// that changed (enabled / trustIdpMfa / updatedAt). Fail-open — the flip already committed.
	if enabled != nil || trustIdpMfa != nil {
		emitMetaAudit(ctx, s.auditor, tenantID, audit.SSOProviderMeta, audit.ActionUpdate, before, p)
	}
	return p, nil
}

// DeleteProvider hard-deletes a tenant-scoped provider (and, via FK CASCADE, every identity
// linked to it — revoking that provider's sessions entirely). Returns auth.ErrSSOProviderNotFound
// for an unknown id or one belonging to another tenant.
func (s *SSOService) DeleteProvider(ctx context.Context, tenantID, id string) error {
	// Snapshot the before-image for the RM-19 audit diff (SSO-ADMIN-AUDIT) before the row is
	// gone. Reading first also keeps NotFound uniform with the other admin ops.
	before, err := s.providers.GetByID(ctx, tenantID, id)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return auth.ErrSSOProviderNotFound
		}
		return fmt.Errorf("services.SSOService.DeleteProvider: load before: %w", err)
	}
	if err := s.providers.Delete(ctx, tenantID, id); err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return auth.ErrSSOProviderNotFound
		}
		return fmt.Errorf("services.SSOService.DeleteProvider: %w", err)
	}
	// RM-19 (SSO-ADMIN-AUDIT): before-only diff (delete). Fail-open — the row is already gone.
	emitMetaAudit(ctx, s.auditor, tenantID, audit.SSOProviderMeta, audit.ActionDelete, before, nil)
	return nil
}

// --- Public login flow (ENG-AUTH2 #8 item 3) -----------------------------------------------

// resolveProvider resolves the `provider` query param either as a provider id or, when that
// misses, as a literal issuer string match (the brief's "id-hoặc-issuer-slug" — sso_providers
// carries no separate slug column, so the issuer URL itself doubles as the human-referenceable
// slug). Both lookups are enabled-only and tenant-pinned.
func (s *SSOService) resolveProvider(ctx context.Context, tenantID, param string) (*auth.SSOProvider, error) {
	param = strings.TrimSpace(param)
	if param == "" {
		return nil, auth.ErrSSOProviderNotFound
	}
	if p, err := s.providers.GetEnabledByID(ctx, tenantID, param); err == nil {
		return p, nil
	} else if !errors.Is(err, auth.ErrSSOProviderNotFound) {
		return nil, fmt.Errorf("services.SSOService.resolveProvider: %w", err)
	}
	p, err := s.providers.GetEnabledByIssuer(ctx, tenantID, param)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return nil, auth.ErrSSOProviderNotFound
		}
		return nil, fmt.Errorf("services.SSOService.resolveProvider: %w", err)
	}
	return p, nil
}

// PublicSSOProvider is the pre-auth login page's projection of one ENABLED provider —
// the id (usable as /start's ?provider=), a display name, and the issuer (lets the FE
// brand well-known IdPs, e.g. accounts.google.com → a Google button). No secrets, no flags.
type PublicSSOProvider struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Issuer      string `json:"issuer"`
}

// PublicProviders lists the tenant's ENABLED providers for the login page
// (SSO-PROVIDER-PUBLIC-LIST — Founder accepted the enumeration tradeoff 2026-07-25: the
// login page needs a real "Đăng nhập bằng Google" button, and the issuer becomes visible
// the moment /start redirects anyway). Anti-enumeration on the TENANT still holds: an
// unknown slug returns the SAME empty list a provider-less tenant does.
func (s *SSOService) PublicProviders(ctx context.Context, tenantSlug string) ([]PublicSSOProvider, error) {
	if s.sealer == nil || strings.TrimSpace(s.redirectBase) == "" {
		return nil, ErrSSOUnavailable
	}
	tenant, err := s.authSvc.users.ResolveTenantBySlug(ctx, strings.TrimSpace(tenantSlug))
	if err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			return []PublicSSOProvider{}, nil
		}
		return nil, fmt.Errorf("services.SSOService.PublicProviders: resolve tenant: %w", err)
	}
	rows, err := s.providers.List(ctx, tenant.ID)
	if err != nil {
		return nil, fmt.Errorf("services.SSOService.PublicProviders: list providers: %w", err)
	}
	out := make([]PublicSSOProvider, 0, len(rows))
	for _, p := range rows {
		if !p.Enabled {
			continue
		}
		name := strings.TrimSpace(p.DisplayName)
		if name == "" {
			name = p.Issuer
		}
		out = append(out, PublicSSOProvider{ID: p.ID, DisplayName: name, Issuer: p.Issuer})
	}
	return out, nil
}

// StartLogin resolves (tenantSlug, providerParam) and returns the IdP authorization-endpoint
// redirect URL PLUS the flow's nonce. The tenant is FIXED here from the path slug and carried
// forward opaquely inside the signed state — /callback never re-reads it from anywhere else
// (../specs/004-sso-oidc/spec.md §"#8" MUST-FIX). An unknown tenant slug is folded into the SAME
// auth.ErrSSOProviderNotFound an unknown provider produces (anti-enumeration: a caller cannot
// distinguish "no such tenant" from "tenant exists but no such provider").
//
// SSO-STATE-COOKIE: the returned nonce is the SAME value sealed inside the state (and sent to
// the IdP as the OIDC `nonce` param — it is browser-visible by design, never a server secret).
// The HTTP handler sets it as an HttpOnly cookie so HandleCallback can require the callback to
// arrive from the SAME browser session that initiated /start (login-CSRF bind).
func (s *SSOService) StartLogin(ctx context.Context, tenantSlug, providerParam string) (redirectURL, nonce string, err error) {
	if s.sealer == nil || strings.TrimSpace(s.redirectBase) == "" {
		return "", "", ErrSSOUnavailable
	}

	tenant, err := s.authSvc.users.ResolveTenantBySlug(ctx, strings.TrimSpace(tenantSlug))
	if err != nil {
		if errors.Is(err, auth.ErrTenantNotFound) {
			return "", "", auth.ErrSSOProviderNotFound
		}
		return "", "", fmt.Errorf("services.SSOService.StartLogin: resolve tenant: %w", err)
	}

	provider, err := s.resolveProvider(ctx, tenant.ID, providerParam)
	if err != nil {
		return "", "", err
	}

	nonce, err = newResetToken()
	if err != nil {
		return "", "", fmt.Errorf("services.SSOService.StartLogin: generate nonce: %w", err)
	}
	st := ssoState{TenantID: tenant.ID, ProviderID: provider.ID, Nonce: nonce, ExpiresAt: s.clock().Add(ssoStateTTL)}
	sealedState, err := s.sealState(st)
	if err != nil {
		return "", "", fmt.Errorf("services.SSOService.StartLogin: %w", err)
	}

	secretRaw, err := s.sealer.Open(provider.ClientSecretEnc)
	if err != nil {
		return "", "", fmt.Errorf("services.SSOService.StartLogin: open client secret: %w", err)
	}

	redirectURL, err = s.oidc.AuthCodeURL(ctx, provider, string(secretRaw), s.callbackRedirectURI(), sealedState, nonce)
	if err != nil {
		// FIX 6: classify a discovery failure into its own public sentinel instead of falling
		// through to the generic 500 branch (errOIDCDiscoveryFailed is set by realOIDCClient's
		// provider() — sso_oidc_client.go — and propagates unwrapped through AuthCodeURL).
		if errors.Is(err, errOIDCDiscoveryFailed) {
			return "", "", ErrSSOProviderDiscoveryFailed
		}
		return "", "", fmt.Errorf("services.SSOService.StartLogin: build auth url: %w", err)
	}
	return redirectURL, nonce, nil
}

// HandleCallback is the /callback resolution (../specs/004-sso-oidc/spec.md §"#8" US1, thứ tự
// load-bearing):
//  1. Open the signed state -> {tenant, provider, nonce, expiry}. This is the ONLY source of
//     tenant/provider; the callback's own query string carries neither. THEN (SSO-STATE-COOKIE,
//     login-CSRF bind — #8 post-review LOW-5) require browserNonce — the `sso_state` cookie the
//     handler set at /start — to match the state's nonce, constant-time. A signed state is
//     tamper-proof but was NOT previously bound to the browser that initiated /start: an attacker
//     could run their OWN /start and trick a victim into opening /callback with the attacker's
//     code+state, silently logging the victim into the attacker's account. Missing cookie or
//     mismatch -> ErrSSOStateMismatch. This is an ADDITIONAL bind on top of the sealed-state
//     tamper/expiry checks, not a replacement.
//  2. Re-resolve the provider (enabled-only, tenant-pinned) — an admin disabling it between
//     /start and /callback must not let the flow complete.
//  3. Exchange the code, verify the id_token (issuer/audience/expiry/signature — inside the
//     oidcClient), and check its `nonce` claim against the state's nonce HERE (binding check).
//  4. Resolve identity by (tenant, provider, sub). Found -> load the linked user, tenant-pinned
//     + must be active (an orphaned link whose user was deactivated fails clean, not 500) -> if
//     the user has LOCAL MFA enabled, return the mfaPending/pendingToken shape instead of a
//     token pair (see the MFA doc paragraph below); otherwise issue a session identical in shape
//     to password login.
//  5. Not found -> a blank email claim can never be safely resolved -> not-provisioned. A
//     non-blank email matching an EXISTING local user -> deny-link (safe default, no
//     auto-takeover). No local match -> JIT-provision atomically if the provider opts in,
//     else not-provisioned.
//
// SSO login ENFORCES any LOCAL MFA enrollment on the resolved account (reviewer FIX 2, MED —
// api-conventions.md §3c.8 item 4 revised): a found-identity login for a user with
// user_mfa.enabled=1 does NOT go straight to issueSessionForSubject — it returns the SAME
// {mfaPending, pendingToken} shape Login returns, and the caller must complete
// POST /auth/mfa/verify exactly like a password login would. The original design let the IdP
// round-trip stand in for BOTH factors (IdP auth as factor 1, and skip local factor 2 entirely),
// which downgrades security for any account that separately opted into local TOTP — reachable in
// practice via JIT-provision-then-self-enroll-MFA, then log in again via SSO. The default here is
// safe: ALWAYS enforce local MFA when enrolled. This check runs ONLY on the found-identity
// branch: a JIT-provisioned brand new user cannot possibly have a pre-existing user_mfa row, so
// that branch is unaffected.
//
// SSO-MFA-POLICY (per-provider opt-out of the gate above): when the RESOLVED provider carries
// trust_idp_mfa=1 (migration 000040 — settable ONLY via the admin PATCH, default 0), the
// found-identity branch SKIPS mfaLoginGate entirely and issues the session directly — the admin
// has explicitly declared this IdP runs its own strong MFA, so demanding the local TOTP again
// would be a redundant second prompt for every login through it. SECURITY ASSUMPTION
// (deliberate, documented — api-conventions.md §3c.8): this is BLANKET trust in the provider;
// the callback does NOT inspect the id_token's amr/acr claims to verify an MFA challenge
// actually happened at the IdP for THIS login (many IdPs omit/blur those claims; an honest MVP
// pins the trust decision on the admin, not on unverifiable claim heuristics). Consequences the
// admin accepts by flipping the flag: (a) local MFA is bypassed for EVERY found-identity login
// through this provider, including while the local MFA key is present; (b) the MFA-ENFORCE
// fail-closed 503 does not apply on this path (there is no local gate to fail closed). With the
// flag OFF — the default, and every pre-existing row — this paragraph changes NOTHING: the gate
// runs exactly as before, including the fail-closed key-absent arm.
func (s *SSOService) HandleCallback(ctx context.Context, code, state, browserNonce string) (*LoginOutput, error) {
	// SSO-FE-REDIRECT split HandleCallback into resolveSSOUser (steps 1-5: verify the round-trip
	// and resolve WHO logged in) + completeSSOLogin (the local-MFA gate + session issuance), so
	// StartHandoff can reuse the first half verbatim and ExchangeHandoff the second — the two
	// halves can never drift from the direct JSON path because they ARE the direct JSON path.
	u, provider, err := s.resolveSSOUser(ctx, code, state, browserNonce)
	if err != nil {
		return nil, err
	}
	return s.completeSSOLogin(ctx, u, provider)
}

// resolveSSOUser runs the callback verification pipeline (doc comment above: state → cookie bind
// → provider re-resolve → code exchange → nonce/subject checks → identity / deny-link / JIT) and
// returns the resolved local user + the provider row — WITHOUT minting anything. Every guard and
// its ordering is exactly the pre-split HandleCallback body.
func (s *SSOService) resolveSSOUser(ctx context.Context, code, state, browserNonce string) (*auth.User, *auth.SSOProvider, error) {
	if s.sealer == nil {
		return nil, nil, ErrSSOUnavailable
	}
	if strings.TrimSpace(code) == "" || strings.TrimSpace(state) == "" {
		return nil, nil, ErrSSOStateInvalid
	}

	st, err := s.openState(state)
	if err != nil {
		return nil, nil, ErrSSOStateInvalid
	}

	// SSO-STATE-COOKIE: the browser-session bind (see the doc comment, step 1). Runs AFTER
	// openState (a tampered/expired state still folds into ErrSSOStateInvalid, revealing nothing
	// extra to a prober) and BEFORE the provider load / code exchange (a bind failure must never
	// redeem the authorization code). subtle.ConstantTimeCompare, not ==: the nonce acts as a
	// per-flow session secret here, and an empty browserNonce (no cookie sent) compares unequal
	// to any non-empty nonce by the same call (it returns 0 on length mismatch).
	if subtle.ConstantTimeCompare([]byte(browserNonce), []byte(st.Nonce)) != 1 {
		return nil, nil, ErrSSOStateMismatch
	}

	provider, err := s.providers.GetEnabledByID(ctx, st.TenantID, st.ProviderID)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return nil, nil, auth.ErrSSOProviderNotFound
		}
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: load provider: %w", err)
	}

	secretRaw, err := s.sealer.Open(provider.ClientSecretEnc)
	if err != nil {
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: open client secret: %w", err)
	}

	claims, err := s.oidc.Exchange(ctx, provider, string(secretRaw), s.callbackRedirectURI(), code)
	if err != nil {
		if errors.Is(err, errOIDCExchangeFailed) {
			return nil, nil, ErrSSOExchangeFailed
		}
		if errors.Is(err, errOIDCIDTokenInvalid) {
			return nil, nil, ErrSSOIDTokenInvalid
		}
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: exchange: %w", err)
	}

	if claims.Nonce == "" || claims.Nonce != st.Nonce {
		return nil, nil, ErrSSOIDTokenInvalid
	}
	subject := strings.TrimSpace(claims.Subject)
	if subject == "" {
		return nil, nil, ErrSSOIDTokenInvalid
	}

	identity, err := s.identities.FindBySubject(ctx, st.TenantID, provider.ID, subject)
	switch {
	case err == nil:
		u, uerr := s.authSvc.users.GetUserByID(ctx, st.TenantID, identity.UserID)
		if uerr != nil || u.Status != statusActive {
			// Orphaned link (user soft-deleted/disabled after linking, or a lookup fault) —
			// fail clean rather than surface a 500 for what is really an authorization refusal.
			return nil, nil, ErrSSOUserNotProvisioned
		}
		return u, provider, nil
	case errors.Is(err, auth.ErrSSOIdentityNotFound):
		// Fall through to deny-link / JIT resolution below.
	default:
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: find identity: %w", err)
	}

	email := strings.TrimSpace(claims.Email)
	if email == "" {
		// No identity AND no email claim to resolve against — never safe to JIT-create a
		// blank-email account, and there is nothing to deny-link check against either.
		return nil, nil, ErrSSOUserNotProvisioned
	}

	_, err = s.authSvc.users.GetUserByEmail(ctx, st.TenantID, email)
	switch {
	case err == nil:
		// An existing local user owns this email but has never linked this (provider, sub) —
		// safe-default deny-link (see ErrSSOAccountLinkDenied's doc).
		return nil, nil, ErrSSOAccountLinkDenied
	case errors.Is(err, auth.ErrUserNotFound):
		// No local account at all under this email — proceed to the JIT decision.
	default:
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: lookup local user by email: %w", err)
	}

	if !provider.JITProvisioning {
		return nil, nil, ErrSSOUserNotProvisioned
	}

	// FIX 4 (LOW): only an IdP-VERIFIED email may be trusted to CREATE a brand-new local account.
	// claims.EmailVerified was decoded (sso_oidc_client.go) but never checked before this fix — an
	// IdP is free to assert any string in `email` regardless of whether that mailbox is actually
	// owned by the authenticating subject. Deliberately gated HERE (the JIT-create branch only),
	// not before the deny-link lookup above: an unverified email used to DENY a login only ever
	// makes the outcome MORE restrictive, so it is safe to let that comparison run unconditionally.
	if !claims.EmailVerified {
		return nil, nil, ErrSSOUserNotProvisioned
	}

	randomHash, err := randomUnusablePasswordHash()
	if err != nil {
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: %w", err)
	}
	newUser := &auth.User{
		ID: s.genID(), TenantID: st.TenantID, Email: email, PasswordHash: randomHash,
		Status: statusActive, IsAdmin: false,
	}
	newIdentity := &auth.SSOIdentity{
		ID: s.genID(), TenantID: st.TenantID, ProviderID: provider.ID, UserID: newUser.ID,
		Subject: subject, Email: email,
	}
	if err := s.identities.CreateUserWithIdentity(ctx, newUser, newIdentity); err != nil {
		if errors.Is(err, auth.ErrUserAlreadyExists) {
			// Race: a local signup or a concurrent link claimed this email between our check
			// above and this insert — deny-link is the correct outcome either way.
			return nil, nil, ErrSSOAccountLinkDenied
		}
		return nil, nil, fmt.Errorf("services.SSOService.resolveSSOUser: jit provision: %w", err)
	}

	return newUser, provider, nil
}

// completeSSOLogin is the second half of an SSO login: the local-MFA decision + session issuance
// for an already-RESOLVED user. Shared verbatim by the direct JSON path (HandleCallback) and the
// redirect path (ExchangeHandoff) so the MFA gate can never be bypassed by choosing a mode.
//
// Pre-split note: the JIT branch used to call issueSessionForSubject directly, skipping the gate.
// Routing JIT users through here is output-equivalent — a brand-new user cannot possibly have a
// user_mfa row, so mfaLoginGate falls through to the same direct session (the gate's doc says
// exactly this); the only delta is one enrollment read and, for a trust_idp_mfa provider, the
// info log below also covering JIT logins.
func (s *SSOService) completeSSOLogin(ctx context.Context, u *auth.User, provider *auth.SSOProvider) (*LoginOutput, error) {
	// SSO-MFA-POLICY: the per-provider trust flag short-circuits the local gate BEFORE it
	// runs (see HandleCallback's doc comment for the blanket-trust security assumption).
	// `provider` here is the row re-resolved from the signed state (or re-loaded enabled-only
	// at exchange) — never anything client-supplied. Logged loudly on every use so an enrolled
	// account logging in without its local second factor is always attributable to this
	// deliberate admin decision in the audit trail.
	if provider.TrustIdpMfa {
		s.authSvc.logger.Info("services.SSOService.completeSSOLogin: local MFA gate skipped — provider has trust_idp_mfa=1 (SSO-MFA-POLICY blanket trust, no per-login AMR check)",
			zap.String("tenant_id", u.TenantID),
			zap.String("user_id", u.ID),
			zap.String("provider_id", provider.ID),
		)
		return s.authSvc.issueSessionForSubject(ctx, u)
	}
	// FIX 2 + MFA-ENFORCE: enforce local MFA on the found-identity path via the SAME
	// mfaLoginGate AuthService.Login uses (auth_mfa_enforce.go) — parity by construction,
	// not by a hand-mirrored copy. Enrolled + full support → {mfaPending, pendingToken};
	// enrolled + key lost → fail-closed ErrMFAEnrolledButUnavailable (503 MFA_UNAVAILABLE
	// at the handler; MFA_FAIL_OPEN honored inside the gate) — otherwise SSO login would
	// be the exact bypass MFA-ENFORCE closes on the password path. Not-enrolled accounts
	// fall through to a direct session exactly as before.
	pendingToken, mfaPendingRequired, gateErr := s.authSvc.mfaLoginGate(ctx, u.TenantID, u.ID)
	if gateErr != nil {
		if errors.Is(gateErr, ErrMFAEnrolledButUnavailable) {
			return nil, ErrMFAEnrolledButUnavailable
		}
		return nil, fmt.Errorf("services.SSOService.completeSSOLogin: %w", gateErr)
	}
	if mfaPendingRequired {
		return &LoginOutput{MFAPending: true, PendingToken: pendingToken}, nil
	}
	return s.authSvc.issueSessionForSubject(ctx, u)
}

// ssoHandoffTTL bounds how long the one-time handoff code stays redeemable. It only needs to
// survive one browser redirect + one XHR, so it is deliberately much shorter than the state TTL.
const ssoHandoffTTL = 120 * time.Second

// StartHandoff (SSO-FE-REDIRECT) runs the FULL callback verification pipeline (resolveSSOUser —
// identical guards, identical ordering) and, instead of minting a session, stores a single-use
// handoff row and returns the raw opaque code for the 302 redirect to the frontend. Only the
// SHA-256 hash is persisted; the session tokens are minted later at ExchangeHandoff, so they
// never appear in a URL, browser history, Referer header, or access log.
func (s *SSOService) StartHandoff(ctx context.Context, code, state, browserNonce string) (string, error) {
	if s.handoffs == nil {
		return "", ErrSSOUnavailable
	}
	u, provider, err := s.resolveSSOUser(ctx, code, state, browserNonce)
	if err != nil {
		return "", err
	}
	raw, err := newResetToken()
	if err != nil {
		return "", fmt.Errorf("services.SSOService.StartHandoff: generate code: %w", err)
	}
	t := &auth.SSOHandoffToken{
		ID:         s.genID(),
		TenantID:   u.TenantID,
		UserID:     u.ID,
		ProviderID: provider.ID,
		TokenHash:  auth.HashRefreshToken(raw),
		ExpiresAt:  s.clock().Add(ssoHandoffTTL),
		CreatedAt:  s.clock(),
	}
	if err := s.handoffs.Store(ctx, t); err != nil {
		return "", fmt.Errorf("services.SSOService.StartHandoff: store handoff: %w", err)
	}
	return raw, nil
}

// ExchangeHandoff (SSO-FE-REDIRECT) redeems a one-time handoff code from the frontend /exchange
// XHR and completes the login exactly like the direct path: re-load the user (must still be
// active) and the provider (must still be ENABLED — an admin disabling it inside the TTL window
// must not let the login complete, mirroring the /start→/callback re-resolve discipline), then
// run completeSSOLogin (trust_idp_mfa honor + local-MFA gate + session issuance).
//
// Failure semantics: every "this code buys nothing" outcome — unknown, already used, expired,
// user disabled, provider disabled/deleted mid-window — folds into the ONE opaque
// auth.ErrSSOHandoffInvalid (anti-probing; mirrors ErrMFAPendingInvalid). Only genuine server
// faults and the MFA-ENFORCE fail-closed arm surface distinctly.
func (s *SSOService) ExchangeHandoff(ctx context.Context, rawCode string) (*LoginOutput, error) {
	// Honest 503 when SSO is fully off (no sealer): no valid handoff code can exist without
	// StartHandoff, which needs the sealer — so gating here just returns the truthful
	// SSO_NOT_CONFIGURED instead of a misleading "code invalid/expired" (reviewer LOW-2). The
	// sealer check also composes with the WithHandoffStore-wired-unconditionally shape.
	if s.sealer == nil || s.handoffs == nil {
		return nil, ErrSSOUnavailable
	}
	rawCode = strings.TrimSpace(rawCode)
	if rawCode == "" {
		return nil, auth.ErrSSOHandoffInvalid
	}

	row, err := s.handoffs.FindByHash(ctx, auth.HashRefreshToken(rawCode))
	if err != nil {
		if errors.Is(err, auth.ErrSSOHandoffInvalid) {
			return nil, auth.ErrSSOHandoffInvalid
		}
		return nil, fmt.Errorf("services.SSOService.ExchangeHandoff: find handoff: %w", err)
	}
	// Atomic single-use claim — tenant pinned FROM THE ROW (never client input). A second
	// concurrent submit of the same code loses the conditional UPDATE and lands here.
	consumed, err := s.handoffs.ConsumeByHash(ctx, row.TenantID, row.TokenHash)
	if err != nil {
		return nil, fmt.Errorf("services.SSOService.ExchangeHandoff: consume handoff: %w", err)
	}
	if !consumed {
		return nil, auth.ErrSSOHandoffInvalid
	}

	u, err := s.authSvc.users.GetUserByID(ctx, row.TenantID, row.UserID)
	if err != nil || u.Status != statusActive {
		// Disabled/deleted inside the TTL window — the handoff buys nothing (opaque refusal).
		return nil, auth.ErrSSOHandoffInvalid
	}
	provider, err := s.providers.GetEnabledByID(ctx, row.TenantID, row.ProviderID)
	if err != nil {
		if errors.Is(err, auth.ErrSSOProviderNotFound) {
			return nil, auth.ErrSSOHandoffInvalid
		}
		return nil, fmt.Errorf("services.SSOService.ExchangeHandoff: load provider: %w", err)
	}

	return s.completeSSOLogin(ctx, u, provider)
}

// issueSessionForSubject mints an access+refresh token pair for an already-authenticated user
// and returns them in the SAME LoginOutput shape as a password login (SSOService's HandleCallback
// is the only caller — same package, unexported deliberately so it does not widen AuthService's
// public surface for a single call site). No password/MFA check runs here: identity has already
// been verified by the IdP round-trip that led here.
func (s *AuthService) issueSessionForSubject(ctx context.Context, u *auth.User) (*LoginOutput, error) {
	sub := auth.Subject{UserID: u.ID, TenantID: u.TenantID, IsAdmin: u.IsAdmin}
	if u.RoleID != nil {
		sub.RoleID = *u.RoleID
	}
	access, refresh, err := s.issueTokenPair(ctx, sub, nil)
	if err != nil {
		return nil, fmt.Errorf("services.AuthService.issueSessionForSubject: %w", err)
	}
	lu := &LoginUser{ID: u.ID, TenantID: u.TenantID, Email: u.Email, FullName: u.FullName, IsAdmin: u.IsAdmin}
	if u.RoleID != nil {
		lu.RoleID = *u.RoleID
	}
	return &LoginOutput{AccessToken: access, RefreshToken: refresh, User: lu}, nil
}

// randomUnusablePasswordHash returns a bcrypt hash of a random, never-recorded 256-bit secret —
// used as the PasswordHash for a JIT-provisioned SSO user so the account has no usable local
// password (login via the ordinary email/password flow always fails bcrypt comparison) until an
// admin explicitly sets one.
func randomUnusablePasswordHash() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("randomUnusablePasswordHash: read random: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword(raw, bcryptCost)
	if err != nil {
		return "", fmt.Errorf("randomUnusablePasswordHash: hash: %w", err)
	}
	return string(hash), nil
}
