package services

// specs/043 S7 (T130): GET /approvals/:id on a routed process answers with its nodes (kind and
// routing included) and the path it took, and says when it stands still on an automatic step — the
// reason only to an admin.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
)

func routedInstance(t *testing.T) *bpm.Instance {
	t.Helper()
	g := &bpm.Graph{Start: "route", Nodes: []bpm.Node{
		{Step: bpm.Step{ID: "route", Name: "Phân luồng"}, Kind: bpm.NodeGateway, Default: "end",
			Branches: []bpm.Branch{{Name: "lon", To: "stamp"}}},
		{Step: bpm.Step{ID: "stamp", Name: "Ghi mã"}, Kind: bpm.NodeAuto, Set: []bpm.SetField{{Field: "code", Value: "A"}}, Next: "end"},
		{Step: bpm.Step{ID: "end", Name: "Xong"}, Kind: bpm.NodeEnd, Result: bpm.ResultApproved},
	}}
	ctxMap, err := (bpm.InstanceContext)(nil).WithGateway("route", "lon")
	if err != nil {
		t.Fatal(err)
	}
	return &bpm.Instance{
		ID: "inst-1", TenantID: "t1", ProcessID: "proc-1", EntityName: "deals", RecordID: "rec-1",
		CurrentStep: 1, Status: bpm.StatusPending, CreatedBy: "creator-1",
		Definition: &bpm.Definition{Steps: []bpm.Step{}, Graph: g},
		Context:    ctxMap,
		LastError:  `giá trị "A" đã được dùng`,
		History: []bpm.HistoryEntry{
			{Step: 0, Action: bpm.HistorySubmitted, Actor: "creator-1"},
			{Step: 0, Action: bpm.HistoryGateway, Node: "route", Comment: "lon"},
			{Step: 1, Action: bpm.HistoryAuto, Node: "stamp", Comment: "code=A"},
			{Step: 1, Action: bpm.HistoryEffectFailed, Node: "stamp", Comment: `giá trị "A" đã được dùng`},
			{Step: 1, Action: bpm.HistoryRetried, Actor: "boss", Comment: "thử lại"},
		},
	}
}

func TestApprovalDetail_RoutedGraphPathAndStuck(t *testing.T) {
	inst := routedInstance(t)
	svc := w6ApprovalSvc(inst, w6Process())

	admin, err := svc.GetApprovalDetail(context.Background(), RecordScope{}, "t1", "inst-1", bpm.Actor{UserID: "boss", IsAdmin: true})
	if err != nil {
		t.Fatalf("admin detail: %v", err)
	}
	if admin.Gateway["route"] != "lon" {
		t.Fatalf("gateway path %v; want route=lon", admin.Gateway)
	}
	if len(admin.History) != 5 {
		t.Fatalf("admin history = %d lines; want all 5 (the Nexus team reads the machine's lines)", len(admin.History))
	}
	if !admin.Stuck || admin.LastError == "" {
		t.Fatalf("admin sees stuck=%v lastError=%q; want the reason", admin.Stuck, admin.LastError)
	}
	var nodes []struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Next string `json:"next"`
	}
	if err := json.Unmarshal(admin.Process.Steps, &nodes); err != nil || len(nodes) != 3 || nodes[0].Kind != "gateway" || nodes[1].Next != "end" || nodes[2].Kind != "end" {
		t.Fatalf("process.steps = %s (%v); want the three nodes with kind and next", admin.Process.Steps, err)
	}

	// The submitter (visible through their record) sees that it stands still, never why.
	other := w6ApprovalSvc(inst, w6Process())
	other.WithRequestFlow(nil, &fakeApprovalRecordReader{authzLevel: "all"})
	sub, err := other.GetApprovalDetail(context.Background(), RecordScope{UserID: "creator-1"}, "t1", "inst-1", bpm.Actor{UserID: "creator-1"})
	if err != nil {
		t.Fatalf("submitter detail: %v", err)
	}
	if !sub.Stuck || sub.LastError != "" {
		t.Fatalf("submitter sees stuck=%v lastError=%q; want stuck without the engine's reason", sub.Stuck, sub.LastError)
	}
	// The pinned nodes carry no routing conditions, writes or results for the submitter.
	for _, leak := range []string{`"when"`, `"set"`, `"result"`, `"rules"`} {
		if strings.Contains(string(sub.Process.Steps), leak) {
			t.Fatalf("submitter process.steps leaks %s: %s", leak, sub.Process.Steps)
		}
	}
	// ... and not in the timeline either: the failure's comment IS the reason.
	if len(sub.History) != 1 || sub.History[0].Action != bpm.HistorySubmitted {
		t.Fatalf("submitter history = %+v; want only the submit line (branch names, writes and failures are the Nexus team's)", sub.History)
	}
}
