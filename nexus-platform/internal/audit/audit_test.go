// RM-19 audit unit tests (no DB, no build tag — always run with `go test ./...`).
//
// Two things are proven here without a database:
//   - ComputeDiff / StructToMap: only the keys that actually changed appear; create is
//     after-only, delete is before-only, an unchanged update yields nil (spec §2/§4).
//   - AsyncDispatcher fail-open: a full buffer or a Writer that errors NEVER blocks, panics,
//     or propagates an error to the caller; Record always returns immediately (spec §3/§5).
package audit

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestComputeDiff_OnlyChangedKeys(t *testing.T) {
	before := map[string]any{"name": "Acme", "stage": "lead", "amount": 100.0}
	after := map[string]any{"name": "Acme", "stage": "won", "amount": 250.0}

	d := ComputeDiff(before, after)
	if d == nil {
		t.Fatalf("ComputeDiff returned nil for a changed update; want a diff")
	}
	// "name" is unchanged → must NOT appear on either side.
	if _, ok := d.Before["name"]; ok {
		t.Errorf("unchanged key 'name' leaked into Before: %+v", d.Before)
	}
	if _, ok := d.After["name"]; ok {
		t.Errorf("unchanged key 'name' leaked into After: %+v", d.After)
	}
	wantBefore := map[string]any{"stage": "lead", "amount": 100.0}
	wantAfter := map[string]any{"stage": "won", "amount": 250.0}
	if !reflect.DeepEqual(d.Before, wantBefore) {
		t.Errorf("Before = %+v; want %+v", d.Before, wantBefore)
	}
	if !reflect.DeepEqual(d.After, wantAfter) {
		t.Errorf("After = %+v; want %+v", d.After, wantAfter)
	}
}

func TestComputeDiff_CreateIsAfterOnly(t *testing.T) {
	after := map[string]any{"name": "NewCo", "stage": "lead"}
	d := ComputeDiff(nil, after)
	if d == nil {
		t.Fatalf("ComputeDiff(nil, after) = nil; want after-only diff")
	}
	if d.Before != nil {
		t.Errorf("create diff Before = %+v; want nil (after-only)", d.Before)
	}
	if !reflect.DeepEqual(d.After, after) {
		t.Errorf("create diff After = %+v; want %+v", d.After, after)
	}
}

func TestComputeDiff_DeleteIsBeforeOnly(t *testing.T) {
	before := map[string]any{"name": "GoneCo", "stage": "lost"}
	d := ComputeDiff(before, nil)
	if d == nil {
		t.Fatalf("ComputeDiff(before, nil) = nil; want before-only diff")
	}
	if d.After != nil {
		t.Errorf("delete diff After = %+v; want nil (before-only)", d.After)
	}
	if !reflect.DeepEqual(d.Before, before) {
		t.Errorf("delete diff Before = %+v; want %+v", d.Before, before)
	}
}

func TestComputeDiff_NoChangeReturnsNil(t *testing.T) {
	m := map[string]any{"name": "Same", "n": 1.0}
	if d := ComputeDiff(m, map[string]any{"name": "Same", "n": 1.0}); d != nil {
		t.Errorf("ComputeDiff of identical maps = %+v; want nil (store NULL diff)", d)
	}
	if d := ComputeDiff(nil, nil); d != nil {
		t.Errorf("ComputeDiff(nil, nil) = %+v; want nil", d)
	}
}

func TestComputeDiff_NestedStructuralEquality(t *testing.T) {
	// multi_select / composite values compare structurally (reflect.DeepEqual), not by pointer.
	before := map[string]any{"tags": []any{"a", "b"}}
	after := map[string]any{"tags": []any{"a", "b"}}
	if d := ComputeDiff(before, after); d != nil {
		t.Errorf("structurally-equal nested slices reported as changed: %+v", d)
	}
	changed := ComputeDiff(map[string]any{"tags": []any{"a"}}, map[string]any{"tags": []any{"a", "b"}})
	if changed == nil {
		t.Fatalf("changed nested slice reported as unchanged; want a diff")
	}
}

func TestStructToMap_UsesJSONTags(t *testing.T) {
	type meta struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	m := StructToMap(meta{Name: "accounts", Label: "Accounts"})
	if m["name"] != "accounts" || m["label"] != "Accounts" {
		t.Errorf("StructToMap = %+v; want json-tag keyed map", m)
	}
	if StructToMap(nil) != nil {
		t.Errorf("StructToMap(nil) must be nil")
	}
}

// ---- dispatcher fail-open ---------------------------------------------------------------

// countingWriter records how many events were written; optionally errors on every write.
type countingWriter struct {
	count   int64
	fail    bool
	gotErr  bool
	mu      sync.Mutex
	written []Event
}

func (w *countingWriter) InsertEvent(_ context.Context, evt Event) error {
	atomic.AddInt64(&w.count, 1)
	if w.fail {
		w.mu.Lock()
		w.gotErr = true
		w.mu.Unlock()
		return errors.New("simulated audit DB write failure")
	}
	w.mu.Lock()
	w.written = append(w.written, evt)
	w.mu.Unlock()
	return nil
}

func TestAsyncDispatcher_WritesEvent(t *testing.T) {
	w := &countingWriter{}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 2}, w, zap.NewNop())

	d.Record(context.Background(), Event{TenantID: "t1", Action: ActionCreate, EntityName: "accounts", RecordID: "r1"})

	// Shutdown drains all buffered events before returning.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown drain: %v", err)
	}
	if got := atomic.LoadInt64(&w.count); got != 1 {
		t.Fatalf("writer saw %d events; want exactly 1", got)
	}
}

// TestAsyncDispatcher_FailOpenOnWriteError proves a Writer that always errors NEVER causes
// Record to panic or block: the business flow (the caller) is unaffected; the error is
// swallowed (logged) inside the worker.
func TestAsyncDispatcher_FailOpenOnWriteError(t *testing.T) {
	w := &countingWriter{fail: true}
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 8, Workers: 2}, w, zap.NewNop())

	// Record must return immediately and never panic even though the writer errors.
	for i := 0; i < 5; i++ {
		d.Record(context.Background(), Event{TenantID: "t1", Action: ActionUpdate, EntityName: "accounts", RecordID: "r1"})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown drain: %v", err)
	}
	w.mu.Lock()
	gotErr := w.gotErr
	w.mu.Unlock()
	if !gotErr {
		t.Fatalf("expected the failing writer to have been invoked")
	}
	// The key assertion is simply that we got here: no panic, no deadlock, Record returned.
}

// TestAsyncDispatcher_FullBufferDropsNotBlocks proves a full buffer drops the event with a
// log warning rather than blocking the caller (fail-open). We use a writer that blocks until
// released so the buffer fills, then assert Record still returns promptly.
func TestAsyncDispatcher_FullBufferDropsNotBlocks(t *testing.T) {
	release := make(chan struct{})
	bw := &blockingWriter{release: release}
	// 1 worker + tiny buffer: the worker grabs the first event and blocks; the buffer then
	// fills; further Records must drop, not block.
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 1, Workers: 1}, bw, zap.NewNop())

	// Fire many events while the single worker is blocked. If Record blocked, this would hang
	// and the test would time out. A bounded loop returning quickly proves non-blocking drop.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			d.Record(context.Background(), Event{TenantID: "t1", Action: ActionCreate, EntityName: "accounts"})
		}
		close(done)
	}()
	select {
	case <-done:
		// Good: Record never blocked despite a stalled worker + full buffer.
	case <-time.After(2 * time.Second):
		t.Fatalf("Record blocked on a full buffer — fail-open violated")
	}

	close(release) // unblock the worker so Shutdown can drain
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown drain: %v", err)
	}
}

type blockingWriter struct {
	release chan struct{}
}

func (b *blockingWriter) InsertEvent(_ context.Context, _ Event) error {
	<-b.release
	return nil
}
