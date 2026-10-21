#!/usr/bin/env bash
# Run once before `claude plugin eval` (and again after changing BASE or Go): fills .cache/harness-eval
# (gitignored) with the Go modules of BASE, copied from your module cache, and a build cache warmed on them.
# Every case's scaffold clones both into its run in a second instead of compiling for a minute.
source "$(dirname "$0")/lib/common.sh"

src=$(mktemp -d)
trap 'rm -rf "$src"' EXIT
checkout "$src"

modcache=$(go env GOMODCACHE)
mkdir -p "$cache"
cd "$src/nexus-platform"
export GOMODCACHE="$cache/mod" GOCACHE="$cache/gocache" GOPROXY="file://$modcache/cache/download" \
	GOSUMDB=off GOTOOLCHAIN=local GOFLAGS="$GOFLAGS_EVAL -modcacherw"
go build ./...
go vet ./...
go test -count=1 -run '^$' ./... >/dev/null
du -sh "$cache/mod" "$cache/gocache"
