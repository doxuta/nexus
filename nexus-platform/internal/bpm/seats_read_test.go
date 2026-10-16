package bpm

import "testing"

// TestAwaitsActor_TrustedSeatsThenProjection (specs/043 S3a2, T060 + "Nợ còn mở" 21): the inbox
// reads the SEATS of an instance, but only the seats that agree with it — on the node the
// instance stands on (per its pin) and, while the instance projects a holder, held by that
// holder. A seat that disagrees was left open by a binary older than S3a (rolling deploy, or a
// rollback on this schema) and is ignored; an instance with no seat it can trust falls back to
// its current_approver_* projection (D-6). A NULL projection is the one place the seats are the
// only source (a pool or several seats project nothing, D-5).
func TestAwaitsActor_TrustedSeatsThenProjection(t *testing.T) {
	def := &Definition{Steps: []Step{
		{ID: "s1", ApproverType: ApproverUser, ApproverID: "u1"},
		{ID: "s2", ApproverType: ApproverRole, ApproverID: "r2"},
	}}
	inst := func(step int, at ApproverType, id string) *Instance {
		return &Instance{Status: StatusPending, CurrentStep: step, CurrentApproverType: at, CurrentApproverID: id, Definition: def}
	}
	userSeat := func(node, id string) Seat { return Seat{NodeID: node, AssigneeType: ApproverUser, AssigneeID: id} }
	roleSeat := func(node, id string) Seat { return Seat{NodeID: node, AssigneeType: ApproverRole, AssigneeID: id} }
	type who struct {
		user  string
		roles []string
		want  bool
	}
	cases := []struct {
		name  string
		inst  *Instance
		seats []Seat
		who   []who
	}{
		{"seat agrees with the projection", inst(0, ApproverUser, "u1"), []Seat{userSeat("s1", "u1")},
			[]who{{"u1", nil, true}, {"u2", nil, false}}},
		{"seatless instance falls back to the projection", inst(0, ApproverUser, "u1"), nil,
			[]who{{"u1", nil, true}, {"u2", nil, false}}},
		{"seat left on a step the instance has left is not trusted", inst(1, ApproverRole, "r2"), []Seat{userSeat("s1", "u1")},
			[]who{{"u1", nil, false}, {"u5", []string{"r2"}, true}}},
		{"seat whose holder the projection no longer names is not trusted", inst(0, ApproverUser, "u3"), []Seat{userSeat("s1", "u1")},
			[]who{{"u1", nil, false}, {"u3", nil, true}}},
		{"NULL projection: the seat on the current node is the source", inst(0, "", ""), []Seat{userSeat("s1", "u1")},
			[]who{{"u1", nil, true}, {"u2", nil, false}}},
		{"NULL projection: a seat on another node shows nobody", inst(0, "", ""), []Seat{userSeat("s2", "u1")},
			[]who{{"u1", nil, false}}},
		{"NULL projection: every seat of the current visit", inst(0, "", ""), []Seat{userSeat("s1", "u1"), roleSeat("s1", "r2")},
			[]who{{"u1", nil, true}, {"u5", []string{"r2"}, true}, {"u9", []string{"r9"}, false}}},
		{"blank ids never match", inst(0, "", ""), []Seat{roleSeat("s1", "")},
			[]who{{"", []string{""}, false}}},
	}
	for _, c := range cases {
		for _, w := range c.who {
			if got := AwaitsActor(c.inst, c.seats, w.user, w.roles, nil); got != w.want {
				t.Errorf("%s: AwaitsActor(user %q roles %v) = %v; want %v", c.name, w.user, w.roles, got, w.want)
			}
		}
	}
}

// TestSeatDriftOf_ClassifiesPendingInstances (specs/043 S3a2, T063): the Doctor names the pending
// instances whose inbox still rests on the projection fallback — no open seat at all, or open
// seats none of which agrees with the instance. When it counts zero the fallback can go.
func TestSeatDriftOf_ClassifiesPendingInstances(t *testing.T) {
	def := &Definition{Steps: []Step{{ID: "s1", ApproverType: ApproverUser, ApproverID: "u1"}}}
	pending := &Instance{Status: StatusPending, CurrentApproverType: ApproverUser, CurrentApproverID: "u1", Definition: def}
	cases := []struct {
		name  string
		seats []Seat
		want  SeatDrift
	}{
		{"trusted seat", []Seat{{NodeID: "s1", AssigneeType: ApproverUser, AssigneeID: "u1"}}, ""},
		{"no open seat", nil, SeatDriftMissing},
		{"seat on another node", []Seat{{NodeID: "s9", AssigneeType: ApproverUser, AssigneeID: "u1"}}, SeatDriftMismatch},
		{"seat held by someone else", []Seat{{NodeID: "s1", AssigneeType: ApproverUser, AssigneeID: "u2"}}, SeatDriftMismatch},
	}
	for _, c := range cases {
		if got := seatDriftOf(pending, c.seats); got != c.want {
			t.Errorf("%s: seatDriftOf = %q; want %q", c.name, got, c.want)
		}
	}
}
