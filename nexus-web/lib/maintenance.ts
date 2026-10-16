// Pure words of the Bảo trì › Sao lưu page (specs/079 f7-sao-luu-bang). No React, no network.
import type { AccessChanges, BackupKind, BackupWarning, NotRestorableReason, RestorePlan, RestoreReport, RestoreWarningCode, TenantBackup } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";

export function notRestorableText(reason: NotRestorableReason): string {
  return t(`studio.maintain.backup.reason.${reason}` as I18nKey);
}

export const backupKindLabel = (kind: BackupKind): string => t(`backup.kind.${kind}` as I18nKey);

/** One line for the warnings of a backup ("" = none). Unknown codes keep the server's count, never the raw code alone. */
export function backupWarningText(ws: BackupWarning[]): string {
  return ws
    .map((w) => {
      const key = `backup.warn.${w.code}` as I18nKey;
      return t(key) === key ? tf("backup.warn.unknown", { n: w.count, code: w.code }) : tf(key, { n: w.count });
    })
    .join("; ");
}

export const backupSizeText = (b: Pick<TenantBackup, "sizeBytes">): string => formatBytes(b.sizeBytes);

/** "12.400 bản ghi · 320 tệp"; "" while the counts are not known (a running backup). */
export function backupDataText(b: Pick<TenantBackup, "rowCount" | "fileCount">): string {
  if (b.rowCount == null) return "";
  return tf("studio.maintain.backup.dataLine", { rows: b.rowCount.toLocaleString("vi-VN"), files: (b.fileCount ?? 0).toLocaleString("vi-VN") });
}

const pad = (n: number) => String(n).padStart(2, "0");

/** `<mã>-<yyyymmdd-hhmm>` of the backup's creation time (local), the zip name without extension (FR-103). */
export function backupFileStem(slug: string, createdAt: string): string {
  const d = new Date(createdAt);
  const stamp = isNaN(d.getTime()) ? "khong-ro" : `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}`;
  return `${slug.toLowerCase().replace(/[^a-z0-9-]/g, "-")}-${stamp}`;
}

/** null = "Khôi phục" may be pressed; else the sentence that says why not. Only `restorable === true` unlocks it. */
export function restoreLock(b: Pick<TenantBackup, "restorable" | "notRestorableReason">): string | null {
  if (b.restorable === true) return null;
  return b.notRestorableReason ? notRestorableText(b.notRestorableReason) : t("studio.maintain.backup.restoreUnknown");
}

const num = (v: number) => v.toLocaleString("vi-VN");

const WARNING_KEY: Partial<Record<RestoreWarningCode, I18nKey>> = {
  ROLE_LOST: "studio.maintain.restore.warn.ROLE_LOST",
  MEMBERSHIP_SKIPPED: "studio.maintain.restore.warn.MEMBERSHIP_SKIPPED",
  PENDING_JOBS_KEPT: "studio.maintain.restore.warn.PENDING_JOBS_KEPT",
  BLOB_MISSING_IN_BACKUP: "studio.maintain.restore.warn.BLOB_MISSING_IN_BACKUP",
  WEBHOOK_KEY_DROPPED: "studio.maintain.restore.warn.WEBHOOK_KEY_DROPPED",
};

/** One line for a restore warning; null = no line of its own (ACCESS_WIDENED lives in the access block; an unknown code is dropped, never shown raw). */
export function restoreWarningText(code: RestoreWarningCode, n: number): string | null {
  const key = WARNING_KEY[code];
  return key ? tf(key, { n: num(n) }) : null;
}

/** The kinds of widened access above zero, in the order of spec FR-035a. */
export function accessChangesRows(a: AccessChanges): string[] {
  const rows: [number, I18nKey][] = [
    [a.roleGrantsWidened, "studio.maintain.restore.widened.roleGrants"],
    [a.fieldAclReopened, "studio.maintain.restore.widened.fieldAcl"],
    [a.sharesRegranted, "studio.maintain.restore.widened.shares"],
    [a.membershipsReadded, "studio.maintain.restore.widened.memberships"],
    [a.sensitiveFieldsCleared, "studio.maintain.restore.widened.sensitive"],
    [a.workflowsReenabled, "studio.maintain.restore.widened.workflows"],
    [a.outboundDestinationsReenabled, "studio.maintain.restore.widened.outbound"],
  ];
  return rows.filter(([c]) => c > 0).map(([c, key]) => tf(key, { n: num(c) }));
}

type PlanTable = RestorePlan["groups"][number]["tables"][number];

/** The server's own label; a physical table name (`obj_…`, `sys_…`, `acl_…` — any snake_case token or the table itself) is never shown. */
export function tableLabel(tb: Pick<PlanTable, "table" | "label">): string {
  const l = (tb.label ?? "").trim();
  return l && l !== tb.table && !/^[a-z0-9]+(_[a-z0-9]+)+$/i.test(l) ? l : t("studio.maintain.restore.unnamedTable");
}

// ── Restore run (specs/079 f7-khoi-phuc-chay) ──
type RestoreRow = NonNullable<TenantBackup["restore"]>;
export const RESTORE_STEPS = ["lock", "prebackup", "apply", "doctor"] as const;

/** Index for ProgressIndicator from the step the SERVER wrote (never guessed from time). -1 = no restore block; done = all four finished. */
export function restoreStepIndex(r: Pick<RestoreRow, "status" | "step"> | null | undefined): number {
  if (!r) return -1;
  if (r.status === "done") return RESTORE_STEPS.length;
  return r.step ? RESTORE_STEPS.indexOf(r.step) : 0;
}

/** One sentence for a failed restore: the code names the cause, `committedAt` says whether the customer's data changed. */
export function restoreFailureText(r: Pick<RestoreRow, "step" | "committedAt">, code?: string | null): string {
  if (code === "TENANT_RESTORE_PREBACKUP_FAILED") return t("studio.maintain.restore.failure.prebackup");
  const step = t(`studio.maintain.restore.step.${r.step ?? "lock"}` as I18nKey).toLowerCase();
  const cause = code === "TENANT_RESTORE_CONFLICT" ? `${t("studio.maintain.restore.failure.conflict")} ` : "";
  return cause + tf(r.committedAt ? "studio.maintain.restore.failure.committed" : "studio.maintain.restore.failure.notCommitted", { step });
}

type ReportGroup = "data" | "config" | "access" | "files";
const ACCESS_TABLE = /^(acl_|team|sys_field_acl$|sys_record_shares$)/;
const DATA_SYS = new Set(["sys_record_notes", "sys_notifications", "sys_unique_index", "sys_sample_records", "sys_sequences"]);
// ponytail: mirrors nexus-platform/internal/tenantbackup/policy.go (obj_/rel_ + DATA_SYS = data, acl_/team* = access, sys_files = files,
// rest = config). Upgrade path: the server adds `group` per table to the report.
const groupOfTable = (tb: string): ReportGroup =>
  /^(obj|rel)_/.test(tb) || DATA_SYS.has(tb) ? "data" : tb === "sys_files" ? "files" : ACCESS_TABLE.test(tb) ? "access" : "config";

/** Rows deleted/inserted per group (data, config, access); groups with nothing are left out. No physical name survives. */
export function restoreReportGroups(r: Pick<RestoreReport, "deleted" | "inserted">): { group: ReportGroup; deleted: number; inserted: number }[] {
  const acc = { data: { deleted: 0, inserted: 0 }, config: { deleted: 0, inserted: 0 }, access: { deleted: 0, inserted: 0 }, files: { deleted: 0, inserted: 0 } };
  for (const [tb, c] of Object.entries(r.deleted ?? {})) acc[groupOfTable(tb)].deleted += c;
  for (const [tb, c] of Object.entries(r.inserted ?? {})) acc[groupOfTable(tb)].inserted += c;
  return (["data", "config", "access", "files"] as const).map((group) => ({ group, ...acc[group] })).filter((g) => g.deleted > 0 || g.inserted > 0);
}
