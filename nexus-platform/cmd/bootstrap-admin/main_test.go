package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/services"
)

// TestBootstrap_PureValidationBranches exercises the branches that must reject BEFORE any DB
// access. It passes a zero-value deps (nil db/repo/service): every case here must return an
// error while the pure validations are still running, so no nil-db dereference can happen. The
// password-rejection branch in particular proves services.ValidatePassword (RM-17) gates the
// tool the same way it gates every set-password path.
func TestBootstrap_PureValidationBranches(t *testing.T) {
	tests := []struct {
		name string
		in   input
		// Exactly one of wantSub / wantCode is set. wantSub matches a plain fmt.Errorf message;
		// wantCode asserts the wrapped error is a *services.ValidationError with that Code (the
		// RM-17 password policy surfaces its reason via Code, not the human message string).
		wantSub  string
		wantCode string
	}{
		{
			name:    "empty email",
			in:      input{tenant: "acme", email: "  ", password: "a-strong-pass"},
			wantSub: "BOOTSTRAP_ADMIN_EMAIL is required",
		},
		{
			name:    "slug with whitespace",
			in:      input{tenant: "acme corp", email: "admin@acme.com", password: "a-strong-pass"},
			wantSub: "not a valid slug",
		},
		{
			name:    "slug with symbol rejected",
			in:      input{tenant: "acme!", email: "admin@acme.com", password: "a-strong-pass"},
			wantSub: "not a valid slug",
		},
		{
			name:     "password too short (RM-17)",
			in:       input{tenant: "acme", email: "admin@acme.com", password: "short"},
			wantCode: "PASSWORD_TOO_WEAK",
		},
		{
			name:     "password all whitespace (RM-17)",
			in:       input{tenant: "acme", email: "admin@acme.com", password: "          "},
			wantCode: "PASSWORD_TOO_WEAK",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Zero-value deps: nil db. A pass-through to the DB would panic, which is exactly what
			// we DON'T want — every case must reject during pure validation.
			_, err := bootstrap(context.Background(), deps{}, tt.in)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tt.wantCode != "" {
				var ve *services.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("error %v is not a *services.ValidationError", err)
				}
				if ve.Code != tt.wantCode {
					t.Fatalf("ValidationError.Code = %q, want %q", ve.Code, tt.wantCode)
				}
				return
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tt.wantSub)
			}
		})
	}
}

func TestDefaultName(t *testing.T) {
	tests := []struct {
		email string
		want  string
	}{
		{"admin@acme.com", "admin"},
		{"  jane.doe@example.org  ", "jane.doe"},
		{"noatsign", "noatsign"},
		{"@leading", "@leading"}, // '@' at index 0 → no local-part, return as-is
	}
	for _, tt := range tests {
		if got := defaultName(tt.email); got != tt.want {
			t.Errorf("defaultName(%q) = %q, want %q", tt.email, got, tt.want)
		}
	}
}

// TestTenantSlugPattern pins the accepted/rejected slug shapes so a future edit cannot silently
// widen the login-slug surface.
func TestTenantSlugPattern(t *testing.T) {
	ok := []string{"acme", "a", "acme-corp", "acme_corp", "t1", "0abc", strings.Repeat("a", 64)}
	bad := []string{"", "Acme", "acme corp", "acme!", "-acme", "_acme", strings.Repeat("a", 65)}
	for _, s := range ok {
		if !services.ValidTenantSlug(s) {
			t.Errorf("slug %q should be accepted", s)
		}
	}
	for _, s := range bad {
		if services.ValidTenantSlug(s) {
			t.Errorf("slug %q should be rejected", s)
		}
	}
}
