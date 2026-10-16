// specs/075 L1c T064: the screens route no longer knows the temporary `?ban=moi` flag; the other keys survive.
import { describe, expect, it } from "vitest";
import { parseStudioPath, studioHref } from "@/lib/studioRoutes";

describe("dung/man-hinh/<bang> query keys", () => {
  it("drops the retired ban flag and keeps man, che-do, lien-quan, truong, app", () => {
    const href = studioHref("acme", "dung/man-hinh/:bang", { bang: "crm_x" }, { man: "form", ban: "moi", truong: "email", "lien-quan": "a", "che-do": "danh-sach", app: "crm" });
    expect(href).not.toContain("ban=");
    for (const k of ["man=form", "truong=email", "lien-quan=a", "che-do=danh-sach", "app=crm"]) expect(href).toContain(k);
  });
  it("parses the plain form route", () => {
    expect(parseStudioPath("/studio/du-an/acme/dung/man-hinh/crm_x?man=ho-so")?.query).toEqual({ man: "ho-so" });
  });
  it("the table-less entry also forgets ban", () => {
    expect(studioHref("acme", "dung/man-hinh", {}, { bang: "crm_x", ban: "moi" })).not.toContain("ban=");
  });
});
