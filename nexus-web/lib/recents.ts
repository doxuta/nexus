// lib/recents.ts — per-user "recently visited" stores (W3 C/D/E), localStorage only.
// Backend sys_recent is P2 (plan §3.9) — until then these are honest client-side recents.
// Two small stores, all namespaced per user (same scheme as lib/pins.ts):
//
//   1. recent ROUTES  (cap 5)  — feeds ⌘K "Gần đây" group; captured by AppShell on nav.
//   2. recent RECORDS (cap 8)  — feeds Home "Vừa xem" widget; captured by LayoutDetail
//      once the record + its VIP label have actually loaded (no phantom entries on 404).
//
// Pure functions take an explicit userKey for unit tests; hooks/components use the default.

import { getStorageScope } from "@/lib/api";
import { readForUser } from "@/lib/scopedStorage";
import { isLiveBuilderPath } from "@/lib/studioRoutes";

export interface RecentRoute { href: string; label: string; at: number }
export interface RecentRecord { entity: string; id: string; label: string; at: number }

export const RECENT_ROUTES_CAP = 5;
export const RECENT_RECORDS_CAP = 8;

const ROUTES_KEY = "nexus.recent-routes.v1";
const RECORDS_KEY = "nexus.recent-records.v1";
const RECENTS_EVENT = "nexus:recents-changed";

// `${tenantId}:${userId}` (specs/062 L2b) — Nexus and each customer workspace keep their own recents.
function userKey(): string {
  return getStorageScope().key;
}

// `uk` omitted = the current scope: only then may a pre-L2b value (bare user id) be adopted.
function readRaw(key: string, uk?: string): string | null {
  return uk === undefined ? readForUser((k) => `${key}:${k}`) : window.localStorage.getItem(`${key}:${uk}`);
}

function read<T>(key: string, uk?: string): T[] {
  if (typeof window === "undefined") return [];
  try {
    const raw = readRaw(key, uk);
    if (!raw) return [];
    const parsed = JSON.parse(raw);
    return Array.isArray(parsed) ? (parsed as T[]) : [];
  } catch {
    return [];
  }
}

function write(key: string, uk: string, value: unknown): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(`${key}:${uk}`, JSON.stringify(value));
    window.dispatchEvent(new CustomEvent(RECENTS_EVENT));
  } catch {
    /* fail silent — recents are a convenience */
  }
}

/** Subscribe to any recents change in this tab; returns the unsubscribe fn. */
export function onRecentsChanged(cb: () => void): () => void {
  window.addEventListener(RECENTS_EVENT, cb);
  window.addEventListener("storage", cb);
  return () => {
    window.removeEventListener(RECENTS_EVENT, cb);
    window.removeEventListener("storage", cb);
  };
}

// ── 1. Recent routes (⌘K "Gần đây") ────────────────────────────────────────────

// Routes deleted by the Studio IA clean-up (kh7) must not resurface from old localStorage as 404 links.
const DEAD_ADMIN = /^\/admin\/(doctor|api-keys)(\/|\?|#|$)/;
const isLiveRecentHref = (h: string) => !DEAD_ADMIN.test(h) && (!h.startsWith("/studio") || isLiveBuilderPath(h));

export function loadRecentRoutes(uk?: string): RecentRoute[] {
  return read<RecentRoute>(ROUTES_KEY, uk).filter((r) => isLiveRecentHref(r.href));
}

/** Most-recent-first, dedup by href, cap RECENT_ROUTES_CAP. Pure on the list. */
export function pushRecentRoute(
  list: RecentRoute[],
  entry: { href: string; label: string },
  now: number = Date.now(),
): RecentRoute[] {
  const rest = list.filter((r) => r.href !== entry.href);
  return [{ ...entry, at: now }, ...rest].slice(0, RECENT_ROUTES_CAP);
}

export function recordRouteVisit(entry: { href: string; label: string }): void {
  // load with NO key: that is the path that adopts a pre-L2b value before we write the new key.
  write(ROUTES_KEY, userKey(), pushRecentRoute(loadRecentRoutes(), entry));
}

// ── 2. Recent records (Home "Vừa xem") ────────────────────────────────────────

export function loadRecentRecords(uk?: string): RecentRecord[] {
  return read<RecentRecord>(RECORDS_KEY, uk);
}

/** Most-recent-first, dedup by entity+id, cap RECENT_RECORDS_CAP. Pure on the list. */
export function pushRecentRecord(
  list: RecentRecord[],
  entry: { entity: string; id: string; label: string },
  now: number = Date.now(),
): RecentRecord[] {
  const rest = list.filter((r) => !(r.entity === entry.entity && r.id === entry.id));
  return [{ ...entry, at: now }, ...rest].slice(0, RECENT_RECORDS_CAP);
}

export function recordRecordVisit(entry: { entity: string; id: string; label: string }): void {
  write(RECORDS_KEY, userKey(), pushRecentRecord(loadRecentRecords(), entry));
}

/**
 * Drop entries whose entity is no longer among `known`. Pure on the list.
 *
 * Recents outlive the thing they point at. Once an entity is deleted (or the user's access to it
 * is withdrawn) its rows become links that 404, and the subtitle falls back to the machine name —
 * "51K-123.45 · xe_cho_thue" — because no label can be resolved for an entity that is gone. A row
 * that cannot be opened is not something you recently viewed; it is a dead link wearing the
 * engine's vocabulary, which is exactly what the customer face is not allowed to show.
 */
export function filterKnownRecords(list: RecentRecord[], known: string[]): RecentRecord[] {
  const set = new Set(known);
  return list.filter((r) => set.has(r.entity));
}

/**
 * Prune dead entries from storage and return what remains.
 *
 * `known` MUST be a loaded, non-empty entity list — an empty one means "not loaded yet", and
 * pruning against it would wipe every recent the first time the widget renders. Callers pass the
 * list only once it has arrived; this guard makes the mistake impossible from here too.
 */
export function pruneRecentRecords(known: string[], uk?: string): RecentRecord[] {
  const list = loadRecentRecords(uk);
  if (known.length === 0) return list;
  const kept = filterKnownRecords(list, known);
  if (kept.length !== list.length) write(RECORDS_KEY, uk ?? userKey(), kept);
  return kept;
}
