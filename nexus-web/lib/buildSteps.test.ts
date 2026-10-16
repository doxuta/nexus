// specs/073 kh1a T143: the eight build steps, derived only from one ProjectRow (architecture 6.3).
import { describe, expect, it } from "vitest";
import { buildSteps, nextStep, STEP_ROUTE, stepOfRoute, type BuildStepsRow } from "@/lib/buildSteps";
import { parseStudioPath, ROUTES, zoneOfRoute } from "@/lib/studioRoutes";

const empty: BuildStepsRow = {
  branded: false,
  trialSince: null,
  deliveredAt: null,
  counts: { entities: 0, customLayouts: 0, workflows: 0, approvals: 0, roles: 0, staffUsers: 0, reports: 0, dashboards: 0 },
};
const row = (over: Omit<Partial<BuildStepsRow>, "counts"> & { counts?: Partial<BuildStepsRow["counts"]> } = {}): BuildStepsRow => ({
  ...empty,
  ...over,
  counts: { ...empty.counts, ...over.counts },
});
const done = (r: BuildStepsRow | null, home = false) => Object.fromEntries(buildSteps(r, { home }).map((s) => [s.key, s.complete]));

describe("buildSteps", () => {
  it("a project just created: nothing is enough", () => {
    expect(done(row())).toEqual({ data: false, screens: false, automation: false, access: false, reports: false, branding: false, trial: false, deliver: false });
  });

  it("a build in progress: data and access are enough, the rest is not", () => {
    const r = row({ counts: { entities: 3, roles: 2, staffUsers: 1 } });
    expect(done(r)).toMatchObject({ data: true, access: true, screens: false, automation: false, reports: false });
  });

  it("access needs a role AND a non-admin staff member", () => {
    expect(done(row({ counts: { roles: 2, staffUsers: 0 } })).access).toBe(false);
    expect(done(row({ counts: { roles: 0, staffUsers: 2 } })).access).toBe(false);
  });

  it("automation counts rules plus approvals; reports count reports plus dashboards", () => {
    expect(done(row({ counts: { workflows: 0, approvals: 1 } })).automation).toBe(true);
    expect(done(row({ counts: { reports: 0, dashboards: 1 } })).reports).toBe(true);
  });

  it("ready to try: everything required is enough but not yet trial", () => {
    const r = row({ branded: true, counts: { entities: 1, roles: 1, staffUsers: 1, reports: 1 } });
    expect(done(r)).toMatchObject({ data: true, access: true, reports: true, branding: true, trial: false, deliver: false });
    expect(nextStep(buildSteps(r, { home: false }))).toBe("trial");
  });

  it("trial running: trial is enough, deliver is not", () => {
    const r = row({ trialSince: "2026-10-01T00:00:00Z" });
    expect(done(r)).toMatchObject({ trial: true, deliver: false });
  });

  it("delivered: trial counts as enough even when trialSince is null", () => {
    const r = row({ deliveredAt: "2026-10-05T00:00:00Z" });
    expect(done(r)).toMatchObject({ trial: true, deliver: true });
  });

  it("the sub-labels come from the dictionary and carry the numbers", () => {
    const steps = buildSteps(row({ counts: { entities: 4, workflows: 2, approvals: 1, roles: 3, staffUsers: 5, reports: 6, dashboards: 7 }, branded: true }), { home: false });
    const sub = Object.fromEntries(steps.map((s) => [s.key, s.sublabel]));
    expect(sub.data).toBe("4 bảng");
    expect(sub.automation).toBe("2 luật · 1 quy trình duyệt");
    expect(sub.access).toBe("3 vai · 5 nhân viên");
    expect(sub.reports).toBe("6 báo cáo · 7 trang tổng quan");
    expect(sub.branding).toBe("màu riêng");
    expect(buildSteps(row(), { home: false }).find((s) => s.key === "data")!.sublabel).toBe("chưa có bảng");
  });

  it("screens and automation are optional, the others are not", () => {
    const opt = Object.fromEntries(buildSteps(row(), { home: false }).map((s) => [s.key, s.optional]));
    expect(opt).toMatchObject({ screens: true, automation: true, data: false, access: false, reports: false, branding: false, trial: false, deliver: false });
  });

  it("(null, home) is the Nexus workshop: seven steps, none with a tick", () => {
    const steps = buildSteps(null, { home: true });
    expect(steps).toHaveLength(7);
    expect(steps.map((s) => s.key)).not.toContain("deliver");
    expect(steps.every((s) => s.complete === null && s.sublabel === "")).toBe(true);
  });

  it("(null, not home) is a customer project before the registry exists: eight steps, none with a tick", () => {
    const steps = buildSteps(null, { home: false });
    expect(steps).toHaveLength(8);
    expect(steps.every((s) => s.complete === null)).toBe(true);
  });

  it("a real row with home:true still drops the deliver step", () => {
    expect(buildSteps(row(), { home: true })).toHaveLength(7);
  });
});

describe("nextStep", () => {
  it("skips optional steps and steps that are already enough", () => {
    const steps = buildSteps(row({ counts: { entities: 1 } }), { home: false });
    expect(nextStep(steps)).toBe("access"); // screens + automation are optional
  });

  it("is null when nothing is known or everything is enough", () => {
    expect(nextStep(buildSteps(null, { home: false }))).toBeNull();
    const full = row({ deliveredAt: "2026-10-05T00:00:00Z", branded: true, counts: { entities: 1, roles: 1, staffUsers: 1, reports: 1 } });
    expect(nextStep(buildSteps(full, { home: false }))).toBeNull();
  });
});

// specs/073 kh1b1 T154: where a step opens, and which step a page belongs to (the step bar's current mark).
describe("step routes", () => {
  it("every step opens a declared route, and that route belongs to the same step", () => {
    for (const [key, id] of Object.entries(STEP_ROUTE)) {
      const route = ROUTES.find((r) => r.id === id);
      expect(route, id).toBeDefined();
      expect(stepOfRoute(route!)).toBe(key);
    }
    expect(stepOfRoute(parseStudioPath("/studio/du-an/a/dung/tu-dong/duyet")!.route)).toBe("automation");
    expect(stepOfRoute(parseStudioPath("/studio/du-an/a/van-hanh/suc-khoe")!.route)).toBeNull();
    expect(stepOfRoute(null)).toBeNull();
  });

  it("zones: Cài đặt sits in Bảo trì, an unknown page in Dựng", () => {
    expect(zoneOfRoute(parseStudioPath("/studio/du-an/a/van-hanh/suc-khoe")!.route)).toBe("van-hanh");
    expect(zoneOfRoute(parseStudioPath("/studio/du-an/a/cai-dat")!.route)).toBe("bao-tri");
    expect(zoneOfRoute(parseStudioPath("/studio/du-an/a/dung/du-lieu")!.route)).toBe("dung");
    expect(zoneOfRoute(null)).toBe("dung");
  });
});
