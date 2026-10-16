import { describe, it, expect } from "vitest";
import type { ReportRunData } from "@/lib/api";
import { buildPivotGrid, buildReportTree } from "./reportGrid";

type Matrix = Extract<ReportRunData, { type: "summary" | "matrix" }>;
const base: Matrix = {
  type: "matrix",
  rowAxis: { field: "stage" },
  columnAxis: { field: "close_date", bucket: "month" },
  rows: [
    { key: "Negotiation", label: "Đàm phán", value: "2000.5", count: 1 },
    { key: "Proposal", value: "1000.25", count: 1 },
  ],
  columns: [
    { key: "2026-01", value: "1000.25", count: 1 },
    { key: "2026-02", value: "2000.5", count: 1 },
  ],
  cells: [
    { r: 0, c: 1, value: "2000.5", count: 1 },
    { r: 1, c: 0, value: "1000.25", count: 1 },
  ],
  total: { value: "3000.75", count: 2 },
};

describe("buildPivotGrid", () => {
  it("lays the server rows/columns/cells out densely, label first, key as fallback", () => {
    const g = buildPivotGrid(base)!;
    expect(g.rows.map((r) => r.label)).toEqual(["Đàm phán", "Proposal"]);
    expect(g.columns.map((c) => c.label)).toEqual(["2026-01", "2026-02"]);
    expect(g.rows[0].cells).toEqual([null, "2000.5"]);
    expect(g.rows[1].cells).toEqual(["1000.25", null]);
    expect(g.rows.map((r) => r.total)).toEqual(["2000.5", "1000.25"]);
    expect(g.columns.map((c) => c.total)).toEqual(["1000.25", "2000.5"]);
    expect(g.grand).toBe("3000.75");
  });

  it("does NOT zero-fill a bucket the server did not return (file = screen)", () => {
    const g = buildPivotGrid({ ...base, columns: [base.columns![0], { key: "2026-04", value: "1", count: 1 }], cells: [] })!;
    expect(g.columns.map((c) => c.label)).toEqual(["2026-01", "2026-04"]);
  });

  it("names a NULL key (trống)", () => {
    const g = buildPivotGrid({ ...base, rows: [{ key: null as unknown as string, value: "1", count: 1 }], cells: [] })!;
    expect(g.rows[0].label).toBe("(trống)");
  });

  it("ignores a cell outside the grid and returns null for non-grids", () => {
    const g = buildPivotGrid({ ...base, cells: [{ r: 9, c: 9, value: "1", count: 1 }] })!;
    expect(g.rows.every((r) => r.cells.every((c) => c === null))).toBe(true);
    expect(buildPivotGrid({ type: "summary", groups: [] })).toBeNull();
  });
});

describe("buildReportTree (specs/049 F2, T090)", () => {
  const tree: Matrix = {
    type: "summary",
    levels: [{ field: "stage" }, { field: "close_date", bucket: "month" }],
    groups: [
      { key: "Negotiation", label: "Đàm phán", value: "2000.5", count: 1, children: [{ key: "2026-02", value: "2000.5", count: 1 }] },
      {
        key: "Proposal", value: "1600", count: 3,
        children: [{ key: "2026-01", value: "1100", count: 2 }, { key: "2026-02", value: "500", count: 1 }],
      },
    ],
    total: { value: "3600.5", count: 4 },
  };

  it("makes nested rows with a stable id per key path and a closing Tổng cộng that never expands", () => {
    const t = buildReportTree(tree)!;
    expect(t.nodes.map((n) => n.label)).toEqual(["Đàm phán", "Proposal", "Tổng cộng"]);
    expect(t.nodes[1].children!.map((n) => n.label)).toEqual(["2026-01", "2026-02"]);
    expect(t.nodes[1].value).toBe("1600"); // the subtotal is the group's own row
    // "2026-02" under two parents must not share an id
    const ids = t.nodes.flatMap((n) => [n.id, ...(n.children ?? []).map((c) => c.id)]);
    expect(new Set(ids).size).toBe(ids.length);
    expect(t.nodes[2]).toMatchObject({ value: "3600.5", count: 4, total: true });
    expect(t.nodes[2].children).toBeUndefined();
    expect(buildReportTree(tree)!.nodes[1].id).toBe(t.nodes[1].id); // stable across builds
  });

  it("a one-level result that has levels (threshold path, N=1) is a flat tree + Tổng cộng", () => {
    const t = buildReportTree({
      type: "summary", levels: [{ field: "stage" }],
      groups: [{ key: "Negotiation", value: "600.25", count: 1 }], total: { value: "600.25", count: 1 },
    })!;
    expect(t.nodes.map((n) => n.label)).toEqual(["Negotiation", "Tổng cộng"]);
    expect(t.nodes[0].children).toBeUndefined();
    expect(t.empty).toBe(false);
  });

  it("a threshold that removes everything: empty state, no Tổng cộng row", () => {
    const t = buildReportTree({
      type: "summary", levels: [{ field: "stage" }], groups: [], total: { value: null as unknown as string, count: 0 },
    })!;
    expect(t.empty).toBe(true);
    expect(t.nodes).toEqual([]);
  });

  it("uses the code→label map for the first level when the server sent no label; NULL is (trống)", () => {
    const t = buildReportTree(
      { type: "summary", levels: [{ field: "stage" }], groups: [{ key: "Proposal", value: "1", count: 1 }, { key: "", value: "1", count: 1 }], total: { value: "2", count: 2 } },
      { Proposal: "Báo giá" },
    )!;
    expect(t.nodes.map((n) => n.label)).toEqual(["Báo giá", "(trống)", "Tổng cộng"]);
    expect(t.nodes[0].code).toBe("Proposal");
  });

  it("is null for a result without levels, a cross-tab and a tabular run", () => {
    expect(buildReportTree({ type: "summary", groups: [{ key: "a", value: "1", count: 1 }] })).toBeNull();
    expect(buildReportTree(base)).toBeNull();
    expect(buildReportTree({ type: "tabular", columns: [], rows: [], total: 0 })).toBeNull();
  });
});
