package services

// specs/068-tenant-backup-restore T038 — an inbound webhook is checked against the tenant write
// freeze strictly AFTER its signature verifies: a correctly signed signal for a frozen tenant gets
// ErrTenantMaintenance (the partner retries later), while a bad signature stays the uniform
// ErrHookUnauthorized so nobody can probe which slugs exist or are under maintenance.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/inboundhook"
)

type freezeHookStore struct {
	*fakeHookStore
	hook *inboundhook.Hook
}

func (f *freezeHookStore) GetByName(context.Context, string, string) (*inboundhook.Hook, error) {
	return f.hook, nil
}

type slugResolver struct{}

func (slugResolver) ResolveTenantBySlug(_ context.Context, slug string) (*auth.Tenant, error) {
	return &auth.Tenant{ID: "tenant-1", Slug: slug}, nil
}

type stubFreeze struct {
	until time.Time
	err   error
	calls int
}

func (s *stubFreeze) FrozenUntil(context.Context, string) (time.Time, bool, error) {
	s.calls++
	return s.until, s.until.After(time.Now()), s.err
}

func signHook(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestInboundHookReceive_FreezeCheckedAfterSignature(t *testing.T) {
	body := []byte(`{"data":{"name":"x"}}`)
	hook := &inboundhook.Hook{ID: "h1", TenantID: "tenant-1", Name: "bank", EntityName: "customers", IsActive: true, Secret: "s3cret"}
	tests := []struct {
		name    string
		sig     string
		until   time.Time
		wantErr error
		calls   int
	}{
		{"good signature, frozen tenant -> maintenance", signHook("s3cret", body), time.Now().Add(time.Minute), ErrTenantMaintenance, 1},
		{"bad signature, frozen tenant -> uniform 401, freeze never consulted", "deadbeef", time.Now().Add(time.Minute), ErrHookUnauthorized, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := &stubFreeze{until: tc.until}
			svc := NewInboundHookService(&freezeHookStore{newFakeHookStore(), hook}, nil, slugResolver{}, nil).WithTenantFreeze(fr)
			_, err := svc.Receive(context.Background(), "acme", "bank", tc.sig, body)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if fr.calls != tc.calls {
				t.Fatalf("freeze consulted %d times, want %d", fr.calls, tc.calls)
			}
			var me *MaintenanceError
			if tc.wantErr == ErrTenantMaintenance && (!errors.As(err, &me) || !me.Until.Equal(tc.until)) {
				t.Fatalf("error must carry the deadline: %v", err)
			}
		})
	}
}
