//go:build integration

package tenantbackup

// specs/068-tenant-backup-restore T071 — the restore against a real MySQL. Customers A and B share
// the same obj_ table; A is restored, B must not move by one bit.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"nexus/platform/internal/audit"
)

type fakeFreeze struct {
	mu                  sync.Mutex
	sets, extends, clrs int
}

func (f *fakeFreeze) Set(context.Context, string, time.Time, string) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	return nil, nil
}
func (f *fakeFreeze) Extend(context.Context, string, time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends++
	return nil
}
func (f *fakeFreeze) Clear(context.Context, string, *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clrs++
	return nil
}
func (f *fakeFreeze) FrozenUntil(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

type fakeDoc struct{ called int }

func (d *fakeDoc) Repair(context.Context, string) (int, int, error) { d.called++; return 0, 0, nil }
func (d *fakeDoc) FlushCaches(context.Context, string)              {}

// restoreEnv wires slice D onto the integration env.
func (e *env) restoreEnv(hooks ApplyHooks) (*fakeFreeze, *fakeDoc) {
	fz, doc := &fakeFreeze{}, &fakeDoc{}
	e.svc.WithPlan(e.store, 0).WithRestore(RestoreDeps{Store: e.store, Freezer: fz, Doctor: doc, Settle: time.Millisecond, Hooks: hooks})
	return fz, doc
}

// restoreAndWait runs StartRestore and waits until the pre_restore row stops running.
func (e *env) restoreAndWait(c cust, backupID string) (*RestoreStarted, *Backup, error) {
	e.t.Helper()
	st, err := e.svc.StartRestore(context.Background(), RestoreInput{TenantID: c.ID, BackupID: backupID, ConfirmSlug: c.Slug, Actor: e.actor()})
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		b, gerr := e.store.Get(context.Background(), c.ID, st.RestoreID)
		if gerr == nil && b.RestoreStatus != nil && *b.RestoreStatus != restoreRunning {
			return st, b, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatalf("restore %s still running after 90s", st.RestoreID)
	return nil, nil, nil
}

// restoreSums = checksums of the tables a restore REWRITES (class Restore or MergeMax).
func (e *env) restoreSums(c cust) map[string]string {
	out := map[string]string{}
	for name, v := range e.checksums(c) {
		if r, ok := RuleFor(name); ok && (r.Class == Restore || r.Class == MergeMax) && name != "sys_sequences" {
			out[name] = v
		}
	}
	return out
}

func (e *env) val(q string, args ...any) string {
	e.t.Helper()
	var s *string
	if err := e.db.Get(&s, q, args...); err != nil {
		e.t.Fatalf("%.60s: %v", q, err)
	}
	if s == nil {
		return "<null>"
	}
	return *s
}

func sameSums(t *testing.T, label string, want, got map[string]string) {
	t.Helper()
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: table %s = %s, want %s", label, k, got[k], v)
		}
	}
}

func TestRestore_RoundTrip_Integration(t *testing.T) {
	e := newEnv(t, nil)
	fz, doc := e.restoreEnv(ApplyHooks{})

	keep := e.putFile(e.A, "keep-me", false, "")
	gone := e.putFile(e.A, "deleted-for-good", false, "")
	// a second rule that exists only in the backup (webhook key must NOT come back for it)
	e.exec("INSERT INTO sys_workflows (id, tenant_id, name, entity_name, trigger_event, actions) VALUES (?, ?, 'wf-only-in-backup', 'accounts', 'after_create', ?)",
		uuid.NewString(), e.A.ID, `[{"type":"outbound_webhook","url":"https://old.example/x","secret":"OLD"}]`)
	e.exec("INSERT INTO sys_sequences (tenant_id, entity_name, field_name, next_val) VALUES (?, 'accounts', 'code', 5)", e.A.ID)

	s := e.create(e.A)
	if s.Status != StatusReady {
		t.Fatalf("backup = %s %v", s.Status, s.ErrorCode)
	}
	atBackup := e.restoreSums(e.A)
	roleOf := func() string { return e.val("SELECT role_id FROM users WHERE id = ?", e.uA) }
	roleBefore := roleOf()

	// ---- drift after the backup ----
	e.exec("DELETE FROM `"+e.objTable+"` WHERE id = ?", e.objA[0])
	e.exec("UPDATE `"+e.objTable+"` SET data = JSON_SET(data, '$.note', 'changed') WHERE id = ?", e.objA[1])
	e.exec("INSERT INTO `"+e.objTable+"` (id, tenant_id, created_by, data) VALUES (?, ?, ?, ?)",
		uuid.NewString(), e.A.ID, e.opUser, `{"code":"NEW-`+randSuffix(t)+`"}`)
	newRole, u2 := uuid.NewString(), uuid.NewString()
	e.exec("INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'dieu-phoi', 'Điều phối')", newRole, e.A.ID)
	e.exec("INSERT INTO users (id, tenant_id, email, password_hash, full_name, status, role_id) VALUES (?, ?, ?, 'PW2', 'U2', 'active', ?)",
		u2, e.A.ID, "u2-"+randSuffix(t)+"@khach.test", newRole)
	e.exec("UPDATE users SET password_hash = 'CHANGED_HASH' WHERE id = ?", e.uA)
	e.exec("UPDATE users SET status = 'inactive' WHERE id = ?", u2)
	e.exec("UPDATE sys_sequences SET next_val = 100 WHERE tenant_id = ? AND entity_name = 'accounts'", e.A.ID)
	e.exec("UPDATE sys_workflows SET last_run_at = '2026-10-01 10:00:00.000' WHERE tenant_id = ? AND name = 'wf-a'", e.A.ID)
	e.exec("UPDATE sys_workflows SET actions = JSON_SET(actions, '$[0].secret', 'K2_ROTATED') WHERE tenant_id = ? AND name = 'wf-a'", e.A.ID)
	e.exec("DELETE FROM sys_workflows WHERE tenant_id = ? AND name = 'wf-only-in-backup'", e.A.ID)
	e.exec("INSERT INTO sys_idempotency_keys (id, tenant_id, principal_id, idem_key, entity_name, request_hash, status, expires_at) VALUES (?, ?, 'p', 'k', 'accounts', ?, 'completed', NOW() + INTERVAL 1 DAY)",
		uuid.NewString(), e.A.ID, strings.Repeat("a", 64))
	later := e.putFile(e.A, "added-after", false, "")
	e.exec("DELETE FROM sys_files WHERE id = ?", gone)
	if err := e.blobs.Delete(context.Background(), e.A.ID+"/"+gone); err != nil {
		t.Fatal(err)
	}

	sumsB := e.checksums(e.B)
	st, row, err := e.restoreAndWait(e.A, s.ID)
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if *row.RestoreStatus != restoreDone {
		t.Fatalf("restore = %s code=%v detail=%v", *row.RestoreStatus, row.ErrorCode, row.ErrorDetail)
	}

	// (a) every rewritten table of A is back to the backup
	// webhook keys differ BY DESIGN (D-21: the file never held them), so those two tables are compared elsewhere
	delete(atBackup, "sys_workflows")
	delete(atBackup, "sys_pack_items")
	sameSums(t, "A after restore", atBackup, e.restoreSums(e.A))
	// (b) B did not move
	sameSums(t, "B", sumsB, e.checksums(e.B))
	// (c) identity kept
	if h := e.val("SELECT password_hash FROM users WHERE id = ?", e.uA); h != "CHANGED_HASH" {
		t.Errorf("password hash was rewound: %s", h)
	}
	if st := e.val("SELECT status FROM users WHERE id = ?", u2); st != "inactive" {
		t.Errorf("user status rewound: %s", st)
	}
	// (d) a role present in the backup is re-attached; a role born after it is lost, counted
	if got := roleOf(); got != roleBefore {
		t.Errorf("uA role = %s, want %s (re-attached)", got, roleBefore)
	}
	if got := e.val("SELECT role_id FROM users WHERE id = ?", u2); got != "<null>" {
		t.Errorf("u2 role = %s, want NULL", got)
	}
	var rep RestoreReport
	if err := json.Unmarshal(row.RestoreReport, &rep); err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.RolesLost != 1 {
		t.Errorf("rolesLost = %d, want 1", rep.RolesLost)
	}
	// (e) counters never go backwards
	if v := e.val("SELECT CAST(next_val AS CHAR) FROM sys_sequences WHERE tenant_id = ? AND entity_name = 'accounts'", e.A.ID); v != "100" {
		t.Errorf("next_val = %s, want 100 (GREATEST)", v)
	}
	// (f) last_run_at keeps the current value
	if v := e.val("SELECT DATE_FORMAT(last_run_at, '%Y-%m-%d %H') FROM sys_workflows WHERE tenant_id = ? AND name = 'wf-a'", e.A.ID); v != "2026-10-01 10" {
		t.Errorf("last_run_at = %s", v)
	}
	// (q) webhook key: current key re-attached, the rule that only the backup knew gets none
	if v := e.val("SELECT JSON_UNQUOTE(JSON_EXTRACT(actions, '$[0].secret')) FROM sys_workflows WHERE tenant_id = ? AND name = 'wf-a'", e.A.ID); v != "K2_ROTATED" {
		t.Errorf("wf-a secret = %s, want K2_ROTATED", v)
	}
	if v := e.val("SELECT COALESCE(JSON_EXTRACT(actions, '$[0].secret'), 'none') FROM sys_workflows WHERE tenant_id = ? AND name = 'wf-only-in-backup'", e.A.ID); v != "none" {
		t.Errorf("rule only in backup got a secret back: %s", v)
	}
	if rep.WebhookKeysDropped < 1 {
		t.Errorf("webhookKeysDropped = %d", rep.WebhookKeysDropped)
	}
	// (g) idempotency cache emptied
	if v := e.val("SELECT CAST(COUNT(*) AS CHAR) FROM sys_idempotency_keys WHERE tenant_id = ?", e.A.ID); v != "0" {
		t.Errorf("idempotency keys left: %s", v)
	}
	// (h) attachments: the one added later is gone, the one deleted for good is back and readable
	if rc, err := e.blobs.Open(context.Background(), e.A.ID+"/"+later); err == nil {
		_ = rc.Close()
		t.Errorf("blob added after the backup is still on disk")
	}
	if rc, err := e.blobs.Open(context.Background(), e.A.ID+"/"+gone); err != nil {
		t.Errorf("blob deleted for good was not restored: %v", err)
	} else {
		_ = rc.Close()
	}
	if rc, err := e.blobs.Open(context.Background(), e.A.ID+"/"+keep); err != nil {
		t.Errorf("kept blob lost: %v", err)
	} else {
		_ = rc.Close()
	}
	// (n)/(s) lock lifecycle + Doctor + step trail
	if fz.sets != 1 || fz.clrs != 1 || doc.called != 1 {
		t.Errorf("freeze sets=%d clears=%d doctor=%d", fz.sets, fz.clrs, doc.called)
	}
	if row.RestoreStep == nil || *row.RestoreStep != restoreStepDoctor {
		t.Errorf("last step = %v", row.RestoreStep)
	}
	if row.RestoreCommittedAt == nil {
		t.Errorf("no commit marker")
	}
	// (u) audit: start + done, in both trails
	var phases []string
	e.aud.mu.Lock()
	events := append([]audit.Event(nil), e.aud.events...)
	e.aud.mu.Unlock()
	for _, ev := range events {
		if ev.Action == "restore" && ev.TenantID == e.A.ID {
			phases = append(phases, ev.Diff.After["phase"].(string))
			if ev.Diff.After["by"] == "" {
				t.Errorf("customer-side restore row has no by label")
			}
		}
	}
	if strings.Join(phases, ",") != "start,done" {
		t.Errorf("customer trail phases = %v", phases)
	}

	// (k) undo: restoring the pre_restore row returns to the state right before the first restore
	_, undo, err := e.restoreAndWait(e.A, st.RestoreID)
	if err != nil || *undo.RestoreStatus != restoreDone {
		t.Fatalf("undo: %v %+v", err, undo)
	}
	if v := e.val("SELECT JSON_UNQUOTE(JSON_EXTRACT(data, '$.note')) FROM `"+e.objTable+"` WHERE id = ?", e.objA[1]); v != "changed" {
		t.Errorf("undo did not bring the drifted row back: %s", v)
	}
	sameSums(t, "B after undo", sumsB, e.checksums(e.B))
}

func TestRestore_Refusals_Integration(t *testing.T) {
	e := newEnv(t, nil)
	e.restoreEnv(ApplyHooks{})
	b := e.create(e.A)
	cases := []struct {
		name string
		in   RestoreInput
		code string
	}{
		{"wrong slug", RestoreInput{TenantID: e.A.ID, BackupID: b.ID, ConfirmSlug: "nope", Actor: e.actor()}, "TENANT_RESTORE_CONFIRM_MISMATCH"},
		{"own workspace", RestoreInput{TenantID: e.W.ID, BackupID: b.ID, ConfirmSlug: e.W.Slug, Actor: Actor{TenantID: e.W.ID, UserID: e.opUser}}, "TENANT_RESTORE_SELF"},
	}
	before := e.restoreSums(e.A)
	for _, tc := range cases {
		_, err := e.svc.StartRestore(context.Background(), tc.in)
		asErr(t, err, tc.code)
	}
	sameSums(t, "A untouched by refusals", before, e.restoreSums(e.A))
	if n := e.val("SELECT CAST(COUNT(*) AS CHAR) FROM sys_tenant_backups WHERE tenant_id = ? AND kind = 'pre_restore'", e.A.ID); n != "0" {
		t.Errorf("a refusal left %s pre_restore rows", n)
	}
}

func TestRestore_FaultInjection_RollsBack_Integration(t *testing.T) {
	e := newEnv(t, nil)
	boom := errors.New("injected")
	fz, _ := e.restoreEnv(ApplyHooks{AfterBatch: func(string, int) error {
		return boom
	}})
	b := e.create(e.A)
	e.exec("DELETE FROM `"+e.objTable+"` WHERE id = ?", e.objA[0])
	beforeA, beforeB := e.restoreSums(e.A), e.checksums(e.B)

	_, row, err := e.restoreAndWait(e.A, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *row.RestoreStatus != restoreFailed {
		t.Fatalf("restore = %s", *row.RestoreStatus)
	}
	sameSums(t, "A rolled back", beforeA, e.restoreSums(e.A))
	sameSums(t, "B", beforeB, e.checksums(e.B))
	if fz.sets != 1 || fz.clrs != 1 {
		t.Errorf("freeze not handed back: sets=%d clears=%d", fz.sets, fz.clrs)
	}
}

func TestRestore_CrashAfterCommit_ReaperFinishes_Integration(t *testing.T) {
	e := newEnv(t, nil)
	e.restoreEnv(ApplyHooks{AfterCommit: func() error { return errors.New("process died") }})
	loser := e.putFile(e.A, "x", false, "")
	b := e.create(e.A)
	later := e.putFile(e.A, "y", false, "")
	e.exec("DELETE FROM `"+e.objTable+"` WHERE id = ?", e.objA[0])

	st, err := e.svc.StartRestore(context.Background(), RestoreInput{TenantID: e.A.ID, BackupID: b.ID, ConfirmSlug: e.A.Slug, Actor: e.actor()})
	if err != nil {
		t.Fatal(err)
	}
	var row *Backup
	for i := 0; i < 400; i++ {
		row, _ = e.store.Get(context.Background(), e.A.ID, st.RestoreID)
		if row != nil && row.RestoreCommittedAt != nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if row == nil || row.RestoreCommittedAt == nil || *row.RestoreStatus != restoreRunning {
		t.Fatalf("expected a committed, still-running restore, got %+v", row)
	}
	_ = loser
	// the process "died": 31 minutes without a heartbeat
	e.exec("UPDATE sys_tenant_backups SET updated_at = NOW(3) - INTERVAL 31 MINUTE WHERE id = ?", st.RestoreID)
	if n, err := e.svc.ReapStale(context.Background()); err != nil || n < 1 {
		t.Fatalf("ReapStale = %d, %v", n, err)
	}
	row, _ = e.store.Get(context.Background(), e.A.ID, st.RestoreID)
	if *row.RestoreStatus != restoreDone {
		t.Fatalf("after reap restore = %s (want done, never failed)", *row.RestoreStatus)
	}
	var rep RestoreReport
	if err := json.Unmarshal(row.RestoreReport, &rep); err != nil || !rep.ResumedAfterCrash {
		t.Errorf("report = %+v err=%v", rep, err)
	}
	if rc, err := e.blobs.Open(context.Background(), e.A.ID+"/"+later); err == nil {
		_ = rc.Close()
		t.Errorf("orphan blob was not removed by the resumed run")
	}
	// the DB-level UNIQUE released: a new backup can start
	if v := e.val("SELECT CAST(COUNT(*) AS CHAR) FROM sys_tenant_backups WHERE tenant_id = ? AND op_lock IS NOT NULL", e.A.ID); v != "0" {
		t.Errorf("op_lock still held: %s", v)
	}
}

func TestRestore_DroppedObjTableIsRebuilt_Integration(t *testing.T) {
	e := newEnv(t, nil)
	e.restoreEnv(ApplyHooks{})
	// A needs its own entity + table so dropping it does not hit B
	sfx := randSuffix(t)
	name := "tbk_it_" + sfx + "x"
	e.exec("INSERT INTO sys_entities (id, tenant_id, name, label, table_name) VALUES (?, ?, ?, 'X', ?)", uuid.NewString(), e.A.ID, name, "obj_"+name)
	create := "CREATE TABLE `obj_" + name + "` LIKE `" + e.objTable + "`"
	e.exec(create)
	t.Cleanup(func() { _, _ = e.db.Exec("DROP TABLE IF EXISTS `obj_" + name + "`") })
	rowID := uuid.NewString()
	e.exec("INSERT INTO `obj_"+name+"` (id, tenant_id, created_by, data) VALUES (?, ?, ?, '{\"k\":1}')", rowID, e.A.ID, e.opUser)
	b := e.create(e.A)
	e.exec("DROP TABLE `obj_" + name + "`")

	_, row, err := e.restoreAndWait(e.A, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *row.RestoreStatus != restoreDone {
		t.Fatalf("restore = %s code=%v", *row.RestoreStatus, row.ErrorCode)
	}
	if v := e.val("SELECT id FROM `obj_"+name+"` WHERE tenant_id = ?", e.A.ID); v != rowID {
		t.Errorf("row not back in the rebuilt table: %s", v)
	}
}

// specs/065 FR-026 / 068 T071(aa): a backup taken BEFORE the conversion, restored AFTER it, must not
// lock the owners out — the reserved role is recreated and they are re-attached, none counted lost.
func TestRestore_OwnerRoleRecreated_Integration(t *testing.T) {
	e := newEnv(t, nil)
	e.restoreEnv(ApplyHooks{})
	s := e.create(e.A)
	if s.Status != StatusReady {
		t.Fatalf("backup = %s %v", s.Status, s.ErrorCode)
	}
	// convert after the backup: owner role + grants, uA becomes a customer owner
	ownerRole := uuid.NewString()
	e.exec("INSERT INTO acl_roles (id, tenant_id, name, label) VALUES (?, ?, 'tenant_owner', 'Chủ khách')", ownerRole, e.A.ID)
	e.exec("INSERT INTO acl_role_actions (id, role_id, entity_name, action, access_level) VALUES (?, ?, '*', 'view', 'all')", uuid.NewString(), ownerRole)
	e.exec("UPDATE users SET is_admin = 0, can_manage_users = 1, role_id = ? WHERE id = ?", ownerRole, e.uA)

	_, row, err := e.restoreAndWait(e.A, s.ID)
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if *row.RestoreStatus != restoreDone {
		t.Fatalf("restore = %s code=%v detail=%v", *row.RestoreStatus, row.ErrorCode, row.ErrorDetail)
	}
	if n := e.val("SELECT COUNT(*) FROM users u JOIN acl_roles r ON r.id = u.role_id WHERE u.id = ? AND r.name = 'tenant_owner' AND r.tenant_id = ?", e.uA, e.A.ID); n != "1" {
		t.Errorf("owner not re-attached to a recreated tenant_owner role (count=%s)", n)
	}
	if n := e.val("SELECT COUNT(*) FROM acl_role_actions a JOIN acl_roles r ON r.id = a.role_id WHERE r.tenant_id = ? AND r.name = 'tenant_owner' AND a.entity_name = '*'", e.A.ID); n != "4" {
		t.Errorf("owner role grants = %s, want 4", n)
	}
	var rep RestoreReport
	if err := json.Unmarshal(row.RestoreReport, &rep); err != nil {
		t.Fatalf("report: %v", err)
	}
	if rep.RolesLost != 0 {
		t.Errorf("rolesLost = %d, want 0", rep.RolesLost)
	}
}
