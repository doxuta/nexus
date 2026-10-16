package bpm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"nexus/platform/internal/jobs"
)

// fakeArmQueue records the jobs the engine parks when its buffer is full.
type fakeArmQueue struct {
	jobs []jobs.Job
	err  error
}

func (f *fakeArmQueue) Enqueue(_ context.Context, j jobs.Job) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.jobs = append(f.jobs, j)
	return fmt.Sprintf("job-%d", len(f.jobs)), nil
}

func durableProc() Process {
	return Process{
		ID: "p1", TenantID: "t1", EntityName: "deals", Trigger: TriggerAfterCreate,
		Steps: []Step{{ApproverType: ApproverUser, ApproverID: "u-mgr"}}, IsActive: true,
	}
}

// T150: a full buffer used to DROP the start event, so the record never reached its approval and
// nobody was told. With a job queue the event is parked as a bpm_arm job; the handler arms it.
func TestEmit_BufferFull_EnqueuesDurableArm(t *testing.T) {
	g := gateProcLoader{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	store := newFakeStore()
	q := &fakeArmQueue{}
	e := NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, g, store, store, nil).WithArmJobs(q)
	defer func() { close(g.gate); _ = e.Shutdown(context.Background()) }()
	mk := func(rec string) Event {
		return Event{TenantID: "t1", ActorUserID: "u-1", EntityName: "deals", RecordID: rec,
			Trigger: TriggerAfterCreate, Data: map[string]any{"amount": float64(5000)}, Depth: 1}
	}

	e.Emit(context.Background(), mk("r1"))
	<-g.entered                            // the worker holds r1
	e.Emit(context.Background(), mk("r2")) // fills the buffer
	if len(q.jobs) != 0 {
		t.Fatalf("a free slot parked %d job(s); want 0", len(q.jobs))
	}
	e.Emit(context.Background(), mk("r3")) // overflow
	if len(q.jobs) != 1 {
		t.Fatalf("overflow parked %d job(s); want 1", len(q.jobs))
	}
	j := q.jobs[0]
	if j.Type != ArmJobType || j.TenantID != "t1" {
		t.Fatalf("job = %s tenant %q; want %s tenant t1 (the tenant rides the job row)", j.Type, j.TenantID, ArmJobType)
	}

	// The handler arms the parked event from its payload.
	store2 := newFakeStore()
	h := NewArmJobHandler(NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, fakeLoader{procs: []Process{durableProc()}}, store2, store2, nil))
	if err := h(context.Background(), j); err != nil {
		t.Fatalf("handler: %v", err)
	}
	if len(store2.created) != 1 || store2.created[0].RecordID != "r3" || store2.created[0].CreatedBy != "u-1" {
		t.Fatalf("handler armed %+v; want one instance for r3 by u-1", store2.created)
	}
}

func TestEmit_BufferFull_QueueErrorStillDoesNotBlock(t *testing.T) {
	g := gateProcLoader{entered: make(chan struct{}, 1), gate: make(chan struct{})}
	store := newFakeStore()
	e := NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, g, store, store, nil).WithArmJobs(&fakeArmQueue{err: errors.New("db down")})
	defer func() { close(g.gate); _ = e.Shutdown(context.Background()) }()
	evt := Event{TenantID: "t1", EntityName: "deals", RecordID: "r", Trigger: TriggerAfterCreate}
	e.Emit(context.Background(), evt)
	<-g.entered
	e.Emit(context.Background(), evt)
	e.Emit(context.Background(), evt) // must return (fail-open), not block or panic
}

func TestArmJob_Cases(t *testing.T) {
	valid, _ := json.Marshal(map[string]any{"entityName": "deals", "recordId": "r1", "trigger": "after_create", "data": map[string]any{}})
	for _, tc := range []struct {
		name      string
		job       jobs.Job
		openErr   error
		createErr error
		wantErr   bool
		perm      bool
		wantMade  int
	}{
		{"arms", jobs.Job{TenantID: "t1", Payload: valid}, nil, nil, false, false, 1},
		{"already waiting is a success", jobs.Job{TenantID: "t1", Payload: valid}, nil, ErrAlreadyPending, false, false, 0},
		{"store failure retries", jobs.Job{TenantID: "t1", Payload: valid}, errors.New("db"), nil, true, false, 0},
		{"create failure retries", jobs.Job{TenantID: "t1", Payload: valid}, nil, errors.New("db"), true, false, 0},
		{"no tenant is permanent", jobs.Job{Payload: valid}, nil, nil, true, true, 0},
		{"garbage payload is permanent", jobs.Job{TenantID: "t1", Payload: []byte(`{`)}, nil, nil, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.openErr, store.createErr = tc.openErr, tc.createErr
			h := NewArmJobHandler(NewAsyncEngine(EngineConfig{Buffer: 1, Workers: 1}, fakeLoader{procs: []Process{durableProc()}}, store, store, nil))
			err := h(context.Background(), tc.job)
			if (err != nil) != tc.wantErr || (err != nil && jobs.IsPermanent(err) != tc.perm) {
				t.Fatalf("err = %v (permanent %v); want error %v permanent %v", err, jobs.IsPermanent(err), tc.wantErr, tc.perm)
			}
			if len(store.created) != tc.wantMade {
				t.Fatalf("created %d; want %d", len(store.created), tc.wantMade)
			}
		})
	}
}
