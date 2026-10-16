//go:build integration

package emaillog

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// specs/066 L2 (T052, repository half): Get is tenant-pinned and carries the action hash; LockResend
// is a one-shot CAS that only a failed, not-yet-resent row of THIS tenant can take, under real
// concurrency; ReleaseResend gives the handle back.
func TestEmailLogGetAndResendLock_Integration(t *testing.T) {
	_, r := elogDB(t)
	ctx := context.Background()
	hash := strings.Repeat("a", 64)
	idx := 1
	e := Entry{ID: "rs-1", TenantID: tenA, Source: SourceWorkflow, Status: StatusFailed, RecipientMasked: "a***@gmail.com",
		SubjectTemplate: "Chào", WorkflowID: "wf-1", ActionIndex: &idx, ActionHash: hash, EntityName: "crm_contacts", RecordID: "rec-1"}
	if err := r.Insert(ctx, e); err != nil {
		t.Fatal(err)
	}

	got, err := r.Get(ctx, tenA, "rs-1")
	if err != nil || got.ID != "rs-1" || got.ActionHash == nil || *got.ActionHash != hash || got.ActionIndex == nil || *got.ActionIndex != 1 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := r.Get(ctx, tenB, "rs-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant's Get err = %v; want ErrNotFound (same as a ghost id)", err)
	}
	if _, err := r.Get(ctx, tenA, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ghost Get err = %v; want ErrNotFound", err)
	}

	// Another tenant cannot take the lock; a sent or queued row cannot be locked at all.
	if ok, err := r.LockResend(ctx, tenB, "rs-1", time.Now()); err != nil || ok {
		t.Fatalf("tenant B lock = %v, %v; want false", ok, err)
	}
	seed(t, r, tenA, "rs-sent", StatusSent)
	seed(t, r, tenA, "rs-queued", StatusQueued)
	for _, id := range []string{"rs-sent", "rs-queued"} {
		if ok, err := r.LockResend(ctx, tenA, id, time.Now()); err != nil || ok {
			t.Fatalf("lock of %s = %v, %v; want false", id, ok, err)
		}
	}

	// 20 rounds of two racers on a fresh row: exactly one wins.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("rs-race-%02d", i)
		seed(t, r, tenA, id, StatusFailed)
		results := make(chan bool, 2)
		for j := 0; j < 2; j++ {
			go func() {
				ok, err := r.LockResend(ctx, tenA, id, time.Now())
				if err != nil {
					t.Errorf("lock: %v", err)
				}
				results <- ok
			}()
		}
		wins := 0
		for j := 0; j < 2; j++ {
			if <-results {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("round %d: %d winners; want exactly 1", i, wins)
		}
	}

	// A sequential second press loses; the handle comes back after Release (and only for the owner tenant).
	if ok, _ := r.LockResend(ctx, tenA, "rs-1", time.Now()); !ok {
		t.Fatal("first lock lost")
	}
	if ok, _ := r.LockResend(ctx, tenA, "rs-1", time.Now()); ok {
		t.Fatal("second lock won")
	}
	if err := r.ReleaseResend(ctx, tenB, "rs-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, tenA, "rs-1"); got.ResentAt == nil {
		t.Fatal("tenant B released tenant A's lock")
	}
	if err := r.ReleaseResend(ctx, tenA, "rs-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Get(ctx, tenA, "rs-1"); got.ResentAt != nil {
		t.Fatal("ReleaseResend left resent_at set")
	}
}
