source "$(dirname "$0")/../lib/regrade.sh"
go_gates
hidden notes
hidden handlers
check "go test ./internal/notes/ ./internal/handlers/" go test -count=1 ./internal/notes/ ./internal/handlers/
check "hidden tests ran and passed: pinned is read and listed first, the notes list returns it" hidden_pass notes handlers
up=$(ls migrations/000100_*.up.sql 2>/dev/null)
down=$(ls migrations/000100_*.down.sql 2>/dev/null)
check "one new migration, 000100, up and down" bash -c '[ "$(ls migrations | grep -c "^0001[0-9][0-9]_")" -eq 2 ] && [ -n "$0" ] && [ -n "$1" ]' "$up" "$down"
check "up adds pinned to sys_record_notes, off by default (comments ignored)" \
	sql_matches "${up:-/dev/null}" 'ALTER\s+TABLE\s+`?sys_record_notes`?[\s\S]*\bpinned\b[\s\S]*DEFAULT\s+(0|FALSE)\b'
check "down drops pinned (comments ignored)" sql_matches "${down:-/dev/null}" 'DROP\s+(COLUMN\s+)?`?pinned\b'
finish
