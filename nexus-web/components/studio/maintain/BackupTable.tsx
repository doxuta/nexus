"use client";

// BackupTable — "Bản sao lưu" of one tenant (specs/079 f7-sao-luu-bang FR-100): FOUR columns. The kind sits under the
// time, the warnings and a failure's sentence under the status, "{n} bản ghi · {m} tệp" under the size, and the
// actions close the row. SLDS data-table markup (`slds-table_cell-buffer`, stacked under 768px).
import { Download, ShieldCheck, Trash2 } from "lucide-react";
import { Fragment, useState } from "react";
import { Button } from "@/components/slds/Button";
import type { TenantBackup } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { backupDataText, backupKindLabel, backupSizeText, backupWarningText, restoreLock } from "@/lib/maintenance";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { InlineConfirm } from "../InlineConfirm";
import { StatusText, type StatusTone } from "../StatusText";

const TONE: Record<TenantBackup["status"], StatusTone> = { ready: "success", running: "weak", failed: "error" };

export interface BackupTableProps {
  rows: TenantBackup[];
  downloadingId: string | null;
  onDownload: (b: TenantBackup) => void;
  onRestore: (b: TenantBackup) => void;
  /** Called after the in-page confirmation; the page runs the DELETE and reports the outcome. */
  onDelete: (b: TenantBackup) => void;
}

export function BackupTable({ rows, downloadingId, onDownload, onRestore, onDelete }: BackupTableProps) {
  const [asking, setAsking] = useState<string | null>(null);
  // the backup a pre_restore row was taken before; unknown (older page or deleted) -> the plain kind label
  const source = (b: TenantBackup) => rows.find((x) => x.id === b.restore?.sourceBackupId);
  return (
    <table className="slds-table slds-table_cell-buffer slds-table_bordered slds-max-medium-table_stacked-horizontal" aria-label={t("studio.maintain.backup.title")}>
      <thead>
        <tr className="slds-line-height_reset">
          <th scope="col">{t("studio.maintain.backup.col.time")}</th>
          <th scope="col" className="text-right">{t("studio.maintain.backup.col.content")}</th>
          <th scope="col">{t("studio.maintain.backup.col.status")}</th>
          <th scope="col"><span className="slds-assistive-text">{t("studio.maintain.backup.col.actions")}</span></th>
        </tr>
      </thead>
      <tbody>
        {rows.map((b) => {
          const lock = restoreLock(b);
          const undo = b.kind === "pre_restore";
          const warn = backupWarningText(b.warnings);
          const when = formatDateTime(b.createdAt);
          return (
            <Fragment key={b.id}>
            <tr className="slds-hint-parent" data-backup-id={b.id}>
              <th scope="row" data-label={t("studio.maintain.backup.col.time")}>
                <div className="tabular-nums">{when}</div>
                <div className="slds-text-body_small slds-text-color_weak">
                  {undo && source(b) ? tf("studio.maintain.restore.preRestoreNote", { time: formatDateTime(source(b)!.createdAt) }) : backupKindLabel(b.kind)}
                  {b.createdBy ? ` · ${tf("backup.by", { name: b.createdBy.label })}` : ""}
                </div>
                {b.note && <div className="slds-text-body_small slds-text-color_weak slds-cell-wrap">{b.note}</div>}
              </th>
              <td data-label={t("studio.maintain.backup.col.content")} className="text-right tabular-nums">
                <div>{backupSizeText(b)}</div>
                <div className="slds-text-body_small slds-text-color_weak">{backupDataText(b)}</div>
              </td>
              <td data-label={t("studio.maintain.backup.col.status")}>
                <StatusText tone={TONE[b.status]}>{t(`backup.status.${b.status}` as I18nKey)}</StatusText>
                {b.error && <div className="slds-text-body_small slds-text-color_error slds-cell-wrap">{b.error.message}</div>}
                {warn && <div className="slds-text-body_small slds-text-color_weak slds-cell-wrap">{warn}</div>}
              </td>
              <td data-label={t("studio.maintain.backup.col.actions")}>
                <div className="flex flex-wrap justify-end gap-2">
                  <Button
                    variant="neutral"
                    iconLeft={Download}
                    disabled={b.status !== "ready" || downloadingId === b.id}
                    aria-label={tf("backup.downloadFor", { time: when })}
                    onClick={() => onDownload(b)}
                  >
                    {downloadingId === b.id ? t("backup.downloading") : t("studio.maintain.backup.download")}
                  </Button>
                  <Button
                    variant="neutral"
                    iconLeft={ShieldCheck}
                    disabled={lock !== null}
                    title={lock ?? undefined}
                    aria-label={tf(undo ? "backup.restore.undoFor" : "backup.restore.checkFor", { time: when })}
                    onClick={() => onRestore(b)}
                  >
                    {undo ? t("backup.restore.undo") : t("studio.maintain.backup.restore")}
                  </Button>
                  <Button
                    variant="neutral"
                    iconLeft={Trash2}
                    disabled={b.status === "running"}
                    aria-label={tf("studio.maintain.backup.deleteFor", { time: when })}
                    onClick={() => setAsking(b.id)}
                  >
                    {t("studio.maintain.backup.delete")}
                  </Button>
                </div>
                {lock && <div className="slds-text-body_small slds-text-color_weak slds-cell-wrap text-right">{lock}</div>}
              </td>
            </tr>
            {asking === b.id && (
              <tr>
                <td colSpan={4}>
                  <InlineConfirm
                    tone="error"
                    message={tf("studio.maintain.backup.deleteAsk", { time: when })}
                    confirmLabel={t("studio.maintain.backup.deleteConfirm")}
                    onCancel={() => setAsking(null)}
                    onConfirm={() => { setAsking(null); onDelete(b); }}
                  />
                </td>
              </tr>
            )}
            </Fragment>
          );
        })}
      </tbody>
    </table>
  );
}
