// Dense cross-tab from EXACTLY the rows/columns/cells the server returned (specs/049 F1, T085).
// No zero-fill of bucket columns: a month with no data is not a column here, as in the CSV of S4 —
// a screen with a column the file lacks would break "the file is the grid on screen" (#51).
import type { ReportRunData } from "@/lib/api";
import { t } from "@/lib/i18n";

export interface PivotHead { label: string; total: string | null }
export interface PivotRow extends PivotHead { cells: (string | null)[] }
export interface PivotGrid { rows: PivotRow[]; columns: PivotHead[]; grand: string | null }

interface Header { key?: string | null; label?: string | null }
// Server-resolved name → key → "(trống)" for the NULL group — the same order as the CSV.
const name = (h: Header) => h.label ?? h.key ?? t("page.reports.viewer.pivot.blank");

/** null when the result is not a cross-tab (no column axis). */
export function buildPivotGrid(data: ReportRunData): PivotGrid | null {
  if (data.type === "tabular" || !data.columnAxis || !data.rows || !data.columns) return null;
  const cols = data.columns;
  const rows: PivotRow[] = data.rows.map((r) => ({
    label: name(r), total: r.value ?? null, cells: cols.map(() => null),
  }));
  for (const c of data.cells ?? []) {
    if (rows[c.r] && c.c >= 0 && c.c < cols.length) rows[c.r].cells[c.c] = c.value ?? null;
  }
  return {
    rows,
    columns: cols.map((c) => ({ label: name(c), total: c.value ?? null })),
    grand: data.total?.value ?? null,
  };
}

// ── Tree (specs/049 F2): levelled groups → TreeGrid rows ───────────────────────────
export interface TreeNode {
  /** Stable across runs: the key path, so an expanded row stays expanded after a refetch. */
  id: string;
  label: string;
  /** Server key — what a drill filters by (never the display label). */
  code: string;
  value: string | null;
  count: number;
  /** 0 = first level. */
  depth: number;
  children?: TreeNode[];
  /** The closing "Tổng cộng" row: a leaf, never a parent. */
  total?: boolean;
}
export interface ReportTree { nodes: TreeNode[]; empty: boolean }

/**
 * Null unless the result carries `levels` (a levelled tree, or the one-level threshold path N=1) and no
 * column axis. Each group row IS its subtotal; the grand total closes the list as a leaf. A threshold
 * that removed every group (`groups: []`) is `empty` with no total row — a "Tổng cộng 0" would read as data.
 */
export function buildReportTree(data: ReportRunData, firstLevelLabels?: Record<string, string>): ReportTree | null {
  if (data.type === "tabular" || !data.levels?.length || data.columnAxis || data.cells) return null;
  const build = (gs: NonNullable<typeof data.groups>, path: string[], depth: number): TreeNode[] =>
    gs.map((g) => {
      const key = g.key ?? "";
      const kids = g.children?.length ? build(g.children, [...path, key], depth + 1) : undefined;
      return {
        id: JSON.stringify([...path, key]), code: key, depth, value: g.value ?? null, count: g.count,
        label: g.label ?? (depth === 0 ? firstLevelLabels?.[key] : undefined) ?? (key || t("page.reports.viewer.pivot.blank")),
        ...(kids && { children: kids }),
      };
    });
  const nodes = build(data.groups ?? [], [], 0);
  if (nodes.length === 0) return { nodes, empty: true };
  nodes.push({
    id: "__total__", code: "", label: t("page.reports.viewer.tree.total"), depth: 0,
    value: data.total?.value ?? null, count: data.total?.count ?? 0, total: true,
  });
  return { nodes, empty: false };
}
