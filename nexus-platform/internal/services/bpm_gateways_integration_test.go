//go:build integration

// specs/043 S7 — gateways, auto nodes and end nodes over a freshly-migrated MySQL, through the
// production composition (record service → arming engine → runner → real record writes):
//   - "< 5 triệu tự duyệt / < 50 triệu duyet1 / ≥ 50 triệu duyet1 rồi duyet3" walks as specified and
//     the auto node's field really lands on the record;
//   - an auto node the record refuses (unique clash) stalls the hồ sơ with a readable error, the
//     admin list shows it, "Thử lại" before the fix stays stalled and after it advances;
//   - a hồ sơ left standing on a machine node (a crash) is picked up by the bpm_continue job, and a
//     second delivery changes nothing;
//   - an auto chain that lands on a person's step with a deadline parks exactly one bpm_sla_due job
//     for the seq it stands at (the T290 invariant for the new transition).
//
// Run with:
//
//	TEST_DATABASE_URL=... go test -tags=integration -race ./internal/services/ -run 'TestBPMRoute|TestAutoChain'
package services

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nexus/platform/internal/bpm"
	"nexus/platform/internal/jobs"
)

const (
	rtSubmitter = "11111111-0000-0000-0000-0000000000a1"
	rtMgr       = "22222222-0000-0000-0000-0000000000a2"
	rtDir       = "33333333-0000-0000-0000-0000000000a3"
)

func (s *bpmStack) routedEntity(t *testing.T, ctx context.Context, tenant, name string) {
	t.Helper()
	s.createEntity(t, ctx, tenant, name, []FieldInput{
		{Name: "amount", Label: "Giá trị", FieldType: "number"},
		{Name: "name", Label: "Tên", FieldType: "text"},
		{Name: "code", Label: "Mã", FieldType: "text", IsUnique: true},
		{Name: "status", Label: "Trạng thái", FieldType: "picklist", Options: map[string]any{"values": []any{"Nháp", "Chấp nhận"}}},
	})
}

func crit(field, op string, v any) ApprovalCriteriaRuleInput {
	return ApprovalCriteriaRuleInput{Field: field, Op: op, Value: v}
}

// quoteRouting is the spec's process: under 5 million approves itself (writing status), under 50
// million goes to rtMgr, from 50 million rtMgr then rtDir.
func quoteRouting() *ApprovalGraphInput {
	mgr := humanNode("mgr", "big")
	mgr.ApproverID, mgr.SLASeconds = rtMgr, 3600
	dir := humanNode("dir", "done")
	dir.ApproverID = rtDir
	return &ApprovalGraphInput{Start: "route", Nodes: []ApprovalNodeInput{
		gwNode("route", "mgr",
			branchTo("tu-duyet", "auto-ok", crit("amount", "lt", 5e6)),
			branchTo("cap-1", "mgr", crit("amount", "lt", 5e7))),
		autoNode("auto-ok", "done", ApprovalSetInput{Field: "status", Value: "Chấp nhận"}),
		mgr,
		gwNode("big", "done", branchTo("tren-50tr", "dir", crit("amount", "gte", 5e7))),
		dir,
		endNode("done", "approved"),
	}}
}

func (s *bpmStack) routedProcess(t *testing.T, ctx context.Context, tenant, entity, trigger string, g *ApprovalGraphInput, out *ApprovalOutcomeInput) {
	t.Helper()
	if _, err := s.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: entity + "-routed", EntityName: entity, Trigger: trigger, Graph: g, Outcome: out,
		IsActive: boolPtr(true), ActorID: rtSubmitter,
	}); err != nil {
		t.Fatalf("CreateProcess (routed): %v", err)
	}
}

// lastInstanceOf is the newest hồ sơ of a record, any status.
func (s *bpmStack) lastInstanceOf(t *testing.T, tenant, entity, recordID string) *bpm.Instance {
	t.Helper()
	var id string
	if err := s.sdb.Get(&id, `SELECT id FROM sys_approval_instances WHERE tenant_id = ? AND entity_name = ? AND record_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		tenant, entity, recordID); err != nil {
		t.Fatalf("locate instance of %s/%s: %v", entity, recordID, err)
	}
	inst, err := s.repo.GetInstance(context.Background(), tenant, id)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return inst
}

func (s *bpmStack) routedField(t *testing.T, tenant, entity, recordID, field string) any {
	t.Helper()
	rec, err := s.recordSvc.Get(context.Background(), bpmAdmin(rtSubmitter), tenant, entity, recordID, nil, nil)
	if err != nil {
		t.Fatalf("Get record: %v", err)
	}
	return rec.Data[field]
}

func historyActions(inst *bpm.Instance) []string {
	out := make([]string, len(inst.History))
	for i, h := range inst.History {
		out[i] = h.Action
	}
	return out
}

// TestBPMRoute_QuoteTiers: the Independent Test of the story, on real rows.
func TestBPMRoute_QuoteTiers(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant, entity = o3aTenantA, "rt_quotes"
	st.routedEntity(t, ctx, tenant, entity)
	st.routedProcess(t, ctx, tenant, entity, "after_create", quoteRouting(), &ApprovalOutcomeInput{StatusField: "status", ApprovedValue: "Chấp nhận"})

	mk := func(amount float64) string {
		id, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity, map[string]any{"amount": amount, "status": "Nháp"})
		if err != nil {
			t.Fatalf("create record %v: %v", amount, err)
		}
		return id
	}
	small, mid, big := mk(3e6), mk(3e7), mk(8e7)
	st.drain(t)

	// 3 triệu: approved by the rules; the auto node's value is really on the record.
	si := st.lastInstanceOf(t, tenant, entity, small)
	if si.Status != bpm.StatusApproved || si.Context.Gateway()["route"] != "tu-duyet" {
		t.Fatalf("3 triệu: %s via %v; want approved via tu-duyet", si.Status, si.Context.Gateway())
	}
	if got := st.routedField(t, tenant, entity, small, "status"); got != "Chấp nhận" {
		t.Fatalf("3 triệu: record status %v; want Chấp nhận written by the auto node", got)
	}
	want := []string{bpm.HistoryGateway, bpm.HistoryAuto, bpm.HistoryEnded}
	if got := historyActions(si); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("3 triệu history %v; want %v", got, want)
	}

	// 30 triệu: only rtMgr has a seat; one approval ends it and the outcome lands.
	mi := st.lastInstanceOf(t, tenant, entity, mid)
	if mi.Status != bpm.StatusPending || mi.CurrentApproverID != rtMgr || mi.SLADueAt == nil {
		t.Fatalf("30 triệu: %s approver %q due %v; want pending on rtMgr with the step's deadline", mi.Status, mi.CurrentApproverID, mi.SLADueAt)
	}
	if got := st.routedField(t, tenant, entity, mid, "status"); got != "Nháp" {
		t.Fatalf("30 triệu: status %v before any approval; want untouched", got)
	}
	if _, err := st.approval.Decide(ctx, tenant, mi.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: rtDir}); err == nil {
		t.Fatal("rtDir approved rtMgr's step — authority leak")
	}
	done, err := st.approval.Decide(ctx, tenant, mi.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: rtMgr})
	if err != nil || done.Status != bpm.StatusApproved {
		t.Fatalf("rtMgr approves 30 triệu: %v %v; want approved with no director step", err, done)
	}
	if got := st.routedField(t, tenant, entity, mid, "status"); got != "Chấp nhận" {
		t.Fatalf("30 triệu: status %v after approval; want the outcome write-back", got)
	}

	// 80 triệu: rtMgr, then rtDir.
	bi := st.lastInstanceOf(t, tenant, entity, big)
	if bi.CurrentApproverID != rtMgr {
		t.Fatalf("80 triệu starts with %q; want rtMgr", bi.CurrentApproverID)
	}
	after, err := st.approval.Decide(ctx, tenant, bi.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: rtMgr})
	if err != nil || after.Status != bpm.StatusPending || after.CurrentApproverID != rtDir {
		t.Fatalf("80 triệu after rtMgr: %v %v; want pending on rtDir", err, after)
	}
	if after.Context.Gateway()["big"] != "tren-50tr" {
		t.Fatalf("path %v; want big=tren-50tr", after.Context.Gateway())
	}
	end, err := st.approval.Decide(ctx, tenant, bi.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: rtDir})
	if err != nil || end.Status != bpm.StatusApproved {
		t.Fatalf("rtDir approves 80 triệu: %v %v", err, end)
	}
	// Nobody can decide a finished hồ sơ, nor retry one that is not stuck.
	if _, err := st.approval.RetryApproval(ctx, tenant, bi.ID, "", bpm.Actor{UserID: rtMgr, IsAdmin: true}); err == nil {
		t.Fatal("Retry on an approved hồ sơ succeeded")
	}
}

// TestBPMRoute_AutoNodeFailure_RetryAdvances: the record refuses the auto write (a unique clash);
// the hồ sơ stands still with the reason, the admin sees it, retry stays stalled until the cause is
// fixed and then advances.
func TestBPMRoute_AutoNodeFailure_RetryAdvances(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant, entity = o3aTenantA, "rt_stuck"
	st.routedEntity(t, ctx, tenant, entity)

	// A record already holds code DUP; the process will try to write DUP onto another.
	holder, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity, map[string]any{"code": "DUP", "amount": 1.0})
	if err != nil {
		t.Fatalf("create holder: %v", err)
	}
	st.routedProcess(t, ctx, tenant, entity, "after_create", &ApprovalGraphInput{Nodes: []ApprovalNodeInput{
		autoNode("stamp", "done", ApprovalSetInput{Field: "code", Value: "DUP"}), endNode("done", "approved"),
	}}, nil)
	victim, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity, map[string]any{"code": "OWN", "amount": 2.0})
	if err != nil {
		t.Fatalf("create victim: %v", err)
	}
	st.drain(t)

	inst := st.lastInstanceOf(t, tenant, entity, victim)
	if inst.Status != bpm.StatusPending || inst.LastError == "" || !inst.OnMachineNode() {
		t.Fatalf("after the refused write: %s last_error %q on machine=%v; want pending, stalled with a reason", inst.Status, inst.LastError, inst.OnMachineNode())
	}
	if h := inst.History[len(inst.History)-1]; h.Action != bpm.HistoryEffectFailed || h.Node != "stamp" {
		t.Fatalf("last history %+v; want effect_failed on the stamp node", h)
	}
	if got := st.routedField(t, tenant, entity, victim, "code"); got != "OWN" {
		t.Fatalf("the refused write changed the record: code %v", got)
	}

	// The admin's list shows it; the other tenant's does not; a non-admin may not retry.
	var pid string
	if err := st.sdb.Get(&pid, `SELECT id FROM sys_approval_processes WHERE tenant_id = ? AND entity_name = ? AND deleted_at IS NULL`, tenant, entity); err != nil {
		t.Fatalf("process id: %v", err)
	}
	rows, err := st.approval.ListProcessInstances(ctx, tenant, pid, nil, "", true, 0)
	if err != nil || len(rows) != 1 || !rows[0].Stuck || rows[0].LastError == "" || !rows[0].EffectFailed || rows[0].NodeID != "stamp" {
		t.Fatalf("stuck list = %+v (%v); want the one stalled hồ sơ with its error", rows, err)
	}
	if other, err := st.approval.ListProcessInstances(ctx, o3aTenantB, pid, nil, "", true, 0); err == nil && len(other) != 0 {
		t.Fatalf("tenant B read tenant A's stuck list: %+v", other)
	}
	if _, err := st.approval.RetryApproval(ctx, tenant, inst.ID, "", bpm.Actor{UserID: rtMgr}); err == nil {
		t.Fatal("a non-admin retried a hồ sơ")
	}
	if _, err := st.approval.RetryApproval(ctx, o3aTenantB, inst.ID, "", bpm.Actor{UserID: rtMgr, IsAdmin: true}); err == nil {
		t.Fatal("tenant B's admin retried tenant A's hồ sơ")
	}

	// Retry before the fix: still stalled, and the press is on the timeline.
	again, err := st.approval.RetryApproval(ctx, tenant, inst.ID, "thử lần 1", bpm.Actor{UserID: rtMgr, IsAdmin: true})
	if err != nil || again.Status != bpm.StatusPending || again.LastError == "" {
		t.Fatalf("retry before the fix: %v %v; want still stalled", err, again)
	}

	// Fix the cause (free the code), retry: the write lands and the hồ sơ ends.
	if err := st.recordSvc.Update(ctx, bpmAdmin(rtSubmitter), tenant, entity, holder, map[string]any{"code": "FREED"}, nil); err != nil {
		t.Fatalf("free the code: %v", err)
	}
	fixed, err := st.approval.RetryApproval(ctx, tenant, inst.ID, "đã giải phóng mã", bpm.Actor{UserID: rtMgr, IsAdmin: true})
	if err != nil || fixed.Status != bpm.StatusApproved || fixed.LastError != "" {
		t.Fatalf("retry after the fix: %v %v; want approved and clear", err, fixed)
	}
	if got := st.routedField(t, tenant, entity, victim, "code"); got != "DUP" {
		t.Fatalf("record code %v after the retry; want the auto node's DUP", got)
	}
	// Not stuck any more: the same press is refused.
	if _, err := st.approval.RetryApproval(ctx, tenant, inst.ID, "", bpm.Actor{UserID: rtMgr, IsAdmin: true}); err == nil {
		t.Fatal("retry of a finished hồ sơ succeeded")
	}
}

// continueJobsOf returns the bpm_continue jobs parked for an instance, each with the seq it carries.
func continueJobsOf(t *testing.T, st *bpmStack, tenant, instanceID string) (jobRows []jobs.Job, seqs []int64) {
	t.Helper()
	var rows []struct {
		ID      string          `db:"id"`
		Tenant  string          `db:"tenant_id"`
		Payload json.RawMessage `db:"payload"`
	}
	if err := st.sdb.Select(&rows, `SELECT id, tenant_id, payload FROM sys_jobs WHERE tenant_id = ? AND job_type = ? AND payload->>'$.instanceId' = ? ORDER BY created_at, id`,
		tenant, bpm.ContinueJobType, instanceID); err != nil {
		t.Fatalf("read continue jobs: %v", err)
	}
	for _, r := range rows {
		var p struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatalf("payload %s: %v", r.Payload, err)
		}
		jobRows = append(jobRows, jobs.Job{ID: r.ID, TenantID: r.Tenant, Type: bpm.ContinueJobType, Payload: r.Payload})
		seqs = append(seqs, p.Seq)
	}
	return jobRows, seqs
}

// recoverWithJob delivers the parked jobs to the production handler and returns the hồ sơ after.
func recoverWithJob(t *testing.T, st *bpmStack, tenant string, inst *bpm.Instance) *bpm.Instance {
	t.Helper()
	ctx := context.Background()
	js, seqs := continueJobsOf(t, st, tenant, inst.ID)
	current := false
	for _, q := range seqs {
		current = current || q == inst.Seq
	}
	if !current {
		t.Fatalf("continue jobs carry seqs %v; none for the seq %d the hồ sơ stands at — a crash here would strand it", seqs, inst.Seq)
	}
	handler := bpm.NewContinueJobHandler(st.repo, st.runner)
	deliver := func() {
		for _, j := range js {
			if err := handler(ctx, j); err != nil {
				t.Fatalf("continue job: %v", err)
			}
		}
	}
	deliver()
	got, err := st.repo.GetInstance(ctx, tenant, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	deliver() // a re-delivery: the seq moved on, nothing changes
	again, _ := st.repo.GetInstance(ctx, tenant, inst.ID)
	if again.Seq != got.Seq || len(again.History) != len(got.History) {
		t.Fatalf("a second delivery moved the hồ sơ: seq %d→%d history %d→%d", got.Seq, again.Seq, len(got.History), len(again.History))
	}
	// A job under another tenant never reaches the instance.
	if err := handler(ctx, jobs.Job{ID: "x", TenantID: o3aTenantB, Type: bpm.ContinueJobType, Payload: js[0].Payload}); err != nil {
		t.Fatalf("cross-tenant delivery: %v", err)
	}
	return got
}

// TestBPMRoute_SubmittedHoSoLeftOnAGatewayIsRecovered: the server stopped after the commit that
// left a submitted hồ sơ on its first gateway. The job parked by that transaction walks it on.
func TestBPMRoute_SubmittedHoSoLeftOnAGatewayIsRecovered(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	const tenant, entity = o3aTenantA, "rt_recover"
	st.routedEntity(t, ctx, tenant, entity)
	st.routedProcess(t, ctx, tenant, entity, "manual", quoteRouting(), nil)
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity, map[string]any{"amount": 3e7})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}

	// A requester WITHOUT the runner: the submit commits and "the server dies" before the walk.
	crashed := bpm.NewRequester(st.repo, st.repo, st.repo, st.repo, nil)
	sub, err := crashed.Submit(ctx, tenant, entity, rid, map[string]any{"amount": 3e7}, bpm.Actor{UserID: rtSubmitter})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if sub.Status != bpm.StatusPending || !sub.OnMachineNode() {
		t.Fatalf("after the crash the hồ sơ is %s on machine=%v; want pending on the gateway", sub.Status, sub.OnMachineNode())
	}
	cur, err := st.repo.GetInstance(ctx, tenant, sub.ID) // Submit returns the row as it was before its own history line
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	got := recoverWithJob(t, st, tenant, cur)
	if got.Status != bpm.StatusPending || got.CurrentApproverID != rtMgr || got.OnMachineNode() {
		t.Fatalf("after the job: %s approver %q machine=%v; want pending on rtMgr's step", got.Status, got.CurrentApproverID, got.OnMachineNode())
	}
}

// TestBPMRoute_ArmedHoSoLeftOnAGatewayIsRecovered: the same, through the ARMING path (a record
// event; no history write follows the insert) — the job comes from CreateInstance itself.
func TestBPMRoute_ArmedHoSoLeftOnAGatewayIsRecovered(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	const tenant, entity = o3aTenantA, "rt_arm_rec"
	st.routedEntity(t, ctx, tenant, entity)
	// The record first, the process after: creating the record arms nothing, so the only arming is
	// the event below, by an engine that has no runner ("the server dies before the walk").
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity, map[string]any{"amount": 3e7})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.routedProcess(t, ctx, tenant, entity, "after_create", quoteRouting(), nil)

	crashed := bpm.NewAsyncEngine(bpm.EngineConfig{Buffer: 4, Workers: 1}, st.repo, st.repo, st.repo, nil)
	crashed.Emit(ctx, bpm.Event{TenantID: tenant, ActorUserID: rtSubmitter, EntityName: entity, RecordID: rid,
		Trigger: bpm.TriggerAfterCreate, Data: map[string]any{"amount": 3e7}})
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := crashed.Shutdown(sctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	inst := st.lastInstanceOf(t, tenant, entity, rid)
	if inst.Status != bpm.StatusPending || !inst.OnMachineNode() {
		t.Fatalf("after the crash the hồ sơ is %s on machine=%v; want pending on the gateway", inst.Status, inst.OnMachineNode())
	}
	got := recoverWithJob(t, st, tenant, inst)
	if got.Status != bpm.StatusPending || got.CurrentApproverID != rtMgr || got.OnMachineNode() {
		t.Fatalf("after the job: %s approver %q machine=%v; want pending on rtMgr's step", got.Status, got.CurrentApproverID, got.OnMachineNode())
	}
}

// TestAutoChainLandingOnHumanNode_EnqueuesSLADue (T125, the T290 invariant for the new transition):
// a gateway → auto → person's step chain that ends on a step with a deadline leaves exactly one
// bpm_sla_due job for the seq the hồ sơ stands at, however many hops the walk took.
func TestAutoChainLandingOnHumanNode_EnqueuesSLADue(t *testing.T) {
	st := bpmWire(t)
	st.withDueJobs()
	ctx := context.Background()
	const tenant, entity = o3aTenantA, "rt_sla"
	st.routedEntity(t, ctx, tenant, entity)
	mgr := humanNode("mgr", "")
	mgr.ApproverID, mgr.SLASeconds = rtMgr, 3600
	st.routedProcess(t, ctx, tenant, entity, "after_create", &ApprovalGraphInput{Nodes: []ApprovalNodeInput{
		gwNode("route", "stamp", branchTo("x", "stamp", crit("amount", "lt", 1))),
		autoNode("stamp", "mgr", ApprovalSetInput{Field: "name", Value: "đã qua cổng"}),
		mgr,
	}}, nil)
	rid, err := st.recordSvc.Create(ctx, bpmAdmin(rtSubmitter), tenant, rtSubmitter, entity, map[string]any{"amount": 10.0})
	if err != nil {
		t.Fatalf("create record: %v", err)
	}
	st.drain(t)

	inst := st.lastInstanceOf(t, tenant, entity, rid)
	if inst.Status != bpm.StatusPending || inst.CurrentApproverID != rtMgr || inst.SLADueAt == nil {
		t.Fatalf("landed %s approver %q due %v; want pending on rtMgr with a deadline", inst.Status, inst.CurrentApproverID, inst.SLADueAt)
	}
	if got := st.routedField(t, tenant, entity, rid, "name"); got != "đã qua cổng" {
		t.Fatalf("auto node's write %v; want it on the record", got)
	}
	_, seqs := dueJobsOf(t, st, tenant, inst.ID)
	n := 0
	for _, q := range seqs {
		if q == inst.Seq {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d bpm_sla_due jobs for seq %d (all %v); want exactly one", n, inst.Seq, seqs)
	}
}
