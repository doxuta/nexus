import { describe, it, expect } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { ReportPivotTable } from "./ReportPivotTable";
import type { PivotGrid } from "@/lib/reportGrid";

const grid: PivotGrid = {
  rows: [
    { label: "Đàm phán", total: "2000", cells: [null, "2000"] },
    { label: "Báo giá", total: "1000", cells: ["1000", null] },
  ],
  columns: [{ label: "2026-01", total: "1000" }, { label: "2026-02", total: "2000" }],
  grand: "3000",
};

describe("ReportPivotTable", () => {
  it("draws row/column headers, a Total column, a Total foot row and the grand total", () => {
    render(<ReportPivotTable grid={grid} label="Bảng chéo" rowTitle="Giai đoạn" columnTitle="Ngày chốt" format={(v) => `#${v}`} />);
    const table = screen.getByRole("table", { name: "Bảng chéo" });
    const rowHeads = within(table).getAllByRole("rowheader");
    expect(rowHeads.map((h) => h.textContent)).toEqual(["Đàm phán", "Báo giá", "Tổng"]);
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Giai đoạn \\ Ngày chốt", "2026-01", "2026-02", "Tổng",
    ]);
    const foot = table.querySelector("tfoot")!;
    expect(foot.textContent).toContain("#3000");
    expect(within(table).getAllByText("—")).toHaveLength(2); // the two empty cells
  });

  it("puts the grand total in the foot row's last cell, apart from the column totals", () => {
    const g: PivotGrid = { ...grid, grand: "4242" };
    render(<ReportPivotTable grid={g} label="Bảng chéo" rowTitle="R" columnTitle="C" format={(v) => `#${v}`} />);
    const foot = screen.getByRole("table").querySelector("tfoot tr")!;
    const cells = foot.querySelectorAll("td");
    expect(cells[cells.length - 1].textContent).toBe("#4242");
    expect(cells[0].textContent).toBe("#1000");
  });

  it("scopes headers: columns scope=col, rows scope=row; null totals draw a dash", () => {
    const g: PivotGrid = { ...grid, grand: null, columns: [{ label: "a", total: null }, { label: "b", total: null }] };
    render(<ReportPivotTable grid={g} label="Bảng chéo" rowTitle="R" columnTitle="C" format={(v) => v} />);
    const table = screen.getByRole("table");
    expect([...table.querySelectorAll("thead th")].every((h) => h.getAttribute("scope") === "col")).toBe(true);
    expect([...table.querySelectorAll("tbody th, tfoot th")].every((h) => h.getAttribute("scope") === "row")).toBe(true);
    const foot = table.querySelector("tfoot tr")!;
    expect([...foot.querySelectorAll("td")].map((d) => d.textContent)).toEqual(["—", "—", "—"]);
  });

  it("shows an empty-state sentence, not a header-only table, when the grid has no rows or columns", () => {
    render(<ReportPivotTable grid={{ rows: [], columns: [], grand: null }} label="Bảng chéo" rowTitle="R" columnTitle="C" format={(v) => v} />);
    expect(screen.queryByRole("table")).toBeNull();
    expect(screen.getByText("Chưa có dữ liệu trong khoảng đã chọn.")).toBeTruthy();
  });
});
