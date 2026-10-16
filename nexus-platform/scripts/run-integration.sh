#!/bin/bash
# run-integration.sh — run Go integration tests and refuse a silent green.
#
# Why: every *_integration_test.go SKIPS (never fails) when MySQL is unreachable or the
# golang-migrate CLI is missing (internal/ittest.Setup contract), so `go test` prints `ok` for a
# package whose tests never touched a database. A `-run` pattern that matches nothing is `ok` too.
# Both have false-greened a slice gate. This wrapper runs with -v and exits non-zero when:
#   - go test itself fails,
#   - any test reports `--- SKIP` (the environment decided the result, not the code),
#   - no test reports `--- PASS` (the -run pattern matched nothing),
#   - any package line reads `[no tests to run]` (a -run pattern that matched nothing in THAT
#     package; the whole-run PASS count above is satisfied by its siblings, queue #249).
# CI (ci.yml, job integration) runs the whole suite through this wrapper so a dead MySQL service
# or a missing migrate CLI turns the job red instead of an all-SKIP green (queue #244).
#
# Usage (from nexus-platform/; extra go test flags pass straight through):
#   export TEST_DATABASE_URL="root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local&clientFoundRows=true"
#   bash scripts/run-integration.sh ./internal/metadata/ ./internal/janitor/
#   bash scripts/run-integration.sh -run 'TestLayout_' ./internal/handlers/
#
# The full -v log goes to $IT_LOG when set, else to a fresh mktemp file — never a fixed shared
# name, because parallel worktree agents share one scratchpad and overwrite each other's files.
set -uo pipefail

# Production counts MATCHED rows for an UPDATE (config.ApplyDBTimeouts forces clientFoundRows,
# specs/057-found-rows-dsn), but most suites open TEST_DATABASE_URL as given. A stale export
# without the flag would judge the code under CHANGED-row semantics production never uses, so
# default the DSN when unset and append the flag when missing (the driver's last value wins, so
# an explicit clientFoundRows=false is overridden exactly as production overrides it).
: "${TEST_DATABASE_URL:=root:@tcp(127.0.0.1:3306)/nexus_platform?charset=utf8mb4&parseTime=true&multiStatements=true&loc=Local}"
case "$TEST_DATABASE_URL" in
*clientFoundRows=true*) ;;
*\?*) TEST_DATABASE_URL="$TEST_DATABASE_URL&clientFoundRows=true" ;;
*) TEST_DATABASE_URL="$TEST_DATABASE_URL?clientFoundRows=true" ;;
esac
export TEST_DATABASE_URL

log="${IT_LOG:-$(mktemp -t nexus-it.XXXXXX)}"
go test -tags=integration -count=1 -p 4 -v "$@" >"$log" 2>&1
status=$?

grep -E '^(ok|FAIL|panic:)|--- FAIL' "$log"
pass=$(grep -c -- '--- PASS' "$log")
skip=$(grep -c -- '--- SKIP' "$log")
echo "run-integration: PASS=$pass SKIP=$skip go-test-exit=$status log=$log"

if [ "$status" -ne 0 ]; then
	# CI cannot read the mktemp log after the job: surface the diagnosis here. The failing tests
	# first, then what a --- FAIL window misses: compile errors (# pkg + file.go:N), race reports
	# (printed BEFORE their --- FAIL line) and a timeout's goroutine dump (the log's tail).
	grep -B2 -A12 -- '--- FAIL' "$log" | head -150 >&2
	grep -A25 -E 'WARNING: DATA RACE|^# ' "$log" | head -120 >&2
	echo "::group::last 300 lines of the go test log" >&2
	tail -n 300 "$log" >&2
	echo "::endgroup::" >&2
	exit "$status"
fi
empty=$(grep -c -- '\[no tests to run\]' "$log")
if [ "$empty" -gt 0 ]; then
	echo "❌ $empty package(s) ran no tests (-run matched nothing there) — check with go test -list:" >&2
	grep -- '\[no tests to run\]' "$log" >&2
	exit 1
fi
if [ "$skip" -gt 0 ]; then
	echo "❌ $skip test(s) SKIPPED — environment gap (MySQL down? migrate CLI missing?), not a pass:" >&2
	grep -B1 -- '--- SKIP' "$log" | head -20 >&2 # go test -v prints the Skip reason just above
	exit 1
fi
if [ "$pass" -eq 0 ]; then
	echo "❌ 0 tests PASSED — the package/-run pattern matched nothing (check with go test -list)." >&2
	exit 1
fi
