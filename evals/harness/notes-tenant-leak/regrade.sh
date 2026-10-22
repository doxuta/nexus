source "$(dirname "$0")/../lib/regrade.sh"
go_gates
hidden notes
check "go test ./internal/notes/" go test -count=1 ./internal/notes/
check "hidden tests ran and passed: a top-level tenant_id = ? term bound to the tenant, no top-level OR" hidden_pass notes
finish
