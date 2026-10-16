package services

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// baseProvisionInput is a valid create request; each test mutates one field to exercise a
// rejection branch. All rejection branches return BEFORE any DB touch, so these run against a
// nil-db service (spec §5.1 "pure validations first").
func baseProvisionInput() ProvisionTenantInput {
	return ProvisionTenantInput{
		Slug:          "acme",
		Name:          "Công ty ACME",
		AdminEmail:    "admin@acme.com",
		AdminPassword: "StrongPass1",
	}
}

func provisionErrCode(t *testing.T, err error) (string, map[string]any) {
	t.Helper()
	if err == nil {
		t.Fatal("Provision returned nil error, want a ValidationError")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error is not a ValidationError: %v", err)
	}
	return ve.Code, ve.Details
}

func TestProvision_PureValidationRejections(t *testing.T) {
	// nil db is safe: every case below returns before the transaction (no I/O).
	svc := NewTenantProvisionService(nil)
	ctx := context.Background()

	cases := []struct {
		name      string
		mutate    func(*ProvisionTenantInput)
		wantCode  string
		wantField string // "" = don't check details.field
	}{
		{"slug empty", func(in *ProvisionTenantInput) { in.Slug = "" }, "TENANT_FIELD_REQUIRED", "slug"},
		// NB: "Acme" is NOT rejected — the service lowercase-trims first (spec §4.1), so it
		// normalizes to "acme". Only shapes that survive lowercasing and still fail the pattern
		// (spaces, punctuation, leading _/-) are invalid.
		{"slug spaces", func(in *ProvisionTenantInput) { in.Slug = "acme corp" }, "TENANT_SLUG_INVALID", ""},
		{"slug punctuation", func(in *ProvisionTenantInput) { in.Slug = "acme!" }, "TENANT_SLUG_INVALID", ""},
		{"email empty", func(in *ProvisionTenantInput) { in.AdminEmail = "" }, "TENANT_FIELD_REQUIRED", "adminEmail"},
		{"both password modes", func(in *ProvisionTenantInput) { in.GeneratePassword = true }, "TENANT_PASSWORD_CONFLICT", ""},
		{"neither password mode", func(in *ProvisionTenantInput) { in.AdminPassword = ""; in.GeneratePassword = false }, "TENANT_PASSWORD_CONFLICT", ""},
		{"weak password", func(in *ProvisionTenantInput) { in.AdminPassword = "aa" }, "PASSWORD_TOO_WEAK", ""},
		{"name too long", func(in *ProvisionTenantInput) { in.Name = strings.Repeat("a", maxTenantNameRunes+1) }, "TENANT_NAME_TOO_LONG", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseProvisionInput()
			tc.mutate(&in)
			_, err := svc.Provision(ctx, in)
			code, details := provisionErrCode(t, err)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			if tc.wantField != "" && details["field"] != tc.wantField {
				t.Errorf("details.field = %v, want %q", details["field"], tc.wantField)
			}
		})
	}
}

func TestGenerateInitialPassword_ValidAndUnique(t *testing.T) {
	p1, err := generateInitialPassword()
	if err != nil {
		t.Fatalf("generateInitialPassword: %v", err)
	}
	if err := ValidatePassword(p1); err != nil {
		t.Errorf("generated password fails RM-17 policy: %v", err)
	}
	p2, _ := generateInitialPassword()
	if p1 == p2 {
		t.Errorf("two generated passwords are identical (%q) — entropy source broken", p1)
	}
}

func TestValidTenantSlug(t *testing.T) {
	ok := []string{"acme", "a", "acme-corp", "acme_corp", "t1", "0abc"}
	bad := []string{"", "Acme", "acme corp", "acme!", "-acme", "_acme"}
	for _, s := range ok {
		if !ValidTenantSlug(s) {
			t.Errorf("slug %q should be accepted", s)
		}
	}
	for _, s := range bad {
		if ValidTenantSlug(s) {
			t.Errorf("slug %q should be rejected", s)
		}
	}
}
