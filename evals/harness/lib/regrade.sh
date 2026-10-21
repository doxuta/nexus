# Sourced by every case's regrade.sh, which regrade.py runs as `bash regrade.sh <copy of a run's workspace>`
# outside the sandbox, with your Go and module cache. Each check prints PASS or FAIL; the script exits 1 if any
# failed. The checks run real commands on what the agent left, plus tests the agent never saw (hidden/).
set -uo pipefail
case_dir=$(cd "$(dirname "$0")" && pwd)
ws=$(cd "$1" && pwd)
failed=0

check() { # check <name> <command...>
	local name=$1
	shift
	if out=$("$@" 2>&1); then
		echo "PASS $name"
	else
		echo "FAIL $name"
		echo "$out" | tail -15 | sed 's/^/     /'
		failed=1
	fi
}

go_gates() { # the AGENTS.md Go gates that run without a database
	cd "$ws/nexus-platform"
	check "gofmt -l . is empty" bash -c 'test -z "$(gofmt -l .)"'
	check "go vet ./..." go vet ./...
	check "go build ./..." go build ./...
}

finish() { exit $failed; }
