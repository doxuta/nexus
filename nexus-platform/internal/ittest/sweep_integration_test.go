//go:build integration

package ittest

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// probeName matches the throwaway DBs only this test creates; group 1 is the owning test process.
var probeName = regexp.MustCompile(`^nexus_swp([0-9]+)_it_[0-9]+$`)

// dropDeadProbes drops probe DBs left by earlier runs of this test that were killed before their
// t.Cleanup ran (queue #93). The sweep under test deliberately never touches an EMPTY schema, so a
// killed run leaked its empty probe forever. Only probes whose owner process is gone are dropped:
// a copy of this test running right now in a parallel worktree keeps its probes.
func dropDeadProbes(t *testing.T, admin *sqlx.DB) []string {
	t.Helper()
	var names []string
	if err := admin.Select(&names, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'nexus\\_swp%'"); err != nil {
		t.Fatalf("list probes: %v", err)
	}
	var dropped []string
	for _, n := range names {
		m := probeName.FindStringSubmatch(n)
		if m == nil {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid == os.Getpid() {
			continue
		}
		// Signal 0 probes existence without touching the process; ESRCH = no such process.
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			continue
		}
		if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + n + "`"); err != nil {
			t.Errorf("drop dead probe %s: %v", n, err)
			continue
		}
		dropped = append(dropped, n)
	}
	return dropped
}

// TestSetupSweepsStaleThrowawayDBs_Integration proves queue #71: a throwaway DB whose run was
// killed before cleanup() is dropped by the NEXT Setup, while an empty DB (a sibling Setup
// between CREATE and its first migration) and a young DB (a sibling package still running) stay.
//
// The sweep is narrowed to this test's own probe names. It must never run the real pattern with
// a zero age: sibling packages and parallel worktrees share this MySQL and their live DBs match.
func TestSetupSweepsStaleThrowawayDBs_Integration(t *testing.T) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = "root:@tcp(127.0.0.1:3306)/?charset=utf8mb4&parseTime=True&loc=Local"
	}
	admin, err := sqlx.Connect("mysql", serverDSN(input))
	if err != nil {
		t.Skipf("ittest: MySQL not reachable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = admin.Close() }) // registered first ⇒ runs last, after the probe drops

	// A killed earlier run's EMPTY probe is never swept (no tables ⇒ no age); this test owns the
	// name space, so it collects its own dead leftovers first. Proven with a probe owned by a pid
	// that cannot be alive (above the kernel's pid ceiling on Linux and macOS).
	const deadPID = 99999999
	deadProbe := fmt.Sprintf("nexus_swp%d_it_2", deadPID)
	if _, e := admin.Exec("CREATE DATABASE IF NOT EXISTS `" + deadProbe + "`"); e != nil {
		t.Fatalf("create dead probe: %v", e)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE IF EXISTS `" + deadProbe + "`") })
	if dropped := dropDeadProbes(t, admin); !contains(dropped, deadProbe) {
		t.Errorf("empty probe %s of a dead run was not collected (dropped=%v) — killed runs leak forever", deadProbe, dropped)
	}

	pid := os.Getpid()
	probe := func(n int) string { return fmt.Sprintf("nexus_swp%d_it_%d", pid, n) }
	leaked, empty, young := probe(1), probe(2), probe(3)
	t.Cleanup(func() {
		for _, n := range []string{leaked, empty, young} {
			if _, e := admin.Exec("DROP DATABASE IF EXISTS `" + n + "`"); e != nil {
				t.Errorf("cleanup drop %s: %v", n, e)
			}
		}
	})
	create := func(name string, withTable bool) {
		t.Helper()
		if _, e := admin.Exec("CREATE DATABASE `" + name + "`"); e != nil {
			t.Fatalf("create %s: %v", name, e)
		}
		if withTable {
			if _, e := admin.Exec("CREATE TABLE `" + name + "`.t (id INT PRIMARY KEY)"); e != nil {
				t.Fatalf("create table in %s: %v", name, e)
			}
		}
	}
	exists := func(name string) bool {
		t.Helper()
		var n int
		if e := admin.Get(&n, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", name); e != nil {
			t.Fatalf("probe %s: %v", name, e)
		}
		return n == 1
	}

	create(leaked, true)
	create(empty, false)

	origName, origAge := staleDBName, staleAfter
	staleDBName = regexp.MustCompile(fmt.Sprintf(`^nexus_swp%d_it_[0-9]+$`, pid))
	staleAfter = 0
	t.Cleanup(func() { staleDBName, staleAfter = origName, origAge })

	_, cleanup, skip, err := Setup(input, "nexus_ittest_it", "../../migrations")
	if skip {
		t.Skip("ittest: MySQL not reachable — skipping")
	}
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	cleanup()

	if exists(leaked) {
		t.Errorf("%s (tables, older than staleAfter) survived Setup — the leak is never collected", leaked)
	}
	if !exists(empty) {
		t.Errorf("%s (no tables = age unknown) was dropped — a sibling Setup mid-CREATE would lose its DB", empty)
	}

	create(young, true)
	dropped, err := sweepStale(admin, staleDBName, time.Hour)
	if err != nil {
		t.Fatalf("sweepStale: %v", err)
	}
	if len(dropped) != 0 || !exists(young) {
		t.Errorf("young DB swept: dropped=%v exists(%s)=%v — a live sibling package would lose its DB", dropped, young, exists(young))
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
