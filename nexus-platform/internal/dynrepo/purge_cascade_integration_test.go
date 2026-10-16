//go:build integration

// specs/056 US2 (#47 record-hard-delete-b) — the purge takes the record's dependent data with it
// in the SAME transaction, driven by the relationship graph the service resolves from metadata.
//
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/ -run 'TestPurgeCascade'
package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ddl"
)

const (
	pcTenant = "56000000-0000-0000-0000-0000000000c1"
	pcActor  = "56000000-0000-0000-0000-00000000a0a0"
	pcUser   = "00000000-0000-0000-0000-0000000000cc"
	pcParent = "it_pc_parent" // the entity purged from
	pcChild  = "it_pc_child"  // required 1-N child (parent)
	pcGrand  = "it_pc_grand"  // required 1-N grandchild (child)
	pcOpt    = "it_pc_opt"    // optional 1-N reference (ref)
	pcOne    = "it_pc_one"    // required 1-1 child (holder) — its FK gencol is CASE-guarded
	pcPeer   = "it_pc_peer"   // N-N partner
	pcAct    = "it_pc_act"    // optional polymorphic reference (regarding → {type, id})
)

// pcGraph is what RecordService.Purge resolves from sys_relationships + sys_fields for pcParent.
func pcGraph() map[string]PurgeEntityDeps {
	return map[string]PurgeEntityDeps{
		pcParent: {
			Children: []PurgeChildRef{
				{Entity: pcChild, Field: "parent", Required: true},
				{Entity: pcOpt, Field: "ref"},
				{Entity: pcOne, Field: "holder", Required: true, OneToOne: true},
			},
			JoinPeers: []string{pcPeer},
		},
		pcChild: {Children: []PurgeChildRef{{Entity: pcGrand, Field: "child", Required: true}}},
		pcGrand: {},
		pcOne:   {},
	}
}

type pcFixture struct {
	db   *sqlx.DB
	repo *Repository
	ctx  context.Context
}

func newPCFixture(t *testing.T) *pcFixture {
	t.Helper()
	db := testDB(t)
	f := &pcFixture{db: db, repo: New(db), ctx: context.Background()}
	joinTable, _, _, _ := ddl.JoinParts(pcParent, pcPeer)
	clean := func() {
		for _, e := range []string{pcParent, pcChild, pcGrand, pcOpt, pcOne, pcPeer, pcAct} {
			_ = f.repo.DropTable(f.ctx, e)
		}
		_, _ = db.Exec("DROP TABLE IF EXISTS `" + joinTable + "`")
		for _, tbl := range []string{"sys_record_notes", "sys_notifications", "sys_record_shares", "sys_approval_instances", "sys_bpm_tasks", "sys_email_log", "sys_workflow_runs", "sys_files"} {
			_, _ = db.Exec("DELETE FROM `"+tbl+"` WHERE tenant_id = ?", pcTenant)
		}
	}
	clean()
	t.Cleanup(clean)
	mk := func(entity string, fields ...ddl.FieldDef) {
		t.Helper()
		if err := f.repo.CreateTable(f.ctx, entity, append([]ddl.FieldDef{{Name: "name", FieldType: "text"}}, fields...)); err != nil {
			t.Fatalf("CreateTable %s: %v", entity, err)
		}
	}
	mk(pcParent)
	mk(pcChild, ddl.FieldDef{Name: "parent", FieldType: "relation", RelatedEntity: pcParent})
	mk(pcGrand, ddl.FieldDef{Name: "child", FieldType: "relation", RelatedEntity: pcChild})
	mk(pcOpt, ddl.FieldDef{Name: "ref", FieldType: "relation", RelatedEntity: pcParent})
	mk(pcOne, ddl.FieldDef{Name: "holder", FieldType: "relation", RelatedEntity: pcParent, OneToOne: true})
	mk(pcPeer)
	mk(pcAct, ddl.FieldDef{Name: "regarding", FieldType: "polymorphic_relation", Polymorphic: true})
	if err := f.repo.CreateJoinTable(f.ctx, pcParent, pcPeer); err != nil {
		t.Fatalf("CreateJoinTable: %v", err)
	}
	return f
}

func (f *pcFixture) insert(t *testing.T, entity string, data map[string]any) string {
	t.Helper()
	id, err := f.repo.InsertRecord(f.ctx, entity, pcTenant, pcUser, data, nil)
	if err != nil {
		t.Fatalf("insert %s: %v", entity, err)
	}
	return id
}

func (f *pcFixture) bin(t *testing.T, entity, id string) {
	t.Helper()
	if err := f.repo.SoftDeleteRecord(f.ctx, entity, pcTenant, id, AdminScope()); err != nil {
		t.Fatalf("soft-delete %s/%s: %v", entity, id, err)
	}
}

// sysRows seeds one row keyed by (entity, record) in every dependent system table.
func (f *pcFixture) sysRows(t *testing.T, entity, id string) {
	t.Helper()
	stmts := []struct {
		q    string
		args []any
	}{
		{"INSERT INTO sys_record_notes (id, tenant_id, entity_name, record_id, body, created_by) VALUES (?, ?, ?, ?, 'ghi chú', ?)", []any{uuid.NewString(), pcTenant, entity, id, pcUser}},
		{"INSERT INTO sys_notifications (id, tenant_id, user_id, message, entity_name, record_id) VALUES (?, ?, ?, 'thông báo', ?, ?)", []any{uuid.NewString(), pcTenant, pcUser, entity, id}},
		{"INSERT INTO sys_record_shares (id, tenant_id, entity_name, record_id, user_id, shared_by, created_by) VALUES (?, ?, ?, ?, ?, ?, ?)", []any{uuid.NewString(), pcTenant, entity, id, uuid.NewString(), pcUser, pcUser}},
		{"INSERT INTO sys_approval_instances (id, tenant_id, process_id, entity_name, record_id, history) VALUES (?, ?, ?, ?, ?, JSON_ARRAY())", []any{uuid.NewString(), pcTenant, uuid.NewString(), entity, id}},
		{"INSERT INTO sys_bpm_tasks (id, tenant_id, instance_id, process_id, entity_name, record_id, node_id, assignee_type, assignee_id) VALUES (?, ?, ?, ?, ?, ?, 'n1', 'user', ?)", []any{uuid.NewString(), pcTenant, uuid.NewString(), uuid.NewString(), entity, id, pcUser}},
		{"INSERT INTO sys_email_log (id, tenant_id, source, status, recipient_masked, entity_name, record_id) VALUES (?, ?, 'workflow', 'sent', 'a***@x.vn', ?, ?)", []any{uuid.NewString(), pcTenant, entity, id}},
		{"INSERT INTO sys_workflow_runs (id, tenant_id, workflow_id, trigger_type, record_id, status, started_at, finished_at) VALUES (?, ?, ?, 'after_update', ?, 'success', NOW(3), NOW(3))", []any{uuid.NewString(), pcTenant, uuid.NewString(), id}},
	}
	for _, s := range stmts {
		if _, err := f.db.Exec(s.q, s.args...); err != nil {
			t.Fatalf("seed %q: %v", s.q, err)
		}
	}
}

func (f *pcFixture) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	return purgeCount(t, f.db, q, args...)
}

// sysCount counts the dependent system rows still naming (entity, id) — sys_workflow_runs has no
// entity_name, record ids are UUIDs.
func (f *pcFixture) sysCount(t *testing.T, entity, id string) int {
	t.Helper()
	n := 0
	for _, tbl := range []string{"sys_record_notes", "sys_notifications", "sys_record_shares", "sys_approval_instances", "sys_bpm_tasks", "sys_email_log"} {
		n += f.count(t, "SELECT COUNT(*) FROM `"+tbl+"` WHERE tenant_id = ? AND entity_name = ? AND record_id = ?", pcTenant, entity, id)
	}
	return n + f.count(t, "SELECT COUNT(*) FROM sys_workflow_runs WHERE tenant_id = ? AND record_id = ?", pcTenant, id)
}

func (f *pcFixture) exists(t *testing.T, entity, id string) bool {
	t.Helper()
	return f.count(t, "SELECT COUNT(*) FROM `obj_"+entity+"` WHERE tenant_id = ? AND id = ?", pcTenant, id) == 1
}

// TestPurgeCascade_TakesDependentsAndClearsOptionalRefs: the root, its in-bin required children
// (and theirs), every system row keyed by them and the root's N-N link rows go in one transaction;
// an optional reference to the root is CLEARED (key removed, version+1, modified_by = actor); rows
// of a sibling parent are untouched; a dry run reports all of it and changes nothing.
func TestPurgeCascade_TakesDependentsAndClearsOptionalRefs(t *testing.T) {
	f := newPCFixture(t)
	p1 := f.insert(t, pcParent, map[string]any{"name": "P1"})
	p2 := f.insert(t, pcParent, map[string]any{"name": "P2"})
	c1 := f.insert(t, pcChild, map[string]any{"name": "C1", "parent": p1})
	c2 := f.insert(t, pcChild, map[string]any{"name": "C2", "parent": p2})
	g1 := f.insert(t, pcGrand, map[string]any{"name": "G1", "child": c1})
	one := f.insert(t, pcOne, map[string]any{"name": "ONE", "holder": p1})
	o1 := f.insert(t, pcOpt, map[string]any{"name": "O1", "ref": p1})
	o2 := f.insert(t, pcOpt, map[string]any{"name": "O2", "ref": p2})
	t1 := f.insert(t, pcPeer, map[string]any{"name": "T1"})
	for _, pid := range []string{p1, p2} {
		if _, err := f.repo.LinkRecords(f.ctx, pcTenant, pcUser, pcParent, pid, pcPeer, t1); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	f.sysRows(t, pcParent, p1)
	f.sysRows(t, pcChild, c1)
	f.sysRows(t, pcParent, p2)
	// Bin bottom-up: required children must already be in the bin to go with the parent.
	f.bin(t, pcGrand, g1)
	f.bin(t, pcChild, c1)
	f.bin(t, pcOne, one) // 1-1: its holder_id gencol now projects NULL — must still be found
	f.bin(t, pcParent, p1)
	joinTable, colA, _, _ := ddl.JoinParts(pcParent, pcPeer)

	params := PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p1, ActorID: pcActor, Graph: pcGraph()}
	check := func(label string, res *PurgeResult) {
		t.Helper()
		got := map[string]bool{}
		for _, r := range res.Records {
			got[r.Entity+"/"+r.ID] = true
		}
		for _, want := range []string{pcParent + "/" + p1, pcChild + "/" + c1, pcGrand + "/" + g1, pcOne + "/" + one} {
			if !got[want] {
				t.Fatalf("%s: records %+v miss %s", label, res.Records, want)
			}
		}
		if len(res.Records) != 4 || res.Records[0].ID != p1 {
			t.Fatalf("%s: records = %+v; want exactly the 4, root first", label, res.Records)
		}
		if len(res.Cleared) != 1 || res.Cleared[0].Entity != pcOpt || res.Cleared[0].ID != o1 || res.Cleared[0].Field != "ref" || res.Cleared[0].Was != p1 {
			t.Fatalf("%s: cleared = %+v; want O1.ref (was P1)", label, res.Cleared)
		}
		if len(res.Blocked) != 0 {
			t.Fatalf("%s: blocked = %+v; want none", label, res.Blocked)
		}
		for table, want := range map[string]int64{
			"obj_" + pcParent: 1, "obj_" + pcChild: 1, "obj_" + pcGrand: 1, "obj_" + pcOne: 1,
			joinTable: 1, "sys_record_notes": 2, "sys_notifications": 2, "sys_record_shares": 2,
			"sys_approval_instances": 2, "sys_bpm_tasks": 2, "sys_email_log": 2, "sys_workflow_runs": 2,
		} {
			if n := purgeTableRows(res, table); n != want {
				t.Fatalf("%s: %s rows = %d; want %d (tables=%+v)", label, table, n, want, res.Tables)
			}
		}
	}

	dry := params
	dry.DryRun = true
	res, err := f.repo.PurgeRecord(f.ctx, dry)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	check("dry run", res)
	if !f.exists(t, pcChild, c1) || !f.exists(t, pcParent, p1) || f.sysCount(t, pcParent, p1) != 7 {
		t.Fatal("dry run deleted something")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM `obj_"+pcOpt+"` WHERE tenant_id = ? AND id = ? AND ref_id = ?", pcTenant, o1, p1); n != 1 {
		t.Fatal("dry run cleared O1.ref")
	}

	res, err = f.repo.PurgeRecord(f.ctx, params)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	check("purge", res)
	for _, gone := range [][2]string{{pcParent, p1}, {pcChild, c1}, {pcGrand, g1}, {pcOne, one}} {
		if f.exists(t, gone[0], gone[1]) {
			t.Fatalf("%s/%s survived the purge", gone[0], gone[1])
		}
	}
	if f.sysCount(t, pcParent, p1) != 0 || f.sysCount(t, pcChild, c1) != 0 {
		t.Fatal("dependent system rows survived the purge")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM `"+joinTable+"` WHERE tenant_id = ? AND `"+colA+"` = ?", pcTenant, p1); n != 0 {
		t.Fatalf("N-N link rows of P1 survived (%d)", n)
	}
	// O1: reference cleared the way a partial PUT clears it (key gone), version bumped, actor stamped.
	var o struct {
		Ref     *string `db:"ref_id"`
		Version int     `db:"version"`
		ModBy   *string `db:"modified_by"`
		HasKey  bool    `db:"has_key"`
	}
	if err := f.db.Get(&o, "SELECT ref_id, version, modified_by, JSON_CONTAINS_PATH(data, 'one', '$.ref') AS has_key FROM `obj_"+pcOpt+"` WHERE tenant_id = ? AND id = ?", pcTenant, o1); err != nil {
		t.Fatalf("read O1: %v", err)
	}
	if o.Ref != nil || o.HasKey || o.Version != 1 || o.ModBy == nil || *o.ModBy != pcActor { // inserted at version 0
		t.Fatalf("O1 after purge: ref=%v hasKey=%v version=%d modified_by=%v; want cleared, version+1, actor", o.Ref, o.HasKey, o.Version, o.ModBy)
	}
	// The sibling parent's world is untouched.
	if !f.exists(t, pcParent, p2) || !f.exists(t, pcChild, c2) || f.sysCount(t, pcParent, p2) != 7 {
		t.Fatal("P2 / C2 / P2's system rows were touched")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM `obj_"+pcOpt+"` WHERE tenant_id = ? AND id = ? AND ref_id = ?", pcTenant, o2, p2); n != 1 {
		t.Fatal("O2.ref (pointing at P2) was touched")
	}
	if n := f.count(t, "SELECT COUNT(*) FROM `"+joinTable+"` WHERE tenant_id = ? AND `"+colA+"` = ?", pcTenant, p2); n != 1 {
		t.Fatalf("P2's link row was touched (%d)", n)
	}
}

// TestPurgeCascade_LiveRequiredChildBlocks: a required child that is still LIVE is never destroyed
// by emptying the bin. A dry run reports it; the real call refuses (typed error) and changes
// nothing; once the child is binned too, the purge takes both.
func TestPurgeCascade_LiveRequiredChildBlocks(t *testing.T) {
	f := newPCFixture(t)
	p := f.insert(t, pcParent, map[string]any{"name": "P"})
	c := f.insert(t, pcChild, map[string]any{"name": "C live", "parent": p})
	f.bin(t, pcParent, p)
	params := PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p, ActorID: pcActor, Graph: pcGraph()}

	dry := params
	dry.DryRun = true
	res, err := f.repo.PurgeRecord(f.ctx, dry)
	if err != nil {
		t.Fatalf("dry run must report blockers, not fail: %v", err)
	}
	if len(res.Blocked) != 1 || res.Blocked[0].Entity != pcChild || res.Blocked[0].Field != "parent" || res.Blocked[0].Count != 1 {
		t.Fatalf("dry-run blocked = %+v; want it_pc_child.parent ×1", res.Blocked)
	}

	_, err = f.repo.PurgeRecord(f.ctx, params)
	var be *PurgeBlockedError
	if !errors.As(err, &be) || len(be.Blocked) != 1 || be.Blocked[0].Count != 1 {
		t.Fatalf("purge with a live required child: err = %v; want *PurgeBlockedError ×1", err)
	}
	if !f.exists(t, pcParent, p) || !f.exists(t, pcChild, c) {
		t.Fatal("a blocked purge deleted something")
	}

	f.bin(t, pcChild, c)
	res, err = f.repo.PurgeRecord(f.ctx, params)
	if err != nil {
		t.Fatalf("purge after binning the child: %v", err)
	}
	if len(res.Records) != 2 || f.exists(t, pcChild, c) || f.exists(t, pcParent, p) {
		t.Fatalf("records = %+v; want parent + child gone", res.Records)
	}
}

// TestPurgeCascade_CapRefusesWholePurge: a cascade larger than MaxRecords is refused as a whole —
// never truncated, never half-done.
func TestPurgeCascade_CapRefusesWholePurge(t *testing.T) {
	f := newPCFixture(t)
	p := f.insert(t, pcParent, map[string]any{"name": "P"})
	var kids []string
	for i := 0; i < 3; i++ {
		kids = append(kids, f.insert(t, pcChild, map[string]any{"name": "C", "parent": p}))
	}
	for _, k := range kids {
		f.bin(t, pcChild, k)
	}
	f.bin(t, pcParent, p)

	for _, dryRun := range []bool{true, false} {
		_, err := f.repo.PurgeRecord(f.ctx, PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p, ActorID: pcActor, Graph: pcGraph(), MaxRecords: 3, DryRun: dryRun})
		var tl *PurgeTooLargeError
		if !errors.As(err, &tl) || tl.Max != 3 {
			t.Fatalf("dryRun=%v: err = %v; want *PurgeTooLargeError{Max:3}", dryRun, err)
		}
	}
	if !f.exists(t, pcParent, p) || !f.exists(t, pcChild, kids[0]) {
		t.Fatal("a refused (too large) purge deleted something")
	}
	// Exactly at the cap (root + 3 children = 4) is allowed: the cap refuses MORE than Max.
	res, err := f.repo.PurgeRecord(f.ctx, PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p, ActorID: pcActor, Graph: pcGraph(), MaxRecords: 4, DryRun: true})
	if err != nil {
		t.Fatalf("at the cap (4 records, Max 4): %v; want allowed", err)
	}
	if len(res.Records) != 4 {
		t.Fatalf("at the cap: records = %d; want 4", len(res.Records))
	}
}

// connRows is this connection's running total of rows its statements sent back / changed
// (performance_schema, on by default in MySQL 8 — CI pins mysql:8.0.46), plus the thread it read
// them on. The caller pins the pool to ONE connection so the purge's transaction runs on it too.
func connRows(t *testing.T, db *sqlx.DB) (thread, sent, affected int64) {
	t.Helper()
	if err := db.QueryRow("SELECT PS_CURRENT_THREAD_ID(), COALESCE(SUM(SUM_ROWS_SENT), 0), COALESCE(SUM(SUM_ROWS_AFFECTED), 0) "+
		"FROM performance_schema.events_statements_summary_by_thread_by_event_name WHERE THREAD_ID = PS_CURRENT_THREAD_ID()").
		Scan(&thread, &sent, &affected); err != nil {
		t.Fatalf("read performance_schema: %v", err)
	}
	return thread, sent, affected
}

// TestPurgeCascade_ProbesStayBoundedPastTheCap: the cap bounds what a purge READS and LOCKS, not only
// what it deletes. A binned parent with far more referencing rows than MaxRecords — live required
// children (blockers) or optional references — must not pull them all into the transaction:
// blockers are counted, never loaded; optional references are fetched at most cap+1 at a time and the
// purge is refused before it writes anything. Measured on the wire (rows the server sent back).
func TestPurgeCascade_ProbesStayBoundedPastTheCap(t *testing.T) {
	const (
		n   = 40 // referencing rows — far past the cap
		max = 3
		// Root lock (1) + one probe (≤ max+1) + the reading query itself (1), with headroom — and
		// still well under n: an unbounded probe sends n.
		wireBound = 10
	)
	measure := func(t *testing.T, f *pcFixture, p PurgeParams) (res *PurgeResult, sent, affected int64, err error) {
		t.Helper()
		f.db.SetMaxOpenConns(1) // the purge's transaction and connRows share one connection/thread
		thread, sent0, aff0 := connRows(t, f.db)
		res, err = f.repo.PurgeRecord(f.ctx, p)
		thread2, sent1, aff1 := connRows(t, f.db)
		if thread2 != thread {
			t.Fatalf("connection changed between reads (thread %d → %d); measurement void", thread, thread2)
		}
		return res, sent1 - sent0, aff1 - aff0, err
	}

	t.Run("live required children are counted, not loaded", func(t *testing.T) {
		f := newPCFixture(t)
		p := f.insert(t, pcParent, map[string]any{"name": "P"})
		for i := 0; i < n; i++ {
			f.insert(t, pcChild, map[string]any{"name": "C live", "parent": p})
		}
		f.bin(t, pcParent, p)
		graph := map[string]PurgeEntityDeps{pcParent: {Children: []PurgeChildRef{{Entity: pcChild, Field: "parent", Required: true}}}}

		res, sent, _, err := measure(t, f, PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p, ActorID: pcActor, Graph: graph, MaxRecords: max, DryRun: true})
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if len(res.Blocked) != 1 || res.Blocked[0].Entity != pcChild || res.Blocked[0].Field != "parent" || res.Blocked[0].Count != n {
			t.Fatalf("blocked = %+v; want it_pc_child.parent ×%d (the exact count, though never loaded)", res.Blocked, n)
		}
		if sent > wireBound {
			t.Fatalf("the dry run pulled %d rows over the wire for %d live blockers; want ≤ %d (counted, not loaded)", sent, n, wireBound)
		}
	})

	t.Run("optional references past the cap are refused before any write", func(t *testing.T) {
		f := newPCFixture(t)
		p := f.insert(t, pcParent, map[string]any{"name": "P"})
		for i := 0; i < n; i++ {
			f.insert(t, pcOpt, map[string]any{"name": "O", "ref": p})
		}
		f.bin(t, pcParent, p)
		graph := map[string]PurgeEntityDeps{pcParent: {Children: []PurgeChildRef{{Entity: pcOpt, Field: "ref"}}}}

		_, sent, affected, err := measure(t, f, PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p, ActorID: pcActor, Graph: graph, MaxRecords: max, DryRun: true})
		var tl *PurgeTooLargeError
		if !errors.As(err, &tl) || tl.Max != max {
			t.Fatalf("err = %v; want *PurgeTooLargeError{Max:%d}", err, max)
		}
		if sent > wireBound {
			t.Fatalf("the probe pulled %d rows over the wire for %d references; want ≤ %d (at most cap+1)", sent, n, wireBound)
		}
		if affected != 0 {
			t.Fatalf("%d rows written before the refusal; want 0 (refuse, then write nothing)", affected)
		}
		if c := f.count(t, "SELECT COUNT(*) FROM `obj_"+pcOpt+"` WHERE tenant_id = ? AND ref_id = ?", pcTenant, p); c != n {
			t.Fatalf("%d of %d references survive the refused dry run", c, n)
		}
	})
}

// TestPurgeCascade_PolymorphicReferences: a polymorphic_relation value {type, id} naming the purged
// record is cleared like any optional reference; one naming the same id under ANOTHER type, or
// another record, is untouched (matched on the indexed (<f>_type, <f>_id) pair, never on id alone).
func TestPurgeCascade_PolymorphicReferences(t *testing.T) {
	f := newPCFixture(t)
	p1 := f.insert(t, pcParent, map[string]any{"name": "P1"})
	p2 := f.insert(t, pcParent, map[string]any{"name": "P2"})
	a1 := f.insert(t, pcAct, map[string]any{"name": "A1", "regarding": map[string]any{"type": pcParent, "id": p1}})
	a2 := f.insert(t, pcAct, map[string]any{"name": "A2", "regarding": map[string]any{"type": pcPeer, "id": p1}})
	a3 := f.insert(t, pcAct, map[string]any{"name": "A3", "regarding": map[string]any{"type": pcParent, "id": p2}})
	f.bin(t, pcParent, p1)
	graph := map[string]PurgeEntityDeps{pcParent: {Children: []PurgeChildRef{{Entity: pcAct, Field: "regarding", Polymorphic: true}}}}

	res, err := f.repo.PurgeRecord(f.ctx, PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p1, ActorID: pcActor, Graph: graph})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(res.Cleared) != 1 || res.Cleared[0].ID != a1 || res.Cleared[0].Field != "regarding" || res.Cleared[0].Was != p1 {
		t.Fatalf("cleared = %+v; want only A1.regarding", res.Cleared)
	}
	if n := f.count(t, "SELECT COUNT(*) FROM `obj_"+pcAct+"` WHERE tenant_id = ? AND id = ? AND regarding_id IS NULL AND NOT JSON_CONTAINS_PATH(data, 'one', '$.regarding')", pcTenant, a1); n != 1 {
		t.Fatal("A1.regarding not cleared")
	}
	for _, keep := range [][2]string{{a2, p1}, {a3, p2}} {
		if n := f.count(t, "SELECT COUNT(*) FROM `obj_"+pcAct+"` WHERE tenant_id = ? AND id = ? AND regarding_id = ?", pcTenant, keep[0], keep[1]); n != 1 {
			t.Fatalf("%s.regarding was touched", keep[0])
		}
	}
}
