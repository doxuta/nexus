// Unit tests for lib/dashLayout.ts — default 12-col layout generation + reconcile.

import { describe, it, expect } from "vitest";
import {
  defaultSpan,
  defaultLayoutFor,
  reconcileLayout,
  GRID_COLS,
  type GridRect,
  type WidgetShape,
} from "./dashLayout";

const w = (id: string, type: string, grouped = false): WidgetShape => ({ id, type, grouped });

describe("defaultSpan", () => {
  it("sizes by widget kind", () => {
    expect(defaultSpan("kpi", false)).toMatchObject({ w: 3, h: 2 });  // scalar KPI
    expect(defaultSpan("kpi", true)).toMatchObject({ w: 6, h: 5 });   // donut
    expect(defaultSpan("bar", true)).toMatchObject({ w: 6, h: 5 });
    expect(defaultSpan("line", true)).toMatchObject({ w: 6, h: 5 });
    expect(defaultSpan("table", true)).toMatchObject({ w: 12, h: 5 });
  });
});

describe("defaultLayoutFor", () => {
  it("flows left→right and wraps rows at 12 cols", () => {
    // CRM Overview shape: bar(6) + kpi(3) + bar(6) + bar(6)
    const rects = defaultLayoutFor([w("w1", "bar", true), w("w2", "kpi"), w("w3", "bar", true), w("w4", "bar", true)]);
    expect(rects[0]).toMatchObject({ i: "w1", x: 0, y: 0, w: 6 });
    expect(rects[1]).toMatchObject({ i: "w2", x: 6, y: 0, w: 3 });
    // w3 (6 wide) does not fit at x=9 → wraps below the tallest item of row 1 (h=5)
    expect(rects[2]).toMatchObject({ i: "w3", x: 0, y: 5, w: 6 });
    expect(rects[3]).toMatchObject({ i: "w4", x: 6, y: 5, w: 6 });
  });
  it("four scalar KPIs share one row", () => {
    const rects = defaultLayoutFor([w("a", "kpi"), w("b", "kpi"), w("c", "kpi"), w("d", "kpi")]);
    expect(rects.map((r) => r.y)).toEqual([0, 0, 0, 0]);
    expect(rects.map((r) => r.x)).toEqual([0, 3, 6, 9]);
  });
  it("is deterministic", () => {
    const input = [w("a", "kpi"), w("b", "table", true)];
    expect(defaultLayoutFor(input)).toEqual(defaultLayoutFor(input));
  });
});

describe("reconcileLayout", () => {
  const widgets = [w("w1", "bar", true), w("w2", "kpi")];

  it("keeps stored rects for surviving widgets", () => {
    const stored: GridRect[] = [
      { i: "w1", x: 6, y: 0, w: 6, h: 4 },
      { i: "w2", x: 0, y: 0, w: 4, h: 2 },
    ];
    const out = reconcileLayout(stored, widgets);
    expect(out.find((r) => r.i === "w1")).toMatchObject({ x: 6, y: 0, w: 6, h: 4 });
    expect(out.find((r) => r.i === "w2")).toMatchObject({ x: 0, y: 0, w: 4, h: 2 });
  });

  it("drops rects of deleted widgets", () => {
    const stored: GridRect[] = [{ i: "ghost", x: 0, y: 0, w: 6, h: 4 }];
    const out = reconcileLayout(stored, widgets);
    expect(out.some((r) => r.i === "ghost")).toBe(false);
    expect(out).toHaveLength(2);
  });

  it("appends NEW widgets below the stored grid with their default span", () => {
    const stored: GridRect[] = [{ i: "w1", x: 0, y: 0, w: 12, h: 6 }];
    const out = reconcileLayout(stored, widgets);
    const w2 = out.find((r) => r.i === "w2")!;
    expect(w2.y).toBeGreaterThanOrEqual(6); // below w1's bottom
    expect(w2).toMatchObject({ w: 3, h: 2 }); // scalar KPI default
  });

  it("sanitizes garbage numbers back into the 12-col bounds", () => {
    const stored: GridRect[] = [
      { i: "w1", x: 11, y: 2, w: 6, h: 4 },            // x+w overflows → pulled back
      { i: "w2", x: -3, y: -1, w: 99, h: 0 } as GridRect, // clamped
    ];
    const out = reconcileLayout(stored, widgets);
    const r1 = out.find((r) => r.i === "w1")!;
    expect(r1.x + r1.w).toBeLessThanOrEqual(GRID_COLS);
    const r2 = out.find((r) => r.i === "w2")!;
    expect(r2.x).toBeGreaterThanOrEqual(0);
    expect(r2.w).toBeLessThanOrEqual(GRID_COLS);
    expect(r2.h).toBeGreaterThanOrEqual(1);
  });

  it("non-finite stored values fall back to the default span", () => {
    const stored = [{ i: "w2", x: Number.NaN, y: 0, w: 3, h: 2 }] as GridRect[];
    const out = reconcileLayout(stored, widgets);
    expect(out.find((r) => r.i === "w2")).toMatchObject({ w: 3, h: 2 });
  });
});
