package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
)

// slaFixture: one record with an approval (exclusive lane) and one SLA tracker (independent lane).
func slaFixture() *fakeApprovalInstanceRepo {
	due := time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
	return &fakeApprovalInstanceRepo{
		byRecord: map[string][]bpm.Instance{"tk_tickets\x00rec-1": {{ID: "ap-1", EntityName: "tk_tickets", RecordID: "rec-1"}}},
		byLane: map[string][]bpm.Instance{"independent\x00tk_tickets\x00rec-1": {{
			ID: "sla-1", EntityName: "tk_tickets", RecordID: "rec-1", Status: bpm.StatusPending, SLADueAt: &due,
			History: []bpm.HistoryEntry{{Action: bpm.HistoryWaiting, StepName: "Phản hồi đầu tiên", SLABasis: bpm.SLABasisBusiness}},
		}}},
	}
}

func TestRecordApprovals_LaneIndependentParam(t *testing.T) {
	svc := NewApprovalService(&fakeApprovalProcessRepo{}, slaFixture(), &fakeApprovalDecider{})
	admin := RecordScope{IsAdmin: true}

	items, err := svc.GetRecordSLA(context.Background(), admin, "t1", "tk_tickets", "rec-1")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v; want the one SLA line", items, err)
	}
	got := items[0]
	if got.StepName != "Phản hồi đầu tiên" || got.SLA.Basis != string(bpm.SLABasisBusiness) || got.SLA.DueAt == nil || got.SLA.Met || got.SLA.Breached || got.SLA.Paused {
		t.Fatalf("sla line = %+v; want step name, business basis, a deadline, not met/breached/paused", got)
	}
	// the default read is unchanged: approvals only, the tracker never leaks into the Duyệt tab
	rows, err := svc.GetInstancesForRecord(context.Background(), admin, "t1", "tk_tickets", "rec-1")
	if err != nil || len(rows) != 1 || rows[0].ID != "ap-1" {
		t.Fatalf("default read = %+v err=%v; want only the approval", rows, err)
	}
}

func TestSLAItem_MetAndBreached(t *testing.T) {
	tests := []struct {
		name    string
		in      bpm.Instance
		met     bool
		breache bool
	}{
		{"answered in time", bpm.Instance{History: []bpm.HistoryEntry{{Action: bpm.HistoryWaiting, StepName: "A"}, {Action: bpm.HistoryWaitMet, StepName: "A"}}}, true, false},
		{"missed, still waiting", bpm.Instance{History: []bpm.HistoryEntry{{Action: bpm.HistoryWaiting, StepName: "A"}, {Action: bpm.HistoryEscalated}}}, false, true},
		{"missed then answered late", bpm.Instance{History: []bpm.HistoryEntry{{Action: bpm.HistoryWaiting, StepName: "A"}, {Action: bpm.HistoryEscalated}, {Action: bpm.HistoryWaitMet, StepName: "A"}}}, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			it := slaItem(tc.in)
			if it.SLA.Met != tc.met || it.SLA.Breached != tc.breache || it.StepName != "A" {
				t.Fatalf("slaItem = %+v; want met=%v breached=%v", it, tc.met, tc.breache)
			}
		})
	}
}

func TestRecordApprovals_LaneIndependent_RecordViewGate404(t *testing.T) {
	svc := NewApprovalService(&fakeApprovalProcessRepo{}, slaFixture(), &fakeApprovalDecider{}).
		WithRequestFlow(nil, &fakeApprovalRecordReader{err: ErrRecordNotFound})
	items, err := svc.GetRecordSLA(context.Background(), RecordScope{UserID: "u2", AccessLevel: "owner"}, "t1", "tk_tickets", "rec-1")
	if !errors.Is(err, ErrRecordNotFound) || len(items) != 0 {
		t.Fatalf("hidden row: items=%v err=%v; want ErrRecordNotFound and no SLA line", items, err)
	}
}

func TestApprovalOptions_LaneIsValidatedAtSave(t *testing.T) {
	g := &ApprovalGraphInput{Nodes: []ApprovalNodeInput{{Kind: "wait"}, {Kind: "end"}}}
	human := &ApprovalGraphInput{Nodes: []ApprovalNodeInput{{Kind: "wait"}, {Kind: "approval"}}}
	opt := func(lane string) []byte {
		b, err := approvalOptionsJSON(&ApprovalOptionsInput{Lane: lane})
		if err != nil {
			t.Fatalf("options(%q): %v", lane, err)
		}
		return b
	}
	tests := []struct {
		name    string
		lane    string
		trigger bpm.Trigger
		graph   *ApprovalGraphInput
		code    string
	}{
		{"machine nodes only is fine", "independent", bpm.TriggerAfterCreate, g, ""},
		{"exclusive stores nothing and checks nothing", "exclusive", bpm.TriggerManual, human, ""},
		{"a human node is refused", "independent", bpm.TriggerAfterCreate, human, "APPROVAL_LANE_HUMAN_NODE"},
		{"manual trigger is refused", "independent", bpm.TriggerManual, g, "APPROVAL_OPTIONS_INVALID"},
		{"no graph is refused", "independent", bpm.TriggerAfterCreate, nil, "APPROVAL_OPTIONS_INVALID"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLaneForm(opt(tc.lane), tc.trigger, tc.graph)
			var ve *ValidationError
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.code != "" && (!errors.As(err, &ve) || ve.Code != tc.code):
				t.Fatalf("err = %v; want code %s", err, tc.code)
			}
		})
	}
	if _, err := approvalOptionsJSON(&ApprovalOptionsInput{Lane: "sideways"}); err == nil {
		t.Fatal("an unknown lane saved")
	}
}
