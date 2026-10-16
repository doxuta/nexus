//go:build integration

// Integration test for the built-in TICKETING vertical pack (campaign-3). It installs
// ticketingPack end-to-end against real MySQL and proves the whole pack is config-valid: both
// entities created, the 1-N + polymorphic relation columns materialised, the B1 indexed picklists
// present, samples inserted, both roles created, the two PACK-WORKFLOWS rules created (queried
// back, including the TIMER-1 date_offset SLA rule), the dashboard created, and — the headline —
// the dashboard RUN returns the status + priority breakdowns the pack ships.
//
// It reuses the harness in vertical_pack_integration_test.go (vpSetup / vpBuildWF / vpColumnExists
// / vpTenantA / vpSeedUserID). vpBuildWF (not vpBuild) is used because it is the only build helper
// that wires the WorkflowWriter — without it the pack's workflows are silently skipped.
package vertical_pack

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// TestVerticalPack_InstallTicketing installs the built-in Ticketing pack and asserts the full
// surface: 2 entities, the customer FK + polymorphic related_to columns, B1 indexed status/priority
// columns, 10 samples (4 customers + 6 tickets), 2 roles, 4 workflows (queried back — SLA reminder +
// ack + routing + SLA-ESCALATION), 2 teams, 1 dashboard, and the dashboard-run status/priority
// breakdowns. The SLA-ESCALATION rule (v1.1.0) is decoded and asserted in full: the {due_date, 1h,
// after} timer, the status-open "or" gate, and all three actions (priority→Urgent, assign over the
// escalation team resolved to its id, owner notification) — a broken compose fails the assertion.
func TestVerticalPack_InstallTicketing(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, workflowSvc := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// PACK-TEAMS: seed a REAL install user so the teams pass can add it as the seeded team's first
	// member — auth.TeamRepository.AddMember rejects a non-live user (ErrMemberUserNotFound), and the
	// round-robin resolver only counts status='active' members. tenants(vpTenantA) is seeded by vpSetup.
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@vp-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack, ok := BuiltinPack("ticketing")
	if !ok {
		t.Fatal("builtin ticketing pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin ticketing: %v", err)
	}

	// --- Entities: tk_customers + tk_tickets. ---
	if len(res.EntitiesCreated) != 2 {
		t.Errorf("ticketing: created %v, want 2 entities", res.EntitiesCreated)
	}

	// --- Relation columns: the 1-N `customer` FK and the polymorphic related_to pair. ---
	// The polymorphic field materialises two STORED generated columns (related_to_type VARCHAR(100),
	// related_to_id CHAR(36)) — proving tk_customers preceded tk_tickets so the target existed.
	for _, tc := range []struct{ table, col string }{
		{"obj_tk_tickets", "customer_id"},
		{"obj_tk_tickets", "related_to_type"},
		{"obj_tk_tickets", "related_to_id"},
	} {
		if !vpColumnExists(t, sdb, tc.table, tc.col) {
			t.Errorf("ticketing: %s missing column %s", tc.table, tc.col)
		}
	}

	// --- B1: indexed generated columns for the two dashboard GROUP BY picklists. ---
	for _, col := range []string{"status", "priority"} {
		if !vpColumnExists(t, sdb, "obj_tk_tickets", col) {
			t.Errorf("ticketing: B1 indexed generated column obj_tk_tickets.%s missing", col)
		}
	}

	// --- Samples: 4 customers + 6 tickets = 10. ---
	const wantSamples = 10
	if res.SamplesInserted != wantSamples {
		t.Errorf("ticketing: SamplesInserted = %d, want %d", res.SamplesInserted, wantSamples)
	}

	// --- Roles: tk_agent + tk_manager. ---
	if len(res.RolesCreated) != 2 {
		t.Errorf("ticketing: roles created %v, want 2", res.RolesCreated)
	}

	// --- Workflows: all twelve created and queryable back (config-validated through the real service). ---
	// Four automation rules (SLA reminder + ack + routing + escalation), the four v1.7.0
	// SLA-DEADLINE tiers that fill due_date (without which the two due_date timers never park), and
	// the four v1.8.0 recompute tiers (#81: priority change shortens a machine deadline only).
	// Their own assertions live in ticketing_sla_test.go + ticketing_sla_integration_test.go +
	// ticketing_priority_tighten_integration_test.go.
	if len(res.WorkflowsCreated) != 12 {
		t.Fatalf("ticketing: WorkflowsCreated = %v, want 12 (4 SLA-deadline tiers + 4 recompute tiers + SLA reminder + ack + routing + escalation)", res.WorkflowsCreated)
	}
	rules, err := workflowSvc.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	byName := map[string]workflow.WorkflowRecord{}
	for _, r := range rules {
		byName[r.Name] = r
	}
	if r, ok := byName["Tự động phản hồi ticket mới"]; !ok || r.Trigger != "after_create" || r.EntityName != "tk_tickets" {
		t.Errorf("ack rule = %+v, want trigger after_create on tk_tickets", r)
	}
	sla, ok := byName["Nhắc hạn SLA"]
	if !ok || sla.Trigger != "date_offset" {
		t.Fatalf("SLA rule = %+v, want trigger date_offset", sla)
	}
	// The TIMER-1 date_offset timer config persisted inside the condition JSON — decode and assert
	// the actual struct (due_date anchor, 2 hours before) rather than a raw-substring check.
	slaWF, err := sla.ToWorkflow()
	if err != nil {
		t.Fatalf("decode SLA condition: %v", err)
	}
	if slaWF.Condition == nil || slaWF.Condition.Timer == nil {
		t.Fatalf("SLA rule has no persisted timer config; condition = %+v", slaWF.Condition)
	}
	if tm := slaWF.Condition.Timer; tm.DateField != "due_date" || tm.Offset.Amount != 2 ||
		tm.Offset.Unit != "hours" || tm.Direction != "before" {
		t.Errorf("SLA timer = %+v, want {due_date, {2 hours}, before}", *slaWF.Condition.Timer)
	}

	// --- PACK-TEAMS: teams "Đội hỗ trợ" + "Đội quản lý SLA" created (slice order), each with the
	// install user as its first active member. ---
	if len(res.TeamsCreated) != 2 || res.TeamsCreated[0] != "Đội hỗ trợ" || res.TeamsCreated[1] != "Đội quản lý SLA" {
		t.Fatalf("ticketing: TeamsCreated = %v, want [Đội hỗ trợ, Đội quản lý SLA]", res.TeamsCreated)
	}
	teamRepo := auth.NewTeamRepository(gdb)
	teams, err := teamRepo.ListTeams(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	var teamID, escalationTeamID string
	for _, tm := range teams {
		switch tm.Name {
		case "Đội hỗ trợ":
			teamID = tm.ID
		case "Đội quản lý SLA":
			escalationTeamID = tm.ID
		}
	}
	if teamID == "" {
		t.Fatalf("team 'Đội hỗ trợ' not found after install; got %+v", teams)
	}
	if escalationTeamID == "" {
		t.Fatalf("escalation team 'Đội quản lý SLA' not found after install; got %+v", teams)
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
		t.Errorf("install user %s is not a member of team 'Đội hỗ trợ'; members = %+v", vpSeedUserID, members)
	}

	// --- ASSIGN-1 routing rule: created, and its assign action's team resolved NAME → the team id. ---
	// The RecordService in this harness uses the NOOP workflow engine (vpBuildWF does not call
	// WithEngine), so an after_create rule does NOT fire on a record insert. We therefore assert the
	// STORED RULE CONFIG — the team resolved to the real UUID, not the literal "Đội hỗ trợ" — rather
	// than a live assignment; the task explicitly allows this fallback when the dispatcher is unwired.
	routing, ok := byName["Định tuyến ticket mới"]
	if !ok || routing.Trigger != "after_create" || routing.EntityName != "tk_tickets" {
		t.Fatalf("routing rule = %+v, want trigger after_create on tk_tickets", routing)
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
	if act.Team == "Đội hỗ trợ" {
		t.Errorf("routing action team is the literal NAME — name→id resolution did not run")
	}

	// --- SLA-ESCALATION (v1.1.0): the past-due escalation rule, decoded and asserted in full. ---
	// This is the discriminating assertion: a broken compose (wrong timer direction, dropped status
	// gate, wrong/missing action) must fail here. It re-reads the STORED rule (config-validated
	// through the real WorkflowService), so it also proves toWorkflowInput threaded Match + Rules +
	// Timer (not just Timer) into the persisted condition.
	esc, ok := byName["Leo thang ticket quá hạn SLA"]
	if !ok || esc.Trigger != "date_offset" || esc.EntityName != "tk_tickets" {
		t.Fatalf("escalation rule = %+v, want trigger date_offset on tk_tickets", esc)
	}
	escWF, err := esc.ToWorkflow()
	if err != nil {
		t.Fatalf("decode escalation rule: %v", err)
	}
	if escWF.Condition == nil {
		t.Fatalf("escalation rule has no condition")
	}
	// Timer: fire 1 hour AFTER due_date (the whole point — "after", not "before").
	if tm := escWF.Condition.Timer; tm == nil || tm.DateField != "due_date" || tm.Offset.Amount != 1 ||
		tm.Offset.Unit != "hours" || tm.Direction != "after" {
		t.Errorf("escalation timer = %+v, want {due_date, {1 hours}, after}", escWF.Condition.Timer)
	}
	// Status gate: match "or" over [status eq Open, status eq Pending] — only a still-open ticket
	// escalates. If this were dropped, a Resolved/Closed past-due ticket would wrongly escalate.
	if escWF.Condition.Match != "or" {
		t.Errorf("escalation condition match = %q, want %q (membership gate needs any-of)", escWF.Condition.Match, "or")
	}
	if len(escWF.Condition.Rules) != 2 {
		t.Fatalf("escalation condition rules = %+v, want 2 (status eq Open, status eq Pending)", escWF.Condition.Rules)
	}
	wantRules := map[string]bool{"Open": false, "Pending": false}
	for _, r := range escWF.Condition.Rules {
		if r.Field != "status" || r.Op != "eq" {
			t.Errorf("escalation rule = %+v, want {status, eq, <status>}", r)
			continue
		}
		v, _ := r.Value.(string)
		if _, want := wantRules[v]; !want {
			t.Errorf("escalation rule value = %q, want Open or Pending", v)
			continue
		}
		wantRules[v] = true
	}
	for v, seen := range wantRules {
		if !seen {
			t.Errorf("escalation condition missing rule for status %q", v)
		}
	}
	// Actions: priority→Urgent (field_update), assign round_robin over the ESCALATION team into
	// assigned_to, and an owner notification — in that order.
	if len(escWF.Actions) != 3 {
		t.Fatalf("escalation actions = %+v, want 3 (field_update, assign, notification)", escWF.Actions)
	}
	if a0 := escWF.Actions[0]; a0.Type != "field_update" || a0.Field != "priority" || a0.Value != "Urgent" {
		t.Errorf("escalation action[0] = %+v, want {field_update, priority, Urgent}", a0)
	}
	a1 := escWF.Actions[1]
	if a1.Type != "assign" || a1.Strategy != "round_robin" || a1.Field != "assigned_to" {
		t.Errorf("escalation action[1] = %+v, want {assign, round_robin, assigned_to}", a1)
	}
	if a1.Team != escalationTeamID {
		t.Errorf("escalation assign team = %q, want the resolved escalation team id %q (NAME must resolve to id)", a1.Team, escalationTeamID)
	}
	if a1.Team == "Đội quản lý SLA" {
		t.Errorf("escalation assign team is the literal NAME — name→id resolution did not run")
	}
	if a2 := escWF.Actions[2]; a2.Type != "in_app_notification" || a2.UserID != "$owner" || a2.Message == "" {
		t.Errorf("escalation action[2] = %+v, want {in_app_notification, $owner, <message>}", a2)
	}

	// --- Escalation does NOT fire on install/seeding. Pack workflows install in Pass 8 (AFTER the
	// Pass 3 samples), and this harness wires the NOOP workflow engine (vpBuildWF), so no after_*/
	// date_offset rule runs on the seed inserts. Concretely: the "Không đăng nhập được" seed ticket
	// is Open + priority High; if escalation had fired it would be Urgent with assigned_to set. ---
	ticketRepo := dynrepo.New(sdb)
	seedTickets, _, err := ticketRepo.ListRecords(ctx, "tk_tickets", vpTenantA, dynrepo.AdminScope(), dynrepo.ListParams{Limit: 100})
	if err != nil {
		t.Fatalf("list seed tickets: %v", err)
	}
	var probe map[string]any
	for _, row := range seedTickets {
		if s, _ := row.Data["subject"].(string); s == "Không đăng nhập được" {
			probe = row.Data
		}
	}
	if probe == nil {
		t.Fatalf("seed ticket 'Không đăng nhập được' not found; escalation non-fire probe cannot run")
	}
	if p, _ := probe["priority"].(string); p != "High" {
		t.Errorf("seed ticket priority = %q, want High (escalation must NOT fire on seed/install)", p)
	}
	if a, ok := probe["assigned_to"]; ok && a != nil && a != "" {
		t.Errorf("seed ticket assigned_to = %v, want unset (no workflow fires on seed insert)", a)
	}

	// --- Dashboard: created by name. ---
	if len(res.DashboardsCreated) != 1 || res.DashboardsCreated[0] != "Tổng quan Ticketing" {
		t.Errorf("ticketing: DashboardsCreated = %v, want [Tổng quan Ticketing]", res.DashboardsCreated)
	}

	// --- Run the dashboard and assert the KPI + the status/priority breakdowns. ---
	dashRepo := dashboard.NewSQLRepository(sdb)
	rows, err := dashRepo.List(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list dashboards: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 dashboard row, got %d", len(rows))
	}
	dashID := rows[0].ID

	recordSvc := services.NewRecordService(metadata.NewRepository(gdb), dynrepo.New(sdb))
	dashSvc := services.NewDashboardService(dashRepo, recordSvc, zap.NewNop())
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	runResult, err := dashSvc.RunDashboard(ctx, scope, vpTenantA, dashID, nil)
	if err != nil {
		t.Fatalf("RunDashboard: %v", err)
	}
	if len(runResult.Widgets) != 3 {
		t.Fatalf("RunDashboard returned %d widgets, want 3", len(runResult.Widgets))
	}

	findWidget := func(id string) *services.WidgetRunResult {
		for i := range runResult.Widgets {
			if runResult.Widgets[i].WidgetID == id {
				return &runResult.Widgets[i]
			}
		}
		return nil
	}

	// w1: total ticket count (scalar KPI). 6 seed tickets → count 6.
	w1 := findWidget("w1")
	if w1 == nil {
		t.Fatal("widget w1 (total ticket count) not found")
	}
	if w1.Error != "" {
		t.Fatalf("widget w1 error: %s (code: %s)", w1.Error, w1.Code)
	}
	if w1.Result == nil || w1.Result.Grouped {
		t.Fatalf("widget w1: expected scalar result, got %+v", w1.Result)
	}
	if w1.Result.Scalar.Count != 6 {
		t.Errorf("widget w1 count = %d, want 6 (seed ticket count)", w1.Result.Scalar.Count)
	}

	// w2: count by status — 6 tickets span 4 distinct status values (Open×3, Pending, Resolved,
	// Closed) → 4 groups. The indexed generated column makes this a clean GROUP BY (no full-scan).
	w2 := findWidget("w2")
	if w2 == nil {
		t.Fatal("widget w2 (count by status) not found")
	}
	if w2.Error != "" {
		t.Fatalf("widget w2 error: %s (code: %s)", w2.Error, w2.Code)
	}
	if w2.Result == nil || !w2.Result.Grouped {
		t.Fatalf("widget w2: expected grouped result, got %+v", w2.Result)
	}
	if len(w2.Result.Groups) != 4 {
		t.Errorf("widget w2 groups = %d, want 4 (Open/Pending/Resolved/Closed)", len(w2.Result.Groups))
	}

	// w3: count by priority — 6 tickets span 4 distinct priority values (Low, Medium×2, High×2,
	// Urgent) → 4 groups.
	w3 := findWidget("w3")
	if w3 == nil {
		t.Fatal("widget w3 (count by priority) not found")
	}
	if w3.Error != "" {
		t.Fatalf("widget w3 error: %s (code: %s)", w3.Error, w3.Code)
	}
	if w3.Result == nil || !w3.Result.Grouped {
		t.Fatalf("widget w3: expected grouped result, got %+v", w3.Result)
	}
	if len(w3.Result.Groups) != 4 {
		t.Errorf("widget w3 groups = %d, want 4 (Low/Medium/High/Urgent)", len(w3.Result.Groups))
	}

	// --- Idempotent re-install: both teams + all four workflows are skipped by name (no duplicates). ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install ticketing: %v", err)
	}
	if len(res2.TeamsCreated) != 0 || len(res2.TeamsSkipped) != 2 {
		t.Errorf("re-install teams: created %v / skipped %v, want [] / 2 (Đội hỗ trợ + Đội quản lý SLA)", res2.TeamsCreated, res2.TeamsSkipped)
	}
	if len(res2.WorkflowsCreated) != 0 || len(res2.WorkflowsSkipped) != 12 {
		t.Errorf("re-install workflows: created %v / skipped %v, want 0 / 12 (all by name)", res2.WorkflowsCreated, res2.WorkflowsSkipped)
	}
	// No duplicate team row appeared for the same name (per-tenant UNIQUE(name) + skip-by-name).
	teams2, _ := teamRepo.ListTeams(ctx, vpTenantA)
	nHelp := 0
	for _, tm := range teams2 {
		if tm.Name == "Đội hỗ trợ" {
			nHelp++
		}
	}
	if nHelp != 1 {
		t.Errorf("after re-install, %d teams named 'Đội hỗ trợ', want 1 (no dup)", nHelp)
	}
}
