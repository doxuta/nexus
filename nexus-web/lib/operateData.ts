// Vận hành › Dữ liệu helpers (specs/078 lát u7-du-lieu-danh-sach-bang).
import { useProjectQuery } from "@/components/studio/useProjectQuery";
import { getUsage } from "@/lib/admin";
import { ApiError, listEntities, type EntityMeta, type RecordPurgeReport } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import { studioHref } from "@/lib/studioRoutes";

const TAIL = { import: "van-hanh/du-lieu/:bang/nhap", batch: "van-hanh/du-lieu/:bang/tao-nhieu", deleted: "van-hanh/du-lieu/:bang/thung-rac" } as const;

/**
 * LayoutList header links (`/<bang>/import|batch|deleted`) -> the Studio route of the same table.
 * Anything else (another table, an absolute URL, an unknown tail) is refused with null: the caller drops it, so
 * a click can never fall back to `window.location.assign` and leave the Studio frame.
 */
export function operateListHref(slug: string, entity: string, href: string): string | null {
  const m = /^\/([a-z][a-z0-9_]*)\/(import|batch|deleted)$/.exec(href);
  if (!m || m[1] !== entity) return null;
  return studioHref(slug, TAIL[m[2] as keyof typeof TAIL], { bang: entity });
}

/** The longest window GET /admin/usage accepts (usageMaxDays on the server). */
const USAGE_MAX_DAYS = 366;

/**
 * The usage window ending on the SERVER's today. The server refuses a `to` after its own today (APP_TIMEZONE),
 * so the calendar day is read in the server's `timezone`, never from the browser clock (specs/078 spec 20b).
 */
export function usageRange(rangeDays: number, now: Date, timezone: string): { from: string; to: string } {
  const to = new Intl.DateTimeFormat("en-CA", { timeZone: timezone, year: "numeric", month: "2-digit", day: "2-digit" }).format(now);
  const d = new Date(`${to}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() - (Math.min(Math.max(rangeDays, 1), USAGE_MAX_DAYS) - 1));
  return { from: d.toISOString().slice(0, 10), to };
}

export type KnownEntity = { state: "loading" } | { state: "error"; error: unknown; refetch: () => unknown } | { state: "missing" } | { state: "ok"; label: string };

/**
 * Asks the table list BEFORE any data call (FR-133): a name in the URL that is not a table of this project
 * (`admin`, `auth`, `metadata`…) must never become a request to `/${bang}/…`.
 */
/** The table list of THIS project, gated by projectTokenMatches (never another customer's cached list). */
export const useProjectEntities = (slug: string) => useProjectQuery(slug, ["operate", "entities"], listEntities);

export function useKnownEntity(slug: string, bang: string): KnownEntity {
  const q = useProjectEntities(slug);
  if (q.isPending) return { state: "loading" };
  if (q.isError) return { state: "error", error: q.error, refetch: q.refetch };
  const hit = q.data?.find((e) => e.name === bang);
  return hit ? { state: "ok", label: hit.label } : { state: "missing" };
}

type UsageRecord = { entity: string; label: string; count: number };

/** Joins usage rows to the project's tables by machine name (R12): a deleted table is dropped, a missing one stays absent (blank cell). */
export function mergeRecordCounts(entities: Pick<EntityMeta, "name">[], records: UsageRecord[]): Map<string, number> {
  const byName = new Map(records.map((r) => [r.entity, r.count] as const));
  const out = new Map<string, number>();
  for (const e of entities) {
    const n = byName.get(e.name);
    if (n !== undefined) out.set(e.name, n);
  }
  return out;
}

/**
 * Live row count per table from the ONE usage read (`GET /admin/usage` -> current.records[]), keyed by table code (R12).
 * The owner hook of the "Bản ghi" figure: Vận hành › Dữ liệu and the Dựng table list/page all import it, none reads usage itself.
 * `data` is null while the read is pending, failed, or the server has no `current` block (route/gauge not there yet): callers draw no figure.
 * Counts are the server's hourly snapshot (up to one hour stale); `computedAt` says when.
 */
export function useRecordCounts(slug: string): { data: Map<string, number> | null; computedAt: string | null; isPending: boolean; isError: boolean } {
  const q = useProjectQuery(slug, ["operate", "usage", "current"], () => getUsage());
  const records = q.data?.current?.records;
  return {
    data: records ? new Map(records.map((r) => [r.entity, r.count] as const)) : null,
    computedAt: q.data?.current?.computedAt ?? null,
    isPending: q.isPending,
    isError: q.isError,
  };
}

// ── Permanent delete (specs/078 lát u7-du-lieu-thung-rac-xoa-han, FR-058) ──

/**
 * Whether to OFFER "Xoá vĩnh viễn". Fail closed: only a settled /auth/me can say yes. The server decides for real
 * (RequireBuilderTier); this only keeps the button from appearing where the answer is always 403.
 * `workspace` is the block the SERVER puts in /auth/me inside a build session; the tab's raw sessionStorage is never a source.
 */
export function canPurge(me: { status: "loading" | "error" | "ok"; user?: { isPlatformOperator?: boolean } | null; workspace?: unknown }): boolean {
  if (me.status !== "ok") return false;
  return me.user?.isPlatformOperator === true || !!me.workspace;
}

export interface PurgeLabels {
  entity: (name: string) => string;
  field: (entity: string, field: string) => string;
}

/** The dry-run report in the customer's words: never a physical table name, never "ở N bảng". */
export function purgeSummary(r: RecordPurgeReport, labels: PurgeLabels): { headline: string; cleared: string[]; blocked: string[]; blobsNote?: string } {
  const files = r.tables.find((x) => x.table === "sys_files")?.rows ?? 0;
  const children = Math.max(0, r.records.length - 1);
  const headline =
    tf("studio.operate.recycle.sum.head", { records: r.records.length }) +
    (children > 0 ? tf("studio.operate.recycle.sum.children", { k: children }) : "") +
    (files > 0 ? tf("studio.operate.recycle.sum.files", { files }) : "") +
    t("studio.operate.recycle.sum.tail");
  const groups = new Map<string, { entity: string; field: string; count: number }>();
  for (const c of r.cleared) {
    const k = `${c.entity}|${c.field}`;
    const g = groups.get(k);
    if (g) g.count += 1;
    else groups.set(k, { entity: c.entity, field: c.field, count: 1 });
  }
  return {
    headline,
    cleared: [...groups.values()].map((g) => tf("studio.operate.recycle.sum.cleared", { count: g.count, table: labels.entity(g.entity), field: labels.field(g.entity, g.field) })),
    blocked: r.blocked.map((b) => tf("studio.operate.recycle.sum.blocked", { count: b.count, table: labels.entity(b.entity) })),
    ...(r.blobsNote ? { blobsNote: r.blobsNote } : {}),
  };
}

/**
 * What a refused purge says, and what the screen does next: `hide` = stop offering the button for this session
 * (403, or a 404 that is not "record gone": the route is absent), `gone` = the record is no longer there (drop the row).
 */
export function purgeRefusal(e: unknown, labels: PurgeLabels): { text: string; hide?: boolean; gone?: boolean } {
  if (!(e instanceof ApiError)) return { text: t("studio.operate.recycle.err.generic") };
  switch (e.code) {
    case "FORBIDDEN":
      return { text: t("studio.operate.recycle.err.needBuilder"), hide: true };
    case "RECORD_NOT_FOUND":
      return { text: t("studio.operate.recycle.err.gone"), gone: true };
    case "RECORD_NOT_IN_RECYCLE_BIN":
      return { text: t("studio.operate.recycle.err.notInBin"), gone: true };
    case "RECORD_WRITE_CONFLICT":
      return { text: t("studio.operate.recycle.err.busy") };
    case "RECORD_PURGE_TOO_LARGE":
      return { text: tf("studio.operate.recycle.err.tooLarge", { max: Number(e.details?.max) || 500 }) };
    case "RECORD_PURGE_LIVE_CHILDREN": {
      const raw = Array.isArray(e.details?.blockers) ? (e.details.blockers as { entity: string; count: number }[]) : [];
      const lines = raw.map((b) => tf("studio.operate.recycle.sum.blocked", { count: b.count, table: labels.entity(b.entity) }));
      return { text: lines.length ? lines.join(" ") : t("studio.operate.recycle.sum.blockedFallback") };
    }
    default:
      return e.status === 404 || e.status === 403 ? { text: t("studio.operate.recycle.err.needBuilder"), hide: true } : { text: t("studio.operate.recycle.err.generic") };
  }
}
