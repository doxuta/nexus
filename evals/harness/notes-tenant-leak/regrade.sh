source "$(dirname "$0")/../lib/regrade.sh"
cp "$case_dir/hidden/tenant_pin_hidden_test.go" "$ws/nexus-platform/internal/notes/"
go_gates
check "go test ./internal/notes/ (with the hidden tenant test)" go test -count=1 ./internal/notes/
finish
