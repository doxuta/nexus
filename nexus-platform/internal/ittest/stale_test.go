package ittest

import (
	"testing"
	"time"
)

// TestStaleDBName pins which schema names Setup's age sweep may ever touch. The keep-list is the
// real 2026-09-28 dev MySQL listing (queue #71) plus the parallel-worktree clones: a drift here
// would drop somebody's live database, so every non-throwaway shape is named explicitly.
func TestStaleDBName(t *testing.T) {
	sweep := []string{
		"nexus_handlers_it_14577",   // ittest.Setup: nexus_<pkg>_it_<pid>
		"nexus_dynrepo_it_52532",    // ittest.Setup
		"nexus_a4_it_50332_76",      // a4 harness: nexus_a4_it_<pid>_<n>
		"nexus_jobs_it_4242_3",      // jobs/janitor/o3a/vp/pi/rm18 harnesses: <prefix>_<pid>_<n>
		"nexus_layout_it_7",         // layout harness before specs/054: nexus_layout_it_<n>
		"nexus_layout_it_4242_7",    // layout harness: nexus_layout_it_<pid>_<n>
		"nexus_notification_it_311", // ittest.Setup
	}
	keep := []string{
		"nexus_platform",                         // the shared dev DB
		"nexus_it",                               // static DB the ai/services suites document
		"nexus_wt_ittest_db_leak_and_jobs_index", // this batch's worktree clone
		"nexus_wt_audit_it_x",                    // a clone whose slug happens to contain "it"
		"nexus_wt_foo_it_2",                      // …even with a digit suffix
		"nexus_rm2527_70249_054fad81",            // other naming scheme — out of scope, never guessed at
		"nexus_mig012_1a2b3c4d",
		"information_schema", "mysql", "performance_schema", "sys",
		"nexus_handlers_it_",            // no suffix
		"nexus_handlers_it_12x",         // non-digit suffix
		"nexus_handlers_it_1_2_3",       // deeper than <pid>_<n>
		"NEXUS_HANDLERS_IT_1",           // case matters
		"nexus_handlers_it_1`; DROP x",  // the name is spliced into DROP — must stay [a-z0-9_]
		"xnexus_handlers_it_1",          // anchored at the start
		"nexus_handlers_it_1\nnexus_it", // …and at the end
	}
	for _, n := range sweep {
		if !staleDBName.MatchString(n) {
			t.Errorf("staleDBName must match throwaway DB %q", n)
		}
	}
	for _, n := range keep {
		if staleDBName.MatchString(n) {
			t.Errorf("staleDBName must NOT match %q", n)
		}
	}
	if staleAfter < 6*time.Hour { // < 6h would reach a sibling package still running (-timeout 25m in CI)
		t.Errorf("staleAfter = %s, want hours so a live run is never swept", staleAfter)
	}
}
