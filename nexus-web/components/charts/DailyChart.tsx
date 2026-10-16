"use client";

// DailyChart — one or two daily series, drawn flat (specs/078 FR-101, lát u7-muc-dung-bieu-do).
// Added next to NxLine/NxBar, which belong to the customer's dashboard and stay byte-for-byte as they were.
//   • one y axis (no dual axis), a 2px line, the faintest grid; the only value printed on the plot is the LAST one
//     ("{n} hôm nay"), the x axis names only the first day and "Hôm nay" (dataviz: label the end, let the table carry the rest)
//   • the plot is a focusable group: ←/→/Home/End move a crosshair and a tooltip names the day and every value;
//     "Xem dạng bảng" lists exactly the same points
//   • a failing series is told by an icon and a word as well as by colour
// ponytail: plain SVG polylines (viewBox 0..100, non-scaling stroke), all text in HTML so it stays crisp at 375px.
// Recharts would need a measured width and gave us no keyboard crosshair; ceiling ~366 points, no zoom/brush.
import clsx from "clsx";
import { AlertTriangle } from "lucide-react";
import { useId, useState, type KeyboardEvent, type PointerEvent } from "react";

import { t, tf } from "@/lib/i18n";
import { AXIS_TICK, CHART_COLORS, CHART_GRID, CHART_REF, formatAxisNumber } from "./nx-shared";

export interface DailyChartSeries {
  key: string;
  label: string;
  /** "error" = a failure count: coloured with the red of the palette AND marked with an icon and its word. */
  tone: "normal" | "error";
}
export interface DailyChartPoint {
  /** YYYY-MM-DD; a day with nothing to say is a point of zeros, never a hole. */
  day: string;
  values: Record<string, number>;
}

const COLOR = { normal: CHART_COLORS[0], error: CHART_COLORS[7] } as const;
const nf = (n: number) => n.toLocaleString("vi-VN");
const PAD = 6; // % kept free above and below the plot so a 2px line is never cut

export function DailyChart({
  series,
  points,
  title,
  formatDay = (d) => d,
}: {
  series: DailyChartSeries[];
  points: DailyChartPoint[];
  /** Names the chart for screen readers and the table. */
  title?: string;
  formatDay?: (day: string) => string;
}) {
  const [active, setActive] = useState<number | null>(null);
  const [table, setTable] = useState(false);
  const tableId = useId();
  const last = points.length - 1;
  const val = (p: DailyChartPoint, k: string) => p.values[k] ?? 0;
  const max = Math.max(1, ...points.flatMap((p) => series.map((s) => val(p, s.key))));
  const x = (i: number) => (last <= 0 ? 50 : (i / last) * 100);
  const y = (v: number) => 100 - PAD - (v / max) * (100 - 2 * PAD);
  const primary = series[0];

  const onKey = (e: KeyboardEvent) => {
    if (last < 0) return;
    const move = (to: number) => {
      e.preventDefault();
      setActive(Math.max(0, Math.min(last, to)));
    };
    if (e.key === "ArrowLeft") move((active ?? last) - 1);
    else if (e.key === "ArrowRight") move((active ?? last) + 1);
    else if (e.key === "Home") move(0);
    else if (e.key === "End") move(last);
    else if (e.key === "Escape") setActive(null);
  };
  const onPointer = (e: PointerEvent<HTMLDivElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    if (r.width <= 0 || last < 0) return;
    setActive(Math.round(Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)) * last));
  };

  return (
    <div className="nx-daily-chart">
      <div className="flex">
        <div data-y-axis className="flex flex-col justify-between tabular-nums" style={{ ...AXIS_TICK, width: "2.25rem", paddingBottom: 0 }} aria-hidden="true">
          <span>{formatAxisNumber(max)}</span>
          <span>0</span>
        </div>
        <div className="relative min-w-0 flex-1" style={{ marginRight: "5.5rem" }}>
          <div
            role="group"
            tabIndex={0}
            aria-label={tf("studio.operate.usage.chartAria", { title: title ?? "" })}
            className="relative outline-offset-2"
            style={{ height: "9rem" }}
            onKeyDown={onKey}
            onBlur={() => setActive(null)}
            onPointerMove={onPointer}
            onPointerLeave={() => setActive(null)}
          >
            <svg viewBox="0 0 100 100" preserveAspectRatio="none" className="absolute inset-0 size-full" aria-hidden="true" focusable="false">
              {[PAD, 50, 100 - PAD].map((g) => (
                <line key={g} x1="0" x2="100" y1={g} y2={g} stroke={CHART_GRID} strokeWidth="1" vectorEffect="non-scaling-stroke" />
              ))}
              {series.map((s) => (
                <polyline
                  key={s.key}
                  fill="none"
                  stroke={COLOR[s.tone]}
                  strokeWidth="2"
                  strokeLinejoin="round"
                  strokeDasharray={s.tone === "error" ? "5 3" : undefined}
                  vectorEffect="non-scaling-stroke"
                  points={points.map((p, i) => `${x(i)},${y(val(p, s.key))}`).join(" ")}
                />
              ))}
              {active != null && <line x1={x(active)} x2={x(active)} y1="0" y2="100" stroke={CHART_REF} strokeWidth="1" vectorEffect="non-scaling-stroke" />}
            </svg>
            {active != null && points[active] && (
              <>
                {series.map((s) => (
                  <span
                    key={s.key}
                    aria-hidden="true"
                    className="absolute size-2 rounded-full"
                    style={{ left: `${x(active)}%`, top: `${y(val(points[active], s.key))}%`, background: COLOR[s.tone], transform: "translate(-50%,-50%)" }}
                  />
                ))}
                <div
                  role="tooltip"
                  className="slds-popover slds-popover_tooltip absolute"
                  style={{ left: `${x(active)}%`, top: 0, transform: `translateX(${active > last / 2 ? "-105%" : "5%"})`, width: "max-content" }}
                >
                  <div className="slds-popover__body">
                    <p>{formatDay(points[active].day)}</p>
                    {series.map((s) => (
                      <p key={s.key} className="tabular-nums">
                        {s.label}: {nf(val(points[active], s.key))}
                      </p>
                    ))}
                  </div>
                </div>
              </>
            )}
            {primary && last >= 0 && (
              <span data-end-label className="absolute whitespace-nowrap tabular-nums slds-text-title_bold" style={{ left: "100%", top: `${y(val(points[last], primary.key))}%`, transform: "translate(0.5rem,-50%)" }}>
                {tf("studio.operate.usage.endLabel", { n: nf(val(points[last], primary.key)) })}
              </span>
            )}
          </div>
          <div data-axis className="flex justify-between slds-m-top_xx-small" style={AXIS_TICK}>
            <span data-axis-label>{points[0] ? formatDay(points[0].day) : ""}</span>
            <span data-axis-label>{t("studio.operate.usage.today")}</span>
          </div>
        </div>
      </div>

      <div className="slds-grid slds-grid_vertical-align-center slds-wrap slds-m-top_x-small">
        {series.length >= 2 && (
          <ul aria-label={t("studio.operate.usage.legend")} className="flex flex-wrap gap-4 slds-text-body_small">
            {series.map((s) => (
              <li key={s.key} className="flex items-center gap-1.5">
                {s.tone === "error" ? (
                  <AlertTriangle data-tone-icon="error" aria-hidden="true" size={14} color={COLOR.error} />
                ) : (
                  <span aria-hidden="true" className="inline-block" style={{ width: "0.875rem", height: 2, background: COLOR.normal }} />
                )}
                <span>{s.label}</span>
              </li>
            ))}
          </ul>
        )}
        <button type="button" className="slds-button slds-col_bump-left" aria-expanded={table} aria-controls={tableId} onClick={() => setTable((v) => !v)}>
          {t(table ? "studio.operate.usage.hideTable" : "studio.operate.usage.showTable")}
        </button>
      </div>

      {table && (
        <div id={tableId} className={clsx("slds-m-top_x-small")} style={{ maxHeight: "20rem", overflow: "auto" }}>
          <table className="slds-table slds-table_bordered slds-table_cell-buffer">
            <thead>
              <tr className="slds-line-height_reset">
                <th scope="col" style={{ position: "sticky", top: 0, background: "var(--slds-g-color-neutral-base-100, #fff)" }}>
                  {t("studio.operate.usage.colDay")}
                </th>
                {series.map((s) => (
                  <th key={s.key} scope="col" className="slds-text-align_right" style={{ position: "sticky", top: 0, background: "var(--slds-g-color-neutral-base-100, #fff)" }}>
                    {s.label}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {points.map((p) => (
                <tr key={p.day}>
                  <td>{formatDay(p.day)}</td>
                  {series.map((s) => (
                    <td key={s.key} className="slds-text-align_right tabular-nums">
                      {nf(val(p, s.key))}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
