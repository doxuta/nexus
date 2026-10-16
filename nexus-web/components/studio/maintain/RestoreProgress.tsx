"use client";

// RestoreProgress — the four steps of a running restore (specs/079 f7-khoi-phuc-chay FR-122). Vertical
// ProgressIndicator, the ONLY place that uses it in the maintenance zone: no card per step. The step comes from
// `restore.step`, written by the server; time never decides it.
import { ProgressIndicator } from "@/components/slds/ProgressIndicator";
import type { TenantBackup } from "@/lib/api";
import { formatDateTime } from "@/lib/format";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { RESTORE_STEPS, restoreStepIndex } from "@/lib/maintenance";

export function RestoreProgress({ restore }: { restore: NonNullable<TenantBackup["restore"]> }) {
  return (
    <div className="flex flex-col gap-2">
      <ProgressIndicator
        vertical
        label={t("studio.maintain.restore.progress")}
        current={restoreStepIndex(restore)}
        error={restore.status === "failed"}
        steps={RESTORE_STEPS.map((s) => ({
          id: s,
          label: t(`studio.maintain.restore.step.${s}` as I18nKey),
          description: t(`studio.maintain.restore.stepDesc.${s}` as I18nKey),
        }))}
      />
      {restore.committedAt && (
        <p className="slds-text-body_small slds-text-color_weak">{tf("studio.maintain.restore.committedAt", { time: formatDateTime(restore.committedAt) })}</p>
      )}
    </div>
  );
}
