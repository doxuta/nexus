#!/bin/bash
# batch-preflight.sh — disk guard for a parallel worktree batch (DAILY-QUEUE #363 / #366).
#
# 2026-09-30: ~20 parallel worktrees grew ~/Library/Caches/go-build to 78 GB, the host had 289 MB
# free, the Docker Desktop VM went read-only and nexus-mysql died for every lane. Run this BEFORE
# handing out each wave; a non-zero exit means "do not dispatch", with the reason on stderr.
#
#   bash scripts/batch-preflight.sh            # check only
#   PRUNE=1 bash scripts/batch-preflight.sh    # first drop go-build cache files unused for PRUNE_MIN
#
# Env: MIN_FREE_GB (default 20) — free space required on the host disk AND inside the Docker VM
#      PRUNE_MIN   (default 180) — minutes a go-build cache file must be unused before PRUNE=1 removes it
#      MYSQL_CONTAINER (default nexus-mysql) — used to read the VM disk (its /var/lib/mysql lives there)
# The Go cache is regenerable, so pruning only costs a rebuild. Also set GOFLAGS=-trimpath for the
# batch: otherwise every worktree path is a separate cache key (measured: +75 MB per extra checkout
# without it, +0.8 MB with it — see docs/dev-environment.md "Worktree song song").

MIN_FREE_GB="${MIN_FREE_GB:-20}"
PRUNE_MIN="${PRUNE_MIN:-180}"
MYSQL_CONTAINER="${MYSQL_CONTAINER:-nexus-mysql}"

if [ "${PRUNE:-0}" = "1" ]; then
  cache="$(go env GOCACHE 2>/dev/null)"
  if [ -d "$cache" ]; then
    find "$cache" -type f -mmin "+$PRUNE_MIN" -delete 2>/dev/null
    echo "preflight: pruned go-build files unused > ${PRUNE_MIN} min in $cache"
  fi
fi

fail=0
free_gb() { awk 'NR==2 {printf "%d", $4 / 1048576}' ; }   # df -k column 4 (KB available) -> GB

host_free="$(df -k / | free_gb)"
echo "preflight: host disk free ${host_free} GB (need ${MIN_FREE_GB})"
if [ "$host_free" -lt "$MIN_FREE_GB" ]; then
  echo "preflight: STOP — host disk below ${MIN_FREE_GB} GB free; run with PRUNE=1 or clear caches" >&2
  fail=1
fi

if vm_df="$(docker exec "$MYSQL_CONTAINER" df -k /var/lib/mysql 2>/dev/null)"; then
  vm_free="$(printf '%s\n' "$vm_df" | free_gb)"
  echo "preflight: Docker VM disk free ${vm_free} GB (need ${MIN_FREE_GB})"
  if [ "$vm_free" -lt "$MIN_FREE_GB" ]; then
    echo "preflight: STOP — Docker VM disk below ${MIN_FREE_GB} GB free" >&2
    fail=1
  fi
else
  echo "preflight: STOP — cannot read the Docker VM disk (container $MYSQL_CONTAINER down or Docker read-only)" >&2
  fail=1
fi
exit $fail
