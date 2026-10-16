// lib/ruleLastRun.test.ts — #337: the red mark on a rule whose NEWEST run failed.
import { describe, it, expect } from "vitest";
import { chunkIds, newestRunByWorkflow, failedWorkflowIds } from "./ruleLastRun";
import type { WorkflowRun } from "./workflowRuns";

const run = (id: string, workflowId: string, status: string) => ({ id, workflowId, status }) as WorkflowRun;

describe("newestRunByWorkflow", () => {
  it("keeps the FIRST row per rule (the page is newest-first)", () => {
    const m = newestRunByWorkflow([run("r1", "a", "success"), run("r2", "a", "failed"), run("r3", "b", "failed")]);
    expect(m.get("a")?.id).toBe("r1");
    expect(m.get("b")?.id).toBe("r3");
    expect(m.has("c")).toBe(false);
  });
});

describe("failedWorkflowIds", () => {
  it("marks a rule only when its newest run failed; an older failure behind a success has no mark", () => {
    const f = failedWorkflowIds([run("r1", "a", "success"), run("r2", "a", "failed"), run("r3", "b", "failed")]);
    expect([...f.keys()]).toEqual(["b"]);
    expect(f.get("b")).toBe("r3");
  });
  it("does not mark blocked/skipped/running, nor a rule absent from the page", () => {
    expect(failedWorkflowIds([run("r1", "a", "blocked"), run("r2", "b", "skipped"), run("r3", "c", "running")]).size).toBe(0);
  });
});

describe("chunkIds", () => {
  it("splits into groups of at most 100 ids", () => {
    const ids = Array.from({ length: 205 }, (_, i) => `w${i}`);
    expect(chunkIds(ids).map((c) => c.length)).toEqual([100, 100, 5]);
    expect(chunkIds([])).toEqual([]);
  });
});
