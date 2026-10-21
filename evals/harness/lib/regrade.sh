# Sourced by every case's regrade.sh, which regrade.py runs as `bash regrade.sh <copy of a run's workspace>`
# outside the sandbox, with your Go and module cache. Each check prints PASS or FAIL; the script exits 1 if any
# failed. The checks run real commands on what the agent left, plus tests it never saw (hidden/). Gate scripts and
# test files the checks rely on are taken from BASE, not from the workspace the agent could edit.
#
# This runs code the agent wrote (its Go code and tests, its Python) as you, outside any sandbox: grade only runs
# you produced, or run regrade.py in a container.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"
set +e
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

from_base() { # from_base <path>: overwrite the workspace's copy of a repo file with BASE's
	mkdir -p "$(dirname "$ws/$1")"
	git -C "$repo" show "$BASE:$1" >"$ws/$1"
}

hidden() { # hidden <package>: add the case's hidden tests for internal/<package>, with the SQL driver they use
	local dst="$ws/nexus-platform/internal/$1"
	cp "$case_dir/hidden/$1/"*_test.go "$dst/"
	sed "s/^package PKG$/package $1/" "$lib/hidden/sqldriver_test.go" >"$dst/zz_hidden_sqldriver_test.go"
	if [ "$1" = handlers ]; then cp "$lib/hidden/handlers_wiring_test.go" "$dst/zz_hidden_wiring_test.go"; fi
}

go_gates() { # the AGENTS.md Go gates that run without a database
	cd "$ws/nexus-platform"
	check "gofmt -l . is empty" bash -c 'test -z "$(gofmt -l .)"'
	check "go vet ./..." go vet ./...
	check "go build ./..." go build ./...
}

sql_matches() { # sql_matches <file> <regex>: the regex matches the SQL file with its comments removed
	python3 -c 'import re, sys
s = re.sub(r"--[^\n]*|#[^\n]*|/\*.*?\*/", "", open(sys.argv[1]).read(), flags=re.S)
sys.exit(0 if re.search(sys.argv[2], s, re.I) else 1)' "$@"
}

finish() { exit $failed; }
