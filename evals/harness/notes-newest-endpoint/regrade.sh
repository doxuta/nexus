source "$(dirname "$0")/../lib/regrade.sh"
go_gates
from_base nexus-platform/scripts/check-orphan-endpoints.py
check "check-orphan-endpoints.py --strict (the script from BASE)" python3 scripts/check-orphan-endpoints.py --strict
check "nexus-web/lib/api.ts: an exported function GETs /\${…}/\${…}/notes/newest (comments ignored)" \
	python3 "$lib/web_caller.py" ../nexus-web/lib/api.ts '/\$\{[^}]+\}/\$\{[^}]+\}/notes/newest'
hidden handlers
check "go test ./internal/handlers/ ./internal/notes/" go test -count=1 ./internal/handlers/ ./internal/notes/
check "hidden tests ran and passed: null, the tenant's newest, 404 on a record the caller cannot see" hidden_pass handlers
finish
