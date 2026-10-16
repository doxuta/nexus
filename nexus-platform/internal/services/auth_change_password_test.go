package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"nexus/platform/internal/auth"
)

// buildAuthSvcForPassword seeds a fakeUserRepo with a single user whose password is plain,
// wires it over the existing fakeRefreshRepo + a real TokenService, and returns the service
// plus a handle to the refresh repo so tests can inspect revocation state.
func buildAuthSvcForPassword(t *testing.T, tenantID, userID, plain string) (*AuthService, *fakeUserRepo, *fakeRefreshRepo) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	u := &auth.User{
		ID:           userID,
		TenantID:     tenantID,
		Email:        "user@example.com",
		PasswordHash: string(hash),
		Status:       "active",
	}
	userRepo := &fakeUserRepo{
		byID:  map[string]*auth.User{tenantID + "\x00" + userID: u},
		users: map[string]*auth.User{tenantID + "\x00" + u.Email: u},
	}
	refreshRepo := newFakeRefreshRepo()
	tokens, err := auth.NewTokenService("test-secret-key-1234567890abcdef", 15*60*time.Second, 7*24*60*60*time.Second)
	if err != nil {
		t.Fatalf("token service: %v", err)
	}
	svc := NewAuthService(userRepo, refreshRepo, tokens)
	return svc, userRepo, refreshRepo
}

func TestAuthService_ChangePassword_Success(t *testing.T) {
	const (
		tenantID = "tenant-1"
		userID   = "user-1"
		oldPass  = "oldpassword123"
		newPass  = "newpassword456"
	)

	svc, userRepo, refreshRepo := buildAuthSvcForPassword(t, tenantID, userID, oldPass)

	// Plant a live refresh token so we can verify it gets revoked.
	liveToken := &auth.RefreshToken{
		ID:        "rt-1",
		TenantID:  tenantID,
		UserID:    userID,
		TokenHash: "somehash",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := refreshRepo.Store(context.Background(), liveToken); err != nil {
		t.Fatalf("seed refresh token: %v", err)
	}

	if err := svc.ChangePassword(context.Background(), tenantID, userID, oldPass, newPass, ""); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}

	// New hash must verify against newPass.
	u := userRepo.byID[tenantID+"\x00"+userID]
	if bcryptErr := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(newPass)); bcryptErr != nil {
		t.Errorf("stored hash does not match new password: %v", bcryptErr)
	}

	// The live refresh token must now be revoked.
	_, err := refreshRepo.FindLiveByHash(context.Background(), tenantID, "somehash")
	if !errors.Is(err, auth.ErrRefreshTokenNotFound) {
		t.Errorf("expected refresh token to be revoked after password change, got err=%v", err)
	}
}

func TestAuthService_ChangePassword_WrongCurrent(t *testing.T) {
	svc, _, _ := buildAuthSvcForPassword(t, "t1", "u1", "correctpassword")

	err := svc.ChangePassword(context.Background(), "t1", "u1", "wrongpassword", "newpassword456", "")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestAuthService_ChangePassword_WeakNew(t *testing.T) {
	svc, _, _ := buildAuthSvcForPassword(t, "t1", "u1", "oldpassword123")

	err := svc.ChangePassword(context.Background(), "t1", "u1", "oldpassword123", "short", "")
	if err == nil {
		t.Fatal("expected ValidationError for weak password, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if ve.Code != "PASSWORD_TOO_WEAK" {
		t.Errorf("code = %q, want PASSWORD_TOO_WEAK", ve.Code)
	}
}

func TestAuthService_ChangePassword_CrossTenantIsolation(t *testing.T) {
	// User exists in tenant-1; attempting change under tenant-2 must fail.
	svc, _, _ := buildAuthSvcForPassword(t, "tenant-1", "user-1", "oldpassword123")

	err := svc.ChangePassword(context.Background(), "tenant-2", "user-1", "oldpassword123", "newpassword456", "")
	// GetUserByID is tenant-scoped → returns ErrUserNotFound → mapped to ErrInvalidCredentials.
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("cross-tenant: expected ErrInvalidCredentials, got %v", err)
	}
}
