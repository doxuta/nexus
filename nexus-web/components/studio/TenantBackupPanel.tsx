"use client";

// TenantBackupPanel — "Sao lưu & khôi phục" of ONE customer (specs/068-tenant-backup-restore lát A).
// The platform operator's tool: list the customer's backups, "Sao lưu ngay", watch a running one
// until "Sẵn sàng", and download the zip. It is a stand-alone component that takes the customer as a
// parameter (spec FR-064) so the Studio-mới "Bảo trì" area can mount it unchanged (task
// FE-tenant-backup-restore); today it opens in a Modal from each row of the Workspace-khách card.
// Restore / maintenance / schedule controls arrive with their own slices (B–E) in this same panel.
// SLDS only (components/slds/*); every string through lib/i18n (S13: "khách", "bản sao lưu").

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Download, HardDriveDownload, ShieldCheck, Trash2 } from "lucide-react";
import {
  ApiError, clearTenantMaintenance, createTenantBackup, deleteTenantBackup, downloadTenantBackup, getTenantBackup,
  getTenantBackupSchedule, listTenantBackups, putTenantBackupSchedule, setTenantMaintenance,
  type BackupKind, type TenantBackup, type TenantBackupSchedule, type TenantSummary,
} from "@/lib/api";
import { toast } from "@/components/ui/Toaster";
import { formatBytes, formatDateTime, formatTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Badge } from "@/components/slds/Badge";
import { Button } from "@/components/slds/Button";
import { DataTable } from "@/components/slds/DataTable";
import { EmptyState } from "@/components/slds/EmptyState";
import { Input } from "@/components/slds/Input";
import { Select } from "@/components/slds/Select";
import { Toggle } from "@/components/slds/Toggle";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { TenantRestoreDialog } from "@/components/studio/TenantRestoreDialog";
import { ConfirmPrompt } from "@/components/studio/workspaceShared";

/** The poll period while a backup runs (spec FR-017: the screen updates itself). */
const POLL_MS = 3000;

/** The freeze lengths the operator can pick (the server accepts any whole 1..120). */
const FREEZE_MINUTES = [5, 15, 30, 60, 120];

/** The "keep N scheduled backups" choices (the server accepts any whole 1..30). */
const KEEP_CHOICES = [1, 3, 5, 7, 14, 30];

export interface TenantBackupPanelProps {
  tenant: Pick<TenantSummary, "id" | "slug" | "name">;
  /** Called after the write freeze was switched, so the registry row can refresh its badge. */
  onMaintenanceChange?: () => void;
}

const pad = (n: number) => String(n).padStart(2, "0");

/** nexus-<slug>-<YYYYMMDD-HHMMSS>-<loại>.zip — the same name the server puts in Content-Disposition
 *  (the browser cannot read that header across origins, so it is rebuilt here from the same parts). */
export function backupFileName(slug: string, createdAt: string, kind: BackupKind): string {
  const d = new Date(createdAt);
  const stamp = isNaN(d.getTime()) ? "unknown"
    : `${d.getUTCFullYear()}${pad(d.getUTCMonth() + 1)}${pad(d.getUTCDate())}-${pad(d.getUTCHours())}${pad(d.getUTCMinutes())}${pad(d.getUTCSeconds())}`;
  const safe = (s: string) => s.toLowerCase().replace(/[^a-z0-9-]/g, "-");
  return `nexus-${safe(slug)}-${stamp}-${safe(kind)}.zip`;
}

const errText = (e: unknown, fallback: I18nKey) => (e instanceof ApiError ? e.message : t(fallback));

function warningText(w: { code: string; count: number }): string {
  const key = `backup.warn.${w.code}` as I18nKey;
  const text = t(key);
  return text === key ? tf("backup.warn.unknown", { n: w.count, code: w.code }) : tf(key, { n: w.count });
}

function StatusBadge({ b }: { b: TenantBackup }) {
  const theme = b.status === "ready" ? "success" : b.status === "failed" ? "error" : "warning";
  return <Badge theme={theme}>{t(`backup.status.${b.status}` as I18nKey)}</Badge>;
}

export function TenantBackupPanel({ tenant, onMaintenanceChange }: TenantBackupPanelProps) {
  const [items, setItems] = useState<TenantBackup[]>([]);
  const [cursor, setCursor] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [note, setNote] = useState("");
  const [creating, setCreating] = useState(false);
  const [downloading, setDownloading] = useState<string | null>(null);
  // Write freeze (specs/068 lát B): the deadline the server reported, null = not frozen. It lapses by
  // itself, so a timer clears it at the deadline instead of comparing Date.now() during render.
  const [frozenUntil, setFrozenUntil] = useState<string | null>(null);
  const [freezeMinutes, setFreezeMinutes] = useState(15);
  const [freezeBusy, setFreezeBusy] = useState(false);
  const [checking, setChecking] = useState<TenantBackup | null>(null);
  // Bumped when a restore ends: the list reloads so the new pre_restore row ("Hoàn tác") shows up.
  const [reloadKey, setReloadKey] = useState(0);
  const onRestoreFinished = useCallback(() => setReloadKey((k) => k + 1), []);
  // Automatic-backup schedule (specs/068 lát E): null until loaded; a failed load hides the controls and says so.
  const [schedule, setSchedule] = useState<TenantBackupSchedule | null>(null);
  const [scheduleError, setScheduleError] = useState(false);
  const [scheduleBusy, setScheduleBusy] = useState(false);
  const [deleting, setDeleting] = useState<TenantBackup | null>(null);
  const [deleteBusy, setDeleteBusy] = useState(false);

  // First page. A failed load is shown as the error it is — never as "no backups yet".
  useEffect(() => {
    let live = true;
    listTenantBackups(tenant.id, {})
      .then((r) => { if (live) { setItems(r.data); setCursor(r.meta.nextCursor); setLoadError(null); setFrozenUntil(r.meta.maintenanceUntil); } })
      .catch((e) => { if (live) setLoadError(errText(e, "backup.loadFailed")); })
      .finally(() => { if (live) setLoaded(true); });
    return () => { live = false; };
  }, [tenant.id, reloadKey]);

  useEffect(() => {
    let live = true;
    getTenantBackupSchedule(tenant.id)
      .then((s) => { if (live) { setSchedule(s); setScheduleError(false); } })
      .catch(() => { if (live) setScheduleError(true); });
    return () => { live = false; };
  }, [tenant.id]);

  // Poll ONLY the rows that are running (GET …/:backupId), every 3 s; the effect re-keys on the set
  // of running ids, so it stops by itself when the last one finishes.
  const pollFails = useRef(0);
  const runningKey = items.filter((b) => b.status === "running").map((b) => b.id).join(",");
  useEffect(() => {
    if (!runningKey) return;
    const ids = runningKey.split(",");
    pollFails.current = 0;
    const timer = setInterval(() => {
      void Promise.all(ids.map((id) => getTenantBackup(tenant.id, id).catch(() => null))).then((fresh) => {
        // Every poll failing 3 times in a row (backup purged = 404, access lost = 403, network) ⇒ stop
        // and say so instead of polling silently forever.
        pollFails.current = fresh.every((f) => !f) ? pollFails.current + 1 : 0;
        if (pollFails.current >= 3) {
          clearInterval(timer);
          toast.error(t("backup.pollLost"));
          return;
        }
        const done = fresh.filter((f): f is TenantBackup => !!f && f.status !== "running");
        if (done.some((f) => f.status === "ready")) toast.success(t("backup.finished"));
        if (done.some((f) => f.status === "failed")) toast.error(t("backup.failedNow"));
        setItems((prev) => prev.map((b) => fresh.find((f) => f?.id === b.id) ?? b));
      });
    }, POLL_MS);
    return () => clearInterval(timer);
  }, [runningKey, tenant.id]);

  useEffect(() => {
    if (!frozenUntil) return;
    const id = setTimeout(() => setFrozenUntil(null), Math.max(0, new Date(frozenUntil).getTime() - Date.now()));
    return () => clearTimeout(id);
  }, [frozenUntil]);

  const switchFreeze = async (on: boolean) => {
    setFreezeBusy(true);
    try {
      if (on) {
        const r = await setTenantMaintenance(tenant.id, freezeMinutes);
        setFrozenUntil(r.maintenanceUntil);
        toast.success(tf("maintenance.locked", { time: formatTime(r.maintenanceUntil) }));
      } else {
        await clearTenantMaintenance(tenant.id);
        setFrozenUntil(null);
        toast.success(t("maintenance.unlocked"));
      }
      onMaintenanceChange?.();
    } catch (e) {
      // 409 BACKUP_IN_PROGRESS (a restore holds the freeze), 422 MAINTENANCE_SELF / _MINUTES_INVALID carry their own sentence.
      toast.error(errText(e, "maintenance.failed"));
    } finally {
      setFreezeBusy(false);
    }
  };

  const saveSchedule = async (enabled: boolean, keep: number) => {
    setScheduleBusy(true);
    try {
      setSchedule(await putTenantBackupSchedule(tenant.id, enabled, keep));
      toast.success(t("backup.schedule.saved"));
    } catch (e) {
      toast.error(errText(e, "backup.schedule.failed"));
    } finally {
      setScheduleBusy(false);
    }
  };

  const confirmDelete = async () => {
    if (!deleting) return;
    setDeleteBusy(true);
    try {
      await deleteTenantBackup(tenant.id, deleting.id);
      setItems((prev) => prev.filter((b) => b.id !== deleting.id));
      setDeleting(null);
      toast.success(t("backup.deleted"));
    } catch (e) {
      // 409 BACKUP_DELETE_IN_USE carries its own sentence.
      toast.error(errText(e, "backup.deleteFailed"));
    } finally {
      setDeleteBusy(false);
    }
  };

  const create = async () => {
    setCreating(true);
    try {
      const b = await createTenantBackup(tenant.id, note);
      setItems((prev) => [b, ...prev.filter((x) => x.id !== b.id)]);
      setNote("");
      toast.success(t("backup.started"));
    } catch (e) {
      // 409 BACKUP_IN_PROGRESS / BACKUP_DISK_LOW and 422 BACKUP_NOTE_INVALID carry their own sentence.
      toast.error(errText(e, "backup.createFailed"));
    } finally {
      setCreating(false);
    }
  };

  const loadMore = async () => {
    if (!cursor) return;
    setLoadingMore(true);
    try {
      const r = await listTenantBackups(tenant.id, { after: cursor });
      setItems((prev) => [...prev, ...r.data.filter((x) => !prev.some((p) => p.id === x.id))]);
      setCursor(r.meta.nextCursor);
    } catch (e) {
      toast.error(errText(e, "backup.loadFailed"));
    } finally {
      setLoadingMore(false);
    }
  };

  const download = useCallback(async (b: TenantBackup) => {
    setDownloading(b.id);
    try {
      await downloadTenantBackup(tenant.id, b.id, backupFileName(tenant.slug, b.createdAt, b.kind));
    } catch (e) {
      toast.error(errText(e, "backup.downloadFailed"));
    } finally {
      setDownloading(null);
    }
  }, [tenant.id, tenant.slug]);

  const columns = useMemo<ColumnDef<TenantBackup>[]>(() => [
    {
      id: "time",
      header: t("backup.col.time"),
      cell: ({ row: { original: b } }) => (
        <div>
          <div className="tabular-nums">{formatDateTime(b.createdAt)}</div>
          {b.createdBy && <div className="slds-text-body_small slds-text-color_weak">{tf("backup.by", { name: b.createdBy.label })}</div>}
          {b.note && <div className="slds-text-body_small slds-text-color_weak slds-cell-wrap">{b.note}</div>}
        </div>
      ),
    },
    { id: "kind", header: t("backup.col.kind"), cell: ({ row: { original: b } }) => t(`backup.kind.${b.kind}` as I18nKey) },
    { id: "size", header: t("backup.col.size"), meta: { numeric: true }, cell: ({ row: { original: b } }) => formatBytes(b.sizeBytes) },
    {
      id: "data",
      header: t("backup.col.data"),
      cell: ({ row: { original: b } }) => b.rowCount == null ? "—"
        : tf("backup.data", { rows: b.rowCount.toLocaleString("vi-VN"), files: (b.fileCount ?? 0).toLocaleString("vi-VN") }),
    },
    {
      id: "status",
      header: t("backup.col.status"),
      cell: ({ row: { original: b } }) => (
        <div>
          <StatusBadge b={b} />
          {b.error && <div className="slds-text-body_small slds-text-color_error slds-cell-wrap">{b.error.message}</div>}
        </div>
      ),
    },
    {
      id: "warnings",
      header: t("backup.col.warnings"),
      cell: ({ row: { original: b } }) => b.warnings.length === 0 ? "—" : (
        <ul className="slds-cell-wrap">
          {b.warnings.map((w) => <li key={w.code}>{warningText(w)}</li>)}
        </ul>
      ),
    },
    {
      id: "actions",
      header: t("backup.col.actions"),
      cell: ({ row: { original: b } }) => (
        <div className="flex gap-2">
          <Button
            iconLeft={Download}
            disabled={b.status !== "ready" || downloading === b.id}
            aria-label={tf("backup.downloadFor", { time: formatDateTime(b.createdAt) })}
            title={b.status === "ready" ? t("backup.downloadTitle") : t("backup.downloadNotReady")}
            onClick={() => void download(b)}
          >
            {downloading === b.id ? t("backup.downloading") : t("backup.download")}
          </Button>
          <Button
            iconLeft={ShieldCheck}
            disabled={!b.restorable}
            aria-label={tf(b.kind === "pre_restore" ? "backup.restore.undoFor" : "backup.restore.checkFor", { time: formatDateTime(b.createdAt) })}
            title={b.restorable || !b.notRestorableReason
              ? t(b.kind === "pre_restore" ? "backup.restore.undoTitle" : "backup.restore.checkTitle")
              : t(`backup.restore.reason.${b.notRestorableReason}` as I18nKey)}
            onClick={() => setChecking(b)}
          >
            {b.kind === "pre_restore" ? t("backup.restore.undo") : t("backup.restore.check")}
          </Button>
          {b.status !== "running" && b.restore?.status !== "running" && (
            <Button
              iconLeft={Trash2}
              aria-label={tf("backup.deleteFor", { time: formatDateTime(b.createdAt) })}
              onClick={() => setDeleting(b)}
            >
              {t("backup.delete")}
            </Button>
          )}
        </div>
      ),
    },
  ], [download, downloading]);

  return (
    <div className="flex flex-col gap-3">
      <p className="slds-text-color_weak">{t("backup.intro")}</p>
      <ScopedNotification theme="warning">{t("backup.privacy")}</ScopedNotification>

      <div className="flex items-end gap-3">
        <Toggle
          label={t("maintenance.toggle")}
          help={t("maintenance.toggleHelp")}
          checked={frozenUntil !== null}
          disabled={freezeBusy}
          onChange={(e) => void switchFreeze(e.target.checked)}
        />
        <Select
          label={t("maintenance.minutes")}
          placeholder={false}
          value={String(freezeMinutes)}
          disabled={freezeBusy || frozenUntil !== null}
          onChange={(e) => setFreezeMinutes(Number(e.target.value))}
          options={FREEZE_MINUTES.map((n) => ({ value: String(n), label: tf("maintenance.minutesOption", { n }) }))}
        />
        {frozenUntil && (
          <>
            <Badge theme="warning">{tf("maintenance.badge", { time: formatTime(frozenUntil) })}</Badge>
            <Button disabled={freezeBusy} onClick={() => void switchFreeze(false)}>{t("maintenance.unlock")}</Button>
          </>
        )}
      </div>

      {schedule && (
        <div className="flex flex-col gap-1">
          <div className="flex items-end gap-3">
            <Toggle
              label={tf("backup.schedule.toggle", { hour: String(schedule.hourLocal).padStart(2, "0") })}
              help={t("backup.schedule.toggleHelp")}
              checked={schedule.enabled}
              disabled={scheduleBusy}
              onChange={(e) => void saveSchedule(e.target.checked, schedule.keepScheduled)}
            />
            <Select
              label={t("backup.schedule.keep")}
              placeholder={false}
              value={String(schedule.keepScheduled)}
              disabled={scheduleBusy}
              onChange={(e) => void saveSchedule(schedule.enabled, Number(e.target.value))}
              options={(KEEP_CHOICES.includes(schedule.keepScheduled) ? KEEP_CHOICES : [...KEEP_CHOICES, schedule.keepScheduled].sort((a, b) => a - b))
                .map((n) => ({ value: String(n), label: tf("backup.schedule.keepOption", { n }) }))}
            />
          </div>
          <p className="slds-text-body_small slds-text-color_weak">
            {schedule.lastScheduledAt ? tf("backup.schedule.last", { time: formatDateTime(schedule.lastScheduledAt) }) : t("backup.schedule.never")}
            {schedule.enabled && schedule.nextDueAt && ` · ${tf("backup.schedule.next", { time: formatDateTime(schedule.nextDueAt) })}`}
          </p>
        </div>
      )}
      {scheduleError && <ScopedNotification theme="error">{t("backup.schedule.loadFailed")}</ScopedNotification>}

      <div className="flex items-end gap-3">
        <div className="grow">
          <Input
            label={t("backup.noteLabel")}
            placeholder={t("backup.notePh")}
            maxLength={200}
            value={note}
            onChange={(e) => setNote(e.target.value)}
            disabled={creating}
          />
        </div>
        <Button variant="brand" iconLeft={HardDriveDownload} disabled={creating} onClick={() => void create()}>
          {creating ? t("backup.creating") : t("backup.create")}
        </Button>
      </div>

      {!loaded ? (
        <div className="flex items-center gap-2"><Spinner size="x-small" inline /></div>
      ) : loadError ? (
        <ScopedNotification theme="error">{loadError}</ScopedNotification>
      ) : items.length === 0 ? (
        <EmptyState title={t("backup.empty.title")} description={t("backup.empty.desc")} />
      ) : (
        <>
          <DataTable label={t("backup.table")} columns={columns} data={items} getRowId={(b) => b.id} maxHeight="24rem" bordered />
          {cursor && (
            <div className="slds-text-align_center">
              <Button onClick={() => void loadMore()} disabled={loadingMore}>{t("backup.loadMore")}</Button>
            </div>
          )}
        </>
      )}
      {checking && (
        <TenantRestoreDialog
          tenant={tenant}
          backup={checking}
          mode={checking.kind === "pre_restore" ? "undo" : "restore"}
          onClose={() => setChecking(null)}
          onFinished={onRestoreFinished}
        />
      )}
      <ConfirmPrompt
        open={deleting !== null}
        onOpenChange={(o) => { if (!o) setDeleting(null); }}
        title={t("backup.deleteTitle")}
        description={deleting ? tf("backup.deleteDesc", { time: formatDateTime(deleting.createdAt) }) : undefined}
        danger
        confirmLabel={t("backup.delete")}
        loading={deleteBusy}
        onConfirm={() => void confirmDelete()}
      />
    </div>
  );
}
