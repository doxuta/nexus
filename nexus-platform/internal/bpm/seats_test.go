package bpm

import (
	"testing"
	"time"
)

// TestProjection_OnlySingleUserOrRoleSeat (D-5): current_approver_* is ENUM('user','role') and the
// old inbox reads it, so it names the step's holder only when there is exactly ONE seat and that
// seat is a user or a role. Anything else — a team pool, a queue, several seats — projects NULL
// (writing 'team' into the ENUM is error 1265 on the fail-open arming path).
func TestProjection_OnlySingleUserOrRoleSeat(t *testing.T) {
	user := Seat{AssigneeType: ApproverUser, AssigneeID: "u1"}
	role := Seat{AssigneeType: ApproverRole, AssigneeID: "r1"}
	team := Seat{AssigneeType: ApproverType("team"), AssigneeID: "t1"}
	cases := []struct {
		name     string
		seats    []Seat
		wantType ApproverType
		wantID   string
	}{
		{"one user seat", []Seat{user}, ApproverUser, "u1"},
		{"one role seat", []Seat{role}, ApproverRole, "r1"},
		{"one team seat", []Seat{team}, "", ""},
		{"two seats", []Seat{user, role}, "", ""},
		{"no seat", nil, "", ""},
	}
	for _, c := range cases {
		gotType, gotID := projection(c.seats)
		if gotType != c.wantType || gotID != c.wantID {
			t.Errorf("%s: projection = %q/%q; want %q/%q", c.name, gotType, gotID, c.wantType, c.wantID)
		}
	}
}

// TestStepSeats_OneSeatForTheStepsApprover: until parallel steps (S4) a step opens exactly one
// seat — its approver, on its stable id, with the deadline of this visit. A step with no approver
// opens none (the D-6 fallback owns such an instance, as Migration A's backfill does).
func TestStepSeats_OneSeatForTheStepsApprover(t *testing.T) {
	due := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	got := stepSeats(Step{ID: "s2", ApproverType: ApproverRole, ApproverID: "r9"}, &due, SLABasisBusiness)
	if len(got) != 1 {
		t.Fatalf("stepSeats = %+v; want one seat", got)
	}
	s := got[0]
	if s.NodeID != "s2" || s.AssigneeType != ApproverRole || s.AssigneeID != "r9" || s.DueAt == nil || !s.DueAt.Equal(due) || s.DueBasis != SLABasisBusiness {
		t.Fatalf("seat = %+v; want s2 held by role r9, due %v on business", s, due)
	}
	if got := stepSeats(Step{ID: "s1", ApproverType: ApproverUser}, nil, ""); got != nil {
		t.Fatalf("a step with no approver opened %+v; want no seat", got)
	}
}
