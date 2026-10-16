// lib/approvalDetail.test.ts — W6: PascalCase/camelCase adapter + inbox filter tests.
import { describe, it, expect } from "vitest";
import {
  normalizeHistory,
  fromDetailPayload,
  fromPendingItem,
  isAwaitingUser,
  canDecide,
  canRecall,
  approvalStatusMeta,
  historyActionMeta,
  fmtSLA,
} from "./approvalDetail";

describe("normalizeHistory", () => {
  it("accepts both `comment` (detail endpoint) and `note` (pending endpoint)", () => {
    const h = normalizeHistory([
      { step: 1, action: "approved", actor: "u1", comment: "ok", at: "2026-07-01T00:00:00Z" },
      { step: 2, action: "rejected", actor: "u2", note: "sai số", at: "2026-07-02T00:00:00Z" },
      { step: 3, action: "approved", actor: "u3", at: "2026-07-03T00:00:00Z" },
    ]);
    expect(h[0].comment).toBe("ok");
    expect(h[1].comment).toBe("sai số");
    expect(h[2].comment).toBeUndefined();
  });
  it("tolerates garbage input", () => {
    expect(normalizeHistory(null)).toEqual([]);
    expect(normalizeHistory("x")).toEqual([]);
    expect(normalizeHistory([null, 42, { step: "2", action: "approved", actor: "u", at: "t" }])).toEqual([
      { step: 2, action: "approved", actor: "u", comment: undefined, at: "t" },
    ]);
  });
});

describe("fromDetailPayload (camelCase, §9.5)", () => {
  const payload = {
    id: "i1",
    processId: "p1",
    entityName: "deals",
    recordId: "r1",
    currentStep: 1,
    status: "pending",
    currentApproverType: "user",
    currentApproverId: "u9",
    slaDueAt: "2026-07-04T09:00:00Z",
    escalated: false,
    history: [{ step: 0, action: "approved", actor: "u1", comment: "ok", at: "t0" }],
    createdBy: "u0",
    createdAt: "t-1",
    updatedAt: "t-1",
    process: {
      id: "p1",
      name: "Duyệt chi",
      entityName: "deals",
      trigger: "after_create",
      steps: [{ approverType: "user", approverId: "u9", slaSeconds: 3600 }],
      isActive: true,
    },
  };
  it("maps the full envelope including nested process", () => {
    const a = fromDetailPayload(payload);
    expect(a.id).toBe("i1");
    expect(a.entityName).toBe("deals");
    expect(a.process?.name).toBe("Duyệt chi");
    expect(a.process?.steps).toHaveLength(1);
    expect(a.history[0].comment).toBe("ok");
  });
  it("process=null when the process was deleted after arming", () => {
    const a = fromDetailPayload({ ...payload, process: null });
    expect(a.process).toBeNull();
  });
  it("process.deleted=true keeps the pinned steps of a deleted process (§9.5)", () => {
    const a = fromDetailPayload({
      ...payload,
      process: { id: "p1", entityName: "deals", name: "", trigger: "", steps: payload.process.steps, isActive: false, deleted: true },
    });
    expect(a.process?.deleted).toBe(true);
    expect(a.process?.steps).toHaveLength(1);
    expect(fromDetailPayload(payload).process?.deleted).toBe(false);
  });
  it("process.pinned=true is carried through for a LIVE but edited process (§9.5)", () => {
    // The engine walks the pinned steps whether or not the live row still exists, so the flag that
    // tells the screen "this chain is frozen" must survive the mapper on the live-row path too.
    const a = fromDetailPayload({
      ...payload,
      process: { ...payload.process, pinned: true },
    });
    expect(a.process?.pinned).toBe(true);
    expect(a.process?.deleted).toBe(false);
    expect(a.process?.name).toBe("Duyệt chi");
    expect(fromDetailPayload(payload).process?.pinned).toBe(false);
  });
  it("carries each step's pinned name through the mapper (#69)", () => {
    // The BE serves the pinned bpm.Step with `name`; dropping it here is what left the step table
    // printing "vai trò #<uuid8>" on the one screen where the step name is the only label left.
    const a = fromDetailPayload({
      ...payload,
      process: { ...payload.process, steps: [{ name: "Giám đốc duyệt", approverType: "role", approverId: "r1" }, { approverType: "user", approverId: "u9" }] },
    });
    expect(a.process?.steps.map((s) => s.name)).toEqual(["Giám đốc duyệt", undefined]);
  });
  it("recordId null-safe", () => {
    const a = fromDetailPayload({ ...payload, recordId: null });
    expect(a.recordId).toBeNull();
  });
});

describe("fromPendingItem (PascalCase deviation)", () => {
  const raw = {
    ID: "i2",
    TenantID: "t1",
    EntityName: "crm_leads",
    RecordID: "r2",
    CurrentStep: 2,
    Status: "pending",
    CurrentApproverType: "role",
    CurrentApproverID: "role-1",
    SLADueAt: "2026-07-04T09:00:00Z",
    Escalated: true,
    History: [{ step: 1, action: "approved", actor: "u1", note: "bước 1 ok", at: "t1" }],
  };
  it("converts PascalCase → the shared camelCase shape", () => {
    const a = fromPendingItem(raw);
    expect(a.id).toBe("i2");
    expect(a.entityName).toBe("crm_leads");
    expect(a.recordId).toBe("r2");
    expect(a.currentStep).toBe(2);
    expect(a.currentApproverType).toBe("role");
    expect(a.currentApproverId).toBe("role-1");
    expect(a.escalated).toBe(true);
    expect(a.history[0].comment).toBe("bước 1 ok");
    expect(a.process).toBeNull();
  });
  it("defaults status to pending when absent (pending queue implies it)", () => {
    const a = fromPendingItem({ ID: "x", EntityName: "e", CurrentStep: 1 });
    expect(a.status).toBe("pending");
  });
});

describe("isAwaitingUser", () => {
  const me = { id: "u9", roleId: "role-1" };
  it("user-target matches by user id", () => {
    expect(
      isAwaitingUser({ status: "pending", currentApproverType: "user", currentApproverId: "u9" }, me),
    ).toBe(true);
    expect(
      isAwaitingUser({ status: "pending", currentApproverType: "user", currentApproverId: "other" }, me),
    ).toBe(false);
  });
  it("role-target matches by the role I hold", () => {
    expect(
      isAwaitingUser({ status: "pending", currentApproverType: "role", currentApproverId: "role-1" }, me),
    ).toBe(true);
    expect(
      isAwaitingUser({ status: "pending", currentApproverType: "role", currentApproverId: "role-2" }, me),
    ).toBe(false);
  });
  it("non-pending or missing approver never matches", () => {
    expect(
      isAwaitingUser({ status: "approved", currentApproverType: "user", currentApproverId: "u9" }, me),
    ).toBe(false);
    expect(isAwaitingUser({ status: "pending" }, me)).toBe(false);
  });
});

// specs/043 S4: on a step held by several people current_approver_* is EMPTY, so the screen takes
// the server's own answer (awaitsMe, from the seats) — without it duyet1 saw the approval in the
// inbox but never got a Duyệt / Từ chối verb.
describe("awaitsMe (server answer) beats the empty single-holder projection", () => {
  const council = { status: "pending", awaitsMe: true } as const;
  it("a seat holder is awaited and may decide; one who is not, is not", () => {
    expect(isAwaitingUser(council, { id: "u1" })).toBe(true);
    expect(canDecide(council, { id: "u1" })).toBe(true);
    expect(isAwaitingUser({ status: "pending", awaitsMe: false }, { id: "u1" })).toBe(false);
    expect(canDecide({ status: "pending", awaitsMe: false }, { id: "u1" })).toBe(false);
  });
  it("the server saying no wins over a projection that would say yes", () => {
    expect(isAwaitingUser({ status: "pending", currentApproverType: "user", currentApproverId: "u1", awaitsMe: false }, { id: "u1" })).toBe(false);
  });
  it("a finished approval awaits nobody, whatever the flag says", () => {
    expect(isAwaitingUser({ status: "approved", awaitsMe: true }, { id: "u1" })).toBe(false);
  });
  it("both payload dialects carry it through", () => {
    expect(fromPendingItem({ ID: "a1", Status: "pending", awaitsMe: true }).awaitsMe).toBe(true);
    expect(fromDetailPayload({ id: "a1", status: "pending", awaitsMe: false }).awaitsMe).toBe(false);
    expect(fromPendingItem({ ID: "a1", Status: "pending" }).awaitsMe).toBeUndefined();
  });
});

// specs/038 "Nợ còn mở" (#64): the screen offers a verb only to the person the server lets use it.
// Mirrors bpm.CanDecide (decide + delegate) and Requester.Recall — admin passes both.
describe("canDecide / canRecall", () => {
  const pending = { status: "pending", currentApproverType: "user", currentApproverId: "u1", createdBy: "u9" };
  it("the current approver decides, a colleague does not, an admin always can", () => {
    expect(canDecide(pending, { id: "u1" })).toBe(true);
    expect(canDecide(pending, { id: "u7" })).toBe(false);
    expect(canDecide(pending, { id: "u7", isAdmin: true })).toBe(true);
    expect(canDecide({ ...pending, currentApproverType: "role", currentApproverId: "r1" }, { id: "u7", roleId: "r1" })).toBe(true);
  });
  it("only the sender (or an admin) recalls", () => {
    expect(canRecall(pending, { id: "u9" })).toBe(true);
    expect(canRecall(pending, { id: "u1" })).toBe(false);
    expect(canRecall(pending, { id: "u1", isAdmin: true })).toBe(true);
    expect(canRecall({ ...pending, createdBy: undefined }, { id: "u1" })).toBe(false);
  });
  it("nothing is offered on a finished approval or before we know who is looking", () => {
    expect(canDecide({ ...pending, status: "approved" }, { id: "u1", isAdmin: true })).toBe(false);
    expect(canRecall({ ...pending, status: "rejected" }, { id: "u9" })).toBe(false);
    expect(canDecide(pending, null)).toBe(false);
    expect(canRecall(pending, null)).toBe(false);
  });
});

describe("display meta", () => {
  it("approvalStatusMeta", () => {
    expect(approvalStatusMeta("pending")).toEqual({ tone: "wn", label: "Đang chờ duyệt" });
    expect(approvalStatusMeta("approved")).toEqual({ tone: "ok", label: "Đã duyệt" });
    expect(approvalStatusMeta("rejected")).toEqual({ tone: "dg", label: "Đã từ chối" });
    expect(approvalStatusMeta("odd").tone).toBe("neu");
  });
  it("historyActionMeta", () => {
    expect(historyActionMeta("approved").tone).toBe("ok");
    expect(historyActionMeta("rejected").tone).toBe("dg");
    expect(historyActionMeta("escalated").tone).toBe("wn");
    expect(historyActionMeta("submitted").tone).toBe("ac");
    expect(historyActionMeta("completed")).toEqual({ tone: "ok", label: "Đã hoàn thành" });
    expect(historyActionMeta("???").tone).toBe("neu");
  });
});

describe("fromAnyInstance", () => {
  it("routes PascalCase (pending/per-record endpoints) and camelCase (detail) correctly", async () => {
    const { fromAnyInstance } = await import("./approvalDetail");
    const pas = fromAnyInstance({ ID: "a", EntityName: "e", CurrentStep: 0, Status: "pending" });
    expect(pas.id).toBe("a");
    expect(pas.currentStep).toBe(0);
    const cam = fromAnyInstance({ id: "b", entityName: "e", currentStep: 1, status: "approved" });
    expect(cam.id).toBe("b");
    expect(cam.status).toBe("approved");
  });
});

describe("stepLabel", () => {
  it("renders the 0-indexed backend step as 1-based Vietnamese", async () => {
    const { stepLabel } = await import("./approvalDetail");
    expect(stepLabel(0)).toBe("Bước 1");
    expect(stepLabel(2)).toBe("Bước 3");
  });
});

describe("fmtSLA", () => {
  const NOW = new Date("2026-07-03T12:00:00Z").getTime();
  it("counts down in minutes / hours / days", () => {
    expect(fmtSLA("2026-07-03T12:30:00Z", NOW)).toEqual({ label: "30 phút còn lại", overdue: false });
    expect(fmtSLA("2026-07-03T15:00:00Z", NOW)).toEqual({ label: "3 giờ còn lại", overdue: false });
    expect(fmtSLA("2026-07-06T12:00:00Z", NOW)).toEqual({ label: "3 ngày còn lại", overdue: false });
  });
  it("flags overdue", () => {
    expect(fmtSLA("2026-07-03T11:00:00Z", NOW)).toEqual({ label: "Quá hạn", overdue: true });
  });
  it("no deadline → em dash", () => {
    expect(fmtSLA(undefined, NOW)).toEqual({ label: "—", overdue: false });
  });
});

// specs/043 S1 (#39): the deadline's BASIS reaches both screens through the one mapper, and every
// basis has a human label — a wall-clock fallback never reads like working hours.
describe("slaBasis", () => {
  it("reads sla.basis from the detail payload and slaBasis from a pending row", () => {
    const d = fromDetailPayload({ id: "a", entityName: "e", currentStep: 0, status: "pending",
      slaDueAt: "2026-10-05T04:00:00Z", sla: { dueAt: "2026-10-05T04:00:00Z", basis: "business" } });
    expect(d.slaBasis).toBe("business");
    const p = fromPendingItem({ ID: "b", EntityName: "e", CurrentStep: 0, SLADueAt: "2026-10-05T04:00:00Z", slaBasis: "wall_no_calendar" });
    expect(p.slaBasis).toBe("wall_no_calendar");
    expect(fromDetailPayload({ id: "c", entityName: "e", currentStep: 0, status: "pending" }).slaBasis).toBeUndefined();
  });
  it("labels every basis, and says nothing when there is none", async () => {
    const { slaBasisLabel } = await import("./approvalDetail");
    expect(slaBasisLabel("business")).toBe("giờ làm việc");
    expect(slaBasisLabel("wall")).toBe("giờ đồng hồ");
    expect(slaBasisLabel("wall_no_calendar")).toBe("giờ đồng hồ — tổ chức chưa khai lịch làm việc");
    expect(slaBasisLabel("wall_calendar_error")).toBe("tạm tính giờ đồng hồ — không đọc được lịch làm việc");
    expect(slaBasisLabel(undefined)).toBeNull();
    expect(slaBasisLabel("")).toBeNull();
  });
});

// specs/043 S2 (#40): "hồ sơ này duyệt mất bao lâu" — the server measures, the screen says it in
// words, and a wall-clock figure never reads as working hours.
describe("durations", () => {
  it("reads the totals and their basis from the detail payload", async () => {
    const d = fromDetailPayload({ id: "a", entityName: "e", currentStep: 0, status: "approved",
      durations: { basis: "business", totalBusinessSeconds: 10800, totalWallSeconds: 237600,
        perNode: [{ step: 0, node: "s1", businessSeconds: 10800, wallSeconds: 237600 }] } });
    expect(d.durations).toEqual({ basis: "business", totalBusinessSeconds: 10800, totalWallSeconds: 237600 });
    const bare = fromDetailPayload({ id: "b", entityName: "e", currentStep: 0, status: "approved" });
    expect(bare.durations).toBeUndefined();
    // No business figure on the wire (wall basis) stays absent — never read as 0 working hours.
    const wall = fromDetailPayload({ id: "c", entityName: "e", currentStep: 0, status: "approved",
      durations: { basis: "wall_no_calendar", totalWallSeconds: 237600, perNode: [] } });
    expect(wall.durations?.totalBusinessSeconds).toBeUndefined();
  });

  it("says it in words: done vs still waiting, working time vs a labelled wall clock", async () => {
    const { elapsedLabel } = await import("./approvalDetail");
    const biz = (s: number) => ({ basis: "business", totalBusinessSeconds: s, totalWallSeconds: 999_999 });
    expect(elapsedLabel(biz(10800), "approved")).toBe("Mất 3 giờ làm việc");
    expect(elapsedLabel(biz(12059), "pending")).toBe("Đã chờ 3 giờ 20 phút làm việc");
    expect(elapsedLabel(biz(2700), "rejected")).toBe("Mất 45 phút làm việc");
    expect(elapsedLabel(biz(20), "approved")).toBe("Mất dưới 1 phút làm việc");
    expect(elapsedLabel(biz(0), "approved")).toBe("Mất 0 phút làm việc");
    expect(elapsedLabel({ basis: "wall_no_calendar", totalWallSeconds: 237600 }, "approved"))
      .toBe("Mất 66 giờ (giờ đồng hồ — tổ chức chưa khai lịch làm việc)");
    expect(elapsedLabel({ basis: "wall_calendar_error", totalWallSeconds: 3600 }, "pending"))
      .toBe("Đã chờ 1 giờ (tạm tính giờ đồng hồ — không đọc được lịch làm việc)");
    expect(elapsedLabel(undefined, "approved")).toBeNull();
    // A business basis without its figure never passes the wall number off as working hours.
    expect(elapsedLabel({ basis: "business", totalWallSeconds: 237600 }, "approved")).toBeNull();
  });

  it("keeps the step each history entry was recorded on", () => {
    const [h] = normalizeHistory([{ step: 0, action: "approved", actor: "u", node: "s1", stepName: "Trưởng phòng", at: "t" }]);
    expect(h.node).toBe("s1");
    expect(h.stepName).toBe("Trưởng phòng");
  });
});

// specs/043 S8: an approval returned to its submitter waits for "Gửi lại".
describe("awaitingRework (approval returned to the submitter)", () => {
  it("is carried from the detail payload, with the reason", () => {
    const d = fromDetailPayload({ id: "a1", status: "pending", awaitingRework: true, returnReason: "Sai đơn giá" });
    expect(d.awaitingRework).toBe(true);
    expect(d.returnReason).toBe("Sai đơn giá");
    expect(fromDetailPayload({ id: "a1", status: "pending" }).awaitingRework).toBeUndefined();
  });

  it("nobody — admin included — decides it while it is with the submitter", () => {
    const a = { status: "pending", awaitsMe: true, awaitingRework: true } as const;
    expect(canDecide(a, { id: "u1" })).toBe(false);
    expect(canDecide(a, { id: "admin", isAdmin: true })).toBe(false);
    expect(canDecide({ status: "pending", awaitsMe: true }, { id: "u1" })).toBe(true);
  });

  it("the timeline names a resubmission", () => {
    expect(historyActionMeta("resubmitted")).toEqual({ tone: "ac", label: "Đã gửi lại" });
  });
});

// specs/043 S6b: a quorum step carries its percent, so the steps list can say «đủ N%».
describe("step completion policy", () => {
  it("keeps the quorum percent beside the mode", () => {
    const d = fromDetailPayload({
      id: "a1", status: "pending",
      process: { id: "p", steps: [{ name: "Hội đồng", approvers: [{ type: "user", id: "u1" }], completion: { mode: "quorum", percent: 60 } }] },
    });
    expect(d.process?.steps[0]).toMatchObject({ completion: "quorum", completionPercent: 60 });
  });
});
