"use client";

// NxBar — categorical column chart (W1a Nx Charts). One series = ONE colour (--chart-1, an SLDS
// palette hook) — no rainbow bars. Square column tops like Lightning report charts. The hover band is
// the SLDS neutral-95 hook. Drill-through: click a column → onSelect(key).

import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  AXIS_TICK,
  CHART_COLORS,
  CHART_GRID,
  NxChartTooltip,
  formatAxisNumber,
  truncateLabel,
  type NxDatum,
} from "./nx-shared";

export function NxBar({
  data,
  height = 208,
  color = CHART_COLORS[0],
  formatValue,
  onSelect,
}: {
  data: NxDatum[];
  height?: number;
  color?: string;
  formatValue?: (n: number) => string;
  onSelect?: (key: string) => void;
}) {
  return (
    <ResponsiveContainer width="100%" height={height}>
      <BarChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 0 }}>
        <CartesianGrid stroke={CHART_GRID} vertical={false} />
        <XAxis
          dataKey="key"
          tick={AXIS_TICK}
          tickLine={false}
          axisLine={{ stroke: CHART_GRID }}
          tickFormatter={(v: string) => truncateLabel(v)}
          interval="preserveStartEnd"
        />
        <YAxis
          tick={AXIS_TICK}
          tickLine={false}
          axisLine={false}
          width={52}
          allowDecimals={false}
          tickFormatter={formatAxisNumber}
        />
        <Tooltip
          cursor={{ fill: "var(--slds-g-color-neutral-base-95)" }}
          content={(p) => <NxChartTooltip {...p} formatValue={formatValue} />}
        />
        <Bar
          dataKey="value"
          fill={color}
          maxBarSize={44}
          isAnimationActive={false}
          className={onSelect ? "cursor-pointer" : undefined}
          onClick={(entry) => {
            const key = (entry as unknown as { payload?: NxDatum }).payload?.key;
            if (key != null) onSelect?.(key);
          }}
        />
      </BarChart>
    </ResponsiveContainer>
  );
}
