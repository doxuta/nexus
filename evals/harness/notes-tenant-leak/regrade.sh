source "$(dirname "$0")/../lib/regrade.sh"
go_gates
hidden notes
check "go test ./internal/notes/ (hidden: WHERE tenant_id = ? AND …, no OR, tenant bound first)" go test -count=1 ./internal/notes/
finish
