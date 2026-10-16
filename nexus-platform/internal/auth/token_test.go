package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testSecret      = "a-sufficiently-long-real-secret-value-1234567890"
	testUserID      = "user-123"
	testTenantID    = "tenant-abc"
	testRoleID      = "role-xyz"
	testAccessTTL   = 15 * time.Minute
	testRefreshTTL  = 7 * 24 * time.Hour
	otherSecretGood = "another-totally-different-secret-0987654321xyz"
)

func newTestService(t *testing.T) *TokenService {
	t.Helper()
	svc, err := NewTokenService(testSecret, testAccessTTL, testRefreshTTL)
	if err != nil {
		t.Fatalf("NewTokenService: unexpected error: %v", err)
	}
	return svc
}

func sampleSubject() Subject {
	return Subject{
		UserID:   testUserID,
		TenantID: testTenantID,
		RoleID:   testRoleID,
		IsAdmin:  true,
	}
}

// --- issue happy path ---------------------------------------------------------

func TestIssueAccessToken_HappyPath(t *testing.T) {
	svc := newTestService(t)

	tok, err := svc.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: unexpected error: %v", err)
	}
	if tok == "" {
		t.Fatal("IssueAccessToken: returned empty token")
	}
	// A JWT has three dot-separated segments.
	if got := strings.Count(tok, "."); got != 2 {
		t.Errorf("token segment count: got %d dots, want 2 (header.payload.signature)", got)
	}

	claims, err := svc.VerifyAccessToken(tok)
	if err != nil {
		t.Fatalf("VerifyAccessToken: unexpected error: %v", err)
	}
	if claims.UserID != testUserID {
		t.Errorf("UserID = %q, want %q", claims.UserID, testUserID)
	}
	if claims.TenantID != testTenantID {
		t.Errorf("TenantID = %q, want %q", claims.TenantID, testTenantID)
	}
	if claims.RoleID != testRoleID {
		t.Errorf("RoleID = %q, want %q", claims.RoleID, testRoleID)
	}
	if !claims.IsAdmin {
		t.Errorf("IsAdmin = false, want true")
	}
	if claims.TokenType != TokenTypeAccess {
		t.Errorf("TokenType = %q, want %q", claims.TokenType, TokenTypeAccess)
	}
}

func TestIssueRefreshToken_HappyPath(t *testing.T) {
	svc := newTestService(t)

	tok, err := svc.IssueRefreshToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueRefreshToken: unexpected error: %v", err)
	}
	claims, err := svc.VerifyRefreshToken(tok)
	if err != nil {
		t.Fatalf("VerifyRefreshToken: unexpected error: %v", err)
	}
	if claims.UserID != testUserID || claims.TenantID != testTenantID {
		t.Errorf("claims = %+v, want user/tenant carried through", claims)
	}
	if claims.TokenType != TokenTypeRefresh {
		t.Errorf("TokenType = %q, want %q", claims.TokenType, TokenTypeRefresh)
	}
}

// Access and refresh TTLs must come from config, so the two tokens must differ in expiry.
func TestIssue_TTLsFromConfig(t *testing.T) {
	svc := newTestService(t)
	sub := sampleSubject()

	at, err := svc.IssueAccessToken(sub)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	rt, err := svc.IssueRefreshToken(sub)
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}

	ac, err := svc.VerifyAccessToken(at)
	if err != nil {
		t.Fatalf("VerifyAccessToken: %v", err)
	}
	rc, err := svc.VerifyRefreshToken(rt)
	if err != nil {
		t.Fatalf("VerifyRefreshToken: %v", err)
	}

	accessLife := ac.ExpiresAt.Time.Sub(ac.IssuedAt.Time)
	refreshLife := rc.ExpiresAt.Time.Sub(rc.IssuedAt.Time)

	if accessLife < testAccessTTL-time.Second || accessLife > testAccessTTL+time.Second {
		t.Errorf("access lifetime = %v, want ~%v", accessLife, testAccessTTL)
	}
	if refreshLife < testRefreshTTL-time.Second || refreshLife > testRefreshTTL+time.Second {
		t.Errorf("refresh lifetime = %v, want ~%v", refreshLife, testRefreshTTL)
	}
}

// --- verify failure paths -----------------------------------------------------

func TestVerify_ExpiredToken(t *testing.T) {
	// Negative TTL => token is already expired the instant it is issued.
	svc, err := NewTokenService(testSecret, -1*time.Minute, testRefreshTTL)
	if err != nil {
		t.Fatalf("NewTokenService: %v", err)
	}
	tok, err := svc.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	verifier := newTestService(t)
	_, err = verifier.VerifyAccessToken(tok)
	if !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("VerifyAccessToken on expired token: got %v, want ErrTokenExpired", err)
	}
}

func TestVerify_TamperedSignature(t *testing.T) {
	svc := newTestService(t)
	tok, err := svc.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	// Flip the last character of the signature segment.
	tampered := flipLastChar(tok)
	_, err = svc.VerifyAccessToken(tampered)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyAccessToken on tampered token: got %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_WrongSecret(t *testing.T) {
	issuer := newTestService(t)
	tok, err := issuer.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	other, err := NewTokenService(otherSecretGood, testAccessTTL, testRefreshTTL)
	if err != nil {
		t.Fatalf("NewTokenService(other): %v", err)
	}
	_, err = other.VerifyAccessToken(tok)
	if !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("VerifyAccessToken with wrong secret: got %v, want ErrTokenInvalid", err)
	}
}

func TestVerify_MalformedToken(t *testing.T) {
	svc := newTestService(t)
	_, err := svc.VerifyAccessToken("not-a-jwt")
	if !errors.Is(err, ErrTokenMalformed) {
		t.Fatalf("VerifyAccessToken on garbage: got %v, want ErrTokenMalformed", err)
	}
}

// An access token must not validate as a refresh token and vice versa.
func TestVerify_WrongTokenType(t *testing.T) {
	svc := newTestService(t)

	access, err := svc.IssueAccessToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	if _, err := svc.VerifyRefreshToken(access); !errors.Is(err, ErrWrongTokenType) {
		t.Fatalf("VerifyRefreshToken on access token: got %v, want ErrWrongTokenType", err)
	}

	refresh, err := svc.IssueRefreshToken(sampleSubject())
	if err != nil {
		t.Fatalf("IssueRefreshToken: %v", err)
	}
	if _, err := svc.VerifyAccessToken(refresh); !errors.Is(err, ErrWrongTokenType) {
		t.Fatalf("VerifyAccessToken on refresh token: got %v, want ErrWrongTokenType", err)
	}
}

// alg-confusion guard (W1): a token signed with a non-HMAC method (here: "none")
// must be rejected, never accepted by trusting the header's alg.
func TestVerify_RejectsNonHMACAlg(t *testing.T) {
	svc := newTestService(t)

	claims := &Claims{
		Subject:   sampleSubject(),
		TokenType: TokenTypeAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	// Craft a token with alg=none (unsigned).
	unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	raw, err := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := svc.VerifyAccessToken(raw); err == nil {
		t.Fatal("VerifyAccessToken accepted an alg=none token; want rejection")
	}
}

// --- constructor validation ---------------------------------------------------

func TestNewTokenService_RejectsEmptySecret(t *testing.T) {
	if _, err := NewTokenService("", testAccessTTL, testRefreshTTL); err == nil {
		t.Fatal("NewTokenService with empty secret: expected error, got nil")
	}
}

// flipLastChar tampers with the signature segment of a JWT so the signature no longer
// verifies, while keeping the overall structure (3 dot-separated segments) intact. It flips
// a character in the MIDDLE of the signature — not the final base64url char, whose low-order
// bits can be truncated on decode and leave the signature bytes unchanged (which would make
// the tampered token still verify). The replacement char is always different from the original.
func flipLastChar(s string) string {
	if s == "" {
		return s
	}
	lastDot := strings.LastIndex(s, ".")
	if lastDot < 0 || lastDot == len(s)-1 {
		// No signature segment to tamper with; fall back to flipping the final byte.
		return flipByteAt(s, len(s)-1)
	}
	// Pick a position in the first half of the signature segment so every flipped bit lands
	// on a fully significant base64url character.
	sig := s[lastDot+1:]
	pos := lastDot + 1 + len(sig)/3
	return flipByteAt(s, pos)
}

// flipByteAt returns s with the byte at i replaced by a different character.
func flipByteAt(s string, i int) string {
	if i < 0 || i >= len(s) {
		return s
	}
	repl := byte('A')
	if s[i] == 'A' {
		repl = 'B'
	}
	return s[:i] + string(repl) + s[i+1:]
}
