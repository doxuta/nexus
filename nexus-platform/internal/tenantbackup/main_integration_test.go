//go:build integration

package tenantbackup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"

	"nexus/platform/internal/ittest"
)

// packageTestDSN points every tenantbackup integration test at ONE self-provisioned, migrated
// throwaway database (TestMain below) — never at the shared nexus_platform: a backup test writes
// and (later) restores customer rows, and the dev DB holds other lanes' real data.
var packageTestDSN = "root:@tcp(127.0.0.1:3306)/nexus_platform?parseTime=true&multiStatements=true"

func TestMain(m *testing.M) {
	input := os.Getenv("TEST_DATABASE_URL")
	if input == "" {
		input = packageTestDSN
	}
	thr, cleanup, skip, err := ittest.Setup(input, "nexus_tbk_it", "../../migrations")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tenantbackup integration setup failed: %v\n", err)
		os.Exit(1)
	}
	// Skip (MySQL unreachable at the DSN asked for) must not leave the package default (shared dev
	// nexus_platform on :3306) in place: a wrong TEST_DATABASE_URL would then run and WRITE there.
	packageTestDSN = input
	if !skip {
		packageTestDSN = thr
		if err := stampSchemaMigrations(thr); err != nil {
			fmt.Fprintf(os.Stderr, "tenantbackup integration setup failed: %v\n", err)
			cleanup()
			os.Exit(1)
		}
	}
	code := m.Run()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

// stampSchemaMigrations gives the THROWAWAY database the golang-migrate bookkeeping table that
// ittest.Setup leaves out (it applies the .up.sql files directly). A backup records the schema
// version it was taken at, so the table must exist and name the newest migration. Only ever done
// on this package's own nexus_tbk_it_<pid> database — never on a shared one.
func stampSchemaMigrations(dsn string) error {
	db, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		return fmt.Errorf("stamp: connect: %w", err)
	}
	defer db.Close()
	files, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil {
		return err
	}
	var newest int64
	for _, f := range files {
		n, err := strconv.ParseInt(strings.SplitN(filepath.Base(f), "_", 2)[0], 10, 64)
		if err == nil && n > newest {
			newest = n
		}
	}
	if newest == 0 {
		return fmt.Errorf("stamp: no migrations found")
	}
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty tinyint(1) NOT NULL)"); err != nil {
		return fmt.Errorf("stamp: create: %w", err)
	}
	if _, err := db.Exec("INSERT INTO schema_migrations (version, dirty) VALUES (?, 0) ON DUPLICATE KEY UPDATE version = VALUES(version)", newest); err != nil {
		return fmt.Errorf("stamp: insert: %w", err)
	}
	return nil
}

// itDB opens the throwaway database, skipping when MySQL is unreachable.
func itDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := sqlx.Connect("mysql", packageTestDSN)
	if err != nil {
		t.Skipf("MySQL not reachable (%v) — skipping integration test", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// randSuffix returns 8 hex chars: temp tables / entities are named tbk_it_<rand> so a test can
// never touch (or collide with) a table another test or lane owns.
func randSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}
