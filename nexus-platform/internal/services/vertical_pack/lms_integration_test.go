//go:build integration

// Integration test for the built-in LMS/Education vertical pack (campaign-3 finale — the 3rd shipped
// vertical after CRM + Ticketing). It installs lmsPack end-to-end against real MySQL and proves the
// whole pack is config-valid AND that its headline primitive — the parent-side GRADEBOOK ROLLUP —
// works E2E: 3 entities created, the two deferred rollup fields (enrollment_count + avg_grade) added
// on lms_courses, the "Đội Tư vấn" team seeded with the install user, the two PACK-WORKFLOWS rules
// created (the ASSIGN-1 routing rule's team NAME resolved to the seeded UUID, the TIMER-1 date_offset
// decoded to a tuition_due reminder, and the v1.1.0 TUITION-OVERDUE escalation decoded to a
// {tuition_due, 1 day, after} timer gated on status "Chờ đóng" → field_update Quá hạn + owner notify),
// the 13 samples seeded (3/4/6), and — THE KEY ASSERTION — reading
// the @co_go course with ?rollup= returns enrollment_count == 3 and avg_grade == 8.4 (the exact mean
// of its three seeded grades 8.5/7.5/9.2). Re-install is idempotent (entities/team/workflows skipped).
//
// It reuses the harness in vertical_pack_integration_test.go (vpSetup / vpBuildWF / vpTenantA /
// vpSeedUserID). vpBuildWF (not vpBuild) is used because it is the only build helper that wires the
// WorkflowWriter + TeamWriter — without them the pack's teams/workflows are silently skipped.
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

// TestVerticalPack_InstallLMS installs the built-in LMS pack and asserts the full surface plus the
// discriminating gradebook rollup (@co_go: count 3, avg 8.4).
func TestVerticalPack_InstallLMS(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, workflowSvc := vpBuildWF(gdb, sdb)
	ctx := context.Background()

	// PACK-TEAMS: seed a REAL install user so the teams pass can add it as the seeded team's first
	// member — auth.TeamRepository.AddMember rejects a non-live user (ErrMemberUserNotFound), and the
	// round-robin resolver only counts status='active' members. tenants(vpTenantA) is seeded by vpSetup.
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@lms-it.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}

	pack, ok := BuiltinPack("lms")
	if !ok {
		t.Fatal("builtin lms pack not found")
	}
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("install builtin lms: %v", err)
	}

	// --- #1: 3 entities created; lms_courses/lms_students/lms_enrollments exist (metadata). ---
	if len(res.EntitiesCreated) != 3 {
		t.Errorf("lms: created %v, want 3 entities", res.EntitiesCreated)
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
	for _, want := range []string{"lms_courses", "lms_students", "lms_enrollments"} {
		if _, ok := entByName[want]; !ok {
			t.Fatalf("lms: entity %q not found after install; got %v", want, keys(entByName))
		}
	}

	// --- #2: deferred rollup fields enrollment_count + avg_grade exist on lms_courses. ---
	courseFields, err := metaRepo.ListFields(ctx, vpTenantA, entByName["lms_courses"].ID)
	if err != nil {
		t.Fatalf("list lms_courses fields: %v", err)
	}
	courseFieldNames := map[string]metadata.FieldDefinition{}
	for _, f := range courseFields {
		courseFieldNames[f.Name] = f
	}
	for _, want := range []string{"enrollment_count", "avg_grade"} {
		f, ok := courseFieldNames[want]
		if !ok {
			t.Fatalf("lms: deferred rollup field lms_courses.%s missing; got %v", want, keys(courseFieldNames))
		}
		if f.FieldType != "rollup" {
			t.Errorf("lms: lms_courses.%s fieldType = %q, want rollup", want, f.FieldType)
		}
	}
	// --- #243: assigned_advisor is a `user` field on a fresh install (screen shows a name). ---
	if got := lmsAdvisorFieldType(t, metaRepo, entByName["lms_enrollments"].ID); got != "user" {
		t.Errorf("lms: lms_enrollments.assigned_advisor fieldType = %q, want user", got)
	}
	// The result summary also records both deferred fields as added in Pass 2b.
	if len(res.DeferredFieldsAdded) != 2 {
		t.Errorf("lms: DeferredFieldsAdded = %v, want 2 (enrollment_count + avg_grade)", res.DeferredFieldsAdded)
	}

	// --- #3: team "Đội Tư vấn" created with the install user as its first active member. ---
	if len(res.TeamsCreated) != 1 || res.TeamsCreated[0] != "Đội Tư vấn" {
		t.Fatalf("lms: TeamsCreated = %v, want [Đội Tư vấn]", res.TeamsCreated)
	}
	teamRepo := auth.NewTeamRepository(gdb)
	teams, err := teamRepo.ListTeams(ctx, vpTenantA)
	if err != nil {
		t.Fatalf("list teams: %v", err)
	}
	var teamID string
	for _, tm := range teams {
		if tm.Name == "Đội Tư vấn" {
			teamID = tm.ID
		}
	}
	if teamID == "" {
		t.Fatalf("team 'Đội Tư vấn' not found after install; got %+v", teams)
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
		t.Errorf("install user %s is not a member of team 'Đội Tư vấn'; members = %+v", vpSeedUserID, members)
	}

	// --- #4: three workflows created; routing team resolved to UUID; timers decode to date_offset. ---
	if len(res.WorkflowsCreated) != 3 {
		t.Fatalf("lms: WorkflowsCreated = %v, want 3 (tuition reminder + advisor routing + overdue escalation)", res.WorkflowsCreated)
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
	routing, ok := byName["Phân bổ tư vấn viên"]
	if !ok || routing.Trigger != "after_create" || routing.EntityName != "lms_enrollments" {
		t.Fatalf("routing rule = %+v, want trigger after_create on lms_enrollments", routing)
	}
	routeWF, err := routing.ToWorkflow()
	if err != nil {
		t.Fatalf("decode routing rule: %v", err)
	}
	if len(routeWF.Actions) != 1 {
		t.Fatalf("routing rule actions = %+v, want exactly 1 assign action", routeWF.Actions)
	}
	act := routeWF.Actions[0]
	if act.Type != "assign" || act.Strategy != "round_robin" || act.Field != "assigned_advisor" {
		t.Errorf("routing action = %+v, want {assign, round_robin, assigned_advisor}", act)
	}
	if act.Team != teamID {
		t.Errorf("routing action team = %q, want the resolved team id %q (NAME must resolve to id)", act.Team, teamID)
	}
	if act.Team == "Đội Tư vấn" {
		t.Errorf("routing action team is the literal NAME — name→id resolution did not run")
	}

	// TIMER-1 rule: decodes back to a date_offset timer on tuition_due, 3 days before.
	timer, ok := byName["Nhắc đóng học phí"]
	if !ok || timer.Trigger != "date_offset" || timer.EntityName != "lms_enrollments" {
		t.Fatalf("timer rule = %+v, want trigger date_offset on lms_enrollments", timer)
	}
	timerWF, err := timer.ToWorkflow()
	if err != nil {
		t.Fatalf("decode timer condition: %v", err)
	}
	if timerWF.Condition == nil || timerWF.Condition.Timer == nil {
		t.Fatalf("timer rule has no persisted timer config; condition = %+v", timerWF.Condition)
	}
	if tm := timerWF.Condition.Timer; tm.DateField != "tuition_due" || tm.Offset.Amount != 3 ||
		tm.Offset.Unit != "days" || tm.Direction != "before" {
		t.Errorf("timer = %+v, want {tuition_due, {3 days}, before}", *timerWF.Condition.Timer)
	}

	// --- TUITION-OVERDUE (v1.1.0): the past-due escalation rule, decoded and asserted in full. ---
	// This is the discriminating assertion: a broken compose (wrong timer direction, dropped status
	// gate, wrong/missing action) must fail here. It re-reads the STORED rule (config-validated
	// through the real WorkflowService), so it also proves toWorkflowInput threaded Match + Rules +
	// Timer (not just Timer) into the persisted condition. Unlike the Ticketing escalation this has
	// NO assign action and NO new team — just field_update + notification, so exactly 2 actions.
	esc, ok := byName["Đánh dấu quá hạn học phí"]
	if !ok || esc.Trigger != "date_offset" || esc.EntityName != "lms_enrollments" {
		t.Fatalf("overdue rule = %+v, want trigger date_offset on lms_enrollments", esc)
	}
	escWF, err := esc.ToWorkflow()
	if err != nil {
		t.Fatalf("decode overdue rule: %v", err)
	}
	if escWF.Condition == nil {
		t.Fatalf("overdue rule has no condition")
	}
	// Timer: fire 1 day AFTER tuition_due (the whole point — "after", not "before").
	if tm := escWF.Condition.Timer; tm == nil || tm.DateField != "tuition_due" || tm.Offset.Amount != 1 ||
		tm.Offset.Unit != "days" || tm.Direction != "after" {
		t.Errorf("overdue timer = %+v, want {tuition_due, {1 days}, after}", escWF.Condition.Timer)
	}
	// Status gate: a single {status, eq, "Chờ đóng"} rule — only a still-pending enrolment escalates.
	// If this were dropped (empty rules), EVERY past-due enrolment (incl. paid/already-overdue) would
	// wrongly flip to Quá hạn. Default match "and" over one rule.
	if len(escWF.Condition.Rules) != 1 {
		t.Fatalf("overdue condition rules = %+v, want exactly 1 (status eq Chờ đóng)", escWF.Condition.Rules)
	}
	if r := escWF.Condition.Rules[0]; r.Field != "status" || r.Op != "eq" || r.Value != "Chờ đóng" {
		t.Errorf("overdue gate rule = %+v, want {status, eq, Chờ đóng}", r)
	}
	// Actions: field_update status → "Quá hạn", then an owner notification — in that order.
	if len(escWF.Actions) != 2 {
		t.Fatalf("overdue actions = %+v, want 2 (field_update, notification)", escWF.Actions)
	}
	if a0 := escWF.Actions[0]; a0.Type != "field_update" || a0.Field != "status" || a0.Value != "Quá hạn" {
		t.Errorf("overdue action[0] = %+v, want {field_update, status, Quá hạn}", a0)
	}
	if a1 := escWF.Actions[1]; a1.Type != "in_app_notification" || a1.UserID != "$owner" || a1.Message == "" {
		t.Errorf("overdue action[1] = %+v, want {in_app_notification, $owner, <message>}", a1)
	}

	// Read-side RecordService with the SAME production wiring as vpBuild's record writer (rollup opt-in
	// needs WithRollupMeta/WithRollupLinked; lookup needs WithFieldValueFetcher).
	dyn := dynrepo.New(sdb)
	recordSvc := services.NewRecordService(metaRepo, dyn).
		WithLabelFetcher(dyn).
		WithFieldValueFetcher(dyn).
		WithRollupMeta(metaRepo).
		WithRollupLinked(dyn).
		WithSequenceClaimer(dyn)
	scope := services.RecordScope{UserID: vpSeedUserID, AccessLevel: "all", IsAdmin: true}

	// --- #5: samples seeded — 3 courses + 4 students + 6 enrollments = 13 (per-entity distribution). ---
	if res.SamplesInserted != 13 {
		t.Errorf("lms: SamplesInserted = %d, want 13 (3 courses + 4 students + 6 enrollments)", res.SamplesInserted)
	}
	courseList, err := recordSvc.List(ctx, scope, vpTenantA, "lms_courses", services.ListQuery{Page: 1, Limit: 50, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list lms_courses: %v", err)
	}
	if len(courseList.Records) != 3 {
		t.Errorf("lms: %d courses, want 3", len(courseList.Records))
	}
	studentList, err := recordSvc.List(ctx, scope, vpTenantA, "lms_students", services.ListQuery{Page: 1, Limit: 50, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list lms_students: %v", err)
	}
	if len(studentList.Records) != 4 {
		t.Errorf("lms: %d students, want 4", len(studentList.Records))
	}
	enrollList, err := recordSvc.List(ctx, scope, vpTenantA, "lms_enrollments", services.ListQuery{Page: 1, Limit: 50, Sort: "created_at", Order: "asc"})
	if err != nil {
		t.Fatalf("list lms_enrollments: %v", err)
	}
	if len(enrollList.Records) != 6 {
		t.Errorf("lms: %d enrollments, want 6", len(enrollList.Records))
	}

	// --- TUITION-OVERDUE does NOT fire on install/seeding. Pack workflows install in Pass 8 (AFTER
	// the Pass 3 samples), and this harness wires the NOOP workflow engine (vpBuildWF), so no
	// after_*/date_offset rule runs on the seed inserts. Concretely: the seed literals carry exactly
	// ONE "Quá hạn" enrolment (@co_react/@s4); the three "Chờ đóng" seeds are preserved. If the
	// overdue rule had retro-fired it would flip those three to "Quá hạn" → 4. Assert the count stays
	// 1 — this guards the install-sequence invariant (same as the reminder/routing rules). ---
	overdueCount := 0
	for _, r := range enrollList.Records {
		if s, _ := r.Data["status"].(string); s == "Quá hạn" {
			overdueCount++
		}
	}
	if overdueCount != 1 {
		t.Errorf("lms: %d enrolments in status Quá hạn after install, want 1 (overdue workflow must NOT fire on seed)", overdueCount)
	}

	// --- #6: THE KEY ASSERTION — the gradebook rollup works E2E. ---
	// Find @co_go by its unique code, then read it WITH the rollup opt-in (exactly how the CRM tests
	// read a record with ?rollup=). enrollment_count is a COUNT rollup → int64 at the service layer;
	// avg_grade is an AVG over the number field `grade` → the exact DECIMAL string (ToJSONValue's
	// non-currency branch returns *DecimalValue verbatim, never float-parsed). @co_go has 3 enrolments
	// with grades 8.5/7.5/9.2 → count 3, avg (8.5+7.5+9.2)/3 = 8.4.
	var goCourseID string
	for _, r := range courseList.Records {
		if code, _ := r.Data["code"].(string); code == "GO-101" {
			goCourseID = r.ID
		}
	}
	if goCourseID == "" {
		t.Fatal("could not find @co_go course (code GO-101)")
	}
	goCourse, err := recordSvc.Get(ctx, scope, vpTenantA, "lms_courses", goCourseID, nil, []string{"enrollment_count", "avg_grade"})
	if err != nil {
		t.Fatalf("get @co_go with rollups: %v", err)
	}

	// enrollment_count: COUNT rollup → int64 (service layer) or float64 (JSON round-trip). Normalise.
	gotCount, ok := lmsAsInt(goCourse.Data["enrollment_count"])
	if !ok {
		t.Fatalf("@co_go enrollment_count = %v (%T), want an integer", goCourse.Data["enrollment_count"], goCourse.Data["enrollment_count"])
	}
	if gotCount != 3 {
		t.Errorf("@co_go enrollment_count = %d, want 3 (three seeded enrolments)", gotCount)
	}

	// avg_grade: AVG over a NON-currency number field → a bare DECIMAL string. Fail loudly if it is not
	// a string (a currency map / float would mean the rollup shaping regressed), then parse numerically
	// and compare to the expected mean derived from the SAME seed literals with a float epsilon.
	avgRaw, ok := goCourse.Data["avg_grade"].(string)
	if !ok {
		t.Fatalf("@co_go avg_grade = %v (%T), want a bare DECIMAL string (non-currency AVG rollup)", goCourse.Data["avg_grade"], goCourse.Data["avg_grade"])
	}
	gotAvg, err := strconv.ParseFloat(avgRaw, 64)
	if err != nil {
		t.Fatalf("@co_go avg_grade %q does not parse as a number: %v", avgRaw, err)
	}
	const wantAvg = (8.5 + 7.5 + 9.2) / 3.0 // = 8.4, computed from the @co_go seed grades
	if math.Abs(gotAvg-wantAvg) > 1e-6 {
		t.Errorf("@co_go avg_grade = %v (%q), want %v (mean of 8.5/7.5/9.2)", gotAvg, avgRaw, wantAvg)
	}

	// --- Dashboard: created by name, and its 3 widgets RUN cleanly (parity with the ticketing test —
	// a bad groupBy surfaces only at run-time; runWidget captures per-widget errors + still 200s). ---
	if len(res.DashboardsCreated) != 1 || res.DashboardsCreated[0] != "Tổng quan Đào tạo" {
		t.Errorf("lms: DashboardsCreated = %v, want [Tổng quan Đào tạo]", res.DashboardsCreated)
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
	// w1: total enrolment count (scalar KPI) → 6.
	if w1 := findWidget("w1"); w1 == nil {
		t.Fatal("widget w1 (total enrolment count) not found")
	} else if w1.Error != "" {
		t.Fatalf("widget w1 error: %s (code: %s)", w1.Error, w1.Code)
	} else if w1.Result == nil || w1.Result.Grouped || w1.Result.Scalar.Count != 6 {
		t.Errorf("widget w1 = %+v, want scalar count 6", w1.Result)
	}
	// w2: count by enrolment status — 6 enrolments span 3 distinct status values (Đã đóng×2, Chờ
	// đóng×3, Quá hạn×1) → 3 groups. The indexed generated column makes this a clean GROUP BY.
	if w2 := findWidget("w2"); w2 == nil {
		t.Fatal("widget w2 (count by enrolment status) not found")
	} else if w2.Error != "" {
		t.Fatalf("widget w2 error: %s (code: %s)", w2.Error, w2.Code)
	} else if w2.Result == nil || !w2.Result.Grouped || len(w2.Result.Groups) != 3 {
		t.Errorf("widget w2 groups = %+v, want 3 (Đã đóng/Chờ đóng/Quá hạn)", w2.Result)
	}
	// w3: count by course status — 3 courses span 3 distinct status values (Đang học/Đang mở/Nháp).
	if w3 := findWidget("w3"); w3 == nil {
		t.Fatal("widget w3 (count by course status) not found")
	} else if w3.Error != "" {
		t.Fatalf("widget w3 error: %s (code: %s)", w3.Error, w3.Code)
	} else if w3.Result == nil || !w3.Result.Grouped || len(w3.Result.Groups) != 3 {
		t.Errorf("widget w3 groups = %+v, want 3 (Đang học/Đang mở/Nháp)", w3.Result)
	}

	// --- #7: idempotent re-install — entities/team/workflows skipped, exactly one team. ---
	res2, err := svc.Install(ctx, vpTenantA, vpSeedUserID, pack)
	if err != nil {
		t.Fatalf("re-install lms into same tenant: %v", err)
	}
	if len(res2.EntitiesCreated) != 0 || len(res2.EntitiesSkipped) != 3 {
		t.Errorf("re-install: entities created %v / skipped %v, want 0 / 3", res2.EntitiesCreated, res2.EntitiesSkipped)
	}
	if len(res2.TeamsCreated) != 0 || len(res2.TeamsSkipped) != 1 || res2.TeamsSkipped[0] != "Đội Tư vấn" {
		t.Errorf("re-install teams: created %v / skipped %v, want [] / [Đội Tư vấn]", res2.TeamsCreated, res2.TeamsSkipped)
	}
	if len(res2.WorkflowsCreated) != 0 || len(res2.WorkflowsSkipped) != 3 {
		t.Errorf("re-install workflows: created %v / skipped %v, want 0 / 3 (all by name)", res2.WorkflowsCreated, res2.WorkflowsSkipped)
	}
	// No duplicate team row appeared for the same name (per-tenant UNIQUE(name) + skip-by-name).
	teams2, _ := teamRepo.ListTeams(ctx, vpTenantA)
	nAdvisor := 0
	for _, tm := range teams2 {
		if tm.Name == "Đội Tư vấn" {
			nAdvisor++
		}
	}
	if nAdvisor != 1 {
		t.Errorf("after re-install, %d teams named 'Đội Tư vấn', want 1 (no dup)", nAdvisor)
	}
}

// lmsAsInt normalises a rollup COUNT value: RollupResult.ToJSONValue emits int64 at the service layer;
// a JSON round-trip (HTTP path) would deliver float64. Accept both so the assertion pins the NUMBER,
// not the carrier type.
func lmsAsInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	}
	return 0, false
}

func lmsAdvisorFieldType(t *testing.T, repo *metadata.Repository, entityID string) string {
	t.Helper()
	fields, err := repo.ListFields(context.Background(), vpTenantA, entityID)
	if err != nil {
		t.Fatalf("list lms_enrollments fields: %v", err)
	}
	for _, f := range fields {
		if f.Name == "assigned_advisor" {
			return f.FieldType
		}
	}
	t.Fatal("lms_enrollments.assigned_advisor missing")
	return ""
}

// #243: a tenant installed before v1.4.0 has assigned_advisor as `text`. Re-installing the current
// pack must NOT retype it, and the routing rule keeps working (assignTargetTypeOK accepts text).
func TestVerticalPack_InstallLMS_LegacyTextAdvisorKept(t *testing.T) {
	gdb, sdb := vpSetup(t)
	svc, _ := vpBuildWF(gdb, sdb)
	ctx := context.Background()
	if err := gdb.Exec(
		"INSERT INTO users (id, tenant_id, email, password_hash, status) VALUES (?, ?, ?, ?, 'active')",
		vpSeedUserID, vpTenantA, "installer@lms-legacy.example.com", "x",
	).Error; err != nil {
		t.Fatalf("seed install user: %v", err)
	}
	legacy, _ := BuiltinPack("lms")
	for ei := range legacy.Entities {
		for fi := range legacy.Entities[ei].Fields {
			if f := &legacy.Entities[ei].Fields[fi]; f.Name == "assigned_advisor" {
				f.FieldType = "text"
			}
		}
	}
	if _, err := svc.Install(ctx, vpTenantA, vpSeedUserID, legacy); err != nil {
		t.Fatalf("install legacy lms: %v", err)
	}
	current, _ := BuiltinPack("lms")
	res, err := svc.Install(ctx, vpTenantA, vpSeedUserID, current)
	if err != nil {
		t.Fatalf("reinstall current lms: %v", err)
	}
	_ = res
	metaRepo := metadata.NewRepository(gdb)
	entities, err := metaRepo.ListEntities(vpTenantA)
	if err != nil {
		t.Fatalf("list entities: %v", err)
	}
	for _, e := range entities {
		if e.Name == "lms_enrollments" {
			if got := lmsAdvisorFieldType(t, metaRepo, e.ID); got != "text" {
				t.Errorf("legacy tenant assigned_advisor = %q, want text kept", got)
			}
			return
		}
	}
	t.Fatal("lms_enrollments not found")
}
