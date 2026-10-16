package auth

// token_workspace_test.go — specs/062-customer-workspace T007. The builder workspace-session token
// is an ordinary typ=access JWT with three extra claims (act/att/wsid) and an exp capped at the
// session's own deadline. Tokens without those claims must keep their exact payload (omitempty).

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func workspaceSubject() Subject {
	return Subject{
		UserID: testUserID, TenantID: "customer-tenant", IsAdmin: true, SessionID: "login-sid-1",
		ActorUserID: testUserID, ActorTenantID: testTenantID, WorkspaceSessionID: "ws-1",
	}
}

func payloadOf(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}

func TestSubject_NormalToken_ByteIdentical(t *testing.T) {
	svc := newTestService(t)
	tok, err := svc.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	p := payloadOf(t, tok)
	for _, k := range []string{"act", "att", "wsid", "IsPlatformOperator"} {
		if _, ok := p[k]; ok {
			t.Errorf("normal access token payload gained claim %q: %v", k, p)
		}
	}
}

func TestIssueAccessTokenUntil(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		until   time.Time
		wantErr bool
		wantTTL time.Duration // expected exp - now, 0 when wantErr
	}{
		{"until beyond access TTL is capped to the access TTL", now.Add(8 * time.Hour), false, testAccessTTL},
		{"until sooner than the access TTL wins", now.Add(2 * time.Minute), false, 2 * time.Minute},
		{"until in the past is refused", now.Add(-time.Second), true, 0},
		{"until == now is refused", now, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			tok, err := svc.IssueAccessTokenUntil(workspaceSubject(), tc.until)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("IssueAccessTokenUntil: %v", err)
			}
			claims, err := svc.VerifyAccessToken(tok)
			if err != nil {
				t.Fatalf("VerifyAccessToken: %v", err)
			}
			if claims.Subject != workspaceSubject() {
				t.Errorf("round-trip subject: got %+v, want %+v", claims.Subject, workspaceSubject())
			}
			if got := claims.ExpiresAt.Time.Sub(now); got < tc.wantTTL-2*time.Second || got > tc.wantTTL+2*time.Second {
				t.Errorf("exp-now = %s, want ≈ %s", got, tc.wantTTL)
			}
			if claims.TokenType != TokenTypeAccess {
				t.Errorf("typ = %q, want access", claims.TokenType)
			}
		})
	}
}

// IsPlatformOperator is json:"-": a correctly signed JWT whose payload says true still verifies to false.
func TestSubject_IsPlatformOperatorIsNeverAClaim(t *testing.T) {
	svc := newTestService(t)
	claims := jwt.MapClaims{
		"uid": testUserID, "tid": testTenantID, "adm": true, "typ": "access",
		"IsPlatformOperator": true, "is_platform_operator": true,
		"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := svc.VerifyAccessToken(signed)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	if got.IsPlatformOperator {
		t.Fatal("IsPlatformOperator came out of a JWT payload — it must only ever be set by the auth stage")
	}
}
