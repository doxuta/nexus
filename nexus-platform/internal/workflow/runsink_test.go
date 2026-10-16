package workflow

// specs/048 S3 (T060) — RunSink writes the runs of the synchronous before rules off the save's path:
// a full buffer drops the run (never makes the save wait), Close drains what is buffered.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// gateRecorder records every run; with gate set, each InsertRun first waits for the gate to close.
type gateRecorder struct {
	mu      sync.Mutex
	runs    []RunInput
	gate    chan struct{}
	entered chan struct{}
}

func (r *gateRecorder) InsertRun(_ context.Context, in RunInput) error {
	if r.entered != nil {
		select {
		case r.entered <- struct{}{}:
		default:
		}
	}
	if r.gate != nil {
		<-r.gate
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, in)
	return nil
}

func (r *gateRecorder) written() []RunInput {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunInput(nil), r.runs...)
}

func TestRunSink_FullBufferNeverBlocks(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	rec := &gateRecorder{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	sink := NewRunSink(rec, zap.New(core))

	sink.Record(RunInput{WorkflowID: "held"})
	<-rec.entered // the worker now holds "held"; the buffer is empty
	for i := 0; i < runSinkBuffer; i++ {
		sink.Record(RunInput{WorkflowID: fmt.Sprint("buffered-", i)})
	}
	done := make(chan struct{})
	go func() {
		sink.Record(RunInput{TenantID: "t1", WorkflowID: "overflow"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked on a full buffer — every save would wait on the run history")
	}
	if n := logs.FilterMessageSnippet("dropped").Len(); n != 1 {
		t.Fatalf("drop warnings = %d, want 1", n)
	}

	close(rec.gate)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	runs := rec.written()
	if len(runs) != 1+runSinkBuffer {
		t.Fatalf("runs written = %d, want %d (the held one + a full buffer)", len(runs), 1+runSinkBuffer)
	}
	for _, r := range runs {
		if r.WorkflowID == "overflow" {
			t.Fatal("the dropped run was written")
		}
	}
}

func TestRunSink_DrainOnShutdown(t *testing.T) {
	rec := &gateRecorder{}
	sink := NewRunSink(rec, zap.NewNop())
	for i := 0; i < 100; i++ {
		sink.Record(RunInput{WorkflowID: fmt.Sprint(i)})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := len(rec.written()); n != 100 {
		t.Fatalf("runs written by the time Close returned = %d, want 100", n)
	}
	sink.Record(RunInput{WorkflowID: "late"}) // after Close: dropped, never a send on a closed channel
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if n := len(rec.written()); n != 100 {
		t.Fatalf("a run recorded after Close was written (%d)", n)
	}

	var none *RunSink // an unwired sink records nothing and closes clean
	none.Record(RunInput{WorkflowID: "x"})
	if err := none.Close(ctx); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}
