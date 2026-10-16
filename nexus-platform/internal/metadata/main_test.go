//go:build integration

package metadata

import (
	"fmt"
	"os"
	"testing"

	"nexus/platform/internal/ittest"
)

// testDSN points the repinDSN-based suites (repin, inverse-registry, rm21-sweep) at a
// self-provisioned, migrated throwaway database set by TestMain below. Its initial value is a
// valid-shaped default so that, when MySQL is unreachable, the connect helpers fail on Connect
// and Skip rather than hard-failing. rm18's suite provisions its own throwaway DB independently
// and does not read this var.
var testDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local"

// TestMain self-provisions ONE throwaway migrated database for the repinDSN-based suites in this
// package (harness bug #84: they previously connected to the raw TEST_DATABASE_URL, which is
// db-less in the gate recipe → "Error 1046 (3D000): No database selected"). It mirrors how
// internal/services self-provisions via o3aSetup, using the shared internal/ittest helper.
func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = testDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_metadata_it", "../../migrations")
	if err != nil {
		// MySQL reachable but provisioning failed — a real break, never a silent skip.
		fmt.Fprintf(os.Stderr, "metadata integration setup failed: %v\n", err)
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
