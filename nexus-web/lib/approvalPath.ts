// lib/approvalPath.ts — which steps and branches ONE approval went through (specs/043 US6, T215).
// Pure: reads GET /approvals/:id — the pinned nodes (id, kind, next, branches), `gateway` (node id →
// branch taken) and the history's node ids. Machine nodes (auto, wait, end) are never listed; a
// gateway is, with the arm it took. A node nobody reached is "skipped" once the approval is over or
// no forward route leads to it, "todo" while it still can be reached.

import type { ApprovalDetailData, ApprovalProcessStep } from "./approvalDetail";

export type PathState = "done" | "current" | "todo" | "skipped" | "rejected";

export interface PathStep {
  id: string;
  index: number;
  name: string;
  kind: "approval" | "task" | "gateway";
  state: PathState;
  /** Gateway only: the arm taken; undefined = not passed yet. */
  branch?: string;
}

const SHOWN = new Set(["approval", "task", "gateway"]);

/** Indexes reachable going forward from `from` (exclusive) — every arm of an unresolved gateway counts. */
function forward(steps: ApprovalProcessStep[], from: number, gateway: Record<string, string>): Set<number> {
  const at = new Map(steps.map((s, i) => [s.id ?? String(i), i]));
  const seen = new Set<number>();
  const queue = [from];
  while (queue.length) {
    const i = queue.shift() as number;
    const s = steps[i];
    if (!s) continue; // currentStep outside the pinned chain (a fixture, or an older pin): nothing to walk
    let to: (string | undefined)[];
    if (s.kind === "gateway") {
      const taken = s.id ? gateway[s.id] : undefined;
      to = taken ? [s.branches?.find((b) => b.name === taken)?.to ?? s.default] : [...(s.branches ?? []).map((b) => b.to), s.default];
    } else if (s.kind === "end") to = [];
    else to = [s.next ?? (i + 1 < steps.length ? steps[i + 1].id ?? String(i + 1) : undefined)];
    for (const id of to) {
      const j = id === undefined ? undefined : at.get(id);
      if (j !== undefined && !seen.has(j)) {
        seen.add(j);
        queue.push(j);
      }
    }
  }
  return seen;
}

export function approvalPath(d: ApprovalDetailData): PathStep[] {
  const steps = d.process?.steps ?? [];
  if (steps.length === 0) return [];
  const gateway = d.gateway ?? {};
  const graph = steps.some((s) => s.kind);
  const touched = new Set([...d.history.map((h) => h.node).filter((n): n is string => !!n), ...Object.keys(gateway)]);
  const pending = d.status === "pending";
  const reach = pending ? forward(steps, d.currentStep, gateway) : new Set<number>();

  const out: PathStep[] = [];
  steps.forEach((s, i) => {
    const kind = s.kind ?? "approval";
    if (!SHOWN.has(kind)) return;
    const visited = graph ? !!s.id && touched.has(s.id) : i < d.currentStep || d.status === "approved";
    let state: PathState;
    if (i === d.currentStep && pending) state = "current";
    else if (i === d.currentStep && d.status === "rejected") state = "rejected";
    else if (visited || (!graph && d.status === "approved")) state = "done";
    else if (pending && reach.has(i)) state = "todo";
    else state = "skipped";
    out.push({
      id: s.id ?? String(i),
      index: i,
      name: s.name ?? "",
      kind: kind as PathStep["kind"],
      state,
      branch: kind === "gateway" && s.id ? gateway[s.id] : undefined,
    });
  });
  return out;
}
