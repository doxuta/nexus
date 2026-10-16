//go:build integration

// Integration test for the built-in Project-Management vertical pack (the 5th shipped vertical
// after CRM + Clinic + Ticketing + LMS). It installs pmPack end-to-end against real MySQL and
// proves the whole pack is config-valid AND that its headline primitive — the parent-side rollup
// (task_count + total_estimate) — works E2E, exactly like the LMS gradebook rollup: 2 entities
// created, the two deferred rollup fields added on pm_projects, the "Đội dự án" team seeded with
// the install user, the three PACK-WORKFLOWS rules created (the ASSIGN-1 routing rule's team NAME
// resolved to the seeded UUID, the TIMER-1 date_offset decoded to a due_date reminder, and the
// escalation decoded to a {due_date, 1 day, after} timer gated by an "or" over the three non-done
// statuses → field_update priority Khẩn + owner notify), the 8 samples seeded (3/5), and — THE KEY
// ASSERTION — reading @proj_web with ?rollup= returns task_count == 3 and total_estimate == 16 (the
// exact count/sum of its three seeded tasks' estimate_hours 8/5/3, NOT the whole-table 5/26). A
// second test (TestVerticalPack_ReportsPM) installs the pack through the report-wired harness and
// RUNS both canned reports. Re-install is idempotent.
//
// It reuses the harness in vertical_pack_integration_test.go (vpSetup / vpBuildWF / vpBuildReports /
// vpTenantA / vpSeedUserID) plus the helpers lmsAsInt / keys / vpFindReport / vpStrSliceEqual /
// vpReportSetEqual defined in the sibling integration test files (same package). vpBuildWF wires the
// WorkflowWriter + TeamWriter (else the pack's teams/workflows are silently skipped); vpBuildReports
// wires the ReportWriter (its teams/workflows skip cleanly — the report test does not assert them).
package vertical_pack

import (
	"context"
	"math"
	"strconv"
	"testing"

	"go.uber.org/zap"

	"nexus/platform/internal/auth"
	"nexus/platform/internal/dashboard"
	"nexus/platform/internal/dynrepo"
	"nexus/platform/internal/metadata"
	"nexus/platform/internal/services"
	"nexus/platform/internal/workflow"
)

// TestVerticalPack_InstallPM installs the built-in PM pack and asserts the full surface plus the
// discriminating per-project rollup (@proj_web: task_count 3, total_estimate 16).
func TestVerticalPack_InstallPM(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, workflowSvc := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// PACK-TEAMS: seed a REAL install user so the teams pass can add it as the seeded team's first
	// member — auth.TeamRepository.AddMember rejects a non-live user (ErrMemberUserNotFound), and the
	// round-robin resolver only counts status='active' members. tenants(vpTenantA) is seeded by vpSetup.
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@pm-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack, ok := BuiltinPack("pm")
	if !ok {
		t.Fatal("builtin pm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin pm: %v", err)
	}

	// --- #1: 2 entities created; pm_projects/pm_tasks exist (metadata). ---
	if len(res.EntitiesCreated) != 2 {
		t.Errorf("pm: created %v, want 2 entities", res.EntitiesCreated)
	}
	metaRepo := metadata.NewRepository(gdb)
	entities, err := metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	entByName := map[string]metadata.EntityDefinition{}
	for _, e := range entities {
		entByName[e.Name] = e
	}
	for _, want := range []string{"pm_projects", "pm_tasks"} {
		if _, ok := entByName[want]; !ok {
			t.Fatalf("pm: entity %q not found after install; got %v", want, keys(entByName))
		}
	}

	// --- #2: deferred rollup fields task_count + total_estimate exist on pm_projects. ---
	projFields, err := metaRepo.ListFields(ctx, vpTenantA, entByName["pm_projects"].ID)
	if err != nil {
		t.Fatalf("list pm_projects fields: %v", err)
	}
	projFieldNames := map[string]metadata.FieldDefinition{}
	for _, f := range projFields {
		projFieldNames[f.Name] = f
	}
	for _, want := range []string{"task_count", "total_estimate"} {
		f, ok := projFieldNames[want]
		if !ok {
			t.Fatalf("pm: deferred rollup field pm_projects.%s missing; got %v", want, keys(projFieldNames))
		}
		if f.FieldType != "rollup" {
			t.Errorf("pm: pm_projects.%s fieldType = %q, want rollup", want, f.FieldType)
		}
	}
	if len(res.DeferredFieldsAdded) != 2 {
		t.Errorf("pm: DeferredFieldsAdded = %v, want 2 (task_count + total_estimate)", res.DeferredFieldsAdded)
	}

	// --- #3: team "Đội dự án" created with the install user as its first active member. ---
	if len(res.TeamsCreated) != 1 || res.TeamsCreated[0] != "Đội dự án" {
		t.Fatalf("pm: TeamsCreated = %v, want [Đội dự án]", res.TeamsCreated)
	}
	teamRepo := auth.NewTeamRepository(gdb)
	teams, err := teamRepo.ListTeams(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	var teamID string
	for _, tm := range teams {
		if tm.Name == "Đội dự án" {
			teamID = tm.ID
		}
	}
	if teamID == "" {
		t.Fatalf("team 'Đội dự án' not found after install; got %+v", teams)
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
		t.Errorf("install user %s is not a member of team 'Đội dự án'; members = %+v", vpSeedUserID, members)
	}

	// --- #4: three workflows created; routing team resolved to id; timers decode. ---
	if len(res.WorkflowsCreated) != 3 {
		t.Fatalf("pm: WorkflowsCreated = %v, want 3 (routing + reminder + escalation)", res.WorkflowsCreated)
	}
	rules, err := workflowSvc.ListWorkflows(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list workflows: %v", err)
	}
	byName := map[string]workflow.WorkflowRecord{}
	for _, r := range rules {
		byName[r.Name] = r
	}

	// ASSIGN-1 routing rule: created, and its assign action's team resolved NAME → the team id. The
	// RecordService in this harness uses the NOOP workflow engine (vpBuildWF does not call WithEngine),
	// so an after_create rule does NOT fire on a record insert — assert the STORED RULE CONFIG.
	routing, ok := byName["Phân bổ công việc"]
	if !ok || routing.Trigger != "after_create" || routing.EntityName != "pm_tasks" {
		t.Fatalf("routing rule = %+v, want trigger after_create on pm_tasks", routing)
	}
	routeWF, err := routing.ToWorkflow()
	if err != nil {
		t.Fatalf("decode routing rule: %v", err)
	}
	if len(routeWF.Actions) != 1 {
		t.Fatalf("routing rule actions = %+v, want exactly 1 assign action", routeWF.Actions)
	}
	act := routeWF.Actions[0]
	if act.Type != "assign" || act.Strategy != "round_robin" || act.Field != "assignee" {
		t.Errorf("routing action = %+v, want {assign, round_robin, assignee}", act)
	}
	if act.Team != teamID {
		t.Errorf("routing action team = %q, want the resolved team id %q (NAME must resolve to id)", act.Team, teamID)
	}
	if act.Team == "Đội dự án" {
		t.Errorf("routing action team is the literal NAME — name→id resolution did not run")
	}

	// TIMER-1 reminder: decodes back to a date_offset timer on due_date, 1 day before.
	reminder, ok := byName["Nhắc hạn công việc"]
	if !ok || reminder.Trigger != "date_offset" || reminder.EntityName != "pm_tasks" {
		t.Fatalf("reminder rule = %+v, want trigger date_offset on pm_tasks", reminder)
	}
	reminderWF, err := reminder.ToWorkflow()
	if err != nil {
		t.Fatalf("decode reminder condition: %v", err)
	}
	if reminderWF.Condition == nil || reminderWF.Condition.Timer == nil {
		t.Fatalf("reminder rule has no persisted timer config; condition = %+v", reminderWF.Condition)
	}
	if tm := reminderWF.Condition.Timer; tm.DateField != "due_date" || tm.Offset.Amount != 1 ||
		tm.Offset.Unit != "days" || tm.Direction != "before" {
		t.Errorf("reminder timer = %+v, want {due_date, {1 days}, before}", *reminderWF.Condition.Timer)
	}

	// --- Escalation: the past-due escalation rule, decoded and asserted in full. ---
	// This is the discriminating assertion: a broken compose (wrong timer direction, dropped/partial
	// status gate, wrong/missing action) must fail here. It re-reads the STORED rule (config-validated
	// through the real WorkflowService), so it also proves toWorkflowInput threaded Match + Rules +
	// Timer into the persisted condition. The gate is match "or" over THREE eq rules (the three
	// non-done statuses) — the "or"+3-eq persist/decode path is identical to Ticketing's 2-eq (no rule
	// cap); the discriminator is that all three non-done statuses are present and "Đã xong" is absent.
	esc, ok := byName["Leo thang công việc trễ"]
	if !ok || esc.Trigger != "date_offset" || esc.EntityName != "pm_tasks" {
		t.Fatalf("escalation rule = %+v, want trigger date_offset on pm_tasks", esc)
	}
	escWF, err := esc.ToWorkflow()
	if err != nil {
		t.Fatalf("decode escalation rule: %v", err)
	}
	if escWF.Condition == nil {
		t.Fatalf("escalation rule has no condition")
	}
	// Timer: fire 1 day AFTER due_date (the whole point — "after", not "before").
	if tm := escWF.Condition.Timer; tm == nil || tm.DateField != "due_date" || tm.Offset.Amount != 1 ||
		tm.Offset.Unit != "days" || tm.Direction != "after" {
		t.Errorf("escalation timer = %+v, want {due_date, {1 days}, after}", escWF.Condition.Timer)
	}
	// Status gate: match "or" over [status eq To Do, status eq Đang làm, status eq Bị chặn] — only a
	// still-open task escalates. If a status were dropped or "Đã xong" leaked in, this fails.
	if escWF.Condition.Match != "or" {
		t.Errorf("escalation condition match = %q, want %q (membership gate needs any-of)", escWF.Condition.Match, "or")
	}
	if len(escWF.Condition.Rules) != 3 {
		t.Fatalf("escalation condition rules = %+v, want 3 (status eq To Do/Đang làm/Bị chặn)", escWF.Condition.Rules)
	}
	wantRules := map[string]bool{"To Do": false, "Đang làm": false, "Bị chặn": false}
	for _, r := range escWF.Condition.Rules {
		if r.Field != "status" || r.Op != "eq" {
			t.Errorf("escalation rule = %+v, want {status, eq, <status>}", r)
			continue
		}
		v, _ := r.Value.(string)
		if _, want := wantRules[v]; !want {
			t.Errorf("escalation rule value = %q, want one of To Do/Đang làm/Bị chặn (Đã xong must NOT be a gate value)", v)
			continue
		}
		wantRules[v] = true
	}
	for v, seen := range wantRules {
		if !seen {
			t.Errorf("escalation condition missing rule for status %q", v)
		}
	}
	// Actions: field_update priority → "Khẩn", then an owner notification — in that order.
	if len(escWF.Actions) != 2 {
		t.Fatalf("escalation actions = %+v, want 2 (field_update, notification)", escWF.Actions)
	}
	if a0 := escWF.Actions[0]; a0.Type != "field_update" || a0.Field != "priority" || a0.Value != "Khẩn" {
		t.Errorf("escalation action[0] = %+v, want {field_update, priority, Khẩn}", a0)
	}
	if a1 := escWF.Actions[1]; a1.Type != "in_app_notification" || a1.UserID != "$owner" || a1.Message == "" {
		t.Errorf("escalation action[1] = %+v, want {in_app_notification, $owner, <message>}", a1)
	}

	// Read-side RecordService with the SAME production wiring as vpBuild's record writer (rollup opt-in
	// needs WithRollupMeta/WithRollupLinked).
	dyn := dynrepo.New(sdb)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// --- #5: samples seeded — 3 projects + 5 tasks = 8 (per-entity distribution). ---
	if res.SamplesInserted != 8 {
		t.Errorf("pm: SamplesInserted = %d, want 8 (3 projects + 5 tasks)", res.SamplesInserted)
	}
	projList, err := recordSvc.List(ctx, scope, vpTenantA, "pm_projects", services.ListQuery{Page: 1, Limit: 50, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list pm_projects: %v", err)
	}
	if len(projList.Records) != 3 {
		t.Errorf("pm: %d projects, want 3", len(projList.Records))
	}
	taskList, err := recordSvc.List(ctx, scope, vpTenantA, "pm_tasks", services.ListQuery{Page: 1, Limit: 50, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list pm_tasks: %v", err)
	}
	if len(taskList.Records) != 5 {
		t.Errorf("pm: %d tasks, want 5", len(taskList.Records))
	}

	// --- Escalation does NOT fire on install/seeding. Pack workflows install in Pass 8 (AFTER the
	// Pass 3 samples), and this harness wires the NOOP workflow engine (vpBuildWF), so no after_*/
	// date_offset rule runs on the seed inserts. Concretely: exactly ONE seed task ("API đăng nhập")
	// carries priority "Khẩn"; the other four non-done seeds are NOT Khẩn. If the escalation rule had
	// retro-fired it would bump those non-done tasks to Khẩn → >1. Assert the count stays 1. ---
	khanCount := 0
	for _, r := range taskList.Records {
		if p, _ := r.Data["priority"].(string); p == "Khẩn" {
			khanCount++
		}
	}
	if khanCount != 1 {
		t.Errorf("pm: %d tasks in priority Khẩn after install, want 1 (escalation must NOT fire on seed)", khanCount)
	}

	// --- #6: THE KEY ASSERTION — the per-project rollup works E2E. ---
	// Find @proj_web by its unique code, then read it WITH the rollup opt-in (exactly how the LMS/CRM
	// tests read a record with ?rollup=). task_count is a COUNT rollup → int64 at the service layer;
	// total_estimate is a SUM over the number field `estimate_hours` → the exact DECIMAL string. The
	// values are DISCRIMINATING against grouping: @proj_web has 3 tasks (est 8/5/3), @proj_mobile has
	// 2 (est 4/6). A rollup that ignored viaRelation grouping would return count 5 / total 26.
	var webProjID string
	for _, r := range projList.Records {
		if code, _ := r.Data["code"].(string); code == "PRJ-001" {
			webProjID = r.ID
		}
	}
	if webProjID == "" {
		t.Fatal("could not find @proj_web project (code PRJ-001)")
	}
	webProj, err := recordSvc.Get(ctx, scope, vpTenantA, "pm_projects", webProjID, nil, []string{"task_count", "total_estimate"})
	if err != nil {
		t.Fatalf("get @proj_web with rollups: %v", err)
	}

	// task_count: COUNT rollup → int64 (service layer) or float64 (JSON round-trip). Normalise.
	gotCount, ok := lmsAsInt(webProj.Data["task_count"])
	if !ok {
		t.Fatalf("@proj_web task_count = %v (%T), want an integer", webProj.Data["task_count"], webProj.Data["task_count"])
	}
	if gotCount != 3 {
		t.Errorf("@proj_web task_count = %d, want 3 (three seeded tasks — NOT the whole-table 5)", gotCount)
	}

	// total_estimate: SUM over a NON-currency number field → a bare DECIMAL string. Fail loudly if it
	// is not a string (a currency map / float would mean the rollup shaping regressed), then parse
	// numerically and compare to 16 (8+5+3) — NOT the whole-table 26 (a broken per-project grouping).
	totalRaw, ok := webProj.Data["total_estimate"].(string)
	if !ok {
		t.Fatalf("@proj_web total_estimate = %v (%T), want a bare DECIMAL string (non-currency SUM rollup)", webProj.Data["total_estimate"], webProj.Data["total_estimate"])
	}
	gotTotal, err := strconv.ParseFloat(totalRaw, 64)
	if err != nil {
		t.Fatalf("@proj_web total_estimate %q does not parse as a number: %v", totalRaw, err)
	}
	const wantTotal = 8.0 + 5.0 + 3.0 // = 16, sum of @proj_web task estimate_hours
	if math.Abs(gotTotal-wantTotal) > 1e-6 {
		t.Errorf("@proj_web total_estimate = %v (%q), want %v (8+5+3 — NOT the whole-table 26)", gotTotal, totalRaw, wantTotal)
	}

	// --- Dashboard: created by name, and its 3 widgets RUN cleanly (a bad groupBy surfaces only at
	// run-time; runWidget captures per-widget errors + still 200s). ---
	if len(res.DashboardsCreated) != 1 || res.DashboardsCreated[0] != "Tổng quan Dự án" {
		t.Errorf("pm: DashboardsCreated = %v, want [Tổng quan Dự án]", res.DashboardsCreated)
	}
	dashRepo := dashboard.NewSQLRepository(sdb)
	dashRows, err := dashRepo.List(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list dashboards: %v", err)
	}
	if len(dashRows) != 1 {
		t.Fatalf("expected 1 dashboard row, got %d", len(dashRows))
	}
	dashSvc := services.NewDashboardService(dashRepo, recordSvc, zap.NewNop())
	runResult, err := dashSvc.RunDashboard(ctx, scope, vpTenantA, dashRows[0].ID, nil)
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
	// w1: total task count (scalar KPI) → 5.
	if w1 := findWidget("w1"); w1 == nil {
		t.Fatal("widget w1 (total task count) not found")
	} else if w1.Error != "" {
		t.Fatalf("widget w1 error: %s (code: %s)", w1.Error, w1.Code)
	} else if w1.Result == nil || w1.Result.Grouped || w1.Result.Scalar.Count != 5 {
		t.Errorf("widget w1 = %+v, want scalar count 5", w1.Result)
	}
	// w2: count by task status — 5 tasks span 4 distinct status values (To Do×2, Đang làm, Đã xong,
	// Bị chặn) → 4 groups. The indexed generated column makes this a clean GROUP BY.
	if w2 := findWidget("w2"); w2 == nil {
		t.Fatal("widget w2 (count by task status) not found")
	} else if w2.Error != "" {
		t.Fatalf("widget w2 error: %s (code: %s)", w2.Error, w2.Code)
	} else if w2.Result == nil || !w2.Result.Grouped || len(w2.Result.Groups) != 4 {
		t.Errorf("widget w2 groups = %+v, want 4 (To Do/Đang làm/Đã xong/Bị chặn)", w2.Result)
	}
	// w3: count by task priority — 5 tasks span 4 distinct priority values (Cao×2, Khẩn, Trung bình,
	// Thấp) → 4 groups.
	if w3 := findWidget("w3"); w3 == nil {
		t.Fatal("widget w3 (count by task priority) not found")
	} else if w3.Error != "" {
		t.Fatalf("widget w3 error: %s (code: %s)", w3.Error, w3.Code)
	} else if w3.Result == nil || !w3.Result.Grouped || len(w3.Result.Groups) != 4 {
		t.Errorf("widget w3 groups = %+v, want 4 (Thấp/Trung bình/Cao/Khẩn)", w3.Result)
	}

	// --- #7: idempotent re-install — entities/team/workflows skipped, exactly one team. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install pm into same tenant: %v", err)
	}
	if len(res2.EntitiesCreated) != 0 || len(res2.EntitiesSkipped) != 2 {
		t.Errorf("re-install: entities created %v / skipped %v, want 0 / 2", res2.EntitiesCreated, res2.EntitiesSkipped)
	}
	if len(res2.TeamsCreated) != 0 || len(res2.TeamsSkipped) != 1 || res2.TeamsSkipped[0] != "Đội dự án" {
		t.Errorf("re-install teams: created %v / skipped %v, want [] / [Đội dự án]", res2.TeamsCreated, res2.TeamsSkipped)
	}
	if len(res2.WorkflowsCreated) != 0 || len(res2.WorkflowsSkipped) != 3 {
		t.Errorf("re-install workflows: created %v / skipped %v, want 0 / 3 (all by name)", res2.WorkflowsCreated, res2.WorkflowsSkipped)
	}
	teams2, _ := teamRepo.ListTeams(ctx, vpTenantA)
	nTeam := 0
	for _, tm := range teams2 {
		if tm.Name == "Đội dự án" {
			nTeam++
		}
	}
	if nTeam != 1 {
		t.Errorf("after re-install, %d teams named 'Đội dự án', want 1 (no dup)", nTeam)
	}
}

// TestVerticalPack_ReportsPM installs the built-in PM pack through the report-wired harness and
// asserts both canned reports are created and RUN correctly: the summary "Công việc theo trạng
// thái" returns the seeded COUNT-by-status distribution, and the tabular "Danh sách việc đang mở"
// filters to EXACTLY the non-done tasks (status in {To Do, Đang làm, Bị chặn}) — 4 of the 5 seeded
// tasks (the one "Đã xong" is excluded). A broken filter would return all 5, failing the assertion.
func TestVerticalPack_ReportsPM(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, reportSvc := vpBuildReports(gdb, sdb)
	ctx := context.Background()

	pack, ok := BuiltinPack("pm")
	if !ok {
		t.Fatal("builtin pm pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install pm into A: %v", err)
	}
	if !vpReportSetEqual(res.ReportsCreated, "Công việc theo trạng thái", "Danh sách việc đang mở") {
		t.Fatalf("ReportsCreated = %v, want [Công việc theo trạng thái, Danh sách việc đang mở]", res.ReportsCreated)
	}

	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// --- Summary report: COUNT GROUP BY status over the 5 seeded tasks. ---
	rec := vpFindReport(t, ctx, reportSvc, vpTenantA, "Công việc theo trạng thái")
	if rec.EntityName != "pm_tasks" || rec.Type != "summary" || !rec.IsActive {
		t.Errorf("report = {entity:%q type:%q active:%v}, want {pm_tasks summary true}", rec.EntityName, rec.Type, rec.IsActive)
	}
	run, err := reportSvc.RunReport(ctx, scope, vpTenantA, rec.ID, nil)
	if err != nil {
		t.Fatalf("run summary report: %v", err)
	}
	if run.Aggregate == nil || !run.Aggregate.Grouped {
		t.Fatalf("run result = %+v, want a grouped aggregate", run)
	}
	counts := map[string]int64{}
	for _, g := range run.Aggregate.Groups {
		if g.Key == nil {
			t.Errorf("report group with nil status key: %+v", g)
			continue
		}
		counts[*g.Key] = g.Count
	}
	wantCounts := map[string]int64{"To Do": 2, "Đang làm": 1, "Đã xong": 1, "Bị chặn": 1}
	if len(counts) != len(wantCounts) {
		t.Errorf("report groups = %v, want %v", counts, wantCounts)
	}
	for status, want := range wantCounts {
		if got := counts[status]; got != want {
			t.Errorf("report COUNT for status %q = %d, want %d", status, got, want)
		}
	}

	// --- Tabular report: status IN {To Do, Đang làm, Bị chặn} over the 5 seeded tasks → exactly 4
	// rows (the one "Đã xong" excluded). Exercises the `in` operator (three values) on a picklist. ---
	wl := vpFindReport(t, ctx, reportSvc, vpTenantA, "Danh sách việc đang mở")
	if wl.EntityName != "pm_tasks" || wl.Type != "tabular" || !wl.IsActive {
		t.Errorf("worklist report = {entity:%q type:%q active:%v}, want {pm_tasks tabular true}", wl.EntityName, wl.Type, wl.IsActive)
	}
	wlRun, err := reportSvc.RunReport(ctx, scope, vpTenantA, wl.ID, nil)
	if err != nil {
		t.Fatalf("run worklist report: %v", err)
	}
	if wlRun.Tabular == nil {
		t.Fatalf("worklist run result = %+v, want a tabular result", wlRun)
	}
	wantCols := []string{"title", "project", "priority", "status", "due_date", "assignee"}
	if !vpStrSliceEqual(wlRun.Tabular.Columns, wantCols) {
		t.Errorf("worklist columns = %v, want %v", wlRun.Tabular.Columns, wantCols)
	}
	if wlRun.Tabular.Total != 4 || len(wlRun.Tabular.Records) != 4 {
		t.Fatalf("worklist rows = total %d / %d records, want 4 (non-done tasks; filter broken if 5)", wlRun.Tabular.Total, len(wlRun.Tabular.Records))
	}
	open := map[string]bool{"To Do": true, "Đang làm": true, "Bị chặn": true}
	for _, r := range wlRun.Tabular.Records {
		s, _ := r.Data["status"].(string)
		if !open[s] {
			t.Errorf("worklist row status = %q, want one of To Do/Đang làm/Bị chặn; data=%v", s, r.Data)
		}
		if title, _ := r.Data["title"].(string); title == "" {
			t.Errorf("worklist row title is empty, want the seeded title; data=%v", r.Data)
		}
		if pri, _ := r.Data["priority"].(string); pri == "" {
			t.Errorf("worklist row priority is empty, want the seeded priority; data=%v", r.Data)
		}
		// The relation column `project` is a STORED generated column off data.$.project → the target
		// UUID; it must come back populated (proves relation columns project through the tabular run).
		// `assignee` is deliberately NOT asserted — it is routing-filled and never seeded.
		// #15/#16: the relation column resolves to {id,label}, so a task list names its project
		// instead of printing a UUID. Both halves asserted — a regression to the bare id fails here.
		if id, label := vpRelationCell(t, r.Data["project"]); id == "" || label == "" {
			t.Errorf("worklist row project = {id:%q label:%q}, want both set (relation label, #15/#16); data=%v", id, label, r.Data)
		}
	}

	// --- Re-install: both reports already exist by name → skipped, no duplicate. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install pm: %v", err)
	}
	if len(res2.ReportsCreated) != 0 || len(res2.ReportsSkipped) != 2 {
		t.Errorf("re-install reports: created %v / skipped %v, want 0 / 2 (idempotent by name)", res2.ReportsCreated, res2.ReportsSkipped)
	}
	after, err := reportSvc.ListReports(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list reports after re-install: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("reports after re-install = %d, want 2 (no duplicate)", len(after))
	}
}
