import { describe, expect, it } from "vitest";
import type { FieldMeta } from "@/lib/api";
import { setRequiredEdit, withRequiredEdits } from "@/lib/screenEditorModel";

const F = (name: string, isRequired = false): FieldMeta => ({ name, label: name, fieldType: "text", isRequired }) as FieldMeta;
const fields = [F("a", true), F("b"), F("c")];

describe("required edits (L1c)", () => {
  it("setRequiredEdit keeps only edits that differ from the stored flag", () => {
    const on = setRequiredEdit({}, fields[1], true);
    expect(on).toEqual({ b: true });
    expect(setRequiredEdit(on, fields[1], false)).toEqual({});
    expect(withRequiredEdits(fields, on).find((f) => f.name === "b")?.isRequired).toBe(true);
    expect(withRequiredEdits(fields, {})).toBe(fields);
  });
});
