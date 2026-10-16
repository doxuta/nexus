// specs/075 L3 T120: bulk steps are pure and report what they skipped and why (FR-072) — never silently.
import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import type { DraftSection } from "@/lib/layoutDraft";
import { applyBulkMoveToSection, applyBulkRemove, applyBulkRequired } from "@/lib/screenEditorModel";

const F = (name: string, isRequired = false): FieldMeta => ({ name, label: name, fieldType: "text", isRequired }) as FieldMeta;
const fc = (field: string, extra: object = {}) => ({ id: `c-${field}`, kind: "field" as const, field, ...extra });
const sec = (id: string, cells: DraftSection["cells"]): DraftSection => ({ id, title: id, columns: 2, cells }) as DraftSection;
const fields = [F("a", true), F("b"), F("c"), F("d"), F("e")];
const names = (d: DraftSection[], id: string) => d.find((s) => s.id === id)!.cells.map((c) => (c.kind === "field" ? c.field : "_"));
const when = { field: "b", op: "eq", value: "x" };

describe("applyBulkRequired", () => {
  const draft = [sec("s1", [fc("a"), fc("b"), fc("c", { visibleWhen: when })]), sec("s2", [])];
  it("sets edits, places unplaced fields in the last section, skips conditional ones with a reason", () => {
    const r = applyBulkRequired(draft, fields, {}, ["b", "c", "d"], "Mới");
    expect(r.edits).toEqual({ b: true, d: true });
    expect(names(r.draft, "s2")).toEqual(["d"]);
    expect(r.skipped).toEqual([{ field: "c", reason: "hasCondition", detail: [] }]);
  });
  it("a field already required is not an edit and not a skip", () => {
    const r = applyBulkRequired(draft, fields, {}, ["a"], "Mới");
    expect(r.edits).toEqual({});
    expect(r.skipped).toEqual([]);
    expect(r.draft).toBe(draft);
  });
});

describe("applyBulkRemove", () => {
  const draft = [sec("s1", [fc("a"), fc("b"), fc("c", { visibleWhen: when }), fc("d")])];
  it("skips required fields and controllers of another cell (naming the dependents)", () => {
    const r = applyBulkRemove(draft, fields, ["a", "b", "d"]);
    expect(names(r.draft, "s1")).toEqual(["a", "b", "c"]);
    expect(r.skipped).toEqual([
      { field: "a", reason: "required", detail: [] },
      { field: "b", reason: "controls", detail: ["c"] },
    ]);
  });
  it("removes a controller together with its dependents in one batch", () => {
    const r = applyBulkRemove(draft, fields, ["b", "c"]);
    expect(names(r.draft, "s1")).toEqual(["a", "d"]);
    expect(r.skipped).toEqual([]);
  });
  it("a field that is not on the form is a no-op", () => {
    const r = applyBulkRemove(draft, fields, ["e"]);
    expect(r.draft).toBe(draft);
    expect(r.skipped).toEqual([]);
  });
});

describe("applyBulkMoveToSection", () => {
  const draft = [sec("s1", [fc("a"), fc("b")]), sec("s2", [fc("c")])];
  it("moves placed cells to the end of the section and places unplaced ones", () => {
    const r = applyBulkMoveToSection(draft, ["a", "d"], "s2");
    expect(names(r.draft, "s1")).toEqual(["b"]);
    expect(names(r.draft, "s2")).toEqual(["c", "a", "d"]);
    expect(r.skipped).toEqual([]);
  });
  it("an unknown section skips everything with a reason", () => {
    const r = applyBulkMoveToSection(draft, ["a"], "nope");
    expect(r.draft).toBe(draft);
    expect(r.skipped).toEqual([{ field: "a", reason: "noSection", detail: [] }]);
  });
});
