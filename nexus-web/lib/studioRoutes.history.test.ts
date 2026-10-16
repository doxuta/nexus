// specs/079 f7-lich-su-dong-thoi-gian T051: history list takes no filters; the version page keeps only loai/bang.
import { describe, expect, it } from "vitest";
import { parseStudioPath, studioHref } from "@/lib/studioRoutes";

const ID = "3f0c9c1e-6b1a-4c7e-9d2a-0a1b2c3d4e5f";

describe("studioRoutes lich-su", () => {
  it("version page round-trips with loai/bang", () => {
    const href = studioHref("dental", "bao-tri/lich-su/:id", { id: ID }, { loai: "role", bang: "crm_leads" });
    const p = parseStudioPath(href);
    expect(p?.route?.id).toBe("bao-tri/lich-su/:id");
    expect(p?.params.id).toBe(ID);
    expect(p?.query).toEqual({ loai: "role", bang: "crm_leads" });
  });
  it("unknown query keys are dropped", () => {
    const p = parseStudioPath(studioHref("dental", "bao-tri/lich-su/:id", { id: ID }, { loai: "role", zzz: "1" } as never));
    expect(p?.query).toEqual({ loai: "role" });
  });
  it("history list takes no loai/bang", () => {
    const p = parseStudioPath(`${studioHref("dental", "bao-tri/lich-su")}?loai=role&bang=x`);
    expect(p?.route?.id).toBe("bao-tri/lich-su");
    expect(p?.query ?? {}).toEqual({});
  });
});
