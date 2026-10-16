// lib/savedFilters — the saved {field, op, values[]} filters of a report / dashboard widget
// (report_service.go ReportFilterInput) ↔ the expression builder's FilterRow (lib/filterModel),
// specs/070 #266. The split must match how the list handler splits ?field[op]=value
// (dynamic_record_handler.go): in/between comma-split with blanks dropped, isnull/notnull carry no
// value, any other operator exactly one.
import { describe, expect, it } from "vitest";
import { filterRowIssues, filterRowsFromSaved, savedFromFilterRows } from "./savedFilters";

const strip = (rows: ReturnType<typeof filterRowsFromSaved>) => rows.map((r) => ({ field: r.field, op: r.op, value: r.value }));

describe("filterRowsFromSaved", () => {
  it("maps each saved operator to one builder row", () => {
    const rows = filterRowsFromSaved([
      { field: "stage", op: "neq", values: ["Done"] },
      { field: "due_date", op: "lt", values: ["today"] },
      { field: "stage", op: "in", values: ["Won", "Lost"] },
      { field: "amount", op: "between", values: ["10", "20"] },
      { field: "phone", op: "isnull" },
      { field: "legacy", values: ["x"] }, // op omitted = eq (the handler's ?field=value default)
    ]);
    expect(strip(rows)).toEqual([
      { field: "stage", op: "neq", value: "Done" },
      { field: "due_date", op: "lt", value: "today" },
      { field: "stage", op: "in", value: "Won,Lost" },
      { field: "amount", op: "between", value: "10,20" },
      { field: "phone", op: "isnull", value: "" },
      { field: "legacy", op: "eq", value: "x" },
    ]);
    expect(new Set(rows.map((r) => r.id)).size).toBe(rows.length);
  });

  it("tolerates a missing or malformed saved value", () => {
    expect(filterRowsFromSaved(undefined)).toEqual([]);
    expect(filterRowsFromSaved("nope")).toEqual([]);
    expect(strip(filterRowsFromSaved([{ field: "x", op: "eq" }, null, { op: "eq" }]))).toEqual([{ field: "x", op: "eq", value: "" }]);
  });
});

describe("savedFromFilterRows", () => {
  it("is the inverse for every operator, and drops rows with nothing typed", () => {
    const saved = [
      { field: "stage", op: "neq", values: ["Done"] },
      { field: "close_date", op: "eq", values: ["this_month"] },
      { field: "stage", op: "in", values: ["Won", "Lost"] },
      { field: "amount", op: "between", values: ["10", "20"] },
      { field: "phone", op: "notnull" },
    ];
    const rows = [...filterRowsFromSaved(saved), { id: "blank", field: "", op: "", value: "" }];
    expect(savedFromFilterRows(rows)).toEqual(saved);
  });

  it("trims values and drops blank members of a list", () => {
    expect(savedFromFilterRows([
      { id: "1", field: "name", op: "contains", value: "  QS-1 " },
      { id: "2", field: "stage", op: "in", value: "Won, ,Lost ," },
    ])).toEqual([
      { field: "name", op: "contains", values: ["QS-1"] },
      { field: "stage", op: "in", values: ["Won", "Lost"] },
    ]);
  });
});

describe("filterRowIssues", () => {
  it("flags a half-typed condition instead of saving without it", () => {
    const issues = filterRowIssues([
      { id: "ok", field: "stage", op: "eq", value: "Won" },
      { id: "empty", field: "stage", op: "eq", value: " " },
      { id: "half", field: "amount", op: "between", value: "10," },
      { id: "null", field: "phone", op: "isnull", value: "" },
      { id: "untouched", field: "", op: "", value: "" },
    ]);
    expect(Object.keys(issues).sort()).toEqual(["empty", "half"]);
    expect(issues.empty).toBe("Nhập giá trị");
    expect(issues.half).toBe("Nhập đủ hai đầu khoảng");
  });
});

describe("saved filters survive a round trip untouched (#451)", () => {
  const saved = [
    { field: "company", op: "in", values: ["Công ty A, B"] },
    { field: "stage", op: "neq", values: ["Done"] },
  ];
  it("keeps a comma-bearing value byte-for-byte when the row is not edited", () => {
    const rows = filterRowsFromSaved(saved);
    expect(JSON.stringify(savedFromFilterRows(rows, saved))).toBe(JSON.stringify(saved));
  });
  it("still re-splits a row the user edited", () => {
    const rows = filterRowsFromSaved(saved);
    rows[0].value = "X,Y";
    expect(savedFromFilterRows(rows, saved)[0]).toEqual({ field: "company", op: "in", values: ["X", "Y"] });
  });
  it("flags an in with no members and a between with more than two ends", () => {
    const issues = filterRowIssues([
      { id: "a", field: "f", op: "in", value: " , " },
      { id: "b", field: "g", op: "between", value: "1,2,3" },
    ]);
    expect(Object.keys(issues).sort()).toEqual(["a", "b"]);
  });
});
