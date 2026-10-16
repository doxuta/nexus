package services

// specs/043 S6a — routing: the lookups behind a rule-based holder (BPMRouter), the config-time
// validators (shape + references), the process options, and team membership on the actor. The SQL
// side (tenant-pinned team membership, the seats a team pool opens) is proven on MySQL in
// bpm_routing_integration_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
)

// ---- BPMRouter ------------------------------------------------------------------------------

type routeRecords struct {
	rec  *dynrepo.Record
	err  error
	seen []string // tenant|entity|id of each read
}

func (f *routeRecords) GetRecord(_ context.Context, entity, tenantID, id string, scope dynrepo.RowScope) (*dynrepo.Record, error) {
	f.seen = append(f.seen, tenantID+"|"+entity+"|"+id)
	if !scope.IsAdmin {
		return nil, errors.New("routing must read as the system, not under a person's row scope")
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.rec, nil
}

type routeUsers struct {
	active map[string]bool // tenant|user
	err    error
}

func (f routeUsers) UserActive(_ context.Context, tenantID, userID string) (bool, error) {
	return f.active[tenantID+"|"+userID], f.err
}

func routeIn(submitter string) bpm.RouteInput {
	return bpm.RouteInput{TenantID: "t1", EntityName: "crm_quotes", RecordID: "q1", Submitter: submitter}
}

// T109 — the field names the holder: the user the field points at, or the record's owner.
func TestRouting_FieldOwner(t *testing.T) {
	recs := &routeRecords{rec: &dynrepo.Record{OwnerID: "u-owner", Data: map[string]any{"assigned_to": "u-field"}}}
	users := routeUsers{active: map[string]bool{"t1|u-owner": true, "t1|u-field": true}}
	r := NewBPMRouter(recs, users, nil)

	for _, c := range []struct{ field, want string }{{bpm.OwnerField, "u-owner"}, {"assigned_to", "u-field"}} {
		h, ok, err := r.Resolve(context.Background(), routeIn("u-sender"), bpm.Assignment{Type: bpm.AssignField, Field: c.field})
		if err != nil || !ok || h.Type != bpm.ApproverUser || h.ID != c.want {
			t.Fatalf("field %s: %+v %v %v; want user %s", c.field, h, ok, err, c.want)
		}
	}
	if len(recs.seen) != 2 || recs.seen[0] != "t1|crm_quotes|q1" {
		t.Fatalf("record reads %v; want tenant-pinned reads of crm_quotes/q1", recs.seen)
	}
}

// T109 — an empty field, a ghost, a person of another tenant, a value that is not a user id and a
// vanished record all mean "nobody" (not an error): the engine then uses the fallback.
func TestRouting_FieldEmptyUsesFallback(t *testing.T) {
	users := routeUsers{active: map[string]bool{"t1|u-ok": true, "t2|u-other-tenant": true}}
	cases := []struct {
		name string
		rec  *dynrepo.Record
		err  error
	}{
		{"empty field", &dynrepo.Record{Data: map[string]any{"assigned_to": ""}}, nil},
		{"field missing", &dynrepo.Record{Data: map[string]any{}}, nil},
		{"user left the company", &dynrepo.Record{Data: map[string]any{"assigned_to": "u-gone"}}, nil},
		{"user of ANOTHER tenant", &dynrepo.Record{Data: map[string]any{"assigned_to": "u-other-tenant"}}, nil},
		{"not a string", &dynrepo.Record{Data: map[string]any{"assigned_to": 42}}, nil},
		{"record deleted", nil, dynrepo.ErrRecordNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewBPMRouter(&routeRecords{rec: c.rec, err: c.err}, users, nil)
			h, ok, err := r.Resolve(context.Background(), routeIn("u-sender"), bpm.Assignment{Type: bpm.AssignField, Field: "assigned_to"})
			if err != nil || ok {
				t.Fatalf("got %+v ok=%v err=%v; want nobody and no error", h, ok, err)
			}
		})
	}
	// ...but a store that cannot be read is an ERROR (the caller must not mistake an outage for "nobody")
	r := NewBPMRouter(&routeRecords{err: errors.New("db down")}, users, nil)
	if _, _, err := r.Resolve(context.Background(), routeIn(""), bpm.Assignment{Type: bpm.AssignField, Field: "assigned_to"}); err == nil {
		t.Fatal("a record store failure was reported as 'nobody found'")
	}
	r = NewBPMRouter(&routeRecords{rec: &dynrepo.Record{Data: map[string]any{"assigned_to": "u-ok"}}}, routeUsers{err: errors.New("db down")}, nil)
	if _, _, err := r.Resolve(context.Background(), routeIn(""), bpm.Assignment{Type: bpm.AssignField, Field: "assigned_to"}); err == nil {
		t.Fatal("a user directory failure was reported as 'nobody found'")
	}
}

// T110 — the submitter holds the step; with no known submitter (an API key, a rule) the record's
// owner does; with neither, nobody (the engine falls back to options.fallbackAssignee, then stuck).
func TestRouting_SubmitterEmptyFallsBackToOwnerThenStuck(t *testing.T) {
	users := routeUsers{active: map[string]bool{"t1|u-sender": true, "t1|u-owner": true}}
	sub := bpm.Assignment{Type: bpm.AssignSubmitter}

	r := NewBPMRouter(&routeRecords{rec: &dynrepo.Record{OwnerID: "u-owner"}}, users, nil)
	if h, ok, err := r.Resolve(context.Background(), routeIn("u-sender"), sub); err != nil || !ok || h.ID != "u-sender" {
		t.Fatalf("submitter known: %+v %v %v; want u-sender", h, ok, err)
	}
	if h, ok, err := r.Resolve(context.Background(), routeIn(""), sub); err != nil || !ok || h.ID != "u-owner" {
		t.Fatalf("submitter unknown (API key): %+v %v %v; want the owner u-owner", h, ok, err)
	}
	if h, ok, err := r.Resolve(context.Background(), routeIn("api-key-id-not-a-user"), sub); err != nil || !ok || h.ID != "u-owner" {
		t.Fatalf("submitter is not a user: %+v %v %v; want the owner", h, ok, err)
	}
	noOwner := NewBPMRouter(&routeRecords{rec: &dynrepo.Record{}}, users, nil)
	if h, ok, err := noOwner.Resolve(context.Background(), routeIn(""), sub); err != nil || ok {
		t.Fatalf("no submitter and no owner: %+v %v %v; want nobody", h, ok, err)
	}
}

func TestRouting_StaticAssignmentsAreNotLookedUp(t *testing.T) {
	r := NewBPMRouter(&routeRecords{err: errors.New("must not be read")}, routeUsers{err: errors.New("must not be read")}, nil)
	for _, ty := range []bpm.AssignmentType{bpm.AssignUser, bpm.AssignRole, bpm.AssignTeam, bpm.AssignQueue} {
		if _, ok, err := r.Resolve(context.Background(), routeIn("u"), bpm.Assignment{Type: ty, ID: "x"}); ok || err != nil {
			t.Fatalf("%s: ok=%v err=%v; a named holder is not the router's business", ty, ok, err)
		}
	}
}

// ---- team directory -------------------------------------------------------------------------

type teamsFake struct {
	teams map[string][]string // tenant|user
	err   error
	calls int
}

func (f *teamsFake) UserTeamIDs(_ context.Context, tenantID, userID string) ([]string, error) {
	f.calls++
	return f.teams[tenantID+"|"+userID], f.err
}

func TestTeamIDsOf_FailsClosed(t *testing.T) {
	d := &teamsFake{teams: map[string][]string{"t1|duyet1": {"tm-ban"}}}
	if got := teamIDsOf(context.Background(), d, nil, "t1", "duyet1"); len(got) != 1 || got[0] != "tm-ban" {
		t.Fatalf("teams = %v; want [tm-ban]", got)
	}
	if got := teamIDsOf(context.Background(), d, nil, "t2", "duyet1"); len(got) != 0 {
		t.Fatalf("another tenant's lookup = %v; want none", got)
	}
	if got := teamIDsOf(context.Background(), &teamsFake{err: errors.New("db down")}, nil, "t1", "duyet1"); got != nil {
		t.Fatalf("a failing directory = %v; want none (fail closed)", got)
	}
	if got := teamIDsOf(context.Background(), nil, nil, "t1", "duyet1"); got != nil {
		t.Fatalf("no directory = %v; want none", got)
	}
	if got := teamIDsOf(context.Background(), d, nil, "t1", ""); got != nil || d.calls != 2 {
		t.Fatalf("a blank user must not hit the directory (calls=%d got=%v)", d.calls, got)
	}
}

// The approval surface judges a person by their teams too: withQueueView is the one place the actor
// is completed, so every verb (list, detail, approve, reject, delegate) sees the same membership.
func TestApprovalService_ActorCarriesTeams(t *testing.T) {
	s := NewApprovalService(nil, nil, nil).WithTeamDirectory(&teamsFake{teams: map[string][]string{"t1|duyet1": {"tm-ban"}}})
	a := s.withQueueView(context.Background(), "t1", bpm.Actor{UserID: "duyet1", RoleIDs: []string{"r-mgr"}})
	if got := strings.Join(a.HolderIDs(), ","); got != "r-mgr,tm-ban" {
		t.Fatalf("holder ids = %q; want role then team", got)
	}
	a = s.withQueueView(context.Background(), "t1", bpm.Actor{UserID: "test", RoleIDs: []string{"r-rep"}})
	if got := strings.Join(a.HolderIDs(), ","); got != "r-rep" {
		t.Fatalf("a non-member holds %q; want the role only", got)
	}
}

// ---- config-time validation -----------------------------------------------------------------

func assignStep(a ApprovalAssignmentInput) ApprovalStepInput {
	return ApprovalStepInput{Name: "Duyệt", Assignment: &a}
}

// A linear `steps` body that carries an assignment is stored as a graph (the linear column is what
// a binary older than the slice reads); the assignment survives, trimmed, with a stable id.
func TestValidateApprovalChain_AssignmentStepsLiftToGraph(t *testing.T) {
	_, _, stepsJSON, graphJSON, err := ValidateApprovalChain("P", "manual", "", nil, []ApprovalStepInput{
		{ApproverType: "user", ApproverID: "u-1"},
		assignStep(ApprovalAssignmentInput{Type: " team ", ID: " tm-ban ", Strategy: "pool"}),
	})
	if err != nil {
		t.Fatalf("ValidateApprovalChain: %v", err)
	}
	if string(stepsJSON) != "[]" || len(graphJSON) == 0 {
		t.Fatalf("steps=%s graph=%s; want steps [] beside a graph", stepsJSON, graphJSON)
	}
	if !strings.Contains(string(graphJSON), `"assignment":{"type":"team","id":"tm-ban","strategy":"pool"}`) {
		t.Fatalf("graph %s lost or kept untrimmed the assignment", graphJSON)
	}
}

func TestValidateApprovalChain_AssignmentShape422(t *testing.T) {
	cases := []struct {
		name  string
		step  ApprovalStepInput
		code  string
		field string
	}{
		{"bad type", assignStep(ApprovalAssignmentInput{Type: "boss", ID: "x"}), "APPROVAL_STEP_INVALID", "steps[0].assignment.type"},
		{"no id", assignStep(ApprovalAssignmentInput{Type: "team"}), "APPROVAL_STEP_INVALID", "steps[0].assignment.id"},
		{"field without a field", assignStep(ApprovalAssignmentInput{Type: "field"}), "APPROVAL_STEP_INVALID", "steps[0].assignment.field"},
		{"unknown strategy (round_robin opened in S6b)", assignStep(ApprovalAssignmentInput{Type: "team", ID: "tm", Strategy: "lottery"}), "APPROVAL_STEP_INVALID", "steps[0].assignment.strategy"},
		{"a strategy on a queue is refused", assignStep(ApprovalAssignmentInput{Type: "queue", ID: "q", Strategy: "each_member"}), "APPROVAL_STEP_INVALID", "steps[0].assignment.strategy"},
		{"beside approverType", ApprovalStepInput{ApproverType: "user", ApproverID: "u", Assignment: &ApprovalAssignmentInput{Type: "submitter"}}, "APPROVAL_STEP_INVALID", "steps[0].assignment"},
		{"beside approvers", ApprovalStepInput{Approvers: []ApprovalApproverInput{{Type: "user", ID: "a"}, {Type: "user", ID: "b"}}, Assignment: &ApprovalAssignmentInput{Type: "submitter"}}, "APPROVAL_STEP_INVALID", "steps[0].assignment"},
		{"unknown key inside", assignStep(ApprovalAssignmentInput{Type: "team", ID: "tm", Unsupported: []string{"quorum"}}), "APPROVAL_NODE_KIND_UNSUPPORTED", "steps[0].assignment.quorum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, _, _, err := ValidateApprovalChain("P", "manual", "", nil, []ApprovalStepInput{c.step})
			ve := wantApprovalCode(t, err, c.code)
			if ve.Field != c.field {
				t.Fatalf("field = %q, want %q (%s)", ve.Field, c.field, ve.Msg)
			}
		})
	}
	// a bare buildApprovalStepsJSON caller that skipped the lift must not store one linear
	if _, err := buildApprovalStepsJSON([]ApprovalStepInput{assignStep(ApprovalAssignmentInput{Type: "team", ID: "tm"})}, nil); err == nil {
		t.Fatal("a rule-based step was stored in the linear column")
	}
}

func TestApprovalOptionsJSON(t *testing.T) {
	if b, err := approvalOptionsJSON(nil); b != nil || err != nil {
		t.Fatalf("nil options = %s %v; want none", b, err)
	}
	if b, err := approvalOptionsJSON(&ApprovalOptionsInput{}); b != nil || err != nil {
		t.Fatalf("empty options = %s %v; want none (an empty block is no options)", b, err)
	}
	b, err := approvalOptionsJSON(&ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: " role ", ID: " r-ops "}})
	if err != nil || string(b) != `{"fallbackAssignee":{"type":"role","id":"r-ops"}}` {
		t.Fatalf("options = %s %v", b, err)
	}
	for _, c := range []struct {
		name string
		in   *ApprovalOptionsInput
		code string
	}{
		{"unknown key", &ApprovalOptionsInput{Unsupported: []string{"lane"}}, "APPROVAL_NODE_KIND_UNSUPPORTED"},
		{"bad type", &ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "boss", ID: "x"}}, "APPROVAL_OPTIONS_INVALID"},
		{"no id", &ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "user"}}, "APPROVAL_OPTIONS_INVALID"},
	} {
		_, err := approvalOptionsJSON(c.in)
		wantApprovalCode(t, err, c.code)
	}
}

// ---- references -----------------------------------------------------------------------------

// refEntities is an entity catalog with the fields the field-assignment checks read.
type refEntities struct{ fields []metadata.FieldDefinition }

func (r refEntities) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	return &metadata.EntityDefinition{ID: "e1", TenantID: tenantID, Name: name}, nil
}
func (r refEntities) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	return r.fields, nil
}

func refService(existing map[string]bool) *ApprovalService {
	return NewApprovalService(nil, nil, nil).
		WithEntityChecker(refEntities{fields: []metadata.FieldDefinition{
			{Name: "assigned_to", FieldType: "user"}, {Name: "title", FieldType: "text"}, {Name: "region", FieldType: "picklist"},
		}}).
		WithHolderChecker(func(_ context.Context, tenantID string, kind bpm.ApproverType, id string) (bool, error) {
			return existing[tenantID+"|"+string(kind)+"|"+id], nil
		}).
		WithQueueChecker(func(_ context.Context, tenantID, id string) (bool, error) {
			return existing[tenantID+"|queue|"+id], nil
		})
}

func nodeOf(a ApprovalAssignmentInput) []ApprovalNodeInput {
	return []ApprovalNodeInput{{Kind: "approval", ApprovalStepInput: ApprovalStepInput{Assignment: &a}}}
}

// T113 — a user / role / team / queue that does not exist in THIS tenant is refused at save, at the
// key that names it; one of another tenant is just as missing.
func TestValidate_AssigneeNotFound422(t *testing.T) {
	s := refService(map[string]bool{
		"t1|user|u1": true, "t1|role|r1": true, "t1|team|tm1": true, "t1|queue|q1": true,
		"t2|team|tm-other": true,
	})
	ctx := context.Background()
	for _, ok := range []ApprovalAssignmentInput{{Type: "user", ID: "u1"}, {Type: "role", ID: "r1"}, {Type: "team", ID: "tm1"}, {Type: "queue", ID: "q1"}} {
		if err := s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(ok), "graph.nodes", nil); err != nil {
			t.Fatalf("%s existing: %v", ok.Type, err)
		}
	}
	for _, bad := range []ApprovalAssignmentInput{
		{Type: "user", ID: "u-ghost"}, {Type: "role", ID: "r-ghost"}, {Type: "team", ID: "tm-ghost"}, {Type: "queue", ID: "q-ghost"},
		{Type: "team", ID: "tm-other"}, // exists, but in tenant t2
	} {
		err := s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(bad), "graph.nodes", nil)
		ve := wantApprovalCode(t, err, "APPROVAL_ASSIGNEE_NOT_FOUND")
		if ve.Field != "graph.nodes[0].assignment.id" {
			t.Fatalf("%s %s: field = %q", bad.Type, bad.ID, ve.Field)
		}
	}
	// the fallback assignee is judged by the same rule
	err := s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(ApprovalAssignmentInput{Type: "user", ID: "u1"}), "graph.nodes",
		&ApprovalOptionsInput{FallbackAssignee: &ApprovalApproverInput{Type: "role", ID: "r-ghost"}})
	if ve := wantApprovalCode(t, err, "APPROVAL_ASSIGNEE_NOT_FOUND"); ve.Field != "options.fallbackAssignee.id" {
		t.Fatalf("fallback field = %q", ve.Field)
	}
	// a lifted linear body is located at steps[i]
	err = s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(ApprovalAssignmentInput{Type: "user", ID: "u-ghost"}), "steps", nil)
	if ve := wantApprovalCode(t, err, "APPROVAL_ASSIGNEE_NOT_FOUND"); ve.Field != "steps[0].assignment.id" {
		t.Fatalf("steps field = %q", ve.Field)
	}
}

// T113 — `field` must name a user-type field of the entity (or the owner): a text field would
// "resolve" to a string no user has, a missing one to nothing — both forever "stuck".
func TestValidate_AssigneeFieldMustBeUserType(t *testing.T) {
	s := refService(nil)
	ctx := context.Background()
	for _, ok := range []string{"assigned_to", bpm.OwnerField} {
		if err := s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(ApprovalAssignmentInput{Type: "field", Field: ok}), "graph.nodes", nil); err != nil {
			t.Fatalf("field %q: %v", ok, err)
		}
	}
	for _, bad := range []string{"title", "region", "no_such_field"} {
		err := s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(ApprovalAssignmentInput{Type: "field", Field: bad}), "graph.nodes", nil)
		ve := wantApprovalCode(t, err, "APPROVAL_ASSIGNEE_FIELD_INVALID")
		if ve.Field != "graph.nodes[0].assignment.field" {
			t.Fatalf("field %q: error field = %q", bad, ve.Field)
		}
	}
	// the submitter needs no reference at all
	if err := s.checkRefs(ctx, "t1", "crm_quotes", nodeOf(ApprovalAssignmentInput{Type: "submitter"}), "graph.nodes", nil); err != nil {
		t.Fatalf("submitter: %v", err)
	}
}
