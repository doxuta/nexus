import { describe, it, expect } from "vitest";
import type { WorkItem } from "@/lib/api";
import { workItemActions, slaState, dueText } from "./workItems";

const wi = (o: Partial<WorkItem> = {}): WorkItem => ({
  id: "w1", instanceId: "i1", processName: "P", nodeName: "N", kind: "approval", status: "open",
  entityName: "e", entityLabel: "E", recordId: "r", recordLabel: "R", assigneeType: "user", assigneeName: "A",
  claimedByMe: false, dueAt: null, overdue: false, paused: false, createdAt: "2026-10-01T00:00:00Z", ...o,
});
const NOW = Date.parse("2026-10-05T04:00:00Z");

describe("workItemActions", () => {
  it("named approver decides, cannot claim/release", () => {
    expect(workItemActions(wi(), "mine")).toEqual(["approve", "reject"]);
  });
  it("unclaimed pool seat offers only Nhận", () => {
    expect(workItemActions(wi({ assigneeType: "role" }), "pool")).toEqual(["claim"]);
  });
  it("claimed-by-me pool approval: decide or give back", () => {
    expect(workItemActions(wi({ assigneeType: "role", claimedBy: "u1", claimedByMe: true }), "mine")).toEqual(["approve", "reject", "release"]);
  });
  it("someone else's claim: nothing", () => {
    expect(workItemActions(wi({ assigneeType: "role", claimedBy: "u2" }), "pool")).toEqual([]);
  });
  it("task / rework complete; named task cannot release", () => {
    expect(workItemActions(wi({ kind: "task" }), "mine")).toEqual(["complete"]);
    expect(workItemActions(wi({ kind: "rework", assigneeType: "queue", claimedByMe: true, claimedBy: "u1" }), "mine")).toEqual(["complete", "release"]);
  });
  it("done tab: no verbs", () => {
    expect(workItemActions(wi({ status: "done" }), "done")).toEqual([]);
  });
});

describe("slaState / dueText", () => {
  it("states", () => {
    expect(slaState(wi({ dueAt: "2026-10-05T03:00:00Z" }), NOW)).toBe("overdue");
    expect(slaState(wi({ dueAt: "2026-10-05T05:00:00Z" }), NOW)).toBe("soon");
    expect(slaState(wi({ dueAt: "2026-10-09T05:00:00Z" }), NOW)).toBe("ok");
    expect(slaState(wi({ dueAt: "2026-10-05T03:00:00Z", paused: true }), NOW)).toBe("paused");
    expect(slaState(wi(), NOW)).toBeNull();
    expect(slaState(wi({ status: "done", dueAt: "2026-10-05T03:00:00Z" }), NOW)).toBeNull();
  });
  it("due text names the basis", () => {
    expect(dueText(wi({ dueAt: "2026-10-05T04:00:00Z", dueBasis: "business" }), NOW - 3_600_000)).toMatch(/\(giờ làm việc\) · 1 giờ còn lại/);
    expect(dueText(wi())).toBe("—");
  });
});
