import { describe, it, expect } from "vitest";
import { readProcess, buildBody, newNode, newNodeId, stepsToNodes, removeNode, wireNode } from "./processModel";
import { approvalOpsWithEdge, approvalOpsForType, condFromWire, condToWire } from "./approvalCriteria";
import type { ApprovalGraph, ApprovalProcessSummary } from "./api";

const head = { name: "n", entityName: "e", trigger: "after_update", isActive: true };
const P = (over: Partial<ApprovalProcessSummary>): ApprovalProcessSummary => ({ id: "p", name: "n", entityName: "e", trigger: "manual", isActive: true, ...over });

const branching: ApprovalGraph = {
  nodes: [
    { id: "g", kind: "gateway", branches: [{ name: "Lớn", when: { match: "and", rules: [{ field: "amount", op: "gt", value: "50000000" }] }, to: "a" }], default: "e" },
    { id: "a", kind: "approval", approverType: "role", approverId: "r1", slaSeconds: 7200 },
    { id: "w", kind: "wait", until: { match: "or", rules: [{ field: "status", op: "eq", value: "ok" }] }, sla: { amount: 4, unit: "business_hours", writeDueTo: "due_at", onChange: "tighten_only", notify: { type: "submitter" }, matrix: [{ when: { match: "and", rules: [{ field: "priority", op: "eq", value: "high" }] }, amount: 1, unit: "hours" }] } },
    { id: "x", kind: "auto", set: [{ field: "status", value: "Đã duyệt" }], next: "e" },
    { id: "e", kind: "end", result: "approved" },
  ],
};

describe("general graph (branches / machine / wait nodes)", () => {
  it("is editable as nodes and round-trips to the same wire graph", () => {
    const m = readProcess(P({ steps: [], graph: branching }));
    expect(m.general).toBe(true);
    expect(m.editable).toBe(true);
    expect(buildBody(m, head).graph).toEqual(branching);
  });

  it("an unknown node kind stays read-only and is sent back verbatim", () => {
    const graph = { nodes: [{ id: "q", kind: "from-the-future" }] };
    const m = readProcess(P({ steps: [], graph }));
    expect(m.editable).toBe(false);
    expect(buildBody(m, head).graph).toBe(graph);
  });

  it("pauseWhen and fallback ride in options and round-trip", () => {
    const pw = { match: "and", rules: [{ field: "status", op: "eq", value: "Chờ khách" }] };
    const m = readProcess(P({ steps: [{ id: "s1", approverType: "user", approverId: "u" }], options: { lane: "independent", pauseWhen: pw } }));
    expect(m.pause?.rows).toHaveLength(1);
    expect(buildBody(m, head).options).toEqual({ lane: "independent", pauseWhen: pw });
    expect(buildBody({ ...m, pause: { match: "and", rows: [] } }, head).options).toEqual({ lane: "independent" });
  });

  it("a linear process gains a gateway: it turns into nodes that keep their ids", () => {
    const used = new Set<string>();
    const nodes = stepsToNodes([{ id: "s1", approverType: "user", approverId: "u" }, { approverType: "role", approverId: "r" }], used);
    expect(nodes.map((n) => n.id)).toEqual(["s1", "s2"]);
    const gw = newNode("gateway", newNodeId(used));
    expect(gw.id).toBe("s3");
    expect(newNodeId(used)).toBe("s4"); // ids never come back
  });

  it("removing a node clears every pointer at it", () => {
    const m = readProcess(P({ steps: [], graph: branching }));
    const left = removeNode(m.nodes, "e");
    const g = left.find((n) => n.id === "g")!;
    expect(g.default).toBe("");
    expect(left.find((n) => n.id === "x")!.next).toBeUndefined();
  });

  it("wait node sla drops blank keys; an empty matrix is left out", () => {
    const n = newNode("wait", "w");
    n.sla = { amount: 2, unit: "hours", writeDueTo: "", onChange: "", notify: { type: "user", id: "u" }, matrix: [] };
    expect(wireNode(n).sla).toEqual({ amount: 2, unit: "hours", notify: { type: "user", id: "u" } });
  });
});

describe("criteria: edge operators (T045)", () => {
  it("changed / changedTo are offered only with the edge list", () => {
    expect(approvalOpsForType("picklist")).not.toContain("changedTo");
    expect(approvalOpsWithEdge("picklist")).toEqual(expect.arrayContaining(["changed", "changedTo"]));
  });
  it("changedTo keeps its value, changed drops it, match or survives", () => {
    const c = condToWire({ match: "or", rows: [{ id: "1", field: "status", op: "changedTo", value: "Đã gửi" }, { id: "2", field: "note", op: "changed", value: "" }] });
    expect(c).toEqual({ match: "or", rules: [{ field: "status", op: "changedTo", value: "Đã gửi" }, { field: "note", op: "changed" }] });
    expect(condFromWire(c).match).toBe("or");
    expect(condToWire({ match: "and", rows: [] })).toBeUndefined();
  });
});
