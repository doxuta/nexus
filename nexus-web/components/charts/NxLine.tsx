"use client";

// NxLine — biểu đồ đường (W1a Nx Charts). Thay polyline SVG tự vẽ của viewer cũ:
// có grid, trục, tooltip thật. Drill-through: click điểm → onSelect(key).

import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  AXIS_TICK,
  CHART_COLORS,
  CHART_GRID,
  CHART_REF,
  NxChartTooltip,
  formatAxisNumber,
  truncateLabel,
  type NxDatum,
} from "./nx-shared";

export function NxLine({
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
      <LineChart data={data} margin={{ top: 6, right: 8, bottom: 0, left: 0 }}>
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
          cursor={{ stroke: CHART_REF, strokeDasharray: "3 3" }}
          content={(p) => <NxChartTooltip {...p} formatValue={formatValue} />}
        />
        <Line
          type="monotone"
          dataKey="value"
          stroke={color}
          strokeWidth={2}
          isAnimationActive={false}
          dot={{ r: 3, fill: color, stroke: "var(--slds-g-color-neutral-base-100)", strokeWidth: 1 }}
          activeDot={{
            r: 4.5,
            className: onSelect ? "cursor-pointer" : undefined,
            onClick: (_e, payload) => {
              const key = (payload as unknown as { payload?: NxDatum }).payload?.key;
              if (key != null) onSelect?.(key);
            },
          }}
        />
      </LineChart>
    </ResponsiveContainer>
  );
}
