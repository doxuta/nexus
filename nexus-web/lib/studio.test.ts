import { describe, it, expect } from "vitest";
import { VERTICALS, BUILTIN_PACK_FACTS, orderBuiltinsForDisplay } from "./studio";

describe("orderBuiltinsForDisplay", () => {
  it("puts crm (reference vertical) before clinic even when backend returns alphabetical", () => {
    expect(orderBuiltinsForDisplay(["clinic", "crm"])).toEqual(["crm", "clinic"]);
  });

  it("keeps backend order for unknown slugs and places them last, without mutating input", () => {
    const input = ["zzz_custom", "clinic", "aaa_custom", "crm"];
    const out = orderBuiltinsForDisplay(input);
    expect(out).toEqual(["crm", "clinic", "zzz_custom", "aaa_custom"]);
    expect(input).toEqual(["zzz_custom", "clinic", "aaa_custom", "crm"]); // unmutated
  });

  it("also orders {slug,...} objects (the FE-CATCHUP-2 /builtin manifest entries) by the same rule", () => {
    const input = [
      { slug: "clinic", manifest: { name: "Clinic", version: "1.0.0" }, entityCount: 2 },
      { slug: "crm", manifest: { name: "CRM", version: "1.1.0" }, entityCount: 5 },
    ];
    const out = orderBuiltinsForDisplay(input);
    expect(out.map((b) => b.slug)).toEqual(["crm", "clinic"]);
    // Objects pass through untouched — manifest/version stay attached for the card to render.
    expect(out[0].manifest.version).toBe("1.1.0");
    expect(input.map((b) => b.slug)).toEqual(["clinic", "crm"]); // unmutated
  });
});

describe("catalog honesty (builtin packs mirror builtin.go)", () => {
  it("every builtin with facts exists in the catalog", () => {
    for (const slug of Object.keys(BUILTIN_PACK_FACTS)) {
      expect(VERTICALS.some((v) => v.name === slug)).toBe(true);
    }
  });

  it("crm entity chips match the real 5-entity pack; clinic matches its 2", () => {
    expect(VERTICALS.find((v) => v.name === "crm")?.entities).toHaveLength(5);
    expect(VERTICALS.find((v) => v.name === "clinic")?.entities).toEqual(["Bệnh nhân", "Lịch hẹn"]);
  });
});
