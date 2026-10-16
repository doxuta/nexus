"use client";

// "Lịch sử" tab — per-record audit log on the SLDS activity timeline (blueprint
// activity-timeline/base). One item per audit entry: the action as the title, the time, "Bởi <tên>"
// as the summary, and the changed values as the item's detail fields ("Trạng thái: New → Working").
// Items open by default so the diff reads at a glance, like the old inline list; the toggle still
// folds any of them away. A page of 20, "Tải thêm lịch sử" follows the cursor.
import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { FilePlus, History, Pencil, Share2, Trash2, Undo2, UserMinus, type LucideIcon } from "lucide-react";
import { ensureAuth, listRecordHistory, listTeamDirectory, listUserDirectory, ApiError, type AuditEntry, type FieldMeta } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { metaToSpec } from "@/lib/fieldSpec";
import { ENGINE_ACTORS } from "@/lib/auditLabels";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { FieldDisplay } from "@/components/fields/display";
import { ActivityTimeline } from "@/components/slds/ActivityTimeline";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { EmptyState } from "@/components/slds/EmptyState";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";

// Synthetic actor ids the engine stamps on audit rows → a readable name (never the raw token).
function systemActor(id: string): string | undefined {
  if (id === "__role__") return t("page.record.history.actor.role");
  if (id === "__entity__") return t("page.record.history.actor.entity");
  return ENGINE_ACTORS[id];
}

export const shortId = (id: string) => (id ?? "").slice(0, 8);

// Tile per action (standard icon colours: record-create / record-update / record-delete / recycle-bin).
const ACTIONS: Record<string, { icon: LucideIcon; tile: string; label: I18nKey }> = {
  create: { icon: FilePlus, tile: "record-create", label: "page.record.history.action.create" },
  update: { icon: Pencil, tile: "record-update", label: "page.record.history.action.update" },
  delete: { icon: Trash2, tile: "record-delete", label: "page.record.history.action.delete" },
  restore: { icon: Undo2, tile: "recycle-bin", label: "page.record.history.action.restore" },
  // specs/051 FR-007 manual shares.
  share: { icon: Share2, tile: "people", label: "page.record.history.action.share" },
  unshare: { icon: UserMinus, tile: "user", label: "page.record.history.action.unshare" },
};

const isShareAction = (entry: AuditEntry) => entry.action === "share" || entry.action === "unshare";

// Who a `share` / `unshare` row is about: `share` writes diff.after, `unshare` diff.BEFORE — so the
// generic after-diff rows would name nobody on a revoke. A person is `sharedWith` (user id), a whole
// team is `sharedWithTeam` (team id, #224).
function sharedTarget(entry: AuditEntry): { team: boolean; id: string } | undefined {
  for (const side of [entry.diff?.after, entry.diff?.before]) {
    const u = side?.sharedWith;
    if (typeof u === "string" && u) return { team: false, id: u };
    const g = side?.sharedWithTeam;
    if (typeof g === "string" && g) return { team: true, id: g };
  }
  return undefined;
}

/**
 * The `diff.after` entries a History-tab row shows (at most `max`). An OBJECT value that no field
 * of this record describes is dropped: approval decisions share this audit trail, and since
 * DAILY-QUEUE #70 their diff carries `definition` (the chain the decision executed) — printed
 * with String() it read "[object Object]" on the customer's record page. A field's own object
 * (currency {amount,currencyCode}) still renders through FieldDisplay.
 */
export function historyDiffRows(
  after: Record<string, unknown> | undefined,
  fields: Record<string, FieldMeta>,
  max = 5,
): [string, unknown][] {
  return Object.entries(after ?? {})
    .filter(([k, v]) => fields[k] || v === null || typeof v !== "object")
    .slice(0, max);
}

/**
 * One changed value in the history diff, read the way the rest of the app reads values.
 *
 * The diff holds RAW stored JSON: a picklist is its code ("New"), a user field is a bare uuid, a
 * currency is {amount,currencyCode}. FieldDisplay is the same renderer the record body uses, so
 * labels, money, dates and booleans resolve here exactly as they do above; a value with no field
 * spec (ownerId is not in sys_fields) falls back to the directory names, then to the raw text.
 * Nothing is invented: an id that resolves to no name stays an id.
 */
function HistoryValue({ field, value, names }: { field?: FieldMeta; value: unknown; names: Record<string, string> }) {
  if (value === null || value === undefined || value === "") return <span className="slds-text-color_weak">∅</span>;
  if (typeof value === "string" && (!field || field.fieldType === "user")) {
    const n = names[value] ?? systemActor(value);
    if (n) return <span>{n}</span>;
  }
  if (!field) return <span>{String(value)}</span>;
  return <FieldDisplay field={metaToSpec(field)} value={value} />;
}

export function HistoryTab({ entity, recordId, allFields }: { entity: string; recordId: string; allFields: FieldMeta[] }) {
  const [entries, setEntries] = useState<AuditEntry[]>([]);
  const [nextCursor, setNextCursor] = useState<string | undefined>();
  const [loading, setLoading] = useState(true);
  const [loadingMore, setLoadingMore] = useState(false);
  const [errMsg, setErrMsg] = useState("");
  // Items the user folded away; everything else stays open (see header).
  const [folded, setFolded] = useState<string[]>([]);

  const histFields = useMemo(
    () => Object.fromEntries(allFields.map((f) => [f.name, f])) as Record<string, FieldMeta>,
    [allFields],
  );

  // Actor names from /users/directory — reachable by any authenticated user (the old
  // /admin/users call 403'd for a non-admin and every name degraded to a uuid) and cached under the
  // same react-query key as the record header's owner lookup, so this adds no request.
  const { data: directory } = useQuery({
    queryKey: ["user-directory"],
    queryFn: () => listUserDirectory(),
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  const names = useMemo(
    () => Object.fromEntries((directory ?? []).map((u) => [u.id, u.fullName?.trim() || u.email])),
    [directory],
  );

  // Team names for `sharedWithTeam` rows; the same key/endpoint as the share dialog's picker. Any
  // signed-in user may read it, and a failed read degrades to the raw id like the user directory.
  const { data: teamDirectory } = useQuery({
    queryKey: ["team-directory"],
    queryFn: () => listTeamDirectory(),
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  const teamNames = useMemo(() => Object.fromEntries((teamDirectory ?? []).map((g) => [g.id, g.name])), [teamDirectory]);

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        await ensureAuth();
        const result = await listRecordHistory(entity, recordId, 20);
        if (!cancelled) {
          setEntries(result.entries);
          setNextCursor(result.nextCursor);
          setLoading(false);
        }
      } catch (e) {
        if (!cancelled) {
          setErrMsg(e instanceof ApiError ? e.message : t("page.record.history.loadError"));
          setLoading(false);
        }
      }
    })();
    return () => { cancelled = true; };
  }, [entity, recordId]);

  const loadMore = async () => {
    if (!nextCursor || loadingMore) return;
    setLoadingMore(true);
    try {
      await ensureAuth();
      const result = await listRecordHistory(entity, recordId, 20, nextCursor);
      setEntries((prev) => [...prev, ...result.entries]);
      setNextCursor(result.nextCursor);
    } catch (e) {
      setErrMsg(e instanceof ApiError ? e.message : t("page.record.history.loadMoreError"));
    } finally {
      setLoadingMore(false);
    }
  };

  if (loading) {
    return (
      <div className="relative h-24">
        <Spinner size="small" label={t("page.record.history.loading")} />
      </div>
    );
  }

  // Customer user -> engine id -> the server's builder label (specs/062 L3, shown with a badge) -> short id.
  // The engine id comes BEFORE the label: the server labels any non-user actor on a meta row, and a
  // schedule's system id must read as the schedule, not pose as a Nexus builder.
  const actorOf = (entry: AuditEntry) => {
    const id = entry.actorUserId;
    const known = names[id] ?? systemActor(id);
    if (known) return <strong>{known}</strong>;
    if (entry.actorLabel) {
      return (
        <>
          <strong>{entry.actorLabel}</strong> <Badge variant="lightest">{t("audit.byBuilder")}</Badge>
        </>
      );
    }
    return <strong>{shortId(id)}</strong>;
  };
  const sharedWithRow = (entry: AuditEntry) => {
    const who = sharedTarget(entry);
    if (!who) return [];
    if (who.team) {
      // Directory loaded without this id = the team was deleted since; not loaded = show the id.
      const name = teamNames[who.id] ?? (teamDirectory ? t("page.record.history.teamGone") : who.id);
      return [{ label: t("page.record.history.sharedWithTeam"), value: <span>{name}</span> }];
    }
    return [{ label: t("page.record.history.sharedWith"), value: <HistoryValue value={who.id} names={names} /> }];
  };
  const diffRows = (entry: AuditEntry) =>
    historyDiffRows(entry.diff?.after, histFields).map(([k, v]) => {
      const f = histFields[k];
      return {
        label: f?.label ?? k,
        value: (
          <>
            <HistoryValue field={f} value={entry.diff?.before?.[k]} names={names} />
            {" → "}
            <HistoryValue field={f} value={v} names={names} />
          </>
        ),
      };
    });
  const ids = entries.map((e) => e.id);

  return (
    <>
      {errMsg && <ScopedNotification theme="warning" className="slds-m-bottom_small">{errMsg}</ScopedNotification>}
      {entries.length === 0 && !errMsg ? (
        <EmptyState icon={History} title={t("page.record.history.emptyTitle")} description={t("page.record.history.emptyText")} />
      ) : (
        <ActivityTimeline
          expanded={ids.filter((id) => !folded.includes(id))}
          onExpandedChange={(open) => setFolded(ids.filter((id) => !open.includes(id)))}
          items={entries.map((entry) => {
            const meta = ACTIONS[entry.action];
            return {
              id: entry.id,
              icon: meta?.icon ?? History,
              iconName: meta?.tile ?? "record",
              title: meta ? t(meta.label) : entry.action,
              date: formatDateTime(new Date(entry.at)),
              summary: (
                <>
                  {t("page.record.history.by")} {actorOf(entry)}
                </>
              ),
              fields: isShareAction(entry) ? sharedWithRow(entry) : diffRows(entry),
            };
          })}
        />
      )}
      {nextCursor && (
        <Button className="slds-m-top_small" onClick={loadMore} disabled={loadingMore}>
          {loadingMore ? t("page.record.loading") : t("page.record.history.loadMore")}
        </Button>
      )}
    </>
  );
}
