"use client";

// NxPie — "part of the whole" donut (W1a Nx Charts). Donut + total in the centre (reads faster than
// a solid pie); the legend beside it (NxLegend) drills like the slices do. Colours cycle
// --chart-1..8 (SLDS palette hooks); more than 8 groups fold the tail into "Còn lại".

import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip } from "recharts";
import { t } from "@/lib/i18n";
import { CHART_COLORS, NxChartTooltip, NxLegend, type NxDatum } from "./nx-shared";

const MAX_SLICES = 8;

export function NxPie({
  data,
  height = 190,
  formatValue,
  centerLabel = t("page.dashboards.viewer.total"),
  onSelect,
}: {
  data: NxDatum[];
  height?: number;
  formatValue?: (n: number) => string;
  /** Small text under the centre total. */
  centerLabel?: string;
  onSelect?: (key: string) => void;
}) {
  const fmt = formatValue ?? ((n: number) => n.toLocaleString("vi-VN"));
  const rest = t("page.charts.rest");

  // >8 groups → top-7 + one folded "Còn lại" slice.
  const slices: NxDatum[] =
    data.length <= MAX_SLICES
      ? data
      : [
          ...data.slice(0, MAX_SLICES - 1),
          {
            key: rest,
            value: data.slice(MAX_SLICES - 1).reduce((s, d) => s + d.value, 0),
            count: data.slice(MAX_SLICES - 1).reduce((s, d) => s + (d.count ?? 0), 0),
          },
        ];
  const total = slices.reduce((s, d) => s + d.value, 0);

  return (
    <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
      <div className="relative mx-auto w-full max-w-[220px] shrink-0 sm:mx-0" style={{ height }}>
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Tooltip content={(p) => <NxChartTooltip {...p} formatValue={formatValue} />} />
            <Pie
              data={slices}
              dataKey="value"
              nameKey="key"
              innerRadius="62%"
              outerRadius="92%"
              paddingAngle={slices.length > 1 ? 2 : 0}
              strokeWidth={0}
              isAnimationActive={false}
              className={onSelect ? "cursor-pointer" : undefined}
              onClick={(entry) => {
                const key = (entry as unknown as { payload?: NxDatum }).payload?.key;
                if (key != null && key !== rest) onSelect?.(key);
              }}
            >
              {slices.map((s, i) => (
                <Cell key={s.key} fill={CHART_COLORS[i % CHART_COLORS.length]} />
              ))}
            </Pie>
          </PieChart>
        </ResponsiveContainer>
        {/* Centre total — pointer-events-none so it never covers a slice's tooltip. */}
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
          <span className="slds-text-heading_medium tabular-nums">{fmt(total)}</span>
          <span className="slds-text-body_small slds-text-color_weak">{centerLabel}</span>
        </div>
      </div>

      <NxLegend
        onSelect={onSelect}
        items={slices.map((s, i) => {
          const pct = total > 0 ? (s.value / total) * 100 : 0;
          return {
            key: s.key,
            color: CHART_COLORS[i % CHART_COLORS.length],
            value: fmt(s.value),
            share: `${pct.toFixed(pct >= 10 ? 0 : 1)}%`,
            drillable: s.key !== rest,
          };
        })}
      />
    </div>
  );
}
