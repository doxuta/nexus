package tenantbackup

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"nexus/platform/internal/audit"
)

// Only one job runs at a time; the second waits for the semaphore and keeps proving it is alive.
func TestRunner_OneAtATimeAndWaitingBackupBeats(t *testing.T) {
	r := NewRunner(nil)
	r.beat = 10 * time.Millisecond
	release := make(chan struct{})
	var running, maxRunning, beats int32
	started := make(chan struct{}, 2)
	job := func(ctx context.Context) {
		n := atomic.AddInt32(&running, 1)
		for {
			m := atomic.LoadInt32(&maxRunning)
			if n <= m || atomic.CompareAndSwapInt32(&maxRunning, m, n) {
				break
			}
		}
		started <- struct{}{}
		<-release
		atomic.AddInt32(&running, -1)
	}
	beat := func(context.Context) bool { atomic.AddInt32(&beats, 1); return true }

	if err := r.Go("u1", beat, job); err != nil {
		t.Fatal(err)
	}
	<-started // the first holds the semaphore
	if err := r.Go("u2", beat, job); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&beats) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if atomic.LoadInt32(&beats) < 2 {
		t.Fatal("a backup waiting for its turn must keep beating, or the reaper fails it as abandoned")
	}
	if atomic.LoadInt32(&running) != 1 {
		t.Fatalf("running = %d, want exactly 1", running)
	}
	close(release)
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if maxRunning != 1 {
		t.Errorf("maxRunning = %d, want 1", maxRunning)
	}
}

// A waiting job whose row vanished is dropped without ever running.
func TestRunner_BeatReportingGoneAbandonsTheJob(t *testing.T) {
	r := NewRunner(nil)
	r.beat = 5 * time.Millisecond
	hold := make(chan struct{})
	started := make(chan struct{})
	_ = r.Go("u1", func(context.Context) bool { return true }, func(context.Context) { close(started); <-hold })
	<-started
	var ran int32
	_ = r.Go("u2", func(context.Context) bool { return false }, func(context.Context) { atomic.AddInt32(&ran, 1) })
	time.Sleep(60 * time.Millisecond)
	close(hold)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Error("an abandoned job must not run")
	}
}

// The job's context carries the actor (audit) and is cancelled by Stop; Go after Stop refuses.
func TestRunner_ActorInContextStopCancelsAndRefusesNewWork(t *testing.T) {
	r := NewRunner(nil)
	got := make(chan string, 1)
	cancelled := make(chan struct{})
	_ = r.Go("operator-7", func(context.Context) bool { return true }, func(ctx context.Context) {
		got <- audit.ActorFromContext(ctx)
		<-ctx.Done()
		close(cancelled)
	})
	if a := <-got; a != "operator-7" {
		t.Errorf("actor in job context = %q", a)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Error("Stop must cancel the running job's context")
	}
	if err := r.Go("u", func(context.Context) bool { return true }, func(context.Context) {}); !errors.Is(err, ErrStopping) {
		t.Errorf("Go after Stop = %v, want ErrStopping", err)
	}
}

// A job that panics must not kill the server nor wedge the semaphore.
func TestRunner_PanicDoesNotWedgeTheSemaphore(t *testing.T) {
	r := NewRunner(nil)
	_ = r.Go("u", func(context.Context) bool { return true }, func(context.Context) { panic("boom") })
	done := make(chan struct{})
	_ = r.Go("u", func(context.Context) bool { return true }, func(context.Context) { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the job after a panicking one never ran: the semaphore leaked")
	}
}
