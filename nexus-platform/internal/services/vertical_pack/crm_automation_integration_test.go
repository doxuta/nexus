//go:build integration

// Integration test for the CRM vertical pack's AUTOMATION surface (v1.2.0 — PACK-TEAMS +
// PACK-WORKFLOWS). It installs crmPack end-to-end against real MySQL and proves the two
// engine-depth automation primitives now ship in the CRM pack:
//   - PACK-TEAMS: the "Đội Sales" team is seeded with the install user as its first active member.
//   - ASSIGN-1 lead-routing: the "Định tuyến lead mới" after_create rule round-robins new leads
//     over "Đội Sales" into crm_leads.assigned_to — its assign action's team NAME resolves to the
//     seeded team's real UUID at install (a pack cannot know the id).
//   - TIMER-1 follow-up SLA: the "Nhắc theo dõi hoạt động" date_offset rule decodes back to a timer
//     anchored on crm_activities.due_date, 1 day BEFORE.
//
// Re-install is idempotent: the team + both workflows are skipped by name (no duplicate rows).
//
// It reuses the harness in vertical_pack_integration_test.go (vpSetup / vpBuildWF / vpTenantA /
// vpSeedUserID). vpBuildWF (not vpBuild) is used because it is the only build helper that wires the
// WorkflowWriter + TeamWriter — without them the pack's teams/workflows are silently skipped. The
// full-schema install (entities/samples/rollups/dashboard) is covered by TestVerticalPack_InstallBuiltin;
// this test focuses on the team + workflow surface added in v1.2.0.
package vertical_pack

import (
	"context"
	"testing"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/workflow"
)

// TestVerticalPack_InstallCRMAutomation installs the built-in CRM pack and asserts the v1.2.0
// automation surface: team "Đội Sales" + member, the two new workflows created (routing +
// follow-up SLA), the routing rule's assign team resolved to a UUID, the SLA rule's decoded timer,
// and idempotent re-install (team + both workflows skipped by name).
func TestVerticalPack_InstallCRMAutomation(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, workflowSvc := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// PACK-TEAMS: seed a REAL install user so the teams pass can add it as the seeded team's first
	// member — auth.TeamRepository.AddMember rejects a non-live user (ErrMemberUserNotFound), and the
	// round-robin resolver only counts status='active' members. tenants(vpTenantA) is seeded by vpSetup.
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@crm-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	crm, ok := BuiltinPack("crm")
	if !ok {
		t.Fatal("builtin crm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("install builtin crm: %v", err)
	}

	// --- PACK-TEAMS: team "Đội Sales" created with the install user as its first active member. ---
	if len(res.TeamsCreated) != 1 || res.TeamsCreated[0] != "Đội Sales" {
		t.Fatalf("crm: TeamsCreated = %v, want [Đội Sales]", res.TeamsCreated)
	}
	teamRepo := auth.NewTeamRepository(gdb)
	teams, err := teamRepo.ListTeams(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	var teamID string
	for _, tm := range teams {
		if tm.Name == "Đội Sales" {
			teamID = tm.ID
		}
	}
	if teamID == "" {
		t.Fatalf("team 'Đội Sales' not found after install; got %+v", teams)
	}
	members, err := teamRepo.ListMembers(ctx, vpTenantA, teamID)
	if err != nil {
		t.Fatalf("list team members: %v", err)
	}
	foundMember := false
	for _, m := range members {
		if m.ID == vpSeedUserID {
			foundMember = true
		}
	}
	if !foundMember {
		t.Errorf("install user %s is not a member of team 'Đội Sales'; members = %+v", vpSeedUserID, members)
	}

	// --- Workflows: both created and queryable back (config-validated through the real service). ---
	if len(res.WorkflowsCreated) != 4 {
		t.Fatalf("crm: WorkflowsCreated = %v, want 4 (routing + follow-up SLA + renewal reminder + auto-expire)", res.WorkflowsCreated)
	}
	rules, err := workflowSvc.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	byName := map[string]workflow.WorkflowRecord{}
	for _, r := range rules {
		byName[r.Name] = r
	}

	// --- ASSIGN-1 lead-routing rule: created, and its assign action's team resolved NAME → the id. ---
	// The RecordService in this harness uses the NOOP workflow engine (vpBuildWF does not call
	// WithEngine), so an after_create rule does NOT fire on a record insert. We therefore assert the
	// STORED RULE CONFIG — the team resolved to the real UUID, not the literal "Đội Sales" — rather
	// than a live assignment.
	routing, ok := byName["Định tuyến lead mới"]
	if !ok || routing.Trigger != "after_create" || routing.EntityName != "crm_leads" {
		t.Fatalf("routing rule = %+v, want trigger after_create on crm_leads", routing)
	}
	routeWF, err := routing.ToWorkflow()
	if err != nil {
		t.Fatalf("decode routing rule: %v", err)
	}
	if len(routeWF.Actions) != 1 {
		t.Fatalf("routing rule actions = %+v, want exactly 1 assign action", routeWF.Actions)
	}
	act := routeWF.Actions[0]
	if act.Type != "assign" || act.Strategy != "round_robin" || act.Field != "assigned_to" {
		t.Errorf("routing action = %+v, want {assign, round_robin, assigned_to}", act)
	}
	if act.Team != teamID {
		t.Errorf("routing action team = %q, want the resolved team id %q (NAME must resolve to id)", act.Team, teamID)
	}
	if act.Team == "Đội Sales" {
		t.Errorf("routing action team is the literal NAME — name→id resolution did not run")
	}

	// --- TIMER-1 follow-up SLA rule: decodes back to a date_offset timer on due_date, 1 day before. ---
	sla, ok := byName["Nhắc theo dõi hoạt động"]
	if !ok || sla.Trigger != "date_offset" || sla.EntityName != "crm_activities" {
		t.Fatalf("SLA rule = %+v, want trigger date_offset on crm_activities", sla)
	}
	slaWF, err := sla.ToWorkflow()
	if err != nil {
		t.Fatalf("decode SLA condition: %v", err)
	}
	if slaWF.Condition == nil || slaWF.Condition.Timer == nil {
		t.Fatalf("SLA rule has no persisted timer config; condition = %+v", slaWF.Condition)
	}
	if tm := slaWF.Condition.Timer; tm.DateField != "due_date" || tm.Offset.Amount != 1 ||
		tm.Offset.Unit != "days" || tm.Direction != "before" {
		t.Errorf("SLA timer = %+v, want {due_date, {1 days}, before}", *slaWF.Condition.Timer)
	}

	// --- Idempotent re-install: the team + both workflows are skipped by name (no duplicates). ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, crm)
	if err != nil {
		t.Fatalf("re-install crm: %v", err)
	}
	if len(res2.TeamsCreated) != 0 || len(res2.TeamsSkipped) != 1 || res2.TeamsSkipped[0] != "Đội Sales" {
		t.Errorf("re-install teams: created %v / skipped %v, want [] / [Đội Sales]", res2.TeamsCreated, res2.TeamsSkipped)
	}
	if len(res2.WorkflowsCreated) != 0 || len(res2.WorkflowsSkipped) != 4 {
		t.Errorf("re-install workflows: created %v / skipped %v, want 0 / 4 (all by name)", res2.WorkflowsCreated, res2.WorkflowsSkipped)
	}
	// No duplicate team row appeared for the same name (per-tenant UNIQUE(name) + skip-by-name).
	teams2, _ := teamRepo.ListTeams(ctx, vpTenantA)
	nSales := 0
	for _, tm := range teams2 {
		if tm.Name == "Đội Sales" {
			nSales++
		}
	}
	if nSales != 1 {
		t.Errorf("after re-install, %d teams named 'Đội Sales', want 1 (no dup)", nSales)
	}
}
