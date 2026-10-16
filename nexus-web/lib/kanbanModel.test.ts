// lib/kanbanModel.test.ts — W9c Kanban pure-model contract:
//   1. pickKanbanField — first picklist by default; explicit preference wins when valid.
//   2. columnsForField — columns follow picklist DECLARATION order, empty bucket LAST.
//   3. groupRecords — known values grouped; empty/null/stale values land in the empty bucket.
//   4. moveReducer — the optimistic-move + rollback state machine (begin → commit/rollback).

import { describe, it, expect } from "vitest";
import type { ApiRecord, FieldMeta } from "@/lib/api";
import {
  EMPTY_BUCKET,
  EMPTY_LABEL,
  pickKanbanField,
  fieldValues,
  columnsForField,
  columnOf,
  groupRecords,
  moveRecordValue,
  moveReducer,
  type MoveState,
} from "@/lib/kanbanModel";

const stageField: FieldMeta = {
  name: "stage",
  label: "Giai đoạn",
  fieldType: "picklist",
  isRequired: true,
  options: { values: ["Prospecting", "Qualification", "Proposal", "Negotiation", "Closed Won"] },
};
const nameField: FieldMeta = { name: "name", fieldType: "text" };
const prioField: FieldMeta = {
  name: "priority",
  fieldType: "picklist",
  options: { values: ["Low", "High"] },
};

function rec(id: string, data: Record<string, unknown>): ApiRecord {
  return { id, data, version: 1 };
}

describe("pickKanbanField", () => {
  it("returns the first picklist field by default", () => {
    expect(pickKanbanField([nameField, stageField, prioField])?.name).toBe("stage");
  });
  it("honours an explicit preference when it names a picklist field", () => {
    expect(pickKanbanField([nameField, stageField, prioField], "priority")?.name).toBe("priority");
  });
  it("falls back to the first picklist when the preference is not a picklist", () => {
    expect(pickKanbanField([nameField, stageField], "name")?.name).toBe("stage");
  });
  it("returns undefined when the entity has no picklist field", () => {
    expect(pickKanbanField([nameField])).toBeUndefined();
  });
});

describe("fieldValues", () => {
  it("reads a plain string[] verbatim", () => {
    expect(fieldValues(prioField)).toEqual(["Low", "High"]);
  });
  it("coerces {value} object options and de-dupes / drops empties", () => {
    const f: FieldMeta = {
      name: "s",
      fieldType: "picklist",
      options: { values: [{ value: "A" }, "B", "A", ""] as unknown[] },
    };
    expect(fieldValues(f)).toEqual(["A", "B"]);
  });
  it("returns [] when there are no options", () => {
    expect(fieldValues(nameField)).toEqual([]);
  });
});

describe("columnsForField", () => {
  it("columns follow picklist declaration order, with the empty bucket appended LAST", () => {
    const cols = columnsForField(stageField);
    expect(cols.map((c) => c.key)).toEqual([
      "Prospecting",
      "Qualification",
      "Proposal",
      "Negotiation",
      "Closed Won",
      EMPTY_BUCKET,
    ]);
    expect(cols[cols.length - 1]).toEqual({ key: EMPTY_BUCKET, label: EMPTY_LABEL, isEmpty: true });
    expect(cols.slice(0, -1).every((c) => !c.isEmpty && c.label === c.key)).toBe(true);
  });
  it("still yields a single empty bucket when the field has no values", () => {
    expect(columnsForField(nameField)).toEqual([
      { key: EMPTY_BUCKET, label: EMPTY_LABEL, isEmpty: true },
    ]);
  });
});

describe("columnOf / groupRecords", () => {
  const cols = columnsForField(stageField);
  const valid = ["Prospecting", "Qualification", "Proposal", "Negotiation", "Closed Won"];

  it("maps a known value to its own column", () => {
    expect(columnOf(rec("1", { stage: "Proposal" }), "stage", valid)).toBe("Proposal");
  });
  it("maps null / empty / stale (undeclared) values to the empty bucket", () => {
    expect(columnOf(rec("2", { stage: null }), "stage", valid)).toBe(EMPTY_BUCKET);
    expect(columnOf(rec("3", { stage: "" }), "stage", valid)).toBe(EMPTY_BUCKET);
    expect(columnOf(rec("4", {}), "stage", valid)).toBe(EMPTY_BUCKET);
    expect(columnOf(rec("5", { stage: "Archived" }), "stage", valid)).toBe(EMPTY_BUCKET);
  });

  it("groups records into every column, empty bucket included, order preserved", () => {
    const records = [
      rec("a", { stage: "Proposal" }),
      rec("b", { stage: "Negotiation" }),
      rec("c", { stage: "Proposal" }),
      rec("d", { stage: null }),
      rec("e", { stage: "Ghost" }),
    ];
    const g = groupRecords(records, "stage", cols);
    // every declared column key + the empty bucket is present as a (possibly empty) array
    expect(Object.keys(g).sort()).toEqual(
      [...valid, EMPTY_BUCKET].sort(),
    );
    expect(g["Proposal"].map((r) => r.id)).toEqual(["a", "c"]); // insertion order kept
    expect(g["Negotiation"].map((r) => r.id)).toEqual(["b"]);
    expect(g["Qualification"]).toEqual([]);
    expect(g[EMPTY_BUCKET].map((r) => r.id)).toEqual(["d", "e"]); // null + stale both here
  });
});

describe("moveRecordValue", () => {
  it("sets the field immutably (original array + records untouched)", () => {
    const records = [rec("a", { stage: "Proposal", name: "Acme" })];
    const next = moveRecordValue(records, "a", "stage", "Negotiation");
    expect(next).not.toBe(records);
    expect(next[0]).not.toBe(records[0]);
    expect(next[0].data.stage).toBe("Negotiation");
    expect(next[0].data.name).toBe("Acme"); // other fields preserved
    expect(records[0].data.stage).toBe("Proposal"); // snapshot untouched
  });
  it("clears the field with null (drag to the empty bucket)", () => {
    const next = moveRecordValue([rec("a", { stage: "Proposal" })], "a", "stage", null);
    expect(next[0].data.stage).toBeNull();
  });
  it("is a no-op copy when the card id is not present", () => {
    const records = [rec("a", { stage: "Proposal" })];
    expect(moveRecordValue(records, "zzz", "stage", "X")[0].data.stage).toBe("Proposal");
  });
});

describe("moveReducer — optimistic move + rollback state machine", () => {
  const cols = columnsForField(stageField);
  const start = (): MoveState => ({
    records: [rec("a", { stage: "Proposal" }), rec("b", { stage: "Negotiation" })],
    pending: null,
  });
  const colOf = (s: MoveState, id: string) => {
    const g = groupRecords(s.records, "stage", cols);
    return Object.keys(g).find((k) => g[k].some((r) => r.id === id));
  };

  it("begin moves the card optimistically and snapshots for rollback", () => {
    const s0 = start();
    const s1 = moveReducer(s0, { type: "begin", cardId: "a", fieldName: "stage", newValue: "Negotiation" });
    expect(colOf(s1, "a")).toBe("Negotiation"); // card visually moved
    expect(s1.pending).not.toBeNull();
    expect(s1.pending?.snapshot).toBe(s0.records); // snapshot === pre-move array (rollback target)
  });

  it("commit keeps the moved value and clears pending (success path)", () => {
    const s1 = moveReducer(start(), { type: "begin", cardId: "a", fieldName: "stage", newValue: "Negotiation" });
    const s2 = moveReducer(s1, { type: "commit" });
    expect(colOf(s2, "a")).toBe("Negotiation");
    expect(s2.pending).toBeNull();
  });

  it("rollback restores the pre-move snapshot and clears pending (403/409/422 path)", () => {
    const s0 = start();
    const s1 = moveReducer(s0, { type: "begin", cardId: "a", fieldName: "stage", newValue: "Negotiation" });
    const s2 = moveReducer(s1, { type: "rollback" });
    expect(colOf(s2, "a")).toBe("Proposal"); // card snapped back
    expect(s2.records).toBe(s0.records); // exact snapshot restored
    expect(s2.pending).toBeNull();
  });

  it("rollback is a no-op when nothing is pending", () => {
    const s0 = start();
    expect(moveReducer(s0, { type: "rollback" })).toBe(s0);
  });

  it("reset reseeds from fresh server records and drops any pending", () => {
    const s1 = moveReducer(start(), { type: "begin", cardId: "a", fieldName: "stage", newValue: "Negotiation" });
    const fresh = [rec("a", { stage: "Closed Won" })];
    const s2 = moveReducer(s1, { type: "reset", records: fresh });
    expect(s2.records).toBe(fresh);
    expect(s2.pending).toBeNull();
  });
});
