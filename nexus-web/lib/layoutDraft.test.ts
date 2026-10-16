// layoutDraft — unit tests for the W5 WYSIWYG grid model (normalize/serialize, colspan bounds,
// cross-section moves, string↔object cell promotion, floor rule).
import { describe, it, expect } from "vitest";
import {
  draftFromSections, sectionsToJson, addSection, removeSection, moveSection, updateSection,
  addFieldCell, addSpacer, removeCell, moveCell, updateCell, unplacedFields, missingRequired,
  draftEquals, onForm, type DraftFieldCell,
} from "./layoutDraft";
import type { EntityLayout, FieldMeta, LayoutSection } from "./api";

const stored: LayoutSection[] = [
  {
    title: "Chính",
    columns: 2,
    fields: [
      "name", // string cell
      { field: "email", labelOverride: "E-mail", colspan: 2, helpText: "Email công ty" },
      { spacer: true },
    ],
  },
];

const meta = (name: string, required = false): FieldMeta => ({ name, fieldType: "text", isRequired: required, id: name });

describe("draftFromSections / sectionsToJson round trip", () => {
  it("normalizes string cells to draft objects and serializes them back to strings (demotion)", () => {
    const draft = draftFromSections(stored);
    expect(draft[0].cells[0]).toMatchObject({ kind: "field", field: "name" });
    const json = sectionsToJson(draft);
    expect(json.version).toBe(1);
    const fields = (json.sections[0] as { fields: unknown[] }).fields;
    expect(fields[0]).toBe("name"); // no overrides → terse string form
    expect(fields[1]).toEqual({ field: "email", labelOverride: "E-mail", colspan: 2, helpText: "Email công ty" });
    expect(fields[2]).toEqual({ spacer: true });
  });

  it("promotion is transparent: adding an override to a string cell yields an object cell", () => {
    let draft = draftFromSections(stored);
    const nameCell = draft[0].cells[0];
    draft = updateCell(draft, nameCell.id, { helpText: "Tên đầy đủ" });
    const fields = (sectionsToJson(draft).sections[0] as { fields: unknown[] }).fields;
    expect(fields[0]).toEqual({ field: "name", helpText: "Tên đầy đủ" });
    // …and clearing it demotes back to the string form.
    draft = updateCell(draft, nameCell.id, { helpText: "" });
    const fields2 = (sectionsToJson(draft).sections[0] as { fields: unknown[] }).fields;
    expect(fields2[0]).toBe("name");
  });

  it("carries visibleWhen through a Studio load → save untouched (specs/054)", () => {
    // Studio has no editor for it yet (#23): saving must not silently drop a condition set via API/AI.
    const cond = { field: "customer_type", op: "eq" as const, value: "business" };
    const draft = draftFromSections([
      { columns: 2, fields: ["customer_type", { field: "tax_code", visibleWhen: cond }] },
    ]);
    const fields = (sectionsToJson(draft).sections[0] as { fields: unknown[] }).fields;
    expect(fields[1]).toEqual({ field: "tax_code", visibleWhen: cond });
  });

  it("empty title is omitted; collapsed only emitted when true", () => {
    const draft = draftFromSections([{ columns: 1, fields: ["a"] }]);
    const sec = sectionsToJson(draft).sections[0];
    expect("title" in sec).toBe(false);
    expect("collapsed" in sec).toBe(false);
  });
});

describe("colspan bounds", () => {
  it("clamps colspan to section columns on save and drops colspan 1", () => {
    let draft = draftFromSections([{ columns: 2, fields: [{ field: "a", colspan: 9 }, { field: "b", colspan: 1 }] }]);
    const fields = (sectionsToJson(draft).sections[0] as { fields: unknown[] }).fields;
    expect(fields[0]).toEqual({ field: "a", colspan: 2 });
    expect(fields[1]).toBe("b"); // colspan 1 = default → demoted to string
    // Editing beyond bounds clamps at edit time too.
    const cellA = draft[0].cells[0];
    draft = updateCell(draft, cellA.id, { colspan: 4 });
    expect((draft[0].cells[0] as DraftFieldCell).colspan).toBe(2);
  });

  it("narrowing section columns re-clamps existing colspans", () => {
    let draft = draftFromSections([{ columns: 4, fields: [{ field: "a", colspan: 4 }] }]);
    draft = updateSection(draft, draft[0].id, { columns: 2 });
    expect((draft[0].cells[0] as DraftFieldCell).colspan).toBe(2);
  });

  it("fullWidth and colspan are mutually exclusive — fullWidth wins", () => {
    let draft = draftFromSections([{ columns: 3, fields: [{ field: "a", colspan: 2 }] }]);
    draft = updateCell(draft, draft[0].cells[0].id, { fullWidth: true });
    const cell = draft[0].cells[0] as DraftFieldCell;
    expect(cell.fullWidth).toBe(true);
    expect(cell.colspan).toBeUndefined();
    const fields = (sectionsToJson(draft).sections[0] as { fields: unknown[] }).fields;
    expect(fields[0]).toEqual({ field: "a", fullWidth: true });
  });

  it("columns accepts 4 (W5 widened) and invalid values fall back to 2", () => {
    const d4 = draftFromSections([{ columns: 4, fields: [] }]);
    expect(d4[0].columns).toBe(4);
    const dBad = draftFromSections([{ columns: 7 as unknown as 4, fields: [] }]);
    expect(dBad[0].columns).toBe(2);
  });

  it("helpText is trimmed and capped at 200 chars", () => {
    let draft = draftFromSections([{ columns: 1, fields: ["a"] }]);
    draft = updateCell(draft, draft[0].cells[0].id, { helpText: `  ${"x".repeat(300)}  ` });
    const fields = (sectionsToJson(draft).sections[0] as { fields: unknown[] }).fields;
    expect((fields[0] as { helpText: string }).helpText.length).toBeLessThanOrEqual(200);
  });
});

describe("cell + section ops", () => {
  it("moveCell across sections lands at the target index", () => {
    let draft = draftFromSections([
      { title: "A", columns: 2, fields: ["a1", "a2"] },
      { title: "B", columns: 2, fields: ["b1"] },
    ]);
    const a1 = draft[0].cells[0];
    draft = moveCell(draft, a1.id, draft[1].id, 1);
    expect(draft[0].cells.map((c) => (c as DraftFieldCell).field)).toEqual(["a2"]);
    expect(draft[1].cells.map((c) => (c as DraftFieldCell).field)).toEqual(["b1", "a1"]);
  });

  it("moveCell within the same section adjusts for the removal shift when moving down", () => {
    let draft = draftFromSections([{ columns: 1, fields: ["a", "b", "c"] }]);
    const a = draft[0].cells[0];
    draft = moveCell(draft, a.id, draft[0].id, 2); // drop on c's slot
    expect(draft[0].cells.map((c) => (c as DraftFieldCell).field)).toEqual(["b", "a", "c"]);
  });

  it("addFieldCell / addSpacer / removeCell", () => {
    let draft = draftFromSections([{ columns: 2, fields: [] }]);
    draft = addFieldCell(draft, draft[0].id, "x");
    draft = addSpacer(draft, draft[0].id, 0);
    expect(draft[0].cells[0].kind).toBe("spacer");
    expect((draft[0].cells[1] as DraftFieldCell).field).toBe("x");
    draft = removeCell(draft, draft[0].cells[0].id);
    expect(draft[0].cells).toHaveLength(1);
  });

  it("addSection / removeSection / moveSection", () => {
    let draft = draftFromSections([{ title: "A", columns: 2, fields: [] }]);
    draft = addSection(draft, "B");
    expect(draft.map((s) => s.title)).toEqual(["A", "B"]);
    draft = moveSection(draft, draft[1].id, -1);
    expect(draft.map((s) => s.title)).toEqual(["B", "A"]);
    draft = moveSection(draft, draft[0].id, -1); // already first → no-op
    expect(draft.map((s) => s.title)).toEqual(["B", "A"]);
    draft = removeSection(draft, draft[0].id);
    expect(draft.map((s) => s.title)).toEqual(["A"]);
  });
});

describe("derived views", () => {
  it("unplacedFields = metadata minus placed names", () => {
    const draft = draftFromSections([{ columns: 2, fields: ["a"] }]);
    const fields = [meta("a"), meta("b"), meta("c")];
    expect(unplacedFields(fields, draft).map((f) => f.name)).toEqual(["b", "c"]);
  });

  it("missingRequired surfaces the floor rule for the edit view", () => {
    const draft = draftFromSections([{ columns: 2, fields: ["a"] }]);
    const fields = [meta("a", true), meta("b", true), meta("c")];
    expect(missingRequired(fields, draft).map((f) => f.name)).toEqual(["b"]);
  });

  it("missingRequired ignores spacers and counts a visibleWhen cell as placed (screens header)", () => {
    const draft = draftFromSections([
      { columns: 2, fields: ["a", { spacer: true }, { field: "c", visibleWhen: { field: "a", op: "eq", value: "x" } }] },
    ]);
    const fields = [meta("a", true), meta("b", true), meta("c", true)];
    expect(missingRequired(fields, draft).map((f) => f.name)).toEqual(["b"]);
    expect(missingRequired(fields, []).map((f) => f.name)).toEqual(["a", "b", "c"]);
  });

  it("draftEquals compares by serialized form (id churn does not count as dirty)", () => {
    const d1 = draftFromSections(stored);
    const d2 = draftFromSections(stored); // fresh ids, same content
    expect(draftEquals(d1, d2)).toBe(true);
    const d3 = updateCell(d2, d2[0].cells[0].id, { helpText: "khác" });
    expect(draftEquals(d1, d3)).toBe(false);
  });
});

describe("onForm", () => {
  const fields = [meta("name"), meta("email"), meta("note")];
  const lay = (sections?: LayoutSection[], isDefault = false): EntityLayout =>
    ({ entityName: "x", views: { edit: { isDefault, sections }, detail: {}, list: {} } as EntityLayout["views"] });

  it("default layout (or none loaded as default) puts every field on the form", () => {
    expect([...onForm(lay(undefined, true), fields)!]).toEqual(["name", "email", "note"]);
    expect([...onForm(lay([]), fields)!]).toEqual(["name", "email", "note"]);
  });
  it("custom layout: only placed fields, spacer ignored, visibleWhen still counts as placed", () => {
    const l = lay([{ title: "A", columns: 2, fields: ["name", { spacer: true }, { field: "email", visibleWhen: { field: "name", op: "eq", value: "a" } } as never] }]);
    expect([...onForm(l, fields)!].sort()).toEqual(["email", "name"]);
  });
  it("no layout (null) = unknown, returns null", () => {
    expect(onForm(null, fields)).toBeNull();
  });
});
