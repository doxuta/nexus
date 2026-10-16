package bpm

import (
	"context"
	"errors"
	"testing"
	"time"
)

type gateProcLoader struct {
	entered chan struct{}
	gate    chan struct{}
}

func (g gateProcLoader) ListActiveProcesses(context.Context, string, string, Trigger) ([]Process, error) {
	select {
	case g.entered <- struct{}{}:
	default:
	}
	<-g.gate
	return nil, nil
}

// specs/048 S7b — same three cases as the workflow dispatcher: AsyncEngine.Emit drops on a full
// buffer, EmitWait waits (bounded by ctx) and errors so the import job retries.
func TestBPMAsyncEngine_EmitWait_Backpressure(t *testing.T) {
	g := gateProcLoader{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	store := newFakeStore()
	e := NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, g, store, store, nil)
	evt := Event{TenantID: "t1", EntityName: "leads", RecordID: "r1", Trigger: TriggerAfterCreate}

	e.Emit(context.Background(), evt)
	<-g.entered
	e.Emit(context.Background(), evt)

	t.Run("full buffer waits until the deadline then errors", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		start := time.Now()
		if err := e.EmitWait(ctx, evt); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want DeadlineExceeded", err)
		}
		if time.Since(start) < 30*time.Millisecond {
			t.Fatal("returned early: it dropped instead of waiting")
		}
	})
	t.Run("a freed slot lets the waiting send through", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { done <- e.EmitWait(context.Background(), evt) }()
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
			t.Fatal("still blocked")
		}
	})
	t.Run("after Shutdown errors, no panic", func(t *testing.T) {
		if err := e.Shutdown(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := e.EmitWait(context.Background(), evt); err == nil {
			t.Fatal("want an error after Shutdown")
		}
	})
}
