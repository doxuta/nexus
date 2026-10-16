package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// dateOffsetLoader returns one date_offset rule so scheduleTimers calls the TimerScheduler once per
// after_create/after_update event — a cheap probe for the order events are processed in.
type dateOffsetLoader struct{}

func (dateOffsetLoader) ListActive(_ context.Context, t, e string, trig Trigger) ([]Workflow, error) {
	if trig == TriggerDateOffset {
		return []Workflow{{ID: "w", TenantID: t, EntityName: e, Trigger: trig, IsActive: true}}, nil
	}
	return nil, nil
}

type recTimers struct {
	mu   sync.Mutex
	seq  []string
	slow map[string]time.Duration // key "recordID/trigger"
}

func (r *recTimers) ScheduleTimer(_ context.Context, _ Workflow, evt Event) error {
	key := evt.RecordID + "/" + string(evt.Trigger)
	if d := r.slow[key]; d > 0 {
		time.Sleep(d)
	}
	r.mu.Lock()
	r.seq = append(r.seq, key)
	r.mu.Unlock()
	return nil
}

// #444: events of ONE record must run in emit order even with several workers and a slow first
// event; before the fix a free worker took the later event and finished first.
func TestDispatcher_SameRecordEventsRunInEmitOrder(t *testing.T) {
	for round := 0; round < 5; round++ {
		rec := &recTimers{slow: map[string]time.Duration{"r1/" + string(TriggerAfterCreate): 60 * time.Millisecond}}
		d := NewAsyncDispatcher(DispatcherConfig{Buffer: 16, Workers: 4}, dateOffsetLoader{}, nil, nil, zap.NewNop()).WithTimerScheduler(rec)
		for _, tr := range []Trigger{TriggerAfterCreate, TriggerAfterUpdate} {
			d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "tickets", RecordID: "r1", Trigger: tr})
		}
		drainDispatcher(t, d)
		want := []string{"r1/" + string(TriggerAfterCreate), "r1/" + string(TriggerAfterUpdate)}
		if fmt.Sprint(rec.seq) != fmt.Sprint(want) {
			t.Fatalf("round %d: order = %v, want %v", round, rec.seq, want)
		}
	}
}

// Different records stay parallel: a slow record must not hold back a record on another shard.
func TestDispatcher_DifferentRecordsStayParallel(t *testing.T) {
	d := NewAsyncDispatcher(DispatcherConfig{Buffer: 16, Workers: 4}, dateOffsetLoader{}, nil, nil, zap.NewNop())
	slowID, fastID := "slow", ""
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("fast-%d", i)
		if d.shard("t1", id) != d.shard("t1", slowID) {
			fastID = id
			break
		}
	}
	if fastID == "" {
		t.Fatal("no record id on another shard")
	}
	rec := &recTimers{slow: map[string]time.Duration{slowID + "/" + string(TriggerAfterCreate): 80 * time.Millisecond}}
	d.WithTimerScheduler(rec)
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "tickets", RecordID: slowID, Trigger: TriggerAfterCreate})
	d.Emit(context.Background(), Event{TenantID: "t1", EntityName: "tickets", RecordID: fastID, Trigger: TriggerAfterCreate})
	drainDispatcher(t, d)
	if len(rec.seq) != 2 || rec.seq[0] != fastID+"/"+string(TriggerAfterCreate) {
		t.Fatalf("fast record should finish first, got %v", rec.seq)
	}
}
