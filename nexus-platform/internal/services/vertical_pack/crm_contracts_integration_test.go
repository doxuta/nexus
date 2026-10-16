//go:build integration

// Integration test for the CRM pack's CONTRACT-RENEWAL enrichment (v1.4.0): crm_contracts tracks
// signed contracts on an account, with TWO date_offset timer workflows keyed on end_date — a
// renewal reminder (30 days BEFORE, gated on status "Hiệu lực") and an auto-expire (1 day AFTER,
// gated on status "Hiệu lực", then field_update status → "Hết hạn" + $owner notification). It is a
// pure declarative compose of already-shipped primitives (entity + 1-N relation + currency +
// picklist + date + TIMER-1 before/after + field_update + notification) — NO new engine code,
// mirroring Ticketing SLA-ESCALATION / LMS TUITION-OVERDUE.
//
// THE DISCRIMINATING ASSERTION (a broken compose fails it): install the CRM pack → decode the two
// STORED workflow rules and assert the exact persisted structs — reminder timer {end_date, 30d,
// before} + gate {status eq "Hiệu lực"}; expire timer {end_date, 1d, after} + gate {status eq
// "Hiệu lực"} + actions [field_update status → "Hết hạn", in_app_notification $owner]. A mistyped
// picklist value, a wrong direction, a missing gate, or a dropped action decodes to a different
// struct (or 422s at install), turning this test red.
//
// vpBuildWF (not vpBuild) is used because crmPack ships Teams + Workflows — without
// WorkflowWriter/TeamWriter wired those passes silently skip. The record writer in that harness
// uses the NOOP workflow engine, so a date_offset rule cannot FIRE here (that is the automation/
// quote tests' scope too); we assert the STORED RULE CONFIG, which is the compose contract.
package vertical_pack

import (
	"context"
	"testing"

	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// TestVerticalPack_InstallCRMContracts installs the built-in CRM pack and asserts the
// CONTRACT-RENEWAL surface: the crm_contracts entity + its fields, the 3 seeded contracts (with
// non-fire-on-seed: the seeded "Hiệu lực" contract is untouched, workflows install AFTER samples),
// and the two contract workflows decoded back to their exact persisted timer/gate/action structs.
func TestVerticalPack_InstallCRMContracts(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, workflowSvc := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// PACK-TEAMS needs a REAL install user (auth.TeamRepository.AddMember rejects a non-live user).
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@crm-contract-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin crm: %v", err)
	}

	// --- #1: crm_contracts entity + its fields exist with the expected types. ---
	metaRepo := metadata.NewRepository(gdb)
	entities, err := metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	var contractsEnt *metadata.EntityDefinition
	for i := range entities {
		if entities[i].Name == "crm_contracts" {
			contractsEnt = &entities[i]
		}
	}
	if contractsEnt == nil {
		t.Fatal("crm_contracts entity not found after install")
	}
	fields, err := metaRepo.ListFields(ctx, vpTenantA, contractsEnt.ID)
	if err != nil {
		t.Fatalf("list crm_contracts fields: %v", err)
	}
	typeByName := map[string]string{}
	for _, f := range fields {
		typeByName[f.Name] = f.FieldType
	}
	for _, want := range []struct{ name, ftype string }{
		{"contract_number", "auto_number"},
		{"name", "text"},
		{"account", "relation"},
		{"start_date", "date"},
		{"end_date", "date"},
		{"value", "currency"},
		{"status", "picklist"},
	} {
		got, present := typeByName[want.name]
		if !present {
			t.Errorf("crm_contracts.%s missing (fields = %v)", want.name, keys(typeByName))
			continue
		}
		if got != want.ftype {
			t.Errorf("crm_contracts.%s fieldType = %q, want %q", want.name, got, want.ftype)
		}
	}

	// --- #2: 3 contracts seeded; non-fire-on-seed (the seeded "Hiệu lực" contract is untouched). ---
	// Workflows are the LAST data pass (after samples), and this harness's record writer uses the
	// NOOP engine — so no date_offset timer is scheduled or fired on the seed rows. The seeded active
	// contract must therefore STILL read status "Hiệu lực" (the auto-expire rule did NOT flip it).
	dyn := dynrepo.New(sdb)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithSequenceClaimer(dyn)
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}
	contractList, err := recordSvc.List(ctx, scope, vpTenantA, "crm_contracts", services.ListQuery{Page: 1, Limit: 20, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list crm_contracts: %v", err)
	}
	if len(contractList.Records) != 3 {
		t.Fatalf("crm: %d contracts seeded, want 3", len(contractList.Records))
	}
	statusByName := map[string]string{}
	for _, r := range contractList.Records {
		name, _ := r.Data["name"].(string)
		status, _ := r.Data["status"].(string)
		statusByName[name] = status
	}
	if got := statusByName["Hợp đồng dịch vụ Acme 2026"]; got != "Hiệu lực" {
		t.Errorf("seeded active contract status = %q, want %q (non-fire-on-seed: auto-expire must NOT touch a seed row)", got, "Hiệu lực")
	}

	// --- #3: both contract workflows created (proves ordering: samples Pass 3 before workflows Pass 8). ---
	// The full CRM pack ships 4 workflows: routing + follow-up SLA (v1.2.0) + renewal reminder +
	// auto-expire (v1.4.0). SamplesInserted covers the contracts too (28 pre-v1.4.0 + 3 = 31).
	if res.SamplesInserted != 31 {
		t.Errorf("SamplesInserted = %d, want 31 (contracts landed in the samples pass)", res.SamplesInserted)
	}
	if len(res.WorkflowsCreated) != 4 {
		t.Fatalf("WorkflowsCreated = %v, want 4 (routing + follow-up SLA + renewal reminder + auto-expire)", res.WorkflowsCreated)
	}
	rules, err := workflowSvc.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	byName := map[string]workflow.WorkflowRecord{}
	for _, r := range rules {
		byName[r.Name] = r
	}

	// --- #4: renewal reminder — date_offset {end_date, 30d, before} + gate {status eq "Hiệu lực"}. ---
	reminder, ok := byName["Nhắc gia hạn hợp đồng"]
	if !ok || reminder.Trigger != "date_offset" || reminder.EntityName != "crm_contracts" {
		t.Fatalf("reminder rule = %+v, want trigger date_offset on crm_contracts", reminder)
	}
	remWF, err := reminder.ToWorkflow()
	if err != nil {
		t.Fatalf("decode reminder rule: %v", err)
	}
	if remWF.Condition == nil || remWF.Condition.Timer == nil {
		t.Fatalf("reminder rule has no persisted timer; condition = %+v", remWF.Condition)
	}
	if tm := remWF.Condition.Timer; tm.DateField != "end_date" || tm.Offset.Amount != 30 ||
		tm.Offset.Unit != "days" || tm.Direction != "before" {
		t.Errorf("reminder timer = %+v, want {end_date, {30 days}, before}", *remWF.Condition.Timer)
	}
	assertActiveGate(t, "reminder", remWF)
	if len(remWF.Actions) != 1 || remWF.Actions[0].Type != "in_app_notification" || remWF.Actions[0].UserID != "$owner" {
		t.Errorf("reminder actions = %+v, want exactly 1 in_app_notification to $owner", remWF.Actions)
	}

	// --- #5: auto-expire — date_offset {end_date, 1d, after} + gate {status eq "Hiệu lực"} +
	// actions [field_update status → "Hết hạn", in_app_notification $owner]. ---
	expire, ok := byName["Tự động hết hạn hợp đồng"]
	if !ok || expire.Trigger != "date_offset" || expire.EntityName != "crm_contracts" {
		t.Fatalf("auto-expire rule = %+v, want trigger date_offset on crm_contracts", expire)
	}
	expWF, err := expire.ToWorkflow()
	if err != nil {
		t.Fatalf("decode auto-expire rule: %v", err)
	}
	if expWF.Condition == nil || expWF.Condition.Timer == nil {
		t.Fatalf("auto-expire rule has no persisted timer; condition = %+v", expWF.Condition)
	}
	if tm := expWF.Condition.Timer; tm.DateField != "end_date" || tm.Offset.Amount != 1 ||
		tm.Offset.Unit != "days" || tm.Direction != "after" {
		t.Errorf("auto-expire timer = %+v, want {end_date, {1 days}, after}", *expWF.Condition.Timer)
	}
	assertActiveGate(t, "auto-expire", expWF)
	if len(expWF.Actions) != 2 {
		t.Fatalf("auto-expire actions = %+v, want exactly 2 (field_update + in_app_notification)", expWF.Actions)
	}
	fu := expWF.Actions[0]
	if fu.Type != "field_update" || fu.Field != "status" {
		t.Errorf("auto-expire action[0] = %+v, want field_update on status", fu)
	}
	if v, _ := fu.Value.(string); v != "Hết hạn" {
		t.Errorf("auto-expire field_update value = %v, want %q (byte-identical to the picklist value)", fu.Value, "Hết hạn")
	}
	if n := expWF.Actions[1]; n.Type != "in_app_notification" || n.UserID != "$owner" {
		t.Errorf("auto-expire action[1] = %+v, want in_app_notification to $owner", n)
	}

	// --- #6: idempotent re-install — crm_contracts skipped, both contract workflows skipped by name. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install crm: %v", err)
	}
	skippedEnt := map[string]bool{}
	for _, e := range res2.EntitiesSkipped {
		skippedEnt[e] = true
	}
	if !skippedEnt["crm_contracts"] {
		t.Errorf("re-install: crm_contracts not in EntitiesSkipped (%v)", res2.EntitiesSkipped)
	}
	skippedWF := map[string]bool{}
	for _, w := range res2.WorkflowsSkipped {
		skippedWF[w] = true
	}
	for _, want := range []string{"Nhắc gia hạn hợp đồng", "Tự động hết hạn hợp đồng"} {
		if !skippedWF[want] {
			t.Errorf("re-install: workflow %q not skipped by name (%v)", want, res2.WorkflowsSkipped)
		}
	}
}

// assertActiveGate asserts a contract workflow's condition gates on exactly one rule: status eq
// "Hiệu lực" (byte-identical to the picklist value) — the fire-time re-eval that keeps a draft/
// renewed/expired contract from being reminded or auto-expired.
func assertActiveGate(t *testing.T, label string, wf workflow.Workflow) {
	t.Helper()
	if wf.Condition == nil || len(wf.Condition.Rules) != 1 {
		t.Errorf("%s: gate rules = %+v, want exactly 1 (status eq \"Hiệu lực\")", label, wf.Condition)
		return
	}
	r := wf.Condition.Rules[0]
	if r.Field != "status" || r.Op != "eq" {
		t.Errorf("%s: gate rule = %+v, want {status eq ...}", label, r)
	}
	if v, _ := r.Value.(string); v != "Hiệu lực" {
		t.Errorf("%s: gate value = %v, want %q (byte-identical to the picklist value)", label, r.Value, "Hiệu lực")
	}
}
