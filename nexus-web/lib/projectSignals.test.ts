// specs/073 so1a T451 (FR-091, FR-094; architecture 3.2): what the navy "Cần anh xem" band says is a pure function of the
// project rows. A field the server has not sent yet is a line that does not appear — never a guess, never an error.
import { describe, expect, it } from "vitest";
import { projectRow as row } from "@/app/dev/kit/sections/projectRows";
import type { ProjectRow } from "@/lib/api";
import { attentionItems } from "@/lib/projectSignals";

const NOW = new Date("2026-10-09T10:00:00Z");

// Every required step before "Chạy thử" is enough (screens and automation are optional).
const BUILT = { apps: 1, entities: 3, customLayouts: 0, workflows: 0, approvals: 0, roles: 2, users: 4, staffUsers: 3, reports: 1, dashboards: 0 };
const live = (over: Partial<ProjectRow> = {}) =>
  row({ stage: "live", branded: true, trialSince: "2026-09-10T00:00:00Z", deliveredAt: "2026-09-20T00:00:00Z", counts: BUILT, ...over });

describe("attentionItems", () => {
  it("says nothing for a project with no signal", () => {
    expect(attentionItems([row()], NOW)).toEqual([]);
    expect(attentionItems([], NOW)).toEqual([]);
  });

  it("error: a critical health snapshot counts the findings and opens the health check", () => {
    const r = row({ health: { status: "critical", critical: 3, warn: 1, checks: [], appsNotReady: [], checkedAt: "2026-10-09T08:30:00Z" } });
    const [it0] = attentionItems([r], NOW);
    expect(it0).toMatchObject({ level: "error", projectSlug: "pk-ban-nho", projectName: "Phòng khám Bạn Nhỏ", action: { route: "bao-tri/kham" } });
    expect(it0.text).toMatch(/^3 lỗi ở lần khám lúc /);
  });

  it("error: failed background jobs (health.ops) and a failed last backup", () => {
    const r = live({
      health: { status: "ok", critical: 0, warn: 0, checks: [], appsNotReady: [], checkedAt: null, ops: { status: "critical", checkedAt: "2026-10-09T09:00:00Z" } },
      backup: { lastReadyAt: "2026-10-09T01:00:00Z", lastFailed: true },
    });
    const items = attentionItems([r], NOW);
    expect(items.map((i) => [i.level, i.text, i.action.route])).toEqual([
      ["error", "Việc tự động lỗi trong 24 giờ qua", "van-hanh/tu-dong"],
      ["error", "Lần sao lưu gần nhất lỗi", "bao-tri/sao-luu"],
    ]);
  });

  it("warning: write lock still in force shows the end time, an expired one shows nothing", () => {
    const [w] = attentionItems([live({ maintenanceUntil: "2026-10-09T10:30:00Z" })], NOW);
    expect(w).toMatchObject({ level: "warning", action: { route: "bao-tri/sao-luu" } });
    expect(w.text).toMatch(/^Đang khoá ghi đến \d{2}:\d{2}$/);
    expect(attentionItems([live({ maintenanceUntil: "2026-10-09T09:00:00Z" })], NOW)).toEqual([]);
    expect(attentionItems([live({ maintenanceUntil: null })], NOW)).toEqual([]);
  });

  it("warning: a live project without a ready backup in 48 hours; a building one is not asked", () => {
    const stale = { lastReadyAt: "2026-10-07T09:00:00Z", lastFailed: false };
    expect(attentionItems([live({ backup: stale })], NOW).map((i) => i.text)).toEqual(["Chưa có bản sao lưu trong 48 giờ"]);
    expect(attentionItems([live({ backup: { lastReadyAt: null, lastFailed: false } })], NOW)).toHaveLength(1);
    expect(attentionItems([live({ backup: { lastReadyAt: "2026-10-08T09:00:00Z", lastFailed: false } })], NOW)).toEqual([]);
    expect(attentionItems([row({ backup: stale })], NOW)).toEqual([]);
  });

  it("warning: no usable customer owner on a live project only; undefined and building say nothing", () => {
    const [w] = attentionItems([live({ usableOwnerCount: 0 })], NOW);
    expect(w).toMatchObject({ level: "warning", text: "Chưa có chủ khách đăng nhập được", action: { route: "van-hanh/nguoi-dung" } });
    expect(attentionItems([live({ usableOwnerCount: 1 })], NOW)).toEqual([]);
    expect(attentionItems([live()], NOW)).toEqual([]);
    expect(attentionItems([row({ usableOwnerCount: 0 })], NOW)).toEqual([]);
  });

  it("warning: one line per app that is not ready for staff", () => {
    const r = live({
      health: { status: "warn", critical: 0, warn: 2, checks: [], checkedAt: null, appsNotReady: [{ appId: "a1", name: "Bán hàng", reasons: ["no_layout"] }, { appId: "a2", name: "Kho", reasons: [] }] },
    });
    expect(attentionItems([r], NOW).map((i) => [i.level, i.text, i.action.route])).toEqual([
      ["warning", "App Bán hàng chưa sẵn sàng cho nhân viên", "bao-tri/kham"],
      ["warning", "App Kho chưa sẵn sàng cho nhân viên", "bao-tri/kham"],
    ]);
  });

  it("info: built but not on trial opens Chạy thử; on trial and ready opens Giao cho khách", () => {
    const built = row({ counts: BUILT, branded: true });
    expect(attentionItems([built], NOW)).toEqual([
      expect.objectContaining({ level: "info", text: "Dựng đủ; chưa chạy thử.", action: expect.objectContaining({ label: "Mở Chạy thử", route: "dung/chay-thu" }) }),
    ]);
    const trial = row({ stage: "trial", trialSince: "2026-10-01T00:00:00Z", counts: BUILT, branded: true });
    expect(attentionItems([trial], NOW)).toEqual([
      expect.objectContaining({ level: "info", text: "Sẵn sàng giao.", action: expect.objectContaining({ label: "Giao cho khách", route: "dung/giao-cho-khach" }) }),
    ]);
    // one required step short (not branded) ⇒ nothing, and the Chạy thử switch is never in this band
    expect(attentionItems([row({ counts: BUILT, branded: false })], NOW)).toEqual([]);
    expect(attentionItems([row({ stage: "trial", trialSince: "2026-10-01T00:00:00Z", counts: { ...BUILT, entities: 0 }, branded: true })], NOW)).toEqual([]);
  });

  it("orders by severity across projects; same level keeps the order of the rows", () => {
    const a = row({ slug: "a", name: "A", counts: BUILT, branded: true }); // info
    const b = live({ slug: "b", name: "B", usableOwnerCount: 0 }); // warning
    const c = live({ slug: "c", name: "C", backup: { lastReadyAt: null, lastFailed: true } }); // error + warning
    const d = live({ slug: "d", name: "D", usableOwnerCount: 0 }); // warning
    expect(attentionItems([a, b, c, d], NOW).map((i) => `${i.level}:${i.projectSlug}`)).toEqual([
      "error:c",
      "warning:b",
      "warning:c",
      "warning:d",
      "info:a",
    ]);
  });

  it("does not mutate its input", () => {
    const rows = [live({ usableOwnerCount: 0 })];
    const copy = JSON.stringify(rows);
    attentionItems(rows, NOW);
    expect(JSON.stringify(rows)).toBe(copy);
  });
});
