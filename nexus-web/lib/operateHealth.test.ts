// specs/078 lát u7-suc-khoe T162: the pure mapping from GET /admin/health to the lede and the list of signals.
import { describe, expect, it } from "vitest";
import type { TenantHealth } from "@/lib/admin";
import { healthLede, healthSignals } from "./operateHealth";

const NOW = Date.parse("2026-10-14T10:00:00Z");
const base = (over: Partial<TenantHealth> = {}): TenantHealth => ({
  status: "ok",
  generatedAt: "2026-10-14T10:00:00Z",
  queueEnabled: true,
  jobs: { failed7d: [], stuck: 0 },
  workflowRuns: { failed24h: 0, failed7d: 0, topFailing: [] },
  emails: { failed24h: 0, failed7d: 0, unresentFailed: 0 },
  inboundHooks: { failed24h: 0, byHook: [] },
  doctor: { status: "ok", critical: 0, warn: 0, checkedAt: "2026-10-14T08:00:00Z" },
  ...over,
});
// A block the server left out (older build, partial response).
const without = (k: keyof TenantHealth): TenantHealth => {
  const h = base() as unknown as Record<string, unknown>;
  delete h[k];
  return h as unknown as TenantHealth;
};

describe("healthLede", () => {
  it("everything present and zero: clean", () => {
    expect(healthLede(base(), "northwind")).toMatchObject({ clean: true, n: 0, m: 0, unknown: [], href: "/studio/du-an/northwind/van-hanh/tu-dong" });
  });
  it("n = rule runs + emails + inbound failed in 24h; jobs only as a 7-day clause", () => {
    const h = base({
      workflowRuns: { failed24h: 2, failed7d: 5, topFailing: [] },
      emails: { failed24h: 1, failed7d: 1, unresentFailed: 1 },
      inboundHooks: { failed24h: 3, byHook: [] },
      jobs: { failed7d: [{ jobType: "outbound_webhook", count: 4, lastAt: "2026-10-12T00:00:00Z" }, { jobType: "email_alert", count: 1, lastAt: "2026-10-13T00:00:00Z" }], stuck: 0 },
    });
    expect(healthLede(h, "northwind")).toMatchObject({ clean: false, n: 6, m: 5, unknown: [] });
  });
  it("only old job failures: still no 24h failure, but the 7-day clause is kept", () => {
    const h = base({ jobs: { failed7d: [{ jobType: "x", count: 2, lastAt: "2026-10-10T00:00:00Z" }], stuck: 0 } });
    expect(healthLede(h, "northwind")).toMatchObject({ clean: true, n: 0, m: 2 });
  });
  it.each(["workflowRuns", "emails", "inboundHooks", "jobs"] as const)("block %s missing: never clean, names the source (FR-071)", (k) => {
    const l = healthLede(without(k), "northwind");
    expect(l.clean).toBe(false);
    expect(l.unknown).toHaveLength(1);
  });
  it("no data at all: never clean", () => {
    expect(healthLede(null, "northwind").clean).toBe(false);
    expect(healthLede(undefined, "northwind").unknown.length).toBeGreaterThan(0);
  });
  it("a null block counts as missing", () => {
    expect(healthLede({ ...base(), emails: null } as unknown as TenantHealth, "northwind").clean).toBe(false);
  });
});

describe("healthSignals", () => {
  it("all fine and every block present: empty list", () => {
    expect(healthSignals(base(), "northwind", NOW)).toEqual([]);
  });
  it("never examined by Doctor: one 'Chưa khám' signal, level unknown", () => {
    const s = healthSignals(base({ doctor: { status: "unknown", critical: 0, warn: 0, checkedAt: null } }), "northwind", NOW);
    expect(s).toHaveLength(1);
    expect(s[0]).toMatchObject({ level: "unknown", text: expect.stringContaining("Chưa khám") });
    expect(s[0].action).toMatchObject({ href: "/studio/du-an/northwind/bao-tri/kham" });
  });
  it("orders critical before warn before unknown; each has one text and one action", () => {
    const s = healthSignals(
      base({
        jobs: { failed7d: [{ jobType: "outbound_webhook", count: 3, lastAt: "2026-10-14T09:00:00Z" }], stuck: 2 },
        emails: { failed24h: 1, failed7d: 1, unresentFailed: 1 },
        doctor: { status: "unknown", critical: 0, warn: 0, checkedAt: null },
      }),
      "northwind",
      NOW,
    );
    expect(s.map((x) => x.level)).toEqual(["critical", "warn", "warn", "unknown"]);
    expect(s[0].text).toContain("2");
    for (const x of s) expect(x.action.label).toBeTruthy();
    const mail = s.find((x) => x.id === "emails");
    expect(mail?.action.href).toBe("/studio/du-an/northwind/van-hanh/tu-dong/email?trang-thai=failed");
  });
  it("names the rule that fails most and links to its runs", () => {
    const s = healthSignals(
      base({ workflowRuns: { failed24h: 4, failed7d: 9, topFailing: [{ workflowId: "w1", name: "Gửi đơn", failed: 4, lastAt: "2026-10-14T09:00:00Z", lastError: "x" }] } }),
      "northwind",
      NOW,
    );
    expect(s).toHaveLength(1);
    expect(s[0].text).toContain("Gửi đơn");
    expect(s[0].action.href).toContain("/van-hanh/tu-dong/lan-chay");
    expect(s[0].action.href).toContain("luat=w1");
  });
  it("critical Doctor is a critical signal; warn Doctor a warn one", () => {
    expect(healthSignals(base({ doctor: { status: "critical", critical: 2, warn: 1, checkedAt: "2026-10-14T08:00:00Z" } }), "northwind", NOW)[0].level).toBe("critical");
    expect(healthSignals(base({ doctor: { status: "warn", critical: 0, warn: 1, checkedAt: "2026-10-14T08:00:00Z" } }), "northwind", NOW)[0].level).toBe("warn");
  });
  it("missing block: an unknown signal with a retry action, not an empty list (FR-103)", () => {
    const s = healthSignals(without("emails"), "northwind", NOW);
    expect(s).toHaveLength(1);
    expect(s[0]).toMatchObject({ level: "unknown", text: expect.stringContaining("Chưa đọc được số liệu") });
    expect(s[0].action).toMatchObject({ label: "Thử lại", retry: true });
  });
  it("inbound hook failing: one signal per hook with the hook filter", () => {
    const s = healthSignals(base({ inboundHooks: { failed24h: 2, byHook: [{ hookId: "h1", name: "Cổng A", failed: 2, lastAt: "2026-10-14T09:00:00Z" }] } }), "northwind", NOW);
    expect(s[0].text).toContain("Cổng A");
    expect(s[0].action.href).toContain("cong=h1");
  });
});
