// lib/dashWidgetPayload.ts — pure mappers for SERVER-side dashboard grid persistence.
//
// The dashboard grid layout (x/y/w/h per widget) + the funnel widget type live on the SHARED,
// admin-owned dashboard definition (sys_dashboards.widgets). PUT /admin/dashboards/:id is a
// FULL-REPLACE, so persisting a layout change means echoing back the COMPLETE widget array with
// only x/y/w/h (and, for the funnel toggle, `type`) overlaid — every other stored field (filters,
// metric, entity, groupBy, bucket, and any field we don't model) MUST round-trip untouched or it
// is lost on save.
//
// The backend guard `checkWidgetLayout` (dashboard_service.go) rejects a SINGLE-zero extent
// (w>0 & h=0, or w=0 & h>0) with 422 WIDGET_LAYOUT_INVALID. Extents must be BOTH ≥1 (a
// positioned/sized tile) OR BOTH 0 (the "layout unset" sentinel) — never one-sided. Every rect
// that leaves this module is coerced to satisfy that rule, so no partial extent can ever be sent.

import type { GridRect } from "./dashLayout";

// Inclusive upper bound for a widget's grid coordinate — mirrors the backend widgetCoordMax
// (dashboard_service.go). x/y/w/h all validate within [0, WIDGET_COORD_MAX]; out-of-bounds → 422.
export const WIDGET_COORD_MAX = 48;

// A widget's grid rectangle as persisted on the server (react-grid-layout coords).
export interface WidgetLayout {
  x: number;
  y: number;
  w: number;
  h: number;
}

// Integer clamp into [lo, hi]; a non-finite input collapses to lo (defensive — never NaN out).
function clampInt(n: unknown, lo: number, hi: number): number {
  const v = Math.round(Number(n));
  if (!Number.isFinite(v)) return lo;
  return Math.min(Math.max(v, lo), hi);
}

/**
 * Coerce an arbitrary rect into a SERVER-SAFE widget layout, enforcing the backend's
 * both-or-neither extent rule. A rect that is absent, non-finite, or has a sub-1 extent on
 * EITHER axis collapses to the {0,0,0,0} "layout unset" sentinel rather than emitting a
 * one-sided extent (which the backend rejects with 422 WIDGET_LAYOUT_INVALID). A fully sized
 * rect (w≥1 AND h≥1) is clamped to [0, WIDGET_COORD_MAX].
 */
export function normalizeWidgetLayout(rect?: Partial<WidgetLayout> | null): WidgetLayout {
  if (!rect) return { x: 0, y: 0, w: 0, h: 0 };
  const w = Math.round(Number(rect.w));
  const h = Math.round(Number(rect.h));
  // Both-or-neither: if either extent is missing / non-finite / <1, emit the unset sentinel.
  if (!Number.isFinite(w) || !Number.isFinite(h) || w < 1 || h < 1) {
    return { x: 0, y: 0, w: 0, h: 0 };
  }
  return {
    x: clampInt(rect.x, 0, WIDGET_COORD_MAX),
    y: clampInt(rect.y, 0, WIDGET_COORD_MAX),
    w: clampInt(w, 1, WIDGET_COORD_MAX),
    h: clampInt(h, 1, WIDGET_COORD_MAX),
  };
}

// A stored widget as returned by GET/run: we only KNOW id + type, but must PRESERVE every other
// key verbatim (a full-replace PUT drops anything we omit).
export interface StoredWidgetBase {
  id: string;
  type?: string;
}

/**
 * Build the full widget array for a full-replace PUT by overlaying the current grid layout — and
 * an optional per-widget `type` override for the funnel toggle — onto the STORED widget objects.
 * Every field this function does not touch (filters, metric, entity, …) round-trips unchanged.
 * x/y/w/h are normalized so no widget can carry a one-sided extent.
 *
 * The type override is applied ONLY for ids present in `typeById`. Callers MUST restrict that map
 * to bar-family widgets (bar ↔ funnel) so a kpi/line/table widget's type is never clobbered.
 */
export function buildWidgetLayoutPayload<T extends StoredWidgetBase>(
  widgets: readonly T[],
  rectById: Readonly<Record<string, Partial<WidgetLayout> | undefined>>,
  typeById: Readonly<Record<string, string>> = {},
): Array<T & WidgetLayout> {
  return widgets.map((wdg) => {
    const rect = normalizeWidgetLayout(rectById[wdg.id]);
    const overrideType = typeById[wdg.id];
    return {
      ...wdg,
      ...(overrideType ? { type: overrideType } : {}),
      x: rect.x,
      y: rect.y,
      w: rect.w,
      h: rect.h,
    };
  });
}

/**
 * Read the persisted grid rectangles from a server widget array. Only widgets that carry a FULLY
 * positioned layout (w≥1 AND h≥1) are returned; a widget with the unset sentinel (0,0,0,0) — or
 * any partial/degenerate extent — is OMITTED so reconcileLayout() assigns it a default position
 * instead of a collapsed 1×1 tile. This is the legacy-migration path: dashboards saved before the
 * widget layout schema carry all-zero coords and fall back to the default 12-col layout.
 */
export function serverWidgetsToRects(
  widgets: ReadonlyArray<{ id: string; x?: number; y?: number; w?: number; h?: number }>,
): GridRect[] {
  const out: GridRect[] = [];
  for (const wdg of widgets) {
    const w = Math.round(Number(wdg.w));
    const h = Math.round(Number(wdg.h));
    if (!Number.isFinite(w) || !Number.isFinite(h) || w < 1 || h < 1) continue; // unset/degenerate → default
    out.push({
      i: wdg.id,
      x: clampInt(wdg.x, 0, WIDGET_COORD_MAX),
      y: clampInt(wdg.y, 0, WIDGET_COORD_MAX),
      w: clampInt(w, 1, WIDGET_COORD_MAX),
      h: clampInt(h, 1, WIDGET_COORD_MAX),
    });
  }
  return out;
}
