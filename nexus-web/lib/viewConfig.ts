// lib/viewConfig.ts — pure model for W9c saved views (../specs/018-saved-views/spec.md + api-conventions §13.2).
//
// A "view" is an FE-defined UI preset (filters / sort / density / hidden columns) the user stores
// for a dynamic entity. The server treats `config` as an opaque JSON object (validate: object,
// ≤64 KiB) — the SHAPE below is entirely FE-owned. It is VERSIONED ({ v: 1, ... }) so a future
// migration can bump the version and translate old payloads; `parseViewConfig` returns a safe
// default for any version it doesn't understand (honest: we never apply a config we can't read).
//
// No React import — unit-testable in isolation (lib/viewConfig.test.ts).

import type { FilterRow } from "@/lib/filterModel";
import { nextFilterId } from "@/lib/filterModel";

export type ViewDensity = "compact" | "normal" | "relaxed";
export const VIEW_DENSITIES: readonly ViewDensity[] = ["compact", "normal", "relaxed"] as const;

export interface ViewSort {
  field: string;
  order: "asc" | "desc";
}

// The persisted config. `filters` stores only the serialisable triple — the UI `id` is a render key
// (module counter, see filterModel) and is rehydrated fresh on apply so ids never collide.
export interface ViewConfigV1 {
  v: 1;
  filters: { field: string; op: string; value: string }[];
  sort: ViewSort | null;
  density: ViewDensity;
  columns: { hidden: string[] };
}

// Live list state a view captures / restores. LayoutList owns these four pieces of state.
export interface ViewState {
  filters: FilterRow[];
  sort: ViewSort | null;
  density: ViewDensity;
  hiddenColumns: string[];
}

export const DEFAULT_VIEW_CONFIG: ViewConfigV1 = {
  v: 1,
  filters: [],
  sort: null,
  density: "normal",
  columns: { hidden: [] },
};

function isDensity(v: unknown): v is ViewDensity {
  return v === "compact" || v === "normal" || v === "relaxed";
}

// Serialize the current list state → a persistable config. Drops incomplete filter rows (no
// field/op) so a half-built filter never ships; keeps the UI `id` out (rehydrated on apply).
export function serializeViewConfig(state: ViewState): ViewConfigV1 {
  return {
    v: 1,
    filters: state.filters
      .filter((f) => f.field && f.op)
      .map((f) => ({ field: f.field, op: f.op, value: f.value ?? "" })),
    sort: state.sort ? { field: state.sort.field, order: state.sort.order } : null,
    density: isDensity(state.density) ? state.density : "normal",
    columns: { hidden: [...state.hiddenColumns] },
  };
}

// Parse an opaque server `config` object into a known ViewConfigV1. Defensive on EVERY field —
// a malformed/unknown-version payload falls back to DEFAULT_VIEW_CONFIG rather than throwing or
// applying garbage. This is the unknown-version fallback the tests assert.
export function parseViewConfig(raw: unknown): ViewConfigV1 {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return DEFAULT_VIEW_CONFIG;
  const o = raw as Record<string, unknown>;
  if (o.v !== 1) return DEFAULT_VIEW_CONFIG; // unknown / missing version → safe default

  const filters: ViewConfigV1["filters"] = [];
  if (Array.isArray(o.filters)) {
    for (const item of o.filters) {
      if (!item || typeof item !== "object") continue;
      const r = item as Record<string, unknown>;
      if (typeof r.field !== "string" || typeof r.op !== "string") continue;
      filters.push({ field: r.field, op: r.op, value: typeof r.value === "string" ? r.value : "" });
    }
  }

  let sort: ViewSort | null = null;
  if (o.sort && typeof o.sort === "object" && !Array.isArray(o.sort)) {
    const s = o.sort as Record<string, unknown>;
    if (typeof s.field === "string" && (s.order === "asc" || s.order === "desc")) {
      sort = { field: s.field, order: s.order };
    }
  }

  const density: ViewDensity = isDensity(o.density) ? o.density : "normal";

  let hidden: string[] = [];
  if (o.columns && typeof o.columns === "object" && !Array.isArray(o.columns)) {
    const c = o.columns as Record<string, unknown>;
    if (Array.isArray(c.hidden)) {
      hidden = c.hidden.filter((x): x is string => typeof x === "string");
    }
  }

  return { v: 1, filters, sort, density, columns: { hidden } };
}

// Turn a parsed config into live list state, rehydrating fresh UI ids for the filter rows so they
// never collide with rows already on screen.
export function applyViewConfig(config: ViewConfigV1): ViewState {
  return {
    filters: config.filters.map((f) => ({
      id: nextFilterId(),
      field: f.field,
      op: f.op,
      value: f.value,
    })),
    sort: config.sort ? { ...config.sort } : null,
    density: config.density,
    hiddenColumns: [...config.columns.hidden],
  };
}

// Structural equality — used for the "dirty" (modified since applied) dot. Filter rows are
// order-sensitive (the builder preserves row order); hidden columns compare as a SET.
export function viewConfigEqual(a: ViewConfigV1, b: ViewConfigV1): boolean {
  if (a.v !== b.v) return false;
  if (a.density !== b.density) return false;

  if ((a.sort === null) !== (b.sort === null)) return false;
  if (a.sort && b.sort && (a.sort.field !== b.sort.field || a.sort.order !== b.sort.order)) return false;

  if (a.filters.length !== b.filters.length) return false;
  for (let i = 0; i < a.filters.length; i++) {
    const x = a.filters[i];
    const y = b.filters[i];
    if (x.field !== y.field || x.op !== y.op || x.value !== y.value) return false;
  }

  const ah = [...a.columns.hidden].sort();
  const bh = [...b.columns.hidden].sort();
  if (ah.length !== bh.length) return false;
  for (let i = 0; i < ah.length; i++) {
    if (ah[i] !== bh[i]) return false;
  }
  return true;
}

// ── Ownership / visibility (mirrors backend ViewService, ../specs/018-saved-views/spec.md §4) ──────────────────
// SHARED view (userId null) → only admin may create/rename/delete. PERSONAL → only its owner.
export interface SavedViewLike {
  userId: string | null;
  shared: boolean;
}
export interface ViewViewer {
  userId: string | undefined;
  isAdmin: boolean;
}

// Can the viewer rename / update / delete this view?
export function canMutateView(view: SavedViewLike, viewer: ViewViewer): boolean {
  if (view.shared || view.userId === null) return viewer.isAdmin;
  return !!viewer.userId && view.userId === viewer.userId;
}

// Can the viewer create a SHARED view? (admin only — non-admin `shared:true` → 403 VIEW_FORBIDDEN).
export function canCreateShared(viewer: ViewViewer): boolean {
  return viewer.isAdmin;
}
