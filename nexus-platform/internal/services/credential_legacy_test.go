package services

import (
	"context"
	"testing"

	"nexus/platform/internal/credential"
	"nexus/platform/internal/workflow"
)

// specs/050 S3b (tasks T069): the two vault entry points only the Doctor's C10 step calls — never
// mounted on HTTP. ImportLegacy is Create minus CREDENTIAL_SECRET_WEAK (a signing key saved on a rule
// before the vault existed must keep signing V1 byte for byte, however short); FindLegacyImport is
// the reuse-before-create lookup that makes a re-run (or a second operator) mint no duplicate.

func TestCredentialService_ImportLegacyAcceptsShortSigningKey(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	short := "legacy-12chr" // 12 characters — the API refuses this for a typed-in hmac key

	_, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "api", BaseURL: "https://pay.example.vn",
		AuthType: "hmac", Secret: strPtr(short)})
	wantCredCode(t, err, "CREDENTIAL_SECRET_WEAK", "secret")

	v, err := svc.ImportLegacy(ctx, credTenantA, CredentialInput{Name: "webhook-pay-example-vn",
		Label: "Webhook pay.example.vn" + CredentialLegacyLabelSuffix, BaseURL: "https://pay.example.vn",
		AuthType: "hmac", Secret: strPtr(short), ExtraHeaders: map[string]string{"X-Api-Key": "k-77"}})
	if err != nil {
		t.Fatalf("ImportLegacy short hmac key: %v", err)
	}
	if v.Status != credential.StatusReady || len(store.rows) != 1 {
		t.Fatalf("ImportLegacy = %+v, %d row(s); want one ready key", v, len(store.rows))
	}
	got, err := svc.ResolveAuth(ctx, credTenantA, v.ID)
	if err != nil || got.SigningSecret != short || got.Headers["X-Api-Key"] != "k-77" {
		t.Fatalf("the imported key opens to %+v (err %v); want the legacy key and header back", got, err)
	}
}

func TestCredentialService_ImportLegacyKeepsEveryOtherCheck(t *testing.T) {
	svc, store, _, _ := newCredSvc(t)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		in    CredentialInput
		code  string
		field string
	}{
		"userinfo in the base": {CredentialInput{Name: "a", BaseURL: "https://u:p@pay.example.vn", AuthType: "hmac",
			Secret: strPtr("k")}, "CREDENTIAL_BASE_URL_INVALID", "baseUrl"},
		"reserved extra header": {CredentialInput{Name: "b", BaseURL: "https://pay.example.vn", AuthType: "hmac",
			Secret: strPtr("k"), ExtraHeaders: map[string]string{"Host": "x"}}, "CREDENTIAL_HEADER_INVALID", "extraHeaders"},
		"bad name": {CredentialInput{Name: "Webhook", BaseURL: "https://pay.example.vn", AuthType: "hmac",
			Secret: strPtr("k")}, "CREDENTIAL_NAME_INVALID", "name"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.ImportLegacy(ctx, credTenantA, tc.in)
			wantCredCode(t, err, tc.code, tc.field)
		})
	}
	if len(store.rows) != 0 {
		t.Fatalf("%d row(s) stored; want none", len(store.rows))
	}

	inert := NewCredentialService(newFakeCredentialStore(), nil, false)
	_, err := inert.ImportLegacy(ctx, credTenantA, CredentialInput{Name: "a", BaseURL: "https://pay.example.vn",
		AuthType: "hmac", Secret: strPtr("k")})
	wantCredCode(t, err, "CREDENTIALS_NOT_CONFIGURED", "")
}

func TestCredentialService_FindLegacyImport(t *testing.T) {
	svc, _, _, _ := newCredSvc(t)
	ctx := context.Background()
	const base = "https://pay.example.vn"
	legacy := func(name, secret string, extra map[string]string) string {
		t.Helper()
		v, err := svc.ImportLegacy(ctx, credTenantA, CredentialInput{Name: name, Label: "Webhook pay.example.vn" + CredentialLegacyLabelSuffix,
			BaseURL: base, AuthType: "hmac", Secret: strPtr(secret), ExtraHeaders: extra})
		if err != nil {
			t.Fatalf("ImportLegacy %s: %v", name, err)
		}
		return v.ID
	}
	id := legacy("webhook-pay-example-vn", "sig-1", map[string]string{"Authorization": "Bearer t1"})
	other := legacy("webhook-pay-example-vn-2", "sig-2", nil)
	// The same material stored by a customer admin is not the Doctor's to repoint rules at.
	if _, _, err := svc.Create(ctx, credTenantA, CredentialInput{Name: "admin-made", BaseURL: base, AuthType: "hmac",
		Secret: strPtr(strongKey)}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for name, tc := range map[string]struct {
		tenant, base string
		typ          credential.AuthType
		want         workflow.WebhookAuth
		wantID       string
	}{
		"same key, same headers":   {credTenantA, base, credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: "sig-1", Headers: map[string]string{"Authorization": "Bearer t1"}}, id},
		"no headers matches empty": {credTenantA, base, credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: "sig-2"}, other},
		"header value differs":     {credTenantA, base, credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: "sig-1", Headers: map[string]string{"Authorization": "Bearer t2"}}, ""},
		"secret differs":           {credTenantA, base, credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: "sig-9", Headers: map[string]string{"Authorization": "Bearer t1"}}, ""},
		"other base":               {credTenantA, "https://pay.example.vn:8443", credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: "sig-2"}, ""},
		"other type":               {credTenantA, base, credential.AuthHeader, workflow.WebhookAuth{SigningSecret: "sig-2"}, ""},
		"admin-made key":           {credTenantA, base, credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: strongKey}, ""},
		"other tenant":             {credTenantB, base, credential.AuthHMAC, workflow.WebhookAuth{SigningSecret: "sig-2"}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := svc.FindLegacyImport(ctx, tc.tenant, tc.base, tc.typ, tc.want)
			if err != nil || got != tc.wantID {
				t.Fatalf("FindLegacyImport = %q (err %v); want %q", got, err, tc.wantID)
			}
		})
	}
}
