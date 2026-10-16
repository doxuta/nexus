import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { draftFromSections, missingRequired, sectionsToJson, updateSection, type DraftSection } from "@/lib/layoutDraft";
import {
  addSpacerCell, moveCellStep, moveCellToSection, moveSectionStep, placeField, placeFieldAt, removeFieldGuarded, removeSectionGuarded,
  setCellLabel, setSectionColumns,
} from "@/lib/screenEditorModel";

const F = (name: string, isRequired = false): FieldMeta => ({ name, label: name, fieldType: "text", isRequired }) as FieldMeta;
const fields = [F("a", true), F("b"), F("c"), F("d")];
const base = (): DraftSection[] =>
  draftFromSections([
    { title: "S1", columns: 2, fields: ["a", "b"] },
    { title: "S2", columns: 2, fields: ["c"] },
  ]);
const names = (d: DraftSection[]) => d.map((s) => s.cells.map((c) => (c.kind === "field" ? c.field : "_")));

describe("placeField", () => {
  it("appends to the chosen section", () => {
    const d = base();
    expect(names(placeField(d, "d", d[0].id, "Mới").draft)).toEqual([["a", "b", "d"], ["c"]]);
  });
  it("falls back to the last section", () => {
    expect(names(placeField(base(), "d", null, "Mới").draft)).toEqual([["a", "b"], ["c", "d"]]);
  });
  it("creates a titled section when there is none", () => {
    const r = placeField([], "d", null, "Mới thêm");
    expect(r.draft).toHaveLength(1);
    expect(r.draft[0].title).toBe("Mới thêm");
    expect(names(r.draft)).toEqual([["d"]]);
  });
  it("returns the new cell id", () => {
    const r = placeField(base(), "d", null, "x");
    expect(r.draft[1].cells.at(-1)?.id).toBe(r.cellId);
  });
  it("does not place a field twice", () => {
    expect(names(placeField(base(), "a", null, "x").draft)).toEqual([["a", "b"], ["c"]]);
  });
});

describe("removeFieldGuarded", () => {
  it("blocks a required field (requiredMustStay)", () => {
    const d = base();
    const r = removeFieldGuarded(d, d[0].cells[0].id, fields);
    expect(r.blocked).toBe("requiredMustStay");
    expect(r.draft).toBe(d);
  });
  it("removes an optional field and missingRequired stays empty", () => {
    const d = base();
    const r = removeFieldGuarded(d, d[0].cells[1].id, fields);
    expect(r.blocked).toBeUndefined();
    expect(names(r.draft)).toEqual([["a"], ["c"]]);
    expect(missingRequired(fields, r.draft)).toEqual([]);
  });
  it("removes a spacer freely", () => {
    const d = draftFromSections([{ columns: 2, fields: ["a", { spacer: true }] }]);
    expect(names(removeFieldGuarded(d, d[0].cells[1].id, fields).draft)).toEqual([["a"]]);
  });
});

describe("moveCellStep", () => {
  it("moves down and up inside a section", () => {
    const d = base();
    const down = moveCellStep(d, d[0].cells[0].id, 1);
    expect(names(down)).toEqual([["b", "a"], ["c"]]);
    expect(names(moveCellStep(down, down[0].cells[1].id, -1))).toEqual([["a", "b"], ["c"]]);
  });
  it("crosses to the next section at the boundary", () => {
    const d = base();
    expect(names(moveCellStep(d, d[0].cells[1].id, 1))).toEqual([["a"], ["b", "c"]]);
  });
  it("crosses to the previous section going up from index 0", () => {
    const d = base();
    expect(names(moveCellStep(d, d[1].cells[0].id, -1))).toEqual([["a", "b", "c"], []]);
  });
  it("is a no-op at the very first and very last slot", () => {
    const d = base();
    expect(moveCellStep(d, d[0].cells[0].id, -1)).toBe(d);
    expect(moveCellStep(d, d[1].cells[0].id, 1)).toBe(d);
  });
});

describe("moveCellToSection / setCellLabel", () => {
  it("moves to the end of another section", () => {
    const d = base();
    expect(names(moveCellToSection(d, d[0].cells[0].id, d[1].id))).toEqual([["b"], ["c", "a"]]);
  });
  it("setCellLabel writes labelOverride only and never touches required", () => {
    const d = base();
    const id = d[0].cells[0].id;
    const next = setCellLabel(d, id, "  Họ tên  ");
    expect((sectionsToJson(next).sections[0].fields as unknown[])[0]).toEqual({ field: "a", labelOverride: "Họ tên" });
    expect((sectionsToJson(setCellLabel(next, id, "")).sections[0].fields as unknown[])[0]).toBe("a");
  });
  it("narrowing columns still clamps colspan (layoutDraft contract kept)", () => {
    const d = draftFromSections([{ columns: 3, fields: [{ field: "a", colspan: 3 }] }]);
    const n = updateSection(d, d[0].id, { columns: 2 });
    expect((n[0].cells[0] as { colspan?: number }).colspan).toBe(2);
  });
});

describe("sections and spacers (specs/075 L1b, T042)", () => {
  it("removeSectionGuarded: required cells move to the neighbouring section, the rest go", () => {
    const d = base();
    const r = removeSectionGuarded(d, d[0].id, fields);
    expect(r.blocked).toBeUndefined();
    expect(names(r.draft)).toEqual([["c", "a"]]);
  });
  it("removeSectionGuarded: the only section holding a required field cannot go", () => {
    const d = draftFromSections([{ title: "S", columns: 2, fields: ["a", "b"] }]);
    const r = removeSectionGuarded(d, d[0].id, fields);
    expect(r.blocked).toBe("requiredMustStay");
    expect(r.draft).toBe(d);
  });
  it("removeSectionGuarded: a section without required fields just goes", () => {
    const d = base();
    expect(names(removeSectionGuarded(d, d[1].id, fields).draft)).toEqual([["a", "b"]]);
  });
  it("moveSectionStep swaps with the neighbour and stops at the edge", () => {
    const d = base();
    expect(names(moveSectionStep(d, d[1].id, -1))).toEqual([["c"], ["a", "b"]]);
    expect(moveSectionStep(d, d[0].id, -1)).toBe(d);
  });
  it("setSectionColumns clamps colspan of cells", () => {
    let d = draftFromSections([{ title: "S", columns: 3, fields: [{ field: "a", colspan: 3 }] }]);
    d = setSectionColumns(d, d[0].id, 2);
    expect(d[0].columns).toBe(2);
    expect((d[0].cells[0] as { colspan?: number }).colspan).toBe(2);
  });
  it("addSpacerCell appends an empty slot to the section", () => {
    const d = base();
    expect(names(addSpacerCell(d, d[1].id))).toEqual([["a", "b"], ["c", "_"]]);
  });
  it("placeFieldAt inserts at the slot and refuses a field already on the form", () => {
    const d = base();
    expect(names(placeFieldAt(d, "d", d[0].id, 1))).toEqual([["a", "d", "b"], ["c"]]);
    expect(placeFieldAt(d, "a", d[0].id, 0)).toBe(d);
  });
});
