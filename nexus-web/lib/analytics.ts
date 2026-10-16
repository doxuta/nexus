// Pure analytics utility functions — shared between dashboard viewer and report viewer.
// All value fields from the API arrive as decimal strings ("250000.0000000000"), so
// every numeric conversion goes through Number(). This module has no side effects.

import type { WidgetRun, AggGroupRow, DashboardWidgetDef } from "./api";

// ── Chart series point (matches what Chart / BarCard accept) ──────────────────
export interface ChartSeriesPoint {
  key: string;
  value: number;
  count: number;
  /** Server-resolved display name (relation/user/picklist axes, specs/049 S6); drill keeps `key`. */
  label?: string;
}

// ── Widget-result → chart data mapping ───────────────────────────────────────

/**
 * Convert a grouped widget result's groups[] into ChartSeriesPoint[].
 * Returns [] if the widget has no groups, is scalar, or errored.
 * Sorts descending by value for a natural bar-chart presentation.
 */
export function widgetGroupsToChartData(w: WidgetRun): ChartSeriesPoint[] {
  if (!w.result?.grouped || !w.result.groups?.length) return [];
  return w.result.groups
    .map((g: AggGroupRow) => ({
      key: g.key || "(trống)",
      value: Number(g.value),
      count: g.count,
    }))
    .sort((a, b) => b.value - a.value);
}

/**
 * REPORT-LABELS (WAVE 6): swap chart-point keys to their DISPLAY labels while keeping a way back
 * to the CODE. Charts use `key` both as the axis text and as the value handed to onSelect, so
 * relabelling naively breaks drill — the list would be filtered by "Báo giá" while records store
 * "Proposal". This helper returns the display points plus `codeOf`, which every drill call must
 * pass through.
 *
 * Two codes sharing one label would make codeOf ambiguous — in that case the SECOND code keeps
 * its raw code as its display key (honest and unambiguous, over pretty and wrong). No labels →
 * identity, so pre-valueLabels tenants render exactly as before.
 */
export function relabelChartData(
  pts: ChartSeriesPoint[],
  labels: Record<string, string> | undefined,
): { pts: ChartSeriesPoint[]; codeOf: (display: string) => string } {
  const hasLabels = !!labels && Object.keys(labels).length > 0;
  if (!hasLabels && !pts.some((p) => p.label)) {
    return { pts, codeOf: (d) => d };
  }
  const back = new Map<string, string>();
  const out = pts.map((p) => {
    let display = p.label ?? labels?.[p.key] ?? p.key;
    if (back.has(display) && back.get(display) !== p.key) display = p.key; // label collision → keep code
    if (!back.has(display)) back.set(display, p.key);
    return { ...p, key: display };
  });
  return { pts: out, codeOf: (d) => back.get(d) ?? d };
}

/**
 * DASH-FUNNEL-ORDER: reorder chart points to follow a picklist field's DECLARED value order
 * (options.values = stage order), not the value-desc default. A funnel over a pipeline stage must
 * read Prospecting→…→Closed Won top-to-bottom to describe the sales process, never sorted by
 * amount. Keys not present in `order` (e.g. a legacy stage, or the "(trống)" null bucket) keep
 * their incoming relative order AFTER the declared ones. `order` empty → returns pts unchanged.
 * Pure + stable (JS Array.sort is stable since ES2019).
 */
export function orderChartByCategory(pts: ChartSeriesPoint[], order: string[]): ChartSeriesPoint[] {
  if (!order.length) return pts;
  const rank = new Map(order.map((v, i) => [v, i]));
  return [...pts].sort(
    (a, b) => (rank.get(a.key) ?? Infinity) - (rank.get(b.key) ?? Infinity),
  );
}

/**
 * Convert aggregate groups (from /aggregate or report run summary/matrix) into ChartSeriesPoint[].
 * Identical shape to AggGroupRow but we name the function differently to be explicit.
 */
export function aggGroupsToChartData(groups: AggGroupRow[]): ChartSeriesPoint[] {
  return groups
    .map((g) => ({
      key: g.key || "(trống)",
      value: Number(g.value),
      count: g.count,
      ...(g.label && { label: g.label }),
    }))
    .sort((a, b) => b.value - a.value);
}

// ── Scalar KPI formatting ─────────────────────────────────────────────────────

/**
 * Format a widget's scalar value based on metric type AND the aggregated field's currency.
 * Value arrives as a decimal string from the API.
 *   - "count" → integer locale string (no field, no currency).
 *   - sum/avg/min/max over a CURRENCY field → the field's own currency (BE sends currencyCode):
 *     VND stays the compact "tỷ/triệu VNĐ" form; any other ISO code → Intl currency (e.g. "US$").
 *   - sum/avg/min/max over a NUMBER/RATING field → currencyCode is empty → a PLAIN number, never
 *     a bogus "VNĐ" tacked onto a non-money value (DASH-CURRENCY). Also the safe fallback when a
 *     caller has not threaded currencyCode through yet.
 */
export function formatWidgetValue(
  raw: string | number | undefined,
  metric: string | undefined,
  currencyCode?: string | null,
): string {
  const n = Number(raw ?? 0);
  if (metric === "count") return n.toLocaleString("vi-VN");
  const code = (currencyCode || "").toUpperCase();
  if (!code) return n.toLocaleString("vi-VN");
  if (code === "VND") return formatCompactVND(n);
  try {
    return new Intl.NumberFormat("vi-VN", { style: "currency", currency: code, maximumFractionDigits: 0 }).format(n);
  } catch {
    return `${Math.round(n).toLocaleString("vi-VN")} ${code}`;
  }
}

// ── Compact VNĐ formatter (matches lib/format.ts convention, duplicated here for tree-shaking) ──
function formatCompactVND(amount: number): string {
  const abs = Math.abs(amount);
  const sign = amount < 0 ? "-" : "";
  if (abs >= 1_000_000_000) {
    return `${sign}${(abs / 1_000_000_000).toLocaleString("vi-VN", { minimumFractionDigits: 2, maximumFractionDigits: 2 })} tỷ VNĐ`;
  }
  if (abs >= 1_000_000) {
    return `${sign}${Math.round(abs / 1_000_000).toLocaleString("vi-VN")} triệu VNĐ`;
  }
  return `${sign}${Math.round(abs).toLocaleString("vi-VN")} VNĐ`;
}

// ── Categorize dashboard widgets by render type ───────────────────────────────

export interface CategorizedWidgets {
  kpis: WidgetRun[];
  bars: WidgetRun[];
  errors: WidgetRun[];
}

export function categorizeWidgets(
  widgets: WidgetRun[],
  defs: Map<string, DashboardWidgetDef>,
): CategorizedWidgets {
  const kpis: WidgetRun[] = [];
  const bars: WidgetRun[] = [];
  const errors: WidgetRun[] = [];

  for (const w of widgets) {
    if (w.error || !w.result) {
      errors.push(w);
    } else if (w.result.grouped) {
      bars.push(w);
    } else {
      kpis.push(w);
    }
    void defs; // defs used by callers for metadata
  }
  return { kpis, bars, errors };
}
