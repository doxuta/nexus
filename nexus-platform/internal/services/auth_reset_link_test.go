package services

// W7 invite flow — unit tests for AuthService.IssueResetLinkForUser over the SAME fakes the
// forgot-password suite uses (fakeResetRepo / spySender / newTestService seed: tenant t-1,
// active admin u-1). The full HTTP round-trip (admin issues link → user consumes it via
// POST /auth/reset-password → logs in with the new password) lives in the W7 integration
// suite; here we pin the contract: token stored hashed with the SAME TTL as forgot-password,
// honest 404/422 for unknown/inactive targets, mail queued only when SMTP is wired.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

func TestIssueResetLinkForUser(t *testing.T) {
	tests := []struct {
		name     string
		tenantID string
		userID   string
		seed     func(svc *AuthService)
		wantCode string
	}{
		{name: "happy path (active user)", tenantID: "t-1", userID: "u-1"},
		{name: "unknown user → USER_NOT_FOUND", tenantID: "t-1", userID: "ghost", wantCode: "USER_NOT_FOUND"},
		{name: "cross-tenant id misses (tenant-pinned load)", tenantID: "t-2", userID: "u-1", wantCode: "USER_NOT_FOUND"},
		{name: "blank user id → USER_NOT_FOUND", tenantID: "t-1", userID: "  ", wantCode: "USER_NOT_FOUND"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, repo, _ := newResetTestService(t)
			issue, err := svc.IssueResetLinkForUser(context.Background(), tt.tenantID, tt.userID)
			if tt.wantCode != "" {
				var ve *ValidationError
				if !errors.As(err, &ve) || ve.Code != tt.wantCode {
					t.Fatalf("err = %v; want ValidationError %s", err, tt.wantCode)
				}
				if repo.count() != 0 {
					t.Fatalf("no token row may be stored on refusal; got %d", repo.count())
				}
				return
			}
			if err != nil {
				t.Fatalf("IssueResetLinkForUser: %v", err)
			}
			if issue.Token == "" || !strings.Contains(issue.ResetURL, "?token="+issue.Token) {
				t.Fatalf("issue = %+v; want raw token embedded in the reset link", issue)
			}
			if issue.TTLMinutes != int(resetTokenTTL/time.Minute) {
				t.Fatalf("ttlMinutes = %d; want the forgot-password TTL %v", issue.TTLMinutes, resetTokenTTL)
			}
			if repo.count() != 1 {
				t.Fatalf("stored token rows = %d; want 1", repo.count())
			}
			// The stored row must be the SHA-256 hash of the returned token (same at-rest
			// contract as forgot-password) — the raw token is never persisted.
			if _, err := repo.FindByHash(context.Background(), auth.HashRefreshToken(issue.Token)); err != nil {
				t.Fatalf("stored row not found by the returned token's hash: %v", err)
			}
		})
	}
}

func TestIssueResetLinkForUser_MailAndInactive(t *testing.T) {
	t.Run("SMTP wired → courtesy copy queued to the user's address", func(t *testing.T) {
		svc, _, sender := newResetTestService(t) // synchronous dispatcher in tests
		issue, err := svc.IssueResetLinkForUser(context.Background(), "t-1", "u-1")
		if err != nil {
			t.Fatalf("IssueResetLinkForUser: %v", err)
		}
		if !issue.EmailQueued {
			t.Fatalf("EmailQueued = false; want true with a wired mailer")
		}
		if sender.count() != 1 || sender.last().To != "alice@acme.test" {
			t.Fatalf("mails = %d (last to %q); want 1 to alice@acme.test", sender.count(), sender.last().To)
		}
	})

	t.Run("no mailer → link-only, EmailQueued=false", func(t *testing.T) {
		svc, _ := newTestService(t)
		svc.WithResetRepo(newFakeResetRepo())
		issue, err := svc.IssueResetLinkForUser(context.Background(), "t-1", "u-1")
		if err != nil {
			t.Fatalf("IssueResetLinkForUser: %v", err)
		}
		if issue.EmailQueued {
			t.Fatalf("EmailQueued = true; want false without a mailer")
		}
		if issue.Token == "" {
			t.Fatalf("token must still be returned without a mailer")
		}
	})

	t.Run("inactive user refused 422 USER_NOT_ACTIVE, no token stored", func(t *testing.T) {
		svc, repo, _ := newResetTestService(t)
		// Flip the seeded user off "active": the fake's GetUserByID returns the seed pointer,
		// so mutating the returned struct mutates the seed.
		u, err := svc.users.GetUserByID(context.Background(), "t-1", "u-1")
		if err != nil {
			t.Fatalf("seed lookup: %v", err)
		}
		u.Status = "suspended"

		_, err = svc.IssueResetLinkForUser(context.Background(), "t-1", "u-1")
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Code != "USER_NOT_ACTIVE" {
			t.Fatalf("err = %v; want USER_NOT_ACTIVE", err)
		}
		if repo.count() != 0 {
			t.Fatalf("no token row may be stored for an inactive user; got %d", repo.count())
		}
	})
}
