package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
)

// --- MFA test doubles ---------------------------------------------------------------------

type fakeMFARepo struct {
	mu   sync.Mutex
	rows map[string]*auth.UserMFA // key tenantID + "\x00" + userID

	// backup, when set, is where ActivateWithStepAndBackupCodes actually persists the backup
	// codes on a successful (simulated-transaction) call — wired by newMFAFixture so the
	// existing f.backup.count(...) assertions keep working after the reviewer LOW-1 activate+
	// store merge.
	backup *fakeBackupRepo
	// failBackupStore, when true, makes ActivateWithStepAndBackupCodes simulate the backup-code
	// INSERT failing INSIDE the (fake) transaction: the enabled flip is rolled back (never
	// committed) and an error is returned, mirroring what a real GORM Transaction() rollback
	// does — proves MFAActivate's contract that a failed store leaves the row at enabled=0 for
	// a clean retry (reviewer LOW-1).
	failBackupStore bool
}

func newFakeMFARepo() *fakeMFARepo { return &fakeMFARepo{rows: map[string]*auth.UserMFA{}} }

func (f *fakeMFARepo) key(tenantID, userID string) string { return tenantID + "\x00" + userID }

func (f *fakeMFARepo) Get(_ context.Context, tenantID, userID string) (*auth.UserMFA, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(tenantID, userID)]
	if !ok {
		return nil, auth.ErrMFANotEnrolled
	}
	cp := *row
	return &cp, nil
}

func (f *fakeMFARepo) Create(_ context.Context, m *auth.UserMFA) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(m.TenantID, m.UserID)
	if _, exists := f.rows[k]; exists {
		return fmt.Errorf("fakeMFARepo: duplicate (tenant,user)")
	}
	cp := *m
	f.rows[k] = &cp
	return nil
}

func (f *fakeMFARepo) UpdateSecret(_ context.Context, tenantID, userID, secretEnc string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(tenantID, userID)]
	if !ok || row.Enabled {
		return fmt.Errorf("fakeMFARepo: no matching not-yet-enabled row")
	}
	row.SecretEnc = secretEnc
	row.LastUsedStep = 0
	return nil
}

func (f *fakeMFARepo) ActivateWithStepAndBackupCodes(ctx context.Context, tenantID, userID string, step int64, codes []*auth.MFABackupCode) (bool, error) {
	f.mu.Lock()
	row, ok := f.rows[f.key(tenantID, userID)]
	if !ok || row.Enabled {
		f.mu.Unlock()
		return false, nil
	}
	if f.failBackupStore {
		f.mu.Unlock()
		// Simulated transaction rollback: the flip below never actually commits.
		return false, fmt.Errorf("fakeMFARepo: simulated backup-code store failure")
	}
	row.Enabled = true
	row.LastUsedStep = step
	f.mu.Unlock()

	if f.backup != nil {
		if err := f.backup.StoreBatch(ctx, codes); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (f *fakeMFARepo) ConsumeStep(_ context.Context, tenantID, userID string, step int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(tenantID, userID)]
	if !ok || !row.Enabled || row.LastUsedStep >= step {
		return false, nil
	}
	row.LastUsedStep = step
	return true, nil
}

func (f *fakeMFARepo) Delete(_ context.Context, tenantID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, f.key(tenantID, userID))
	return nil
}

type fakeBackupRepo struct {
	mu   sync.Mutex
	rows map[string][]*auth.MFABackupCode // key tenantID + "\x00" + userID
}

func newFakeBackupRepo() *fakeBackupRepo {
	return &fakeBackupRepo{rows: map[string][]*auth.MFABackupCode{}}
}

func (f *fakeBackupRepo) StoreBatch(_ context.Context, codes []*auth.MFABackupCode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range codes {
		k := c.TenantID + "\x00" + c.UserID
		cp := *c
		f.rows[k] = append(f.rows[k], &cp)
	}
	return nil
}

func (f *fakeBackupRepo) ConsumeByHash(_ context.Context, tenantID, userID, codeHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := tenantID + "\x00" + userID
	for _, c := range f.rows[k] {
		if c.CodeHash == codeHash && c.UsedAt == nil {
			now := time.Now()
			c.UsedAt = &now
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeBackupRepo) DeleteAllForUser(_ context.Context, tenantID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, tenantID+"\x00"+userID)
	return nil
}

func (f *fakeBackupRepo) count(tenantID, userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows[tenantID+"\x00"+userID])
}

type fakePendingRepo struct {
	mu     sync.Mutex
	byHash map[string]*auth.MFAPendingToken
	now    func() time.Time
}

func newFakePendingRepo() *fakePendingRepo {
	return &fakePendingRepo{byHash: map[string]*auth.MFAPendingToken{}, now: time.Now}
}

func (f *fakePendingRepo) Store(_ context.Context, pt *auth.MFAPendingToken) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *pt
	f.byHash[pt.TokenHash] = &cp
	return nil
}

func (f *fakePendingRepo) FindByHash(_ context.Context, tokenHash string) (*auth.MFAPendingToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pt, ok := f.byHash[tokenHash]
	if !ok {
		return nil, auth.ErrMFAPendingInvalid
	}
	cp := *pt
	return &cp, nil
}

func (f *fakePendingRepo) ConsumeByHash(_ context.Context, tenantID, tokenHash string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pt, ok := f.byHash[tokenHash]
	if !ok || pt.TenantID != tenantID || pt.UsedAt != nil || !pt.ExpiresAt.After(f.now()) {
		return false, nil
	}
	t := f.now()
	pt.UsedAt = &t
	return true, nil
}

// expire forces the row matching raw's hash into the past, for the expired-token test.
func (f *fakePendingRepo) expire(raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if pt, ok := f.byHash[auth.HashRefreshToken(raw)]; ok {
		pt.ExpiresAt = time.Now().Add(-time.Minute)
	}
}

// DeleteAllForUser purges every pending token for (tenantID, userID) — the fake mirrors
// auth.MFAPendingTokenRepository.DeleteAllForUser (reviewer LOW-2 hygiene fix).
func (f *fakePendingRepo) DeleteAllForUser(_ context.Context, tenantID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for hash, pt := range f.byHash {
		if pt.TenantID == tenantID && pt.UserID == userID {
			delete(f.byHash, hash)
		}
	}
	return nil
}

// countForUser returns how many pending-token rows remain for (tenantID, userID) — test helper
// for the MFADisable pending-token cleanup assertion.
func (f *fakePendingRepo) countForUser(tenantID, userID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, pt := range f.byHash {
		if pt.TenantID == tenantID && pt.UserID == userID {
			n++
		}
	}
	return n
}

// fakeSealer is a deterministic, reversible stand-in for crypto.AESGCM — real AEAD
// round-trip/tamper/wrong-key behaviour is proven by internal/crypto's own test suite;
// these service-level tests only need Seal/Open to compose correctly with the MFA flows.
type fakeSealer struct{}

const fakeSealPrefix = "sealed:"

func (fakeSealer) Seal(pt []byte) (string, error) { return fakeSealPrefix + string(pt), nil }

func (fakeSealer) Open(sealed string) ([]byte, error) {
	if !strings.HasPrefix(sealed, fakeSealPrefix) {
		return nil, fmt.Errorf("fakeSealer: malformed envelope")
	}
	return []byte(strings.TrimPrefix(sealed, fakeSealPrefix)), nil
}

// mfaFixture bundles an MFA-wired AuthService with its fakes and the seeded tenant/user/
// password, so each test can drive Enroll/Activate/Disable/Login/CompleteMFALogin and also
// inspect repo state directly.
type mfaFixture struct {
	svc      *AuthService
	mfaRepo  *fakeMFARepo
	backup   *fakeBackupRepo
	pending  *fakePendingRepo
	userRepo *fakeUserRepo
	tenantID string
	userID   string
	email    string
	password string
	// passwordHash is the bcrypt hash of password — kept so tests seeding EXTRA users (e.g.
	// the per-account-independence lockout test) can give them the same known password.
	passwordHash string
}

func newMFAFixture(t *testing.T) *mfaFixture {
	t.Helper()
	const password = "s3cret-pw"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	userRepo := &fakeUserRepo{
		tenants: map[string]*auth.Tenant{
			"acme": {ID: "t-1", Slug: "acme", Name: "Acme", Status: "active"},
		},
		users: map[string]*auth.User{
			"t-1\x00alice@acme.test": {
				ID:           "u-1",
				TenantID:     "t-1",
				Email:        "alice@acme.test",
				PasswordHash: string(hash),
				Status:       "active",
				IsAdmin:      true,
			},
		},
	}
	tokens, err := auth.NewTokenService("super-secret-test-key-0123456789", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	mfaRepo := newFakeMFARepo()
	backup := newFakeBackupRepo()
	pending := newFakePendingRepo()
	mfaRepo.backup = backup // wires ActivateWithStepAndBackupCodes's simulated-transaction insert
	svc := NewAuthService(userRepo, newFakeRefreshRepo(), tokens).
		WithMFA(mfaRepo, backup, pending, fakeSealer{})

	return &mfaFixture{
		svc: svc, mfaRepo: mfaRepo, backup: backup, pending: pending, userRepo: userRepo,
		tenantID: "t-1", userID: "u-1", email: "alice@acme.test", password: password,
		passwordHash: string(hash),
	}
}

// enrollAndActivate drives Enroll+Activate to a fully-active MFA account and returns the raw
// TOTP secret, the ten backup codes, and the exact time-step the activation code matched
// (activation itself consumes that step via the replay guard — see codeAfterStep).
func (f *mfaFixture) enrollAndActivate(t *testing.T) (secret string, backupCodes []string, activatedStep int64) {
	t.Helper()
	ctx := context.Background()
	secret, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	code, step := matchingCode(t, secret)
	codes, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code)
	if err != nil {
		t.Fatalf("MFAActivate: %v", err)
	}
	return secret, codes, step
}

// matchingCode returns a code valid for secret's CURRENT step plus the step number.
func matchingCode(t *testing.T, secret string) (string, int64) {
	t.Helper()
	now := time.Now().UTC()
	step := now.Unix() / totpPeriodSeconds
	return codeAtStep(t, secret, step), step
}

// codeAfterStep returns a code for the step immediately AFTER priorStep. Tests that already
// consumed priorStep (e.g. via activation, which stamps last_used_step) must use this — not
// matchingCode — to get past the replay guard's CAS (last_used_step < matchedStep) for their
// NEXT verification, exactly like a real user's next 30s-later code would.
func codeAfterStep(t *testing.T, secret string, priorStep int64) string {
	t.Helper()
	return codeAtStep(t, secret, priorStep+1)
}

// --- Enroll --------------------------------------------------------------------------------

func TestMFAEnroll_FirstTime_CreatesRow(t *testing.T) {
	f := newMFAFixture(t)
	secret, url, err := f.svc.MFAEnroll(context.Background(), f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	if secret == "" || url == "" {
		t.Fatalf("MFAEnroll returned empty secret/url: %q / %q", secret, url)
	}
	row, err := f.mfaRepo.Get(context.Background(), f.tenantID, f.userID)
	if err != nil {
		t.Fatalf("Get after enroll: %v", err)
	}
	if row.Enabled {
		t.Error("row.Enabled = true; want false right after enroll (before activate)")
	}
}

func TestMFAEnroll_AlreadyEnabled_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t)

	if _, _, err := f.svc.MFAEnroll(context.Background(), f.tenantID, f.userID, f.password); !errors.Is(err, auth.ErrMFAAlreadyEnabled) {
		t.Fatalf("MFAEnroll on already-enabled account: err = %v; want auth.ErrMFAAlreadyEnabled", err)
	}
}

func TestMFAEnroll_ReEnrollBeforeActivation_OverwritesSecret(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	secret1, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("first MFAEnroll: %v", err)
	}
	secret2, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("second MFAEnroll: %v", err)
	}
	if secret1 == secret2 {
		t.Fatal("re-enroll returned the SAME secret twice — want a fresh secret each call")
	}
	// Activating with a code from the FIRST (overwritten) secret must now fail.
	code, _ := matchingCode(t, secret1)
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("activate with stale (overwritten) secret: err = %v; want ErrMFACodeInvalid", err)
	}
}

func TestMFAEnroll_Unavailable_WhenNotWired(t *testing.T) {
	svc, _ := newTestService(t) // no WithMFA call
	if _, _, err := svc.MFAEnroll(context.Background(), "t-1", "u-1", "any-password"); !errors.Is(err, ErrMFAUnavailable) {
		t.Fatalf("MFAEnroll on a non-MFA-wired service: err = %v; want ErrMFAUnavailable", err)
	}
}

// --- Activate --------------------------------------------------------------------------------

func TestMFAActivate_Success_EnablesAndReturnsTenBackupCodes(t *testing.T) {
	f := newMFAFixture(t)
	_, codes, _ := f.enrollAndActivate(t)
	if len(codes) != mfaBackupCodeCount {
		t.Fatalf("len(backup codes) = %d; want %d", len(codes), mfaBackupCodeCount)
	}
	row, err := f.mfaRepo.Get(context.Background(), f.tenantID, f.userID)
	if err != nil {
		t.Fatalf("Get after activate: %v", err)
	}
	if !row.Enabled {
		t.Error("row.Enabled = false after activate; want true")
	}
	if f.backup.count(f.tenantID, f.userID) != mfaBackupCodeCount {
		t.Errorf("stored backup code rows = %d; want %d", f.backup.count(f.tenantID, f.userID), mfaBackupCodeCount)
	}
}

func TestMFAActivate_WrongCode_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password); err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("MFAActivate with wrong code: err = %v; want ErrMFACodeInvalid", err)
	}
}

func TestMFAActivate_NotEnrolled_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	if _, err := f.svc.MFAActivate(context.Background(), f.tenantID, f.userID, f.password, "123456"); !errors.Is(err, auth.ErrMFANotEnrolled) {
		t.Fatalf("MFAActivate without enroll: err = %v; want auth.ErrMFANotEnrolled", err)
	}
}

func TestMFAActivate_AlreadyEnabled_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	code := codeAfterStep(t, secret, activatedStep)
	if _, err := f.svc.MFAActivate(context.Background(), f.tenantID, f.userID, f.password, code); !errors.Is(err, auth.ErrMFAAlreadyEnabled) {
		t.Fatalf("MFAActivate twice: err = %v; want auth.ErrMFAAlreadyEnabled", err)
	}
}

// TestMFAActivate_BackupCodeStoreFails_RollsBackEnabledFlip is the reviewer LOW-1 regression
// test: when the backup-code insert fails INSIDE ActivateWithStepAndBackupCodes's transaction,
// the enabled=1 flip must roll back too — leaving the row at enabled=0 so the caller can simply
// retry activation, instead of being permanently stuck "MFA enabled" with zero backup codes and
// no way back except disable-by-TOTP (re-activating would hit ErrMFAAlreadyEnabled forever).
func TestMFAActivate_BackupCodeStoreFails_RollsBackEnabledFlip(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	secret, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}

	f.mfaRepo.failBackupStore = true
	code, _ := matchingCode(t, secret)
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code); err == nil {
		t.Fatal("MFAActivate: want an error when the backup-code store fails, got nil")
	}

	row, err := f.mfaRepo.Get(ctx, f.tenantID, f.userID)
	if err != nil {
		t.Fatalf("Get after failed activate: %v", err)
	}
	if row.Enabled {
		t.Fatal("row.Enabled = true after a failed backup-code store; want false (rolled back)")
	}
	if n := f.backup.count(f.tenantID, f.userID); n != 0 {
		t.Errorf("backup code rows stored after a failed activate = %d; want 0", n)
	}

	// Retry, now with the store succeeding, must work — proving the account is NOT stuck.
	f.mfaRepo.failBackupStore = false
	code2, _ := matchingCode(t, secret)
	codes, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code2)
	if err != nil {
		t.Fatalf("retry MFAActivate after fixing the store: %v", err)
	}
	if len(codes) != mfaBackupCodeCount {
		t.Fatalf("retry: len(backup codes) = %d; want %d", len(codes), mfaBackupCodeCount)
	}
}

// TestMFAActivate_FiveWrongCodes_SixthBlockedRegardlessOfCorrectness is the reviewer MED-1 /
// security review 🟡#4 regression test: after mfaAttemptMaxFailures wrong codes, the NEXT
// attempt is rejected as ErrMFATooManyAttempts EVEN WHEN the code presented is actually
// correct — proving the guard trips BEFORE the code is checked at all (fail-secure), not just
// that wrong codes keep failing.
func TestMFAActivate_FiveWrongCodes_SixthBlockedRegardlessOfCorrectness(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	secret, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}

	for i := 0; i < mfaAttemptMaxFailures; i++ {
		if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
			t.Fatalf("attempt %d: err = %v; want ErrMFACodeInvalid", i+1, err)
		}
	}

	code, _ := matchingCode(t, secret)
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code); !errors.Is(err, ErrMFATooManyAttempts) {
		t.Fatalf("6th attempt (CORRECT code): err = %v; want ErrMFATooManyAttempts", err)
	}

	row, err := f.mfaRepo.Get(ctx, f.tenantID, f.userID)
	if err != nil {
		t.Fatalf("Get after lockout: %v", err)
	}
	if row.Enabled {
		t.Fatal("row.Enabled = true despite the 6th (correct) attempt being blocked by the lockout")
	}
}

// --- Disable -------------------------------------------------------------------------------

func TestMFADisable_ByTOTP_Success_DeletesEverything(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	code := codeAfterStep(t, secret, activatedStep)

	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, code); err != nil {
		t.Fatalf("MFADisable: %v", err)
	}
	if _, err := f.mfaRepo.Get(context.Background(), f.tenantID, f.userID); !errors.Is(err, auth.ErrMFANotEnrolled) {
		t.Fatalf("Get after disable: err = %v; want auth.ErrMFANotEnrolled (row deleted)", err)
	}
	if n := f.backup.count(f.tenantID, f.userID); n != 0 {
		t.Errorf("backup codes remaining after disable = %d; want 0", n)
	}
}

func TestMFADisable_ByBackupCode_Success(t *testing.T) {
	f := newMFAFixture(t)
	_, codes, _ := f.enrollAndActivate(t)

	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, codes[0]); err != nil {
		t.Fatalf("MFADisable via backup code: %v", err)
	}
	if _, err := f.mfaRepo.Get(context.Background(), f.tenantID, f.userID); !errors.Is(err, auth.ErrMFANotEnrolled) {
		t.Fatalf("Get after disable: err = %v; want auth.ErrMFANotEnrolled", err)
	}
}

func TestMFADisable_WrongCode_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t)
	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("MFADisable with wrong code: err = %v; want ErrMFACodeInvalid", err)
	}
	// Row must still exist — a failed disable must not have side effects.
	if _, err := f.mfaRepo.Get(context.Background(), f.tenantID, f.userID); err != nil {
		t.Fatalf("Get after failed disable: %v; want row to still exist", err)
	}
}

func TestMFADisable_NotEnrolled_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, "123456"); !errors.Is(err, auth.ErrMFANotEnrolled) {
		t.Fatalf("MFADisable without enroll: err = %v; want auth.ErrMFANotEnrolled", err)
	}
}

// TestMFADisable_PurgesPendingTokens is the reviewer LOW-2 hygiene regression test: a
// still-live two-step-login pendingToken issued before disable must be gone afterward (not a
// security hole either way — CompleteMFALogin re-checks mfa.Enabled — but disable should not
// leave orphaned bridge-token rows for a since-disabled account).
func TestMFADisable_PurgesPendingTokens(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	// Issue a pending token via a normal Login (MFA branch) that is NEVER completed.
	loginOut, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if loginOut.PendingToken == "" {
		t.Fatal("Login did not mint a pendingToken")
	}
	if n := f.pending.countForUser(f.tenantID, f.userID); n != 1 {
		t.Fatalf("pending tokens before disable = %d; want 1", n)
	}

	code := codeAfterStep(t, secret, activatedStep)
	if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, code); err != nil {
		t.Fatalf("MFADisable: %v", err)
	}

	if n := f.pending.countForUser(f.tenantID, f.userID); n != 0 {
		t.Errorf("pending tokens after disable = %d; want 0 (not purged)", n)
	}

	// Belt-and-suspenders: the orphaned token (if it had survived) could never complete a login
	// anyway, since CompleteMFALogin re-checks mfa.Enabled — confirm that contract still holds.
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, code); !errors.Is(err, auth.ErrMFAPendingInvalid) {
		t.Fatalf("CompleteMFALogin with a pre-disable pendingToken: err = %v; want auth.ErrMFAPendingInvalid", err)
	}
}

// --- Attempt throttle (reviewer MED-1 / security review 🟡#4) ------------------------------

// TestMFADisable_FiveWrongCodes_SixthBlockedRegardlessOfCorrectness mirrors the MFAActivate
// variant above: after mfaAttemptMaxFailures wrong codes, even the CORRECT code is rejected as
// ErrMFATooManyAttempts on the very next call — proving the guard runs BEFORE the code is
// checked, not merely that wrong codes keep failing. The account must remain enrolled
// throughout (none of the wrong attempts, nor the blocked 6th, disabled it).
func TestMFADisable_FiveWrongCodes_SixthBlockedRegardlessOfCorrectness(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	for i := 0; i < mfaAttemptMaxFailures; i++ {
		if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
			t.Fatalf("attempt %d: err = %v; want ErrMFACodeInvalid", i+1, err)
		}
	}

	code := codeAfterStep(t, secret, activatedStep)
	if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, code); !errors.Is(err, ErrMFATooManyAttempts) {
		t.Fatalf("6th attempt (CORRECT code): err = %v; want ErrMFATooManyAttempts", err)
	}

	if _, err := f.mfaRepo.Get(ctx, f.tenantID, f.userID); err != nil {
		t.Fatalf("Get after lockout: %v; want the row to still exist (never disabled)", err)
	}
}

// TestMFADisable_LockoutIsPerAccountIndependent proves the throttle is keyed per (tenant_id,
// user_id), not global to the process: locking out user A on the SAME AuthService/limiter
// instance must not affect an unrelated user B.
func TestMFADisable_LockoutIsPerAccountIndependent(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t) // user A (f.tenantID/f.userID)
	ctx := context.Background()

	// Seed a second, independent, already-enabled MFA account directly (same AuthService
	// instance => same underlying attempt limiter as user A). MFA-STEPUP: user B also needs a
	// real user row (same known password) so the step-up password check passes and the call
	// genuinely reaches the code check.
	secretB, _, err := generateTOTPSecret("bob@acme.test")
	if err != nil {
		t.Fatalf("generateTOTPSecret: %v", err)
	}
	tenantB, userB := "t-2", "u-2"
	f.userRepo.users[tenantB+"\x00bob@acme.test"] = &auth.User{
		ID: userB, TenantID: tenantB, Email: "bob@acme.test",
		PasswordHash: f.passwordHash, Status: "active",
	}
	f.mfaRepo.rows[f.mfaRepo.key(tenantB, userB)] = &auth.UserMFA{
		ID: "mfa-2", TenantID: tenantB, UserID: userB,
		SecretEnc: fakeSealPrefix + secretB, Enabled: true,
	}

	for i := 0; i < mfaAttemptMaxFailures; i++ {
		f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, "000000") //nolint:errcheck // intentionally burning attempts
	}
	if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFATooManyAttempts) {
		t.Fatalf("user A should be locked out by now: err = %v", err)
	}

	// User B must be unaffected: a wrong code still maps to the ordinary ErrMFACodeInvalid, not
	// a lockout inherited from user A's failures.
	if err := f.svc.MFADisable(ctx, tenantB, userB, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("user B: err = %v; want ErrMFACodeInvalid (must NOT be locked out by user A)", err)
	}
}

// spyMFAAttemptLimiter records every Allow/RecordFailure/Reset call it receives, so a test can
// assert the SERVICE wired the throttle's success/failure branches correctly without relying on
// a real sliding window (which is separately proven by mfa_attempt_limiter_test.go) — and,
// importantly, without needing to chain multiple real MFADisable/MFAActivate calls on ONE
// account, which is not possible: a correct call terminally mutates state (disable deletes the
// row; activate flips it to enabled), so a "wrong,wrong,correct,wrong..." sequence cannot be
// driven through the real handlers past the first correct call.
type spyMFAAttemptLimiter struct {
	mu        sync.Mutex
	allow     bool // returned by every Allow call; defaults to true (zero value) via newSpy
	failures  []string
	resets    []string
	allowCall int
}

func newSpyMFAAttemptLimiter() *spyMFAAttemptLimiter {
	return &spyMFAAttemptLimiter{allow: true}
}

func (s *spyMFAAttemptLimiter) Allow(tenantID, userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.allowCall++
	return s.allow
}

func (s *spyMFAAttemptLimiter) RecordFailure(tenantID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, tenantID+"\x00"+userID)
}

func (s *spyMFAAttemptLimiter) Reset(tenantID, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets = append(s.resets, tenantID+"\x00"+userID)
}

// TestMFADisable_Success_ResetsAttemptCounterViaLimiter proves the WIRING of the success path:
// MFADisable must call Reset (not RecordFailure) exactly once when the presented code is
// correct.
func TestMFADisable_Success_ResetsAttemptCounterViaLimiter(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	code := codeAfterStep(t, secret, activatedStep)
	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, code); err != nil {
		t.Fatalf("MFADisable: %v", err)
	}

	wantKey := f.tenantID + "\x00" + f.userID
	if len(spy.resets) != 1 || spy.resets[0] != wantKey {
		t.Errorf("resets = %v; want exactly [%q]", spy.resets, wantKey)
	}
	if len(spy.failures) != 0 {
		t.Errorf("failures = %v; want none on a successful disable", spy.failures)
	}
}

// TestMFADisable_WrongCode_RecordsFailureViaLimiter proves the WIRING of the failure path:
// MFADisable must call RecordFailure (not Reset) exactly once when the presented code is wrong.
func TestMFADisable_WrongCode_RecordsFailureViaLimiter(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t)
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("MFADisable: err = %v; want ErrMFACodeInvalid", err)
	}

	wantKey := f.tenantID + "\x00" + f.userID
	if len(spy.failures) != 1 || spy.failures[0] != wantKey {
		t.Errorf("failures = %v; want exactly [%q]", spy.failures, wantKey)
	}
	if len(spy.resets) != 0 {
		t.Errorf("resets = %v; want none on a failed disable", spy.resets)
	}
}

// TestMFAActivate_Success_ResetsAttemptCounterViaLimiter is MFADisable's Reset-wiring test
// mirrored onto MFAActivate.
func TestMFAActivate_Success_ResetsAttemptCounterViaLimiter(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	secret, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	code, _ := matchingCode(t, secret)
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code); err != nil {
		t.Fatalf("MFAActivate: %v", err)
	}

	wantKey := f.tenantID + "\x00" + f.userID
	if len(spy.resets) != 1 || spy.resets[0] != wantKey {
		t.Errorf("resets = %v; want exactly [%q]", spy.resets, wantKey)
	}
	if len(spy.failures) != 0 {
		t.Errorf("failures = %v; want none on a successful activate", spy.failures)
	}
}

// TestMFAActivate_WrongCode_RecordsFailureViaLimiter is MFADisable's RecordFailure-wiring test
// mirrored onto MFAActivate.
func TestMFAActivate_WrongCode_RecordsFailureViaLimiter(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	if _, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password); err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("MFAActivate: err = %v; want ErrMFACodeInvalid", err)
	}

	wantKey := f.tenantID + "\x00" + f.userID
	if len(spy.failures) != 1 || spy.failures[0] != wantKey {
		t.Errorf("failures = %v; want exactly [%q]", spy.failures, wantKey)
	}
	if len(spy.resets) != 0 {
		t.Errorf("resets = %v; want none on a failed activate", spy.resets)
	}
}

// TestMFADisable_BlockedByLimiter_NeverReachesCodeCheck proves the guard short-circuits BEFORE
// touching the repo/crypto at all: with an Allow-always-false spy, even a correct code (which
// would normally succeed) is rejected as ErrMFATooManyAttempts, and the limiter records zero
// failures/resets (the code was never evaluated).
func TestMFADisable_BlockedByLimiter_NeverReachesCodeCheck(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	spy := newSpyMFAAttemptLimiter()
	spy.allow = false
	f.svc.WithMFAAttemptLimiter(spy)

	code := codeAfterStep(t, secret, activatedStep)
	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, f.password, code); !errors.Is(err, ErrMFATooManyAttempts) {
		t.Fatalf("MFADisable with Allow=false: err = %v; want ErrMFATooManyAttempts", err)
	}
	if len(spy.failures) != 0 || len(spy.resets) != 0 {
		t.Errorf("failures=%v resets=%v; want both empty (code must never be evaluated)", spy.failures, spy.resets)
	}
	if spy.allowCall != 1 {
		t.Errorf("Allow called %d times; want exactly 1", spy.allowCall)
	}
}

// --- Login (MFA branch) + CompleteMFALogin --------------------------------------------------

func TestLogin_MFAEnabled_ReturnsPendingNotTokens(t *testing.T) {
	f := newMFAFixture(t)
	f.enrollAndActivate(t)

	out, err := f.svc.Login(context.Background(), LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !out.MFAPending {
		t.Fatal("MFAPending = false; want true for an MFA-enabled account")
	}
	if out.PendingToken == "" {
		t.Fatal("PendingToken is empty")
	}
	if out.AccessToken != "" || out.RefreshToken != "" || out.User != nil {
		t.Fatalf("token/user fields must be zero on the pending response, got AccessToken=%q RefreshToken=%q User=%v",
			out.AccessToken, out.RefreshToken, out.User)
	}
}

func TestCompleteMFALogin_TOTP_Success_IssuesTokensAndConsumesPending(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	loginOut, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	code := codeAfterStep(t, secret, activatedStep)

	out, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, code)
	if err != nil {
		t.Fatalf("CompleteMFALogin: %v", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" || out.User == nil {
		t.Fatalf("CompleteMFALogin result incomplete: %+v", out)
	}
	if out.User.ID != f.userID {
		t.Errorf("User.ID = %q; want %q", out.User.ID, f.userID)
	}
	if out.MFAPending {
		t.Error("MFAPending = true on the completed result; want false")
	}

	// Single-use: the SAME pendingToken must not work a second time.
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, code); !errors.Is(err, auth.ErrMFAPendingInvalid) {
		t.Fatalf("second CompleteMFALogin with the same pendingToken: err = %v; want auth.ErrMFAPendingInvalid", err)
	}
}

func TestCompleteMFALogin_BackupCode_Success_SingleUse(t *testing.T) {
	f := newMFAFixture(t)
	_, codes, _ := f.enrollAndActivate(t)
	ctx := context.Background()

	loginOut, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	out, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, codes[3])
	if err != nil {
		t.Fatalf("CompleteMFALogin via backup code: %v", err)
	}
	if out.AccessToken == "" {
		t.Fatal("expected a real access token from a backup-code login")
	}

	// The SAME backup code must now be single-use: a second login attempt with it fails.
	loginOut2, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut2.PendingToken, codes[3]); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("reuse of a consumed backup code: err = %v; want ErrMFACodeInvalid", err)
	}
}

func TestCompleteMFALogin_WrongCode_PendingTokenStaysUsable(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	loginOut, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("wrong code: err = %v; want ErrMFACodeInvalid", err)
	}

	// Retry with the CORRECT code and the SAME pendingToken must still succeed — a wrong
	// guess must not burn the pending token (normal login UX allows a mistyped code retry).
	code := codeAfterStep(t, secret, activatedStep)
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, code); err != nil {
		t.Fatalf("retry with correct code: %v", err)
	}
}

func TestCompleteMFALogin_UnknownPendingToken_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	if _, err := f.svc.CompleteMFALogin(context.Background(), "not-a-real-token", "123456"); !errors.Is(err, auth.ErrMFAPendingInvalid) {
		t.Fatalf("unknown pendingToken: err = %v; want auth.ErrMFAPendingInvalid", err)
	}
}

func TestCompleteMFALogin_ExpiredPendingToken_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	loginOut, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	f.pending.expire(loginOut.PendingToken)

	code := codeAfterStep(t, secret, activatedStep)
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut.PendingToken, code); !errors.Is(err, auth.ErrMFAPendingInvalid) {
		t.Fatalf("expired pendingToken: err = %v; want auth.ErrMFAPendingInvalid", err)
	}
}

func TestCompleteMFALogin_EmptyPendingToken_Rejected(t *testing.T) {
	f := newMFAFixture(t)
	if _, err := f.svc.CompleteMFALogin(context.Background(), "", "123456"); !errors.Is(err, auth.ErrMFAPendingInvalid) {
		t.Fatalf("empty pendingToken: err = %v; want auth.ErrMFAPendingInvalid", err)
	}
}

// TestMFAReplay_SameCodeRejectedOnSecondLoginAttempt is the unit-level proof of the replay
// guard (../specs/003-mfa-totp/spec.md §"#7" MUST-FIX 2): a code accepted once must not validate
// again on a SEPARATE login attempt (a fresh pendingToken) even though it is still inside the
// +-1 step skew window. The real atomic-CAS proof under concurrent MySQL access is an
// integration test (eng_mfa_totp_integration_test.go); this proves the service-level wiring
// forwards the matched step (not wall-clock now) into ConsumeStep correctly.
func TestMFAReplay_SameCodeRejectedOnSecondLoginAttempt(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	code := codeAfterStep(t, secret, activatedStep)

	loginOut1, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("first Login: %v", err)
	}
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut1.PendingToken, code); err != nil {
		t.Fatalf("first CompleteMFALogin: %v", err)
	}

	loginOut2, err := f.svc.Login(ctx, LoginInput{Tenant: "acme", Email: f.email, Password: f.password})
	if err != nil {
		t.Fatalf("second Login: %v", err)
	}
	// Same code, same time-step, a DIFFERENT (fresh) pendingToken — must be rejected as replay.
	if _, err := f.svc.CompleteMFALogin(ctx, loginOut2.PendingToken, code); !errors.Is(err, ErrMFACodeInvalid) {
		t.Fatalf("replayed code on a fresh pendingToken: err = %v; want ErrMFACodeInvalid", err)
	}
}

// --- MFA-STEPUP: password re-authentication on enroll/activate/disable ----------------------

// TestMFAStepUp_EmptyPassword_ValidationError_NoAttemptBurned proves all three use cases
// reject a missing password as a client error (422 MFA_PASSWORD_REQUIRED family) BEFORE the
// limiter guard runs — a forgotten field must neither consume an attempt nor probe the guard.
func TestMFAStepUp_EmptyPassword_ValidationError_NoAttemptBurned(t *testing.T) {
	tests := []struct {
		name string
		call func(f *mfaFixture) error
	}{
		{"enroll", func(f *mfaFixture) error {
			_, _, err := f.svc.MFAEnroll(context.Background(), f.tenantID, f.userID, "")
			return err
		}},
		{"activate", func(f *mfaFixture) error {
			_, err := f.svc.MFAActivate(context.Background(), f.tenantID, f.userID, "", "123456")
			return err
		}},
		{"disable", func(f *mfaFixture) error {
			return f.svc.MFADisable(context.Background(), f.tenantID, f.userID, "", "123456")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMFAFixture(t)
			spy := newSpyMFAAttemptLimiter()
			f.svc.WithMFAAttemptLimiter(spy)

			err := tt.call(f)
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Code != "MFA_PASSWORD_REQUIRED" {
				t.Fatalf("err = %v; want *ValidationError{Code: MFA_PASSWORD_REQUIRED}", err)
			}
			if spy.allowCall != 0 || len(spy.failures) != 0 || len(spy.resets) != 0 {
				t.Errorf("limiter touched on a missing-field error: allowCall=%d failures=%v resets=%v; want all zero",
					spy.allowCall, spy.failures, spy.resets)
			}
		})
	}
}

// TestMFAEnroll_WrongPassword_InvalidCredentials_ThenLockout proves the two new enroll
// properties together: (1) a wrong step-up password returns ErrInvalidCredentials AND consumes
// a limiter attempt; (2) enroll — which previously had NO throttle — now trips the same
// 5/15min lockout, and the 6th attempt is blocked even with the CORRECT password (guard runs
// before anything is evaluated, fail-secure).
func TestMFAEnroll_WrongPassword_InvalidCredentials_ThenLockout(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()

	for i := 0; i < mfaAttemptMaxFailures; i++ {
		if _, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, "totally-wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d: err = %v; want ErrInvalidCredentials", i+1, err)
		}
	}

	if _, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password); !errors.Is(err, ErrMFATooManyAttempts) {
		t.Fatalf("6th enroll (CORRECT password): err = %v; want ErrMFATooManyAttempts", err)
	}

	// No enrollment row may exist — none of the six attempts got past the step-up gate.
	if _, err := f.mfaRepo.Get(ctx, f.tenantID, f.userID); !errors.Is(err, auth.ErrMFANotEnrolled) {
		t.Fatalf("Get after lockout: err = %v; want auth.ErrMFANotEnrolled (nothing enrolled)", err)
	}
}

// TestMFAActivate_WrongPassword_RecordsFailureViaLimiter proves the wiring: a wrong step-up
// password on activate consumes exactly one limiter attempt (like a wrong TOTP code) and never
// resets.
func TestMFAActivate_WrongPassword_RecordsFailureViaLimiter(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()
	secret, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll: %v", err)
	}
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	code, _ := matchingCode(t, secret)
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, "totally-wrong", code); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("MFAActivate with wrong password: err = %v; want ErrInvalidCredentials", err)
	}

	wantKey := f.tenantID + "\x00" + f.userID
	if len(spy.failures) != 1 || spy.failures[0] != wantKey {
		t.Errorf("failures = %v; want exactly [%q]", spy.failures, wantKey)
	}
	if len(spy.resets) != 0 {
		t.Errorf("resets = %v; want none on a wrong-password activate", spy.resets)
	}

	// The account must NOT have been activated: the correct code was shielded by the
	// failed step-up.
	row, err := f.mfaRepo.Get(ctx, f.tenantID, f.userID)
	if err != nil {
		t.Fatalf("Get after wrong-password activate: %v", err)
	}
	if row.Enabled {
		t.Fatal("row.Enabled = true after a wrong-password activate; want false")
	}
}

// TestMFADisable_WrongPassword_Rejected_RowIntact proves a wrong step-up password on disable —
// even when accompanied by a perfectly VALID TOTP code — is rejected as ErrInvalidCredentials,
// leaves the enrollment untouched, and consumes one limiter attempt.
func TestMFADisable_WrongPassword_Rejected_RowIntact(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	code := codeAfterStep(t, secret, activatedStep)
	if err := f.svc.MFADisable(context.Background(), f.tenantID, f.userID, "totally-wrong", code); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("MFADisable with wrong password + valid code: err = %v; want ErrInvalidCredentials", err)
	}

	wantKey := f.tenantID + "\x00" + f.userID
	if len(spy.failures) != 1 || spy.failures[0] != wantKey {
		t.Errorf("failures = %v; want exactly [%q]", spy.failures, wantKey)
	}
	if len(spy.resets) != 0 {
		t.Errorf("resets = %v; want none on a wrong-password disable", spy.resets)
	}

	// The enrollment must still exist and still be enabled — nothing was deleted.
	row, err := f.mfaRepo.Get(context.Background(), f.tenantID, f.userID)
	if err != nil {
		t.Fatalf("Get after wrong-password disable: %v; want row to still exist", err)
	}
	if !row.Enabled {
		t.Fatal("row.Enabled = false after a rejected disable; want true (untouched)")
	}
}

// TestMFAStepUp_PasswordAndCodeFailures_ShareOneCounter proves the lockout counter is ONE
// shared per-account budget: 2 wrong passwords + 3 wrong codes (correct password) = 5 failures,
// so the 6th attempt — with EVERYTHING correct — is blocked. Brute-forcing the password through
// these endpoints hits exactly the same wall as brute-forcing the code.
func TestMFAStepUp_PasswordAndCodeFailures_ShareOneCounter(t *testing.T) {
	f := newMFAFixture(t)
	secret, _, activatedStep := f.enrollAndActivate(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, "totally-wrong", "000000"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong-password attempt %d: err = %v; want ErrInvalidCredentials", i+1, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
			t.Fatalf("wrong-code attempt %d: err = %v; want ErrMFACodeInvalid", i+1, err)
		}
	}

	code := codeAfterStep(t, secret, activatedStep)
	if err := f.svc.MFADisable(ctx, f.tenantID, f.userID, f.password, code); !errors.Is(err, ErrMFATooManyAttempts) {
		t.Fatalf("6th attempt (correct password AND code): err = %v; want ErrMFATooManyAttempts", err)
	}

	if _, err := f.mfaRepo.Get(ctx, f.tenantID, f.userID); err != nil {
		t.Fatalf("Get after lockout: %v; want the row to still exist (never disabled)", err)
	}
}

// TestMFAStepUp_UserMissing_InvalidCredentials_FailureRecorded exercises the constant-time
// branch of verifyCallerPassword: when the user row cannot be loaded at all (a ghost user id),
// the dummy bcrypt compare runs and the caller gets the SAME generic ErrInvalidCredentials as
// a wrong password — never a distinguishable not-found error — and the attempt is still
// recorded against the presented identity.
func TestMFAStepUp_UserMissing_InvalidCredentials_FailureRecorded(t *testing.T) {
	f := newMFAFixture(t)
	spy := newSpyMFAAttemptLimiter()
	f.svc.WithMFAAttemptLimiter(spy)

	if _, _, err := f.svc.MFAEnroll(context.Background(), f.tenantID, "ghost-user", "whatever"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("MFAEnroll for a missing user: err = %v; want ErrInvalidCredentials (generic, anti-probe)", err)
	}

	wantKey := f.tenantID + "\x00" + "ghost-user"
	if len(spy.failures) != 1 || spy.failures[0] != wantKey {
		t.Errorf("failures = %v; want exactly [%q]", spy.failures, wantKey)
	}
}

// TestMFAEnroll_Success_ResetsAttemptCounter_ActivateNotLockedOut is the dual-review LOW-1
// regression test: a fully successful enroll must reset the shared per-account attempt counter,
// exactly like activate/disable already do on their own success. Without the fix, 4 wrong
// step-up passwords on enroll (count=4) followed by a correct-password enroll (count stays 4,
// no reset) would leave only ONE budget slot before the very next MFAActivate call locks out —
// this test proves instead that a whole FRESH budget (mfaAttemptMaxFailures-1 wrong codes, all
// ErrMFACodeInvalid, never ErrMFATooManyAttempts) is available immediately after enroll
// succeeds, and the final correctly-coded activate call succeeds rather than being blocked.
func TestMFAEnroll_Success_ResetsAttemptCounter_ActivateNotLockedOut(t *testing.T) {
	f := newMFAFixture(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, "totally-wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("wrong-password enroll attempt %d: err = %v; want ErrInvalidCredentials", i+1, err)
		}
	}

	secret, _, err := f.svc.MFAEnroll(ctx, f.tenantID, f.userID, f.password)
	if err != nil {
		t.Fatalf("MFAEnroll with correct password: %v", err)
	}

	for i := 0; i < mfaAttemptMaxFailures-1; i++ {
		if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, "000000"); !errors.Is(err, ErrMFACodeInvalid) {
			t.Fatalf("post-enroll wrong-code activate attempt %d: err = %v; want ErrMFACodeInvalid (counter must have reset after the successful enroll)", i+1, err)
		}
	}

	code, _ := matchingCode(t, secret)
	if _, err := f.svc.MFAActivate(ctx, f.tenantID, f.userID, f.password, code); err != nil {
		t.Fatalf("final correct-code activate after enroll reset budget: err = %v; want success (not locked out)", err)
	}
}
