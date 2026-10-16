// lib/dashLayout.ts — dashboard grid layout math (default placement + reconcile).
//
// PERSISTENCE: the backend widget schema now accepts + round-trips the layout keys x/y/w/h and
// the "funnel" widget type (dashboard_service.go WidgetConfig + checkWidgetLayout). Grid layout is
// therefore persisted SERVER-SIDE on the shared, admin-owned dashboard definition (via PUT
// /admin/dashboards/:id) — see lib/dashWidgetPayload.ts for the read/write mappers. This module is
// now PURE layout math (no localStorage); the server is the single source of truth.
//
// Grid model: 12 columns, rowHeight 64px, margin 12px (see the viewer page GridLayout config).

export interface GridRect {
  /** Widget id (react-grid-layout `i`). */
  i: string;
  x: number;
  y: number;
  w: number;
  h: number;
  minW?: number;
  minH?: number;
}

export const GRID_COLS = 12;
export const GRID_ROW_HEIGHT = 64;
export const GRID_MARGIN = 12;

/** Widget kind → default size (w in 12-col units, h in 64px rows). */
export function defaultSpan(type: string, grouped: boolean): { w: number; h: number; minW: number; minH: number } {
  if (type === "kpi" && !grouped) return { w: 3, h: 2, minW: 2, minH: 2 }; // scalar KPI card
  if (type === "kpi" && grouped) return { w: 6, h: 5, minW: 4, minH: 4 };  // donut (pie thật)
  if (type === "table") return { w: 12, h: 5, minW: 6, minH: 3 };
  return { w: 6, h: 5, minW: 4, minH: 4 }; // bar / line charts
}

export interface WidgetShape {
  id: string;
  type: string;
  /** groupBy presence decides scalar vs grouped rendering (same rule as the BE). */
  grouped: boolean;
}

/**
 * Generate the default 12-col layout: widgets flow left→right, top→bottom in their
 * saved order; a widget that no longer fits the current row wraps to the next row.
 * Deterministic — same input, same layout (tested).
 */
export function defaultLayoutFor(widgets: WidgetShape[]): GridRect[] {
  const out: GridRect[] = [];
  let x = 0;
  let y = 0;
  let rowH = 0;
  for (const w of widgets) {
    const span = defaultSpan(w.type, w.grouped);
    if (x + span.w > GRID_COLS) {
      x = 0;
      y += rowH;
      rowH = 0;
    }
    out.push({ i: w.id, x, y, w: span.w, h: span.h, minW: span.minW, minH: span.minH });
    x += span.w;
    rowH = Math.max(rowH, span.h);
  }
  return out;
}

/**
 * Merge a stored layout with the CURRENT widget list: keep stored rects for widgets that
 * still exist (sanitized), append defaults below the grid for widgets added since the
 * layout was saved, and drop rects of deleted widgets.
 */
export function reconcileLayout(stored: GridRect[], widgets: WidgetShape[]): GridRect[] {
  const byId = new Map<string, GridRect>();
  for (const r of stored) {
    if (r && typeof r.i === "string") byId.set(r.i, r);
  }
  const out: GridRect[] = [];
  const missing: WidgetShape[] = [];
  let maxBottom = 0;
  for (const w of widgets) {
    const r = byId.get(w.id);
    const span = defaultSpan(w.type, w.grouped);
    if (r && [r.x, r.y, r.w, r.h].every((n) => Number.isFinite(n) && n >= 0)) {
      const rect: GridRect = {
        i: w.id,
        x: Math.min(Math.max(0, Math.round(r.x)), GRID_COLS - 1),
        y: Math.max(0, Math.round(r.y)),
        w: Math.min(Math.max(1, Math.round(r.w)), GRID_COLS),
        h: Math.max(1, Math.round(r.h)),
        minW: span.minW,
        minH: span.minH,
      };
      if (rect.x + rect.w > GRID_COLS) rect.x = GRID_COLS - rect.w;
      out.push(rect);
      maxBottom = Math.max(maxBottom, rect.y + rect.h);
    } else {
      missing.push(w);
    }
  }
  if (missing.length > 0) {
    for (const rect of defaultLayoutFor(missing)) {
      out.push({ ...rect, y: rect.y + maxBottom });
    }
  }
  return out;
}

// ── Per-widget viz kind — grouped bar ↔ funnel (persisted server-side as the widget `type`) ──
// A bar-family widget renders as columns ("bar") or a funnel ("funnel"); the admin toggle flips
// the widget's `type` on the shared dashboard definition (see the viewer page + dashWidgetPayload).
export type WidgetViz = "bar" | "funnel";
