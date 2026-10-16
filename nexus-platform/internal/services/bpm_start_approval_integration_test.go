//go:build integration

// specs/043 S9b — `bpm-start-approval-action`: a repeated send makes ONE hồ sơ, proven against real
// MySQL (uq_ai_idem from Migration B):
//
//   - two CreateInstance calls with one (tenant, key) — the loser is ErrIdempotencyKeyConflict, never
//     read as "already waiting" (the other unique key) and never a 500;
//   - the key is tenant-scoped;
//   - N simultaneous sends carrying one Idempotency-Key all answer with the SAME hồ sơ.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration ./internal/services/ -run 'TestIdempotency'
package services

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/bpm"
)

type oneManualProcess struct{ p bpm.Process }

func (l oneManualProcess) ListActiveProcesses(context.Context, string, string, bpm.Trigger) ([]bpm.Process, error) {
	return []bpm.Process{l.p}, nil
}

func TestIdempotencyKey_CreateInstanceConflictAndTenantScope(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	key := "k-" + uuid.NewString()

	in := openKeyInput(o3aTenantA, "quotes", uuid.NewString())
	in.IdempotencyKey, in.RequestHash = key, "h1"
	id, err := st.repo.CreateInstance(ctx, in)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	other := openKeyInput(o3aTenantA, "quotes", uuid.NewString())
	other.IdempotencyKey, other.RequestHash = key, "h2"
	if _, err := st.repo.CreateInstance(ctx, other); !errors.Is(err, bpm.ErrIdempotencyKeyConflict) || errors.Is(err, bpm.ErrAlreadyPending) {
		t.Fatalf("second with the same key: %v; want ErrIdempotencyKeyConflict (and not ErrAlreadyPending)", err)
	}
	got, hash, err := st.repo.FindByIdempotencyKey(ctx, o3aTenantA, key)
	if err != nil || got.ID != id || hash != "h1" {
		t.Fatalf("find = %v %q %v; want the first instance and its hash", got, hash, err)
	}
	if _, _, err := st.repo.FindByIdempotencyKey(ctx, o3aTenantB, key); !errors.Is(err, bpm.ErrInstanceNotFound) {
		t.Fatalf("find in another tenant: %v; want ErrInstanceNotFound", err)
	}
	b := openKeyInput(o3aTenantB, "quotes", uuid.NewString())
	b.IdempotencyKey, b.RequestHash = key, "h1"
	if _, err := st.repo.CreateInstance(ctx, b); err != nil {
		t.Fatalf("the same key in another tenant: %v; want allowed", err)
	}
}

func TestIdempotencyKey_ConcurrentSendsAnswerWithOneInstance(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	rec, key := uuid.NewString(), "k-"+uuid.NewString()
	proc := bpm.Process{ID: uuid.NewString(), TenantID: o3aTenantA, EntityName: "quotes", Trigger: bpm.TriggerManual,
		Steps: []bpm.Step{{Name: "Duyệt", ApproverType: bpm.ApproverUser, ApproverID: uuid.NewString()}}}
	req := bpm.NewRequester(oneManualProcess{proc}, st.repo, st.repo, st.repo, nil)

	const racers = 8
	var wg sync.WaitGroup
	ids, errs := make([]string, racers), make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			inst, err := req.SubmitWith(ctx, o3aTenantA, "quotes", rec, nil, bpm.Actor{UserID: uuid.NewString()[:36]}, bpm.SubmitOptions{IdempotencyKey: key})
			if err == nil {
				ids[i] = inst.ID
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	// Each racer used a different sender, so only the winner's hash matches: the rest are told the key
	// is taken (422), never given the winner's hồ sơ. Same-sender racers are covered below.
	var winner string
	for i := range ids {
		if errs[i] == nil {
			if winner != "" {
				t.Fatalf("two different senders both got a hồ sơ on one key")
			}
			winner = ids[i]
		} else if !errors.Is(errs[i], bpm.ErrIdempotencyKeyReused) && !errors.Is(errs[i], bpm.ErrAlreadyPending) {
			t.Fatalf("racer %d: %v; want success, key reused or already pending", i, errs[i])
		}
	}
	if winner == "" {
		t.Fatal("nobody won")
	}

	rec2, key2, sender := uuid.NewString(), "k-"+uuid.NewString(), uuid.NewString()
	ids2, errs2 := make([]string, racers), make([]error, racers)
	start2 := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start2
			inst, err := req.SubmitWith(ctx, o3aTenantA, "quotes", rec2, nil, bpm.Actor{UserID: sender}, bpm.SubmitOptions{IdempotencyKey: key2})
			if err == nil {
				ids2[i] = inst.ID
			}
			errs2[i] = err
		}(i)
	}
	close(start2)
	wg.Wait()
	for i := range ids2 {
		if errs2[i] != nil || ids2[i] != ids2[0] {
			t.Fatalf("racer %d: id=%q err=%v; want every retry to answer with the same hồ sơ %q", i, ids2[i], errs2[i], ids2[0])
		}
	}
	var n int
	if err := st.sdb.Get(&n, `SELECT COUNT(*) FROM sys_approval_instances WHERE tenant_id = ? AND record_id = ?`, o3aTenantA, rec2); err != nil || n != 1 {
		t.Fatalf("rows for the record = %d (%v); want 1", n, err)
	}
}
