package bpm

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// DAILY-QUEUE #70: the approval audit event carried a step INDEX only. "approved step 1" does not
// say which chain that step belonged to — and since #8 an instance may walk a pinned chain that
// differs from the live process, so the index cannot be resolved against today's definition
// either. Every decision's audit event must carry the process id and the definition the engine
// actually walked for that decision.
func TestDecide_AuditRecordsExecutedDefinition(t *testing.T) {
	three := []Step{
		{Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr"},
		{Name: "Giám đốc", ApproverType: ApproverUser, ApproverID: "u-dir", SLASeconds: 3600},
		{Name: "CFO", ApproverType: ApproverRole, ApproverID: "role-cfo"},
	}
	pinnedOutcome := &Outcome{StatusField: "status", ApprovedValue: "Chấp nhận"}
	cases := []struct {
		name   string
		pinned bool
		want   *Definition
	}{
		// The live process is edited down to 2 steps after arming: the audit must record the
		// 3-step PINNED chain the engine walks, not the live one.
		{"pinned instance records the pinned chain", true, &Definition{Steps: three, Outcome: pinnedOutcome}},
		// Armed before 000065: the engine reads the live process at decision time, and that live
		// read is the chain this decision executed — with the stable ids every read carries since
		// specs/043 S0 (definitionOf normalizes the live steps). The pinned case above stays raw
		// only because fakeStore skips rowToInstance, which normalizes a real pinned row too.
		{"pre-000065 instance records the live chain it was decided under", false, &Definition{Steps: NormalizeSteps(three[:2])}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newFakeStore()
			procWithSteps(t, store, "p1", "t1", three)
			if tc.pinned {
				pinnedInstance(store, "i1", "t1", "p1", &Definition{Steps: three, Outcome: pinnedOutcome})
			} else {
				pinnedInstance(store, "i1", "t1", "p1", &Definition{Steps: three})
				store.instances["i1"].Definition = nil
			}
			procWithSteps(t, store, "p1", "t1", three[:2]) // admin edits the live process
			rec := &fakeRecorder{}
			d := NewDecider(store, store, store, rec, nil)

			if _, err := d.Decide(ctx, "t1", "i1", DecisionApprove, "ok", Actor{UserID: "u-mgr"}); err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if len(rec.events) != 1 {
				t.Fatalf("audit events = %d; want 1", len(rec.events))
			}
			ev := rec.events[0]
			if ev.ProcessID != "p1" {
				t.Errorf("audit ProcessID = %q; want p1 — which process the step index belongs to", ev.ProcessID)
			}
			if ev.Step != 0 || ev.Action != HistoryApproved {
				t.Errorf("audit step/action = %d/%s; want 0/approved", ev.Step, ev.Action)
			}
			if !reflect.DeepEqual(ev.Definition, tc.want) {
				got, _ := json.Marshal(ev.Definition)
				want, _ := json.Marshal(tc.want)
				t.Errorf("audit Definition = %s; want %s", got, want)
			}
		})
	}
}

func TestSLAEscalation_AuditRecordsPinnedDefinition(t *testing.T) {
	store := newFakeStore()
	past := time.Now().UTC().Add(-time.Hour)
	def := &Definition{Steps: []Step{{Name: "Trưởng phòng", ApproverType: ApproverUser, ApproverID: "u-mgr", SLASeconds: 60}}}
	if _, err := store.CreateInstance(context.Background(), CreateInstanceInput{
		TenantID: "t1", ProcessID: "p1", EntityName: "deals", RecordID: "r1",
		CurrentApproverType: ApproverUser, CurrentApproverID: "u-mgr", SLADueAt: &past, Definition: def,
	}); err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	rec := &fakeRecorder{}
	NewSLAScheduler(SLASchedulerConfig{Interval: time.Minute}, store, store, rec, nil).runBreaches(context.Background(), time.Now().UTC())

	if len(rec.events) != 1 {
		t.Fatalf("audit events = %d; want 1 escalation", len(rec.events))
	}
	if ev := rec.events[0]; ev.ProcessID != "p1" || !reflect.DeepEqual(ev.Definition, def) {
		t.Errorf("escalation audit ProcessID=%q Definition=%+v; want p1 + the pinned definition", ev.ProcessID, ev.Definition)
	}
}
