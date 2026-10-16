"use client";

// RecycleBinScreen — soft-deleted records viewer + restore (W9c, ../specs/026-recycle-bin/spec.md)
// on SLDS 2.264.1 (specs/038 T027). Design read: object home for the customer face — blueprints
// page-headers/object-home (ObjectHomePage template) + data-tables/advanced (kit DataTable, row
// action menu) + prompt (restore confirm) + illustration (empty bin) + scoped-notifications.
//
// Lists deleted records through the SAME column pipeline as the live list (shared
// buildListGridColumns / toDataTableColumns / ListCell — no fork) so columns, widths and
// FieldDisplay formatting match. The per-row "Khôi phục" action (gated on the `delete` grant)
// confirms in a prompt, POSTs restore, drops the row on success, and maps 409/403/404 to honest
// toasts. Chrome comes from the (app) route-group AppShell (its breadcrumb leads back to the live
// list); this renders content only.
//
// Honesty notes:
//   • Restore needs the `delete` grant (restore is the inverse of delete). Without it the action
//     column is hidden and a read-only note explains why.
//   • The deleted list rides the live list's ACL/A5/field-ACL — you only see what you could see alive.
//   • Relation columns are NOT expanded here (deleted list is offset-only, no ?expand) so they may
//     show a raw id — accepted for a recovery surface.
//   • No sorting: GET /:entity/deleted is offset-only here, so headers are not clickable.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ColumnSizingState } from "@tanstack/react-table";
import { ChevronDown, Flame, Inbox, Lock, RotateCcw, Trash2 } from "lucide-react";
import {
  ensureAuth, getLayout, listFields, listDeletedPage, restoreRecord, ApiError,
  type EntityLayout, type FieldMeta, type ApiRecord, type RecordPurgeReport,
} from "@/lib/api";
import { buildListGridColumns, toDataTableColumns } from "@/components/layout/listColumns";
import { FieldHelp } from "@/components/fields/widgets";
import { Button } from "@/components/slds/Button";
import { DataTable } from "@/components/slds/DataTable";
import { useEmbedded } from "@/components/slds/EmbeddedSurface";
import { EmptyState } from "@/components/slds/EmptyState";
import { MenuItem } from "@/components/slds/Menu";
import { Prompt } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { ObjectHomePage } from "@/components/slds/templates/ObjectHomePage";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { RecordPurgeConfirm } from "@/components/studio/operate/RecordPurgeConfirm";
import { toast } from "@/components/ui/Toaster";
import { useAuth } from "@/lib/auth/AuthProvider";
import { loadColWidths, saveColWidths } from "@/lib/dataGrid";
import { getHelp } from "@/lib/help-content";
import { t, tf } from "@/lib/i18n";
import { usePermissions } from "@/lib/metadata/permissions";
import { useEntities } from "@/lib/metadata/hooks";
import { recordLabel } from "@/lib/recordLabel";
import { removeById, restoreErrorMessage, shouldDropOnError } from "@/lib/recycleModel";

type LoadState = "loading" | "ok" | "error";
const PAGE_LIMIT = 50;
const softDeleteHelp = getHelp("softDelete");

/** Studio only (specs/078 FR-058): whether to offer "Xoá vĩnh viễn" and how to ask the customer scope before it. */
export interface PurgeGate { allowed: boolean; assertScope: () => boolean }

export function RecycleBinScreen({ entity, purge }: { entity: string; purge?: PurgeGate }) {
  const embedded = useEmbedded();
  const { user } = useAuth();
  const isAdmin = !!user?.isAdmin;
  const { can } = usePermissions();
  const canRestore = can("delete", entity); // restore rides the `delete` grant (api-conventions §13.1)

  const { data: allEntities = [] } = useEntities();
  const entityLabel = useMemo(
    () => allEntities.find((e) => e.name === entity)?.label ?? entity,
    [allEntities, entity],
  );

  const [loadState, setLoadState] = useState<LoadState>("loading");
  const [errMsg, setErrMsg] = useState("");
  const [layout, setLayout] = useState<EntityLayout | null>(null);
  const [fields, setFields] = useState<FieldMeta[]>([]);
  const [records, setRecords] = useState<ApiRecord[]>([]);

  const [page, setPage] = useState(1);
  const [totalPages, setTotalPages] = useState(1);
  const [total, setTotal] = useState(0);
  const [loadingMore, setLoadingMore] = useState(false);
  const [loadMoreErr, setLoadMoreErr] = useState("");
  const hasMore = page < totalPages;

  const [confirmTarget, setConfirmTarget] = useState<ApiRecord | null>(null);
  const [restoring, setRestoring] = useState(false);
  // Permanent delete (Studio only): the row being asked about, and "the server said no" for the rest of the session.
  const [purgeTarget, setPurgeTarget] = useState<ApiRecord | null>(null);
  const [purgeDenied, setPurgeDenied] = useState(false);
  const showPurge = embedded && !!purge?.allowed && !purgeDenied;

  const reqIdRef = useRef(0);
  const gridBox = useRef<HTMLDivElement>(null);

  // User-resized widths, persisted per user + entity (lib/dataGrid) under the bin's own key.
  // Safe against hydration: no header cell renders before the layout loads client-side.
  const widthKey = `deleted:${entity}`;
  const [colSizing, setColSizing] = useState<ColumnSizingState>(() => loadColWidths(widthKey));
  const saveTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const resizeColumns = (next: ColumnSizingState) => {
    setColSizing(next);
    if (saveTimer.current) clearTimeout(saveTimer.current);
    saveTimer.current = setTimeout(() => saveColWidths(widthKey, next), 300);
  };
  useEffect(() => () => { if (saveTimer.current) clearTimeout(saveTimer.current); }, []);

  const fieldsByName = useMemo(
    () => Object.fromEntries(fields.map((f) => [f.name, f])),
    [fields],
  );
  // No record link: a deleted row opens nothing. The row header's value is the record label, so
  // the row action button is named after the record ("Thêm thao tác cho Acme"), not its id.
  const tableColumns = useMemo(() => {
    const cols = toDataTableColumns(buildListGridColumns(layout?.views.list?.columns ?? [], fieldsByName, fields));
    if (cols[0]) cols[0] = { ...cols[0], accessorFn: (r: ApiRecord) => recordLabel(r, fields) };
    return cols;
  }, [layout, fieldsByName, fields]);

  // Initial load — layout + fields + first deleted page (offset, no expand; see header note).
  useEffect(() => {
    let cancelled = false;
    const myReq = ++reqIdRef.current;
    (async () => {
      try {
        await ensureAuth();
        const [lay, flds, firstPage] = await Promise.all([
          getLayout(entity),
          listFields(entity),
          listDeletedPage(entity, `limit=${PAGE_LIMIT}`),
        ]);
        if (cancelled || myReq !== reqIdRef.current) return;
        if (!lay) throw new ApiError(404, "LAYOUT_NOT_FOUND", t("page.recycleBin.layoutMissing"));
        setLayout(lay);
        setFields(flds);
        setRecords(firstPage.records);
        setPage(firstPage.pagination.page);
        setTotalPages(firstPage.pagination.totalPages);
        setTotal(firstPage.pagination.total);
        setLoadState("ok");
      } catch (e) {
        if (cancelled || myReq !== reqIdRef.current) return;
        setErrMsg(e instanceof ApiError ? e.message : t("page.recycleBin.loadFailed"));
        setLoadState("error");
      }
    })();
    return () => { cancelled = true; };
  }, [entity]);

  const loadMore = () => {
    if (loadingMore || page >= totalPages) return;
    const nextPage = page + 1;
    setLoadingMore(true);
    setLoadMoreErr("");
    (async () => {
      try {
        await ensureAuth();
        const res = await listDeletedPage(entity, `limit=${PAGE_LIMIT}&page=${nextPage}`);
        setRecords((prev) => {
          const seen = new Set(prev.map((r) => r.id));
          return [...prev, ...res.records.filter((r) => !seen.has(r.id))];
        });
        setPage(res.pagination.page);
        setTotalPages(res.pagination.totalPages);
        setTotal(res.pagination.total);
      } catch (e) {
        setLoadMoreErr(e instanceof ApiError ? e.message : t("page.recycleBin.loadMoreFailed"));
      } finally {
        setLoadingMore(false);
      }
    })();
  };

  const doRestore = useCallback(async () => {
    if (!confirmTarget) return;
    const target = confirmTarget;
    setRestoring(true);
    try {
      await ensureAuth();
      await restoreRecord(entity, target.id);
      setRecords((prev) => removeById(prev, target.id));
      setTotal((n) => Math.max(0, n - 1));
      toast.success(tf("page.recycleBin.restored", { label: recordLabel(target, fields) }));
      setConfirmTarget(null);
    } catch (e) {
      toast.error(restoreErrorMessage(e, (name) => fieldsByName[name]?.label ?? name));
      // 404 → no longer soft-deleted; drop it so the bin stays honest. 409 → keep it (user can fix).
      if (shouldDropOnError(e)) {
        setRecords((prev) => removeById(prev, target.id));
        setTotal((n) => Math.max(0, n - 1));
      }
      setConfirmTarget(null);
    } finally {
      setRestoring(false);
    }
  }, [confirmTarget, entity, fields, fieldsByName]);

  const dropRows = (ids: string[], count = ids.length) => {
    const gone = new Set(ids);
    setRecords((prev) => prev.filter((r) => !gone.has(r.id)));
    setTotal((n) => Math.max(0, n - count));
  };
  const onPurged = (rep: RecordPurgeReport) => {
    // The report also names children that sat in this same bin (any page of it): they are gone with the record.
    const sameTable = rep.records.filter((r) => r.entity === entity).map((r) => r.id);
    dropRows(sameTable, sameTable.length);
    const done = tf("studio.operate.recycle.purged", { label: purgeTarget ? recordLabel(purgeTarget, fields) : "" });
    // The server never goes silent about files it could not remove: say so verbatim, and keep it on screen.
    if (rep.blobsNote) toast.warning(`${done} ${rep.blobsNote}`, 20000);
    else toast.success(done);
    setPurgeTarget(null);
  };
  const entityLabelOf = useCallback((name: string) => allEntities.find((e) => e.name === name)?.label ?? name, [allEntities]);

  // The machine slug is a builder's word: the customer sees the object's label (the template's
  // label line); the slug stays available to an admin, who is the only person it helps.
  const meta = [isAdmin && !embedded && entity, loadState === "ok" && tf("page.recycleBin.count", { n: total })]
    .filter(Boolean)
    .join(" · ");
  const readOnly = loadState === "ok" && !canRestore && records.length > 0;
  const restoreInline = embedded && confirmTarget !== null;
  const hasNotices = loadState === "error" || readOnly || !!loadMoreErr || restoreInline || purgeTarget !== null;

  return (
    <div className="flex min-h-0 flex-1 flex-col p-4">
      <ObjectHomePage
        icon={{ glyph: Trash2, name: "record" }}
        label={entityLabel}
        title={t("page.recycleBin.title")}
        meta={meta || undefined}
        controls={
          // The shared help card speaks engine words (deletedAt, record, entity): the Studio gets its own sentence (S13).
          embedded
            ? [<FieldHelp key="help" text={t("studio.operate.recycle.help")} label={t("page.recycleBin.title")} />]
            : softDeleteHelp
              ? [<FieldHelp key="help" text={softDeleteHelp.short} label={softDeleteHelp.term} />]
              : undefined
        }
      >
        {hasNotices && (
          <div className="slds-p-around_small slds-border_top">
            {loadState === "error" && (
              <ScopedNotification theme="error">
                <strong>{t("page.recycleBin.loadError")}</strong> — {errMsg}
              </ScopedNotification>
            )}
            {/* Read-only note when the user lacks the delete grant */}
            {readOnly && <ScopedNotification icon={Lock}>{embedded ? t("studio.operate.recycle.readOnly") : t("page.recycleBin.readOnly")}</ScopedNotification>}
            {loadMoreErr && <ScopedNotification theme="error">{loadMoreErr}</ScopedNotification>}
            {/* In the Studio the questions are asked in the page, never in a dialog (specs/073 FR-065). */}
            {restoreInline && confirmTarget && (
              <InlineConfirm
                tone="info"
                message={tf("studio.operate.recycle.restoreMsg", { label: recordLabel(confirmTarget, fields) })}
                confirmLabel={t("page.recycleBin.restore")}
                onConfirm={() => void doRestore()}
                onCancel={() => setConfirmTarget(null)}
                busy={restoring}
              />
            )}
            {purgeTarget && (
              <RecordPurgeConfirm
                key={purgeTarget.id}
                entity={entity}
                id={purgeTarget.id}
                label={recordLabel(purgeTarget, fields)}
                entityLabel={entityLabelOf}
                assertScope={purge?.assertScope ?? (() => false)}
                onClose={() => setPurgeTarget(null)}
                onPurged={onPurged}
                onGone={() => dropRows([purgeTarget.id])}
                onDenied={() => setPurgeDenied(true)}
              />
            )}
          </div>
        )}

        {loadState !== "error" && (
          <>
            <div ref={gridBox}>
              <DataTable<ApiRecord>
                label={tf("page.recycleBin.grid", { label: entityLabel })}
                columns={tableColumns}
                data={loadState === "ok" ? records : []}
                loading={loadState === "loading"}
                getRowId={(r) => r.id}
                resizable
                columnSizing={colSizing}
                onColumnSizingChange={resizeColumns}
                maxHeight="max(24rem, calc(100vh - 19rem))"
                rowActions={
                  canRestore || showPurge
                    ? (r: ApiRecord) => (
                        <>
                          {canRestore && <MenuItem label={t("page.recycleBin.restore")} icon={RotateCcw} onSelect={() => { setPurgeTarget(null); setConfirmTarget(r); }} />}
                          {showPurge && <MenuItem label={t("studio.operate.recycle.purge")} icon={Flame} onSelect={() => { setConfirmTarget(null); setPurgeTarget(r); }} />}
                        </>
                      )
                    : undefined
                }
                empty={
                  loadState === "ok" ? (
                    <EmptyState icon={Inbox} title={t("page.recycleBin.empty")} description={t("page.recycleBin.emptyHint")} />
                  ) : undefined
                }
              />
            </div>

            {/* Footer — offset "load more" */}
            {loadState === "ok" && records.length > 0 && (
              <div className="slds-grid slds-grid_vertical-align-center slds-grid_align-spread slds-p-around_small slds-border_top">
                <span className="slds-text-color_weak">
                  {tf("page.list.loaded", { n: records.length, total })}
                </span>
                {hasMore && (
                  <Button onClick={loadMore} disabled={loadingMore} iconRight={loadingMore ? undefined : ChevronDown}>
                    {loadingMore ? t("page.list.loadingMore") : tf("page.list.loadMore", { n: PAGE_LIMIT })}
                  </Button>
                )}
              </div>
            )}
          </>
        )}
      </ObjectHomePage>

      {/* Restore confirm (prompt/base) — restore is reversible (delete again), so an info prompt. */}
      <Prompt
        open={confirmTarget !== null && !embedded}
        onOpenChange={(o) => !o && !restoring && setConfirmTarget(null)}
        // The row (and its action button) may be gone after a restore: land on the grid's tab stop.
        returnFocus={() => gridBox.current?.querySelector<HTMLElement>('[tabindex="0"]')}
        title={t("page.recycleBin.confirmTitle")}
        theme="info"
        footer={
          <>
            <Button onClick={() => setConfirmTarget(null)} disabled={restoring}>
              {t("action.cancel")}
            </Button>
            <Button variant="brand" onClick={() => void doRestore()} disabled={restoring}>
              {t("page.recycleBin.restore")}
            </Button>
          </>
        }
      >
        <p>
          {confirmTarget && tf("page.recycleBin.confirmBody", { label: recordLabel(confirmTarget, fields) })}
        </p>
      </Prompt>
    </div>
  );
}
