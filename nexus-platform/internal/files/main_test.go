//go:build integration

package files_test

import (
	"fmt"
	"os"
	"testing"

	"nexus/platform/internal/ittest"
)

// packageTestDSN points every files integration test at a self-provisioned, migrated throwaway
// database set by TestMain below. Its initial value is a valid-shaped default so that, when MySQL
// is unreachable, the connect helper fails on Connect and Skips rather than hard-failing.
var packageTestDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=true&multiStatements=true"

// TestMain self-provisions ONE throwaway migrated database for the files integration suite (same
// shape as internal/calmeta), so the sys_files queries run against the schema golang-migrate builds.
func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = packageTestDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_files_it", "../../migrations")
	if err != nil {
		// MySQL reachable but provisioning failed — a real break, never a silent skip.
		fmt.Fprintf(os.Stderr, "files integration setup failed: %v\n", err)
		os.Exit(1)
	}
	// Skip (MySQL unreachable at the DSN asked for) must not leave the package default (shared dev
	// nexus_platform on :3306) in place: a wrong TEST_DATABASE_URL would then run and WRITE there.
	packageTestDSN = input
	if !skip {
		packageTestDSN = thr
	}
	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}
