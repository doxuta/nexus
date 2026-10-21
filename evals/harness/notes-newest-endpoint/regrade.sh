source "$(dirname "$0")/../lib/regrade.sh"
go_gates
check "the route is registered" bash -c "grep -rE --include='*.go' --exclude='*_test.go' '\.GET\(\s*\"/:entity/:id/notes/newest\"' internal/handlers"
check "nexus-web/lib/api.ts calls it" grep -q 'notes/newest' ../nexus-web/lib/api.ts
check "check-orphan-endpoints.py --strict" python3 scripts/check-orphan-endpoints.py --strict
check "go test ./internal/handlers/ ./internal/notes/" go test -count=1 ./internal/handlers/ ./internal/notes/
finish
