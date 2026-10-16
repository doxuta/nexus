// specs/076 f4-proposal-overlay-hook T142: routeForChange covers the draft kinds × op, stays put when the current page
// already draws the kind, and never lets a model-made name into a path (FR-129).
import { describe, expect, it } from "vitest";
import { parseStudioPath, routeForChange, type ChangeForRoute } from "@/lib/studioRoutes";

const S = "phong-kham";
const at = (p: string) => parseStudioPath(`/studio/du-an/${S}/${p}`);
const go = (c: ChangeForRoute, cur: string | null = null) => routeForChange(c, cur ? at(cur) : null, S);
const P = `/studio/du-an/${S}`;

describe("routeForChange targets", () => {
  it.each([
    [{ docType: "entity", op: "create", target: { entity: "lich_hen" } }, `${P}/dung/du-lieu`],
    [{ docType: "entity", op: "update", target: { entity: "lich_hen" } }, `${P}/dung/du-lieu/lich_hen`],
    [{ docType: "field", op: "create", target: { entity: "lich_hen", field: "ngay" } }, `${P}/dung/du-lieu/lich_hen?truong=ngay`],
    [{ docType: "layout", op: "update", target: { entity: "lich_hen", view: "edit" } }, `${P}/dung/man-hinh/lich_hen?man=form`],
    [{ docType: "layout", op: "update", target: { entity: "lich_hen", view: "detail" } }, `${P}/dung/man-hinh/lich_hen?man=ho-so`],
    [{ docType: "layout", op: "update", target: { entity: "lich_hen", view: "list" } }, `${P}/dung/man-hinh/lich_hen?man=danh-sach`],
    [{ docType: "layout", op: "update", target: { entity: "lich_hen", view: "evil" } }, `${P}/dung/man-hinh/lich_hen`],
    [{ docType: "workflow", op: "create" }, `${P}/dung/tu-dong`],
    [{ docType: "workflow", op: "update", docKey: "r1" }, `${P}/dung/tu-dong/luat/r1`],
    [{ docType: "report", op: "create" }, `${P}/dung/bao-cao`],
    [{ docType: "dashboard", op: "create" }, `${P}/dung/bao-cao`],
    [{ docType: "app", op: "create", target: { name: "crm" } }, `${P}/dung/man-hinh/app?app=crm`],
    [{ docType: "role", op: "update", docKey: "3f2b8a10-5c1d-4e2f-9a7b-0c1d2e3f4a5b", target: { name: "le_tan", entity: "invoice" } }, `${P}/dung/nguoi-dung-quyen`],
    [{ docType: "branding", op: "update" }, `${P}/dung/thuong-hieu`],
    [{ docType: "relationship", op: "create" },`${P}/dung/du-lieu/so-do`],
    [{ docType: "approval", op: "create" }, `${P}/dung/tu-dong/duyet`],
    [{ docType: "workqueue", op: "create" }, `${P}/dung/tu-dong/hang-doi`],
    [{ docType: "records", op: "create", target: { entity: "lich_hen" } }, `${P}/van-hanh/du-lieu/lich_hen`],
  ] as [ChangeForRoute, string][])("%j", (c, href) => {
    const r = go(c);
    expect(r?.href).toBe(href);
    expect(r?.stay).toBe(false);
  });

  it("unknown docType has no target", () => expect(go({ docType: "field_acl", op: "update" })).toBeNull());
});

describe("routeForChange stays put", () => {
  it("field while on that table's screen", () => {
    expect(go({ docType: "field", op: "create", target: { entity: "lich_hen", field: "ngay" } }, "dung/man-hinh/lich_hen?man=form")?.stay).toBe(true);
  });
  it("field on another table's screen navigates", () => {
    expect(go({ docType: "field", op: "create", target: { entity: "lich_hen" } }, "dung/man-hinh/khac")?.stay).toBe(false);
  });
  it("layout on the same canvas / data list / rules list", () => {
    expect(go({ docType: "layout", op: "update", target: { entity: "lich_hen", view: "edit" } }, "dung/man-hinh/lich_hen?man=form")?.stay).toBe(true);
    expect(go({ docType: "entity", op: "create" }, "dung/du-lieu/lich_hen")?.stay).toBe(true);
    expect(go({ docType: "workflow", op: "create" }, "dung/tu-dong/luat/moi")?.stay).toBe(true);
  });
});

describe("routeForChange FR-129", () => {
  const bad = ["../../x", "a?b=c", "//e", "javascript:alert(1)", "\u0000", "co khoang trang", "a".repeat(65)];
  it.each(bad)("entity/field %j gives null", (v) => {
    expect(go({ docType: "field", op: "create", target: { entity: v, field: "ngay" } })).toBeNull();
    expect(go({ docType: "field", op: "create", target: { entity: "lich_hen", field: v } })).toBeNull();
    expect(go({ docType: "entity", op: "update", target: { entity: v } })).toBeNull();
  });
  it.each(bad)("docKey %j gives null", (v) => {
    expect(go({ docType: "workflow", op: "update", docKey: v })).toBeNull();
  });
  it("a bad slug gives null; every target stays inside the project", () => {
    expect(routeForChange({ docType: "report" }, null, "../x")).toBeNull();
    expect(go({ docType: "report", op: "create" })?.href.startsWith(`${P}/`)).toBe(true);
  });
});
