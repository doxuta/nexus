//go:build integration

// specs/043 S10 — `bpm-process-versions`: draft / publish / version history and the version every
// hồ sơ pins, proven against real MySQL (tasks T160–T165).
//
// What S10 promises:
//   - Migration C gives every live process a version 1 from its columns, adds the draft columns and
//     instance process_version, and goes down/up cleanly (down refuses while a draft would be lost);
//   - a DRAFT never starts a hồ sơ: arming reads the active definition only;
//   - PUBLISH makes the draft the next version in one transaction; hồ sơ started afterwards pin it,
//     hồ sơ started before keep theirs;
//   - the LEGACY save (admin builder / AI Builder / pack install) is "save + publish now", but a save
//     that changes nothing publishes nothing;
//   - history entries of a hồ sơ name the version it runs under (#70);
//   - a step id released by a deleted step is never handed to a different one (spec "Nợ còn mở" 15);
//   - a draft saved over an older version cannot silently revert a later change.
//
// Run with:
//
//	TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&multiStatements=true&loc=Local" \
//	  go test -tags=integration -race ./internal/services/ -run 'TestMigrationC_|TestDraft_|TestPublish_|TestLegacyPut_|TestVersions_|TestHistory_|TestPackExport_|TestSaveDraft_|TestDiscardDraft_'
package services

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/bpm"
)

const (
	verAp1       = "22222222-0000-0000-0000-000000000b01"
	verAp2       = "33333333-0000-0000-0000-000000000b02"
	verAp3       = "44444444-0000-0000-0000-000000000b03"
	verSubmitter = "11111111-0000-0000-0000-000000000b00"
)

// ---------------------------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------------------------

func verStep(id, name, approver string) ApprovalStepInput {
	return ApprovalStepInput{ID: id, Name: name, ApproverType: "user", ApproverID: approver}
}

// verProcess creates a manual process on a fresh entity and returns its id.
func verProcess(t *testing.T, st *bpmStack, tenant, entity string, steps ...ApprovalStepInput) string {
	t.Helper()
	ctx := context.Background()
	st.createEntity(t, ctx, tenant, entity, []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	rec, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "ver-" + entity, EntityName: entity, Trigger: "manual", Steps: steps, IsActive: boolPtr(true), ActorID: verSubmitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	return rec.ID
}

// verBody is the full-replace body of a save / draft of process `entity`.
func verBody(entity, name string, steps ...ApprovalStepInput) UpdateApprovalProcessInput {
	return UpdateApprovalProcessInput{
		Name: name, EntityName: entity, Trigger: "manual", Steps: steps, IsActive: boolPtr(true), ActorID: verSubmitter,
	}
}

// verSubmit starts a hồ sơ on a fresh record and returns the stored instance.
func verSubmit(t *testing.T, st *bpmStack, tenant, entity string) *bpm.Instance {
	t.Helper()
	ctx := context.Background()
	sub, err := st.requester.Submit(ctx, tenant, entity, uuid.NewString(), map[string]any{"status": "draft"}, bpm.Actor{UserID: verSubmitter})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	inst, err := st.repo.GetInstance(ctx, tenant, sub.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return inst
}

func stepIDsOf(inst *bpm.Instance) []string {
	var out []string
	for _, s := range inst.Definition.Chain() {
		out = append(out, s.ID)
	}
	return out
}

func versionCount(t *testing.T, db *sqlx.DB, tenant, process string) int {
	t.Helper()
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM sys_approval_process_versions WHERE tenant_id = ? AND process_id = ?`, tenant, process); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

func activeVersion(t *testing.T, db *sqlx.DB, tenant, process string) int {
	t.Helper()
	var v int
	if err := db.Get(&v, `SELECT version FROM sys_approval_processes WHERE tenant_id = ? AND id = ?`, tenant, process); err != nil {
		t.Fatalf("read version: %v", err)
	}
	return v
}

// ---------------------------------------------------------------------------------------------
// T160 — Migration C
// ---------------------------------------------------------------------------------------------

const migCSuffix = "_approval_process_versions"

func migCFile(t *testing.T, dir string) string {
	t.Helper()
	for _, f := range migFiles(t, "."+dir+".sql") {
		if strings.HasSuffix(f, migCSuffix+"."+dir+".sql") {
			return f
		}
	}
	t.Fatalf("Migration C (%s.%s.sql) not found in %s", migCSuffix, dir, o3aMigrationsDir)
	return ""
}

// preMigCDB is a throwaway database migrated to just BEFORE Migration C (the same shape as
// preMigADB, stopping one migration later).
func preMigCDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db := throwawayDB(t)
	stop := migCFile(t, "up")
	for _, f := range migFiles(t, ".up.sql") {
		if f >= stop {
			break
		}
		if err := execMigFile(t, db, f); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return db
}

func throwawayDB(t *testing.T) *sqlx.DB {
	t.Helper()
	admin, err := sqlx.Connect("mysql", o3aAdminDSN(t))
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping", err)
	}
	dbName := o3aUniqueDBName()
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); err != nil {
		t.Fatalf("pre-drop: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("create db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
		_ = admin.Close()
	})
	dsn := o3aDSN(t, dbName)
	if !strings.Contains(dsn, "multiStatements") {
		if strings.Contains(dsn, "?") {
			dsn += "&multiStatements=true"
		} else {
			dsn += "?multiStatements=true"
		}
	}
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func rawProcessWith(t *testing.T, db *sqlx.DB, tenant, entity, steps, graph, outcome string, deleted bool) string {
	t.Helper()
	id := uuid.NewString()
	var g, o any
	if graph != "" {
		g = graph
	}
	if outcome != "" {
		o = outcome
	}
	if _, err := db.Exec(`INSERT INTO sys_approval_processes (id, tenant_id, name, entity_name, trigger_event, steps, graph, outcome, is_active, deleted_at)
		VALUES (?, ?, ?, ?, 'manual', ?, ?, ?, 1, IF(?, NOW(3), NULL))`, id, tenant, "raw-"+entity, entity, steps, g, o, deleted); err != nil {
		t.Fatalf("insert raw process: %v", err)
	}
	return id
}

// TestMigrationC_BackfillsV1 (T160): every LIVE process gets exactly one version 1 built from its
// own columns; a soft-deleted one gets none; an instance already in flight keeps process_version
// NULL (nobody can prove which definition it was armed under).
func TestMigrationC_BackfillsV1(t *testing.T) {
	db := preMigCDB(t)
	const tenant = o3aTenantA
	linear := rawProcessWith(t, db, tenant, "quotes", `[{"name":"A","approverType":"user","approverId":"`+verAp1+`"}]`, "", `{"statusField":"status","approvedValue":"ok"}`, false)
	graph := rawProcessWith(t, db, tenant, "orders", `[]`, `{"nodes":[{"id":"n1","kind":"approval","approverType":"user","approverId":"`+verAp2+`"}]}`, "", false)
	gone := rawProcessWith(t, db, tenant, "gone", `[{"name":"A","approverType":"user","approverId":"`+verAp1+`"}]`, "", "", true)
	instID := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO sys_approval_instances (id, tenant_id, process_id, entity_name, record_id, current_step, status, history)
		VALUES (?, ?, ?, 'quotes', ?, 0, 'pending', '[]')`, instID, tenant, linear, uuid.NewString()); err != nil {
		t.Fatalf("plant instance: %v", err)
	}

	for i := 1; i <= 2; i++ { // twice: the second run changes nothing
		if err := execMigFile(t, db, migCFile(t, "up")); err != nil {
			t.Fatalf("up #%d: %v", i, err)
		}
		if n := versionCount(t, db, tenant, linear); n != 1 {
			t.Fatalf("after up #%d: linear process has %d versions; want exactly 1", i, n)
		}
		if n := versionCount(t, db, tenant, graph); n != 1 {
			t.Fatalf("after up #%d: graph process has %d versions; want exactly 1", i, n)
		}
		if n := versionCount(t, db, tenant, gone); n != 0 {
			t.Fatalf("after up #%d: soft-deleted process has %d versions; want 0", i, n)
		}
	}
	var def bpm.DefinitionDoc
	var raw json.RawMessage
	if err := db.Get(&raw, `SELECT definition FROM sys_approval_process_versions WHERE tenant_id = ? AND process_id = ? AND version = 1`, tenant, linear); err != nil {
		t.Fatalf("read v1: %v", err)
	}
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatalf("decode v1 %s: %v", raw, err)
	}
	if def.Name != "raw-quotes" || def.EntityName != "quotes" || def.Trigger != "manual" ||
		!strings.Contains(string(def.Steps), verAp1) || !strings.Contains(string(def.Outcome), `"approvedValue"`) {
		t.Fatalf("v1 definition %s does not mirror the process columns", raw)
	}
	if err := db.Get(&raw, `SELECT definition FROM sys_approval_process_versions WHERE tenant_id = ? AND process_id = ? AND version = 1`, tenant, graph); err != nil {
		t.Fatalf("read graph v1: %v", err)
	}
	if !strings.Contains(string(raw), `"n1"`) {
		t.Fatalf("graph v1 %s lost its graph", raw)
	}
	var pv *int
	if err := db.Get(&pv, `SELECT process_version FROM sys_approval_instances WHERE id = ?`, instID); err != nil || pv != nil {
		t.Fatalf("in-flight instance process_version = %v (%v); want NULL", pv, err)
	}
	if v := activeVersion(t, db, tenant, linear); v != 1 {
		t.Fatalf("process version = %d; want 1", v)
	}
}

// TestMigrationC_UpDownUp (T160): up twice and down twice are harmless; down REFUSES while a live
// process holds an unpublished draft (dropping the column would erase it); up again rebuilds v1.
func TestMigrationC_UpDownUp(t *testing.T) {
	db := preMigCDB(t)
	const tenant = o3aTenantA
	proc := rawProcessWith(t, db, tenant, "quotes", `[{"name":"A","approverType":"user","approverId":"`+verAp1+`"}]`, "", "", false)
	up, down := migCFile(t, "up"), migCFile(t, "down")

	schema := func() (table, cols, idx int) {
		t.Helper()
		if err := db.Get(&table, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'sys_approval_process_versions'`); err != nil {
			t.Fatalf("table probe: %v", err)
		}
		if err := db.Get(&cols, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND
			((TABLE_NAME = 'sys_approval_processes' AND COLUMN_NAME IN ('version','draft','draft_updated_by','draft_updated_at'))
			 OR (TABLE_NAME = 'sys_approval_instances' AND COLUMN_NAME = 'process_version'))`); err != nil {
			t.Fatalf("column probe: %v", err)
		}
		if err := db.Get(&idx, `SELECT COUNT(DISTINCT INDEX_NAME) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND
			((TABLE_NAME = 'sys_approval_process_versions' AND INDEX_NAME IN ('uq_apv','idx_apv_process'))
			 OR (TABLE_NAME = 'sys_approval_instances' AND INDEX_NAME = 'idx_ai_version'))`); err != nil {
			t.Fatalf("index probe: %v", err)
		}
		return table, cols, idx
	}
	for i := 1; i <= 2; i++ {
		if err := execMigFile(t, db, up); err != nil {
			t.Fatalf("up #%d: %v", i, err)
		}
		if tbl, cols, idx := schema(); tbl != 1 || cols != 5 || idx != 3 {
			t.Fatalf("after up #%d: table=%d cols=%d idx=%d; want 1/5/3", i, tbl, cols, idx)
		}
	}
	var notTenantFirst int
	if err := db.Get(&notTenantFirst, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE()
		AND TABLE_NAME = 'sys_approval_process_versions' AND INDEX_NAME <> 'PRIMARY' AND SEQ_IN_INDEX = 1 AND COLUMN_NAME <> 'tenant_id'`); err != nil || notTenantFirst != 0 {
		t.Fatalf("%d version-table index(es) do not lead with tenant_id (%v)", notTenantFirst, err)
	}

	// An unpublished draft makes down refuse — and refuse BEFORE touching anything.
	if _, err := db.Exec(`UPDATE sys_approval_processes SET draft = '{"name":"x"}' WHERE id = ?`, proc); err != nil {
		t.Fatalf("plant draft: %v", err)
	}
	err := execMigFile(t, db, down)
	if err == nil || !strings.Contains(err.Error(), "BPM-C ABORT") {
		t.Fatalf("down with a draft: err = %v; want BPM-C ABORT", err)
	}
	if tbl, cols, idx := schema(); tbl != 1 || cols != 5 || idx != 3 {
		t.Fatalf("a refused down changed the schema: table=%d cols=%d idx=%d", tbl, cols, idx)
	}
	if _, err := db.Exec(`UPDATE sys_approval_processes SET draft = NULL WHERE id = ?`, proc); err != nil {
		t.Fatalf("clear draft: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if err := execMigFile(t, db, down); err != nil {
			t.Fatalf("down #%d: %v", i, err)
		}
		if tbl, cols, idx := schema(); tbl != 0 || cols != 0 || idx != 0 {
			t.Fatalf("after down #%d: table=%d cols=%d idx=%d; want 0/0/0", i, tbl, cols, idx)
		}
	}
	if err := execMigFile(t, db, up); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := versionCount(t, db, tenant, proc); n != 1 {
		t.Fatalf("after up→down→up: %d versions; want 1", n)
	}
}

// ---------------------------------------------------------------------------------------------
// T161 — draft / publish
// ---------------------------------------------------------------------------------------------

// TestDraft_DoesNotAffectArming: a draft is saved, a hồ sơ is started — it runs the ACTIVE
// definition (v1), and says so.
func TestDraft_DoesNotAffectArming(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verdraft", verStep("", "Trưởng phòng", verAp1))

	rec, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verdraft", "ver-verdraft v2",
		verStep("s1", "Trưởng phòng", verAp1), verStep("", "Giám đốc", verAp2)))
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if len(rec.Draft) == 0 || rec.Version != 1 {
		t.Fatalf("after SaveDraft: draft=%s version=%d; want a draft over version 1", rec.Draft, rec.Version)
	}
	if rec.Name != "ver-verdraft" || strings.Contains(string(rec.Steps), verAp2) {
		t.Fatalf("the draft leaked into the active columns: name=%q steps=%s", rec.Name, rec.Steps)
	}

	inst := verSubmit(t, st, tenant, "verdraft")
	if got := stepIDsOf(inst); len(got) != 1 || inst.Definition.Chain()[0].ApproverID != verAp1 {
		t.Fatalf("a hồ sơ started with a draft pending runs %v; want the ONE active step held by ap1", got)
	}
	if inst.ProcessVersion != 1 || inst.Definition.Version != 1 {
		t.Fatalf("pinned version = %d/%d; want 1", inst.ProcessVersion, inst.Definition.Version)
	}
	var col *int
	if err := st.sdb.Get(&col, `SELECT process_version FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, inst.ID); err != nil || col == nil || *col != 1 {
		t.Fatalf("process_version column = %v (%v); want 1", col, err)
	}
	// A second SaveDraft replaces the first; DiscardDraft drops it and the process is as before.
	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verdraft", "again", verStep("s1", "A", verAp1))); err != nil {
		t.Fatalf("SaveDraft #2: %v", err)
	}
	if err := st.approval.DiscardDraft(ctx, tenant, id); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}
	after, err := st.approval.GetProcess(ctx, tenant, id)
	if err != nil || len(after.Draft) != 0 || after.Version != 1 {
		t.Fatalf("after discard: draft=%s version=%d (%v); want none, v1", after.Draft, after.Version, err)
	}
	wantApprovalCode(t, st.approval.DiscardDraft(ctx, tenant, id), "APPROVAL_DRAFT_NOT_FOUND")
}

// TestPublish_BumpsVersion_NewInstancesPinned: publish → v2; hồ sơ started before keep v1, after run
// v2; the history lists both with their definitions; both stay decidable.
func TestPublish_BumpsVersion_NewInstancesPinned(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verpub", verStep("", "Trưởng phòng", verAp1))
	old := verSubmit(t, st, tenant, "verpub")

	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verpub", "ver-verpub",
		verStep("s1", "Trưởng phòng", verAp1), verStep("", "Giám đốc", verAp2))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := st.approval.PublishDraft(ctx, tenant, id, "thêm bước Giám đốc", verSubmitter)
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if res.Version != 2 || res.Process.Version != 2 || len(res.Process.Draft) != 0 {
		t.Fatalf("publish result version=%d process.version=%d draft=%s; want v2 and no draft", res.Version, res.Process.Version, res.Process.Draft)
	}
	if !strings.Contains(string(res.Process.Steps), verAp2) {
		t.Fatalf("the published steps %s are not active", res.Process.Steps)
	}
	fresh := verSubmit(t, st, tenant, "verpub")
	if fresh.ProcessVersion != 2 || len(fresh.Definition.Chain()) != 2 {
		t.Fatalf("new hồ sơ: version=%d steps=%d; want v2 with 2 steps", fresh.ProcessVersion, len(fresh.Definition.Chain()))
	}
	again, err := st.repo.GetInstance(ctx, tenant, old.ID)
	if err != nil || again.ProcessVersion != 1 || len(again.Definition.Chain()) != 1 {
		t.Fatalf("old hồ sơ: version=%d (%v); want it still on v1 with 1 step", again.ProcessVersion, err)
	}

	list, err := st.approval.ListVersions(ctx, tenant, id)
	if err != nil || len(list) != 2 || list[0].Version != 2 || list[1].Version != 1 {
		t.Fatalf("ListVersions = %+v (%v); want v2 then v1", list, err)
	}
	if list[0].Note == nil || *list[0].Note != "thêm bước Giám đốc" || len(list[0].Definition) != 0 {
		t.Fatalf("v2 summary %+v; want its note and NO definition in the list", list[0])
	}
	v1, err := st.approval.GetVersion(ctx, tenant, id, 1)
	if err != nil || strings.Contains(string(v1.Definition), verAp2) || !strings.Contains(string(v1.Definition), verAp1) {
		t.Fatalf("GetVersion(1) = %s (%v); want the ORIGINAL one-step definition", v1.Definition, err)
	}
	_, err = st.approval.GetVersion(ctx, tenant, id, 9)
	wantApprovalCode(t, err, "APPROVAL_VERSION_NOT_FOUND")

	// Both generations stay decidable: v1 ends after one approval, v2 moves to its second step.
	done, err := st.approval.Decide(ctx, tenant, old.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: verAp1})
	if err != nil || done.Status != bpm.StatusApproved {
		t.Fatalf("v1 hồ sơ approve: %v status %v; want approved after its single step", err, done)
	}
	mid, err := st.approval.Decide(ctx, tenant, fresh.ID, bpm.DecisionApprove, "", bpm.Actor{UserID: verAp1})
	if err != nil || mid.Status != bpm.StatusPending || mid.CurrentStep != 1 {
		t.Fatalf("v2 hồ sơ approve: %v → %+v; want pending on step 2", err, mid)
	}
}

func TestPublish_NoDraft_409_AndStaleDraftRefused(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verstale", verStep("", "A", verAp1))

	_, err := st.approval.PublishDraft(ctx, tenant, id, "", verSubmitter)
	wantApprovalCode(t, err, "APPROVAL_VERSION_NO_DRAFT")

	// A draft is saved over v1; then the active definition moves (a legacy save = v2). Publishing the
	// draft now would put v1's base back over v2's change.
	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verstale", "draft", verStep("s1", "A", verAp1), verStep("", "B", verAp2))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := st.approval.UpdateProcess(ctx, tenant, id, verBody("verstale", "ver-verstale", verStep("s1", "A", verAp3))); err != nil {
		t.Fatalf("UpdateProcess: %v", err)
	}
	_, err = st.approval.PublishDraft(ctx, tenant, id, "", verSubmitter)
	wantApprovalCode(t, err, "APPROVAL_DRAFT_STALE")
	if v := activeVersion(t, st.sdb, tenant, id); v != 2 {
		t.Fatalf("a refused publish moved the version to %d; want 2 (the legacy save)", v)
	}
	// Re-saving the draft rebases it; it then publishes as v3.
	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verstale", "draft", verStep("s1", "A", verAp3), verStep("", "B", verAp2))); err != nil {
		t.Fatalf("SaveDraft again: %v", err)
	}
	res, err := st.approval.PublishDraft(ctx, tenant, id, "", verSubmitter)
	if err != nil || res.Version != 3 {
		t.Fatalf("publish after rebase: %v version %v; want 3", err, res)
	}
	_, err = st.approval.PublishDraft(ctx, tenant, id, strings.Repeat("x", 501), verSubmitter)
	wantApprovalCode(t, err, "APPROVAL_VERSION_NOTE_TOO_LONG")
}

// TestPublish_ConcurrentOnlyOneWins (-race): the draft is consumed under a row lock — eight racing
// publishes produce exactly ONE new version.
func TestPublish_ConcurrentOnlyOneWins(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verrace", verStep("", "A", verAp1))
	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verrace", "n", verStep("s1", "A", verAp1), verStep("", "B", verAp2))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.approval.PublishDraft(ctx, tenant, id, "", verSubmitter)
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	won := 0
	for err := range results {
		if err == nil {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("%d publishes succeeded; want exactly 1", won)
	}
	if v, n := activeVersion(t, st.sdb, tenant, id), versionCount(t, st.sdb, tenant, id); v != 2 || n != 2 {
		t.Fatalf("version=%d rows=%d; want 2 and 2", v, n)
	}
}

// ---------------------------------------------------------------------------------------------
// T162 — the legacy save
// ---------------------------------------------------------------------------------------------

// TestLegacyPut_PublishesImmediately: the admin builder / AI Builder / pack install keep working —
// a changed save is v+1 at once; a save that changes nothing, or only the active switch, is not.
func TestLegacyPut_PublishesImmediately(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verput", verStep("", "A", verAp1))

	rec, err := st.approval.UpdateProcess(ctx, tenant, id, verBody("verput", "ver-verput", verStep("s1", "A", verAp1), verStep("", "B", verAp2)))
	if err != nil || rec.Version != 2 {
		t.Fatalf("legacy save: %v version %v; want 2", err, rec)
	}
	if n := versionCount(t, st.sdb, tenant, id); n != 2 {
		t.Fatalf("%d version rows after a changed save; want 2", n)
	}
	inst := verSubmit(t, st, tenant, "verput")
	if inst.ProcessVersion != 2 || len(inst.Definition.Chain()) != 2 {
		t.Fatalf("hồ sơ after the legacy save: v%d with %d steps; want v2 with 2", inst.ProcessVersion, len(inst.Definition.Chain()))
	}

	same := verBody("verput", "ver-verput", verStep("s1", "A", verAp1), verStep("s2", "B", verAp2))
	for i := 1; i <= 2; i++ {
		if r, err := st.approval.UpdateProcess(ctx, tenant, id, same); err != nil || r.Version != 2 {
			t.Fatalf("identical save #%d: %v version %v; want it to stay at 2", i, err, r)
		}
	}
	off := same
	off.IsActive = boolPtr(false)
	r, err := st.approval.UpdateProcess(ctx, tenant, id, off)
	if err != nil || r.Version != 2 || r.IsActive {
		t.Fatalf("switching the process off: %v → %+v; want inactive and still v2", err, r)
	}
	if n := versionCount(t, st.sdb, tenant, id); n != 2 {
		t.Fatalf("%d version rows after no-change saves; want 2", n)
	}
}

// TestUpdate_SelfHealsMissingVersionRow: a process row that has no version row (inserted by hand,
// or restored without its history) still gets a history that starts at its own version.
func TestUpdate_SelfHealsMissingVersionRow(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "verheal", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	id := insertRawProcess(t, st.sdb, tenant, "verheal", "manual", `[{"name":"A","approverType":"user","approverId":"`+verAp1+`"}]`)
	if n := versionCount(t, st.sdb, tenant, id); n != 0 {
		t.Fatalf("setup: %d version rows; want 0", n)
	}
	rec, err := st.approval.UpdateProcess(ctx, tenant, id, verBody("verheal", "raw-verheal", verStep("s1", "A", verAp1), verStep("", "B", verAp2)))
	if err != nil || rec.Version != 2 {
		t.Fatalf("UpdateProcess: %v → %+v; want v2", err, rec)
	}
	list, err := st.approval.ListVersions(ctx, tenant, id)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListVersions = %+v (%v); want v1 (healed) and v2", list, err)
	}
}

// ---------------------------------------------------------------------------------------------
// T163 — read side, tenancy, history
// ---------------------------------------------------------------------------------------------

// TestVersions_ListAndGet_TenantPinned: another tenant cannot read a process's versions or publish
// its draft — the answer is "no such process", never "no such version".
func TestVersions_ListAndGet_TenantPinned(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	id := verProcess(t, st, o3aTenantA, "vertenant", verStep("", "A", verAp1))
	if _, err := st.approval.SaveDraft(ctx, o3aTenantA, id, verBody("vertenant", "d", verStep("s1", "A", verAp1), verStep("", "B", verAp2))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	_, err := st.approval.ListVersions(ctx, o3aTenantB, id)
	wantApprovalCode(t, err, "APPROVAL_PROCESS_NOT_FOUND")
	_, err = st.approval.GetVersion(ctx, o3aTenantB, id, 1)
	wantApprovalCode(t, err, "APPROVAL_PROCESS_NOT_FOUND")
	_, err = st.approval.PublishDraft(ctx, o3aTenantB, id, "", verSubmitter)
	wantApprovalCode(t, err, "APPROVAL_PROCESS_NOT_FOUND")
	wantApprovalCode(t, st.approval.DiscardDraft(ctx, o3aTenantB, id), "APPROVAL_PROCESS_NOT_FOUND")
	_, err = st.approval.SaveDraft(ctx, o3aTenantB, id, verBody("vertenant", "x", verStep("", "A", verAp1)))
	if err == nil {
		t.Fatalf("tenant B saved a draft on tenant A's process")
	}
	if after, _ := st.approval.GetProcess(ctx, o3aTenantA, id); after == nil || len(after.Draft) == 0 || !strings.Contains(string(after.Draft), `"d"`) {
		t.Fatalf("tenant A's draft was disturbed by tenant B: %+v", after)
	}
}

// TestHistory_RecordsVersion (#70): a decision's history entry — in the table — names the version
// the hồ sơ runs under, even after the process moved on to v2.
func TestHistory_RecordsVersion(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verhist", verStep("", "A", verAp1), verStep("", "B", verAp2))
	inst := verSubmit(t, st, tenant, "verhist")
	if _, err := st.approval.UpdateProcess(ctx, tenant, id, verBody("verhist", "ver-verhist", verStep("s1", "A", verAp3))); err != nil {
		t.Fatalf("UpdateProcess: %v", err)
	}
	if _, err := st.approval.Decide(ctx, tenant, inst.ID, bpm.DecisionApprove, "ok", bpm.Actor{UserID: verAp1}); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	var hist json.RawMessage
	if err := st.sdb.Get(&hist, `SELECT history FROM sys_approval_instances WHERE tenant_id = ? AND id = ?`, tenant, inst.ID); err != nil {
		t.Fatalf("read history: %v", err)
	}
	var entries []bpm.HistoryEntry
	if err := json.Unmarshal(hist, &entries); err != nil || len(entries) < 2 {
		t.Fatalf("history %s (%v); want the submitted + approved entries", hist, err)
	}
	for _, e := range entries {
		if e.Version != 1 {
			t.Errorf("history entry %q has version %d; want 1 — the hồ sơ runs v1 though the process is v2", e.Action, e.Version)
		}
	}
	detail, err := st.approval.GetApprovalDetail(ctx, RecordScope{IsAdmin: true, AccessLevel: "all"}, tenant, inst.ID, bpm.Actor{UserID: verAp2, IsAdmin: true})
	if err != nil || detail.Process == nil || detail.Process.Version != 1 {
		t.Fatalf("detail.process.version = %+v (%v); want 1", detail.Process, err)
	}
}

// ---------------------------------------------------------------------------------------------
// T164 — what a pack exports; T165 — ids
// ---------------------------------------------------------------------------------------------

// TestPackExport_UsesActiveVersion: the pack exporter reads ListProcesses — the process rows. A
// pending draft must not leak into them (an exported pack is what RUNS, not what is being edited).
func TestPackExport_UsesActiveVersion(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verpack", verStep("", "A", verAp1))
	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verpack", "renamed in draft", verStep("s1", "A", verAp1), verStep("", "B", verAp2))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	rows, err := st.approval.ListProcesses(ctx, tenant)
	if err != nil {
		t.Fatalf("ListProcesses: %v", err)
	}
	var got *bpm.ProcessRecord
	for i := range rows {
		if rows[i].ID == id {
			got = &rows[i]
		}
	}
	if got == nil || got.Name != "ver-verpack" || strings.Contains(string(got.Steps), verAp2) {
		t.Fatalf("export source %+v shows the draft; want the active v1 columns", got)
	}
}

// TestPublish_ReleasedStepIDNeverReissued (T165, spec "Nợ còn mở" 15): s3 is published, deleted,
// and a NEW step is added — it must not become s3, or S11 would map a hồ sơ standing on the old s3
// onto an unrelated step. Judged for both the draft/publish path and the legacy save.
func TestPublish_ReleasedStepIDNeverReissued(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	id := verProcess(t, st, tenant, "verids", verStep("", "A", verAp1), verStep("", "B", verAp2), verStep("", "C", verAp3))

	// v2: C deleted.
	if _, err := st.approval.UpdateProcess(ctx, tenant, id, verBody("verids", "ver-verids", verStep("s1", "A", verAp1), verStep("s2", "B", verAp2))); err != nil {
		t.Fatalf("delete C: %v", err)
	}
	// v3 through the draft: a new step, no id sent.
	if _, err := st.approval.SaveDraft(ctx, tenant, id, verBody("verids", "ver-verids", verStep("s1", "A", verAp1), verStep("s2", "B", verAp2), verStep("", "D", verAp3))); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	res, err := st.approval.PublishDraft(ctx, tenant, id, "", verSubmitter)
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	var steps []bpm.Step
	if err := json.Unmarshal(res.Process.Steps, &steps); err != nil || len(steps) != 3 {
		t.Fatalf("steps %s (%v); want 3", res.Process.Steps, err)
	}
	if steps[2].Name != "D" || steps[2].ID == "s3" || steps[2].ID != "s4" {
		t.Fatalf("new step got id %q; want s4 — s3 belonged to the deleted step C", steps[2].ID)
	}
	// Legacy save: the same rule.
	rec, err := st.approval.UpdateProcess(ctx, tenant, id, verBody("verids", "ver-verids", verStep("s1", "A", verAp1), verStep("s2", "B", verAp2), verStep("s4", "D", verAp3), verStep("", "E", verAp1)))
	if err != nil {
		t.Fatalf("legacy add: %v", err)
	}
	if err := json.Unmarshal(rec.Steps, &steps); err != nil || len(steps) != 4 || steps[3].ID != "s5" {
		t.Fatalf("steps %s (%v); want the new one to be s5", rec.Steps, err)
	}
}

// TestSaveDraft_GraphOverwriteGuard: the FR-004 guard holds for drafts — a linear-only draft cannot
// erase a stored graph unless forced.
func TestSaveDraft_GraphOverwriteGuard(t *testing.T) {
	st := bpmWire(t)
	ctx := context.Background()
	const tenant = o3aTenantA
	st.createEntity(t, ctx, tenant, "vergraph", []FieldInput{{Name: "status", Label: "Status", FieldType: "text"}})
	g := graphNodes(graphNode("n1", "A", "user", verAp1, 0), graphNode("n2", "B", "user", verAp2, 0))
	rec, err := st.approval.CreateProcess(ctx, tenant, CreateApprovalProcessInput{
		Name: "g", EntityName: "vergraph", Trigger: "manual", Graph: g, IsActive: boolPtr(true), ActorID: verSubmitter,
	})
	if err != nil {
		t.Fatalf("CreateProcess graph: %v", err)
	}
	linear := verBody("vergraph", "g", verStep("", "A", verAp1))
	_, err = st.approval.SaveDraft(ctx, tenant, rec.ID, linear)
	wantApprovalCode(t, err, "APPROVAL_PROCESS_V2_OVERWRITE")
	if after, _ := st.approval.GetProcess(ctx, tenant, rec.ID); len(after.Draft) != 0 {
		t.Fatalf("a refused draft was stored: %s", after.Draft)
	}
	linear.Force = true
	if _, err := st.approval.SaveDraft(ctx, tenant, rec.ID, linear); err != nil {
		t.Fatalf("forced linear draft: %v", err)
	}
	// The graph keeps running until the draft is published.
	if after, _ := st.approval.GetProcess(ctx, tenant, rec.ID); len(after.Graph) == 0 {
		t.Fatalf("the draft erased the active graph")
	}
}
