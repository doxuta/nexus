source "$(dirname "$0")/../lib/regrade.sh"
go_gates
from_base nexus-platform/scripts/check-orphan-endpoints.py
check "check-orphan-endpoints.py --strict (the script from BASE)" python3 scripts/check-orphan-endpoints.py --strict
check "nexus-web/lib/api.ts exports a function that GETs /notes/newest (comments ignored)" \
	python3 "$lib/web_caller.py" ../nexus-web/lib/api.ts /notes/newest
hidden handlers
check "go test ./internal/handlers/ ./internal/notes/ (hidden: null with no notes, else the newest)" \
	go test -count=1 ./internal/handlers/ ./internal/notes/
finish
