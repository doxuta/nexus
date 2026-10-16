"use client";

// NxKPI — metric tile of a dashboard: value + optional delta vs the previous period + sparkline
// (W1a Nx Charts), drawn as an SLDS card (blueprint `cards`: the widget title is the card header, the
// figure is `slds-text-heading_large`, the delta a themed badge). HONEST: delta/spark render ONLY
// when the caller has real data — never a made-up trend. Sparkline = a hand SVG over 12 points
// (no Recharts in a small number tile), stroked with --chart-1 (SLDS palette hook).

import { TrendingDown, TrendingUp } from "lucide-react";
import type { ReactNode } from "react";
import { Badge } from "@/components/slds/Badge";
import { Card } from "@/components/slds/Card";

function Sparkline({ points, width = 96, height = 28 }: { points: number[]; width?: number; height?: number }) {
  if (points.length < 2) return null;
  const min = Math.min(...points);
  const max = Math.max(...points);
  const span = max - min || 1;
  const pad = 2;
  const coords = points.map((v, i) => {
    const x = pad + (i / (points.length - 1)) * (width - pad * 2);
    const y = height - pad - ((v - min) / span) * (height - pad * 2);
    return [x, y] as const;
  });
  const last = coords[coords.length - 1];
  return (
    <svg width={width} height={height} aria-hidden className="shrink-0">
      <polyline
        fill="none"
        stroke="var(--chart-1)"
        strokeWidth={1.5}
        strokeLinecap="round"
        strokeLinejoin="round"
        points={coords.map(([x, y]) => `${x},${y}`).join(" ")}
      />
      <circle cx={last[0]} cy={last[1]} r={2.5} fill="var(--chart-1)" />
    </svg>
  );
}

export function NxKPI({
  label,
  value,
  sub,
  delta,
  spark,
  className,
}: {
  label: string;
  value: string;
  sub?: ReactNode;
  /** % change vs the previous period — positive = up (success), negative = down (error). Real data only. */
  delta?: number;
  /** History for the sparkline — real data only. */
  spark?: number[];
  className?: string;
}) {
  const up = (delta ?? 0) >= 0;
  return (
    <Card title={label} className={className}>
      <div className="flex items-end justify-between gap-3">
        <p className="slds-text-heading_large slds-truncate min-w-0 tabular-nums" title={value}>
          {value}
        </p>
        {spark && spark.length >= 2 && <Sparkline points={spark} />}
      </div>
      <div className="mt-1 flex items-center gap-2">
        {typeof delta === "number" && (
          <Badge theme={up ? "success" : "error"} iconLeft={up ? TrendingUp : TrendingDown} className="tabular-nums">
            {up ? "+" : ""}
            {delta.toLocaleString("vi-VN", { maximumFractionDigits: 1 })}%
          </Badge>
        )}
        {sub && <span className="slds-text-body_small slds-text-color_weak slds-truncate min-w-0">{sub}</span>}
      </div>
    </Card>
  );
}
