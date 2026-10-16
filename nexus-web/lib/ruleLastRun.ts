// lib/ruleLastRun.ts — #337: which rules wear the red "last run failed" mark.
// Source: GET /admin/workflows/runs (newest first, server cap 100 rows per call). A rule's mark
// rests ONLY on its first row in the page: an older failure behind a success has no mark, and a
// rule with no row in the page (its runs are older than 100 runs of other rules) is unmarked and
// never described as "never ran" — absence from the page proves nothing.
// ponytail: 100-run ceiling per id group; a per-rule "latest run" endpoint (E-F2-4) lifts it.

import type { WorkflowRun } from "./workflowRuns";

/** The server caps `limit` and `workflowIds` at 100, so ids go in groups of at most 100. */
export function chunkIds(ids: string[], size = 100): string[][] {
  const out: string[][] = [];
  for (let i = 0; i < ids.length; i += size) out.push(ids.slice(i, i + size));
  return out;
}

/** First row per rule of a newest-first page. */
export function newestRunByWorkflow(runs: WorkflowRun[]): Map<string, WorkflowRun> {
  const m = new Map<string, WorkflowRun>();
  for (const r of runs) if (!m.has(r.workflowId)) m.set(r.workflowId, r);
  return m;
}

/** workflowId → id of its newest run, for rules whose newest run is `failed` (blocked is a veto doing its job). */
export function failedWorkflowIds(runs: WorkflowRun[]): Map<string, string> {
  const out = new Map<string, string>();
  for (const [wid, r] of newestRunByWorkflow(runs)) if (r.status === "failed") out.set(wid, r.id);
  return out;
}
