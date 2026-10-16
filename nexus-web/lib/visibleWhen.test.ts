// specs/054 FR-7: the FE port of services.fieldVisible. The golden file is the parity contract with
// the Go evaluator — read straight from nexus-platform so neither side can drift alone.
import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import type { LayoutSection, VisibleWhen } from "@/lib/api";
import type { FieldMeta } from "@/lib/api";
import { createImage, fieldVisible, layoutConditions, visibleSections } from "./visibleWhen";

interface GoldenCase {
  name: string;
  conditions: Record<string, VisibleWhen>;
  values: Record<string, unknown>;
  visible: Record<string, boolean>;
}

interface GoldenCreateCase {
  name: string;
  fields: Pick<FieldMeta, "name" | "fieldType" | "defaultValue" | "isRequired">[];
  conditions: Record<string, VisibleWhen>;
  payload: Record<string, unknown>;
  visible: Record<string, boolean>;
}

const golden = JSON.parse(
  fs.readFileSync(path.join(process.cwd(), "../nexus-platform/internal/services/testdata/visible_when_cases.json"), "utf8"),
) as { cases: GoldenCase[]; createCases: GoldenCreateCase[] };

describe("fieldVisible — golden cases shared with the Go evaluator", () => {
  it("the golden file parsed (a moved file must not turn this suite into zero cases)", () => {
    expect(golden.cases.length).toBeGreaterThanOrEqual(21);
  });

  it.each(golden.cases.map((c) => [c.name, c] as const))("%s", (_name, c) => {
    const conds = new Map(Object.entries(c.conditions));
    for (const [field, want] of Object.entries(c.visible)) {
      expect({ field, visible: fieldVisible(field, conds, c.values) }).toEqual({ field, visible: want });
    }
  });

  // createCases: the create image (payload + coerced defaults) the form's create mode judges on.
  it("the golden file carries its create-image cases", () => {
    expect(golden.createCases.length).toBeGreaterThanOrEqual(8);
  });

  it.each(golden.createCases.map((c) => [c.name, c] as const))("create image: %s", (_name, c) => {
    const conds = new Map(Object.entries(c.conditions));
    const image = createImage(c.fields as FieldMeta[], c.payload);
    for (const [field, want] of Object.entries(c.visible)) {
      expect({ field, visible: fieldVisible(field, conds, image) }).toEqual({ field, visible: want });
    }
  });

  it("a default that does not coerce leaves the field absent (the server then answers 422 for the whole record)", () => {
    const f = [{ name: "n", fieldType: "number", defaultValue: "abc" }] as FieldMeta[];
    expect(createImage(f, {})).toEqual({});
  });

  it("a field named like an Object.prototype member is not mistaken for a condition or a value", () => {
    const conds = new Map<string, VisibleWhen>([["note", { field: "constructor", op: "notEmpty" }]]);
    expect(fieldVisible("constructor", conds, {})).toBe(true);
    expect(fieldVisible("note", conds, {})).toBe(false);
  });
});

const SECTIONS: LayoutSection[] = [
  {
    title: "Khách hàng",
    columns: 2,
    fields: [
      "customer_type",
      { field: "tax_code", labelOverride: "MST", visibleWhen: { field: "customer_type", op: "eq", value: "business" } },
      { spacer: true },
    ],
  },
  {
    title: "Doanh nghiệp",
    fields: [
      { field: "company_size", visibleWhen: { field: "customer_type", op: "eq", value: "business" } },
      { field: "industry", visibleWhen: { field: "tax_code", op: "notEmpty" } },
    ],
  },
];

describe("layoutConditions", () => {
  it("collects target → condition across every section, bare-string cells and spacers skipped", () => {
    expect([...layoutConditions(SECTIONS).keys()]).toEqual(["tax_code", "company_size", "industry"]);
  });

  it("ignores a condition with no controller or an unknown operator (the server never serves one)", () => {
    const odd = [{ fields: [{ field: "a", visibleWhen: { op: "eq", value: "x" } }, { field: "b", visibleWhen: { field: "c", op: "like" } }] }];
    expect(layoutConditions(odd as unknown as LayoutSection[]).size).toBe(0);
  });
});

describe("visibleSections — the read-only layout a record shows", () => {
  it("drops the cells whose condition fails, keeping spacers and the section's own settings", () => {
    const out = visibleSections(SECTIONS, { customer_type: "individual", tax_code: "0101" });
    expect(out).toEqual([{ title: "Khách hàng", columns: 2, fields: ["customer_type", { spacer: true }] }]);
  });

  it("a section left with no field drops out whole — no empty titled box", () => {
    expect(visibleSections(SECTIONS, { customer_type: "individual" }).map((s) => s.title)).toEqual(["Khách hàng"]);
  });

  it("transitive: a dependent of a hidden cell hides even when its own condition holds on the stored value", () => {
    const out = visibleSections(SECTIONS, { customer_type: "individual", tax_code: "0101" });
    expect(JSON.stringify(out)).not.toContain("industry");
  });

  it("every condition met: the layout comes back as is", () => {
    expect(visibleSections(SECTIONS, { customer_type: "business", tax_code: "0101" })).toEqual(SECTIONS);
  });

  it("a layout without conditions is returned untouched (same reference)", () => {
    const plain: LayoutSection[] = [{ title: "A", fields: ["x", { field: "y", labelOverride: "Y" }] }];
    expect(visibleSections(plain, {})).toBe(plain);
  });

  it("a section that never had a field (spacers only) is not dropped by this filter", () => {
    const spacers: LayoutSection[] = [{ title: "Trống", fields: [{ spacer: true }] }, SECTIONS[0]];
    expect(visibleSections(spacers, { customer_type: "business" })[0]).toBe(spacers[0]);
  });
});
