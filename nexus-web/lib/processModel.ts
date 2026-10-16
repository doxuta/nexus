// Approval process model for the Studio editor (specs/043 FE-BPM-PROPS-a, T222): API process <-> the
// list of steps the maker edits. Pure functions, no React.
//
// Three stored shapes reach the editor: linear (`steps`), a graph that is a plain chain of approval
// nodes (read as steps, sent back as `graph` so it stays a graph), and a graph with branches / machine
// / wait nodes (PROPS-b, T224/T226/T227: edited as a list of nodes — `general`; a graph with a node
// kind this build does not know stays read-only and is sent back verbatim). A step keeps its
// server-given `id` through every edit (FR-002).
import type { ApprovalAssignment, ApprovalGraph, ApprovalGraphNode, ApprovalOptions, ApprovalProcessSummary, ApprovalStep } from "./api";
import { condFromWire, condToWire, type CondState } from "./approvalCriteria";

export type HolderKind = "user" | "role" | "team" | "queue" | "field" | "submitter" | "many";
export const OWNER_FIELD = "owner_id"; // pseudo-field: the record's owner ("chủ hồ sơ")

export interface ProcessModel {
  steps: ApprovalStep[];
  /** true = stored/sent as a graph (chain of approval nodes) */
  graphMode: boolean;
  /** false = the graph has a node kind this editor does not know; `rawGraph` is sent back untouched */
  editable: boolean;
  rawGraph: ApprovalGraph | null;
  /** true = a graph with branches / machine / wait nodes: edited as `nodes` (conditions held as CondState) */
  general: boolean;
  nodes: ApprovalGraphNode[];
  /** options minus the fallback assignee and pauseWhen (lane/calendar ride back unchanged) */
  options: ApprovalOptions;
  /** options.pauseWhen as editable rows (S13); null = never paused */
  pause: CondState | null;
  fallback: { type: "user" | "role"; id: string } | null;
}

export function holderKind(s: ApprovalStep): HolderKind {
  if (s.approvers !== undefined) return "many";
  if (s.assignment && s.assignment.type !== "user" && s.assignment.type !== "role") return s.assignment.type;
  return s.approverType;
}

/** The holder part of a step as one comparable string (used to notice a re-pointed step). */
export const holderSig = (s: ApprovalStep): string =>
  JSON.stringify([s.approverType, s.approverId, s.approvers, s.completion, s.assignment]);

export function setHolderKind(s: ApprovalStep, kind: HolderKind): ApprovalStep {
  const { approvers: _a, completion: _c, assignment: _as, ...rest } = s;
  void _a; void _c; void _as;
  const named = { ...rest, approverType: "user" as const, approverId: "" };
  switch (kind) {
    case "user": case "role": return { ...rest, approverType: kind, approverId: "" };
    case "team": return { ...named, assignment: { type: "team", id: "", strategy: "pool" } };
    case "queue": return { ...named, assignment: { type: "queue", id: "" } };
    case "field": return { ...named, assignment: { type: "field", field: OWNER_FIELD } };
    case "submitter": return { ...named, assignment: { type: "submitter" } };
    case "many": return { ...named, approvers: [{ type: "user", id: "" }, { type: "user", id: "" }], completion: { mode: "all" } };
  }
}

function cleanReject(o: ApprovalStep["onReject"]): ApprovalStep["onReject"] {
  if (!o || o.to === "@end" || !o.to) return undefined;
  return o.to === "@submitter" ? { to: o.to, resumeAt: o.resumeAt ?? "rejector" } : { to: o.to };
}

/** The step as the API wants it: only the keys of its holder kind, no stale leftovers. */
export function toWireStep(s: ApprovalStep): ApprovalStep {
  const { approverType, approverId, approvers, completion, assignment, onReject, ...rest } = s;
  const out: ApprovalStep = { ...rest } as ApprovalStep;
  const rej = cleanReject(onReject);
  if (rej) out.onReject = rej;
  if (approvers !== undefined) {
    out.approvers = approvers;
    const c = completion ?? { mode: "all" as const };
    out.completion = c.mode === "quorum" ? { mode: "quorum", percent: c.percent } : { mode: c.mode };
  } else if (assignment) {
    const a: ApprovalAssignment = { type: assignment.type };
    if (assignment.id) a.id = assignment.id;
    if (assignment.field) a.field = assignment.field;
    if (assignment.type === "team" && assignment.strategy) a.strategy = assignment.strategy;
    out.assignment = a;
  } else {
    out.approverType = approverType;
    out.approverId = approverId;
  }
  return out;
}

export type StepError = "noApprover" | "noField" | "percent";

/** First reason a step cannot be saved, mirroring the server so the maker is told before a round-trip. */
export function stepError(s: ApprovalStep): StepError | null {
  const k = holderKind(s);
  if (k === "submitter") return null;
  if (k === "field") return s.assignment?.field?.trim() ? null : "noField";
  if (k === "many") {
    if (!s.approvers?.length || s.approvers.some((a) => !a.id)) return "noApprover";
    if (s.completion?.mode === "quorum") {
      const p = s.completion.percent ?? 0;
      if (!Number.isInteger(p) || p < 1 || p > 100) return "percent";
    }
    return null;
  }
  if (k === "team" || k === "queue") return s.assignment?.id ? null : "noApprover";
  return s.approverId ? null : "noApprover";
}

const plainNode = (n: ApprovalGraphNode, i: number, nodes: ApprovalGraphNode[]): boolean => {
  if (n.kind !== "approval") return false;
  if (n.branches !== undefined || n.default !== undefined || n.set !== undefined || n.result !== undefined || n.until !== undefined || n.sla !== undefined || n.requiredFields !== undefined) return false;
  return !n.next || n.next === nodes[i + 1]?.id;
};

function nodeToStep(n: ApprovalGraphNode): ApprovalStep {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars -- kind/next are the node's, not the step's
  const { kind, next, ...step } = n;
  const a = step.assignment;
  if (a && (a.type === "user" || a.type === "role")) {
    delete step.assignment;
    return { ...step, approverType: a.type, approverId: a.id ?? "" } as ApprovalStep;
  }
  return { ...step, approverType: step.approverType ?? "user", approverId: step.approverId ?? "" } as ApprovalStep;
}

export const NODE_KINDS = ["approval", "task", "gateway", "auto", "wait", "end"] as const;
export type NodeKind = (typeof NODE_KINDS)[number];
const isHuman = (k: string) => k === "approval" || k === "task";

/** A node as the editor holds it: conditions (gateway branches, wait until + SLA matrix) become CondState. */
function nodeToEditor(n: ApprovalGraphNode): ApprovalGraphNode {
  const out: ApprovalGraphNode = { ...n };
  if (Array.isArray(n.branches)) out.branches = (n.branches as { name?: string; when?: unknown; to?: string }[]).map((b) => ({ ...b, when: condFromWire(b.when) }));
  if (n.until !== undefined) out.until = condFromWire(n.until);
  const sla = n.sla as { matrix?: { when?: unknown }[] } | undefined;
  if (sla && typeof sla === "object" && !Array.isArray(sla)) out.sla = { ...sla, matrix: (sla.matrix ?? []).map((r) => ({ ...r, when: condFromWire(r.when) })) };
  return out;
}

export function readProcess(p: ApprovalProcessSummary): ProcessModel {
  const { fallbackAssignee, pauseWhen, ...options } = p.options ?? {};
  const base = { options: options as ApprovalOptions, fallback: fallbackAssignee ?? null, pause: pauseWhen ? condFromWire(pauseWhen) : null };
  const g = p.graph;
  if (g && g.nodes?.length) {
    const chain = g.nodes.every((n, i, all) => plainNode(n, i, all)) && (!g.start || g.start === g.nodes[0].id);
    if (chain) return { ...base, graphMode: true, editable: true, general: false, nodes: [], rawGraph: g, steps: g.nodes.map(nodeToStep) };
    const known = g.nodes.every((n) => (NODE_KINDS as readonly string[]).includes(n.kind));
    return { ...base, graphMode: true, editable: known, general: known, nodes: known ? g.nodes.map(nodeToEditor) : [], rawGraph: g, steps: [] };
  }
  return { ...base, graphMode: false, editable: true, general: false, nodes: [], rawGraph: null, steps: p.steps ?? [] };
}

/** The step part of a human node (approval/task), as ApprovalStepEditor edits it. */
export const nodeStepView = (n: ApprovalGraphNode): ApprovalStep => nodeToStep(n);

/** Puts an edited step back on its node: keeps kind/next/requiredFields, takes the step's keys. */
export function nodeWithStep(n: ApprovalGraphNode, step: ApprovalStep): ApprovalGraphNode {
  const { kind, next, requiredFields } = n;
  const out: ApprovalGraphNode = { ...toWireStep(step), kind };
  if (next) out.next = next;
  if (requiredFields) out.requiredFields = requiredFields as string[];
  return out;
}

/** Next free `sN` id; `used` remembers every id of the session, so a deleted node's id never returns as another node. */
export function newNodeId(used: Set<string>): string {
  let n = used.size + 1;
  while (used.has(`s${n}`)) n++;
  used.add(`s${n}`);
  return `s${n}`;
}

/** A linear/chain process turned into nodes (the first machine node added to it makes it a graph). */
export function stepsToNodes(steps: ApprovalStep[], used: Set<string>): ApprovalGraphNode[] {
  steps.forEach((s) => s.id && used.add(s.id));
  return steps.map((s) => ({ ...toWireStep(s), kind: "approval", id: s.id ?? newNodeId(used) }));
}

export function newNode(kind: NodeKind, id: string): ApprovalGraphNode {
  switch (kind) {
    case "approval": case "task": return { id, kind, approverType: "user", approverId: "", slaSeconds: 3600 };
    case "gateway": return { id, kind, branches: [], default: "" };
    case "auto": return { id, kind, set: [{ field: "", value: "" }] };
    case "wait": return { id, kind, until: { match: "and", rows: [] } };
    case "end": return { id, kind, result: "approved" };
  }
}

/** Drops node `id` and every pointer at it (next / default / branch / reject target), so a save is not a 422 about a ghost. */
export function removeNode(nodes: ApprovalGraphNode[], id: string): ApprovalGraphNode[] {
  return nodes.filter((n) => n.id !== id).map((n) => {
    const out: ApprovalGraphNode = { ...n };
    if (out.next === id) delete out.next;
    if (out.default === id) out.default = "";
    if (Array.isArray(out.branches)) out.branches = (out.branches as { to?: string }[]).map((b) => (b.to === id ? { ...b, to: "" } : b));
    if (out.onReject?.to === id) delete out.onReject;
    return out;
  });
}

const condOrNone = (c: unknown) => condToWire(c as CondState);

/** The node as the API wants it. Human nodes go through toWireStep; machine nodes keep only their own keys. */
export function wireNode(n: ApprovalGraphNode): ApprovalGraphNode {
  if (isHuman(n.kind)) return nodeWithStep(n, nodeToStep(n));
  const head: ApprovalGraphNode = { id: n.id, kind: n.kind, ...(n.name ? { name: n.name } : {}) };
  const next = n.next ? { next: n.next } : {};
  switch (n.kind) {
    case "gateway":
      return { ...head, branches: ((n.branches ?? []) as { name: string; when: unknown; to: string }[]).map((b) => ({ name: b.name, when: condOrNone(b.when), to: b.to })), default: n.default ?? "" };
    case "auto":
      return { ...head, set: n.set, ...next };
    case "end":
      return { ...head, result: n.result };
    default: { // wait
      const sla = n.sla as Record<string, unknown> | undefined;
      const out: ApprovalGraphNode = { ...head, until: condOrNone(n.until), ...next };
      if (sla) {
        const { matrix, ...rest } = sla as { matrix?: { when: unknown; amount: number; unit: string }[] };
        const clean = Object.fromEntries(Object.entries(rest).filter(([, v]) => v !== "" && v !== undefined && v !== null && v !== 0));
        out.sla = { ...clean, ...(matrix?.length ? { matrix: matrix.map((r) => ({ when: condOrNone(r.when), amount: r.amount, unit: r.unit })) } : {}) };
      }
      return out;
    }
  }
}

/** Number of steps/nodes of a stored process, linear or graph (the list shows it). */
export const stepCount = (p: ApprovalProcessSummary): number => p.graph?.nodes?.length || p.steps?.length || 0;

export interface ProcessHead { name: string; entityName: string; trigger: string; isActive: boolean }

/** The PUT/POST body for the model. `criteria` and `outcome` are added by the caller. */
export function buildBody(m: ProcessModel, head: ProcessHead): ProcessHead & { steps: ApprovalStep[]; graph?: ApprovalGraph; options?: ApprovalOptions } {
  const pauseWhen = m.pause ? condToWire(m.pause) : undefined;
  const opts: ApprovalOptions = { ...m.options, ...(m.fallback ? { fallbackAssignee: m.fallback } : {}), ...(pauseWhen ? { pauseWhen } : {}) };
  const options = Object.keys(opts).length ? { options: opts } : {};
  if (m.general) return { ...head, steps: [], graph: { nodes: m.nodes.map(wireNode), ...(m.rawGraph?.start ? { start: m.rawGraph.start } : {}) }, ...options };
  if (m.graphMode && m.rawGraph) {
    if (!m.editable) return { ...head, steps: [], graph: m.rawGraph, ...options };
    const nodes = m.steps.map((s) => ({ ...toWireStep(s), kind: "approval" }));
    return { ...head, steps: [], graph: { nodes: nodes as ApprovalGraphNode[] }, ...options };
  }
  return { ...head, steps: m.steps.map(toWireStep), ...options };
}

/** Index of the step a 422 points at: its `details.field` path (`steps[1]…` / `graph.nodes[2]…`), else the
 * server's "bước N: …" message prefix (most step errors carry no path). null = not about one step. */
export function errorStepIndex(field: string | undefined, message?: string): number | null {
  const m = /^(?:steps|graph\.nodes)\[(\d+)\]/.exec(field ?? "");
  if (m) return Number(m[1]);
  const n = /^bước (\d+)\b/.exec(message ?? "");
  return n ? Number(n[1]) - 1 : null;
}

/** Earlier steps a rejection of step `idx` may go back to — by id, so only steps the server already named. */
export function rejectTargets(steps: ApprovalStep[], idx: number): { id: string; index: number }[] {
  return steps.slice(0, idx).flatMap((s, index) => (s.id ? [{ id: s.id, index }] : []));
}
