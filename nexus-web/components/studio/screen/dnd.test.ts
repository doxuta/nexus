import { describe, expect, it } from "vitest";
import { PointerSensor, KeyboardSensor } from "@dnd-kit/core";
import type { FieldMeta } from "@/lib/api";
import { draftFromSections, type DraftSection } from "@/lib/layoutDraft";
import { DRAG_THRESHOLD_PX, FORM_SENSORS, UNPLACED_ZONE, applyDrop, dropTarget, sectionDropId, unplacedDragId } from "./dnd";

const F = (name: string, isRequired = false): FieldMeta => ({ name, label: name, fieldType: "text", isRequired }) as FieldMeta;
const fields = [F("a", true), F("b"), F("c"), F("d")];
const base = (): DraftSection[] =>
  draftFromSections([
    { title: "S1", columns: 2, fields: ["a", "b", "c"] },
    { title: "Rỗng", columns: 2, fields: [] },
  ]);
const names = (d: DraftSection[]) => d.map((s) => s.cells.map((c) => (c.kind === "field" ? c.field : "_")));

describe("dnd config", () => {
  it("threshold is 5px, pointer + keyboard sensors", () => {
    expect(DRAG_THRESHOLD_PX).toBe(5);
    expect(FORM_SENSORS.map((s) => s.sensor)).toEqual([PointerSensor, KeyboardSensor]);
    expect(FORM_SENSORS[0].options).toEqual({ activationConstraint: { distance: 5 } });
  });
});

describe("dropTarget", () => {
  it("onto a later cell of the same section lands AFTER it; onto an earlier cell BEFORE it", () => {
    const d = base();
    const [a, , c] = d[0].cells;
    expect(dropTarget(d, fields, a.id, c.id)).toEqual({ kind: "move", cellId: a.id, sectionId: d[0].id, index: 3 });
    expect(dropTarget(d, fields, c.id, a.id)).toEqual({ kind: "move", cellId: c.id, sectionId: d[0].id, index: 0 });
  });
  it("onto an empty section", () => {
    const d = base();
    expect(dropTarget(d, fields, d[0].cells[1].id, sectionDropId(d[1].id))).toEqual({ kind: "move", cellId: d[0].cells[1].id, sectionId: d[1].id, index: 0 });
  });
  it("an unplaced field dropped on a cell is placed before it", () => {
    const d = base();
    expect(dropTarget(d, fields, unplacedDragId("d"), d[0].cells[1].id)).toEqual({ kind: "place", field: "d", sectionId: d[0].id, index: 1 });
  });
  it("out to the unplaced list removes; a required field is refused with the reason", () => {
    const d = base();
    expect(dropTarget(d, fields, d[0].cells[1].id, UNPLACED_ZONE)).toEqual({ kind: "remove", cellId: d[0].cells[1].id });
    expect(dropTarget(d, fields, d[0].cells[0].id, UNPLACED_ZONE)).toEqual({ kind: "blocked", reason: "requiredMustStay" });
  });
  it("no target, itself, or unplaced onto unplaced = nothing", () => {
    const d = base();
    expect(dropTarget(d, fields, d[0].cells[0].id, null)).toEqual({ kind: "none" });
    expect(dropTarget(d, fields, d[0].cells[0].id, d[0].cells[0].id)).toEqual({ kind: "none" });
    expect(dropTarget(d, fields, unplacedDragId("d"), UNPLACED_ZONE)).toEqual({ kind: "none" });
  });
});

describe("applyDrop", () => {
  it("move / place / remove change the draft; blocked and none return the same draft", () => {
    const d = base();
    const [a, b, c] = d[0].cells;
    expect(names(applyDrop(d, dropTarget(d, fields, a.id, c.id)))).toEqual([["b", "c", "a"], []]);
    expect(names(applyDrop(d, dropTarget(d, fields, unplacedDragId("d"), sectionDropId(d[1].id))))).toEqual([["a", "b", "c"], ["d"]]);
    expect(names(applyDrop(d, dropTarget(d, fields, b.id, UNPLACED_ZONE)))).toEqual([["a", "c"], []]);
    expect(applyDrop(d, dropTarget(d, fields, a.id, UNPLACED_ZONE))).toBe(d);
  });
});
