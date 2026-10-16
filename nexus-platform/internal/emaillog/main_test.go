//go:build integration

package emaillog

import (
	"fmt"
	"os"
	"testing"

	"nexus/platform/internal/ittest"
)

// testDSN points the integration tests at the throwaway migrated database TestMain provisions from
// TEST_DATABASE_URL (one source — B7).
var testDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local"

func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = testDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_elog_it", "../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "emaillog integration setup failed: %v\n", err)
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
