//go:build integration

// Real-MySQL proof of the pack-upgrade run lock (sys_pack_upgrades, migration 000094): the CHECK
// and UNIQUE keys that make "one running upgrade per (tenant, pack)" a database fact, the lease,
// reject/abort, the append-only results, the derived "interrupted" status, and tenant isolation.
//
//	TEST_DATABASE_URL=... bash scripts/run-integration.sh ./internal/packinstall/
package packinstall

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"nexus/platform/internal/services/vertical_pack"
)

const (
	tA = "11111111-1111-1111-1111-111111111111"
	tB = "22222222-2222-2222-2222-222222222222"
)

func newRun(id, tenant, slug string) vertical_pack.UpgradeRun {
	return vertical_pack.UpgradeRun{ID: id, TenantID: tenant, Slug: slug, ToVersion: "1.1.0", Source: "builtin", Mode: "live",
		PlanHash: strings.Repeat("a", 64), CreatedBy: "u1"}
}

func TestUpgradeRuns_SchemaInvariants(t *testing.T) {
	gdb := piSetup(t)
	ins := func(id, status string, lock any) error {
		return gdb.Exec("INSERT INTO sys_pack_upgrades (id, tenant_id, pack_slug, to_version, source, plan_hash, status, running_lock, decisions, created_by, started_at) "+
			"VALUES (?, ?, 'p', '1', 'builtin', 'h', ?, ?, '[]', 'u', NOW(3))", id, tA, status, lock).Error
	}
	if err := ins("s1", "done", 1); err == nil {
		t.Error("CHECK: a finished run holding the lock must be rejected")
	}
	if err := ins("s2", "running", nil); err == nil {
		t.Error("CHECK: a running run WITHOUT the lock must be rejected (the NULL case `= 1` would let through)")
	}
	if err := ins("s3", "done", nil); err != nil {
		t.Errorf("finished run: %v", err)
	}
	if err := ins("s4", "done", nil); err != nil {
		t.Errorf("a second finished run of the same pack must be allowed: %v", err)
	}
	if err := ins("s5", "running", 1); err != nil {
		t.Errorf("first running run: %v", err)
	}
	if err := ins("s6", "running", 1); err == nil {
		t.Error("UNIQUE: two running runs of one pack must be rejected")
	}
}

func TestUpgradeRuns_ClaimLeaseAbortAndIsolation(t *testing.T) {
	gdb := piSetup(t)
	r := New(gdb)
	ctx := context.Background()

	if err := r.ClaimRun(ctx, newRun("r1", tA, "p")); err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	var busy *vertical_pack.ErrRunInProgress
	if err := r.ClaimRun(ctx, newRun("r2", tA, "p")); !errors.As(err, &busy) || busy.RunID != "r1" {
		t.Fatalf("claim 2 = %v; want ErrRunInProgress{r1}", err)
	}
	// another pack and another tenant are independent locks
	if err := r.ClaimRun(ctx, newRun("r3", tA, "other")); err != nil {
		t.Fatalf("other slug: %v", err)
	}
	if err := r.ClaimRun(ctx, newRun("r4", tB, "p")); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	if got, _ := r.RunningRun(ctx, tA, "p"); got == nil || got.RunID != "r1" {
		t.Fatalf("RunningRun = %v; want r1", got)
	}

	// heartbeat: a result bumps updated_at; results append in order
	for i := 0; i < 2; i++ {
		if err := r.AppendResult(ctx, tA, "r1", vertical_pack.UpgradeResult{ItemType: "workflow", ItemKey: fmt.Sprintf("k%d", i), Outcome: "applied"}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// lease: silent for 6 minutes ⇒ history shows "interrupted" at once (GET writes nothing), Install is not
	// blocked, and the next claim retires the dead row.
	if err := gdb.Exec("UPDATE sys_pack_upgrades SET updated_at = NOW(3) - INTERVAL 6 MINUTE WHERE id = 'r1'").Error; err != nil {
		t.Fatal(err)
	}
	runs, err := r.ListRuns(ctx, tA, "p", 10)
	if err != nil || len(runs) != 1 || runs[0].Status != "interrupted" || len(runs[0].Results) != 2 || runs[0].Results[1].ItemKey != "k1" {
		t.Fatalf("ListRuns = %+v err=%v; want one interrupted run with 2 results in order", runs, err)
	}
	var raw string
	gdb.Raw("SELECT status FROM sys_pack_upgrades WHERE id = 'r1'").Row().Scan(&raw)
	if raw != "running" {
		t.Errorf("GET must not write: stored status = %q, want running", raw)
	}
	if got, _ := r.RunningRun(ctx, tA, "p"); got != nil {
		t.Errorf("a run past its lease must not block Install, got %+v", got)
	}
	if err := r.ClaimRun(ctx, newRun("r5", tA, "p")); err != nil {
		t.Fatalf("claim after lease: %v", err)
	}
	gdb.Raw("SELECT status FROM sys_pack_upgrades WHERE id = 'r1'").Row().Scan(&raw)
	if raw != "interrupted" {
		t.Errorf("claim must retire the dead run: status = %q", raw)
	}

	// abort: lock released at once, row hidden from history
	if err := r.AbortRun(ctx, tA, "r5"); err != nil {
		t.Fatal(err)
	}
	if runs, _ = r.ListRuns(ctx, tA, "p", 10); len(runs) != 1 {
		t.Errorf("rejected run must be hidden, history = %d rows", len(runs))
	}
	if err := r.ClaimRun(ctx, newRun("r6", tA, "p")); err != nil {
		t.Fatalf("claim right after abort: %v", err)
	}
	// finish: lock released; finishing twice is an error (the lock is gone)
	if err := r.FinishRun(ctx, tA, "r6", "done", map[string]int{"applied": 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.FinishRun(ctx, tA, "r6", "done", nil); err == nil {
		t.Error("second FinishRun must fail")
	}
	// tenant B's run is untouched by tenant A's whole story
	if runs, _ = r.ListRuns(ctx, tB, "p", 10); len(runs) != 1 || runs[0].Status != "running" {
		t.Errorf("tenant B history = %+v; want its one running run", runs)
	}
	// wrong tenant cannot finish/abort
	if err := r.FinishRun(ctx, tA, "r4", "done", nil); err == nil {
		t.Error("tenant A must not finish tenant B's run")
	}
}

func TestUpgradeRuns_TwentyClaimsOneWinner(t *testing.T) {
	gdb := piSetup(t)
	r := New(gdb)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, busyN := 0, 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := r.ClaimRun(context.Background(), newRun(fmt.Sprintf("c%02d", i), tA, "p"))
			var busy *vertical_pack.ErrRunInProgress
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.As(err, &busy):
				busyN++
			default:
				t.Errorf("claim %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 || busyN != 19 {
		t.Fatalf("wins=%d busy=%d; want exactly 1 and 19", wins, busyN)
	}
}

func TestUpgradeRuns_DeleteItemsIsPinnedToTenantAndSlug(t *testing.T) {
	gdb := piSetup(t)
	r := New(gdb)
	ctx := context.Background()
	for _, ten := range []string{tA, tB} {
		if err := r.SaveItems(ctx, ten, "p", "1.0.0", []vertical_pack.PackItem{{Type: "workflow", Key: "k", Content: []byte(`{}`), Wrote: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.DeleteItems(ctx, tA, "p", [][2]string{{"workflow", "k"}}); err != nil {
		t.Fatal(err)
	}
	if a, _ := r.ListItems(ctx, tA, "p"); len(a) != 0 {
		t.Errorf("tenant A row survived: %v", a)
	}
	if b, _ := r.ListItems(ctx, tB, "p"); len(b) != 1 {
		t.Errorf("tenant B row was deleted: %v", b)
	}
}

func TestUpgradeMigration_DownThenUpAgain(t *testing.T) {
	gdb := piSetup(t)
	var db string
	if err := gdb.Raw("SELECT DATABASE()").Row().Scan(&db); err != nil {
		t.Fatal(err)
	}
	bin := piMigrateBin(t)
	run := func(args ...string) {
		t.Helper()
		out, err := exec.Command(bin, append([]string{"-path", piMigrationsDir(), "-database", piMigrateDSN(t, db)}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("migrate %v: %v\n%s", args, err, out)
		}
	}
	has := func() bool {
		var n int
		gdb.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'sys_pack_upgrades'").Row().Scan(&n)
		return n == 1
	}
	if !has() {
		t.Fatal("table missing after up")
	}
	// ponytail: the newest migration may not be ours on a merged tree; pull this one's down by version.
	run("goto", "93")
	if has() {
		t.Fatal("down must drop sys_pack_upgrades")
	}
	run("up")
	if !has() {
		t.Fatal("up again must recreate it")
	}
}
