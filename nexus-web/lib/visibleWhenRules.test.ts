import { describe, expect, it } from "vitest";
import type { FieldMeta, VisibleWhen } from "@/lib/api";
import type { DraftSection } from "@/lib/layoutDraft";
import {
  clearDependents, controllerChoices, dependentsOf, rowToVisibleWhen, visibleWhenToRow, whenLockedReason,
} from "./visibleWhenRules";

const f = (name: string, fieldType: string, extra: Partial<FieldMeta> = {}): FieldMeta => ({ name, label: name, fieldType, ...extra });
const fields: FieldMeta[] = [
  f("tax", "text"), f("kind", "picklist", { options: { values: ["person", "biz"] } }), f("vip", "boolean"),
  f("notes", "textarea"), f("born", "date"), f("level", "number"), f("off", "text"),
];
const cell = (field: string, extra: Record<string, unknown> = {}) => ({ id: "c-" + field, kind: "field" as const, field, ...extra });
const sec = (...cells: ReturnType<typeof cell>[]): DraftSection[] => [{ id: "s1", title: "", columns: 2, cells }];

describe("controllerChoices (FR-3/FR-5)", () => {
  it("lists only single-valued types on the form, never the target itself", () => {
    const d = sec(cell("tax"), cell("kind"), cell("vip"), cell("notes"), cell("born"), cell("level"));
    expect(controllerChoices("tax", fields, d).map((x) => x.name)).toEqual(["kind", "vip", "level"]);
  });
  it("skips fields not on the form", () => {
    expect(controllerChoices("tax", fields, sec(cell("tax"), cell("kind"))).map((x) => x.name)).toEqual(["kind"]);
  });
  it("skips a field whose own chain leads back to the target (cycle)", () => {
    const d = sec(cell("tax"), cell("kind", { visibleWhen: { field: "tax", op: "notEmpty" } }), cell("vip"));
    expect(controllerChoices("tax", fields, d).map((x) => x.name)).toEqual(["vip"]);
  });
});

describe("whenLockedReason (FR-6)", () => {
  it("locks a required field and a cell marked required", () => {
    expect(whenLockedReason(f("a", "text", { isRequired: true }), cell("a"))).toBe("field");
    expect(whenLockedReason(f("a", "text"), cell("a", { required: true }))).toBe("cell");
    expect(whenLockedReason(f("a", "text"), cell("a"))).toBeNull();
  });
});

describe("row <-> visibleWhen mapping", () => {
  const kind = f("kind", "picklist");
  const cases: Array<[VisibleWhen, { field: string; op: string; value: string }]> = [
    [{ field: "kind", op: "eq", value: "biz" }, { field: "kind", op: "eq", value: "biz" }],
    [{ field: "kind", op: "neq", value: "biz" }, { field: "kind", op: "neq", value: "biz" }],
    [{ field: "kind", op: "in", value: ["a", "b"] }, { field: "kind", op: "in", value: "a,b" }],
    [{ field: "kind", op: "empty" }, { field: "kind", op: "isnull", value: "" }],
    [{ field: "kind", op: "notEmpty" }, { field: "kind", op: "notnull", value: "" }],
  ];
  it.each(cases)("round-trips %j", (vw, row) => {
    expect(visibleWhenToRow(vw, "r1")).toEqual({ id: "r1", ...row });
    expect(rowToVisibleWhen({ id: "r1", ...row }, kind)).toEqual(vw);
  });
  it("types the value by field: number, boolean", () => {
    expect(rowToVisibleWhen({ id: "x", field: "level", op: "eq", value: "5" }, f("level", "number"))).toEqual({ field: "level", op: "eq", value: 5 });
    expect(rowToVisibleWhen({ id: "x", field: "vip", op: "eq", value: "true" }, f("vip", "boolean"))).toEqual({ field: "vip", op: "eq", value: true });
  });
  it("incomplete row (no field / blank value) yields null", () => {
    expect(rowToVisibleWhen({ id: "x", field: "", op: "eq", value: "" }, undefined)).toBeNull();
    expect(rowToVisibleWhen({ id: "x", field: "kind", op: "eq", value: " " }, kind)).toBeNull();
  });
  it("notIn is not editable", () => {
    expect(visibleWhenToRow({ field: "kind", op: "notIn", value: ["a"] }, "r")).toBeNull();
  });
});

describe("dependents of a controller", () => {
  const d = sec(cell("kind"), cell("tax", { visibleWhen: { field: "kind", op: "eq", value: "biz" } }), cell("vip"));
  it("names them and clears their conditions", () => {
    expect(dependentsOf(d, "kind")).toEqual(["tax"]);
    expect(dependentsOf(d, "vip")).toEqual([]);
    expect(dependentsOf(clearDependents(d, "kind"), "kind")).toEqual([]);
  });
});
