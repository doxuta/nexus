import { describe, expect, it } from "vitest";
import { mergeRecordCounts, operateListHref, usageRange } from "./operateData";
import { parseStudioPath, studioHref } from "./studioRoutes";

describe("operateListHref (specs/078 T054a)", () => {
  it.each([
    ["/crm_accounts/import", "nhap"],
    ["/crm_accounts/batch", "tao-nhieu"],
    ["/crm_accounts/deleted", "thung-rac"],
  ])("%s goes to the Studio route %s", (href, tail) => {
    expect(operateListHref("acme", "crm_accounts", href)).toBe(`/studio/du-an/acme/van-hanh/du-lieu/crm_accounts/${tail}`);
  });
  it("refuses unknown hrefs and other tables", () => {
    expect(operateListHref("acme", "crm_accounts", "/crm_accounts/whatever")).toBeNull();
    expect(operateListHref("acme", "crm_accounts", "/crm_leads/import")).toBeNull();
    expect(operateListHref("acme", "crm_accounts", "https://evil.test/crm_accounts/import")).toBeNull();
  });
});

describe("list URL keys on van-hanh/du-lieu/<bang> (specs/078 T054)", () => {
  it("studioHref keeps q/sort/order/view and field[op], drops the rest", () => {
    const href = studioHref("acme", "van-hanh/du-lieu/:bang", { bang: "crm_accounts" }, { q: "an", sort: "name", order: "desc", view: "kanban", "status[eq]": "new", evil: "1" });
    const sp = new URL(href, "http://x").searchParams;
    expect(Object.fromEntries(sp)).toEqual({ q: "an", sort: "name", order: "desc", view: "kanban", "status[eq]": "new" });
  });
  it("parseStudioPath gives them back, a stray key is dropped", () => {
    const p = parseStudioPath("/studio/du-an/acme/van-hanh/du-lieu/crm_accounts?q=an&sort=name&status[eq]=new&evil=1&x[bad op!]=2");
    expect(p?.query).toEqual({ q: "an", sort: "name", "status[eq]": "new" });
  });
  it("accepts the /moi and /tao-nhieu routes", () => {
    expect(parseStudioPath("/studio/du-an/acme/van-hanh/du-lieu/crm_accounts/moi")?.route?.id).toBe("van-hanh/du-lieu/:bang/moi");
    expect(parseStudioPath("/studio/du-an/acme/van-hanh/du-lieu/crm_accounts/tao-nhieu")?.route?.id).toBe("van-hanh/du-lieu/:bang/tao-nhieu");
  });
});

describe("mergeRecordCounts (specs/078 T177, R12)", () => {
  const ents = [{ name: "crm_accounts", label: "Khách hàng" }, { name: "crm_leads", label: "Tiềm năng" }] as never;
  it("joins on machine name, drops deleted tables, missing stays absent", () => {
    const m = mergeRecordCounts(ents, [
      { entity: "crm_accounts", label: "Khách hàng", count: 5 },
      { entity: "deleted_tbl", label: "Khách hàng", count: 9 },
    ]);
    expect(m.get("crm_accounts")).toBe(5);
    expect(m.has("crm_leads")).toBe(false);
    expect(m.has("deleted_tbl")).toBe(false);
  });
});

// specs/078 lát u7-muc-dung-bieu-do T170a: the usage window is cut in the SERVER's calendar, not the browser's.
const VN = "Asia/Ho_Chi_Minh";
describe("usageRange", () => {
  it.each([
    [7, "2026-10-08"],
    [30, "2026-09-15"],
    [90, "2026-07-17"],
    [365, "2025-10-15"],
  ])("%i days ends today and starts today-(n-1)", (n, from) => {
    expect(usageRange(n, new Date("2026-10-14T05:00:00Z"), VN)).toEqual({ from, to: "2026-10-14" });
  });
  it("16:59:59 and 17:00:00 UTC are two different server days in Asia/Ho_Chi_Minh", () => {
    expect(usageRange(7, new Date("2026-10-14T16:59:59Z"), VN).to).toBe("2026-10-14");
    expect(usageRange(7, new Date("2026-10-14T17:00:00Z"), VN).to).toBe("2026-10-15");
  });
  it("another server zone cuts another day from the same instant (browser zone never matters)", () => {
    const at = new Date("2026-10-14T17:30:00Z");
    expect(usageRange(7, at, "UTC").to).toBe("2026-10-14");
    expect(usageRange(7, at, "America/Los_Angeles").to).toBe("2026-10-14");
    expect(usageRange(7, at, VN).to).toBe("2026-10-15");
  });
  it("never longer than 366 days; 'to' is the server's today", () => {
    const r = usageRange(9999, new Date("2026-10-14T05:00:00Z"), VN);
    expect(r.to).toBe("2026-10-14");
    expect((Date.parse(r.to) - Date.parse(r.from)) / 86400000 + 1).toBeLessThanOrEqual(366);
  });
});
