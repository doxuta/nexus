"use client";

// Nx Charts — shared theme + tooltip (W1a, plan §4.2 "Nx Charts").
// SLDS has no chart blueprint (RESEARCH §5.0): colours come ONLY from --chart-1..8 / --chart-grid /
// --chart-axis / --chart-ref, which globals.css maps onto the SLDS palette hooks
// (--slds-g-color-palette-*). Never Recharts' default colours. Animation OFF on every series
// (isAnimationActive false) — calm dashboard, reduced-motion satisfied by construction.
// SVG fill/stroke take var(), so a tenant brand/palette change repaints without a re-render.

import type { TooltipContentProps, TooltipValueType } from "recharts";
import { t, tf } from "@/lib/i18n";

/** Bảng màu categorical — thứ tự series 1→8 (xem ghi chú hue ở globals.css). */
export const CHART_COLORS = [
  "var(--chart-1)",
  "var(--chart-2)",
  "var(--chart-3)",
  "var(--chart-4)",
  "var(--chart-5)",
  "var(--chart-6)",
  "var(--chart-7)",
  "var(--chart-8)",
] as const;

export const CHART_GRID = "var(--chart-grid)";
export const CHART_AXIS = "var(--chart-axis)";
export const CHART_REF = "var(--chart-ref)";

/** Điểm dữ liệu chuẩn của mọi Nx chart — khớp ChartSeriesPoint (lib/analytics). */
export interface NxDatum {
  key: string;
  value: number;
  count?: number;
}

/** Tick label trục — 11px, màu token axis (đạt ≥4.5:1 làm text cả 2 mode). */
export const AXIS_TICK = { fill: CHART_AXIS, fontSize: 11 } as const;

/** Số trên trục: compact vi-VN (1200 → "1,2 N"). Tooltip dùng formatValue của caller. */
const compactVN = new Intl.NumberFormat("vi-VN", { notation: "compact", maximumFractionDigits: 1 });
export function formatAxisNumber(n: number): string {
  return compactVN.format(n);
}

/** Cắt nhãn dài (chuỗi VN dài hơn ~10–20%) — tooltip vẫn hiện đủ. */
export function truncateLabel(s: string, max = 12): string {
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

/** Shared tooltip — the SLDS tooltip bubble (blueprint `tooltips`: div.slds-popover.slds-popover_tooltip
    > div.slds-popover__body), placed by Recharts instead of a nubbin. Generic = Recharts' own union
    (ValueType/NameType) so `content={(p) => …{...p}}` type-checks without a cast. */
export function NxChartTooltip({
  active,
  payload,
  label,
  formatValue,
}: TooltipContentProps<TooltipValueType, number | string> & { formatValue?: (n: number) => string }) {
  if (!active || !payload || payload.length === 0) return null;
  const fmt = formatValue ?? ((n: number) => n.toLocaleString("vi-VN"));
  // Pie/Funnel have no axis `label` — take the point name from the payload.
  const heading = label ?? payload[0]?.payload?.key ?? payload[0]?.name;
  return (
    <div className="slds-popover slds-popover_tooltip" role="tooltip">
      <div className="slds-popover__body">
        {heading != null && heading !== "" && <p className="slds-truncate max-w-56">{String(heading)}</p>}
        {payload.map((p, i) => (
          <p key={i} className="flex items-center gap-1.5">
            <span
              aria-hidden="true"
              className="inline-block size-2 shrink-0"
              style={{ background: (p.payload && p.payload.fill) || p.color || CHART_COLORS[0] }}
            />
            <span className="tabular-nums">{fmt(Number(p.value ?? 0))}</span>
            {typeof p.payload?.count === "number" && (
              <span>{tf("page.charts.records", { n: p.payload.count.toLocaleString("vi-VN") })}</span>
            )}
          </p>
        ))}
      </div>
    </div>
  );
}

export interface NxLegendItem {
  key: string;
  color: string;
  /** Funnel stages fade along the ramp; pie slices are opaque. */
  opacity?: number;
  value: string;
  /** "42%" — share of the total (pie) or of the first stage (funnel). */
  share: string;
  drillable: boolean;
}

/** Legend beside a pie / funnel: swatch · label · value · share. A drillable row is a reset button
    inside an SLDS interactive list (`slds-has-list-interactions > .slds-item` hover); a row that
    cannot drill (the pie's grouped rest, or no onSelect) stays plain text — a disabled button would
    grey the numbers out. */
export function NxLegend({ items, onSelect }: { items: NxLegendItem[]; onSelect?: (key: string) => void }) {
  return (
    <ul className="slds-has-list-interactions slds-text-body_small min-w-0 flex-1">
      {items.map((d) => {
        const label = d.key || t("page.charts.blank");
        const row = (
          <>
            <span aria-hidden="true" className="size-2.5 shrink-0" style={{ background: d.color, opacity: d.opacity }} />
            <span className="slds-truncate min-w-0 flex-1">{label}</span>
            <span className="shrink-0 tabular-nums">{d.value}</span>
            <span className="slds-text-color_weak slds-text-align_right w-10 shrink-0 tabular-nums">{d.share}</span>
          </>
        );
        return (
          <li key={d.key} className={d.drillable && onSelect ? "slds-item" : undefined}>
            {d.drillable && onSelect ? (
              <button
                type="button"
                className="slds-button slds-button_reset flex w-full items-center gap-2 px-1 py-0.5"
                title={tf("page.charts.drill", { key: label })}
                onClick={() => onSelect(d.key)}
              >
                {row}
              </button>
            ) : (
              <div className="flex items-center gap-2 px-1 py-0.5" title={label}>
                {row}
              </div>
            )}
          </li>
        );
      })}
    </ul>
  );
}
