source "$(dirname "$0")/../lib/regrade.sh"
cp "$case_dir/hidden/pinned_hidden_test.go" "$ws/nexus-platform/internal/notes/"
go_gates
check "go test ./internal/notes/ (with the hidden pinned test)" go test -count=1 ./internal/notes/
check "go test ./internal/handlers/" go test -count=1 ./internal/handlers/
up=$(ls migrations/000100_*.up.sql 2>/dev/null)
down=$(ls migrations/000100_*.down.sql 2>/dev/null)
check "one new migration, 000100, up and down" bash -c '[ "$(ls migrations | grep -c "^0001[0-9][0-9]_")" -eq 2 ] && [ -n "$0" ] && [ -n "$1" ]' "$up" "$down"
check "up adds pinned to sys_record_notes, off by default" python3 -c '
import re, sys; s = open(sys.argv[1]).read()
assert re.search(r"ALTER\s+TABLE\s+`?sys_record_notes`?[\s\S]*\bpinned\b[\s\S]*DEFAULT\s+(0|FALSE)\b", s, re.I), s' "${up:-/dev/null}"
check "down drops pinned" python3 -c '
import re, sys; s = open(sys.argv[1]).read()
assert re.search(r"DROP\s+(COLUMN\s+)?`?pinned\b", s, re.I), s' "${down:-/dev/null}"
check "the API returns pinned" grep -q 'json:"pinned' internal/handlers/note_handler.go
finish
