import { describe, it, expect } from "vitest";
import {
  approvalOpsForType,
  criteriaMatch,
  criteriaPayload,
  criteriaToRows,
  invalidCriteriaRow,
} from "./approvalCriteria";
import type { FilterRow } from "./filterModel";

const row = (field: string, op: string, value = ""): FilterRow => ({ id: "t", field, op, value });

describe("criteriaPayload", () => {
  it("serializes the exact BE shape: match 'and' + rules of {field, op, value}", () => {
    expect(criteriaPayload([row("amount", "gt", "10000000"), row("stage", "eq", "Proposal")])).toEqual({
      match: "and",
      rules: [
        { field: "amount", op: "gt", value: "10000000" },
        { field: "stage", op: "eq", value: "Proposal" },
      ],
    });
  });

  it("omits the value KEY entirely for isnull/notnull (a value → 422 APPROVAL_CRITERIA_INVALID)", () => {
    const out = criteriaPayload([row("owner", "isnull"), row("email", "notnull")]);
    expect(out?.rules).toEqual([
      { field: "owner", op: "isnull" },
      { field: "email", op: "notnull" },
    ]);
    for (const r of out!.rules) expect("value" in r).toBe(false);
  });

  it("returns undefined for no rows so the payload OMITS criteria (BE: NULL = match-all)", () => {
    expect(criteriaPayload([])).toBeUndefined();
  });

  it("never emits an 'or' match — approval BE only accepts 'and'", () => {
    expect(criteriaPayload([row("a", "eq", "1")])?.match).toBe("and");
  });
});

describe("criteriaToRows (edit hydration)", () => {
  it("hydrates a stored {match, rules} document into editable rows", () => {
    const rows = criteriaToRows({
      match: "and",
      rules: [
        { field: "amount", op: "gt", value: "10000000" },
        { field: "owner", op: "isnull" },
      ],
    });
    expect(rows.map(({ field, op, value }) => ({ field, op, value }))).toEqual([
      { field: "amount", op: "gt", value: "10000000" },
      { field: "owner", op: "isnull", value: "" },
    ]);
    // FilterBuilder needs a UI key per row.
    expect(rows.every((r) => r.id.length > 0)).toBe(true);
  });

  it("stringifies non-string stored values (another client may store 5 instead of '5')", () => {
    expect(criteriaToRows({ rules: [{ field: "qty", op: "gte", value: 5 }] })[0].value).toBe("5");
  });

  it("hydrates null / absent / malformed criteria to zero rows (áp dụng cho mọi bản ghi)", () => {
    expect(criteriaToRows(null)).toEqual([]);
    expect(criteriaToRows(undefined)).toEqual([]);
    expect(criteriaToRows("junk")).toEqual([]);
    expect(criteriaToRows({ match: "and" })).toEqual([]);
    expect(criteriaToRows({ rules: [{ op: "eq" }, null, 7] })).toEqual([]);
  });

  it("round-trips: hydrate then serialize reproduces the stored document", () => {
    const stored = {
      match: "and",
      rules: [
        { field: "amount", op: "lte", value: "500" },
        { field: "note", op: "notnull" },
      ],
    };
    expect(criteriaPayload(criteriaToRows(stored))).toEqual(stored);
  });
});

// specs/043 S2b: the BE now stores match "or" and the edge operators (changed/changedTo, after_update
// only). This screen cannot author them yet (FE-BPM-PROPS-b), but it must not DESTROY them: saving a
// process configured via the API used to rewrite its OR criteria as AND, silently.
describe("round-trip of criteria this screen does not author (S2b)", () => {
  it("keeps match 'or' through hydrate → save", () => {
    const stored = { match: "or", rules: [{ field: "total", op: "gt", value: "50000000" }, { field: "discount", op: "gt", value: "20" }] };
    expect(criteriaMatch(stored)).toBe("or");
    expect(criteriaPayload(criteriaToRows(stored), criteriaMatch(stored))).toEqual(stored);
  });

  it("reads anything but 'or' as AND (absent, 'and', junk)", () => {
    for (const raw of [null, undefined, "junk", {}, { match: "and" }, { match: "xor" }]) {
      expect(criteriaMatch(raw)).toBe("and");
    }
  });

  it("'changed' carries no value — it neither blocks the save nor sends an operand", () => {
    const rows = [row("status", "changed"), row("stage", "changedTo", "Chờ duyệt")];
    expect(invalidCriteriaRow(rows)).toBeNull();
    expect(criteriaPayload(rows)?.rules).toEqual([
      { field: "status", op: "changed" },
      { field: "stage", op: "changedTo", value: "Chờ duyệt" },
    ]);
    expect(invalidCriteriaRow([row("stage", "changedTo", " ")])).not.toBeNull();
  });

  it("a 'changed' row with a typed value blocks the save instead of dropping the value", () => {
    // This screen has no label for `changed`, so a stored one renders as "= [ô giá trị]"; a value
    // typed there used to vanish from the payload while the screen said "Đã lưu".
    expect(invalidCriteriaRow([row("note", "changed", "khach VIP")])).not.toBeNull();
    expect(invalidCriteriaRow([row("note", "changed", "  ")])).toBeNull();
  });
});

describe("approvalOpsForType", () => {
  it("never offers in/between — buildApprovalCriteriaJSON rejects anything outside the 9 workflow ops", () => {
    for (const ft of ["text", "number", "currency", "date", "datetime", "picklist", "multi_select", "boolean", "relation"]) {
      const ops = approvalOpsForType(ft);
      expect(ops).not.toContain("in");
      expect(ops).not.toContain("between");
    }
  });

  it("keeps the per-type spirit of the record-list map", () => {
    expect(approvalOpsForType("number")).toEqual(["eq", "neq", "gt", "gte", "lt", "lte", "isnull", "notnull"]);
    expect(approvalOpsForType("text")).toEqual(["eq", "neq", "contains", "isnull", "notnull"]);
    expect(approvalOpsForType("boolean")).toEqual(["eq", "isnull", "notnull"]);
  });
});

describe("invalidCriteriaRow (pre-save guard)", () => {
  it("flags a row missing its field or a blank operand on a value operator", () => {
    expect(invalidCriteriaRow([row("", "eq", "x")])).not.toBeNull();
    expect(invalidCriteriaRow([row("stage", "eq", "  ")])).not.toBeNull();
  });

  it("accepts null-operators without a value and complete rows", () => {
    expect(invalidCriteriaRow([row("owner", "isnull"), row("amount", "gt", "1")])).toBeNull();
    expect(invalidCriteriaRow([])).toBeNull();
  });
});
