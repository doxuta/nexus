"use client";

// Cross-tab of a Matrix report (specs/049 F1): row headers, column headers, a Total column and a
// Total foot row. SLDS data-tables/base bordered + col-bordered, scrollable on x, first column
// sticky, tabular-nums. Plain table (not the kit DataTable: it has no footer / row-header-per-row).
import { t } from "@/lib/i18n";
import type { PivotGrid } from "@/lib/reportGrid";

export function ReportPivotTable({
  grid, label, rowTitle, columnTitle, format,
}: {
  grid: PivotGrid;
  label: string;
  rowTitle: string;
  columnTitle: string;
  format: (v: string) => string;
}) {
  const total = t("page.reports.viewer.pivot.total");
  const num = "slds-text-align_right tabular-nums";
  const sticky = { position: "sticky", left: 0, background: "var(--slds-g-color-neutral-base-100, #fff)" } as const;
  const show = (v: string | null) => (v == null ? "—" : format(v));
  // A grid with nothing in it (empty filter / every cell under the threshold) is a sentence, not a header-only table.
  if (grid.rows.length === 0 || grid.columns.length === 0) {
    return <p className="slds-p-around_medium slds-text-color_weak">{t("page.reports.viewer.noRange")}</p>;
  }
  return (
    <div className="slds-scrollable_x">
      <table className="slds-table slds-table_bordered slds-table_col-bordered" aria-label={label}>
        <thead>
          <tr className="slds-line-height_reset">
            <th scope="col" style={sticky}>{`${rowTitle} \\ ${columnTitle}`}</th>
            {grid.columns.map((c, i) => (
              <th key={i} scope="col" className={num}>{c.label}</th>
            ))}
            <th scope="col" className={num}>{total}</th>
          </tr>
        </thead>
        <tbody>
          {grid.rows.map((r, i) => (
            <tr key={i} className="slds-hint-parent">
              <th scope="row" style={sticky}>{r.label}</th>
              {r.cells.map((v, j) => (
                <td key={j} className={num}>{show(v)}</td>
              ))}
              <td className={`${num} slds-text-title_bold`}>{show(r.total)}</td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          <tr className="slds-text-title_bold">
            <th scope="row" style={sticky}>{total}</th>
            {grid.columns.map((c, j) => (
              <td key={j} className={num}>{show(c.total)}</td>
            ))}
            <td className={num}>{show(grid.grand)}</td>
          </tr>
        </tfoot>
      </table>
    </div>
  );
}
