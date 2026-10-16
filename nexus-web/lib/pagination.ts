// lib/pagination.ts — pure helpers for List pagination-mode selection (contract §5.7 + api-conventions
// "Chế độ 2 — Cursor/Keyset"). Kept out of the component so the cursor-vs-offset decision is
// unit-testable without a DOM.
//
// Backend entrance (opened 2026-09-28, DAILY-QUEUE #42 / specs/039-cursor-bootstrap): the FIRST
// page of a cursor chain is `GET /:entity?paginate=cursor&sort=…&order=…&limit=…` — it returns
// `pagination.nextCursor`; later pages add `&after=<nextCursor>` and must NOT send `page` (422
// PAGINATION_MODE_CONFLICT). The OFFSET envelope still never carries a cursor (by design: its ORDER
// BY has no id tie-break, so an offset boundary would gap/dupe a keyset walk). The record list
// (components/layout/LayoutList) picks its mode with choosePaginationMode and pages through
// buildListPageQuery + fetchListPage (FE-cursor-bootstrap-a, DAILY-QUEUE #43).

import { listRecordsCursorPage, listRecordsPage, type ApiRecord, type FieldMeta } from "@/lib/api";

// System / physical columns the engine can keyset-sort on. MUST mirror the backend allowlist
// EXACTLY (internal/services/record_orchestration.go buildSortAllowlist → {created_at, updated_at,
// id}) — a column here that the backend rejects would make the FE request cursor mode and get a
// 422 CURSOR_REQUIRES_INDEXED_SORT. `updated_by` is NOT sortable server-side, so it is excluded.
export const CURSOR_SYSTEM_COLUMNS = new Set<string>([
  "id",
  "created_at",
  "updated_at",
]);

// Is a given sort field eligible for CURSOR (keyset) mode? The engine can cursor-sort on a physical/
// indexed column only: a system column, a relation FK, a `unique` field, or a field the admin marked
// `indexed` (D1 generated column). JSON-only (`data`) fields without an index can page in OFFSET mode
// but never CURSOR mode. Mirrors the backend allowlist so the FE picks the mode the server accepts.
export function isCursorSortable(
  fieldName: string | undefined,
  fieldsByName: Record<string, FieldMeta>,
): boolean {
  if (!fieldName) return false;
  if (CURSOR_SYSTEM_COLUMNS.has(fieldName)) return true;
  const f = fieldsByName[fieldName];
  if (!f) return false;
  if (f.fieldType === "relation") return true; // FK column <field>_id is indexed
  if (f.isUnique) return true; // unique index
  if (f.options?.indexed === true) return true; // D1 generated-column index
  return false;
}

export type PageMode = "cursor" | "offset";

// Decide the pagination mode for the current sort. No sort → the server defaults to `created_at desc`
// (a cursor-eligible system column), so cursor is safe; but we return "offset" when no sort is set to
// keep the count/total UX (offset gives `total`, cursor does not). When a sort IS active and the field
// is cursor-eligible, prefer cursor (O(1) deep paging); otherwise fall back to offset.
export function choosePaginationMode(
  sort: { field: string; order: "asc" | "desc" } | null | undefined,
  fieldsByName: Record<string, FieldMeta>,
): PageMode {
  if (!sort) return "offset";
  return isCursorSortable(sort.field, fieldsByName) ? "cursor" : "offset";
}

// Extract the opaque next-page cursor from a list response's pagination block. `null` (or absent)
// means "no more pages" (last page) per api-conventions. Any non-string → null (defensive).
export function parseNextCursor(pagination: unknown): string | null {
  if (!pagination || typeof pagination !== "object") return null;
  const nc = (pagination as { nextCursor?: unknown }).nextCursor;
  return typeof nc === "string" && nc.length > 0 ? nc : null;
}

// ── The list's page client (FE-cursor-bootstrap-a) ─────────────────────────────────────────────
// One request shape per mode. `next` is what the previous page handed back: the 1-based page
// number (offset) or the opaque nextCursor (cursor); null = first page.
export interface ListPageRequest {
  mode: PageMode;
  sort: { field: string; order: "asc" | "desc" } | null;
  expand: string[];
  q: string;
  /** Serialised ?field[op]=value filters (lib/filterModel filtersToQuery). */
  filterQuery: string;
  limit: number;
  next: number | string | null;
}

// Cursor: `paginate=cursor` on every page (spec §2.1 accepts it with or without `after`) and never
// `page` (422 PAGINATION_MODE_CONFLICT). The cursor is bound to sort + order (codec v3), so the
// caller re-sends the same sort on every page of a chain and starts a NEW chain (next=null) when
// the sort changes. Offset keeps the historical key order: sort, order, expand, limit, page, q, filters.
export function buildListPageQuery(r: ListPageRequest): string {
  const p = new URLSearchParams();
  if (r.sort) {
    p.set("sort", r.sort.field);
    p.set("order", r.sort.order);
  }
  if (r.expand.length) p.set("expand", r.expand.join(","));
  p.set("limit", String(r.limit));
  if (r.mode === "cursor") {
    p.set("paginate", "cursor");
    if (typeof r.next === "string" && r.next) p.set("after", r.next);
  } else {
    p.set("page", String(typeof r.next === "number" ? r.next : 1));
  }
  if (r.q) p.set("q", r.q);
  if (r.filterQuery) new URLSearchParams(r.filterQuery).forEach((v, k) => p.set(k, v));
  return p.toString();
}

export interface ListPageResult {
  records: ApiRecord[];
  /** Server total (offset). null in cursor mode: keyset pages never COUNT(*). */
  total: number | null;
  /** What to pass as `next` for the following page; null = no more pages. */
  next: number | string | null;
  /** HIDDEN-COL-SIGNAL meta.hiddenColumns (0 when absent). */
  hiddenColumns: number;
}

export async function fetchListPage(entity: string, mode: PageMode, query: string): Promise<ListPageResult> {
  if (mode === "cursor") {
    const r = await listRecordsCursorPage(entity, query);
    return { records: r.records, total: null, next: r.nextCursor, hiddenColumns: r.meta?.hiddenColumns ?? 0 };
  }
  const r = await listRecordsPage(entity, query);
  const { page, totalPages, total } = r.pagination;
  return { records: r.records, total, next: page < totalPages ? page + 1 : null, hiddenColumns: r.meta?.hiddenColumns ?? 0 };
}
