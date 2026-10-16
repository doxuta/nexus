package services

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"nexus/platform/internal/auth"
)

// realOIDCClient is the production oidcClient: real discovery (/.well-known/openid-configuration),
// real JWKS-backed signature verification, and the real authorization-code exchange via
// golang.org/x/oauth2 (../specs/004-sso-oidc/spec.md §"#8").
//
// Discovery documents are cached per-issuer in providers (a sync.Map, safe for concurrent use):
// *oidc.Provider itself is safe to reuse across requests (its RemoteKeySet does its own JWKS
// refresh-on-unknown-kid internally), so caching it here avoids re-fetching the discovery
// document on every single /start and /callback call for the same issuer.
type realOIDCClient struct {
	providers sync.Map // issuer string -> *oidc.Provider
}

// newRealOIDCClient constructs a realOIDCClient with an empty discovery cache.
func newRealOIDCClient() *realOIDCClient {
	return &realOIDCClient{}
}

// errOIDCDiscoveryFailed wraps a failed OIDC discovery fetch (reviewer FIX 6, LOW): a typo'd but
// otherwise well-formed https:// issuer, or one that is simply unreachable/misconfigured, used to
// surface as a generic 500 SSO_OPERATION_FAILED at /start — indistinguishable from a genuine
// internal fault. StartLogin classifies this sentinel into the public ErrSSOProviderDiscoveryFailed
// (502 SSO_PROVIDER_DISCOVERY_FAILED) instead. Deliberately its OWN sentinel, distinct from
// errOIDCExchangeFailed: HandleCallback's Exchange path also calls provider() internally, but
// wraps ITS failure with `%v` (not `%w`) into errOIDCExchangeFailed (see Exchange below) so that
// path's classification is unchanged by this addition — a discovery failure reached via /callback
// still surfaces as SSO_EXCHANGE_FAILED, matching this suite's existing pinned behavior.
var errOIDCDiscoveryFailed = errors.New("oidc: discovery failed")

// provider resolves (and caches) the *oidc.Provider for issuer via OIDC discovery. The discovery
// fetch AND every subsequent JWKS refresh performed through the returned *oidc.Provider (its
// RemoteKeySet captures the ctx-injected client at construction time, go-oidc's oidc.go/jwks.go)
// go through ssrfSafeHTTPClient (reviewer FIX 3, MED) — an admin-entered `issuer` is untrusted
// input dialed server-side, so it must never be able to reach loopback/private/link-local
// addresses (SSRF into this deployment's own network / cloud metadata endpoint) unless the
// operator has explicitly opted in via SSOAllowPrivateNetworksEnv.
func (c *realOIDCClient) provider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	if v, ok := c.providers.Load(issuer); ok {
		return v.(*oidc.Provider), nil
	}
	ctx = oidc.ClientContext(ctx, ssrfSafeHTTPClient())
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: oidc discovery for issuer %q: %v", errOIDCDiscoveryFailed, issuer, err)
	}
	actual, _ := c.providers.LoadOrStore(issuer, p)
	return actual.(*oidc.Provider), nil
}

// oauth2Config builds the per-request oauth2.Config from the provider's discovered endpoint and
// the tenant's registered client credentials.
func oauth2Config(p *oidc.Provider, prov *auth.SSOProvider, clientSecret, redirectURI string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     prov.ClientID,
		ClientSecret: clientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
}

// AuthCodeURL builds the IdP authorization-endpoint redirect URL, embedding both `state` (the
// caller's already-sealed opaque state string) and `nonce` (oidc.Nonce, per the OIDC spec — the
// IdP echoes it back inside the id_token's `nonce` claim).
func (c *realOIDCClient) AuthCodeURL(ctx context.Context, prov *auth.SSOProvider, clientSecret, redirectURI, state, nonce string) (string, error) {
	p, err := c.provider(ctx, prov.Issuer)
	if err != nil {
		return "", err
	}
	cfg := oauth2Config(p, prov, clientSecret, redirectURI)
	return cfg.AuthCodeURL(state, oidc.Nonce(nonce)), nil
}

// idTokenClaims is the raw JSON shape decoded from a verified id_token — a superset of
// oidcClaims (services.oidcClaims), kept as an unexported decode target so a future claim this
// package does not yet consume can be added without touching the public oidcClaims shape.
type idTokenClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Nonce         string `json:"nonce"`
}

// Exchange trades code for a token, then verifies the returned id_token (signature via the
// provider's JWKS, issuer, audience == prov.ClientID, and expiry — all inside
// oidc.IDTokenVerifier.Verify). A failure BEFORE a token is obtained (bad/expired/replayed code,
// IdP token-endpoint error) is wrapped in errOIDCExchangeFailed; a failure verifying the id_token
// itself (or a response carrying no id_token at all) is wrapped in errOIDCIDTokenInvalid — the
// caller (SSOService.HandleCallback) classifies these into two distinct HTTP statuses.
// Nonce-vs-state binding is NOT checked here (see oidcClaims.Nonce's doc) — this method only
// returns the claim as asserted.
func (c *realOIDCClient) Exchange(ctx context.Context, prov *auth.SSOProvider, clientSecret, redirectURI, code string) (*oidcClaims, error) {
	p, err := c.provider(ctx, prov.Issuer)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errOIDCExchangeFailed, err)
	}
	cfg := oauth2Config(p, prov, clientSecret, redirectURI)

	// FIX 3: the authorization-code-for-token exchange is a SEPARATE outbound HTTP call
	// (golang.org/x/oauth2, not go-oidc) that reads its client from the ctx value keyed by
	// oauth2.HTTPClient fresh on every call — provider()'s oidc.ClientContext injection only
	// covers discovery/JWKS, so the same ssrfSafeHTTPClient must be injected here too.
	exchangeCtx := context.WithValue(ctx, oauth2.HTTPClient, ssrfSafeHTTPClient())
	tok, err := cfg.Exchange(exchangeCtx, code)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errOIDCExchangeFailed, err)
	}

	rawIDToken, ok := tok.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("%w: token response carried no id_token", errOIDCIDTokenInvalid)
	}

	verifier := p.Verifier(&oidc.Config{ClientID: prov.ClientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errOIDCIDTokenInvalid, err)
	}

	var claims idTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: decode claims: %v", errOIDCIDTokenInvalid, err)
	}

	return &oidcClaims{
		Subject:       claims.Sub,
		Email:         claims.Email,
		EmailVerified: claims.EmailVerified,
		Nonce:         claims.Nonce,
	}, nil
}
