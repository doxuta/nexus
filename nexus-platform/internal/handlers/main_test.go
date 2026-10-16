//go:build integration

package handlers

import (
	"fmt"
	"os"
	"testing"

	"nexus/platform/internal/ittest"
)

// testDSN points the shared handlers integration suites (those that connect via dsn()) at a
// self-provisioned, migrated throwaway database set by TestMain below. Its initial value is a
// valid-shaped default so that, when MySQL is unreachable, the connect helpers fail on Connect
// and Skip (and splitDSN/baseDSNNoDB still parse a well-formed shape) rather than hard-failing.
var testDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local"

// TestMain self-provisions ONE throwaway migrated database for the whole handlers integration
// suite (harness bug #84: the dsn()-based suites previously connected to the raw
// TEST_DATABASE_URL, which is db-less in the gate recipe → "Error 1046 (3D000): No database
// selected"). It mirrors how internal/services self-provisions via o3aSetup, using the shared
// internal/ittest helper. The suites that already create their OWN throwaway DB (a4, rm25,
// mig000012, rm24, layout) are unaffected: they derive a db-less server DSN from dsn() and
// create/drop their own databases on the same server.
func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = testDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_handlers_it", "../../migrations")
	if err != nil {
		// MySQL reachable but provisioning failed — a real break, never a silent skip.
		fmt.Fprintf(os.Stderr, "handlers integration setup failed: %v\n", err)
		os.Exit(1)
	}
	// Skip (MySQL unreachable at the DSN asked for) must not leave the package default (shared dev
	// nexus_platform on :3306) in place: a wrong TEST_DATABASE_URL would then run and WRITE there.
	testDSN = input
	if !skip {
		testDSN = thr
	}
	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}
