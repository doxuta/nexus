// specs/078 T012a(iii) + FR-133: a value from the URL is a trust boundary. ?nguoi= / ?cong= / ?luat= and the <id> segment go
// straight into API paths (getUser(id), listHookDeliveries(id), /:entity/:id/purge), which do not encode them, so the parser
// must drop anything that is not an id: `../roles/x` would otherwise become a call to another route with the builder token.
import { describe, expect, it } from "vitest";
import { ID_RE, parseStudioPath, studioHref } from "@/lib/studioRoutes";

const S = "/studio/du-an/acme";
const BAD = ["../roles/x", "a/b", "%2e%2e", "%2e%2e%2froles", "x".repeat(65), "a b", "a?b", "..", ""];

describe("operate URL values are ids or nothing", () => {
  it.each([
    ["van-hanh/nguoi-dung/nhat-ky", "nguoi"],
    ["van-hanh/tu-dong/cong-nhan", "cong"],
    ["van-hanh/tu-dong/lan-chay", "luat"],
  ])("%s drops a malformed ?%s=", (route, key) => {
    for (const bad of BAD) {
      const p = parseStudioPath(`${S}/${route}?${key}=${encodeURIComponent(bad)}`);
      expect(p?.query[key], `${key}=${bad}`).toBeUndefined();
    }
    expect(parseStudioPath(`${S}/${route}?${key}=u-123_AB`)?.query[key]).toBe("u-123_AB");
  });

  it("an <id> path segment that is not an id does not match the record route", () => {
    for (const bad of ["..", "%2e%2e", "x".repeat(65), "a%2fb"]) {
      const p = parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/${bad}`);
      expect(p?.route?.id, bad).not.toBe("van-hanh/du-lieu/:bang/:id");
    }
    expect(parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/ab-12`)?.params).toEqual({ bang: "khach_hang", id: "ab-12" });
  });

  it("studioHref refuses a malformed id and never puts it in a path", () => {
    for (const bad of BAD) expect(studioHref("acme", "van-hanh/du-lieu/:bang/:id", { bang: "khach_hang", id: bad }), bad).toBe(S);
  });

  it("one id pattern is shared (ID_RE)", () => {
    for (const bad of BAD) expect(ID_RE.test(bad), bad).toBe(false);
  });
});

// specs/078 T075 (lát u7-du-lieu-ban-ghi): record page URL keys.
describe("record page URL (moi / sua / cloneFrom / pf_)", () => {
  it("parses moi and a UUID id as the record route", () => {
    expect(parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/moi`)?.route?.id).toBe("van-hanh/du-lieu/:bang/moi");
    const id = "0b6f1c1e-1c2d-4e5f-8a9b-0c1d2e3f4a5b";
    expect(parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/${id}`)?.params.id).toBe(id);
  });

  it("allows cloneFrom, sua and pf_<field> and round-trips them", () => {
    const href = studioHref("acme", "van-hanh/du-lieu/:bang/moi", { bang: "khach_hang" }, { cloneFrom: "ab-12", pf_ngay: "2026-10-14", pf_x: "a b" });
    expect(href).toBe(`${S}/van-hanh/du-lieu/khach_hang/moi?cloneFrom=ab-12&pf_ngay=2026-10-14&pf_x=a%20b`);
    expect(parseStudioPath(href)?.query).toEqual({ cloneFrom: "ab-12", pf_ngay: "2026-10-14", pf_x: "a b" });
    const edit = studioHref("acme", "van-hanh/du-lieu/:bang/:id", { bang: "khach_hang", id: "ab-12" }, { sua: "1" });
    expect(parseStudioPath(edit)?.query).toEqual({ sua: "1" });
  });

  it("drops unknown keys, non-pf_ keys, malformed cloneFrom and pf_ on other routes", () => {
    const p = parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/moi?evil=1&PF_x=1&pf_=1&cloneFrom=${encodeURIComponent("../x")}`);
    expect(p?.query).toEqual({});
    expect(parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/ab-12?pf_x=1&sua=2`)?.query).toEqual({});
  });

  it("a record id never equals a sibling page word", () => {
    for (const w of ["nhap", "tao-nhieu", "thung-rac", "moi"]) {
      expect(studioHref("acme", "van-hanh/du-lieu/:bang/:id", { bang: "khach_hang", id: w }), w).toBe(S);
    }
    expect(parseStudioPath(`${S}/van-hanh/du-lieu/khach_hang/tao-nhieu`)?.route?.id).not.toBe("van-hanh/du-lieu/:bang/:id");
  });
});
