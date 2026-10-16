"use client";

// NxFunnel — pipeline funnel (W1a Nx Charts). One hue (--chart-1, an SLDS palette hook) with an
// opacity ramp per stage — the Salesforce funnel pattern, no rainbow. Stage label + value + share
// of the first stage sit in the legend on the right (NxLegend), never on top of the fill.

import { Funnel, FunnelChart, ResponsiveContainer, Tooltip } from "recharts";
import { CHART_COLORS, NxChartTooltip, NxLegend, type NxDatum } from "./nx-shared";

// Opacity ramp — first stage solid, fading to a 0.35 floor so the last fill stays ≥3:1 on a wide area.
function stageOpacity(i: number, n: number): number {
  if (n <= 1) return 1;
  return 1 - (i / (n - 1)) * 0.65;
}

export function NxFunnel({
  data,
  height = 200,
  formatValue,
  onSelect,
}: {
  data: NxDatum[];
  height?: number;
  formatValue?: (n: number) => string;
  onSelect?: (key: string) => void;
}) {
  const fmt = formatValue ?? ((n: number) => n.toLocaleString("vi-VN"));
  const first = data[0]?.value ?? 0;

  return (
    <div className="flex items-center gap-3">
      <div className="min-w-0 flex-1" style={{ height }}>
        <ResponsiveContainer width="100%" height="100%">
          <FunnelChart margin={{ top: 4, right: 4, bottom: 4, left: 4 }}>
            <Tooltip content={(p) => <NxChartTooltip {...p} formatValue={formatValue} />} />
            <Funnel
              data={data.map((d, i) => ({
                ...d,
                fill: CHART_COLORS[0],
                fillOpacity: stageOpacity(i, data.length),
              }))}
              dataKey="value"
              nameKey="key"
              isAnimationActive={false}
              className={onSelect ? "cursor-pointer" : undefined}
              onClick={(entry) => {
                const key = (entry as unknown as { payload?: NxDatum }).payload?.key;
                if (key != null) onSelect?.(key);
              }}
            />
          </FunnelChart>
        </ResponsiveContainer>
      </div>
      <div className="w-[42%] min-w-[150px] shrink-0">
        <NxLegend
          onSelect={onSelect}
          items={data.map((d, i) => ({
            key: d.key,
            color: CHART_COLORS[0],
            opacity: stageOpacity(i, data.length),
            value: fmt(d.value),
            share: `${(first > 0 ? (d.value / first) * 100 : 0).toFixed(0)}%`,
            drillable: true,
          }))}
        />
      </div>
    </div>
  );
}
