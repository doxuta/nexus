# Shared by prepare.sh and every case's scaffold.sh (sourced, never run on its own).
set -euo pipefail

# Every case starts from this commit (origin/main when the suite was written), so a later change to the repo
# does not change what the cases ask.
BASE=59b2f94aaf00edc3669f48b1e93fc01fdcfb756f

lib=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(git -C "$lib" rev-parse --show-toplevel)
cache="$repo/.cache/harness-eval" # gitignored; prepare.sh fills it

# checkout <dir>: the repo at BASE without the harness (no .claude/, no CLAUDE.md), so the run without the
# plugin cannot read it.
checkout() {
	git -C "$repo" archive "$BASE" AGENTS.md README.md SECURITY.md docs evals nexus-platform nexus-web | tar -x -C "$1"
}

# clone <src> <dst>: a copy-on-write copy where the filesystem has one (APFS, Btrfs, XFS), else a plain copy.
clone() {
	cp -cR "$1" "$2" 2>/dev/null || cp -R --reflink=auto "$1" "$2"
}

# -trimpath keeps build-cache keys free of paths, so the cache warmed by prepare.sh serves every run.
GOFLAGS_EVAL="-buildvcs=false -trimpath"
