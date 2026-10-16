// Unit tests for lib/dashWidgetPayload.ts — the layout→widget-payload mapper that enforces the
// backend both-or-neither extent rule and guarantees a full-replace PUT never drops a stored field.

import { describe, it, expect } from "vitest";
import {
  WIDGET_COORD_MAX,
  normalizeWidgetLayout,
  buildWidgetLayoutPayload,
  serverWidgetsToRects,
} from "./dashWidgetPayload";

describe("normalizeWidgetLayout", () => {
  it("keeps a fully positioned rect", () => {
    expect(normalizeWidgetLayout({ x: 6, y: 5, w: 6, h: 4 })).toEqual({ x: 6, y: 5, w: 6, h: 4 });
  });

  it("coerces a single-zero extent (w>0,h=0) to the unset sentinel — never one-sided", () => {
    expect(normalizeWidgetLayout({ x: 2, y: 3, w: 6, h: 0 })).toEqual({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("coerces a single-zero extent (w=0,h>0) to the unset sentinel — never one-sided", () => {
    expect(normalizeWidgetLayout({ x: 2, y: 3, w: 0, h: 5 })).toEqual({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("passes the both-zero unset sentinel through unchanged", () => {
    expect(normalizeWidgetLayout({ x: 0, y: 0, w: 0, h: 0 })).toEqual({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("clamps out-of-bounds coords into [0, WIDGET_COORD_MAX]", () => {
    expect(normalizeWidgetLayout({ x: -3, y: -1, w: 999, h: 999 })).toEqual({
      x: 0,
      y: 0,
      w: WIDGET_COORD_MAX,
      h: WIDGET_COORD_MAX,
    });
  });

  it("treats a non-finite EXTENT (w or h) as the unset sentinel", () => {
    expect(normalizeWidgetLayout({ x: 2, y: 3, w: Number.NaN, h: 4 })).toEqual({ x: 0, y: 0, w: 0, h: 0 });
    expect(normalizeWidgetLayout({ x: 2, y: 3, w: 6, h: Number.NaN })).toEqual({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("clamps a non-finite OFFSET to 0 but keeps a valid extent positioned", () => {
    // A bad x/y must NOT collapse a sized widget — only the offset resets to 0.
    expect(normalizeWidgetLayout({ x: Number.NaN, y: 0, w: 6, h: 4 })).toEqual({ x: 0, y: 0, w: 6, h: 4 });
  });

  it("treats a missing rect as the unset sentinel", () => {
    expect(normalizeWidgetLayout(undefined)).toEqual({ x: 0, y: 0, w: 0, h: 0 });
    expect(normalizeWidgetLayout(null)).toEqual({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("rounds fractional coords to integers", () => {
    expect(normalizeWidgetLayout({ x: 1.6, y: 2.4, w: 6.5, h: 4.2 })).toEqual({ x: 2, y: 2, w: 7, h: 4 });
  });
});

describe("buildWidgetLayoutPayload", () => {
  // A stored widget carrying fields the FE type does NOT model (filters + a future key) — the
  // full-replace PUT must round-trip them untouched.
  const stored = [
    {
      id: "w1",
      title: "Doanh thu",
      type: "bar",
      entity: "opportunities",
      metric: "sum",
      field: "amount",
      groupBy: "stage",
      filters: [{ field: "stage", op: "eq", values: ["won"] }],
      futureKey: "keep-me",
    },
    { id: "w2", title: "Số KH", type: "kpi", entity: "accounts", metric: "count" },
  ];

  it("preserves every stored field and only overlays x/y/w/h", () => {
    const out = buildWidgetLayoutPayload(stored, {
      w1: { x: 0, y: 0, w: 6, h: 5 },
      w2: { x: 6, y: 0, w: 3, h: 2 },
    });
    expect(out[0]).toEqual({
      id: "w1",
      title: "Doanh thu",
      type: "bar",
      entity: "opportunities",
      metric: "sum",
      field: "amount",
      groupBy: "stage",
      filters: [{ field: "stage", op: "eq", values: ["won"] }],
      futureKey: "keep-me",
      x: 0,
      y: 0,
      w: 6,
      h: 5,
    });
    expect(out[1]).toMatchObject({ id: "w2", type: "kpi", x: 6, y: 0, w: 3, h: 2 });
  });

  it("emits the unset sentinel (never a partial extent) for a widget with no rect", () => {
    const out = buildWidgetLayoutPayload(stored, {}); // no rects at all
    expect(out.every((wdg) => (wdg.w === 0) === (wdg.h === 0))).toBe(true);
    expect(out[0]).toMatchObject({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("never emits a one-sided extent even when handed a degenerate rect", () => {
    const out = buildWidgetLayoutPayload(stored, { w1: { x: 1, y: 1, w: 6, h: 0 } });
    expect(out[0]).toMatchObject({ x: 0, y: 0, w: 0, h: 0 });
  });

  it("applies the type override ONLY to ids present in typeById (bar-family funnel toggle)", () => {
    const out = buildWidgetLayoutPayload(
      stored,
      { w1: { x: 0, y: 0, w: 6, h: 5 }, w2: { x: 6, y: 0, w: 3, h: 2 } },
      { w1: "funnel" }, // only w1 toggled; w2 (kpi) must keep its server type
    );
    expect(out[0].type).toBe("funnel");
    expect(out[1].type).toBe("kpi");
  });

  it("leaves type untouched when typeById is empty", () => {
    const out = buildWidgetLayoutPayload(stored, { w1: { x: 0, y: 0, w: 6, h: 5 } });
    expect(out[0].type).toBe("bar");
  });
});

describe("serverWidgetsToRects", () => {
  it("returns rects only for fully positioned widgets", () => {
    const rects = serverWidgetsToRects([
      { id: "w1", x: 6, y: 5, w: 6, h: 4 },
      { id: "w2", x: 0, y: 0, w: 3, h: 2 },
    ]);
    expect(rects).toEqual([
      { i: "w1", x: 6, y: 5, w: 6, h: 4 },
      { i: "w2", x: 0, y: 0, w: 3, h: 2 },
    ]);
  });

  it("OMITS the unset sentinel (legacy dashboards saved before the layout schema)", () => {
    // All-zero widgets must be dropped so reconcileLayout assigns defaults — NOT kept as 1×1 tiles.
    const rects = serverWidgetsToRects([
      { id: "old1", x: 0, y: 0, w: 0, h: 0 },
      { id: "old2" }, // no coords at all
      { id: "new1", x: 0, y: 0, w: 6, h: 5 },
    ]);
    expect(rects.map((r) => r.i)).toEqual(["new1"]);
  });

  it("omits a degenerate (partial-extent) widget rather than collapsing it", () => {
    const rects = serverWidgetsToRects([{ id: "bad", x: 2, y: 2, w: 6, h: 0 }]);
    expect(rects).toEqual([]);
  });

  it("clamps stored coords into bounds", () => {
    const rects = serverWidgetsToRects([{ id: "w1", x: -5, y: 2, w: 999, h: 4 }]);
    expect(rects[0]).toMatchObject({ i: "w1", x: 0, w: WIDGET_COORD_MAX });
  });
});
