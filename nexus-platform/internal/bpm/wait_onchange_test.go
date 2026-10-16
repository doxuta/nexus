package bpm

import (
	"context"
	"testing"
	"time"
)

// specs/043 S14b — sla.onChange (keep|tighten_only) on a wait node.

func onChangeSLA(mode string) *WaitSLA {
	s := slaMatrix()
	s.OnChange = mode
	return s
}

// restWithPriority rests a wait instance at friday1630 under priority p and returns the rig.
func restWithPriority(t *testing.T, mode, p string) (*waitRig, *Instance) {
	t.Helper()
	data := map[string]any{"status": "Open", "priority": p}
	w := newWaitRig(waitGraph(onChangeSLA(mode)), data, friday1630)
	inst := w.arm(data)
	if inst == nil || inst.SLADueAt == nil {
		t.Fatalf("instance %+v; want it resting with a deadline", inst)
	}
	// the record now carries what the engine wrote (the fake does not apply writes)
	w.rec.data["first_response_due"] = inst.SLADueAt.UTC().Format(time.RFC3339)
	return w, inst
}

func signal(t *testing.T, w *waitRig, inst *Instance, priority string) *Instance {
	t.Helper()
	w.rec.data["priority"] = priority
	if err := w.runner.Continue(context.Background(), "t1", inst.ID); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	return w.store.instances[inst.ID]
}

// TestSLAOnChange_TightenOnlyTable: raising the priority shortens the deadline (from the moment the
// node was entered), lowering or repeating it keeps it, and `keep` is today's behaviour.
func TestSLAOnChange_TightenOnlyTable(t *testing.T) {
	tests := []struct {
		name, mode, from, to string
		wantShorter          bool
	}{
		{"raise Thấp→Cao tightens", "tighten_only", "Thấp", "Cao", true},
		{"lower Cao→Thấp keeps", "tighten_only", "Cao", "Thấp", false},
		{"same priority keeps", "tighten_only", "Cao", "Cao", false},
		{"keep ignores a raise", "keep", "Thấp", "Cao", false},
		{"default (empty) ignores a raise", "", "Thấp", "Cao", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, inst := restWithPriority(t, tc.mode, tc.from)
			old := *inst.SLADueAt
			seq := inst.Seq
			got := signal(t, w, inst, tc.to)
			if tc.wantShorter {
				want := friday1630.Add(30 * time.Minute)
				if got.SLADueAt == nil || !got.SLADueAt.Equal(want) || got.Seq != seq+1 {
					t.Fatalf("due %v seq %d; want %v seq %d", got.SLADueAt, got.Seq, want, seq+1)
				}
				if n := len(w.rec.writes); n != 2 || w.rec.writes[1]["first_response_due"] != want.Format(time.RFC3339) {
					t.Fatalf("record writes %v; want the new deadline written", w.rec.writes)
				}
				return
			}
			if !got.SLADueAt.Equal(old) || got.Seq != seq || len(w.rec.writes) != 1 {
				t.Fatalf("due %v seq %d writes %d; want unchanged", got.SLADueAt, got.Seq, len(w.rec.writes))
			}
		})
	}
}

// TestSLAOnChange_NeverOverwritesManualDue: a person typed another deadline into the field ⇒ the
// instance still tightens its own clock but the record field is left alone.
func TestSLAOnChange_NeverOverwritesManualDue(t *testing.T) {
	w, inst := restWithPriority(t, "tighten_only", "Thấp")
	w.rec.data["first_response_due"] = "2027-03-01T16:00:00Z" // hand-typed
	got := signal(t, w, inst, "Cao")
	if !got.SLADueAt.Equal(friday1630.Add(30 * time.Minute)) {
		t.Fatalf("due %v; the instance clock must still tighten", got.SLADueAt)
	}
	if len(w.rec.writes) != 1 {
		t.Fatalf("record writes %v; a hand-typed deadline was overwritten", w.rec.writes)
	}
}

// TestSLAOnChange_ReenqueuesSLADue: the shortened deadline goes through the guarded write, so seq
// moves (the bpm_sla_due job is parked for the new seq by DecideTaskTx — T198/T290).
func TestSLAOnChange_ReenqueuesSLADue(t *testing.T) {
	w, inst := restWithPriority(t, "tighten_only", "Thấp")
	seq := inst.Seq
	got := signal(t, w, inst, "Cao")
	if got.Seq != seq+1 || got.Status != StatusPending {
		t.Fatalf("seq %d→%d status %s; want a guarded write", seq, got.Seq, got.Status)
	}
}

func TestCheckWait_OnChange(t *testing.T) {
	for mode, ok := range map[string]bool{"": true, "keep": true, "tighten_only": true, "overwrite": false} {
		n := waitGraph(onChangeSLA(mode)).Nodes[0]
		if f, _ := CheckWait(n); (f == "") != ok {
			t.Fatalf("onChange %q: blamed %q, want ok=%v", mode, f, ok)
		}
	}
}
