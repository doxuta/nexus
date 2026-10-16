package services

// specs/050 S2b (tasks T046) — CredentialService.ResolveAuth is the ONE place a vault secret is
// opened for sending (the engine's workflow.CredentialResolver, and the seam custom outbound code
// must go through — spec Nợ 11). A key that cannot be used answers a coded error and no auth at
// all; Describe serves the dry-run without opening anything.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/workflow"
)

func wantResolveCode(t *testing.T, auth workflow.WebhookAuth, err error, code string) {
	t.Helper()
	var ce *workflow.CodedError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v; want CodedError %s", err, code)
	}
	if auth.BaseURL != "" || len(auth.Headers) != 0 || auth.SigningSecret != "" {
		t.Fatalf("a failed resolve returned auth material: %+v", auth)
	}
}

func TestCredentialService_ResolveAuth(t *testing.T) {
	svc, store, keys, _ := newCredSvc(t)
	ctx := context.Background()
	mk := func(in CredentialInput) string {
		t.Helper()
		v, _, err := svc.Create(ctx, credTenantA, in)
		if err != nil {
			t.Fatalf("create %s: %v", in.Name, err)
		}
		return v.ID
	}
	bearer := mk(CredentialInput{Name: "pay", BaseURL: "https://pay.example.vn/api", AuthType: "header",
		Secret: strPtr("tok-123"), ExtraHeaders: map[string]string{"X-Doi-Tac": "p-9"}})
	apiKey := mk(CredentialInput{Name: "apikey", BaseURL: "https://api.example.vn", AuthType: "header",
		HeaderName: strPtr("X-Api-Key"), Secret: strPtr("k-1")})
	basic := mk(CredentialInput{Name: "shop", BaseURL: "https://shop.example.vn", AuthType: "basic",
		Username: strPtr("shop-user"), Secret: strPtr("p@ss")})
	signed := mk(CredentialInput{Name: "signed", BaseURL: "https://hook.example.vn", AuthType: "hmac",
		Secret: strPtr(strongKey), ExtraHeaders: map[string]string{"Authorization": "Bearer partner"}})
	slot := mk(CredentialInput{Name: "slot", BaseURL: "https://slot.example.vn", AuthType: "hmac"})

	t.Run("each type builds its wire auth", func(t *testing.T) {
		a, err := svc.ResolveAuth(ctx, credTenantA, bearer)
		if err != nil || a.BaseURL != "https://pay.example.vn/api" || a.DisplayBase != a.BaseURL ||
			a.Headers["Authorization"] != "Bearer tok-123" || a.Headers["X-Doi-Tac"] != "p-9" || a.SigningSecret != "" {
			t.Fatalf("header: %+v err=%v", a, err)
		}
		a, err = svc.ResolveAuth(ctx, credTenantA, apiKey)
		if err != nil || a.Headers["X-Api-Key"] != "k-1" || len(a.Headers) != 1 {
			t.Fatalf("custom header with empty prefix: %+v err=%v", a, err)
		}
		a, err = svc.ResolveAuth(ctx, credTenantA, basic)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("shop-user:p@ss"))
		if err != nil || a.Headers["Authorization"] != want {
			t.Fatalf("basic: %+v err=%v; want %q", a, err, want)
		}
		a, err = svc.ResolveAuth(ctx, credTenantA, signed)
		if err != nil || a.SigningSecret != strongKey || a.Headers["Authorization"] != "Bearer partner" || len(a.Headers) != 1 {
			t.Fatalf("hmac: %+v err=%v; want the signing key + its extra header only", a, err)
		}
	})

	t.Run("a slot without its secret is not ready", func(t *testing.T) {
		a, err := svc.ResolveAuth(ctx, credTenantA, slot)
		wantResolveCode(t, a, err, workflow.CodeCredentialNotReady)
	})
	t.Run("another tenant's key is not found", func(t *testing.T) {
		a, err := svc.ResolveAuth(ctx, credTenantB, bearer)
		wantResolveCode(t, a, err, workflow.CodeCredentialNotFound)
	})
	t.Run("a deleted key is not found", func(t *testing.T) {
		gone := mk(CredentialInput{Name: "gone", BaseURL: "https://gone.example.vn", AuthType: "header", Secret: strPtr("t")})
		if err := svc.Delete(ctx, credTenantA, gone, "u-1"); err != nil {
			t.Fatal(err)
		}
		a, err := svc.ResolveAuth(ctx, credTenantA, gone)
		wantResolveCode(t, a, err, workflow.CodeCredentialNotFound)
	})
	t.Run("sealed under a master key this server lacks", func(t *testing.T) {
		other := NewCredentialService(store, testKeyring(t), false) // same rows, another master key
		a, err := other.ResolveAuth(ctx, credTenantA, bearer)
		wantResolveCode(t, a, err, workflow.CodeCredentialKeyUnavailable)
	})
	t.Run("an envelope copied from another row does not open", func(t *testing.T) {
		// Same master key, so the key id is held — only the AAD (tenant|id|slot) tells them apart.
		stolen := *store.rows[bearer].SecretEnc
		store.rows[apiKey].SecretEnc = &stolen
		a, err := svc.ResolveAuth(ctx, credTenantA, apiKey)
		wantResolveCode(t, a, err, workflow.CodeCredentialKeyUnavailable)
		if strings.Contains(err.Error(), "v1") || strings.Contains(err.Error(), stolen[:10]) {
			t.Fatalf("error %q names a master-key id or quotes the envelope", err)
		}
		if keys.ActiveID() == "" {
			t.Fatal("keyring lost its id")
		}
	})
	t.Run("vault not configured", func(t *testing.T) {
		inert := NewCredentialService(store, nil, false)
		a, err := inert.ResolveAuth(ctx, credTenantA, bearer)
		wantResolveCode(t, a, err, workflow.CodeCredentialsNotConfigured)
	})
	t.Run("store failure stays transient", func(t *testing.T) {
		broken := NewCredentialService(failingCredStore{newFakeCredentialStore()}, keys, false)
		_, err := broken.ResolveAuth(ctx, credTenantA, bearer)
		var ce *workflow.CodedError
		if err == nil || errors.As(err, &ce) {
			t.Fatalf("err = %v; want a plain (retryable) error", err)
		}
	})
}

// TestCredentialService_Describe: what the dry-run and the save-time check read — the view, no
// secret, and CREDENTIAL_NOT_FOUND across tenants. Works on an inert vault too (nothing is opened).
func TestCredentialService_Describe(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "pay", BaseURL: "https://pay.example.vn/api",
		AuthType: "hmac", Secret: strPtr(strongKey)})
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewCredentialService(store, nil, false).Describe(ctx, credTenantA, v.ID)
	if err != nil || d.Name != "pay" || d.AuthType != credential.AuthHMAC || d.BaseURL != "https://pay.example.vn/api" {
		t.Fatalf("describe = %+v err=%v", d, err)
	}
	_, err = svc.Describe(ctx, credTenantB, v.ID)
	wantCredCode(t, err, "CREDENTIAL_NOT_FOUND", "")
}

// failingCredStore fails every read (a DB outage).
type failingCredStore struct{ *fakeCredentialStore }

func (failingCredStore) Get(context.Context, string, string) (*credential.Credential, error) {
	return nil, errors.New("db down")
}

// TestCredentialService_ResolveAuth_URLToken (specs/050 S5): the secret is spliced into the base's
// {secret} (path-escaped) for the SEND form only; the display form keeps the placeholder; no header
// is added; the Telegram token shape (colon) stays readable and a "/" in a secret cannot add a segment.
func TestCredentialService_ResolveAuth_URLToken(t *testing.T) {
	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	for i, tc := range []struct{ name, base, secret, wantBase string }{
		{"telegram", "https://api.telegram.org/bot{secret}", "123456:ABC-def", "https://api.telegram.org/bot123456:ABC-def"},
		{"slash and percent are escaped", "https://hooks.example.vn/services/{secret}", "T0/B0%x y", "https://hooks.example.vn/services/T0%2FB0%25x%20y"},
		{"placeholder mid-path", "https://api.example.vn/v1/{secret}/hooks", "k-1", "https://api.example.vn/v1/k-1/hooks"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: fmt.Sprintf("u%d", i), BaseURL: tc.base, AuthType: "url_token", Secret: strPtr(tc.secret)})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			a, err := svc.ResolveAuth(ctx, credTenantA, v.ID)
			if err != nil || a.BaseURL != tc.wantBase || a.DisplayBase != tc.base || len(a.Headers) != 0 || a.SigningSecret != "" {
				t.Fatalf("auth = %+v err=%v; want base %q, display %q, no header, no signing key", a, err, tc.wantBase, tc.base)
			}
			if strings.Contains(a.DisplayBase, tc.secret) {
				t.Fatalf("display base carries the secret: %q", a.DisplayBase)
			}
		})
	}
	t.Run("a slot without its token is not ready", func(t *testing.T) {
		v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "slot", BaseURL: "https://api.telegram.org/bot{secret}", AuthType: "url_token"})
		if err != nil {
			t.Fatalf("create slot: %v", err)
		}
		a, err := svc.ResolveAuth(ctx, credTenantA, v.ID)
		wantResolveCode(t, a, err, workflow.CodeCredentialNotReady)
	})
	t.Run("a base that lost its placeholder never goes out unauthenticated", func(t *testing.T) {
		svc, store, _, _ := newCredSvc(t)
		v, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "edited", BaseURL: "https://api.telegram.org/bot{secret}", AuthType: "url_token", Secret: strPtr("123456:ABC-def")})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		store.rows[v.ID].BaseURL = "https://api.telegram.org/bot" // a row edited by hand in the DB
		a, err := svc.ResolveAuth(ctx, credTenantA, v.ID)
		wantResolveCode(t, a, err, workflow.CodeCredentialNotReady)
	})
}
