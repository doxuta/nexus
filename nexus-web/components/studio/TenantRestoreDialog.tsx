"use client";

// TenantRestoreDialog — "Kiểm tra khôi phục" + "Khôi phục về bản này" of ONE backup
// (specs/068-tenant-backup-restore lát C + D). It first asks the server for the restore plan and
// shows what would change — is the file intact, rows per table, who loses a role, which access
// would be widened again, what stays. The restore button opens a type-the-customer-name prompt;
// after the 202 the dialog follows the server's own `restore.step` (4 steps, polled every 2 s) and
// ends with the report. A pre_restore row opens the same dialog as "Hoàn tác". A stand-alone component that takes the
// customer and the backup as parameters so the Studio-mới "Bảo trì" area can mount it unchanged
// (FE-tenant-backup-restore). SLDS only; every string through lib/i18n (S13: "khách", "bản sao lưu").

import { useEffect, useState } from "react";
import {
  ApiError, getTenantBackup, previewTenantRestore, startTenantRestore,
  type AccessChanges, type RestorePlan, type RestoreReport, type TenantBackup, type TenantSummary,
} from "@/lib/api";
import { formatBytes, formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { Button } from "@/components/slds/Button";
import { Modal } from "@/components/slds/Modal";
import { ProgressIndicator } from "@/components/slds/ProgressIndicator";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { ConfirmPrompt, type ImpactRow } from "@/components/studio/workspaceShared";

export interface TenantRestoreDialogProps {
  tenant: Pick<TenantSummary, "id" | "slug" | "name">;
  backup: { id: string; createdAt: string };
  /** "undo" = the backup is a pre_restore row: same flow, worded as "Hoàn tác". */
  mode?: "restore" | "undo";
  onClose: () => void;
  /** Called once a started restore ends (done or failed), so the list behind can reload. */
  onFinished?: () => void;
}

const POLL_MS = 2000;
const STEPS = ["lock", "prebackup", "apply", "doctor"] as const;

const n = (v: number) => v.toLocaleString("vi-VN");

/** The kinds of widened access above zero, in the order of spec FR-035a. */
function widenedLines(a: AccessChanges): string[] {
  const rows: [number, I18nKey][] = [
    [a.roleGrantsWidened, "backup.restore.widened.roleGrants"],
    [a.fieldAclReopened, "backup.restore.widened.fieldAcl"],
    [a.sharesRegranted, "backup.restore.widened.shares"],
    [a.membershipsReadded, "backup.restore.widened.memberships"],
    [a.sensitiveFieldsCleared, "backup.restore.widened.sensitive"],
    [a.workflowsReenabled, "backup.restore.widened.workflows"],
    [a.outboundDestinationsReenabled, "backup.restore.widened.outbound"],
  ];
  return rows.filter(([c]) => c > 0).map(([c, key]) => tf(key, { n: n(c) }));
}

function PlanView({ plan }: { plan: RestorePlan }) {
  const widened = widenedLines(plan.accessChanges);
  const warnings = plan.warnings.filter((w) => w.code !== "ACCESS_WIDENED");
  const est = plan.preRestoreEstimate;
  return (
    <div className="flex flex-col gap-3">
      <ScopedNotification theme="success">
        {tf("backup.restore.intact", { v: plan.schema.current })} {t("backup.restore.readOnly")}
      </ScopedNotification>

      {plan.accessChanges.total > 0 && (
        <ScopedNotification theme="warning">
          <strong>{t("backup.restore.widened.title")}</strong>
          <ul className="slds-list_dotted">
            {widened.map((line) => <li key={line}>{line}</li>)}
          </ul>
          <p>{tf("backup.restore.widened.people", { users: n(plan.accessChanges.usersAffected), keys: n(plan.accessChanges.apiKeysAffected) })}</p>
          {plan.accessChanges.outboundHosts.length > 0 && (
            <p>{tf("backup.restore.widened.hosts", { hosts: plan.accessChanges.outboundHosts.join(", ") })}</p>
          )}
        </ScopedNotification>
      )}

      {warnings.length > 0 && (
        <ul className="slds-list_dotted">
          {warnings.map((w) => <li key={w.code}>{tf(`backup.restore.warn.${w.code}` as I18nKey, { n: n(w.count) })}</li>)}
        </ul>
      )}

      {plan.groups.map((g) => (
        <section key={g.group}>
          <h2 className="slds-text-heading_small slds-m-bottom_x-small">{g.label}</h2>
          <table className="slds-table slds-table_bordered slds-table_cell-buffer">
            <thead>
              <tr className="slds-line-height_reset">
                <th scope="col">{t("backup.restore.colTable")}</th>
                <th scope="col">{t("backup.restore.colRows")}</th>
              </tr>
            </thead>
            <tbody>
              {g.tables.map((tb) => (
                <tr key={tb.table}>
                  <th scope="row" className="slds-cell-wrap">{tb.label}</th>
                  <td className="tabular-nums">{tf("backup.restore.counts", { backup: n(tb.rowsInBackup), now: n(tb.rowsNow) })}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      ))}

      <p>{tf("backup.restore.blobs", { w: n(plan.blobs.toWrite), bytes: formatBytes(plan.blobs.bytesToWrite), r: n(plan.blobs.toRemove) })}</p>

      <section>
        <h2 className="slds-text-heading_small slds-m-bottom_x-small">{t("backup.restore.kept")}</h2>
        <ul className="slds-list_dotted">
          {plan.kept.map((k) => <li key={k}>{k}</li>)}
        </ul>
      </section>

      <p className="slds-text-color_weak">
        {tf("backup.restore.estimate", { rows: n(est.rows), files: n(est.files), bytes: formatBytes(est.bytes), free: formatBytes(est.freeBytes) })}
      </p>
    </div>
  );
}

function ReportView({ report }: { report: RestoreReport }) {
  const rows = Object.values(report.inserted ?? {}).reduce((a, b) => a + b, 0);
  const lines: string[] = [tf("backup.restore.report.rows", { n: n(rows) })];
  if (report.rolesLost > 0) lines.push(tf("backup.restore.report.rolesLost", { n: n(report.rolesLost) }));
  if (report.membershipsSkipped > 0) lines.push(tf("backup.restore.report.membershipsSkipped", { n: n(report.membershipsSkipped) }));
  if (report.sharesSkipped > 0) lines.push(tf("backup.restore.report.sharesSkipped", { n: n(report.sharesSkipped) }));
  if (report.webhookKeysDropped > 0) lines.push(tf("backup.restore.report.webhookKeysDropped", { n: n(report.webhookKeysDropped) }));
  lines.push(tf("backup.restore.report.blobs", { w: n(report.blobsWritten), r: n(report.blobsRemoved) }));
  if (report.doctor) lines.push(tf("backup.restore.report.doctor", { fixed: n(report.doctor.fixed), n: n(report.doctor.remainingCritical) }));
  if (report.resumedAfterCrash) lines.push(t("backup.restore.report.resumed"));
  const widened = report.accessChanges && report.accessChanges.total > 0 ? widenedLines(report.accessChanges) : [];
  return (
    <section>
      <h2 className="slds-text-heading_small slds-m-bottom_x-small">{t("backup.restore.report.title")}</h2>
      <ul className="slds-list_dotted">{lines.map((l) => <li key={l}>{l}</li>)}</ul>
      {widened.length > 0 && (
        <ScopedNotification theme="warning">
          <strong>{t("backup.restore.widened.title")}</strong>
          <ul className="slds-list_dotted">{widened.map((l) => <li key={l}>{l}</li>)}</ul>
        </ScopedNotification>
      )}
    </section>
  );
}

/** The 4-step screen: steps come from the server's `restore.step`, never inferred from `status`. */
function RunView({ row }: { row: TenantBackup | null }) {
  const r = row?.restore ?? null;
  const status = r?.status ?? "running";
  const idx = status === "done" ? STEPS.length : Math.max(0, STEPS.indexOf(r?.step ?? "lock"));
  return (
    <div className="flex flex-col gap-3">
      <ProgressIndicator
        label={t("backup.restore.progress")}
        steps={STEPS.map((s) => ({ id: s, label: t(`backup.restore.step.${s}` as I18nKey) }))}
        current={idx}
        vertical
        error={status === "failed"}
      />
      {status === "running" && <ScopedNotification theme="info">{t("backup.restore.running")}</ScopedNotification>}
      {status === "failed" && (
        <ScopedNotification theme="error">{row?.error?.message ?? t("backup.restore.runFailed")}</ScopedNotification>
      )}
      {status === "done" && <ScopedNotification theme="success">{t("backup.restore.done")}</ScopedNotification>}
      {status === "done" && r?.report && <ReportView report={r.report} />}
    </div>
  );
}

export function TenantRestoreDialog({ tenant, backup, mode = "restore", onClose, onFinished }: TenantRestoreDialogProps) {
  const [plan, setPlan] = useState<RestorePlan | null>(null);
  const [error, setError] = useState<ApiError | Error | null>(null);
  const [confirming, setConfirming] = useState(false);
  const [starting, setStarting] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const [restoreId, setRestoreId] = useState<string | null>(null);
  const [row, setRow] = useState<TenantBackup | null>(null);
  const [pollLost, setPollLost] = useState(false);

  // Follow the restore on the server: its own step + status, every 2 s, until it ends.
  useEffect(() => {
    if (!restoreId) return;
    let live = true;
    const timer = setInterval(() => {
      void (async () => {
        try {
          const b = await getTenantBackup(tenant.id, restoreId);
          if (!live) return;
          setRow(b);
          if (b.restore && b.restore.status !== "running") {
            clearInterval(timer);
            onFinished?.();
          }
        } catch {
          if (live) { clearInterval(timer); setPollLost(true); }
        }
      })();
    }, POLL_MS);
    return () => { live = false; clearInterval(timer); };
  }, [restoreId, tenant.id, onFinished]);

  const start = async () => {
    setStarting(true);
    setStartError(null);
    try {
      const r = await startTenantRestore(tenant.id, backup.id, tenant.slug);
      setConfirming(false);
      setRestoreId(r.restoreId);
    } catch (e) {
      setConfirming(false);
      setStartError(e instanceof ApiError ? e.message : t("backup.restore.failed"));
    } finally {
      setStarting(false);
    }
  };

  const impact: ImpactRow[] = [
    { label: tf("backup.restore.impact.history", { time: formatDateTime(backup.createdAt) }) },
    { label: t("backup.restore.impact.undo") },
  ];
  if (plan && plan.accessChanges.total > 0) {
    impact.push({ label: tf("backup.restore.impact.widened", { n: n(plan.accessChanges.total) }), tone: "warn" });
  }

  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const p = await previewTenantRestore(tenant.id, backup.id);
        if (live) setPlan(p);
      } catch (e) {
        if (live) setError(e instanceof Error ? e : new Error(String(e)));
      }
    })();
    return () => { live = false; };
  }, [tenant.id, backup.id]);

  const body = restoreId ? (
    <>
      <RunView row={row} />
      {pollLost && <ScopedNotification theme="warning">{t("backup.restore.pollLost")}</ScopedNotification>}
    </>
  ) : error ? (
    <ScopedNotification theme="error">
      {error instanceof ApiError && error.code === "TENANT_BACKUP_CORRUPT"
        ? t("backup.restore.corrupt")
        : error instanceof ApiError ? error.message : t("backup.restore.failed")}
    </ScopedNotification>
  ) : plan ? (
    <>
      {startError && <ScopedNotification theme="error">{startError}</ScopedNotification>}
      <PlanView plan={plan} />
    </>
  ) : (
    <div className="flex items-center gap-2"><Spinner size="x-small" inline /> {t("backup.restore.loading")}</div>
  );

  const applyLabel = mode === "undo" ? t("backup.restore.undo") : t("backup.restore.apply");
  return (
    <>
      <Modal
        open
        onOpenChange={(o) => { if (!o) onClose(); }}
        size="large"
        title={tf("backup.restore.title", { time: formatDateTime(backup.createdAt) })}
        footer={
          <>
            {!restoreId && plan && (
              <Button variant="destructive" disabled={starting} title={t("backup.restore.applyTitle")} onClick={() => setConfirming(true)}>
                {applyLabel}
              </Button>
            )}
            <Button onClick={onClose}>{t("backup.close")}</Button>
          </>
        }
      >
        {body}
      </Modal>
      <ConfirmPrompt
        open={confirming}
        onOpenChange={setConfirming}
        title={tf("backup.restore.confirmTitle", { name: tenant.name, time: formatDateTime(backup.createdAt) })}
        description={t("backup.restore.confirmDesc")}
        impact={impact}
        confirmName={tenant.slug}
        danger
        confirmLabel={t("backup.restore.confirmLabel")}
        loading={starting}
        onConfirm={() => void start()}
      />
    </>
  );
}
