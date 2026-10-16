package handlers

// FE-CATCHUP-1 — GET /auth/me must carry the honest MFA state (`mfaEnabled` true/false, always
// present) so the FE MfaCard renders the real on/off instead of discovering it via action error
// responses (enroll→409, activate→422). The flag is resolved via the SAME crypto-free enrollment
// reader the login gate uses (no secret decryption), so these tests deliberately wire ONLY
// WithMFAEnrollmentReader — the "MFA_SECRET_ENC_KEY absent" production shape — and mint the
// access token directly (login itself is fail-closed 503 for an enrolled account in that shape).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/services"
)

// failingMFAEnrollment models a genuine user_mfa read fault (DB down): /auth/me must surface a
// 500 rather than silently claiming "MFA off" for a possibly-enrolled account.
type failingMFAEnrollment struct{}

func (failingMFAEnrollment) Get(_ context.Context, _, _ string) (*auth.UserMFA, error) {
	return nil, errors.New("user_mfa read failed: db down")
}

// newMeMFAFlagRouter mirrors newAuthTestRouter but optionally wires an enrollment reader (the
// key-absent production shape — WithMFA never called) and returns the token service so tests
// can mint an access token for the seeded u-1/t-1 user directly.
func newMeMFAFlagRouter(t *testing.T, reader interface {
	Get(ctx context.Context, tenantID, userID string) (*auth.UserMFA, error)
}) (*gin.Engine, *auth.TokenService) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	repo := &stubUserRepo{
		tenant: &auth.Tenant{ID: "t-1", Slug: "acme", Status: "active"},
		user: &auth.User{
			ID: "u-1", TenantID: "t-1", Email: "alice@acme.test",
			PasswordHash: string(hash), Status: "active",
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	svc := services.NewAuthService(repo, newStubRefreshRepo(), tokens)
	if reader != nil {
		svc = svc.WithMFAEnrollmentReader(reader, false)
	}

	r := gin.New()
	NewAuthHandler(svc, zap.NewNop()).RegisterRoutes(r.Group("/api/v1"))
	return r, tokens
}

// mintAccess issues a valid access token for the seeded u-1/t-1 user. /auth/me self-verifies
// the Bearer token, so no login round-trip is needed (and none is possible in the enrolled +
// key-absent shape, where login is deliberately fail-closed 503).
func mintAccess(t *testing.T, tokens *auth.TokenService) string {
	t.Helper()
	access, err := tokens.IssueAccessToken(auth.Subject{UserID: "u-1", TenantID: "t-1"})
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	return access
}

// TestMeHandler_MFAEnabledFlag drives the three honest states of /auth/me's mfaEnabled: an
// enrolled account reads true, a never-enrolled account reads false, and a server with no MFA
// support at all (no enrollment reader — bare fixture shape) still answers an explicit false.
// In every case the key must be PRESENT (a *bool nil would be omitted — that is the old
// "unknown" dishonesty this slice removes).
func TestMeHandler_MFAEnabledFlag(t *testing.T) {
	tests := []struct {
		name   string
		reader interface {
			Get(ctx context.Context, tenantID, userID string) (*auth.UserMFA, error)
		}
		want bool
	}{
		{
			name:   "enrolled user reads true",
			reader: &stubMFAEnrollment{tenantID: "t-1", userID: "u-1"},
			want:   true,
		},
		{
			name:   "non-enrolled user reads false",
			reader: &stubMFAEnrollment{tenantID: "t-1", userID: "u-someone-else"},
			want:   false,
		},
		{
			name:   "no enrollment reader wired reads false honestly",
			reader: nil,
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, tokens := newMeMFAFlagRouter(t, tt.reader)
			w := getWithAuth(r, "/api/v1/auth/me", mintAccess(t, tokens))
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (%s)", w.Code, w.Body.String())
			}
			var resp struct {
				Data struct {
					ID         string `json:"id"`
					MFAEnabled *bool  `json:"mfaEnabled"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Data.ID != "u-1" {
				t.Errorf("unexpected user: %+v", resp.Data)
			}
			if resp.Data.MFAEnabled == nil {
				t.Fatalf("mfaEnabled key ABSENT from /auth/me response (want explicit %v): %s", tt.want, w.Body.String())
			}
			if *resp.Data.MFAEnabled != tt.want {
				t.Errorf("mfaEnabled = %v; want %v (%s)", *resp.Data.MFAEnabled, tt.want, w.Body.String())
			}
		})
	}
}

// TestMeHandler_MFAFlagReadFault_500 pins the error contract: a genuine enrollment read fault
// must fail /auth/me (500 ME_FAILED) rather than silently answering mfaEnabled:false for an
// account that may well be enrolled.
func TestMeHandler_MFAFlagReadFault_500(t *testing.T) {
	r, tokens := newMeMFAFlagRouter(t, failingMFAEnrollment{})
	w := getWithAuth(r, "/api/v1/auth/me", mintAccess(t, tokens))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on enrollment read fault, got %d (%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Code != "ME_FAILED" {
		t.Errorf("code = %q; want ME_FAILED", resp.Code)
	}
}
