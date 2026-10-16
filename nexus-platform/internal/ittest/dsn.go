package ittest

import "strings"

// withFoundRows makes a DSN carry clientFoundRows=true — production's setting
// (config.ApplyDBTimeouts, specs/057-found-rows-dsn): RowsAffected counts MATCHED rows. Setup
// returns every throwaway DSN through it, so a bare `go test -tags=integration` whose
// TEST_DATABASE_URL lacks the flag still judges the code under production semantics instead of
// "changed rows" (queue #328). The driver keeps the LAST value of a repeated parameter, so an
// explicit clientFoundRows=false is overridden exactly as production overrides it.
func withFoundRows(dsn string) string {
	if strings.Contains(dsn, "clientFoundRows=true") {
		return dsn
	}
	if strings.Contains(dsn, "?") {
		return dsn + "&clientFoundRows=true"
	}
	return dsn + "?clientFoundRows=true"
}

// withUTC pins the throwaway DSN to loc=UTC — production's setting (config.go: "the DB stores
// timestamps in UTC, and loc=Local on a +07 host mislabels them"). Without it, on a non-UTC host
// (the nexus-mac CI runner is +07) Go-clock writers (GORM created_at, publish cutoff) and DB-clock
// writers (CURRENT_TIMESTAMP in a UTC server) land 7h apart, so time comparisons between them
// (draft-save instant vs field birth, publish cutoff vs field birth) flip. The driver keeps the
// LAST value of a repeated parameter, so an explicit loc=Local is overridden.
func withUTC(dsn string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&loc=UTC"
	}
	return dsn + "?loc=UTC"
}
