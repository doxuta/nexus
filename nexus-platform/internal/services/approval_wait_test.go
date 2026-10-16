package services

// specs/043 S12a — the wait node at the API: the config the engine will run is accepted, everything
// else is a coded 422 at the key at fault (bpm.CheckWait is the one judge, shared with the engine);
// the tenant half checks the entity's fields. Plus the real working calendar's answer for the case
// the slice is named after (urgent ticket at 16:30 on a Friday ⇒ Monday 08:30).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/workflow"
)

// ticketEntities answers the field lookups of a ticketing entity.
type ticketEntities struct{}

func (ticketEntities) GetEntityByName(_ context.Context, tenantID, name string) (*metadata.EntityDefinition, error) {
	return &metadata.EntityDefinition{ID: "e1", TenantID: tenantID, Name: name}, nil
}

func (ticketEntities) ListFields(context.Context, string, string) ([]metadata.FieldDefinition, error) {
	return []metadata.FieldDefinition{
		{Name: "status", FieldType: "picklist"},
		{Name: "priority", FieldType: "picklist"},
		{Name: "assigned_to", FieldType: "user"},
		{Name: "title", FieldType: "text"},
		{Name: "first_response_due", FieldType: "datetime"},
		{Name: "opened_on", FieldType: "date"},
	}, nil
}

func waitRule(field, op string, v any) ApprovalCriteriaRuleInput {
	return ApprovalCriteriaRuleInput{Field: field, Op: op, Value: v}
}

// firstResponseWait is the ticket process of the slice: wait until the ticket leaves Open, with the
// priority matrix, the due field and the person in assigned_to told on a breach.
func firstResponseWait() ApprovalNodeInput {
	return ApprovalNodeInput{
		ApprovalStepInput: ApprovalStepInput{ID: "phan-hoi", Name: "Phản hồi đầu tiên"}, Kind: "wait",
		Until: &ApprovalConditionInput{Rules: []ApprovalCriteriaRuleInput{waitRule("status", "neq", "Open")}},
		SLA: &ApprovalWaitSLAInput{
			Amount: 4, Unit: "hours",
			Matrix: []ApprovalSLARowInput{
				{When: ApprovalConditionInput{Rules: []ApprovalCriteriaRuleInput{waitRule("priority", "eq", "Khẩn")}}, Amount: 1, Unit: "business_hours"},
			},
			WriteDueTo: "first_response_due",
			Notify:     &ApprovalAssignmentInput{Type: "field", Field: "assigned_to"},
		},
	}
}

func createWait(t *testing.T, mutate func(*ApprovalNodeInput)) (*graphProcRepo, error) {
	t.Helper()
	n := firstResponseWait()
	mutate(&n)
	repo := &graphProcRepo{}
	_, err := NewApprovalService(repo, nil, nil).WithEntityChecker(ticketEntities{}).CreateProcess(context.Background(), "t1",
		CreateApprovalProcessInput{Name: "SLA phiếu", EntityName: "tk_tickets", Trigger: "after_create",
			Graph: &ApprovalGraphInput{Nodes: []ApprovalNodeInput{n, {Kind: "end", Result: "approved"}}}})
	return repo, err
}

func TestCreateProcess_WaitNodeStoredAndRunnable(t *testing.T) {
	repo, err := createWait(t, func(*ApprovalNodeInput) {})
	if err != nil {
		t.Fatalf("a valid wait node refused: %v", err)
	}
	var g bpm.Graph
	if err := json.Unmarshal(repo.created.Graph, &g); err != nil {
		t.Fatalf("stored graph %s: %v", repo.created.Graph, err)
	}
	if g.Nodes[0].Kind != bpm.NodeWait || g.Nodes[0].SLA == nil || g.Nodes[0].SLA.Notify == nil || len(g.Nodes[0].SLA.Matrix) != 1 {
		t.Fatalf("stored node %+v; want a wait node with its matrix and notify", g.Nodes[0])
	}
	if len((&bpm.Process{Graph: &g}).Chain()) != 2 {
		t.Fatal("the stored graph is not runnable by the engine")
	}
}

// TestValidate_WaitSLANotifyRequired (T268): a deadline nobody hears about is not an SLA (D-18).
func TestValidate_WaitSLANotifyRequired(t *testing.T) {
	repo, err := createWait(t, func(n *ApprovalNodeInput) { n.SLA.Notify = nil })
	ve := wantApprovalCode(t, err, "APPROVAL_SLA_NOTIFY_REQUIRED")
	if ve.Field != "graph.nodes[0].sla.notify" {
		t.Fatalf("field %q; want graph.nodes[0].sla.notify", ve.Field)
	}
	if repo.created != nil {
		t.Fatal("a wait deadline with nobody to tell reached the repository")
	}
	// no sla at all is a plain wait: nothing to miss, nobody to tell
	if _, err := createWait(t, func(n *ApprovalNodeInput) { n.SLA = nil }); err != nil {
		t.Fatalf("a wait node without a deadline refused: %v", err)
	}
}

// TestValidate_WaitNotifyFieldMustBeUserType (T268): the notify field must point at a person.
func TestValidate_WaitNotifyFieldMustBeUserType(t *testing.T) {
	_, err := createWait(t, func(n *ApprovalNodeInput) { n.SLA.Notify = &ApprovalAssignmentInput{Type: "field", Field: "title"} })
	ve := wantApprovalCode(t, err, "APPROVAL_ASSIGNEE_FIELD_INVALID")
	if ve.Field != "graph.nodes[0].sla.notify.field" {
		t.Fatalf("field %q; want graph.nodes[0].sla.notify.field", ve.Field)
	}
	if _, err := createWait(t, func(n *ApprovalNodeInput) {
		n.SLA.Notify = &ApprovalAssignmentInput{Type: "field", Field: bpm.OwnerField}
	}); err != nil {
		t.Fatalf("the record owner is always a user: %v", err)
	}
	if _, err := createWait(t, func(n *ApprovalNodeInput) { n.SLA.Notify = &ApprovalAssignmentInput{Type: "submitter"} }); err != nil {
		t.Fatalf("the submitter is a valid notify target: %v", err)
	}
}

// TestWaitNode_WriteDueToDatetimeOnly (T182): a deadline lands only in a datetime field of the entity.
func TestWaitNode_WriteDueToDatetimeOnly(t *testing.T) {
	for name, field := range map[string]string{"text field": "title", "date field": "opened_on", "unknown field": "nope"} {
		t.Run(name, func(t *testing.T) {
			_, err := createWait(t, func(n *ApprovalNodeInput) { n.SLA.WriteDueTo = field })
			ve := wantApprovalCode(t, err, "APPROVAL_SLA_TARGET_FIELD_INVALID")
			if ve.Field != "graph.nodes[0].sla.writeDueTo" {
				t.Fatalf("field %q; want graph.nodes[0].sla.writeDueTo", ve.Field)
			}
		})
	}
}

func TestValidate_WaitConfigShapes(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*ApprovalNodeInput)
		wantCode  string
		wantField string
	}{
		{"until missing", func(n *ApprovalNodeInput) { n.Until = nil }, "APPROVAL_STEP_INVALID", "graph.nodes[0].until"},
		{"until reads an unknown field", func(n *ApprovalNodeInput) { n.Until.Rules[0].Field = "nope" },
			"APPROVAL_FIELD_UNKNOWN", "graph.nodes[0].until.rules[0].field"},
		{"until on the before image", func(n *ApprovalNodeInput) { n.Until.Rules[0] = waitRule("status", "changed", nil) },
			"APPROVAL_CRITERIA_EDGE_OP_UNSUPPORTED", "graph.nodes[0].until"},
		{"matrix reads an unknown field", func(n *ApprovalNodeInput) { n.SLA.Matrix[0].When.Rules[0].Field = "nope" },
			"APPROVAL_FIELD_UNKNOWN", "graph.nodes[0].sla.matrix[0].when.rules[0].field"},
		{"unit unknown", func(n *ApprovalNodeInput) { n.SLA.Matrix[0].Unit = "weeks" }, "APPROVAL_SLA_UNIT_INVALID", "graph.nodes[0].sla.matrix[0].unit"},
		{"base unit unknown", func(n *ApprovalNodeInput) { n.SLA.Unit = "" }, "APPROVAL_SLA_UNIT_INVALID", "graph.nodes[0].sla.unit"},
		{"amount too large", func(n *ApprovalNodeInput) { n.SLA.Matrix[0].Amount = bpm.MaxWaitAmount + 1 },
			"APPROVAL_SLA_AMOUNT_TOO_LARGE", "graph.nodes[0].sla.matrix[0].amount"},
		{"key of a later slice", func(n *ApprovalNodeInput) { n.SLA.Unsupported = []string{"bogusKey"} },
			"APPROVAL_NODE_KIND_UNSUPPORTED", "graph.nodes[0].sla.bogusKey"},
		{"notify names a queue", func(n *ApprovalNodeInput) { n.SLA.Notify = &ApprovalAssignmentInput{Type: "queue", ID: "q1"} },
			"APPROVAL_STEP_INVALID", "graph.nodes[0].sla.notify.type"},
		{"a person's deadline on the wait node", func(n *ApprovalNodeInput) { n.SLASeconds = 60 }, "APPROVAL_STEP_INVALID", "graph.nodes[0].slaSeconds"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, err := createWait(t, c.mutate)
			ve := wantApprovalCode(t, err, c.wantCode)
			if ve.Field != c.wantField {
				t.Fatalf("field %q; want %q (%s)", ve.Field, c.wantField, ve.Msg)
			}
			if repo.created != nil {
				t.Fatal("an invalid wait node reached the repository")
			}
		})
	}
	// until/sla belong to a wait node only
	repo := &graphProcRepo{}
	_, err := NewApprovalService(repo, nil, nil).CreateProcess(context.Background(), "t1", CreateApprovalProcessInput{
		Name: "P", EntityName: "tk_tickets", Trigger: "manual",
		Graph: &ApprovalGraphInput{Nodes: []ApprovalNodeInput{{Kind: "end", Result: "approved", Until: firstResponseWait().Until}}},
	})
	if ve := wantApprovalCode(t, err, "APPROVAL_STEP_INVALID"); ve.Field != "graph.nodes[0].until" {
		t.Fatalf("field %q; want graph.nodes[0].until", ve.Field)
	}
}

// fakeRules serves a fixed automation-rule list.
type fakeRules []workflow.WorkflowRecord

func (f fakeRules) List(context.Context, string) ([]workflow.WorkflowRecord, error) { return f, nil }

// TestValidate_DueFieldConflictWarning (T182): a live set_deadline rule of the same entity that writes
// the field the wait node writes is a warning — two writers of one promise overwrite each other.
func TestValidate_DueFieldConflictWarning(t *testing.T) {
	setDeadline := json.RawMessage(`[{"type":"set_deadline","field":"first_response_due","offset":{"amount":4,"unit":"hours"}}]`)
	other := json.RawMessage(`[{"type":"set_deadline","field":"resolution_due","offset":{"amount":1,"unit":"days"}}]`)
	rules := fakeRules{
		{Name: "Hạn phản hồi", EntityName: "tk_tickets", IsActive: true, Actions: setDeadline},
		{Name: "Tắt rồi", EntityName: "tk_tickets", IsActive: false, Actions: setDeadline},
		{Name: "Thực thể khác", EntityName: "crm_quotes", IsActive: true, Actions: setDeadline},
		{Name: "Trường khác", EntityName: "tk_tickets", IsActive: true, Actions: other},
	}
	g := &ApprovalGraphInput{Nodes: []ApprovalNodeInput{firstResponseWait()}}
	svc := NewApprovalService(&graphProcRepo{}, nil, nil).WithDueWriters(rules)
	got, err := svc.DueFieldConflicts(context.Background(), "t1", "tk_tickets", g)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("warnings %v; want exactly one (the live rule of this entity writing first_response_due)", got)
	}
	if got, _ := NewApprovalService(&graphProcRepo{}, nil, nil).DueFieldConflicts(context.Background(), "t1", "tk_tickets", g); got != nil {
		t.Fatalf("unwired lister warned: %v", got)
	}
	noDue := firstResponseWait()
	noDue.SLA.WriteDueTo = ""
	if got, _ := svc.DueFieldConflicts(context.Background(), "t1", "tk_tickets", &ApprovalGraphInput{Nodes: []ApprovalNodeInput{noDue}}); got != nil {
		t.Fatalf("a wait node that writes no field warned: %v", got)
	}
}

// TestWaitNode_UrgentFriday1630_RealCalendar (T181, calendar half): on the default Mon–Fri 08:00–17:00
// calendar one WORKING hour from Friday 16:30 is Monday 08:30 — the figure the matrix row "Khẩn" gives
// the ticket (the engine asks the calendar for exactly one hour: bpm.TestWaitNode_PriorityMatrix_BusinessHours).
func TestWaitNode_UrgentFriday1630_RealCalendar(t *testing.T) {
	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 10, 9, 16, 30, 0, 0, ict) // a Friday
	due, basis, err := mustCalc(t, &fakeTimerCalendars{def: vnDefaultCalendar()}).DueAt(context.Background(), "t1", "", from, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 12, 8, 30, 0, 0, ict)
	if !due.Equal(want) || basis != bpm.SLABasisBusiness {
		t.Fatalf("due %v (%s); want %v (business)", due, basis, want)
	}
}

// TestApprovalDetail_WaitMetClosesTheStretch (T270 read side): the record answered Monday 08:45 for a
// ticket opened Friday 16:30 — the wait node's stretch reads "1 giờ 15 phút làm việc" and the customer
// face still carries the public wait_met line (the waiting mark stays the Nexus team's).
func TestApprovalDetail_WaitMetClosesTheStretch(t *testing.T) {
	store := &fakeTimerCalendars{def: vnDefaultCalendar()}
	opened, answered := ictTime(t, 2026, 10, 9, 16, 30).UTC(), ictTime(t, 2026, 10, 12, 8, 45).UTC()
	inst := &bpm.Instance{
		ID: "ai-wait", TenantID: "t1", ProcessID: "p-w", EntityName: "tk_tickets", RecordID: "tk1",
		Status: bpm.StatusApproved, CurrentStep: 0, CreatedBy: "submitter", CreatedAt: opened,
		History: []bpm.HistoryEntry{
			{Step: 0, Action: bpm.HistoryWaiting, Node: "phan-hoi", StepName: "Phản hồi đầu tiên", At: opened},
			{Step: 0, Action: bpm.HistoryWaitMet, Node: "phan-hoi", StepName: "Phản hồi đầu tiên", At: answered},
			{Step: 1, Action: bpm.HistoryEnded, Node: "xong", At: answered},
		},
		Definition: &bpm.Definition{Steps: []bpm.Step{}, Graph: &bpm.Graph{Nodes: []bpm.Node{
			{Step: bpm.Step{ID: "phan-hoi", Name: "Phản hồi đầu tiên"}, Kind: bpm.NodeWait, Until: waitUntilOpen()},
			{Step: bpm.Step{ID: "xong"}, Kind: bpm.NodeEnd, Result: bpm.ResultApproved},
		}}},
	}
	svc := clockSvc(t, inst, store, &fakeApprovalRecordReader{authzLevel: "owner"}, answered.Add(time.Hour))
	got, err := svc.GetApprovalDetail(context.Background(),
		RecordScope{UserID: "submitter", AccessLevel: string(dynrepo.AccessNone)}, "t1", "ai-wait", bpm.Actor{UserID: "submitter"})
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	d := got.Durations
	if d == nil || len(d.PerNode) == 0 {
		t.Fatalf("durations %+v; want the wait node's stretch", d)
	}
	n := d.PerNode[0]
	if n.Node != "phan-hoi" || n.EndedAt == nil || secs(n.BusinessSeconds) != "1h15m0s" {
		t.Fatalf("perNode[0] = %+v (business %s); want phan-hoi closed at the answer, 1h15m0s", n, secs(n.BusinessSeconds))
	}
	var met, waiting bool
	for _, h := range got.History {
		met = met || h.Action == bpm.HistoryWaitMet
		waiting = waiting || h.Action == bpm.HistoryWaiting
	}
	if !met || waiting {
		t.Fatalf("history shown to the submitter: wait_met=%v waiting=%v; want the public line only", met, waiting)
	}
}

func waitUntilOpen() *workflow.Condition {
	return &workflow.Condition{Rules: []workflow.Rule{{Field: "status", Op: "neq", Value: "Open"}}}
}
