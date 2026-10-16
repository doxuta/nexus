//go:build integration

// specs/043 S12b — `bpm-sla-lane`: the tracking lane written by the real CreateInstance and read by
// the real ListByRecordLane, on MySQL (T183, T272).
package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"nexus/platform/internal/bpm"
)

func trackingInput(tenant, record, proc string) bpm.CreateInstanceInput {
	in := openKeyInput(tenant, "quotes", record)
	in.ProcessID = proc
	in.Lane = bpm.LaneIndependent
	in.Definition = &bpm.Definition{
		Steps:   []bpm.Step{},
		Options: &bpm.Options{Lane: bpm.LaneIndependent},
		Graph: &bpm.Graph{Start: "end", Nodes: []bpm.Node{
			{Step: bpm.Step{ID: "end"}, Kind: bpm.NodeEnd, Result: bpm.ResultApproved},
		}},
	}
	in.CurrentApproverType, in.CurrentApproverID = "", ""
	return in
}

func TestIndependentLane_WrittenAndReadByLane_Integration(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	rec := uuid.NewString()
	proc := uuid.NewString()

	// an approval first, then two trackers of different processes: all three coexist
	if _, err := st.repo.CreateInstance(ctx, openKeyInput(tenant, "quotes", rec)); err != nil {
		t.Fatalf("approval: %v", err)
	}
	if _, err := st.repo.CreateInstance(ctx, trackingInput(tenant, rec, proc)); err != nil {
		t.Fatalf("tracker beside an approval: %v; want allowed", err)
	}
	if _, err := st.repo.CreateInstance(ctx, trackingInput(tenant, rec, uuid.NewString())); err != nil {
		t.Fatalf("second tracker (other process): %v; want allowed", err)
	}
	// the same process twice on one record is the unique key's to refuse, as "already waiting"
	if _, err := st.repo.CreateInstance(ctx, trackingInput(tenant, rec, proc)); !errors.Is(err, bpm.ErrAlreadyPending) {
		t.Fatalf("same tracker twice: err = %v; want ErrAlreadyPending", err)
	}

	tab, err := st.repo.ListByRecord(ctx, tenant, "quotes", rec)
	if err != nil || len(tab) != 1 {
		t.Fatalf("Duyệt tab = %d rows (%v); want only the approval", len(tab), err)
	}
	sla, err := st.repo.ListByRecordLane(ctx, tenant, "quotes", rec, bpm.LaneIndependent)
	if err != nil || len(sla) != 2 {
		t.Fatalf("SLA lane = %d rows (%v); want the two trackers", len(sla), err)
	}
	if sla[0].Definition.Lane() != bpm.LaneIndependent {
		t.Fatalf("the pin lost the lane: %q", sla[0].Definition.Lane())
	}
	// another tenant sees nothing of it
	if other, err := st.repo.ListByRecordLane(ctx, o3aTenantB, "quotes", rec, bpm.LaneIndependent); err != nil || len(other) != 0 {
		t.Fatalf("other tenant sees %d tracker rows (%v); want 0", len(other), err)
	}
}
