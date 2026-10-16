package bpm

// specs/043 S9b: the sender picks the process, the same call twice makes one hồ sơ.

import (
	"context"
	"errors"
	"testing"

	"nexus/platform/internal/workflow"
)

func twoManual() map[Trigger][]Process {
	a := manualProc(nil)
	a.ID, a.Name = "p-a", "Duyệt A"
	b := manualProc(nil)
	b.ID, b.Name = "p-b", "Duyệt B"
	b.Steps = []Step{{Name: "GĐ duyệt", ApproverType: ApproverUser, ApproverID: "director", SLASeconds: 3600}}
	return map[Trigger][]Process{TriggerManual: {a, b}}
}

func bigOnly() Process {
	p := manualProc(&workflow.Condition{Match: "and", Rules: []workflow.Rule{{Field: "total", Op: "gt", Value: 100}}})
	p.ID, p.Name = "p-big", "Trên 100"
	return p
}

func TestSubmit_ExplicitProcessId(t *testing.T) {
	r, store, _ := newRequesterFixture(twoManual())
	inst, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter}, SubmitOptions{ProcessID: "p-b"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if inst.ProcessID != "p-b" || inst.CurrentApproverID != "director" {
		t.Fatalf("armed %s on %s, want p-b on director (the first match p-a must not win)", inst.ProcessID, inst.CurrentApproverID)
	}
	if len(store.created) != 1 {
		t.Fatalf("created %d instances, want 1", len(store.created))
	}
}

func TestSubmit_ProcessNotApplicable422(t *testing.T) {
	cases := []struct {
		name string
		opt  SubmitOptions
		data map[string]any
	}{
		{"unknown id", SubmitOptions{ProcessID: "nope"}, nil},
		{"criteria not met", SubmitOptions{ProcessID: "p-big"}, map[string]any{"total": 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, store, _ := newRequesterFixture(map[Trigger][]Process{TriggerManual: {bigOnly()}})
			_, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", tc.data, Actor{UserID: submitter}, tc.opt)
			if !errors.Is(err, ErrProcessNotApplicable) {
				t.Fatalf("err = %v, want ErrProcessNotApplicable", err)
			}
			if len(store.created) != 0 {
				t.Fatal("an inapplicable process must arm nothing")
			}
		})
	}
}

func TestStartable_ListsMatchingManual(t *testing.T) {
	procs := twoManual()
	procs[TriggerManual] = append(procs[TriggerManual], bigOnly())
	r, _, loader := newRequesterFixture(procs)

	got, err := r.Startable(context.Background(), "t1", "quotes", map[string]any{"total": 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ProcessID != "p-a" || got[1].ProcessID != "p-b" || got[1].Name != "Duyệt B" {
		t.Fatalf("startable = %+v, want p-a and p-b only (p-big's criteria fail)", got)
	}
	if len(loader.asked) != 1 || loader.asked[0] != TriggerManual {
		t.Errorf("asked %v, want [manual]", loader.asked)
	}
}

func TestSubmit_IdempotencyKeySameInstance(t *testing.T) {
	r, store, _ := newRequesterFixture(twoManual())
	opt := SubmitOptions{ProcessID: "p-b", IdempotencyKey: "k-1"}
	first, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter}, opt)
	if err != nil {
		t.Fatal(err)
	}
	// The retry arrives while the first hồ sơ is still open: without the key that is
	// APPROVAL_ALREADY_PENDING; with it, it is the same answer as the first call.
	second, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter}, opt)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if second.ID != first.ID || len(store.created) != 1 {
		t.Fatalf("retry gave %s (created %d), want the same instance %s and one row", second.ID, len(store.created), first.ID)
	}
	if store.created[0].IdempotencyKey != "k-1" || store.created[0].RequestHash == "" {
		t.Errorf("key/hash not stored: %+v", store.created[0])
	}
}

func TestSubmit_IdempotencyKeyReuseDiffPayload422(t *testing.T) {
	cases := []struct {
		name  string
		rec   string
		proc  string
		actor string
	}{
		{"other record", "rec2", "p-b", submitter},
		{"other process", "rec1", "p-a", submitter},
		{"other sender", "rec1", "p-b", "mallory"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, store, _ := newRequesterFixture(twoManual())
			if _, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", nil, Actor{UserID: submitter}, SubmitOptions{ProcessID: "p-b", IdempotencyKey: "k-1"}); err != nil {
				t.Fatal(err)
			}
			_, err := r.SubmitWith(context.Background(), "t1", "quotes", tc.rec, nil, Actor{UserID: tc.actor}, SubmitOptions{ProcessID: tc.proc, IdempotencyKey: "k-1"})
			if !errors.Is(err, ErrIdempotencyKeyReused) {
				t.Fatalf("err = %v, want ErrIdempotencyKeyReused", err)
			}
			if len(store.created) != 1 {
				t.Fatal("a reused key must create nothing")
			}
		})
	}
}

// An API key is a principal whose "user id" is the key's own id (api_key_service.go): it can submit,
// is recorded as the sender, and its retry is idempotent like anyone's.
func TestSubmit_APIKeyActor(t *testing.T) {
	r, store, _ := newRequesterFixture(twoManual())
	key := Actor{UserID: "0a1b2c3d-0000-4000-8000-00000000a111"}
	opt := SubmitOptions{IdempotencyKey: "ext-77"}
	first, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", nil, key, opt)
	if err != nil {
		t.Fatal(err)
	}
	if first.CreatedBy != key.UserID {
		t.Errorf("created_by = %q, want the key id", first.CreatedBy)
	}
	again, err := r.SubmitWith(context.Background(), "t1", "quotes", "rec1", nil, key, opt)
	if err != nil || again.ID != first.ID || len(store.created) != 1 {
		t.Fatalf("retry = %v / %v, want the same instance", again, err)
	}
}
