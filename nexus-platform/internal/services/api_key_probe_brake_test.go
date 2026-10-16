package services

// Per-IP brake on failed API-key lookups (queue #356, specs/045 FR-9). #155 stopped a REPEATED dead
// string from reading sys_api_keys; a prober that changes the string every request still costs one
// point-read each. The brake counts only lookups that read the table and found nothing usable.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"nexus/platform/internal/auth"
)

func probeCtx(ip string) context.Context {
	return auth.WithClientIP(context.Background(), ip)
}

func freshProbe(i int) string { return fmt.Sprintf("nxk_%043d", i) }

func TestAPIKeyService_ProbeBrake(t *testing.T) {
	const budget = 5
	newSvc := func() (*APIKeyService, *fakeAPIKeyRepo, *time.Time) {
		repo := newFakeAPIKeyRepo()
		svc := newTestAPIKeyService(repo).WithProbeBrake(budget)
		clock := time.Now()
		svc.now = func() time.Time { return clock }
		return svc, repo, &clock
	}

	t.Run("a rotating prober is refused without a read once its budget is spent", func(t *testing.T) {
		svc, repo, _ := newSvc()
		ctx := probeCtx("203.0.113.9")
		for i := 0; i < budget; i++ {
			if _, err := svc.VerifyKey(ctx, freshProbe(i)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
				t.Fatalf("probe %d: err = %v, want ErrAPIKeyInvalid", i, err)
			}
		}
		reads := repo.findCount()
		for i := budget; i < budget+50; i++ {
			if _, err := svc.VerifyKey(ctx, freshProbe(i)); !errors.Is(err, auth.ErrAPIKeyThrottled) {
				t.Fatalf("probe %d past the budget: err = %v, want ErrAPIKeyThrottled", i, err)
			}
		}
		if got := repo.findCount(); got != reads {
			t.Fatalf("sys_api_keys read %d more times while braked, want 0", got-reads)
		}
	})

	t.Run("another IP is unaffected", func(t *testing.T) {
		svc, repo, _ := newSvc()
		for i := 0; i <= budget; i++ {
			_, _ = svc.VerifyKey(probeCtx("203.0.113.9"), freshProbe(i))
		}
		before := repo.findCount()
		if _, err := svc.VerifyKey(probeCtx("203.0.113.10"), freshProbe(900)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Fatalf("other IP: err = %v, want ErrAPIKeyInvalid (a normal lookup)", err)
		}
		if repo.findCount() != before+1 {
			t.Fatal("other IP's lookup did not read the table")
		}
	})

	t.Run("a live key that verified before still passes from a braked IP, and a revoke still lands", func(t *testing.T) {
		svc, _, _ := newSvc()
		ctx := context.Background()
		key, sec, err := svc.IssueAPIKey(ctx, "t1", IssueAPIKeyInput{Name: "erp", RoleID: "role-1"})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		braked := probeCtx("203.0.113.9")
		if _, err := svc.VerifyKey(braked, sec); err != nil {
			t.Fatalf("first verify: %v", err)
		}
		for i := 0; i < budget; i++ {
			_, _ = svc.VerifyKey(braked, freshProbe(i))
		}
		if _, err := svc.VerifyKey(braked, freshProbe(99)); !errors.Is(err, auth.ErrAPIKeyThrottled) {
			t.Fatalf("IP not braked: %v", err)
		}
		if _, err := svc.VerifyKey(braked, sec); err != nil {
			t.Fatalf("live key from a braked IP: %v — the brake hit a real integration", err)
		}
		if err := svc.RevokeAPIKey(ctx, "t1", key.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := svc.VerifyKey(braked, sec); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Fatalf("revoked key from a braked IP: err = %v, want ErrAPIKeyInvalid (still read, still refused)", err)
		}
	})

	t.Run("a repeated dead string costs no budget", func(t *testing.T) {
		svc, _, _ := newSvc()
		ctx := probeCtx("203.0.113.9")
		for i := 0; i < 10*budget; i++ {
			if _, err := svc.VerifyKey(ctx, freshProbe(1)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
				t.Fatalf("repeat %d: err = %v, want ErrAPIKeyInvalid (the #155 memo answers, no throttle)", i, err)
			}
		}
		if _, err := svc.VerifyKey(ctx, freshProbe(2)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Fatalf("a new string after the repeats: err = %v, want a normal lookup", err)
		}
	})

	t.Run("the window slides", func(t *testing.T) {
		svc, _, clock := newSvc()
		ctx := probeCtx("203.0.113.9")
		for i := 0; i <= budget; i++ {
			_, _ = svc.VerifyKey(ctx, freshProbe(i))
		}
		*clock = clock.Add(apiKeyProbeWindow + time.Second)
		if _, err := svc.VerifyKey(ctx, freshProbe(500)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
			t.Fatalf("after the window: err = %v, want a normal lookup", err)
		}
	})

	t.Run("no client IP, or no budget configured, means no brake", func(t *testing.T) {
		svc, _, _ := newSvc()
		for i := 0; i < 5*budget; i++ {
			if _, err := svc.VerifyKey(context.Background(), freshProbe(i)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
				t.Fatalf("no IP, probe %d: err = %v", i, err)
			}
		}
		off := newTestAPIKeyService(newFakeAPIKeyRepo())
		for i := 0; i < 5*budget; i++ {
			if _, err := off.VerifyKey(probeCtx("203.0.113.9"), freshProbe(i)); !errors.Is(err, auth.ErrAPIKeyInvalid) {
				t.Fatalf("brake off, probe %d: err = %v", i, err)
			}
		}
	})

	t.Run("the per-IP memory is bounded", func(t *testing.T) {
		svc, _, _ := newSvc()
		for i := 0; i <= apiKeyProbeIPsMax+10; i++ {
			_, _ = svc.VerifyKey(probeCtx(fmt.Sprintf("10.%d.%d.%d", i>>16&255, i>>8&255, i&255)), freshProbe(i))
		}
		svc.probeMu.Lock()
		n := len(svc.probes)
		svc.probeMu.Unlock()
		if n > apiKeyProbeIPsMax {
			t.Fatalf("tracking %d IPs, cap is %d", n, apiKeyProbeIPsMax)
		}
	})
}
