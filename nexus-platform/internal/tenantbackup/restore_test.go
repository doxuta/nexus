package tenantbackup

// specs/068-tenant-backup-restore T070 — the orchestration of a restore with a fake RestoreStore:
// order of the steps, what a failed self backup leaves, the typed confirmation, the webhook-key
// re-attach rule. The SQL itself is proven in restore_integration_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRestoreStore struct {
	mu        sync.Mutex
	calls     []string // "step:<name>" | "apply" | "ddl" | "finish:<status>:<code>"
	applyErr  error
	applyRes  *ApplyResult
	finished  chan struct{}
	finishErr error
}

func newFakeRestoreStore() *fakeRestoreStore {
	return &fakeRestoreStore{finished: make(chan struct{}, 1), applyRes: &ApplyResult{Committed: true, Deleted: map[string]int64{}, Inserted: map[string]int64{}}}
}

func (f *fakeRestoreStore) rec(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}
func (f *fakeRestoreStore) get() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}
func (f *fakeRestoreStore) SetRestoreStep(_ context.Context, _, _, step string) error {
	f.rec("step:" + step)
	return nil
}
func (f *fakeRestoreStore) RestoreHeartbeat(context.Context, string, string) (int64, error) {
	return 1, nil
}
func (f *fakeRestoreStore) FinishRestore(_ context.Context, _, _, status string, _ json.RawMessage, code, _ string) (int64, error) {
	f.rec("finish:" + status + ":" + code)
	f.finished <- struct{}{}
	return 1, f.finishErr
}
func (f *fakeRestoreStore) ListStaleRestores(context.Context, time.Duration) ([]StaleRestore, error) {
	return nil, nil
}
func (f *fakeRestoreStore) ExecDDL(context.Context, string) error { f.rec("ddl"); return nil }
func (f *fakeRestoreStore) ApplyRestore(context.Context, ApplyInput) (*ApplyResult, error) {
	f.rec("apply")
	return f.applyRes, f.applyErr
}

type fakeRestoreFreezer struct {
	mu sync.Mutex
	fakeFreezer
	extends int
	setErr  error
}

func (f *fakeRestoreFreezer) Set(ctx context.Context, t string, u time.Time, r string) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return nil, f.setErr
	}
	return f.fakeFreezer.Set(ctx, t, u, r)
}
func (f *fakeRestoreFreezer) Clear(ctx context.Context, t string, p *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fakeFreezer.Clear(ctx, t, p)
}
func (f *fakeRestoreFreezer) Extend(context.Context, string, time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extends++
	return nil
}

// snap returns (tenant frozen, reason, times cleared).
func (f *fakeRestoreFreezer) snap() (string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setTenant, f.setReason, len(f.cleared)
}

type fakeRestoreDoctor struct {
	mu    sync.Mutex
	calls int
}

func (d *fakeRestoreDoctor) Repair(context.Context, string) (int, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return 2, 0, nil
}
func (d *fakeRestoreDoctor) FlushCaches(context.Context, string) {}

type restoreFx struct {
	*planFx
	rs  *fakeRestoreStore
	frz *fakeRestoreFreezer
	doc *fakeRestoreDoctor
}

func newRestoreFx(t *testing.T) *restoreFx {
	t.Helper()
	p := newPlanFx(t, zipSpec{lines: map[string][]string{"acl_roles": rowsOf(`{"id":"` + roleKept + `"}`)}})
	rf := &restoreFx{planFx: p, rs: newFakeRestoreStore(), frz: &fakeRestoreFreezer{}, doc: &fakeRestoreDoctor{}}
	p.svc.WithRestore(RestoreDeps{Store: rf.rs, Freezer: rf.frz, Doctor: rf.doc, Settle: time.Millisecond})
	return rf
}

func (rf *restoreFx) start(confirm string) (*RestoreStarted, error) {
	return rf.svc.StartRestore(context.Background(), RestoreInput{TenantID: tenantA, BackupID: bkID, ConfirmSlug: confirm, Actor: Actor{TenantID: opTen, UserID: opUser}})
}

func (rf *restoreFx) wait(t *testing.T) {
	t.Helper()
	select {
	case <-rf.rs.finished:
	case <-time.After(5 * time.Second):
		t.Fatalf("restore did not finish; calls = %v", rf.rs.get())
	}
}

func TestStartRestore_ConfirmSlugMustMatch(t *testing.T) {
	for _, tc := range []struct{ name, slug string }{{"wrong", "qa-y"}, {"empty", ""}, {"case differs", "QA-X"}} {
		t.Run(tc.name, func(t *testing.T) {
			rf := newRestoreFx(t)
			_, err := rf.start(tc.slug)
			e := asErr(t, err, "TENANT_RESTORE_CONFIRM_MISMATCH")
			if e.Details["expected"] != "qa-x" {
				t.Errorf("details = %v", e.Details)
			}
			if rf.store.insertCalls != 0 || len(rf.rs.get()) != 0 || frozenTenant(rf) != "" {
				t.Errorf("a refused restore touched something: inserts=%d calls=%v", rf.store.insertCalls, rf.rs.get())
			}
		})
	}
}

func TestRestore_StepsRunInOrderAndUnlock(t *testing.T) {
	rf := newRestoreFx(t)
	st, err := rf.start("qa-x")
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	if st.Status != "running" || st.RestoreID == "" {
		t.Fatalf("started = %+v", st)
	}
	rf.wait(t)
	want := "step:prebackup,step:apply,apply,step:doctor,finish:done:"
	if got := strings.Join(rf.rs.get(), ","); got != want {
		t.Errorf("calls = %s\nwant    %s", got, want)
	}
	// the lock is handed back by a deferred call that may land just after FinishRestore
	deadline := time.Now().Add(2 * time.Second)
	for _, _, n := rf.frz.snap(); n == 0 && time.Now().Before(deadline); _, _, n = rf.frz.snap() {
		time.Sleep(5 * time.Millisecond)
	}
	tenant, reason, cleared := rf.frz.snap()
	if tenant != tenantA || reason != "restore" || cleared != 1 {
		t.Errorf("freeze tenant=%s reason=%s cleared=%d", tenant, reason, cleared)
	}
	rf.doc.mu.Lock()
	if rf.doc.calls != 1 {
		t.Errorf("doctor ran %d times", rf.doc.calls)
	}
	rf.doc.mu.Unlock()
	// audit: start then done, both trails
	rf.audit.mu.Lock()
	defer rf.audit.mu.Unlock()
	var phases []string
	for _, ev := range rf.audit.events {
		if ev.TenantID == tenantA {
			phases = append(phases, ev.Diff.After["phase"].(string))
		}
	}
	if strings.Join(phases, ",") != "start,done" {
		t.Errorf("customer-side phases = %v", phases)
	}
}

func TestRestore_FailedSelfBackupStopsBeforeAnyChange(t *testing.T) {
	rf := newRestoreFx(t)
	rf.snaps.openErr = errors.New("snapshot refused")
	if _, err := rf.start("qa-x"); err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	rf.wait(t)
	got := strings.Join(rf.rs.get(), ",")
	if strings.Contains(got, "apply") || strings.Contains(got, "ddl") {
		t.Errorf("a failed self backup went on to change data: %s", got)
	}
	if !strings.HasSuffix(got, "finish:failed:"+errPrebackupFailed) {
		t.Errorf("calls = %s", got)
	}
}

func TestRestore_ErrorAfterCommitIsNotAFailure(t *testing.T) {
	rf := newRestoreFx(t)
	rf.rs.applyErr = errors.New("connection lost after commit")
	if _, err := rf.start("qa-x"); err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, c := range rf.rs.get() {
		if strings.HasPrefix(c, "finish:") {
			t.Errorf("a committed restore was finished by the runner (%s): only the reaper may finish it", c)
		}
	}
}

func TestRestore_RollbackErrorFailsWithItsCode(t *testing.T) {
	rf := newRestoreFx(t)
	rf.rs.applyRes = nil
	rf.rs.applyErr = &Error{Code: "TENANT_RESTORE_TENANT_MISMATCH", Msg: "x"}
	if _, err := rf.start("qa-x"); err != nil {
		t.Fatal(err)
	}
	rf.wait(t)
	if got := strings.Join(rf.rs.get(), ","); !strings.HasSuffix(got, "finish:failed:TENANT_RESTORE_TENANT_MISMATCH") {
		t.Errorf("calls = %s", got)
	}
}

func TestReattachWebhookKeys(t *testing.T) {
	cur := []byte(`[{"type":"outbound_webhook","url":"https://p.example/a","secret":"K2","headers":{"Authorization":"B2"}},{"type":"email_alert","to":"x"}]`)
	tests := []struct {
		name, restored string
		wantSecret     string
		wantDropped    int
	}{
		{"same url gets the current key", `[{"type":"outbound_webhook","url":" https://p.example/a "}]`, "K2", 0},
		{"unknown url gets none and is counted", `[{"type":"outbound_webhook","url":"https://other.example"}]`, "", 1},
		{"no webhook, nothing to do", `[{"type":"email_alert","to":"x"}]`, "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, dropped, err := ReattachWebhookKeys(tc.restored, cur)
			if err != nil || dropped != tc.wantDropped {
				t.Fatalf("out=%s dropped=%d err=%v", out, dropped, err)
			}
			if has := strings.Contains(out, `"secret":"K2"`); has != (tc.wantSecret != "") {
				t.Errorf("secret present = %v in %s", has, out)
			}
		})
	}
	if _, _, err := ReattachWebhookKeys(`{not json`, cur); err == nil {
		t.Error("unreadable restored actions must be an error")
	}
}

func TestTableRefDeleteQuery_AlwaysTenantPinned(t *testing.T) {
	for _, ref := range []TableRef{
		{Table: "obj_accounts", Rule: Rule{Class: Restore}},
		{Table: "acl_role_actions", Rule: Rule{Class: Restore, Owner: &OwnerFK{Parent: "acl_roles", Column: "role_id"}}},
	} {
		q, err := ref.deleteQuery()
		if err != nil || !strings.Contains(q, "tenant_id = ?") || strings.Contains(strings.ToUpper(q), "TRUNCATE") {
			t.Errorf("%s: %q %v", ref.Table, q, err)
		}
	}
	if _, err := (TableRef{Table: "x; DROP TABLE y", Rule: Rule{Class: Restore}}).deleteQuery(); err == nil {
		t.Error("an unsafe identifier must be refused")
	}
}

func frozenTenant(rf *restoreFx) string { t, _, _ := rf.frz.snap(); return t }

// A failure before the self backup finishes must not leave the pre_restore row 'running': op_lock keys on it.
func TestRestore_FailureBeforeSelfBackupReleasesTheRow(t *testing.T) {
	rf := newRestoreFx(t)
	rf.frz.setErr = errors.New("redis down")
	st, err := rf.start("qa-x")
	if err != nil {
		t.Fatalf("StartRestore: %v", err)
	}
	rf.wait(t)
	fs := rf.planFx.store
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if b := fs.rows[st.RestoreID]; b == nil || b.Status == StatusRunning {
		t.Errorf("pre_restore row still running after a failed restore: %+v", b)
	}
}
