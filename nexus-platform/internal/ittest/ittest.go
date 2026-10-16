//go:build integration

package ittest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Setup self-provisions ONE throwaway, migrated MySQL database for a whole test package and
// returns a DSN pointing at it plus a cleanup that DROPs it. It mirrors the gold pattern used
// by internal/services (o3aSetup) and internal/handlers (setupMigratedDB): CREATE a
// uniquely-named database, apply every migrations/*.up.sql from zero, run, drop — so a test
// package never depends on an externally created/USE'd database (harness bug #84: connecting
// to a db-less TEST_DATABASE_URL yielded "Error 1046 (3D000): No database selected").
//
// inputDSN is any go-sql-driver DSN (with or without a db name); only its server segment
// (credentials + host + params) is used. namePrefix disambiguates the throwaway DB per package
// (e.g. "nexus_ai_it"); the PID is appended so package binaries running as separate processes
// under `go test ./...` never collide. migrationsDir is resolved relative to the CALLING
// package's working directory (both callers live at internal/<pkg>, so both pass
// "../../migrations").
//
// Skip vs fail are DELIBERATELY distinct — a migration break must never masquerade as a skip,
// which would false-green the gate (the exact class of false "regression" #84 exists to kill):
//   - skip==true, err==nil : MySQL unreachable. Caller runs its tests as-is; each Skips on its
//     own connect failure (preserving the pre-#84 "MySQL not reachable — skipping" behavior).
//   - err!=nil             : MySQL reachable but CREATE/connect/migrate failed. Caller MUST
//     hard-exit (os.Exit(1)); this is a real break, not a skip.
//   - success              : throwawayDSN + cleanup (idempotent DROP), skip==false, err==nil.
//
// cleanup() never runs when the process is killed (-timeout panic, Ctrl-C, a dead agent), so
// Setup first drops every OTHER throwaway DB (staleDBName) whose tables are older than staleAfter
// — see sweepStale. Its own name is pre-dropped too, for a killed run whose PID came back.
func Setup(inputDSN, namePrefix, migrationsDir string) (throwawayDSN string, cleanup func(), skip bool, err error) {
	server := serverDSN(inputDSN)
	admin, connErr := sqlx.Connect("mysql", server)
	if connErr != nil {
		// MySQL unreachable — signal a skip, never a failure.
		return "", nil, true, nil
	}
	defer admin.Close()

	// Collect the throwaway DBs of earlier runs killed before their cleanup() (queue #71). Best
	// effort: a sweep problem is reported on stderr, never turned into a package failure.
	dropped, sweepErr := sweepStale(admin, staleDBName, staleAfter)
	if sweepErr != nil {
		fmt.Fprintf(os.Stderr, "ittest.Setup: %v\n", sweepErr)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(os.Stderr, "ittest.Setup: dropped %d stale throwaway DB(s) older than %s: %s\n",
			len(dropped), staleAfter, strings.Join(dropped, " "))
	}

	dbName := fmt.Sprintf("%s_%d", namePrefix, os.Getpid())
	// Safety net: a prior run killed before its cleanup (e.g. -race timeout, os.Exit) may have
	// leaked this exact name. Drop before create so no throwaway DB accumulates.
	if _, e := admin.Exec("DROP DATABASE IF EXISTS `" + dbName + "`"); e != nil {
		return "", nil, false, fmt.Errorf("ittest.Setup: pre-drop %s: %w", dbName, e)
	}
	if _, e := admin.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); e != nil {
		return "", nil, false, fmt.Errorf("ittest.Setup: create %s: %w", dbName, e)
	}
	drop := func() {
		if a, e := sqlx.Connect("mysql", server); e == nil {
			_, _ = a.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
			_ = a.Close()
		}
	}

	// Apply migrations over a connection with multiStatements so a whole .up.sql file (incl.
	// stored-procedure bodies) execs in one call.
	migDB, e := sqlx.Connect("mysql", withMultiStatements(namedDSN(inputDSN, dbName)))
	if e != nil {
		drop()
		return "", nil, false, fmt.Errorf("ittest.Setup: connect %s: %w", dbName, e)
	}
	defer migDB.Close()
	if e := applyMigrations(migDB, migrationsDir); e != nil {
		drop()
		return "", nil, false, fmt.Errorf("ittest.Setup: migrate %s: %w", dbName, e)
	}

	// Hand tests a DSN with the ORIGINAL params (no forced multiStatements) — same shape they
	// used before #84, just pointed at the throwaway DB.
	return withUTC(withFoundRows(namedDSN(inputDSN, dbName))), drop, false, nil
}

// sweepStale drops every schema whose name matches match and whose OLDEST table is at least
// minAge old: the throwaway DBs of runs killed before their cleanup() ran (queue #71 — 31 had
// piled up on the dev MySQL). Age is measured on the server's own clock (CREATE_TIME vs NOW()),
// so the DSN's loc/timezone can't skew it. A schema with no tables has no age and is kept: it
// may be a sibling Setup between CREATE DATABASE and its first migration. A failed drop doesn't
// stop the rest; it returns what it dropped plus the joined errors.
func sweepStale(admin *sqlx.DB, match *regexp.Regexp, minAge time.Duration) ([]string, error) {
	var rows []struct {
		Name string `db:"name"`
		Age  int64  `db:"age"`
	}
	if err := admin.Select(&rows, `
		SELECT TABLE_SCHEMA AS name, TIMESTAMPDIFF(SECOND, MIN(CREATE_TIME), NOW()) AS age
		FROM information_schema.TABLES
		WHERE TABLE_SCHEMA LIKE 'nexus%' AND CREATE_TIME IS NOT NULL
		GROUP BY TABLE_SCHEMA`); err != nil {
		return nil, fmt.Errorf("ittest.sweepStale: list: %w", err)
	}

	ctx := context.Background()
	conn, err := admin.Connx(ctx)
	if err != nil {
		return nil, fmt.Errorf("ittest.sweepStale: conn: %w", err)
	}
	defer conn.Close()
	// A zombie still holding a lock inside an old DB must not park every later Setup behind the
	// DROP's metadata lock (the server default lock_wait_timeout is one year).
	if _, err := conn.ExecContext(ctx, "SET SESSION lock_wait_timeout = 5"); err != nil {
		return nil, fmt.Errorf("ittest.sweepStale: lock_wait_timeout: %w", err)
	}

	var dropped []string
	var errs []error
	for _, r := range rows {
		if !match.MatchString(r.Name) || time.Duration(r.Age)*time.Second < minAge {
			continue
		}
		// An identifier can't be a placeholder; staleDBName admits only [a-z0-9_] (TestStaleDBName).
		if _, err := conn.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+r.Name+"`"); err != nil {
			errs = append(errs, fmt.Errorf("drop %s: %w", r.Name, err))
			continue
		}
		dropped = append(dropped, r.Name)
	}
	if err := errors.Join(errs...); err != nil {
		return dropped, fmt.Errorf("ittest.sweepStale: %w", err)
	}
	return dropped, nil
}

// applyMigrations reads migrationsDir/*.up.sql and execs each in lexical order. Each file is one
// multi-statement Exec so stored-procedure bodies apply intact (lifted from handlers'
// setupMigratedDB, the proven file-based applier for the full migration set).
func applyMigrations(db *sqlx.DB, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", dir, err)
	}
	var ups []string
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".up.sql") {
			ups = append(ups, en.Name())
		}
	}
	sort.Strings(ups)
	for _, name := range ups {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.Exec(string(b)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

// serverDSN strips the db name from a go-sql-driver DSN, returning a db-less server DSN (so
// CREATE/DROP DATABASE works) with multiStatements enabled.
func serverDSN(full string) string {
	creds, host, params := split(full)
	return withMultiStatements(creds + host + params) // host ends in "/"
}

// namedDSN rebuilds a DSN pointing at dbName, preserving the original params.
func namedDSN(full, dbName string) string {
	creds, host, params := split(full)
	return creds + host + dbName + params
}

// split breaks "user:pass@tcp(host:port)/db?params" into creds ("user:pass"),
// host ("@tcp(host:port)/") and params ("?params" or ""). An unrecognized shape returns the
// whole string as creds, so the subsequent Connect fails and the caller Skips.
func split(full string) (creds, host, params string) {
	at := strings.Index(full, "@tcp(")
	if at < 0 {
		return full, "", ""
	}
	creds = full[:at]
	rest := full[at:] // "@tcp(host:port)/db?params"
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return creds, rest, ""
	}
	host = rest[:slash+1] // "@tcp(host:port)/"
	if q := strings.Index(rest, "?"); q >= 0 {
		params = rest[q:]
	}
	return creds, host, params
}

func withMultiStatements(dsn string) string {
	if strings.Contains(dsn, "multiStatements") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&multiStatements=true"
	}
	return dsn + "?multiStatements=true"
}
