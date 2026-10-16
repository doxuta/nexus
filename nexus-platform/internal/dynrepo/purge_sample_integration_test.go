//go:build integration

// specs/064 S9-7 (#399 sample-wipe) — PurgeParams.SampleRoot: the purge that empties the sample
// rows only. The root must carry a sample mark (live OR in the bin); required children follow only
// when marked too; anything unmarked that depends on the cluster blocks it, and a row without a
// mark is never deleted, never rewritten (version unchanged).
//
// Run with:
//
//	go test -tags=integration ./internal/dynrepo/ -run 'TestPurgeSample|TestSampleRoots|TestResetSequence'
package dynrepo

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// sampleCtx is the ctx a sample insert runs under.
func sampleCtx() context.Context {
	return WithSampleMark(context.Background(), SampleMark{BatchID: uuid.NewString(), Source: SampleSourceGenerator})
}

// withSampleCleanup wipes the tenant's marks before and after the test (pcFixture cleans obj_* only).
func (f *pcFixture) withSampleCleanup(t *testing.T) *pcFixture {
	t.Helper()
	clean := func() {
		_, _ = f.db.Exec("DELETE FROM `sys_sample_records` WHERE `tenant_id` = ?", pcTenant)
		_, _ = f.db.Exec("DELETE FROM `sys_sequences` WHERE `tenant_id` = ?", pcTenant)
	}
	clean()
	t.Cleanup(clean)
	return f
}

// mark inserts a sample row (mark in the same tx); insert (pcFixture) inserts a real one.
func (f *pcFixture) mark(t *testing.T, entity string, data map[string]any) string {
	t.Helper()
	id, err := f.repo.InsertRecord(sampleCtx(), entity, pcTenant, pcUser, data, nil)
	if err != nil {
		t.Fatalf("insert sample %s: %v", entity, err)
	}
	return id
}

func (f *pcFixture) version(t *testing.T, entity, id string) int {
	t.Helper()
	return f.count(t, "SELECT `version` FROM `obj_"+entity+"` WHERE tenant_id = ? AND id = ?", pcTenant, id)
}

func (f *pcFixture) marks(t *testing.T, entity, id string) int {
	t.Helper()
	return f.count(t, "SELECT COUNT(*) FROM sys_sample_records WHERE tenant_id = ? AND entity_name = ? AND record_id = ?", pcTenant, entity, id)
}

func sampleParams(id string, pinned map[string]PurgePin) PurgeParams {
	return PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: id, ActorID: pcActor, Graph: pcGraph(), SampleRoot: true, Pinned: pinned}
}

func blockedBy(t *testing.T, err error) []PurgeBlocker {
	t.Helper()
	var b *PurgeBlockedError
	if !errors.As(err, &b) {
		t.Fatalf("err = %v; want *PurgeBlockedError", err)
	}
	return b.Blocked
}

// An unmarked root is not-found whether live or binned; a marked root goes live or binned, mark and all.
func TestPurgeSample_RootMustCarryMark(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	real := f.insert(t, pcParent, map[string]any{"name": "that"})
	realBin := f.insert(t, pcParent, map[string]any{"name": "that-bin"})
	f.bin(t, pcParent, realBin)
	live := f.mark(t, pcParent, map[string]any{"name": "mau"})
	binned := f.mark(t, pcParent, map[string]any{"name": "mau-bin"})
	f.bin(t, pcParent, binned)

	for _, id := range []string{real, realBin} {
		if _, err := f.repo.PurgeRecord(f.ctx, sampleParams(id, nil)); !errors.Is(err, ErrRecordNotFound) {
			t.Fatalf("unmarked %s: err = %v; want ErrRecordNotFound", id, err)
		}
		if !f.exists(t, pcParent, id) {
			t.Fatalf("unmarked %s was deleted", id)
		}
	}
	for _, id := range []string{live, binned} {
		res, err := f.repo.PurgeRecord(f.ctx, sampleParams(id, nil))
		if err != nil {
			t.Fatalf("marked %s: %v", id, err)
		}
		if len(res.Records) != 1 || f.exists(t, pcParent, id) {
			t.Fatalf("marked %s: records=%+v exists=%v", id, res.Records, f.exists(t, pcParent, id))
		}
		if f.marks(t, pcParent, id) != 0 {
			t.Fatalf("marked %s: the mark survived the purge", id)
		}
	}
}

// Required children follow only with a mark (a LIVE marked one too); the cluster comes out whole.
func TestPurgeSample_MarkedRequiredChildrenFollow(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	p := f.mark(t, pcParent, map[string]any{"name": "P"})
	c := f.mark(t, pcChild, map[string]any{"name": "C", "parent": p}) // live
	g := f.mark(t, pcGrand, map[string]any{"name": "G", "child": c})
	one := f.mark(t, pcOne, map[string]any{"name": "ONE", "holder": p})
	f.bin(t, pcOne, one) // 1-1 binned: matched on the JSON value
	f.sysRows(t, pcChild, c)

	res, err := f.repo.PurgeRecord(f.ctx, sampleParams(p, nil))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(res.Records) != 4 {
		t.Fatalf("records = %+v; want root + child + grandchild + 1-1 child", res.Records)
	}
	for entity, id := range map[string]string{pcParent: p, pcChild: c, pcGrand: g, pcOne: one} {
		if f.exists(t, entity, id) || f.marks(t, entity, id) != 0 {
			t.Fatalf("%s/%s still there (or its mark)", entity, id)
		}
	}
	if f.sysCount(t, pcChild, c) != 0 {
		t.Fatal("the child's system rows survived")
	}
}

// An unmarked required child (live OR in the bin, 1-N or 1-1) blocks the cluster: nothing is deleted
// or rewritten, and the blocker names it.
func TestPurgeSample_UnmarkedRequiredChildBlocks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		entity string
		field  string
		bin    bool
	}{
		{"live 1-N", pcChild, "parent", false},
		{"binned 1-N", pcChild, "parent", true},
		{"live 1-1", pcOne, "holder", false},
		{"binned 1-1", pcOne, "holder", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPCFixture(t).withSampleCleanup(t)
			p := f.mark(t, pcParent, map[string]any{"name": "P"})
			child := f.insert(t, tc.entity, map[string]any{"name": "C", tc.field: p})
			if tc.bin {
				f.bin(t, tc.entity, child)
			}
			v := f.version(t, tc.entity, child)
			_, err := f.repo.PurgeRecord(f.ctx, sampleParams(p, nil))
			got := blockedBy(t, err)
			if len(got) != 1 || got[0].Entity != tc.entity || got[0].Field != tc.field || got[0].Count != 1 {
				t.Fatalf("blockers = %+v; want %s.%s x1", got, tc.entity, tc.field)
			}
			if !f.exists(t, pcParent, p) || !f.exists(t, tc.entity, child) || f.marks(t, pcParent, p) != 1 {
				t.Fatal("a blocked purge deleted something")
			}
			if f.version(t, tc.entity, child) != v {
				t.Fatal("a blocked purge rewrote the real row")
			}
		})
	}
}

// An optional (or polymorphic) reference held by a REAL row blocks and leaves the real row exactly
// as it was; the same reference held by a MARKED row is cleared like in 056.
func TestPurgeSample_OptionalReferences(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	p := f.mark(t, pcParent, map[string]any{"name": "P"})
	realOpt := f.insert(t, pcOpt, map[string]any{"name": "real", "ref": p})
	binOpt := f.insert(t, pcOpt, map[string]any{"name": "real-bin", "ref": p})
	f.bin(t, pcOpt, binOpt)
	realAct := f.insert(t, pcAct, map[string]any{"name": "act", "regarding": map[string]any{"type": pcParent, "id": p}})
	vOpt, vBin, vAct := f.version(t, pcOpt, realOpt), f.version(t, pcOpt, binOpt), f.version(t, pcAct, realAct)

	graph := pcGraph()
	pe := graph[pcParent]
	pe.Children = append(pe.Children, PurgeChildRef{Entity: pcAct, Field: "regarding", Polymorphic: true})
	graph[pcParent] = pe
	params := sampleParams(p, nil)
	params.Graph = graph

	_, err := f.repo.PurgeRecord(f.ctx, params)
	got := blockedBy(t, err)
	want := map[string]int{pcOpt + ".ref": 2, pcAct + ".regarding": 1}
	for _, b := range got {
		if want[b.Entity+"."+b.Field] != b.Count {
			t.Fatalf("blockers = %+v; want %v", got, want)
		}
		delete(want, b.Entity+"."+b.Field)
	}
	if len(want) != 0 {
		t.Fatalf("blockers = %+v; missing %v", got, want)
	}
	if !f.exists(t, pcParent, p) || f.version(t, pcOpt, realOpt) != vOpt ||
		f.version(t, pcOpt, binOpt) != vBin || f.version(t, pcAct, realAct) != vAct {
		t.Fatal("a blocked purge changed the cluster or a real row")
	}

	// Now the holder is a sample: cleared, not blocked.
	f2 := newPCFixture(t).withSampleCleanup(t)
	p2 := f2.mark(t, pcParent, map[string]any{"name": "P2"})
	m := f2.mark(t, pcOpt, map[string]any{"name": "mau", "ref": p2})
	res, err := f2.repo.PurgeRecord(f2.ctx, sampleParams(p2, nil))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(res.Cleared) != 1 || res.Cleared[0].ID != m || !f2.exists(t, pcOpt, m) {
		t.Fatalf("cleared = %+v; want the sample holder emptied and kept", res.Cleared)
	}
}

// A link row to a REAL peer blocks (and survives); a link between two samples goes with the cluster.
func TestPurgeSample_JoinLinks(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	p := f.mark(t, pcParent, map[string]any{"name": "P"})
	realPeer := f.insert(t, pcPeer, map[string]any{"name": "that"})
	if _, err := f.repo.LinkRecords(f.ctx, pcTenant, pcUser, pcParent, p, pcPeer, realPeer); err != nil {
		t.Fatalf("link: %v", err)
	}
	_, err := f.repo.PurgeRecord(f.ctx, sampleParams(p, nil))
	got := blockedBy(t, err)
	if len(got) != 1 || got[0].Entity != pcPeer || got[0].Count != 1 {
		t.Fatalf("blockers = %+v; want the real peer x1", got)
	}
	joinTable := "rel_" + pcParent + "_" + pcPeer
	if n := f.count(t, "SELECT COUNT(*) FROM `"+joinTable+"` WHERE tenant_id = ?", pcTenant); n != 1 {
		t.Fatalf("link rows = %d; a blocked purge must leave the real link intact", n)
	}

	f2 := newPCFixture(t).withSampleCleanup(t)
	p2 := f2.mark(t, pcParent, map[string]any{"name": "P2"})
	peer2 := f2.mark(t, pcPeer, map[string]any{"name": "mau"})
	if _, err := f2.repo.LinkRecords(f2.ctx, pcTenant, pcUser, pcParent, p2, pcPeer, peer2); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := f2.repo.PurgeRecord(f2.ctx, sampleParams(p2, nil)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n := f2.count(t, "SELECT COUNT(*) FROM `"+joinTable+"` WHERE tenant_id = ?", pcTenant); n != 0 {
		t.Fatalf("link rows = %d; want the sample-to-sample link gone", n)
	}
	if !f2.exists(t, pcPeer, peer2) {
		t.Fatal("the peer sample must stay: it is its own root")
	}
}

// Pinned: a root or a marked required child that a live relation field names as its default blocks
// the cluster with kind field_default, naming the field that pins it.
func TestPurgeSample_PinnedBlocks(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	p := f.mark(t, pcParent, map[string]any{"name": "P"})
	c := f.mark(t, pcChild, map[string]any{"name": "C", "parent": p})

	for name, tc := range map[string]struct {
		pin  map[string]PurgePin
		want PurgeBlocker
	}{
		"root":  {map[string]PurgePin{PinKey(pcParent, p): {Entity: pcOpt, Field: "ref"}}, PurgeBlocker{Entity: pcOpt, Field: "ref", Count: 1, Kind: BlockerFieldDefault}},
		"child": {map[string]PurgePin{PinKey(pcChild, c): {Entity: pcGrand, Field: "child"}}, PurgeBlocker{Entity: pcGrand, Field: "child", Count: 1, Kind: BlockerFieldDefault}},
	} {
		_, err := f.repo.PurgeRecord(f.ctx, sampleParams(p, tc.pin))
		got := blockedBy(t, err)
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("%s: blockers = %+v; want %+v", name, got, tc.want)
		}
		if !f.exists(t, pcParent, p) || !f.exists(t, pcChild, c) {
			t.Fatalf("%s: a pinned cluster was (partly) deleted", name)
		}
	}
}

// The invariant of the whole feature: a wipe-style sweep over a mixed tenant leaves every unmarked
// row of every table where it was, version included.
func TestPurgeSample_UnmarkedRowsUntouched(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	type row struct {
		entity, id string
		version    int
	}
	var real []row
	for _, e := range []string{pcParent, pcChild, pcOpt, pcPeer} {
		for i := 0; i < 2; i++ {
			data := map[string]any{"name": e}
			if e == pcChild {
				data["parent"] = f.insert(t, pcParent, map[string]any{"name": "real-parent"})
			}
			id := f.insert(t, e, data)
			real = append(real, row{e, id, f.version(t, e, id)})
		}
	}
	var roots []string
	for i := 0; i < 3; i++ {
		p := f.mark(t, pcParent, map[string]any{"name": "mau"})
		f.mark(t, pcChild, map[string]any{"name": "mau", "parent": p})
		f.mark(t, pcOpt, map[string]any{"name": "mau", "ref": p})
		roots = append(roots, p)
	}
	for _, p := range roots {
		if _, err := f.repo.PurgeRecord(f.ctx, sampleParams(p, nil)); err != nil {
			t.Fatalf("purge %s: %v", p, err)
		}
	}
	for _, r := range real {
		if !f.exists(t, r.entity, r.id) || f.version(t, r.entity, r.id) != r.version {
			t.Fatalf("real row %s/%s was deleted or rewritten", r.entity, r.id)
		}
	}
	if n := f.count(t, "SELECT COUNT(*) FROM sys_sample_records WHERE tenant_id = ? AND entity_name IN (?, ?)", pcTenant, pcParent, pcChild); n != 0 {
		t.Fatalf("%d marks left", n)
	}
}

// 056's own bin purge also sweeps the mark now (no orphan marks left by a hard delete).
func TestPurge_SweepsSampleMark(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	p := f.mark(t, pcParent, map[string]any{"name": "P"})
	f.bin(t, pcParent, p)
	params := PurgeParams{Entity: pcParent, TenantID: pcTenant, ID: p, ActorID: pcActor, Graph: pcGraph()}
	if _, err := f.repo.PurgeRecord(f.ctx, params); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if f.marks(t, pcParent, p) != 0 {
		t.Fatal("the mark outlived its hard-deleted row")
	}
}

// SampleRoots lists the marked rows (live and binned) of one table, tenant-pinned.
func TestSampleRoots_ListsLiveAndBinnedPerTenant(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	a := f.mark(t, pcParent, map[string]any{"name": "a"})
	b := f.mark(t, pcParent, map[string]any{"name": "b"})
	f.bin(t, pcParent, b)
	f.insert(t, pcParent, map[string]any{"name": "real"})
	// an orphan mark (its row hard-deleted) must not appear
	if _, err := f.db.Exec("INSERT INTO sys_sample_records (id, tenant_id, entity_name, record_id, batch_id, source, created_by) VALUES (?, ?, ?, ?, ?, 'ai', ?)",
		uuid.NewString(), pcTenant, pcParent, uuid.NewString(), uuid.NewString(), pcUser); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.SampleRoots(f.ctx, pcTenant, pcParent)
	if err != nil {
		t.Fatalf("SampleRoots: %v", err)
	}
	if len(got) != 2 || got[0].ID == got[1].ID || (got[0].ID != a && got[0].ID != b) || (got[1].ID != a && got[1].ID != b) ||
		got[0].BatchID == "" || got[1].BatchID == "" {
		t.Fatalf("roots = %+v; want a and b with their batch ids", got)
	}
	other, err := f.repo.SampleRoots(f.ctx, "56000000-0000-0000-0000-0000000000ff", pcParent)
	if err != nil || len(other) != 0 {
		t.Fatalf("other tenant: %v, %v", other, err)
	}
}

// ResetSequenceIfEmpty drops the counter row only when the table has no rows at all (bin included).
func TestResetSequenceIfEmpty(t *testing.T) {
	f := newPCFixture(t).withSampleCleanup(t)
	claim := func() {
		if _, err := f.repo.ClaimSequence(f.ctx, pcTenant, pcParent, "code"); err != nil {
			t.Fatalf("claim: %v", err)
		}
	}
	claim()
	claim()
	id := f.insert(t, pcParent, map[string]any{"name": "x"})
	f.bin(t, pcParent, id)
	if reset, err := f.repo.ResetSequenceIfEmpty(f.ctx, pcTenant, pcParent, "code"); err != nil || reset {
		t.Fatalf("binned row left: reset=%v err=%v; want no reset", reset, err)
	}
	if _, err := f.db.Exec("DELETE FROM obj_" + pcParent + " WHERE tenant_id = '" + pcTenant + "'"); err != nil {
		t.Fatal(err)
	}
	if reset, err := f.repo.ResetSequenceIfEmpty(f.ctx, pcTenant, pcParent, "code"); err != nil || !reset {
		t.Fatalf("empty table: reset=%v err=%v; want reset", reset, err)
	}
	if got, err := f.repo.ClaimSequence(f.ctx, pcTenant, pcParent, "code"); err != nil || got != 1 {
		t.Fatalf("first claim after reset = %d, %v; want 1", got, err)
	}
	if reset, err := f.repo.ResetSequenceIfEmpty(f.ctx, pcTenant, pcParent, "nope"); err != nil || reset {
		t.Fatalf("no counter row: reset=%v err=%v; want false", reset, err)
	}
}
