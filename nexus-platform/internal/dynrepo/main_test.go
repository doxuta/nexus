//go:build integration

package dynrepo

import (
	"fmt"
	"os"
	"testing"

	"nexus/platform/internal/ittest"
)

// testDSN points every dynrepo integration test at a self-provisioned, migrated throwaway
// database set by TestMain below. Its initial value is a valid-shaped default so that, when
// MySQL is unreachable, the connect helper fails on Connect and Skips rather than hard-failing.
var testDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=true&multiStatements=true"

// TestMain self-provisions ONE throwaway migrated database for the whole dynrepo integration
// suite (harness bug #84: these tests previously connected to the raw TEST_DATABASE_URL, which
// is db-less in the gate recipe → "Error 1046 (3D000): No database selected"). It mirrors how
// internal/services self-provisions via o3aSetup, using the shared internal/ittest helper.
func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = testDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_dynrepo_it", "../../migrations")
	if err != nil {
		// MySQL reachable but provisioning failed — a real break, never a silent skip.
		fmt.Fprintf(os.Stderr, "dynrepo integration setup failed: %v\n", err)
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
