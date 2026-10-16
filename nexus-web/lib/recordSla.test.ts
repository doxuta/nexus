import { describe, it, expect } from "vitest";
import { fromSlaItem, slaState, slaBasisText } from "./recordSla";

const NOW = Date.parse("2026-10-15T08:00:00Z");
const base = { id: "i1", stepName: "Phản hồi đầu tiên", status: "waiting", sla: { dueAt: "2026-10-15T10:00:00Z", basis: "business", paused: false, met: false, breached: false } };

describe("recordSla", () => {
  it("maps the server item and tolerates missing sla", () => {
    expect(fromSlaItem({ id: "x", stepName: "B" }).sla.paused).toBe(false);
    expect(fromSlaItem(base).sla.dueAt).toBe("2026-10-15T10:00:00Z");
  });
  it("picks the state: met > breached > paused > overdue > running", () => {
    expect(slaState(fromSlaItem(base), NOW)).toEqual({ kind: "running", theme: "success" });
    expect(slaState(fromSlaItem({ ...base, sla: { ...base.sla, paused: true, dueAt: null } }), NOW).kind).toBe("paused");
    expect(slaState(fromSlaItem({ ...base, sla: { ...base.sla, dueAt: "2026-10-15T07:00:00Z" } }), NOW).kind).toBe("overdue");
    expect(slaState(fromSlaItem({ ...base, sla: { ...base.sla, breached: true } }), NOW).kind).toBe("overdue");
    expect(slaState(fromSlaItem({ ...base, sla: { ...base.sla, met: true, breached: true } }), NOW).kind).toBe("met");
  });
  it("words the basis without engine codes", () => {
    expect(slaBasisText("business")).toBe("giờ làm việc");
    expect(slaBasisText("wall_no_calendar")).toContain("giờ đồng hồ");
    expect(slaBasisText("wall_no_calendar")).not.toContain("wall");
    expect(slaBasisText(undefined)).toBe("");
  });
});
