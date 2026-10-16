import { describe, it, expect } from "vitest";
import { metaToSpec, mergeFieldSpec } from "./fieldSpec";
import type { FieldMeta, FieldCell } from "@/lib/api";

const base: FieldMeta = {
  name: "subject",
  label: "Tiêu đề",
  fieldType: "text",
  isRequired: false,
  isUnique: false,
  options: { maxLength: 200 },
};

describe("fieldSpec — metaToSpec + mergeFieldSpec (§2.3)", () => {
  it("metaToSpec: maps API shape → widget FieldSpec", () => {
    const spec = metaToSpec(base);
    expect(spec).toMatchObject({
      name: "subject",
      label: "Tiêu đề",
      fieldType: "text",
      required: false,
      unique: false,
      options: { maxLength: 200 },
    });
  });

  it("metaToSpec: falls back label → name when label missing", () => {
    expect(metaToSpec({ name: "code", fieldType: "text" }).label).toBe("code");
  });

  it("mergeFieldSpec: labelOverride wins over base label", () => {
    const cell: FieldCell = { field: "subject", labelOverride: "Chủ đề ticket" };
    expect(mergeFieldSpec(base, cell).label).toBe("Chủ đề ticket");
  });

  it("mergeFieldSpec: cell can tighten required true", () => {
    const cell: FieldCell = { field: "subject", required: true };
    expect(mergeFieldSpec(base, cell).required).toBe(true);
  });

  it("mergeFieldSpec: readonly override flows through", () => {
    const cell: FieldCell = { field: "subject", readonly: true };
    expect(mergeFieldSpec(base, cell).readonly).toBe(true);
  });

  it("mergeFieldSpec: type/options always come from base meta (not overridable)", () => {
    const cell: FieldCell = { field: "subject", labelOverride: "X" };
    const spec = mergeFieldSpec(base, cell);
    expect(spec.fieldType).toBe("text");
    expect(spec.options).toEqual({ maxLength: 200 });
  });

  it("mergeFieldSpec: defaults required→false, readonly→false when absent", () => {
    const spec = mergeFieldSpec(base, { field: "subject" });
    expect(spec.required).toBe(false);
    expect(spec.readonly).toBe(false);
  });

  // field-lock-signal (DAILY-QUEUE #12): GET /metadata/fields/:entity marks a field the CALLER's
  // role may see but not edit (sys_field_acl can_edit=0) with `readOnly: true`. That lock is a
  // permission, not a layout choice — it must reach CellRenderer's readonly branch (text, no
  // input) no matter what the layout cell says.
  it("mergeFieldSpec: a field-ACL lock (meta.readOnly) renders read-only even with a bare cell", () => {
    const locked: FieldMeta = { ...base, readOnly: true };
    expect(mergeFieldSpec(locked, { field: "subject" }).readonly).toBe(true);
  });

  it("mergeFieldSpec: a layout cell cannot unlock a field-ACL lock", () => {
    const locked: FieldMeta = { ...base, readOnly: true };
    expect(mergeFieldSpec(locked, { field: "subject", readonly: false }).readonly).toBe(true);
  });

  it("mergeFieldSpec: no lock + no cell override stays editable", () => {
    expect(mergeFieldSpec({ ...base, readOnly: undefined }, { field: "subject" }).readonly).toBe(false);
  });
});
