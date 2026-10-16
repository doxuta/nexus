import { describe, it, expect } from "vitest";
import { normOps, normParamsFromAction, normPayload, normalizeTargets } from "./workflowNormalize";

describe("normalize action helpers (specs/048 S5)", () => {
  it("open → save keeps ops as an ordered array, never the string 'trim,lower'", () => {
    const stored = { type: "normalize", field: "email", ops: ["trim", "lower"] };
    expect(normPayload(normParamsFromAction(stored))).toEqual(stored);
    expect(normPayload(normParamsFromAction({ ...stored, ops: ["lower", "trim", "collapse_spaces"] })).ops).toEqual(["lower", "trim", "collapse_spaces"]);
  });
  it("a new card has no ops yet; garbage params read as empty", () => {
    expect(normPayload({})).toEqual({ type: "normalize", field: "", ops: [] });
    expect(normOps({ ops: "nope" })).toEqual([]);
  });
  it("only text and email fields are targets", () => {
    const f = (name: string, fieldType: string) => ({ name, label: name, fieldType });
    expect(normalizeTargets([f("a", "text"), f("b", "email"), f("c", "rich_text"), f("d", "picklist")]).map((x) => x.name)).toEqual(["a", "b"]);
  });
});
