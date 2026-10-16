// approvalPath — which steps/branches an approval actually went through (specs/043 US6, FE-BPM-DETAIL T215).
import { describe, it, expect } from "vitest";
import { approvalPath } from "./approvalPath";
import type { ApprovalDetailData, ApprovalProcessStep } from "./approvalDetail";

const node = (id: string, kind: string, extra: Partial<ApprovalProcessStep> = {}): ApprovalProcessStep => ({
  id, kind, name: id, approverType: "user", approverId: "u", ...extra,
});

// manager → gw (>50tr: director | default: fin) → end
const graph: ApprovalProcessStep[] = [
  node("manager", "approval", { next: "gw" }),
  node("gw", "gateway", { branches: [{ name: "Trên 50 triệu", to: "director" }], default: "fin" }),
  node("director", "approval", { next: "end" }),
  node("fin", "approval", { next: "end" }),
  node("end", "end"),
];

const base = (over: Partial<ApprovalDetailData>): ApprovalDetailData => ({
  id: "a", entityName: "crm_quotes", recordId: "r", currentStep: 0, status: "pending", history: [],
  process: { id: "p", name: "P", entityName: "crm_quotes", trigger: "manual", steps: graph }, ...over,
});

describe("approvalPath", () => {
  it("branch taken is done, the other arm is skipped, machine nodes are hidden", () => {
    const d = base({
      currentStep: 2, status: "pending", gateway: { gw: "Trên 50 triệu" },
      history: [{ step: 0, action: "approve", actor: "u", node: "manager", at: "t" }],
    });
    const p = approvalPath(d);
    expect(p.map((s) => [s.id, s.state])).toEqual([
      ["manager", "done"], ["gw", "done"], ["director", "current"], ["fin", "skipped"],
    ]);
    expect(p[1].branch).toBe("Trên 50 triệu");
  });

  it("before the gateway: later arms are todo, not skipped", () => {
    const p = approvalPath(base({ currentStep: 0 }));
    expect(p.map((s) => s.state)).toEqual(["current", "todo", "todo", "todo"]);
    expect(p[1].branch).toBeUndefined();
  });

  it("rejected: where it stopped is rejected, never-reached steps are skipped", () => {
    const d = base({
      currentStep: 0, status: "rejected",
      history: [{ step: 0, action: "reject", actor: "u", node: "manager", at: "t" }],
    });
    expect(approvalPath(d).map((s) => s.state)).toEqual(["rejected", "skipped", "skipped", "skipped"]);
  });

  it("linear pin without ids/kinds: earlier steps done, later todo, all done when approved", () => {
    const lin = [0, 1, 2].map((i) => ({ name: `s${i}`, approverType: "user", approverId: "u" }));
    const proc = { id: "p", name: "P", entityName: "e", trigger: "manual", steps: lin };
    expect(approvalPath(base({ process: proc, currentStep: 1 })).map((s) => s.state)).toEqual(["done", "current", "todo"]);
    expect(approvalPath(base({ process: proc, currentStep: 2, status: "approved" })).map((s) => s.state)).toEqual(["done", "done", "done"]);
  });

  it("no process → empty", () => {
    expect(approvalPath(base({ process: null }))).toEqual([]);
  });
});
