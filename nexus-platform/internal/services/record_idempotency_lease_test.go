package services

// specs/047 FR-13 (DAILY-QUEUE #355): an atomic bulk and an import run in ONE transaction, so their
// Idempotency-Key claim is leased like a single create's (FR-11). The claim carries a base id, the
// rows' ids derive from it, and a claim stuck in processing past the lease is reconciled by probing
// row 0: it exists → the whole batch committed → its response is replayed; it does not → the resend
// takes the claim over and runs the batch. "Stuck" is simulated the way a dead process leaves it: the
// write returns ErrCommitOutcomeUnknown (claim kept) and the claim is then aged past the lease.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/idempotency"
)

var errProcessDied = fmt.Errorf("commit: %w: %w", dynrepo.ErrCommitOutcomeUnknown, errors.New("connection lost"))

// ageClaim moves a claim's creation time back by ago (expires_at is claim time + the 24 h window).
func ageClaim(t *testing.T, idem *fakeIdemStore, tenantID, principalID, key string, ago time.Duration) {
	t.Helper()
	idem.mu.Lock()
	defer idem.mu.Unlock()
	k := idemRowKey(tenantID, principalID, key)
	c, ok := idem.rows[k]
	if !ok {
		t.Fatalf("no claim %s to age", key)
	}
	c.ExpiresAt = time.Now().Add(-ago).Add(idempotencyWindow)
	idem.rows[k] = c
}

// leaseBulkInserter is the atomic inserter over idemRecordStore: ids derive from the base id on ctx
// like dynrepo's; commitThenDie writes the rows and reports the commit as unknown (the process died
// around COMMIT); dieBeforeCommit reports it WITHOUT writing (the transaction never committed).
type leaseBulkInserter struct {
	store           *idemRecordStore
	mu              sync.Mutex
	batches         int
	commitThenDie   bool
	dieBeforeCommit bool
}

func (f *leaseBulkInserter) InsertRecordsAtomic(ctx context.Context, _, tenantID, userID string, rows []map[string]any, _ []dynrepo.UniqueFieldSpec) ([]string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	if f.dieBeforeCommit {
		f.dieBeforeCommit = false
		return nil, -1, errProcessDied
	}
	base := dynrepo.BatchBaseIDFrom(ctx)
	ids := make([]string, len(rows))
	f.store.mu.Lock()
	for i := range rows {
		ids[i] = dynrepo.BatchRowID(base, i)
		if base == "" {
			ids[i] = fmt.Sprintf("atomic-%d-%d", f.batches, i)
		}
		f.store.put(ids[i], tenantID, userID)
	}
	f.store.mu.Unlock()
	if f.commitThenDie {
		f.commitThenDie = false
		return nil, -1, errProcessDied
	}
	return ids, -1, nil
}

func newLeaseBulkService() (*RecordService, *idemRecordStore, *fakeIdemStore, *leaseBulkInserter) {
	svc, store, idem := newIdemService()
	ins := &leaseBulkInserter{store: store}
	svc.WithBulkInserter(ins)
	return svc, store, idem, ins
}

func TestCreateBulkIdempotent_AtomicStaleClaim(t *testing.T) {
	ctx := context.Background()
	rows := func() []map[string]any { return bulkRows("Go 101", "Go 102", "Go 103") }

	t.Run("committed, response lost: past the lease the resend replays the rebuilt response", func(t *testing.T) {
		svc, store, idem, ins := newLeaseBulkService()
		ins.commitThenDie = true
		if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows(), BulkModeAtomic, "b-1"); !errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
			t.Fatalf("first = %v, want the unknown-commit error", err)
		}
		stuck, _ := idem.Get(ctx, "t1", "user-1", "b-1")
		if stuck == nil || stuck.Status != idempotency.StatusProcessing || stuck.RecordID == "" {
			t.Fatalf("claim after the crash = %+v, want processing and holding the batch's base id", stuck)
		}

		// Inside the lease: still the first request's.
		ageClaim(t, idem, "t1", "user-1", "b-1", time.Minute)
		if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows(), BulkModeAtomic, "b-1"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
			t.Fatalf("resend inside the lease = %v, want IDEMPOTENCY_IN_PROGRESS", err)
		}

		ageClaim(t, idem, "t1", "user-1", "b-1", idempotencyBatchLease+time.Second)
		_, replay, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows(), BulkModeAtomic, "b-1")
		if err != nil || replay == nil {
			t.Fatalf("resend past the lease = (replay %s, %v), want the rebuilt response", replay, err)
		}
		var got BulkCreateResult
		if err := json.Unmarshal(replay, &got); err != nil {
			t.Fatalf("replay is not a bulk result: %v (%s)", err, replay)
		}
		if got.Mode != "atomic" || got.Created != 3 || len(got.Results) != 3 {
			t.Fatalf("replay = %+v, want an atomic result with 3 created", got)
		}
		for i, r := range got.Results {
			if want := dynrepo.BatchRowID(stuck.RecordID, i); r.Index != i || r.Status != "created" || r.ID != want {
				t.Fatalf("results[%d] = %+v, want created with id %s", i, r, want)
			}
			if ok, _ := store.RecordExists(ctx, "courses", "t1", r.ID); !ok {
				t.Fatalf("replayed id %s does not exist", r.ID)
			}
		}
		if ins.batches != 1 || store.inserts() != 3 {
			t.Fatalf("batches = %d, rows = %d; the resend wrote the batch again", ins.batches, store.inserts())
		}
		if c, _ := idem.Get(ctx, "t1", "user-1", "b-1"); c == nil || c.Status != idempotency.StatusCompleted {
			t.Fatalf("reconciled claim = %+v, want completed", c)
		}
		// And from now on it is an ordinary completed key.
		if _, replay, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows(), BulkModeAtomic, "b-1"); err != nil || replay == nil {
			t.Fatalf("third call = (replay %s, %v), want the stored replay", replay, err)
		}
	})

	t.Run("never committed: past the lease the resend takes the claim over and writes the batch", func(t *testing.T) {
		svc, store, idem, ins := newLeaseBulkService()
		ins.dieBeforeCommit = true
		if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows(), BulkModeAtomic, "b-1"); !errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
			t.Fatalf("first = %v, want the unknown-commit error", err)
		}
		dead, _ := idem.Get(ctx, "t1", "user-1", "b-1")
		ageClaim(t, idem, "t1", "user-1", "b-1", idempotencyBatchLease+time.Second)

		res, replay, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", rows(), BulkModeAtomic, "b-1")
		if err != nil || replay != nil || res.Created != 3 {
			t.Fatalf("resend = (created %d, replay %s, %v), want the batch written (3 created, no replay)", res.Created, replay, err)
		}
		c, _ := idem.Get(ctx, "t1", "user-1", "b-1")
		if c == nil || c.Status != idempotency.StatusCompleted || len(c.Response) == 0 {
			t.Fatalf("claim after the takeover = %+v, want completed with the response", c)
		}
		if ok, _ := store.RecordExists(ctx, "courses", "t1", dead.RecordID); ok {
			t.Fatal("the dead attempt's base id exists — it had committed")
		}
		if want := dynrepo.BatchRowID(res.Results[0].ID, 0); res.Results[0].ID != want || store.inserts() != 3 || idem.takeovers != 1 {
			t.Fatalf("rows = %d, takeovers = %d, results[0] = %s; want exactly one takeover and one batch of 3", store.inserts(), idem.takeovers, res.Results[0].ID)
		}
	})

	t.Run("an old claim with no base id and a partial bulk keep waiting out the window", func(t *testing.T) {
		svc, store, idem, _ := newLeaseBulkService()
		// A partial bulk dies mid-way: rows are written one by one, no id tells which committed.
		store.insertHook = func() {}
		store.insertErr = errProcessDied
		if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101"), BulkModePartial, "p-1"); err == nil {
			t.Fatal("the partial bulk should have failed")
		}
		store.insertErr = nil
		ageClaim(t, idem, "t1", "user-1", "p-1", 2*idempotencyBatchLease)
		if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101"), BulkModePartial, "p-1"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
			t.Fatalf("stale partial claim = %v, want IDEMPOTENCY_IN_PROGRESS (no lease without one id for the batch)", err)
		}
		// An atomic claim written before FR-13 (processing, no base id) is likewise not reconciled.
		hash := idempotencyRequestHash("bulk\x00"+string(BulkModeAtomic)+"\x00courses", map[string]any{"records": bulkRows("Go 101")})
		_ = idem.Claim(ctx, idempotency.Claim{TenantID: "t1", PrincipalID: "user-1", Key: "old-1", EntityName: "courses", RequestHash: hash, ExpiresAt: time.Now().Add(idempotencyWindow)})
		ageClaim(t, idem, "t1", "user-1", "old-1", 2*idempotencyBatchLease)
		if _, _, err := svc.CreateBulkIdempotent(ctx, idemScope, "t1", "user-1", "courses", bulkRows("Go 101"), BulkModeAtomic, "old-1"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
			t.Fatalf("pre-FR-13 claim = %v, want IDEMPOTENCY_IN_PROGRESS", err)
		}
	})
}

// leaseImportStore is the import store over a set of committed ids: ids derive from the base id on
// ctx; commitThenDie commits then reports an unknown outcome; dieBeforeCommit reports it without
// committing.
type leaseImportStore struct {
	*fakeImportStore
	mu              sync.Mutex
	committed       map[string]bool
	batches         int
	commitThenDie   bool
	dieBeforeCommit bool
}

func (f *leaseImportStore) InsertRecords(ctx context.Context, _, _, _ string, rows []map[string]any, _ []dynrepo.UniqueFieldSpec) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	if f.dieBeforeCommit {
		f.dieBeforeCommit = false
		return nil, errProcessDied
	}
	base := dynrepo.BatchBaseIDFrom(ctx)
	ids := make([]string, len(rows))
	for i := range rows {
		ids[i] = dynrepo.BatchRowID(base, i)
		f.committed[ids[i]] = true
	}
	if f.commitThenDie {
		f.commitThenDie = false
		return nil, errProcessDied
	}
	return ids, nil
}

func (f *leaseImportStore) RecordExists(_ context.Context, _, _, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.committed[id], nil
}

func newLeaseImportService() (*ImportService, *leaseImportStore, *fakeIdemStore) {
	svc, base := newImportSvcForTest()
	store := &leaseImportStore{fakeImportStore: base, committed: map[string]bool{}}
	svc.store = store
	idem := newFakeIdemStore()
	svc.WithIdempotencyStore(idem)
	return svc, store, idem
}

// A CSV with one refused row: the per-row refusals are part of the summary and cannot be re-derived
// from the database once the valid rows are in — they are the plan saved before the transaction.
const importWithRefusal = "col_name\nAlice\n\"\"\nBob\n"

func TestImportIdempotent_StaleClaim(t *testing.T) {
	ctx := context.Background()

	t.Run("committed, response lost: past the lease the resend replays the saved summary", func(t *testing.T) {
		svc, store, idem := newLeaseImportService()
		store.commitThenDie = true
		if _, _, err := svc.ImportIdempotent(ctx, importIn(importWithRefusal), "i-1"); !errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
			t.Fatalf("first = %v, want the unknown-commit error", err)
		}
		stuck, _ := idem.Get(ctx, "t1", "u1", "i-1")
		if stuck == nil || stuck.Status != idempotency.StatusProcessing || stuck.RecordID == "" || len(stuck.Response) == 0 {
			t.Fatalf("claim after the crash = %+v, want processing with a base id and the saved plan", stuck)
		}

		ageClaim(t, idem, "t1", "u1", "i-1", time.Minute)
		if _, _, err := svc.ImportIdempotent(ctx, importIn(importWithRefusal), "i-1"); codeOf(err) != "IDEMPOTENCY_IN_PROGRESS" {
			t.Fatalf("resend inside the lease = %v, want IDEMPOTENCY_IN_PROGRESS", err)
		}

		ageClaim(t, idem, "t1", "u1", "i-1", idempotencyBatchLease+time.Second)
		_, replay, err := svc.ImportIdempotent(ctx, importIn(importWithRefusal), "i-1")
		if err != nil || replay == nil {
			t.Fatalf("resend past the lease = (replay %s, %v), want the saved summary", replay, err)
		}
		var got ImportResult
		if err := json.Unmarshal(replay, &got); err != nil {
			t.Fatalf("replay is not an import summary: %v (%s)", err, replay)
		}
		if got.Total != 3 || got.Inserted != 2 || got.Failed != 1 || len(got.Errors) != 1 || got.Errors[0].Row != 2 {
			t.Fatalf("replay = %+v, want the first run's summary (3 rows, 2 inserted, row 2 refused)", got)
		}
		if store.batches != 1 || len(store.committed) != 2 {
			t.Fatalf("batches = %d, committed rows = %d; the resend wrote the file again", store.batches, len(store.committed))
		}
		if c, _ := idem.Get(ctx, "t1", "u1", "i-1"); c == nil || c.Status != idempotency.StatusCompleted {
			t.Fatalf("reconciled claim = %+v, want completed", c)
		}
	})

	t.Run("never committed: past the lease the resend takes the claim over and imports", func(t *testing.T) {
		svc, store, idem := newLeaseImportService()
		store.dieBeforeCommit = true
		if _, _, err := svc.ImportIdempotent(ctx, importIn(importWithRefusal), "i-1"); !errors.Is(err, dynrepo.ErrCommitOutcomeUnknown) {
			t.Fatalf("first = %v, want the unknown-commit error", err)
		}
		ageClaim(t, idem, "t1", "u1", "i-1", idempotencyBatchLease+time.Second)
		res, replay, err := svc.ImportIdempotent(ctx, importIn(importWithRefusal), "i-1")
		if err != nil || replay != nil || res == nil || res.Inserted != 2 || res.Failed != 1 {
			t.Fatalf("resend = (%+v, replay %s, %v), want the file imported (2 inserted, 1 refused)", res, replay, err)
		}
		if len(store.committed) != 2 || idem.takeovers != 1 {
			t.Fatalf("committed rows = %d, takeovers = %d; want one batch of 2 after exactly one takeover", len(store.committed), idem.takeovers)
		}
		c, _ := idem.Get(ctx, "t1", "u1", "i-1")
		var stored ImportResult
		if c == nil || c.Status != idempotency.StatusCompleted || json.Unmarshal(c.Response, &stored) != nil || stored.Inserted != 2 || len(stored.Errors) != 1 {
			t.Fatalf("claim after the takeover = %+v, want completed with the summary", c)
		}
	})

	t.Run("a plan that cannot be saved writes nothing and releases the key", func(t *testing.T) {
		svc, store, idem := newLeaseImportService()
		idem.planErr = errors.New("db down")
		if _, _, err := svc.ImportIdempotent(ctx, importIn(importWithRefusal), "i-1"); err == nil {
			t.Fatal("the import ran without a saved plan")
		}
		if store.batches != 0 {
			t.Fatalf("batches = %d, want 0: no plan, no write", store.batches)
		}
		if _, gerr := idem.Get(ctx, "t1", "u1", "i-1"); !errors.Is(gerr, idempotency.ErrNotFound) {
			t.Fatalf("claim kept after a refused import (%v) — the retry would be refused", gerr)
		}
	})
}
