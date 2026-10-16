package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// gateLoader holds the worker inside ListActive until released, so the buffer can be filled.
type gateLoader struct {
	entered chan struct{}
	gate    chan struct{}
}

func (g gateLoader) ListActive(context.Context, string, string, Trigger) ([]Workflow, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.gate
	return nil, nil
}

// specs/048 S7b — EmitWait never drops: a full buffer waits up to the caller's deadline, then errors
// so the import job retries; after Shutdown it errors instead of panicking.
func TestAsyncDispatcher_EmitWait_Backpressure(t *testing.T) {
	g := gateLoader{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, g, nil, nil, nil)
	evt := Event{TenantID: "t1", EntityName: "leads", RecordID: "r1", Trigger: TriggerAfterCreate}

	d.Emit(context.Background(), evt) // worker takes it and blocks in the loader
	<-g.entered
	d.Emit(context.Background(), evt) // fills the 1-slot buffer

	t.Run("full buffer waits until the deadline then errors", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := d.EmitWait(ctx, evt)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}
		if time.Since(start) < 30*time.Millisecond {
			t.Fatalf("returned after %v: it dropped instead of waiting", time.Since(start))
		}
	})
	t.Run("a freed slot lets the waiting send through", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { done <- d.EmitWait(context.Background(), evt) }()
		select {
		case err := <-done:
			t.Fatalf("returned before a slot was free: %v", err)
		case <-time.After(30 * time.Millisecond):
		}
		close(g.gate)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("err = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("still blocked after the worker was released")
		}
	})
	t.Run("after Shutdown errors, no panic", func(t *testing.T) {
		if err := d.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := d.EmitWait(context.Background(), evt); err == nil {
			t.Fatal("want an error after Shutdown")
		}
	})
}
