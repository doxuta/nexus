package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore is an in-memory Store: a queue of claimable jobs plus a record of every transition,
// guarded by a mutex so the -race worker-pool test is meaningful.
type fakeStore struct {
	mu     sync.Mutex
	queue  []*Job // jobs handed out by ClaimNext in order
	closed bool

	claimErr error

	done        []string // job ids marked done
	failed      map[string]string
	rescheduled map[string]time.Time
	postponed   map[string]time.Time // Postpone calls: job id -> run_at
	applied     bool                 // what MarkDone/Reschedule/MarkFailed report (lost-claim simulation)
	reaps       int
	claimLanes  map[string]Lane // job id → the lane of the worker that claimed it
}

// laneTakes mirrors the SQL store's lane predicate.
func laneTakes(lane Lane, jobType string) bool {
	if lane.Only != "" {
		return jobType == lane.Only
	}
	for _, t := range lane.Exclude {
		if t == jobType {
			return false
		}
	}
	return true
}

func newFakeStore(jobs ...*Job) *fakeStore {
	return &fakeStore{
		queue:       jobs,
		failed:      map[string]string{},
		rescheduled: map[string]time.Time{},
		postponed:   map[string]time.Time{},
		applied:     true,
	}
}

func (f *fakeStore) ClaimNext(_ context.Context, workerID string, _ time.Time, lane Lane) (*Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claimErr != nil {
		return nil, f.claimErr
	}
	// The oldest job the lane may take, as the SQL store's WHERE does.
	i := 0
	for ; i < len(f.queue); i++ {
		if laneTakes(lane, f.queue[i].Type) {
			break
		}
	}
	if i == len(f.queue) {
		return nil, nil
	}
	j := f.queue[i]
	f.queue = append(f.queue[:i:i], f.queue[i+1:]...)
	if f.claimLanes == nil {
		f.claimLanes = map[string]Lane{}
	}
	f.claimLanes[j.ID] = lane
	j.Attempts++ // mirror the SQL store's claim-time increment
	j.Status = StatusRunning
	j.ClaimedBy = workerID
	cp := *j
	return &cp, nil
}

func (f *fakeStore) MarkDone(_ context.Context, id, _ string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = append(f.done, id)
	return f.applied, nil
}

func (f *fakeStore) Reschedule(_ context.Context, id, _ string, runAt time.Time, lastError string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled[id] = runAt
	f.failed[id] = lastError // reuse for error-text assertions
	return f.applied, nil
}

func (f *fakeStore) Postpone(_ context.Context, id, _ string, runAt time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postponed[id] = runAt
	return f.applied, nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id, _, lastError string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed[id] = lastError
	return f.applied, nil
}

func (f *fakeStore) ReapStuck(_ context.Context, _ time.Time) (int64, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reaps++
	return 0, 0, nil
}

// testManager builds an unstarted manager over the fake store with a frozen clock and tiny
// backoff so assertions are deterministic.
func testManager(store Store, reg *Registry, now time.Time) *Manager {
	m := NewManager(ManagerConfig{Workers: 1, PollInterval: time.Second, Lease: time.Minute}, store, reg, nil)
	m.backoffBase = 10 * time.Millisecond
	m.backoffCap = 80 * time.Millisecond
	m.now = func() time.Time { return now }
	return m
}

// TestRunJobSuccess: a claimed job whose handler returns nil is marked done.
func TestRunJobSuccess(t *testing.T) {
	store := newFakeStore()
	reg := NewRegistry()
	var got Job
	_ = reg.Register("t", func(_ context.Context, j Job) error { got = j; return nil })
	m := testManager(store, reg, time.Now().UTC())

	m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 1, MaxAttempts: 5, TenantID: "ten-1"})

	if len(store.done) != 1 || store.done[0] != "j1" {
		t.Fatalf("done = %v, want [j1]", store.done)
	}
	if got.TenantID != "ten-1" {
		t.Fatalf("handler received tenant %q, want ten-1", got.TenantID)
	}
}

// TestRunJobRetriesWithBackoff: a failing handler under budget reschedules at now + Backoff(attempts).
func TestRunJobRetriesWithBackoff(t *testing.T) {
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	store := newFakeStore()
	reg := NewRegistry()
	_ = reg.Register("t", func(context.Context, Job) error { return errors.New("smtp down") })
	m := testManager(store, reg, now)

	// Attempt 2 of 5 → reschedule at now + base*2 (Backoff for attempts=2).
	m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 2, MaxAttempts: 5})

	runAt, ok := store.rescheduled["j1"]
	if !ok {
		t.Fatalf("job not rescheduled; failed=%v done=%v", store.failed, store.done)
	}
	want := now.Add(Backoff(m.backoffBase, m.backoffCap, 2))
	if !runAt.Equal(want) {
		t.Fatalf("rescheduled run_at = %s, want %s", runAt, want)
	}
	if store.failed["j1"] != "smtp down" {
		t.Fatalf("last_error = %q, want the handler error", store.failed["j1"])
	}
}

// TestRunJobFailsAtMaxAttempts: budget exhausted → terminal failure, no reschedule.
func TestRunJobFailsAtMaxAttempts(t *testing.T) {
	store := newFakeStore()
	reg := NewRegistry()
	_ = reg.Register("t", func(context.Context, Job) error { return errors.New("still broken") })
	m := testManager(store, reg, time.Now().UTC())

	m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 5, MaxAttempts: 5})

	if _, ok := store.rescheduled["j1"]; ok {
		t.Fatal("job at max attempts must not be rescheduled")
	}
	if store.failed["j1"] != "still broken" {
		t.Fatalf("failed error = %q, want handler error", store.failed["j1"])
	}
}

// TestRunJobPanicIsAFailedAttempt: a panicking handler never kills the worker — the panic is
// converted to an error and the job retries like any failure.
func TestRunJobPanicIsAFailedAttempt(t *testing.T) {
	store := newFakeStore()
	reg := NewRegistry()
	_ = reg.Register("t", func(context.Context, Job) error { panic("boom") })
	m := testManager(store, reg, time.Now().UTC())

	m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 1, MaxAttempts: 5}) // must not panic the test

	if _, ok := store.rescheduled["j1"]; !ok {
		t.Fatalf("panicked job not rescheduled; failed=%v", store.failed)
	}
}

// TestRunJobUnknownTypeFailsImmediately: an unregistered job_type is a wiring bug — terminal
// failure on the first claim, never an infinite retry.
func TestRunJobUnknownTypeFailsImmediately(t *testing.T) {
	store := newFakeStore()
	m := testManager(store, NewRegistry(), time.Now().UTC())

	m.runJob("w0", &Job{ID: "j1", Type: "ghost", Attempts: 1, MaxAttempts: 5})

	if _, ok := store.rescheduled["j1"]; ok {
		t.Fatal("unregistered type must not be retried")
	}
	if msg := store.failed["j1"]; msg == "" {
		t.Fatal("unregistered type must be marked failed with a clear last_error")
	}
}

// TestRunJobLostClaimIsDropped: applied=false from the store (job reaped meanwhile) must not
// error or panic — the other execution owns the row.
func TestRunJobLostClaimIsDropped(t *testing.T) {
	store := newFakeStore()
	store.applied = false
	reg := NewRegistry()
	_ = reg.Register("t", func(context.Context, Job) error { return nil })
	m := testManager(store, reg, time.Now().UTC())

	m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 1, MaxAttempts: 5})
	// Reaching here without panic is the assertion; the transition was still attempted:
	if len(store.done) != 1 {
		t.Fatalf("MarkDone attempts = %d, want 1", len(store.done))
	}
}

// TestManagerPoolDrainsQueue runs a real Start/Stop cycle with 4 workers over 50 jobs under
// -race: every job is processed exactly once and Stop drains gracefully.
func TestManagerPoolDrainsQueue(t *testing.T) {
	jobs := make([]*Job, 50)
	for i := range jobs {
		jobs[i] = &Job{ID: fmt.Sprintf("j%d", i), Type: "t", MaxAttempts: 5}
	}
	store := newFakeStore(jobs...)
	reg := NewRegistry()
	var mu sync.Mutex
	seen := map[string]int{}
	_ = reg.Register("t", func(_ context.Context, j Job) error {
		mu.Lock()
		seen[j.ID]++
		mu.Unlock()
		return nil
	})
	m := NewManager(ManagerConfig{Workers: 4, PollInterval: time.Second, Lease: time.Minute}, store, reg, nil)
	m.Start()
	m.Start() // idempotent

	deadline := time.Now().Add(5 * time.Second)
	for {
		store.mu.Lock()
		remaining := len(store.queue)
		doneCount := len(store.done)
		store.mu.Unlock()
		if remaining == 0 && doneCount == len(jobs) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pool did not drain: %d queued, %d done", remaining, doneCount)
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := m.Stop(ctx); err != nil { // idempotent
		t.Fatalf("second Stop: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != len(jobs) {
		t.Fatalf("processed %d distinct jobs, want %d", len(seen), len(jobs))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s processed %d times, want exactly 1", id, n)
		}
	}
}

// TestManagerStopInterruptsIdleSleep: Stop must not wait a full poll interval on idle workers.
func TestManagerStopInterruptsIdleSleep(t *testing.T) {
	store := newFakeStore() // empty queue → workers sleep on poll
	reg := NewRegistry()
	m := NewManager(ManagerConfig{Workers: 2, PollInterval: time.Hour, Lease: time.Minute}, store, reg, nil)
	m.Start()
	time.Sleep(20 * time.Millisecond) // let workers reach the idle sleep

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop blocked on idle sleep: %v", err)
	}
}

// TestBuildWorkerPrefix_ClampsLongHost pins the HIGH fix: a k8s-length (63-char) hostname must
// NOT truncate the distinguishing "-<pid>-w<N>" suffix away — the full worker id stays ≤64 (the
// claimed_by column width) and two workers on the same host keep DISTINCT ids, so the lost-lease
// guard (`WHERE claimed_by = ?`) can't collapse.
func TestBuildWorkerPrefix_ClampsLongHost(t *testing.T) {
	longHost := strings.Repeat("a", 63) // a valid k8s pod DNS label
	prefix := buildWorkerPrefix(longHost, 4194303)
	for _, n := range []int{0, 1, 99} {
		id := fmt.Sprintf("%s-w%d", prefix, n)
		if len(id) > maxWorkerIDLen {
			t.Fatalf("worker id %q len %d > %d (claimed_by would truncate the identity suffix)", id, len(id), maxWorkerIDLen)
		}
	}
	// Empty host falls back, and two workers differ.
	p0 := buildWorkerPrefix("", 1)
	if p0 == "" {
		t.Fatal("empty host must fall back to a non-empty prefix")
	}
	w0, w1 := p0+"-w0", p0+"-w1"
	if w0 == w1 {
		t.Fatalf("workers on same host collapsed to one claimed_by: %q", w0)
	}
}

// TestRunJobPermanentErrorFailsImmediately: a handler that KNOWS retrying cannot help (an outbound
// webhook answered 4xx, the rule it points at was deleted) wraps its error in Permanent — the job
// is failed on the FIRST attempt with budget left, never rescheduled, and last_error keeps the
// handler's own text.
func TestRunJobPermanentErrorFailsImmediately(t *testing.T) {
	store := newFakeStore()
	reg := NewRegistry()
	_ = reg.Register("t", func(context.Context, Job) error {
		return fmt.Errorf("services.handler: %w", Permanent(errors.New("status 400")))
	})
	m := testManager(store, reg, time.Now().UTC())

	m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 1, MaxAttempts: 10})

	if _, ok := store.rescheduled["j1"]; ok {
		t.Fatal("a permanent error must not be rescheduled")
	}
	if got := store.failed["j1"]; got != "services.handler: status 400" {
		t.Fatalf("last_error = %q, want the handler's text unchanged", got)
	}
}

// TestPermanent: the marker survives %w wrapping, keeps the cause reachable, and never turns a
// nil into an error.
func TestPermanent(t *testing.T) {
	cause := errors.New("status 410")
	wrapped := fmt.Errorf("outer: %w", Permanent(cause))
	if !IsPermanent(wrapped) {
		t.Fatal("IsPermanent lost the marker through %w")
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("Permanent broke the error chain")
	}
	if IsPermanent(cause) || IsPermanent(nil) {
		t.Fatal("a plain error / nil must not read as permanent")
	}
	if Permanent(nil) != nil {
		t.Fatal("Permanent(nil) must stay nil")
	}
}

// TestManagerDedicatedLane_SlowTypeNeverDelaysOthers (review of #26): a job type whose attempts
// can hold a worker for a long time (outbound_webhook against a receiver that swallows packets —
// one full HTTP timeout per attempt) gets workers of its own. With every dedicated worker busy on
// it, an email/timer job still runs at once on the shared pool, and the shared pool never claims
// the dedicated type.
func TestManagerDedicatedLane_SlowTypeNeverDelaysOthers(t *testing.T) {
	release := make(chan struct{})
	store := newFakeStore(
		&Job{ID: "hook-1", Type: "slow", MaxAttempts: 5},
		&Job{ID: "hook-2", Type: "slow", MaxAttempts: 5},
		&Job{ID: "mail-1", Type: "fast", MaxAttempts: 5},
	)
	reg := NewRegistry()
	_ = reg.Register("slow", func(context.Context, Job) error { <-release; return nil })
	_ = reg.Register("fast", func(context.Context, Job) error { return nil })
	m := NewManager(ManagerConfig{Workers: 1, PollInterval: time.Second, Lease: time.Minute,
		Dedicated: map[string]int{"slow": 1}}, store, reg, nil)
	m.Start()
	defer func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.Stop(ctx); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}()

	// Wait for BOTH: the shared worker finishing mail-1 AND the dedicated worker having claimed
	// hook-1. The two lanes race, so mail-1 can be done before the dedicated goroutine's first
	// claim — asserting on hook-1 then reads an empty lane (the review-found flake).
	deadline := time.Now().Add(2 * time.Second)
	for {
		store.mu.Lock()
		done := append([]string(nil), store.done...)
		_, hookClaimed := store.claimLanes["hook-1"]
		store.mu.Unlock()
		if len(done) == 1 && done[0] == "mail-1" && hookClaimed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("done = %v, hook-1 claimed = %v while the slow jobs hold their workers; the fast job must not wait behind them", done, hookClaimed)
		}
		time.Sleep(5 * time.Millisecond)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if lane := store.claimLanes["hook-1"]; lane.Only != "slow" {
		t.Fatalf("hook-1 claimed by lane %+v, want the dedicated slow lane", lane)
	}
	if _, claimed := store.claimLanes["hook-2"]; claimed {
		t.Fatalf("hook-2 was claimed while the only slow worker is busy — the shared pool took a dedicated type")
	}
	if lane := store.claimLanes["mail-1"]; lane.Only != "" || len(lane.Exclude) != 1 || lane.Exclude[0] != "slow" {
		t.Fatalf("mail-1 claimed by lane %+v, want the shared lane excluding slow", lane)
	}
}

// specs/068 T036: a job of a write-frozen tenant is POSTPONED — the handler never runs, the attempt
// the claim just burnt is given back (Postpone, not Reschedule), run_at = the freeze deadline plus a
// jitter of at most 30 s. A tenant that is not frozen, or a job without a tenant, runs as before.
func TestRunJobFrozenTenantIsPostponed(t *testing.T) {
	now := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	until := now.Add(5 * time.Minute)
	gate := func(_ context.Context, tenantID string) (time.Time, bool) {
		if tenantID == "frozen" {
			return until, true
		}
		return time.Time{}, false
	}
	tests := []struct {
		name     string
		tenant   string
		wantPost bool
	}{
		{"frozen tenant", "frozen", true},
		{"other tenant", "free", false},
		{"system job without tenant", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			reg := NewRegistry()
			ran := 0
			_ = reg.Register("t", func(context.Context, Job) error { ran++; return nil })
			m := testManager(store, reg, now).WithTenantGate(gate)

			m.runJob("w0", &Job{ID: "j1", Type: "t", Attempts: 1, MaxAttempts: 5, TenantID: tc.tenant})

			runAt, posted := store.postponed["j1"]
			if posted != tc.wantPost {
				t.Fatalf("postponed = %v, want %v", posted, tc.wantPost)
			}
			if tc.wantPost {
				if ran != 0 || len(store.done) != 0 || len(store.rescheduled) != 0 {
					t.Fatalf("a postponed job must not run/finish/reschedule: ran=%d done=%v resched=%v", ran, store.done, store.rescheduled)
				}
				if d := runAt.Sub(until); d < 0 || d > 30*time.Second {
					t.Fatalf("run_at = until+%s, want within [0,30s]", d)
				}
			} else if ran != 1 || len(store.done) != 1 {
				t.Fatalf("free job must run: ran=%d done=%v", ran, store.done)
			}
		})
	}
}
