import { describe, it, expect } from "vitest";
import {
  holderKind, setHolderKind, toWireStep, readProcess, stepError, stepCount, buildBody, errorStepIndex, rejectTargets,
} from "./processModel";
import type { ApprovalProcessSummary, ApprovalStep } from "./api";

const P = (over: Partial<ApprovalProcessSummary>): ApprovalProcessSummary =>
  ({ id: "p", name: "n", entityName: "e", trigger: "manual", isActive: true, ...over });

describe("readProcess", () => {
  it("linear process: steps as stored, graph mode off", () => {
    const m = readProcess(P({ steps: [{ id: "s1", approverType: "role", approverId: "r" }] }));
    expect(m.graphMode).toBe(false);
    expect(m.editable).toBe(true);
    expect(m.steps.map((s) => s.id)).toEqual(["s1"]);
  });

  it("graph of approval nodes in order reads as steps (was shown as '0 bước')", () => {
    const m = readProcess(P({
      steps: [],
      graph: { nodes: [
        { id: "a", kind: "approval", approverType: "user", approverId: "u1" },
        { id: "b", kind: "approval", approvers: [{ type: "user", id: "u1" }, { type: "role", id: "r1" }], completion: { mode: "quorum", percent: 60 } },
        { id: "c", kind: "approval", assignment: { type: "team", id: "t1", strategy: "round_robin" }, onReject: { to: "@submitter", resumeAt: "start" } },
      ] },
    }));
    expect(m.graphMode).toBe(true);
    expect(m.editable).toBe(true);
    expect(stepCount(P({ steps: [], graph: { nodes: [{ kind: "approval" }, { kind: "approval" }] } }))).toBe(2);
    expect(m.steps.map(holderKind)).toEqual(["user", "many", "team"]);
    expect(m.steps[2].onReject).toEqual({ to: "@submitter", resumeAt: "start" });
  });

  it("branches / non-approval nodes make the graph a general node list (PROPS-b)", () => {
    const graph = { nodes: [{ id: "g", kind: "gateway", branches: [], default: "e" }, { id: "e", kind: "end", result: "approved" }] };
    const m = readProcess(P({ steps: [], graph }));
    expect(m.general).toBe(true);
    expect(buildBody(m, { name: "n", entityName: "e", trigger: "manual", isActive: true }).graph).toEqual(graph);
  });

  it("a jump (next) that skips a node is not a plain chain", () => {
    const m = readProcess(P({ steps: [], graph: { nodes: [
      { id: "a", kind: "approval", approverType: "user", approverId: "u", next: "c" },
      { id: "b", kind: "approval", approverType: "user", approverId: "u" },
      { id: "c", kind: "approval", approverType: "user", approverId: "u" },
    ] } }));
    expect(m.general).toBe(true);
  });
});

describe("holder kinds and wire shape", () => {
  const base: ApprovalStep = { id: "s1", slaSeconds: 7200, approverType: "role", approverId: "r1" };

  it("user/role stay in the v1 shape", () => {
    expect(toWireStep(base)).toEqual(base);
  });

  it("team keeps strategy, drops approverType/approverId", () => {
    const s = setHolderKind(base, "team");
    expect(s.id).toBe("s1");
    expect(s.slaSeconds).toBe(7200);
    const w = toWireStep({ ...s, assignment: { type: "team", id: "t1", strategy: "each_member" } });
    expect(w).toEqual({ id: "s1", slaSeconds: 7200, assignment: { type: "team", id: "t1", strategy: "each_member" } });
  });

  it("strategy is dropped for anything that is not a team", () => {
    const w = toWireStep({ ...base, approverType: "user", approverId: "", assignment: { type: "queue", id: "q", strategy: "round_robin" } });
    expect(w.assignment).toEqual({ type: "queue", id: "q" });
  });

  it("many: approvers + completion, percent only with quorum", () => {
    const s = { ...setHolderKind(base, "many"), approvers: [{ type: "user" as const, id: "u" }, { type: "role" as const, id: "r" }], completion: { mode: "all" as const, percent: 70 } };
    expect(toWireStep(s)).toEqual({ id: "s1", slaSeconds: 7200, approvers: s.approvers, completion: { mode: "all" } });
    expect(toWireStep({ ...s, completion: { mode: "quorum", percent: 60 } }).completion).toEqual({ mode: "quorum", percent: 60 });
  });

  it("submitter needs no id; field needs a field", () => {
    expect(stepError(setHolderKind(base, "submitter"))).toBeNull();
    expect(stepError(setHolderKind(base, "field"))).toBeNull(); // defaults to the record owner
    expect(stepError({ ...setHolderKind(base, "field"), assignment: { type: "field", field: "" } })).toBe("noField");
    expect(stepError(setHolderKind(base, "team"))).toBe("noApprover");
    expect(stepError(setHolderKind(base, "many"))).toBe("noApprover");
  });

  it("quorum needs 1..100", () => {
    const many = { ...setHolderKind(base, "many"), approvers: [{ type: "user" as const, id: "a" }, { type: "user" as const, id: "b" }] };
    expect(stepError({ ...many, completion: { mode: "quorum", percent: 0 } })).toBe("percent");
    expect(stepError({ ...many, completion: { mode: "quorum", percent: 101 } })).toBe("percent");
    expect(stepError({ ...many, completion: { mode: "quorum", percent: 60 } })).toBeNull();
  });
});

describe("buildBody", () => {
  const head = { name: "n", entityName: "e", trigger: "manual", isActive: true };
  it("graph mode sends nodes (kind approval), never linear steps", () => {
    const m = readProcess(P({ steps: [], graph: { nodes: [{ id: "a", kind: "approval", approverType: "user", approverId: "u" }] } }));
    const b = buildBody(m, head);
    expect(b.steps).toEqual([]);
    expect(b.graph?.nodes).toEqual([{ id: "a", kind: "approval", approverType: "user", approverId: "u" }]);
  });
  it("carries options through, replacing only the fallback", () => {
    const m = readProcess(P({ steps: [{ id: "s1", approverType: "user", approverId: "u" }], options: { lane: "independent", calendarId: "c" } }));
    expect(buildBody(m, head).options).toEqual({ lane: "independent", calendarId: "c" });
    const b = buildBody({ ...m, fallback: { type: "role", id: "r" } }, head);
    expect(b.options).toEqual({ lane: "independent", calendarId: "c", fallbackAssignee: { type: "role", id: "r" } });
    const none = buildBody(readProcess(P({ steps: [{ id: "s1", approverType: "user", approverId: "u" }] })), head);
    expect(none.options).toBeUndefined();
  });
});

describe("errors and reject targets", () => {
  it("pins a 422 path to the step index", () => {
    expect(errorStepIndex("steps[1].completion.percent")).toBe(1);
    expect(errorStepIndex("graph.nodes[2].approvers")).toBe(2);
    expect(errorStepIndex("outcome.returnedValue")).toBeNull();
    expect(errorStepIndex(undefined)).toBeNull();
    // most step errors carry only the sentence "bước 2: …" (verified live on the DB clone, 2026-10-15)
    expect(errorStepIndex(undefined, "bước 2: assignment.id là bắt buộc với loại \"team\"")).toBe(1);
    expect(errorStepIndex(undefined, "quy trình cần ít nhất 1 bước")).toBeNull();
  });
  it("a step can send a rejection to earlier steps only (by id)", () => {
    const steps: ApprovalStep[] = [
      { id: "s1", approverType: "user", approverId: "u" }, { id: "s2", approverType: "user", approverId: "u" }, { id: "s3", approverType: "user", approverId: "u" },
    ];
    expect(rejectTargets(steps, 0)).toEqual([]);
    expect(rejectTargets(steps, 2).map((t) => t.id)).toEqual(["s1", "s2"]);
    // a step without an id yet (just added) cannot be a target — the server names it on save
    expect(rejectTargets([{ approverType: "user", approverId: "u" }, steps[0]], 1)).toEqual([]);
  });
});
