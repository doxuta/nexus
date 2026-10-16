//go:build integration

package tenantbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
)

func jsonlRows(t *testing.T, text string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func (e *env) count(table string, tenantID string) int64 {
	e.t.Helper()
	ref, ok := RuleFor(table)
	if !ok {
		e.t.Fatalf("no rule for %s", table)
	}
	q, err := TableRef{Table: table, Rule: ref}.countQuery()
	if err != nil {
		e.t.Fatal(err)
	}
	var n int64
	if err := e.db.Get(&n, q, tenantID); err != nil {
		e.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// T010 (b)(c)(d)(e)(f)(g)(n): ONE customer's file, the whole truth and nothing else.
func TestBackup_ContentIsolationSecretsAndCompleteness(t *testing.T) {
	e := newEnv(t, nil)
	// Attachments: a live file, a deleted-row tombstone whose bytes are still on disk, a TIMESTAMP(3).
	liveID := e.putFile(e.A, "hello bytes", false, "2026-03-04 05:06:07.123")
	tombID := e.putFile(e.A, "tombstone bytes", true, "")
	bID := e.putFile(e.B, "B-ONLY-BYTES", false, "")

	b := e.create(e.A)
	if b.Status != StatusReady {
		t.Fatalf("status = %s (%v %v)", b.Status, b.ErrorCode, b.ErrorDetail)
	}
	files := entries(t, e.zipBytes(b))

	// --- (d) not one secret anywhere in any inflated entry -------------------------------------
	for name, text := range files {
		if strings.Contains(text, sentinel) {
			i := strings.Index(text, sentinel)
			t.Errorf("%s contains a planted secret: …%s…", name, text[max(0, i-30):min(len(text), i+60)])
		}
	}

	// --- manifest + isolation --------------------------------------------------------------
	var m Manifest
	if err := json.Unmarshal([]byte(files["manifest.json"]), &m); err != nil {
		t.Fatal(err)
	}
	if m.Tenant.ID != e.A.ID || m.Tenant.Slug != e.A.Slug || m.Kind != KindManual || m.FormatVersion != 1 {
		t.Errorf("manifest header = %+v", m)
	}
	if m.CreatedBy != "Nguyễn Văn A ("+e.W.Name+")" {
		t.Errorf("manifest.createdBy = %q, want the operator's label", m.CreatedBy)
	}
	for _, leak := range []string{e.opUser, e.W.ID, e.W.Slug, "@northwind.test"} {
		if strings.Contains(files["manifest.json"], leak) || strings.Contains(files["README.txt"], leak) {
			t.Errorf("manifest/README must not carry the operator/workshop identity %q", leak)
		}
	}
	if m.SchemaVersion < 85 {
		t.Errorf("schemaVersion = %d", m.SchemaVersion)
	}
	var bIDs = []string{e.B.ID, e.uB, bID}
	bIDs = append(bIDs, e.objB...)
	for name, text := range files {
		for _, id := range bIDs {
			if strings.Contains(text, id) {
				t.Errorf("%s contains customer B's id %s", name, id)
			}
		}
	}
	tables := map[string]ManifestTable{}
	for _, mt := range m.Tables {
		tables[mt.Name] = mt
		text, ok := files["tables/"+mt.Name+".jsonl"]
		if !ok {
			t.Errorf("table %s is in the manifest but has no entry", mt.Name)
			continue
		}
		rows := jsonlRows(t, text)
		if int64(len(rows)) != mt.Rows {
			t.Errorf("%s: manifest says %d rows, file has %d", mt.Name, mt.Rows, len(rows))
		}
		if want := e.count(mt.Name, e.A.ID); want != mt.Rows {
			t.Errorf("%s: file has %d rows, COUNT(*) WHERE tenant_id = A is %d", mt.Name, mt.Rows, want)
		}
		for _, r := range rows {
			if tid, has := r["tenant_id"]; has && tid != e.A.ID {
				t.Errorf("%s: row of another customer: %v", mt.Name, r)
			}
		}
	}
	// (c) the three children without tenant_id arrive through their parent, A's only.
	if tables["acl_role_actions"].Rows != 2 || tables["team_memberships"].Rows != 1 || tables["team_sets_teams"].Rows != 1 {
		t.Errorf("children rows: acl_role_actions=%d team_memberships=%d team_sets_teams=%d",
			tables["acl_role_actions"].Rows, tables["team_memberships"].Rows, tables["team_sets_teams"].Rows)
	}
	if tables["team_memberships"].OwnerFK == nil || tables["team_memberships"].OwnerFK.Parent != "teams" {
		t.Errorf("team_memberships ownerFk = %+v", tables["team_memberships"].OwnerFK)
	}

	// --- (e)(f) soft-deleted rows travel; created_at travels; generated columns do not -------
	obj := jsonlRows(t, files["tables/"+e.objTable+".jsonl"])
	if len(obj) != 3 {
		t.Fatalf("obj table rows = %d, want A's 3 (one soft-deleted)", len(obj))
	}
	deleted := 0
	for _, r := range obj {
		if r["deleted_at"] != nil {
			deleted++
		}
		if _, ok := r["created_at"]; !ok {
			t.Errorf("created_at must be in the file (EXTRA=DEFAULT_GENERATED is a real column): %v", r)
		}
		if _, ok := r["code"]; ok {
			t.Errorf("the generated unique column `code` must not be in the file: %v", r)
		}
	}
	if deleted != 1 {
		t.Errorf("the soft-deleted row must be in the file (deleted rows = %d)", deleted)
	}
	cols := map[string]bool{}
	for _, c := range tables[e.objTable].Columns {
		cols[c.Name] = true
	}
	if cols["code"] || !cols["created_at"] || !cols["data"] {
		t.Errorf("manifest columns for the obj table = %v", cols)
	}
	// users: identity travels, the hash does not — not even the key.
	users := jsonlRows(t, files["tables/users.jsonl"])
	if len(users) != 1 {
		t.Fatalf("users rows = %d, want only A's one", len(users))
	}
	if _, has := users[0]["password_hash"]; has {
		t.Error("users.password_hash must not be in the file")
	}
	// (g) TIMESTAMP(3) keeps its milliseconds, pinned to UTC regardless of the DSN.
	var liveRow map[string]any
	for _, r := range jsonlRows(t, files["tables/sys_files.jsonl"]) {
		if r["id"] == liveID {
			liveRow = r
		}
	}
	if liveRow == nil || liveRow["created_at"] != "2026-03-04 05:06:07.123000" {
		t.Errorf("sys_files.created_at = %v, want 2026-03-04 05:06:07.123000", liveRow["created_at"])
	}
	// (e) tombstone: row present, bytes present; B's attachment absent.
	if files["blobs/"+liveID] != "hello bytes" || files["blobs/"+tombID] != "tombstone bytes" {
		t.Error("the live blob and the tombstone's blob must be in the file")
	}
	if _, has := files["blobs/"+bID]; has {
		t.Error("B's blob must not be in A's file")
	}

	// --- excluded things are named, with reasons ---------------------------------------------
	ex := map[string]bool{}
	for _, x := range m.ExcludedTables {
		ex[x.Table] = true
	}
	for _, must := range []string{"refresh_tokens", "user_mfa", "sys_api_keys", "sys_named_credentials", "sys_inbound_hooks", "sys_jobs", "sys_tenant_backups"} {
		if !ex[must] {
			t.Errorf("excludedTables must list %s", must)
		}
	}
	if len(m.ExcludedColumns) < 2 {
		t.Errorf("excludedColumns = %+v", m.ExcludedColumns)
	}
	jp := map[string]int{}
	for _, x := range m.ExcludedJSON {
		jp[x.Table+"."+x.Column] = x.Count
	}
	if jp["sys_workflows.actions"] != 1 || jp["sys_pack_items.content"] != 1 {
		t.Errorf("excludedJsonPaths must name BOTH (table, column) pairs that lost a credential: %v", jp)
	}
	// ...while the URL and an unknown key of the redacted action survive.
	wf := files["tables/sys_workflows.jsonl"]
	if !strings.Contains(wf, "https://partner.example/a") || !strings.Contains(wf, "a-keep") {
		t.Errorf("redaction must remove exactly secret+headers, keeping url and other keys: %s", wf)
	}
	pk := files["tables/sys_pack_items.jsonl"]
	if !strings.Contains(pk, "https://partner.example/a") || strings.Contains(pk, `"secret"`) || strings.Contains(pk, `"headers"`) {
		t.Errorf("sys_pack_items.content must be redacted too: %s", pk)
	}

	// --- row counters, hash, size ---------------------------------------------------------------
	if b.SHA256 == nil || len(*b.SHA256) != 64 || b.SizeBytes == nil || *b.SizeBytes != int64(len(e.zipBytes(b))) {
		t.Errorf("sha256/size not recorded: %v %v", b.SHA256, b.SizeBytes)
	}
	if b.FileCount == nil || *b.FileCount != 2 {
		t.Errorf("file_count = %v, want 2", b.FileCount)
	}

	// --- (n) audit: one row in the workshop, one in the customer with a human label --------------
	var cust, work *audit.Event
	for i := range e.aud.events {
		ev := e.aud.events[i]
		if ev.EntityName != audit.TenantBackupMeta || ev.Action != audit.ActionCreate {
			continue
		}
		if ev.TenantID == e.A.ID {
			cust = &ev
		}
		if ev.TenantID == e.W.ID {
			work = &ev
		}
	}
	if cust == nil || work == nil {
		t.Fatalf("create must be audited in both tenants: %+v", e.aud.events)
	}
	if cust.Diff.After["by"] != "Nguyễn Văn A ("+e.W.Name+")" {
		t.Errorf("customer-side after.by = %v", cust.Diff.After["by"])
	}
	if strings.Contains(toJSON(cust.Diff), e.W.ID) || strings.Contains(toJSON(cust.Diff), e.opUser) {
		t.Errorf("the customer-side diff must not carry the workshop tenant id or the operator id: %s", toJSON(cust.Diff))
	}
	e.aud.events = nil
	dl, err := e.svc.OpenFile(context.Background(), e.A.ID, e.actor(), b.ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = dl.Body.Close()
	if len(e.aud.events) != 2 || e.aud.events[0].Action != audit.ActionDownload {
		t.Errorf("download must be audited in both tenants: %+v", e.aud.events)
	}
}

// T010 (a customer already retired): FR-020.
func TestBackup_RetiredCustomerCanBeBackedUp(t *testing.T) {
	e := newEnv(t, nil)
	e.exec("UPDATE tenants SET deleted_at = NOW(3) WHERE id = ?", e.A.ID)
	b := e.create(e.A)
	if b.Status != StatusReady {
		t.Fatalf("a retired customer must still be backed up: %s %v", b.Status, b.ErrorDetail)
	}
	v, err := e.svc.Get(context.Background(), e.A.ID, e.W.ID, b.ID)
	if err != nil || v.Restorable || v.NotRestorableReason == nil || *v.NotRestorableReason != reasonRetired {
		t.Errorf("view = %+v err = %v", v, err)
	}
}

// Most customers have NO branding (tenants.branding IS NULL). The first live run of slice A2 got a
// 500 here: the tenant lookup scanned that NULL into a json.RawMessage. Every other test seeded a
// branding value, which is exactly why none of them saw it.
func TestBackup_CustomerWithNullBrandingIsBackedUp(t *testing.T) {
	e := newEnv(t, nil)
	e.exec("UPDATE tenants SET branding = NULL WHERE id = ?", e.A.ID)
	b := e.create(e.A)
	if b.Status != StatusReady {
		t.Fatalf("status = %s (%v %v)", b.Status, b.ErrorCode, b.ErrorDetail)
	}
	files := entries(t, e.zipBytes(b))
	var tj struct {
		Branding any `json:"branding"`
	}
	if err := json.Unmarshal([]byte(files["tenant.json"]), &tj); err != nil || tj.Branding != nil {
		t.Errorf("tenant.json = %q (%v), want {\"branding\":null}", files["tenant.json"], err)
	}
	// listing / reading such a customer works too
	if _, _, err := e.svc.List(context.Background(), ListInput{TenantID: e.A.ID, CallerTenantID: e.W.ID}); err != nil {
		t.Errorf("List: %v", err)
	}
}

// T010 (h): two POSTs at once => exactly one 409, decided by the DATABASE (uq_tbk_oplock).
func TestBackup_TwoConcurrentCreatesExactlyOne409(t *testing.T) {
	var g *gatedSnaps
	e := newEnv(t, func(in SnapshotSource) SnapshotSource { g = newGated(in); return g })
	ctx := context.Background()

	first, err := e.svc.Create(ctx, CreateInput{TenantID: e.A.ID, Actor: e.actor()})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err = e.svc.Create(ctx, CreateInput{TenantID: e.A.ID, Actor: e.actor()})
	asErr(t, err, "BACKUP_IN_PROGRESS")
	// A DIFFERENT customer is not blocked by A's running backup.
	other, err := e.svc.Create(ctx, CreateInput{TenantID: e.B.ID, Actor: e.actor()})
	if err != nil {
		t.Fatalf("customer B must not be blocked by A's backup: %v", err)
	}
	close(g.release)
	if b := e.waitDone(e.A, first.ID); b.Status != StatusReady {
		t.Fatalf("first backup status = %s (%v)", b.Status, b.ErrorDetail)
	}
	e.waitDone(e.B, other.ID)
	// the slot frees once the backup is no longer running
	if _, err := e.svc.Create(ctx, CreateInput{TenantID: e.A.ID, Actor: e.actor()}); err != nil {
		t.Fatalf("after the first finished, a new backup must be accepted: %v", err)
	}
}

// T010 (i): the snapshot connection is pinned to UTC while it works and handed back EXACTLY as it
// was found. With a pool of one, the connection that comes back IS the one that was changed.
func TestSnapshot_ConnectionTimeZoneIsRestored(t *testing.T) {
	db := itDB(t)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "SET time_zone = '+05:30'"); err != nil {
		t.Fatal(err)
	}
	snaps := NewSqlxSnapshots(db)
	s, err := snaps.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var inside string
	if err := s.(*sqlxSnapshot).tx.GetContext(ctx, &inside, "SELECT @@session.time_zone"); err != nil {
		t.Fatal(err)
	}
	if inside != "+00:00" {
		t.Errorf("inside the snapshot time_zone = %s, want +00:00", inside)
	}
	s.Close(ctx)
	var after string
	if err := db.GetContext(ctx, &after, "SELECT @@session.time_zone"); err != nil {
		t.Fatal(err)
	}
	if after != "+05:30" {
		t.Errorf("after Close the connection's time_zone = %s, want the original +05:30", after)
	}
	s.Close(ctx) // idempotent
}

// T010 (j): a missing blob and a changed blob do not fail the backup; they are warned.
func TestBackup_MissingAndChangedBlobsWarnButSucceed(t *testing.T) {
	e := newEnv(t, nil)
	okID := e.putFile(e.A, "same", false, "")
	changedID := e.putFile(e.A, "original", false, "")
	missingID := e.putFile(e.A, "will vanish", false, "")
	if _, err := e.blobs.Put(context.Background(), e.A.ID+"/"+changedID, strings.NewReader("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := e.blobs.Delete(context.Background(), e.A.ID+"/"+missingID); err != nil {
		t.Fatal(err)
	}
	b := e.create(e.A)
	if b.Status != StatusReady {
		t.Fatalf("status = %s %v", b.Status, b.ErrorDetail)
	}
	var warns []Warning
	_ = json.Unmarshal(b.Warnings, &warns)
	got := map[string][]string{}
	for _, w := range warns {
		got[w.Code] = w.Sample
	}
	if len(got[WarnBlobMissing]) != 1 || got[WarnBlobMissing][0] != missingID {
		t.Errorf("BLOB_MISSING = %v", got[WarnBlobMissing])
	}
	if len(got[WarnBlobShaMismatch]) != 1 || got[WarnBlobShaMismatch][0] != changedID {
		t.Errorf("BLOB_SHA_MISMATCH = %v", got[WarnBlobShaMismatch])
	}
	files := entries(t, e.zipBytes(b))
	if files["blobs/"+okID] != "same" || files["blobs/"+changedID] != "tampered" {
		t.Error("a mismatching blob is still copied (the warning tells the operator)")
	}
}

// T010 (k): a row committed after the snapshot was taken is NOT in the file.
func TestBackup_SnapshotIsConsistent(t *testing.T) {
	var g *gatedSnaps
	var e *env
	e = newEnv(t, func(in SnapshotSource) SnapshotSource { g = newGated(in); return g })
	var lateID string
	g.after = func(Snapshot) {
		lateID = uuid.NewString()
		e.exec("INSERT INTO `"+e.objTable+"` (id, tenant_id, created_by, data) VALUES (?, ?, ?, '{\"code\":\"LATE\"}')", lateID, e.A.ID, e.opUser)
	}
	v, err := e.svc.Create(context.Background(), CreateInput{TenantID: e.A.ID, Actor: e.actor()})
	if err != nil {
		t.Fatal(err)
	}
	close(g.release)
	b := e.waitDone(e.A, v.ID)
	if b.Status != StatusReady {
		t.Fatalf("status = %s %v", b.Status, b.ErrorDetail)
	}
	if lateID == "" {
		t.Fatal("hook did not run")
	}
	for name, text := range entries(t, e.zipBytes(b)) {
		if strings.Contains(text, lateID) {
			t.Errorf("%s contains a row committed after the snapshot: the read is not one consistent snapshot", name)
		}
	}
	if e.count(e.objTable, e.A.ID) != 4 {
		t.Errorf("the late row must exist in the live table (so the test proves something): %d", e.count(e.objTable, e.A.ID))
	}
}

// T010 (l): a sys_files.path hand-edited to point into another customer's directory must never
// copy that customer's bytes.
func TestBackup_ForeignBlobPathIsNeverRead(t *testing.T) {
	e := newEnv(t, nil)
	bFile := e.putFile(e.B, "B-SECRET-BYTES", false, "")
	aFile := e.putFile(e.A, "a bytes", false, "")
	e.exec("UPDATE sys_files SET path = ? WHERE id = ? AND tenant_id = ?", e.B.ID+"/"+bFile, aFile, e.A.ID)
	traverse := e.putFile(e.A, "t", false, "")
	e.exec("UPDATE sys_files SET path = ? WHERE id = ? AND tenant_id = ?", e.A.ID+"/../"+e.B.ID+"/"+bFile, traverse, e.A.ID)

	b := e.create(e.A)
	if b.Status != StatusReady {
		t.Fatalf("status = %s %v", b.Status, b.ErrorDetail)
	}
	body := e.zipBytes(b)
	for name, text := range entries(t, body) {
		if strings.Contains(text, "B-SECRET-BYTES") {
			t.Fatalf("%s carries customer B's attachment bytes", name)
		}
	}
	var warns []Warning
	_ = json.Unmarshal(b.Warnings, &warns)
	foreign := 0
	for _, w := range warns {
		if w.Code == WarnBlobForeign {
			foreign = w.Count
		}
	}
	if foreign != 2 {
		t.Errorf("BLOB_PATH_FOREIGN count = %d, want 2 (%v)", foreign, warns)
	}
}

// T010 (m): the purge deletes the row while the backup is still starting; the backup must notice
// (0 rows updated) and delete its own file — and the purge's remover sweeps the directory.
func TestBackup_PurgeRaceLeavesNoFile(t *testing.T) {
	var g *gatedSnaps
	e := newEnv(t, func(in SnapshotSource) SnapshotSource { g = newGated(in); return g })
	v, err := e.svc.Create(context.Background(), CreateInput{TenantID: e.A.ID, Actor: e.actor()})
	if err != nil {
		t.Fatal(err)
	}
	<-g.opened
	// the purge transaction: every catalogue row of the customer goes
	e.exec("DELETE FROM sys_tenant_backups WHERE tenant_id = ?", e.A.ID)
	close(g.release)
	// Wait for the job to finish ON ITS OWN (Stop would cancel it and prove the wrong path).
	done := make(chan struct{})
	go func() { e.runner.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the backup job did not finish")
	}
	var left []string
	_ = filepath.Walk(e.backupsDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Fatalf("a backup whose row was purged left files behind: %v (backup %s)", left, v.ID)
	}
	// ...and the remover the purge calls takes the directory too.
	if err := os.MkdirAll(filepath.Join(e.backupsDir, e.A.ID), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.backupsDir, e.A.ID, "stray.zip"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	removed, err := NewRemover(e.backups, e.backupsDir)(context.Background(), e.A.ID, nil)
	if err != nil || removed != 0 {
		t.Fatalf("remover = %d, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(e.backupsDir, e.A.ID)); !os.IsNotExist(err) {
		t.Errorf("the customer's directory must be gone: %v", err)
	}
}

// ---- repository semantics --------------------------------------------------------------------

func TestStore_OpLockIsEnforcedByTheDatabase(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	mk := func(status string) *Backup {
		id := uuid.NewString()
		k := e.A.ID + "/" + id + ".zip"
		return &Backup{ID: id, TenantID: e.A.ID, Kind: KindManual, Status: status, StorageKey: &k, FormatVersion: 1, SchemaVersion: 85}
	}
	if err := e.store.Insert(ctx, mk(StatusRunning)); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Insert(ctx, mk(StatusRunning)); !errors.Is(err, ErrInProgress) {
		t.Fatalf("a second running row for the same customer must be ErrInProgress, got %v", err)
	}
	// other customer, and non-running rows, are unaffected
	other := mk(StatusRunning)
	other.TenantID = e.B.ID
	if err := e.store.Insert(ctx, other); err != nil {
		t.Fatalf("another customer: %v", err)
	}
	if err := e.store.Insert(ctx, mk(StatusReady)); err != nil {
		t.Fatalf("a ready row never takes the lock: %v", err)
	}
	if err := e.store.Insert(ctx, mk(StatusFailed)); err != nil {
		t.Fatalf("a failed row never takes the lock: %v", err)
	}
}

func TestStore_MarkReadyAndFailedAreCompareAndSet(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	id := uuid.NewString()
	k := e.A.ID + "/" + id + ".zip"
	if err := e.store.Insert(ctx, &Backup{ID: id, TenantID: e.A.ID, Kind: KindManual, Status: StatusRunning, StorageKey: &k, FormatVersion: 1, SchemaVersion: 85}); err != nil {
		t.Fatal(err)
	}
	// wrong tenant never matches
	if n, err := e.store.Heartbeat(ctx, e.B.ID, id); err != nil || n != 0 {
		t.Errorf("Heartbeat under another tenant = %d, %v; want 0", n, err)
	}
	if n, err := e.store.Heartbeat(ctx, e.A.ID, id); err != nil || n != 1 {
		t.Errorf("Heartbeat = %d, %v; want 1", n, err)
	}
	ready := ReadyFields{SizeBytes: 10, SHA256: strings.Repeat("a", 64), SchemaVersion: 85, RowCount: 3, TableCount: 1, FileCount: 0,
		Warnings: []Warning{{Code: WarnBlobMissing, Count: 1, Sample: []string{"x"}}}}
	if n, err := e.store.MarkReady(ctx, e.B.ID, id, ready); err != nil || n != 0 {
		t.Errorf("MarkReady under another tenant = %d, %v; want 0", n, err)
	}
	if n, err := e.store.MarkReady(ctx, e.A.ID, id, ready); err != nil || n != 1 {
		t.Fatalf("MarkReady = %d, %v; want 1", n, err)
	}
	// the WHERE excludes the state it writes: the second finisher and a late failure both lose
	if n, _ := e.store.MarkReady(ctx, e.A.ID, id, ready); n != 0 {
		t.Errorf("second MarkReady = %d, want 0", n)
	}
	if n, _ := e.store.MarkFailed(ctx, e.A.ID, id, "BACKUP_ABANDONED", "x"); n != 0 {
		t.Errorf("MarkFailed after ready = %d, want 0 (it must not overwrite a finished backup)", n)
	}
	if n, _ := e.store.Heartbeat(ctx, e.A.ID, id); n != 0 {
		t.Errorf("Heartbeat on a finished backup = %d, want 0", n)
	}
	got, err := e.store.Get(ctx, e.A.ID, id)
	if err != nil || got.Status != StatusReady || got.FinishedAt == nil || got.RowCount == nil || *got.RowCount != 3 {
		t.Fatalf("row = %+v err = %v", got, err)
	}
	if _, err := e.store.Get(ctx, e.B.ID, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("another customer's id under this customer's path must be ErrNotFound, got %v", err)
	}
	e.exec("UPDATE sys_tenant_backups SET deleted_at = NOW(3) WHERE id = ?", id)
	if _, err := e.store.Get(ctx, e.A.ID, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("a hidden row must be ErrNotFound, got %v", err)
	}
}

// B1: keyset pagination in the SAME direction as the sort, with ties on created_at.
func TestStore_ListKeysetNoGapNoDuplicateAndUsesTheIndex(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 7; i++ {
		id := uuid.NewString()
		ids = append(ids, id)
		// identical created_at for four of them: the id tie-break must carry the order
		ts := "2026-05-05 10:00:00.000"
		if i >= 4 {
			ts = "2026-05-05 10:00:0" + string(rune('1'+i-4)) + ".000"
		}
		e.exec(`INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, format_version, schema_version, created_at)
			VALUES (?, ?, 'manual', 'ready', 1, 85, ?)`, id, e.A.ID, ts)
	}
	e.exec(`INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, format_version, schema_version) VALUES (?, ?, 'manual', 'ready', 1, 85)`, uuid.NewString(), e.B.ID)

	var seen []string
	var after *Cursor
	for page := 0; page < 10; page++ {
		rows, err := e.store.List(ctx, e.A.ID, 3, after) // limit 3 => pages of 3,3,1
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			seen = append(seen, r.ID)
		}
		if len(rows) < 3 {
			break
		}
		last := rows[len(rows)-1]
		after = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
		if dec, err := DecodeCursor(after.Encode()); err != nil || dec.ID != last.ID || !dec.CreatedAt.Equal(last.CreatedAt) {
			t.Fatalf("cursor round trip: %+v %v", dec, err)
		}
	}
	if len(seen) != 7 {
		t.Fatalf("paged %d rows, want 7: a gap or a duplicate at a page boundary (%v)", len(seen), seen)
	}
	uniq := map[string]bool{}
	for _, id := range seen {
		uniq[id] = true
	}
	if len(uniq) != 7 {
		t.Errorf("duplicate across pages: %v", seen)
	}
	// the order is created_at DESC, id DESC
	rows, _ := e.store.List(ctx, e.A.ID, 100, nil)
	sorted := sort.SliceIsSorted(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	if !sorted {
		t.Error("List must be ordered created_at DESC, id DESC")
	}

	// EXPLAIN: the first page and a later page both walk idx_tbk_tenant_created, no filesort.
	for name, q := range map[string]string{
		"first page": "EXPLAIN SELECT * FROM sys_tenant_backups WHERE tenant_id = ? AND deleted_at IS NULL ORDER BY created_at DESC, id DESC LIMIT 4",
		"next page":  "EXPLAIN SELECT * FROM sys_tenant_backups WHERE tenant_id = ? AND deleted_at IS NULL AND (created_at < '2026-05-05 10:00:02' OR (created_at = '2026-05-05 10:00:02' AND id < 'zzz')) ORDER BY created_at DESC, id DESC LIMIT 4",
	} {
		rows, err := e.db.Queryx(q, e.A.ID)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var plan []map[string]any
		for rows.Next() {
			m := map[string]any{}
			if err := rows.MapScan(m); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, m)
		}
		_ = rows.Close()
		str := func(v any) string {
			if b, ok := v.([]byte); ok {
				return string(b)
			}
			return fmt.Sprint(v)
		}
		if len(plan) != 1 || str(plan[0]["key"]) != "idx_tbk_tenant_created" {
			t.Errorf("%s: key = %v, want idx_tbk_tenant_created", name, plan)
		}
		if strings.Contains(str(plan[0]["Extra"]), "filesort") {
			t.Errorf("%s: filesort in %s", name, str(plan[0]["Extra"]))
		}
	}
}

func TestReaper_RealRows(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	mk := func(minutesAgo int) (string, string) {
		id := uuid.NewString()
		key := e.A.ID + "/" + id + ".zip"
		e.exec(`INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, storage_key, format_version, schema_version) VALUES (?, ?, 'manual', 'running', ?, 1, 85)`, id, e.A.ID, key)
		e.exec("UPDATE sys_tenant_backups SET updated_at = NOW(3) - INTERVAL ? MINUTE WHERE id = ?", minutesAgo, id)
		return id, key
	}
	// only one running row per customer can exist: use customer B for the fresh one
	staleID, staleKey := mk(45)
	freshID := uuid.NewString()
	e.exec(`INSERT INTO sys_tenant_backups (id, tenant_id, kind, status, format_version, schema_version) VALUES (?, ?, 'manual', 'running', 1, 85)`, freshID, e.B.ID)
	if _, err := e.backups.Put(ctx, staleKey, bytes.NewReader([]byte("partial"))); err != nil {
		t.Fatal(err)
	}
	n, err := e.svc.ReapStale(ctx)
	if err != nil || n < 1 {
		t.Fatalf("ReapStale = %d, %v", n, err)
	}
	st, _ := e.store.Get(ctx, e.A.ID, staleID)
	if st.Status != StatusFailed || st.ErrorCode == nil || *st.ErrorCode != "BACKUP_ABANDONED" {
		t.Errorf("stale row = %+v", st)
	}
	if _, err := e.backups.Open(ctx, staleKey); err == nil {
		t.Error("the stale backup's partial file must be removed")
	}
	fr, _ := e.store.Get(ctx, e.B.ID, freshID)
	if fr.Status != StatusRunning {
		t.Errorf("a backup with a fresh heartbeat must not be reaped: %s", fr.Status)
	}
	// the lock is released: A can start again
	if _, err := e.svc.Create(ctx, CreateInput{TenantID: e.A.ID, Actor: e.actor()}); err != nil {
		t.Errorf("after the reaper, a new backup must be accepted: %v", err)
	}
}

func TestOperatorLabel(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	if got := e.store.OperatorLabel(ctx, e.W.ID, e.opUser); got != "Nguyễn Văn A ("+e.W.Name+")" {
		t.Errorf("label = %q", got)
	}
	e.exec("UPDATE users SET full_name = '' WHERE id = ?", e.opUser)
	if got := e.store.OperatorLabel(ctx, e.W.ID, e.opUser); got != "Người dựng ("+e.W.Name+")" {
		t.Errorf("blank-name label = %q", got)
	}
	// a user id that is not in that workshop never resolves (and never errors)
	if got := e.store.OperatorLabel(ctx, e.A.ID, e.opUser); got != "Người dựng" {
		t.Errorf("unknown pair label = %q", got)
	}
	for _, leak := range []string{"@", e.opUser, e.W.ID} {
		if strings.Contains(e.store.OperatorLabel(ctx, e.W.ID, e.opUser), leak) {
			t.Errorf("label must carry no %q", leak)
		}
	}
}
