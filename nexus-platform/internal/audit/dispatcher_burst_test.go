package audit

// A mass update is the one place where losing audit rows actually costs something: nulling a field
// across 500 records has no undo, and the before-images in the trail are the only way to
// reconstruct what was there. Those two numbers lived in different files and drifted — the buffer
// took the alert dispatcher's default (256) while MAX_BULK_OPS allows 500 rows per call — so the
// product's own documented limit could overrun its own trail, silently.
//
// These tests pin the coupling and, just as importantly, pin that a drop is COUNTABLE. Fail-open is
// still the right behaviour; a drop nobody can measure is not.

import (
	"context"
	"sync"
	"testing"
	"time"
)

// countingBlockingWriter holds every write until released (like the package's blockingWriter) but
// also counts what it eventually got, so a test can assert the graceful drain wrote EVERY buffered
// event rather than merely not dropping them at enqueue time.
type countingBlockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	seen    int
}

func (w *countingBlockingWriter) InsertEvent(_ context.Context, _ Event) error {
	<-w.release
	w.mu.Lock()
	w.seen++
	w.mu.Unlock()
	return nil
}

func TestBufferFor_NeverSmallerThanTheLargestAllowedBurst(t *testing.T) {
	if got := BufferFor(256, 500); got != 500 {
		t.Errorf("BufferFor(256, 500) = %d, want 500 — the shipped default was smaller than one bulk call", got)
	}
	if got := BufferFor(2048, 500); got != 2048 {
		t.Errorf("BufferFor(2048, 500) = %d, want the operator's larger value kept", got)
	}
	if got := BufferFor(0, 0); got != 0 {
		t.Errorf("BufferFor(0,0) = %d — the constructor clamps, this helper must not invent a size", got)
	}
	// Two bulk doors, two independently settable caps. Flooring on only the currently-larger one
	// re-opens the gap the moment the OTHER is raised — which is the same two-numbers-in-two-files
	// drift this function exists to end.
	if got := BufferFor(256, 500, 2000); got != 2000 {
		t.Errorf("BufferFor(256, 500, 2000) = %d, want 2000 — the second bulk door was ignored", got)
	}
	if got := BufferFor(256, 500, 100); got != 500 {
		t.Errorf("BufferFor(256, 500, 100) = %d, want 500 — a smaller second cap must not lower the floor", got)
	}
}

// TestBurstOfMaxBulkOps_DropsNothing is the regression this whole change exists for.
func TestBurstOfMaxBulkOps_DropsNothing(t *testing.T) {
	const largestBurst = 500
	w := &countingBlockingWriter{release: make(chan struct{})}
	d := NewAsyncDispatcher(DispatcherConfig{
		Buffer:  BufferFor(256, largestBurst),
		Workers: 2,
	}, w, nil)

	for i := 0; i < largestBurst; i++ {
		d.Record(context.Background(), Event{Action: ActionUpdate, TenantID: "t1", EntityName: "crm_leads"})
	}
	if n := d.Dropped(); n != 0 {
		t.Fatalf("dropped %d of %d events in ONE allowed bulk burst — before-images are gone with no undo", n, largestBurst)
	}

	close(w.release)
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	w.mu.Lock()
	seen := w.seen
	w.mu.Unlock()
	if seen != largestBurst {
		t.Errorf("writer saw %d events, want all %d after a graceful drain", seen, largestBurst)
	}
}

// TestUndersizedBufferStillFailsOpen_ButCounts: the old sizing must not become a hidden crash or a
// block. It still drops (fail-open is deliberate — audit must never stall a mutation), but the
// number is now readable instead of buried in per-event warn lines.
func TestUndersizedBufferStillFailsOpen_ButCounts(t *testing.T) {
	w := &countingBlockingWriter{release: make(chan struct{})}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 1}, w, nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			d.Record(context.Background(), Event{Action: ActionUpdate, TenantID: "t1"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Record blocked on a full buffer — fail-open is the contract; a mutation must never wait on the trail")
	}

	if d.Dropped() == 0 {
		t.Error("an undersized buffer absorbed 200 events with 1 blocked worker — the counter is not wired")
	}
	close(w.release)
	_ = d.Shutdown(context.Background())
}
