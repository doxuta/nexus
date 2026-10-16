//go:build integration

// ENG-AUTH2 #7 MFA/TOTP — integration suite against a REAL MySQL 8, migrated through 000030
// (user_mfa, mfa_backup_codes, mfa_pending_tokens) via the SAME a4SetupMigratedDB /
// a4SeedTenant / a4SeedUser harness the A4 RBAC suite uses (mirrors its splitDSN/TEST_DATABASE_URL
// single-source DSN discipline — ../specs/003-mfa-totp/spec.md §"#7"). Run with:
//
//	go test -tags=integration -race ./internal/handlers/ -run TestMFAIT
//
// This suite specifically exercises what only a real database + the full protected-route chain
// (AuthMiddleware -> RequireTenant) can prove:
//   - enroll -> activate -> two-step login -> verify TOTP -> full token pair -> the access
//     token genuinely authorizes a protected call (GET /auth/me);
//   - a backup-code login is single-use under REAL concurrent double-submit (exactly one of two
//     simultaneous requests wins the atomic conditional UPDATE);
//   - the replay guard rejects the SAME TOTP code on a SECOND, independent login attempt (a
//     fresh pendingToken) even though it is still inside the +-1 skew window — proving the
//     matched-step CAS (not a wall-clock "now" CAS) under real MySQL, not a fake;
//   - disabling MFA reverts login to the ordinary one-step flow;
//   - a pendingToken for one tenant's user cannot be completed with another tenant's code (the
//     tenant is derived from the token's own row, never trusted from client input);
//   - MFA-STEPUP: enroll/activate/disable additionally require the caller's account password
//     (step-up re-auth) — a wrong password is 401 INVALID_CREDENTIALS, consumes a limiter
//     attempt exactly like a wrong code, and never touches the enrollment.
package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/pquerna/otp/totp"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/config"
	"nexus/platform/internal/crypto"
	"nexus/platform/internal/services"
)

const (
	mfaITTenantAID   = "b4ea0000-0000-0000-0000-0000000000a1"
	mfaITTenantASlug = "mfa-it-tenant-a"
	mfaITTenantBID   = "b4ea0000-0000-0000-0000-0000000000b2"
	mfaITTenantBSlug = "mfa-it-tenant-b"

	// mfaITTestKeyB64 is a fixed, valid 32-byte AES-256 key (base64) for MFA_SECRET_ENC_KEY —
	// a real, deterministic test key (not a placeholder string, which config.LoadJWTConfig-style
	// fail-fast guards target only the JWT secret; MFA's own fail-fast only rejects a
	// non-base64 or wrong-length value, both of which this is not).
	mfaITTestKeyB64 = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=" // base64("01234567890123456789012345678901")
)

// newMFAITRouter builds the production /api/v1 auth surface (Auth handler only — sufficient
// for login/refresh/me + the full mfa/* surface, mirrors newA4Router's minimal-handler-set
// pattern) wired EXACTLY like cmd/server/main.go: MFA config loaded from the environment,
// AES-GCM sealer constructed from it, and WithMFA injecting the real GORM-backed repositories
// over the migrated DB.
func newMFAITRouter(t *testing.T, gdb *gorm.DB) *gin.Engine {
	t.Helper()

	t.Setenv("JWT_SECRET", itJWTSecret)
	t.Setenv("JWT_ACCESS_TTL", "15m")
	t.Setenv("JWT_REFRESH_TTL", "168h")
	t.Setenv("MFA_SECRET_ENC_KEY", mfaITTestKeyB64)
	jwtCfg, err := config.LoadJWTConfig()
	if err != nil {
		t.Fatalf("config.LoadJWTConfig: %v", err)
	}
	tokenSvc, err := auth.NewTokenService(jwtCfg.JWTSecret, jwtCfg.AccessTTL, jwtCfg.RefreshTTL)
	if err != nil {
		t.Fatalf("auth.NewTokenService: %v", err)
	}
	mfaCfg, err := config.LoadMFAConfig()
	if err != nil {
		t.Fatalf("config.LoadMFAConfig: %v", err)
	}
	if !mfaCfg.Enabled {
		t.Fatal("expected MFA config Enabled=true with MFA_SECRET_ENC_KEY set")
	}
	sealer, err := crypto.New(mfaCfg.KeyID, mfaCfg.Key)
	if err != nil {
		t.Fatalf("crypto.New: %v", err)
	}

	userRepo := auth.NewUserRepository(gdb)
	refreshRepo := auth.NewRefreshTokenRepository(gdb)

	authSvc := services.NewAuthService(userRepo, refreshRepo, tokenSvc).
		WithMFA(
			auth.NewUserMFARepository(gdb),
			auth.NewMFABackupCodeRepository(gdb),
			auth.NewMFAPendingTokenRepository(gdb),
			sealer,
		)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterAPIRoutes(r.Group("/api/v1"), tokenSvc, nil, APIHandlers{
		Auth: NewAuthHandler(authSvc, zap.NewNop()),
	}, zap.NewNop())
	return r
}

// mfaLoginResponse decodes every shape POST /auth/login and POST /auth/mfa/verify can return
// (a superset of loginResponse — this file keeps its own copy so it can assert on mfaPending/
// pendingToken, which loginResponse does not carry).
type mfaLoginResponse struct {
	Data struct {
		AccessToken  string          `json:"accessToken"`
		RefreshToken string          `json:"refreshToken"`
		User         json.RawMessage `json:"user"`
		MFAPending   bool            `json:"mfaPending"`
		PendingToken string          `json:"pendingToken"`
	} `json:"data"`
	Error string `json:"error"`
	Code  string `json:"code"`
}

func mfaPostJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// mfaLoginExpectPending logs in and asserts the response IS the mfaPending shape, returning
// the pendingToken.
func mfaLoginExpectPending(t *testing.T, r *gin.Engine, tenantSlug, email, password string) string {
	t.Helper()
	w := mfaPostJSON(t, r, "/api/v1/auth/login", `{"tenant":"`+tenantSlug+`","email":"`+email+`","password":"`+password+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("login: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp mfaLoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login: %v; body=%s", err, w.Body.String())
	}
	if !resp.Data.MFAPending || resp.Data.PendingToken == "" {
		t.Fatalf("expected mfaPending response, got %s", w.Body.String())
	}
	if resp.Data.AccessToken != "" || resp.Data.RefreshToken != "" {
		t.Fatalf("mfaPending response leaked token material: %s", w.Body.String())
	}
	return resp.Data.PendingToken
}

// mfaEnrollResp / mfaActivateResp mirror the handler response shapes.
type mfaEnrollResp struct {
	Data struct {
		Secret     string `json:"secret"`
		OtpauthURL string `json:"otpauthUrl"`
	} `json:"data"`
}

type mfaActivateResp struct {
	Data struct {
		BackupCodes []string `json:"backupCodes"`
	} `json:"data"`
}

// mfaEnrollAndActivate drives POST /auth/mfa/enroll then /auth/mfa/activate through the REAL
// HTTP surface (protected — Bearer access token from an ordinary one-step login), returning
// the raw TOTP secret, the ten backup codes, and the EXACT time-step the activation code
// matched. Activation itself consumes that step via the replay guard (last_used_step), so a
// caller generating a FOLLOW-UP code (e.g. for a subsequent disable/verify call made moments
// later, likely still inside the same 30s wall-clock window) must use mfaCodeAtStep(secret,
// activatedStep+1) — NOT a fresh "now" code, which can collide with the just-consumed step and
// be spuriously rejected as a replay. Mirrors auth_mfa_test.go's codeAfterStep in the service
// unit suite.
func mfaEnrollAndActivate(t *testing.T, r *gin.Engine, accessToken string) (secret string, backupCodes []string, activatedStep int64) {
	t.Helper()
	// MFA-STEPUP: enroll/activate/disable all require the caller's account password in the
	// body (step-up re-authentication) — the seeded users all use authPassword.
	w := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/enroll", accessToken, map[string]any{"password": authPassword}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mfa/enroll: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var enrollResp mfaEnrollResp
	if err := json.Unmarshal(w.Body.Bytes(), &enrollResp); err != nil {
		t.Fatalf("decode enroll: %v; body=%s", err, w.Body.String())
	}
	secret = enrollResp.Data.Secret
	if secret == "" {
		t.Fatalf("empty secret in enroll response: %s", w.Body.String())
	}

	activatedStep = time.Now().UTC().Unix() / 30
	code := mfaCodeAtStep(t, secret, activatedStep)
	w = doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/activate", accessToken, map[string]any{"password": authPassword, "code": code}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("mfa/activate: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var actResp mfaActivateResp
	if err := json.Unmarshal(w.Body.Bytes(), &actResp); err != nil {
		t.Fatalf("decode activate: %v; body=%s", err, w.Body.String())
	}
	if len(actResp.Data.BackupCodes) != 10 {
		t.Fatalf("expected 10 backup codes, got %d", len(actResp.Data.BackupCodes))
	}
	return secret, actResp.Data.BackupCodes, activatedStep
}

// mfaCodeAtStep returns a TOTP code for the exact 30s step number.
func mfaCodeAtStep(t *testing.T, secret string, step int64) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Unix(step*30, 0).UTC(), totp.ValidateOpts{Period: 30, Digits: 6})
	if err != nil {
		t.Fatalf("generate totp code at step %d: %v", step, err)
	}
	return code
}

func mfaITCleanup(t *testing.T, gdb *gorm.DB, tenantIDs ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tid := range tenantIDs {
			gdb.Exec("DELETE FROM mfa_pending_tokens WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM mfa_backup_codes WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM user_mfa WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM refresh_tokens WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM users WHERE tenant_id = ?", tid)
			gdb.Exec("DELETE FROM tenants WHERE id = ?", tid)
		}
	})
}

// TestMFAIT_FullFlow_EnrollActivateLoginVerify_TokenWorks proves the complete happy path
// through the REAL HTTP surface + MySQL: enroll -> activate -> a SUBSEQUENT login now returns
// mfaPending -> verify with a valid TOTP code -> a full token pair -> the access token
// genuinely authorizes a protected call (GET /auth/me).
func TestMFAIT_FullFlow_EnrollActivateLoginVerify_TokenWorks(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	userID := a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	// Before enrollment: login is the ordinary one-step flow.
	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	if pre.Data.AccessToken == "" {
		t.Fatal("pre-enrollment login should be a normal one-step success")
	}

	secret, _, activatedStep := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)

	// After activation: login now returns mfaPending.
	pending := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)

	code := mfaCodeAtStep(t, secret, activatedStep+1)
	w := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pending+`","code":"`+code+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("mfa/verify: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var verifyResp mfaLoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &verifyResp); err != nil {
		t.Fatalf("decode verify: %v", err)
	}
	if verifyResp.Data.AccessToken == "" || verifyResp.Data.RefreshToken == "" {
		t.Fatalf("verify did not return a full token pair: %s", w.Body.String())
	}

	// The freshly minted access token genuinely authorizes a protected call.
	wMe := getMe(t, r, verifyResp.Data.AccessToken)
	if wMe.Code != http.StatusOK {
		t.Fatalf("GET /auth/me with the verified token: got %d, want 200; body=%s", wMe.Code, wMe.Body.String())
	}
	var me meResponse
	if err := json.Unmarshal(wMe.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.Data.ID != userID {
		t.Errorf("me.ID = %q; want %q", me.Data.ID, userID)
	}
}

// TestMFAIT_BackupCode_SingleUse_ConcurrentDoubleSubmit fires two simultaneous
// POST /auth/mfa/verify requests carrying the SAME backup code against two independently
// issued pendingTokens for the same account. Exactly one must succeed (real atomic conditional
// UPDATE on mfa_backup_codes.used_at) — proving the CAS under genuine MySQL concurrency, not a
// fake's mutex.
func TestMFAIT_BackupCode_SingleUse_ConcurrentDoubleSubmit(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	_, codes, _ := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)
	backupCode := codes[7]

	pendingA := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	pendingB := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)

	var wg sync.WaitGroup
	codesResult := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		w := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pendingA+`","code":"`+backupCode+`"}`)
		codesResult[0] = w.Code
	}()
	go func() {
		defer wg.Done()
		w := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pendingB+`","code":"`+backupCode+`"}`)
		codesResult[1] = w.Code
	}()
	wg.Wait()

	successCount := 0
	for _, c := range codesResult {
		if c == http.StatusOK {
			successCount++
		} else if c != http.StatusUnauthorized {
			t.Errorf("unexpected status %d (want 200 or 401)", c)
		}
	}
	if successCount != 1 {
		t.Fatalf("expected exactly 1 winner of the concurrent double-submit, got %d (codes=%v)", successCount, codesResult)
	}
}

// TestMFAIT_ReplayTOTP_SameStepRejectedOnSecondLoginAttempt proves the replay guard under REAL
// MySQL: a TOTP code accepted on one login attempt is rejected when presented again on a
// SEPARATE, independently issued pendingToken — even though it may still be inside the +-1
// step skew window — because the matched STEP (not wall-clock now) was already CAS'd into
// user_mfa.last_used_step.
func TestMFAIT_ReplayTOTP_SameStepRejectedOnSecondLoginAttempt(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	secret, _, activatedStep := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)

	// Use a code one step AHEAD of the activation step so it is guaranteed unconsumed by the
	// activation call itself (which already CAS'd last_used_step=activatedStep) yet still
	// inside the verify call's own +-1 skew window.
	code := mfaCodeAtStep(t, secret, activatedStep+1)

	pendingFirst := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	w1 := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pendingFirst+`","code":"`+code+`"}`)
	if w1.Code != http.StatusOK {
		t.Fatalf("first verify: got %d, want 200; body=%s", w1.Code, w1.Body.String())
	}

	pendingSecond := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	w2 := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pendingSecond+`","code":"`+code+`"}`)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed code on a fresh pendingToken: got %d, want 401; body=%s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "MFA_CODE_INVALID") {
		t.Errorf("expected MFA_CODE_INVALID, got %s", w2.Body.String())
	}
}

// TestMFAIT_DisableThenLogin_IsOneStepAgain proves disable genuinely reverts the account to
// the ordinary one-step login flow (no residual MFA state).
func TestMFAIT_DisableThenLogin_IsOneStepAgain(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	secret, _, activatedStep := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)

	code := mfaCodeAtStep(t, secret, activatedStep+1)
	wDisable := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/disable", pre.Data.AccessToken, map[string]any{"password": authPassword, "code": code}, nil)
	if wDisable.Code != http.StatusOK {
		t.Fatalf("mfa/disable: got %d, want 200; body=%s", wDisable.Code, wDisable.Body.String())
	}

	post := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	if post.Data.AccessToken == "" {
		t.Fatal("post-disable login should be a normal one-step success")
	}
}

// TestMFAIT_DisableAttemptThrottle_FifthWrongThenSixthBlocked proves the reviewer MED-1 /
// security review 🟡#4 per-account throttle end-to-end, through the REAL protected router
// chain and REAL MySQL: POST /auth/mfa/disable sits on the PROTECTED group (Auth+Tenant only —
// no RM-15, which only mounts on the PUBLIC /auth/* group), so a session holder could otherwise
// brute-force the code with zero rate limiting. After 5 wrong codes, the 6th attempt — even
// with the CORRECT code — must be rejected 429 MFA_TOO_MANY_ATTEMPTS with a Retry-After header,
// and the account must still be MFA-enrolled (never disabled).
func TestMFAIT_DisableAttemptThrottle_FifthWrongThenSixthBlocked(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	secret, _, activatedStep := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)

	for i := 0; i < 5; i++ {
		// Correct step-up password + wrong code: the failure being counted here is the CODE.
		w := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/disable", pre.Data.AccessToken, map[string]any{"password": authPassword, "code": "000000"}, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: got %d, want 401; body=%s", i+1, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "MFA_CODE_INVALID") {
			t.Errorf("attempt %d: expected MFA_CODE_INVALID, got %s", i+1, w.Body.String())
		}
	}

	// 6th attempt, WITH THE CORRECT CODE — must still be blocked by the throttle, proving the
	// guard trips before the code is even evaluated.
	code := mfaCodeAtStep(t, secret, activatedStep+1)
	w6 := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/disable", pre.Data.AccessToken, map[string]any{"password": authPassword, "code": code}, nil)
	if w6.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt (correct code): got %d, want 429; body=%s", w6.Code, w6.Body.String())
	}
	if !strings.Contains(w6.Body.String(), "MFA_TOO_MANY_ATTEMPTS") {
		t.Errorf("expected MFA_TOO_MANY_ATTEMPTS, got %s", w6.Body.String())
	}
	if w6.Header().Get("Retry-After") == "" {
		t.Error("expected a Retry-After header on the 429 response")
	}

	// The account must still be able to log in via the two-step MFA flow — i.e. it was NEVER
	// actually disabled by any of the 6 attempts above.
	pending := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	if pending == "" {
		t.Fatal("expected a still-enrolled account to require MFA on login")
	}
}

// TestMFAIT_CrossTenant_PendingTokenBoundToItsOwnTenantSecret proves tenant isolation on the
// verify path: even when an attacker somehow obtains tenant B's pendingToken (simulated here by
// simply holding the real value — the point is what happens NEXT, not how it leaked), it can
// only ever be completed with tenant B's OWN code. Presenting tenant A's valid code against
// tenant B's pendingToken fails, because CompleteMFALogin derives the (tenant,user) — and thus
// which secret to check the code against — from the pendingToken's OWN row, never from any
// client-supplied value.
func TestMFAIT_CrossTenant_PendingTokenBoundToItsOwnTenantSecret(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	a4SeedTenant(t, gdb, mfaITTenantBID, mfaITTenantBSlug, "MFA IT Tenant B")
	mfaITCleanup(t, gdb, mfaITTenantAID, mfaITTenantBID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)
	a4SeedUser(t, gdb, mfaITTenantBID, "bob@"+mfaITTenantBSlug, false, nil)

	preA := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	secretA, _, activatedStepA := mfaEnrollAndActivate(t, r, preA.Data.AccessToken)

	preB := itLogin(t, r, mfaITTenantBSlug, "bob@"+mfaITTenantBSlug, authPassword)
	secretB, _, activatedStepB := mfaEnrollAndActivate(t, r, preB.Data.AccessToken)

	pendingB := mfaLoginExpectPending(t, r, mfaITTenantBSlug, "bob@"+mfaITTenantBSlug, authPassword)

	// Tenant A's valid code against tenant B's pendingToken must fail — wrong secret entirely.
	codeA := mfaCodeAtStep(t, secretA, activatedStepA+1)
	wCross := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pendingB+`","code":"`+codeA+`"}`)
	if wCross.Code != http.StatusUnauthorized {
		t.Fatalf("tenant A code against tenant B pendingToken: got %d, want 401; body=%s", wCross.Code, wCross.Body.String())
	}

	// Sanity: tenant B's OWN code against its OWN pendingToken still works (the isolation above
	// is not accidentally rejecting everything).
	codeB := mfaCodeAtStep(t, secretB, activatedStepB+1)
	wOwn := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pendingB+`","code":"`+codeB+`"}`)
	if wOwn.Code != http.StatusOK {
		t.Fatalf("tenant B's own code against its own pendingToken: got %d, want 200; body=%s", wOwn.Code, wOwn.Body.String())
	}
}

// TestMFAIT_PendingToken_CannotBeUsedAsRefreshToken pins the keystone security property the
// dedicated mfa_pending_tokens table exists to guarantee: a pendingToken is NOT a JWT and must
// never be usable to call any other API — in particular it must not be redeemable at
// POST /auth/refresh to mint a fresh access token, which would let a password-correct-but-
// second-factor-missing attacker bypass MFA entirely by mistaking the pending bridge credential
// for a refresh token. This holds by construction (VerifyRefreshToken parses a JWT; an opaque
// pendingToken fails that parse), but is asserted here explicitly rather than left implicit.
func TestMFAIT_PendingToken_CannotBeUsedAsRefreshToken(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	secret, _, activatedStep := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)

	pending := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)

	wRefresh := postRefresh(t, r, `{"refreshToken":"`+pending+`"}`)
	if wRefresh.Code == http.StatusOK {
		t.Fatalf("pendingToken must NOT be redeemable at /auth/refresh; got 200, body=%s", wRefresh.Body.String())
	}
	if wRefresh.Code != http.StatusUnauthorized {
		t.Fatalf("pendingToken at /auth/refresh: got %d, want 401; body=%s", wRefresh.Code, wRefresh.Body.String())
	}

	// The pendingToken must still be usable for its ACTUAL purpose afterward — proving the
	// failed refresh attempt did not accidentally consume it.
	code := mfaCodeAtStep(t, secret, activatedStep+1)
	wVerify := mfaPostJSON(t, r, "/api/v1/auth/mfa/verify", `{"pendingToken":"`+pending+`","code":"`+code+`"}`)
	if wVerify.Code != http.StatusOK {
		t.Fatalf("mfa/verify after a failed refresh-token-abuse attempt: got %d, want 200; body=%s", wVerify.Code, wVerify.Body.String())
	}
}

// TestMFAIT_StepUp_WrongPasswordRejected_AttemptRecorded proves the MFA-STEPUP contract
// end-to-end against real MySQL: (1) POST /auth/mfa/disable with a WRONG step-up password —
// even alongside a perfectly VALID TOTP code — is rejected 401 INVALID_CREDENTIALS and the
// enrollment survives untouched; (2) each wrong password consumed a limiter attempt exactly
// like a wrong code would: after 5 wrong-password attempts the 6th call — with the CORRECT
// password AND a CORRECT code — is 429 MFA_TOO_MANY_ATTEMPTS; (3) a missing password is the
// 422 MFA_PASSWORD_REQUIRED client error and burns NO attempt (it worked as the 6th-call gate
// shows: only the 5 wrong-password calls counted).
func TestMFAIT_StepUp_WrongPasswordRejected_AttemptRecorded(t *testing.T) {
	gdb, _ := a4SetupMigratedDB(t)
	r := newMFAITRouter(t, gdb)
	a4SeedTenant(t, gdb, mfaITTenantAID, mfaITTenantASlug, "MFA IT Tenant A")
	mfaITCleanup(t, gdb, mfaITTenantAID)
	a4SeedUser(t, gdb, mfaITTenantAID, "alice@"+mfaITTenantASlug, false, nil)

	pre := itLogin(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	secret, _, activatedStep := mfaEnrollAndActivate(t, r, pre.Data.AccessToken)
	code := mfaCodeAtStep(t, secret, activatedStep+1)

	// A missing password is a 422 client error (never a credential failure, never counted).
	wMissing := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/disable", pre.Data.AccessToken, map[string]any{"code": code}, nil)
	if wMissing.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disable without password: got %d, want 422; body=%s", wMissing.Code, wMissing.Body.String())
	}
	if !strings.Contains(wMissing.Body.String(), "MFA_PASSWORD_REQUIRED") {
		t.Errorf("expected MFA_PASSWORD_REQUIRED, got %s", wMissing.Body.String())
	}

	// 5 wrong-password attempts, each carrying the VALID code — all rejected 401
	// INVALID_CREDENTIALS (the code is shielded behind the failed step-up, so it is never
	// consumed) and each consumes one limiter attempt.
	for i := 0; i < 5; i++ {
		w := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/disable", pre.Data.AccessToken, map[string]any{"password": "totally-wrong", "code": code}, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("wrong-password attempt %d: got %d, want 401; body=%s", i+1, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "INVALID_CREDENTIALS") {
			t.Errorf("wrong-password attempt %d: expected INVALID_CREDENTIALS, got %s", i+1, w.Body.String())
		}
	}

	// 6th attempt — correct password AND correct code — must be blocked by the shared
	// throttle, proving the 5 wrong passwords were recorded exactly like wrong codes.
	w6 := doAuthed(t, r, http.MethodPost, "/api/v1/auth/mfa/disable", pre.Data.AccessToken, map[string]any{"password": authPassword, "code": code}, nil)
	if w6.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt (correct password+code): got %d, want 429; body=%s", w6.Code, w6.Body.String())
	}
	if !strings.Contains(w6.Body.String(), "MFA_TOO_MANY_ATTEMPTS") {
		t.Errorf("expected MFA_TOO_MANY_ATTEMPTS, got %s", w6.Body.String())
	}
	if w6.Header().Get("Retry-After") == "" {
		t.Error("expected a Retry-After header on the 429 response")
	}

	// The account must still be MFA-enrolled — none of the seven calls above disabled it.
	pending := mfaLoginExpectPending(t, r, mfaITTenantASlug, "alice@"+mfaITTenantASlug, authPassword)
	if pending == "" {
		t.Fatal("expected a still-enrolled account to require MFA on login")
	}
}
