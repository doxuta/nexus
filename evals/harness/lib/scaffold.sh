# Sourced by every case's scaffold.sh. `claude plugin eval --scaffold` runs that script with bash, as you, in the
# run's empty workspace, with HOME set to the run's own temporary home (the workspace is $HOME/cwd) and only PATH,
# TMPDIR and TERM from your shell.
#
# Claude Code's sandbox can run your `go` but cannot read your home directory, /tmp or the network, so the Go root,
# the modules and a warm build cache are cloned into the run's home, outside the workspace, and `go env -w` points
# Go at them. git cannot run inside that sandbox, hence -buildvcs=false.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

if [ ! -d "$cache/gocache" ]; then
	echo ".cache/harness-eval is missing: run evals/harness/prepare.sh first" >&2
	exit 1
fi

ws=$PWD
checkout "$ws"

gohome="$HOME/.nexus-eval-go"
mkdir -p "$gohome"
clone "$(go env GOROOT)" "$gohome/root"
clone "$cache/mod" "$gohome/mod"
clone "$cache/gocache" "$gohome/cache"
go env -w GOROOT="$gohome/root" GOMODCACHE="$gohome/mod" GOCACHE="$gohome/cache" \
	GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOFLAGS="$GOFLAGS_EVAL"

git -C "$ws" init -q

# commit_base: each case calls it once its fixture is in place, so `git diff` shows only the agent's work.
commit_base() {
	git -C "$ws" add -A
	git -C "$ws" -c user.name=eval -c user.email=eval@example.invalid commit -q -m "base"
}
