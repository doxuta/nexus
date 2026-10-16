package bpm

import "testing"

func TestCheckOnReject_RefusesEndNodeTarget(t *testing.T) {
	steps := []Step{{ID: "fin", NodeKind: NodeEnd}, {ID: "a", NodeKind: NodeApproval}}
	if f, _ := CheckOnReject(&OnReject{To: "fin"}, NodeApproval, 1, steps); f != "to" {
		t.Fatalf("onReject to an end node accepted (field=%q)", f)
	}
	steps[0].NodeKind = NodeApproval
	if f, r := CheckOnReject(&OnReject{To: "fin"}, NodeApproval, 1, steps); f != "" {
		t.Fatalf("onReject to an earlier approval refused: %s", r)
	}
}
