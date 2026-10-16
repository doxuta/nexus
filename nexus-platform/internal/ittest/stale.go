package ittest

import (
	"regexp"
	"time"
)

// staleDBName matches the throwaway databases the integration harnesses create and Setup's age
// sweep may drop (queue #71): nexus_<pkg>_it_<pid> (Setup itself), nexus_<prefix>_it_<pid>_<n>
// (the a4/jobs/janitor/o3a/vp/pi/rm18/layout harnesses) and the older nexus_layout_it_<n>. Anchored, lowercase,
// digit-only suffix: it can never reach nexus_platform, the static nexus_it DB, or a
// nexus_wt_<slug> worktree clone — and the matched name is safe to splice into DROP DATABASE.
// Untagged so the plain `go test ./...` gate pins it (TestStaleDBName); var so the integration
// test can narrow the sweep to its own probes.
var staleDBName = regexp.MustCompile(`^nexus_[a-z0-9]+_it_[0-9]+(_[0-9]+)?$`)

// staleAfter is how old a matching DB's oldest table must be before Setup drops it. A throwaway DB
// lives for one package run (CI caps that at -timeout 25m); 24h keeps every sibling package and
// parallel worktree still running on the same MySQL out of reach.
var staleAfter = 24 * time.Hour
