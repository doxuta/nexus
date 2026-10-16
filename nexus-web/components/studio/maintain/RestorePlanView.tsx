"use client";

// RestorePlanView — what a restore WOULD change, drawn from the server's answer to the dry run (specs/079
// f7-khoi-phuc-ke-hoach, US9). "Danh sách kiểm" pattern: two checks, the widened-access warning above every group,
// then one flat table per group under a 13px/600 head with a rule — no card per group. Pure view: no network, no
// button (the button that runs the restore is the next slice).
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import type { RestorePlan } from "@/lib/api";
import { formatBytes } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { accessChangesRows, restoreWarningText, tableLabel } from "@/lib/maintenance";

const GROUPS = ["data", "config", "access", "files"] as const;
const n = (v: number) => v.toLocaleString("vi-VN");

function Head({ children }: { children: string }) {
  return (
    <h3 className="slds-border_bottom slds-p-bottom_x-small" style={{ fontSize: "13px", fontWeight: 600 }}>
      {children}
    </h3>
  );
}

function Access({ plan }: { plan: RestorePlan }) {
  const a = plan.accessChanges;
  if (!a) return <ScopedNotification theme="warning">{t("studio.maintain.restore.widened.unknown")}</ScopedNotification>;
  if (a.total <= 0) return null;
  return (
    <ScopedNotification theme="warning">
      <strong>{t("studio.maintain.restore.widened.title")}</strong>
      <ul className="slds-list_dotted">
        {accessChangesRows(a).map((line) => <li key={line}>{line}</li>)}
      </ul>
      <p>{tf("studio.maintain.restore.widened.people", { users: n(a.usersAffected), keys: n(a.apiKeysAffected) })}</p>
      {a.outboundHosts.length > 0 && <p>{tf("studio.maintain.restore.widened.hosts", { hosts: a.outboundHosts.join(", ") })}</p>}
    </ScopedNotification>
  );
}

export function RestorePlanView({ plan }: { plan: RestorePlan }) {
  const est = plan.preRestoreEstimate;
  const warnings = (plan.warnings ?? []).map((w) => restoreWarningText(w.code, w.count)).filter((x): x is string => x !== null);
  return (
    <div className="flex flex-col gap-4">
      <p className="nx-lede">{t("studio.maintain.restore.lede")}</p>

      <ul className="slds-list_dotted">
        <li>{plan.integrity.ok ? t("studio.maintain.restore.intact") : t("studio.maintain.restore.corrupt")}</li>
        <li>
          {plan.schema.ok
            ? tf("studio.maintain.restore.schemaOk", { v: plan.schema.current })
            : tf("studio.maintain.restore.schemaBad", { backup: plan.schema.backup, current: plan.schema.current })}
        </li>
      </ul>

      <Access plan={plan} />

      {warnings.length > 0 && (
        <ul className="slds-list_dotted">
          {warnings.map((w) => <li key={w}>{w}</li>)}
        </ul>
      )}

      {GROUPS.map((g) => {
        const group = (plan.groups ?? []).find((x) => x.group === g);
        const label = group?.label || t(`studio.maintain.restore.group.${g}` as I18nKey);
        const tables = group?.tables ?? [];
        return (
          <section key={g} aria-label={label}>
            <Head>{label}</Head>
            {tables.length === 0 ? (
              <p className="slds-text-color_weak slds-m-top_x-small">{t("studio.maintain.restore.groupEmpty")}</p>
            ) : (
              <table className="slds-table slds-table_cell-buffer slds-no-row-hover">
                <thead>
                  <tr className="slds-line-height_reset">
                    <th scope="col">{t("studio.maintain.restore.colTable")}</th>
                    <th scope="col" className="slds-text-align_right">{t("studio.maintain.restore.colInBackup")}</th>
                    <th scope="col" className="slds-text-align_right">{t("studio.maintain.restore.colNow")}</th>
                  </tr>
                </thead>
                <tbody>
                  {tables.map((tb) => (
                    <tr key={tb.table}>
                      <th scope="row" className="slds-cell-wrap">{tableLabel(tb)}</th>
                      <td className="tabular-nums slds-text-align_right">{n(tb.rowsInBackup)}</td>
                      <td className="tabular-nums slds-text-align_right">{n(tb.rowsNow)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {g === "files" && (
              <p className="slds-m-top_x-small">
                {tf("studio.maintain.restore.blobs", { w: n(plan.blobs.toWrite), bytes: formatBytes(plan.blobs.bytesToWrite), r: n(plan.blobs.toRemove) })}
              </p>
            )}
          </section>
        );
      })}

      <section aria-label={t("studio.maintain.restore.kept")}>
        <Head>{t("studio.maintain.restore.kept")}</Head>
        <ul className="slds-list_dotted slds-m-top_x-small">
          {(plan.kept ?? []).map((k) => <li key={k}>{k}</li>)}
        </ul>
      </section>

      <p className="slds-text-color_weak">
        {tf("studio.maintain.restore.estimate", { rows: n(est.rows), files: n(est.files), bytes: formatBytes(est.bytes), free: formatBytes(est.freeBytes) })}
      </p>
    </div>
  );
}
