// lib/listState.ts — pure helpers for record-list state persistence (felt-ease F7 "list amnesia").
//
// Two memories, one precedence rule:
//   • URL (?q= &sort=&order= &view= &field[op]=value) — shareable/deep-linkable list state, written
//     back shallowly via history.replaceState by LayoutList. The FILTER dialect is exactly
//     lib/filterModel's ?field[op]=value — this module reuses filtersToQuery/parseFilterQuery,
//     it does NOT invent a second grammar.
//   • localStorage — per-entity cosmetic prefs (viewMode + density, key `list:<entity>:view`,
//     same "list:<entity>" family the DataGrid column widths already use) and the id of the
//     saved view the user LAST APPLIED (key `list:<entity>:lastView` — the honest client-side
//     stand-in for a backend "default view" flag, which does not exist).
//
// Precedence on mount (deep links stay authoritative):
//   explicit URL params  >  persisted prefs / last-applied view  >  layout defaults.
//
// No React import — unit-testable in isolation (lib/listState.test.ts).

import { filtersToQuery, parseFilterQuery, RESERVED_QUERY_KEYS } from "@/lib/filterModel";
import type { FilterRow } from "@/lib/filterModel";
import { getStorageScope } from "@/lib/api";
import { readForScope } from "@/lib/scopedStorage";

export type ListViewMode = "table" | "kanban" | "calendar";
export type ListDensity = "compact" | "normal" | "relaxed";
export interface ListSort { field: string; order: "asc" | "desc" }

// Same key shape filterModel's parseFilterQuery matches — a bracketed key IS a filter param.
const FILTER_KEY_RE = /^(.+)\[(\w+)\]$/;

function isViewMode(v: unknown): v is ListViewMode {
  return v === "table" || v === "kanban" || v === "calendar";
}
function isDensity(v: unknown): v is ListDensity {
  return v === "compact" || v === "normal" || v === "relaxed";
}

// ── URL round-trip ─────────────────────────────────────────────────────────────

export interface UrlListState {
  q: string;
  viewMode: ListViewMode | null; // null = URL didn't say (NOT "table")
  sort: ListSort | null;
  filters: FilterRow[];
}

// Parse the page's ?search into list state. Sort semantics mirror the historical VIEC-HOM-NAY
// consumption in LayoutList verbatim: order defaults to "asc" unless explicitly "desc".
export function parseListSearch(search: string): UrlListState {
  const sp = new URLSearchParams(search);
  const sortField = sp.get("sort")?.trim() || "";
  const order: "asc" | "desc" =
    sp.get("order")?.trim().toLowerCase() === "desc" ? "desc" : "asc";
  const view = sp.get("view");
  return {
    q: sp.get("q")?.trim() ?? "",
    viewMode: isViewMode(view) ? view : null,
    sort: sortField ? { field: sortField, order } : null,
    filters: parseFilterQuery(search),
  };
}

// Does this URL carry EXPLICIT list state (deep link / drill-through)? When true, persisted
// prefs and the last-applied saved view must NOT auto-apply — the link is authoritative.
export function urlStateIsExplicit(s: UrlListState): boolean {
  return !!(s.q || s.viewMode || s.sort || s.filters.length > 0);
}
export function hasExplicitListState(search: string): boolean {
  return urlStateIsExplicit(parseListSearch(search));
}

export interface BuildListSearchInput {
  /** Existing window.location.search — unknown non-list params are preserved. */
  current: string;
  filters: FilterRow[];
  q: string;
  sort: ListSort | null;
  /** Layout defaultSort — when the live sort equals it, sort/order are OMITTED (clean URLs). */
  defaultSort?: ListSort | null;
  viewMode: ListViewMode;
  /**
   * The incoming URL named sort/order EXPLICITLY — keep emitting them even when they happen to
   * equal the layout default.
   *
   * Without this, a link that says what it wants gets it erased: the home screen's
   * "Lead mới chưa xử lý" card links `?order=desc&sort=created_at&status[eq]=New`, the values match
   * crm_leads' defaultSort, so the canonicalizer drops them and the shared URL becomes
   * `?status[eq]=New`. To be honest about the size of this: the ORDER the user sees does not change
   * today, because the layout default and the server default agree. What is lost is the link's
   * stated intent — the day someone edits the layout's defaultSort, every link like that silently
   * starts meaning something else.
   */
  keepSort?: boolean;
}

// Canonical query string for the current list state. Defaults are omitted (view=table, empty q,
// sort equal to the layout default) so a virgin list keeps a bare /entity URL.
export function buildListSearch(i: BuildListSearchInput): string {
  const out = new URLSearchParams();
  // Preserve unknown bare params; drop everything the list owns (filters + q/sort/order/view)
  // and the fetch-internal reserved keys (page/limit/expand/after) — they get re-emitted (or
  // intentionally not) from live state below.
  new URLSearchParams(i.current).forEach((v, k) => {
    if (FILTER_KEY_RE.test(k)) return;
    if (k === "view" || RESERVED_QUERY_KEYS.has(k)) return;
    out.append(k, v);
  });
  if (i.q.trim()) out.set("q", i.q.trim());
  const d = i.defaultSort;
  if (i.sort && (i.keepSort || !(d && d.field === i.sort.field && d.order === i.sort.order))) {
    out.set("sort", i.sort.field);
    out.set("order", i.sort.order);
  }
  if (i.viewMode !== "table") out.set("view", i.viewMode);
  const fq = filtersToQuery(i.filters);
  if (fq) new URLSearchParams(fq).forEach((v, k) => out.set(k, v));
  return out.toString();
}

// ── Mount-time precedence ─────────────────────────────────────────────────────

export interface ListViewPrefs {
  viewMode?: ListViewMode;
  density?: ListDensity;
  // showOwner: reveal the "Chủ sở hữu" column. Per-user and per-entity, not a layout column,
  // because owner_id is a SYSTEM column and never a sys_field — LayoutBuilder has nothing to
  // offer for it, so the layout config cannot express "show the owner". Off by default: turning
  // it on for everyone would silently add a column (and a directory lookup) to every list in
  // every tenant.
  showOwner?: boolean;
}

// URL wins > persisted prefs > defaults. Density has no URL param (cosmetic, prefs-only).
export function resolveInitialModes(
  url: Pick<UrlListState, "viewMode">,
  prefs: ListViewPrefs,
): { viewMode: ListViewMode; density: ListDensity } {
  return {
    viewMode: url.viewMode ?? prefs.viewMode ?? "table",
    density: prefs.density ?? "normal",
  };
}

// ── localStorage persistence (per entity) ─────────────────────────────────────
// Convenience-only: every reader tolerates junk/absence; every writer swallows quota/private-mode
// errors — persistence must never break the list (same discipline as lib/dataGrid).

// Keys carry the scope `${tenantId}:${userId}` (specs/062 L2b) so Nexus and each customer workspace keep
// their own list memory. Before L2b the key was `list:<entity>:view` — adopted once at home (readForScope).
export function listPrefsKey(entity: string): string {
  return `list:${getStorageScope().key}:${entity}:view`;
}

export function loadListPrefs(entity: string): ListViewPrefs {
  if (typeof window === "undefined") return {};
  try {
    const raw = readForScope((k) => `list:${k}:${entity}:view`, `list:${entity}:view`);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) return {};
    const o = parsed as Record<string, unknown>;
    const out: ListViewPrefs = {};
    if (isViewMode(o.viewMode)) out.viewMode = o.viewMode;
    if (isDensity(o.density)) out.density = o.density;
    if (typeof o.showOwner === "boolean") out.showOwner = o.showOwner;
    return out;
  } catch {
    return {};
  }
}

export function saveListPrefs(entity: string, prefs: ListViewPrefs): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(listPrefsKey(entity), JSON.stringify(prefs));
  } catch {
    // quota / private mode — never critical
  }
}

export function lastAppliedViewKey(entity: string): string {
  return `list:${getStorageScope().key}:${entity}:lastView`;
}

export function loadLastAppliedViewId(entity: string): string | null {
  if (typeof window === "undefined") return null;
  try {
    return readForScope((k) => `list:${k}:${entity}:lastView`, `list:${entity}:lastView`) || null;
  } catch {
    return null;
  }
}

export function saveLastAppliedViewId(entity: string, id: string): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(lastAppliedViewKey(entity), id);
  } catch {
    // ignore
  }
}

// Clear the stored id. With `ifId`, clear ONLY when the stored value matches (delete-view flow:
// removing view A must not forget a later-applied view B).
export function clearLastAppliedViewId(entity: string, ifId?: string): void {
  if (typeof window === "undefined") return;
  try {
    if (ifId !== undefined && window.localStorage.getItem(lastAppliedViewKey(entity)) !== ifId) return;
    window.localStorage.removeItem(lastAppliedViewKey(entity));
  } catch {
    // ignore
  }
}
