"use client";

// RestoreReportView — what a finished restore did (specs/079 f7-khoi-phuc-chay US9.6). A checklist of the things
// that were skipped or dropped, a flat table of rows per group, files and the health check. Pure view.
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import type { RestoreReport } from "@/lib/api";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { accessChangesRows, restoreReportGroups } from "@/lib/maintenance";

const n = (v: number) => v.toLocaleString("vi-VN");

function took(ms: number): string | null {
  if (!Number.isFinite(ms) || ms < 0) return null;
  const s = Math.round(ms / 1000);
  return s < 60 ? tf("studio.maintain.restore.took.sec", { s }) : tf("studio.maintain.restore.took.min", { m: Math.floor(s / 60), s: s % 60 });
}

export function RestoreReportView({ report, durationMs }: { report: RestoreReport; durationMs?: number }) {
  const groups = restoreReportGroups(report);
  const skipped = (g: string) => (g === "access" ? report.membershipsSkipped + report.sharesSkipped : 0);
  const lines: string[] = [];
  if (report.rolesLost > 0) lines.push(tf("studio.maintain.restore.report.rolesLost", { n: n(report.rolesLost) }));
  if (report.membershipsSkipped > 0) lines.push(tf("studio.maintain.restore.report.membershipsSkipped", { n: n(report.membershipsSkipped) }));
  if (report.sharesSkipped > 0) lines.push(tf("studio.maintain.restore.report.sharesSkipped", { n: n(report.sharesSkipped) }));
  if (report.webhookKeysDropped > 0) lines.push(tf("studio.maintain.restore.report.webhookKeysDropped", { n: n(report.webhookKeysDropped) }));
  lines.push(tf("studio.maintain.restore.report.blobs", { w: n(report.blobsWritten), r: n(report.blobsRemoved) }));
  if (report.doctor) lines.push(tf("studio.maintain.restore.report.doctor", { fixed: n(report.doctor.fixed), n: n(report.doctor.remainingCritical) }));
  const dur = durationMs === undefined ? null : took(durationMs);
  if (dur) lines.push(tf("studio.maintain.restore.report.took", { time: dur }));
  const widened = report.accessChanges && report.accessChanges.total > 0 ? accessChangesRows(report.accessChanges) : [];
  return (
    <section className="flex flex-col gap-3" aria-label={t("studio.maintain.restore.report.title")}>
      <h3 className="slds-border_bottom slds-p-bottom_x-small" style={{ fontSize: "13px", fontWeight: 600 }}>{t("studio.maintain.restore.report.title")}</h3>
      {report.resumedAfterCrash && <ScopedNotification theme="info">{t("studio.maintain.restore.resumed")}</ScopedNotification>}
      {groups.length > 0 && (
        <table className="slds-table slds-table_cell-buffer slds-no-row-hover">
          <thead>
            <tr className="slds-line-height_reset">
              <th scope="col">{t("studio.maintain.restore.report.colGroup")}</th>
              <th scope="col" className="slds-text-align_right">{t("studio.maintain.restore.report.colDeleted")}</th>
              <th scope="col" className="slds-text-align_right">{t("studio.maintain.restore.report.colInserted")}</th>
              <th scope="col" className="slds-text-align_right">{t("studio.maintain.restore.report.colSkipped")}</th>
            </tr>
          </thead>
          <tbody>
            {groups.map((g) => (
              <tr key={g.group}>
                <th scope="row">{t(`studio.maintain.restore.group.${g.group}` as I18nKey)}</th>
                <td className="tabular-nums slds-text-align_right">{n(g.deleted)}</td>
                <td className="tabular-nums slds-text-align_right">{n(g.inserted)}</td>
                <td className="tabular-nums slds-text-align_right">{n(skipped(g.group))}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <ul className="slds-list_dotted">{lines.map((l) => <li key={l}>{l}</li>)}</ul>
      {widened.length > 0 && (
        <ScopedNotification theme="warning">
          <strong>{t("studio.maintain.restore.widened.title")}</strong>
        </ScopedNotification>
      )}
      {widened.length > 0 && (
        <ul className="slds-list_dotted">{widened.map((l) => <li key={l}>{l}</li>)}</ul>
      )}
    </section>
  );
}
