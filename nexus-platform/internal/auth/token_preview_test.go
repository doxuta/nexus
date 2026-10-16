package auth

// token_preview_test.go — specs/064-preview-as-role-sample-data T010. The preview token is a
// FOURTH token type: "read-only" is a property of the signed type, so every door that has not
// deliberately opened for it (VerifyAccessToken, VerifyRefreshToken) refuses it by construction.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func previewSubject() Subject {
	return Subject{
		UserID:        "5b1c0e39-2f7b-5d2e-9a57-4c1f0f6d9a10", // synthetic principal (UUIDv5)
		TenantID:      testTenantID,
		RoleID:        testRoleID,
		SessionID:     "preview-session-1",
		ActorUserID:   testUserID,
		ActorTenantID: testTenantID,
	}
}

func TestIssuePreviewToken_RoundTrip(t *testing.T) {
	svc := newTestService(t)
	exp := time.Now().Add(30 * time.Minute)

	tok, err := svc.IssuePreviewToken(previewSubject(), exp)
	if err != nil {
		t.Fatalf("IssuePreviewToken: %v", err)
	}
	claims, err := svc.VerifyPreviewToken(tok)
	if err != nil {
		t.Fatalf("VerifyPreviewToken: %v", err)
	}
	want := previewSubject()
	if claims.Subject != want {
		t.Errorf("subject round-trip: got %+v, want %+v", claims.Subject, want)
	}
	if claims.TokenType != TokenTypePreview {
		t.Errorf("typ: got %q, want %q", claims.TokenType, TokenTypePreview)
	}
	if got := claims.ExpiresAt.Time; got.Sub(exp) > time.Second || exp.Sub(got) > time.Second {
		t.Errorf("exp: got %s, want ≈ %s (the session row's expires_at, not the access TTL)", got, exp)
	}
}

func TestIssuePreviewToken_RefusesPastExpiry(t *testing.T) {
	svc := newTestService(t)
	if _, err := svc.IssuePreviewToken(previewSubject(), time.Now().Add(-time.Second)); err == nil {
		t.Fatal("IssuePreviewToken with an expiry in the past: want error, got nil")
	}
}

// Every door that did not deliberately open for typ=preview refuses it: an access-token door
// (every protected route through a plain verifier, GET /auth/me) and the refresh door.
func TestVerifyAccessAndRefresh_RejectPreviewToken(t *testing.T) {
	svc := newTestService(t)
	tok, err := svc.IssuePreviewToken(previewSubject(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("IssuePreviewToken: %v", err)
	}
	if _, err := svc.VerifyAccessToken(tok); !errors.Is(err, ErrWrongTokenType) {
		t.Errorf("VerifyAccessToken(preview): got %v, want ErrWrongTokenType", err)
	}
	if _, err := svc.VerifyRefreshToken(tok); !errors.Is(err, ErrWrongTokenType) {
		t.Errorf("VerifyRefreshToken(preview): got %v, want ErrWrongTokenType", err)
	}
}

func TestVerifyPreviewToken_RejectsAccessAndRefresh(t *testing.T) {
	svc := newTestService(t)
	access, err := svc.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	refresh, err := svc.IssueRefreshToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	for name, tok := range map[string]string{"access": access, "refresh": refresh} {
		if _, err := svc.VerifyPreviewToken(tok); !errors.Is(err, ErrWrongTokenType) {
			t.Errorf("VerifyPreviewToken(%s): got %v, want ErrWrongTokenType", name, err)
		}
	}
}

// A payload that carries the builder flag under its Go field name must not smuggle it in:
// IsPlatformOperator is json:"-", so no JWT can ever set it.
func TestVerifyPreviewToken_IgnoresSmuggledOperatorFlag(t *testing.T) {
	svc := newTestService(t)
	now := time.Now()
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"uid": "p", "tid": testTenantID, "rid": testRoleID, "sid": "s", "act": testUserID, "att": testTenantID,
		"typ": string(TokenTypePreview), "IsPlatformOperator": true, "isPlatformOperator": true,
		"exp": now.Add(time.Minute).Unix(), "iat": now.Unix(),
	})
	tok, err := forged.SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := svc.VerifyPreviewToken(tok)
	if err != nil {
		t.Fatalf("VerifyPreviewToken: %v", err)
	}
	if claims.IsPlatformOperator {
		t.Fatal("IsPlatformOperator came from the JWT payload; it must never be a claim")
	}
}

// payloadKeys decodes the JWT payload segment and returns its top-level keys.
func payloadKeys(t *testing.T, tok string) map[string]bool {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	keys := map[string]bool{}
	for k := range m {
		keys[k] = true
	}
	return keys
}

// Golden: adding act/att/wsid/pdr/IsPlatformOperator to Subject leaves every access/refresh token
// minted without them carrying exactly the pre-feature claim set (omitempty / json:"-").
func TestAccessAndRefreshPayload_UnchangedByPreviewFields(t *testing.T) {
	svc := newTestService(t)
	sub := sampleSubject()
	sub.SessionID = "login-session-1"
	access, err := svc.IssueAccessToken(sub)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	refresh, err := svc.IssueRefreshToken(sub)
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	want := []string{"uid", "tid", "rid", "adm", "sid", "typ", "jti", "iat", "nbf", "exp", "sub"}
	for name, tok := range map[string]string{"access": access, "refresh": refresh} {
		keys := payloadKeys(t, tok)
		if len(keys) != len(want) {
			t.Errorf("%s payload keys: got %v, want exactly %v", name, keys, want)
		}
		for _, k := range want {
			if !keys[k] {
				t.Errorf("%s payload: missing %q", name, k)
			}
		}
	}
}
