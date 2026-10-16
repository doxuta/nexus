// lib/appHome.ts — pure selection: an app's nav → what the app-scoped Home shows.
// App-face simplification (Founder 2026-07-10): when an app is the active lens, Home stops
// being a tenant-wide console — stat cards / recent records / dashboards all come from the
// app's OWN nav (already ACL-filtered server-side, INV-3). All Items keeps today's behavior;
// callers pass `null` there and ignore this selection entirely (INV-1).
import type { AppNavEntry } from "@/lib/api";

export interface AppHomeEntity {
  /** Entity slug (usable in /:entity URLs and record queries). */
  name: string;
  /** Nav label — the app's override label or the entity's own. */
  label: string;
}

export interface AppHomeSelection {
  /** First N entity nav items, in the admin-arranged nav order → stat cards. */
  statEntities: AppHomeEntity[];
  /** First entity nav item → "Cập nhật gần đây" source; null when the nav has no entity. */
  recentEntity: AppHomeEntity | null;
  /** Dashboard nav items, in nav order → "Bảng điều khiển" section (hidden when empty). */
  dashboards: AppNavEntry[];
  /** Nav resolved AND has zero visible items (unconfigured app or ACL hid everything). */
  isEmpty: boolean;
}

/** Home shows at most 4 stat cards — matches the existing All-Items KPI row. */
export const STAT_ENTITY_LIMIT = 4;

// Entity slug lives in href ("/<entityName>" per GET /apps/:name/nav contract) — refId is
// the sys_entities UUID, which is useless for record queries.
function entityNameFromHref(href: string): string {
  return href.replace(/^\//, "");
}

/**
 * @param nav ACL-filtered nav of the active app; `null` = still loading (or All Items —
 *            callers should not consult the selection in that mode).
 */
export function selectAppHome(nav: AppNavEntry[] | null): AppHomeSelection {
  const items = nav ?? [];
  const entities = items
    .filter((i) => i.type === "entity")
    .map((i) => ({ name: entityNameFromHref(i.href), label: i.label }));
  return {
    statEntities: entities.slice(0, STAT_ENTITY_LIMIT),
    recentEntity: entities[0] ?? null,
    dashboards: items.filter((i) => i.type === "dashboard"),
    // Loading (null) is NOT empty — empty is only a resolved nav with zero items.
    isEmpty: nav !== null && items.length === 0,
  };
}

/**
 * Scope tenant-wide rows (work queues, pending approvals) to the app being viewed: a row belongs
 * to the app when the entity it reads is in the app's nav. Without this an app's home showed the
 * tenant's OTHER apps' queues ("Lead mới", "Ticket tới hạn" inside a clinic app). `nav === null`
 * (still loading) yields [] — never the unscoped list, which would flash the wrong app's work.
 */
export function scopeToAppEntities<T>(rows: T[], nav: AppNavEntry[] | null, entityOf: (r: T) => string): T[] {
  if (!nav) return [];
  const names = new Set(nav.filter((i) => i.type === "entity").map((i) => entityNameFromHref(i.href)));
  return rows.filter((r) => names.has(entityOf(r)));
}

/**
 * Work queues an app home shows. The builder's explicit pick wins (in the order picked, ids of queues
 * since deleted dropped, tables outside the menu allowed); with no pick the queues are guessed from
 * the menu tables as before (scopeToAppEntities).
 */
export function pickAppQueues<T extends { id: string }>(rows: T[], nav: AppNavEntry[] | null, explicitIds: string[] | undefined, entityOf: (r: T) => string): T[] {
  if (explicitIds && explicitIds.length > 0) {
    const byId = new Map(rows.map((r) => [r.id, r]));
    return explicitIds.flatMap((id) => { const r = byId.get(id); return r ? [r] : []; });
  }
  return scopeToAppEntities(rows, nav, entityOf);
}
