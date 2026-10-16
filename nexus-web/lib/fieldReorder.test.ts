// fieldReorder — unit tests for the W5 optimistic drag-reorder model.
import { describe, it, expect } from "vitest";
import { applyMove, reorderPayload, orderChanged } from "./fieldReorder";
import type { FieldMeta } from "./api";

const f = (name: string, id?: string): FieldMeta => ({ name, id, fieldType: "text" });

describe("fieldReorder", () => {
  it("applyMove moves the dragged field to the over field's slot", () => {
    const fields = [f("a", "1"), f("b", "2"), f("c", "3")];
    expect(applyMove(fields, "a", "c").map((x) => x.name)).toEqual(["b", "c", "a"]);
    expect(applyMove(fields, "c", "a").map((x) => x.name)).toEqual(["c", "a", "b"]);
  });

  it("applyMove is identity for unknown names / same slot (no useless re-render)", () => {
    const fields = [f("a", "1"), f("b", "2")];
    expect(applyMove(fields, "zz", "a")).toBe(fields);
    expect(applyMove(fields, "a", "a")).toBe(fields);
  });

  it("reorderPayload sends the FULL id list in display order (contract §10.2)", () => {
    const fields = [f("b", "2"), f("a", "1"), f("c", "3")];
    expect(reorderPayload(fields)).toEqual(["2", "1", "3"]);
  });

  it("reorderPayload drops id-less fields instead of sending empty strings", () => {
    const fields = [f("a", "1"), f("ghost"), f("c", "3")];
    expect(reorderPayload(fields)).toEqual(["1", "3"]);
  });

  it("orderChanged detects moves and is false for identical order (skip no-op PUT)", () => {
    const before = [f("a", "1"), f("b", "2")];
    expect(orderChanged(before, [f("a", "1"), f("b", "2")])).toBe(false);
    expect(orderChanged(before, [f("b", "2"), f("a", "1")])).toBe(true);
    expect(orderChanged(before, [f("a", "1")])).toBe(true);
  });

  it("optimistic rollback model: snapshot before move restores exactly", () => {
    const snapshot = [f("a", "1"), f("b", "2"), f("c", "3")];
    const optimistic = applyMove(snapshot, "a", "c");
    expect(optimistic).not.toBe(snapshot);
    // A 422 rollback = setFields(snapshot); the snapshot was never mutated by applyMove.
    expect(snapshot.map((x) => x.name)).toEqual(["a", "b", "c"]);
  });
});
