//go:build integration

package idempotency

import (
	"fmt"
	"os"
	"testing"

	"nexus/platform/internal/ittest"
)

// testDSN points the integration tests at the throwaway migrated database TestMain provisions from
// TEST_DATABASE_URL (one source — B7). The default only matters when MySQL is unreachable: connect
// fails and the tests Skip.
var testDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=True&loc=Local"

func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = testDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_idem_it", "../../migrations")
	if err != nil {
		// MySQL reachable but provisioning failed — a real break, never a silent skip.
		fmt.Fprintf(os.Stderr, "idempotency integration setup failed: %v\n", err)
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
