//go:build integration

// specs/058 — field_update following a relation, END-TO-END on a real MySQL (wfWire: RecordService →
// AsyncDispatcher → WorkflowRecordMutator → dynrepo). The unit tests prove each half with fakes; this
// proves the halves meet: a rule saved through the admin path, fired by a NON-ADMIN user's save,
// writes the related record the user cannot edit themselves — and nothing outside the tenant, and
// no rule of the related entity, is touched by it.
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -p 4 ./internal/services/ -run WorkflowRelated
package services

import (
	"context"
	"testing"
	"time"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/workflow"
)

func TestWorkflowRelated_PaidInvoiceBooksTheAppointment(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const admin = "11111111-0000-0000-0000-00000000a001"
	const rep = "11111111-0000-0000-0000-00000000a002"

	st.createEntity(t, ctx, tenant, "rel_slots", []FieldInput{
		{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{"values": []any{"booked", "paid"}}},
		{Name: "note", Label: "Ghi chú", FieldType: "text"},
	})
	st.createEntity(t, ctx, tenant, "rel_bills", []FieldInput{
		{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{"values": []any{"unpaid", "paid"}}},
		{Name: "slot", Label: "Lịch hẹn", FieldType: "relation", Options: map[string]any{"relatedEntity": "rel_slots"}},
	})

	// The rule under test, saved through the production admin path (validation + cache invalidate).
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "paid-books-slot", EntityName: "rel_bills", Trigger: "after_update",
		Rules:   []WorkflowRuleInput{{Field: "status", Op: "changedTo", Value: "paid"}},
		Actions: []WorkflowActionInput{{Type: "field_update", Via: "slot", Field: "status", Value: "paid"}},
		ActorID: admin,
	}); err != nil {
		t.Fatalf("CreateWorkflow(related field_update): %v", err)
	}
	// #330: a rule on the RELATED entity that stamps itself on every update. The related write now
	// emits after_update (Depth+1), so it MUST fire — "lịch hẹn Hoàn tất ⇒ làm gì đó".
	if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
		Name: "slot-stamps-itself", EntityName: "rel_slots", Trigger: "after_update",
		Actions: []WorkflowActionInput{{Type: "field_update", Field: "note", Value: "rule of the related entity fired"}},
		ActorID: admin,
	}); err != nil {
		t.Fatalf("CreateWorkflow(loop bait): %v", err)
	}

	// The appointment belongs to the admin; the rep only owns the invoice.
	slotID, err := st.recordSvc.Create(ctx, RecordScope{IsAdmin: true}, tenant, admin, "rel_slots", map[string]any{"status": "booked"})
	if err != nil {
		t.Fatalf("Create slot: %v", err)
	}
	repScope := RecordScope{UserID: rep, AccessLevel: "owner"}
	billID, err := st.recordSvc.Create(ctx, repScope, tenant, rep, "rel_bills", map[string]any{"status": "unpaid", "slot": slotID})
	if err != nil {
		t.Fatalf("rep creates the invoice: %v", err)
	}
	// Precondition: the rep cannot edit the appointment by hand — that is why the rule exists.
	if err := st.recordSvc.Update(ctx, repScope, tenant, "rel_slots", slotID, map[string]any{"status": "paid"}, nil); err == nil {
		t.Fatalf("precondition: the rep could edit the admin's appointment directly — the test proves nothing")
	}

	// A tenant-B row in the same physical table, and a SECOND tenant-A invoice whose relation was
	// forced (straight through the repository, past the write-path FK check) to point at it.
	store := dynrepo.New(st.sdb)
	foreignID, err := store.InsertRecord(ctx, "rel_slots", o3aTenantB, admin, map[string]any{"status": "booked"}, nil)
	if err != nil {
		t.Fatalf("insert tenant-B slot: %v", err)
	}
	crossBill, err := st.recordSvc.Create(ctx, repScope, tenant, rep, "rel_bills", map[string]any{"status": "unpaid"})
	if err != nil {
		t.Fatalf("rep creates the second invoice: %v", err)
	}
	if err := store.UpdateRecord(ctx, "rel_bills", tenant, crossBill, rep, dynrepo.AdminScope(), map[string]any{"slot": foreignID}, nil, nil); err != nil {
		t.Fatalf("force the cross-tenant link: %v", err)
	}

	// The rep marks both invoices paid.
	for _, id := range []string{billID, crossBill} {
		if err := st.recordSvc.Update(ctx, repScope, tenant, "rel_bills", id, map[string]any{"status": "paid"}, nil); err != nil {
			t.Fatalf("rep marks invoice %s paid: %v", id, err)
		}
	}
	// The cascade runs on the dispatcher worker after the user's save returns: wait for it (a
	// drain would close the dispatcher and drop the nested event).
	waitFor(t, func() bool {
		var n int
		_ = st.sdb.Get(&n, "SELECT COUNT(*) FROM obj_rel_slots WHERE tenant_id = ? AND id = ? AND JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.note')) = ?",
			tenant, slotID, "rule of the related entity fired")
		return n == 1
	})
	st.drain(t)

	var slot struct {
		Status     *string `db:"status"`
		Note       *string `db:"note"`
		ModifiedBy *string `db:"modified_by"`
	}
	if err := st.sdb.Get(&slot, "SELECT JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.status')) AS status, "+
		"JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.note')) AS note, modified_by FROM obj_rel_slots WHERE tenant_id = ? AND id = ?",
		tenant, slotID); err != nil {
		t.Fatalf("read slot: %v", err)
	}
	if slot.Status == nil || *slot.Status != "paid" {
		t.Fatalf("slot status = %v; want \"paid\" — the rule did not write the related record", deref(slot.Status))
	}
	if slot.ModifiedBy == nil || *slot.ModifiedBy != workflow.SystemActorID {
		t.Errorf("slot modified_by = %v; want the engine's system actor %s", deref(slot.ModifiedBy), workflow.SystemActorID)
	}
	if slot.Note == nil || *slot.Note != "rule of the related entity fired" {
		t.Errorf("slot note = %v; want the related entity's own rule to have fired on the automation write (#330)", deref(slot.Note))
	}

	var foreignStatus string
	if err := st.sdb.Get(&foreignStatus, "SELECT JSON_UNQUOTE(JSON_EXTRACT(`data`, '$.status')) FROM obj_rel_slots WHERE tenant_id = ? AND id = ?",
		o3aTenantB, foreignID); err != nil {
		t.Fatalf("read tenant-B slot: %v", err)
	}
	if foreignStatus != "booked" {
		t.Fatalf("tenant-B slot status = %q; want \"booked\" — a tenant-A rule wrote another tenant's record", foreignStatus)
	}
}

func deref(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// waitFor polls cond for up to 5s — the cascade hops through the async worker.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within 5s")
}

// #330: four rules on one self-related entity form a ring (x⇒peer p, p⇒peer q, q⇒peer r, r⇒peer x),
// every write changing a value, so only the depth guard can end it. It must stop, leave exactly
// maxDepth+1 matched runs (depth 0..5), and say why on the last one.
func TestWorkflowRelated_RingStopsAtMaxChainDepth(t *testing.T) {
	st := wfWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	const admin = "11111111-0000-0000-0000-00000000a001"

	st.createEntity(t, ctx, tenant, "rel_nodes", []FieldInput{
		{Name: "state", Label: "Trạng thái", FieldType: "text"},
		{Name: "peer", Label: "Đối ứng", FieldType: "relation", Options: map[string]any{"relatedEntity": "rel_nodes"}},
	})
	for _, step := range [][2]string{{"x", "p"}, {"p", "q"}, {"q", "r"}, {"r", "x"}} {
		if _, err := st.workflowSvc.CreateWorkflow(ctx, tenant, CreateWorkflowInput{
			Name: "ring-" + step[0], EntityName: "rel_nodes", Trigger: "after_update",
			Rules:   []WorkflowRuleInput{{Field: "state", Op: "changedTo", Value: step[0]}},
			Actions: []WorkflowActionInput{{Type: "field_update", Via: "peer", Field: "state", Value: step[1]}},
			ActorID: admin,
		}); err != nil {
			t.Fatalf("CreateWorkflow(%s): %v", step[0], err)
		}
	}
	scope := wfAdmin(admin)
	n1, err := st.recordSvc.Create(ctx, scope, tenant, admin, "rel_nodes", map[string]any{"state": "start"})
	if err != nil {
		t.Fatal(err)
	}
	n2, err := st.recordSvc.Create(ctx, scope, tenant, admin, "rel_nodes", map[string]any{"state": "start", "peer": n1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.recordSvc.Update(ctx, scope, tenant, "rel_nodes", n1, map[string]any{"peer": n2}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.recordSvc.Update(ctx, scope, tenant, "rel_nodes", n1, map[string]any{"state": "x"}, nil); err != nil {
		t.Fatal(err)
	}
	// Wait for the ring to settle: the matched-run count stops moving.
	matched := func() int {
		var n int
		_ = st.sdb.Get(&n, "SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND status = 'success'", tenant)
		return n
	}
	waitFor(t, func() bool { return matched() >= 6 })
	time.Sleep(500 * time.Millisecond) // a runaway ring would keep adding rows
	if got := matched(); got != 6 {
		t.Fatalf("matched runs = %d; want exactly 6 (depth 0..5) — the ring did not stop at WORKFLOW_MAX_CHAIN_DEPTH", got)
	}
	st.drain(t)
	var capped int
	if err := st.sdb.Get(&capped, "SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND status = 'success' AND CAST(actions_result AS CHAR) LIKE ?",
		tenant, "%độ sâu tối đa%"); err != nil {
		t.Fatal(err)
	}
	if capped != 1 {
		t.Errorf("runs naming the depth cap = %d; want exactly 1 (the last hop)", capped)
	}
}
