// lib/dataGrid.ts — pure helpers for the DataGrid primitive (W9a, plan §4.2 "DataGrid" row).
// Kept out of the component so the width-math + persistence contract is unit-testable
// without a DOM. The component (components/ui/DataGrid.tsx) is a thin consumer.

import { currentUserKey } from "@/lib/pins";
import { readForUser } from "@/lib/scopedStorage";

export interface GridColumnSpec {
  id: string;
  /** Declared layout width in px (list layout `columns[].width`). Fallback 140. */
  width?: number;
}

// Build the CSS grid-template-columns string for header + rows.
// Behavior preserved VERBATIM from the hand-rolled LayoutList table:
//   • optional leading fixed slot (selection checkbox — 40px in LayoutList)
//   • FIRST data column is fluid `minmax(0,1.6fr)` UNLESS the user resized it
//   • other columns default to their declared width: `minmax(0,<width|140>px)`
//   • any user-resized column becomes a hard px width (that's the only visible change)
//   • optional TRAILING fixed slot (per-row action — recycle-bin restore). Absent (undefined) →
//     no trailing column, so every existing caller (LayoutList) is byte-identical.
export function buildGridTemplate(
  cols: GridColumnSpec[],
  sizing: Record<string, number>,
  leadPx?: number,
  trailPx?: number,
): string {
  const parts: string[] = [];
  if (leadPx && leadPx > 0) parts.push(`${leadPx}px`);
  cols.forEach((c, i) => {
    const resized = sizing[c.id];
    if (typeof resized === "number" && Number.isFinite(resized) && resized > 0) {
      parts.push(`${Math.round(resized)}px`);
    } else if (i === 0) {
      parts.push("minmax(0,1.6fr)");
    } else {
      parts.push(`minmax(0,${c.width ?? 140}px)`);
    }
  });
  if (trailPx && trailPx > 0) parts.push(`${trailPx}px`);
  return parts.join(" ");
}

// Resolve a CSS row-height ("var(--row-compact)" | "48px") to a px NUMBER for the
// virtualizer's estimateSize. The density token stays the single source of truth —
// we READ the computed var at runtime instead of restating 36/48/60 here.
// Fallback 48 (cozy) when the var can't be read (SSR, tests, missing token).
export function resolveRowHeightPx(
  cssValue: string,
  readVar: (name: string) => string,
): number {
  const m = cssValue.trim().match(/^var\((--[\w-]+)\)$/);
  const raw = m ? readVar(m[1]) : cssValue;
  const n = Number.parseFloat(raw);
  return Number.isFinite(n) && n > 0 ? n : 48;
}

// ── Column-width persistence — per-user + per-grid, localStorage only ──────────
// Same per-user namespacing discipline as lib/pins.ts (no backend prefs yet, P2).
// Storage shape: { [columnId]: widthPx } — ONLY user-resized columns are stored, so
// un-touched columns keep following the layout's declared widths (metadata stays boss).

const KEY_PREFIX = "nexus.gridColW.v1";

export function gridWidthStorageKey(gridKey: string, userKey: string): string {
  return `${KEY_PREFIX}:${userKey}:${gridKey}`;
}

export function loadColWidths(
  gridKey: string,
  userKey?: string,
): Record<string, number> {
  if (typeof window === "undefined") return {};
  try {
    // No explicit key = the current scope: only then may a pre-L2b value (bare user id) be adopted.
    const raw = userKey === undefined
      ? readForUser((k) => gridWidthStorageKey(gridKey, k))
      : window.localStorage.getItem(gridWidthStorageKey(gridKey, userKey));
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    const out: Record<string, number> = {};
    for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
      if (typeof v === "number" && Number.isFinite(v) && v > 0) out[k] = v;
    }
    return out;
  } catch {
    return {};
  }
}

export function saveColWidths(
  gridKey: string,
  widths: Record<string, number>,
  userKey: string = currentUserKey(),
): void {
  if (typeof window === "undefined") return;
  try {
    const key = gridWidthStorageKey(gridKey, userKey);
    if (Object.keys(widths).length === 0) window.localStorage.removeItem(key);
    else window.localStorage.setItem(key, JSON.stringify(widths));
  } catch {
    // quota / private mode — persistence is a convenience, never critical
  }
}
