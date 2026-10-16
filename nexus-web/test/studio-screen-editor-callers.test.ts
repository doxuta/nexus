// specs/075 T004: four api.ts functions have routes that check-orphan-endpoints.py cannot see being called (it is blind to
// the method and to who calls the helper). Each must keep at least one caller in app/ or components/, so deleting the last
// screen (or relation) builder that uses it cannot silently orphan a backend route. Today the callers are LayoutBuilder,
// ScreenEditor (L1a), the old relationship designer and PropertyPanel (deleted in L6); L6 moved the relation ones to the new map.
import { describe, expect, it } from "vitest";
import { listSources, read, stripComments } from "./studioGateScan";

const NAMES = ["deleteLayout", "setRelationshipViewConfig", "getRelationshipImpact", "deleteRelationshipById"];
const files = listSources(["app", "components"]);

describe("callers of the four endpoint helpers the orphan gate cannot see", () => {
  for (const name of NAMES) {
    it(`${name}( has at least one caller`, () => {
      const callers = files.filter((f) => new RegExp(`\\b${name}\\(`).test(stripComments(read(f))));
      expect(callers, `no caller of ${name}( in app/ or components/`).not.toHaveLength(0);
    });
  }

  it("the screen editor is a caller of deleteLayout (reset to auto-generated)", () => {
    expect(stripComments(read("components/studio/screen/ScreenEditor.tsx"))).toMatch(/\bdeleteLayout\(/);
  });
});
