"use client";

// RevertConfirm — "Hoàn tác" of ONE applied-at-once history entry, confirmed IN the page under its row, never in a modal
// (specs/079 f7-lich-su-hoan-tac-thu-lai, specs/063 S7-7b). It reads the entry first so the builder sees what the undo
// touches: for a deleted role, WHO would get the role back (name, status, whether they now hold another role) and who made
// the original change, because giving a role back is giving access back - possibly to someone the customer cut off on
// purpose. Nothing is sent until the confirm button.
import { useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { ApiError, revertDirectEntry } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { maintainErrorText } from "@/lib/maintainErrors";
import { t, tf } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { toast } from "@/components/ui/Toaster";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { InlineConfirm } from "../InlineConfirm";
import { usePublishVersion } from "./queries";

const STATUS: Record<string, I18nKey> = {
  active: "studio.maintain.revert.status.active", inactive: "studio.maintain.revert.status.inactive", locked: "studio.maintain.revert.status.locked",
};

export interface RevertConfirmProps {
  /** Project code: the entry is read through useProjectQuery so a token of another project never fetches it. */
  project: string;
  versionId: string;
  onCancel: () => void;
  /** Called after the undo committed. */
  onDone: () => void;
}

export function RevertConfirm({ project, versionId, onCancel, onDone }: RevertConfirmProps) {
  const qc = useQueryClient();
  const q = usePublishVersion(project, versionId);
  const detail = q.data;
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const what = detail?.docs.map((d) => d.label).join(", ") ?? "";

  async function submit() {
    setBusy(true);
    setErr(null);
    try {
      const res = await revertDirectEntry(versionId);
      toast.success(res.reassigned > 0 ? tf("studio.maintain.revert.doneReassigned", { what, n: res.reassigned }) : tf("studio.maintain.revert.done", { what }));
      void qc.invalidateQueries();
      onDone();
    } catch (e) {
      // The server's own text names the app/table here; the fixed key would say "quay lại" in an undo flow.
      setErr(e instanceof ApiError && e.code === "ROLLBACK_NAME_TAKEN" ? errorMessage(e) : maintainErrorText(e));
      // Stale / already in a version: the row on screen is out of date, so reload the history behind the message.
      if (e instanceof ApiError && (e.code === "REVERT_STALE" || e.code === "REVERT_USE_ROLLBACK")) void qc.invalidateQueries();
      setBusy(false);
    }
  }

  return (
    <InlineConfirm tone="warning" message={detail ? tf("studio.maintain.revert.intro", { what }) : q.isError ? t("studio.maintain.revert.loadFailed") : t("studio.maintain.revert.loading")}
      confirmLabel={busy ? t("studio.maintain.revert.submitting") : t("studio.maintain.revert.submit")}
      onConfirm={() => void submit()} onCancel={onCancel} busy={busy} confirmDisabled={!detail}>
      <div>
                {q.isPending && <div className="slds-p-around_small"><Spinner size="x-small" inline label={t("studio.maintain.revert.loading")} /></div>}
        {detail && (
          <>
            <p className="slds-text-color_weak slds-m-bottom_small">
              {tf("studio.maintain.revert.origin", { who: detail.createdByLabel || detail.createdByName, at: new Date(detail.createdAt).toLocaleString("vi-VN") })}
            </p>
            {detail.docs.filter((d) => d.docType === "role" && d.op === "delete").map((d) => (
              <div key={d.docKey} className="slds-m-bottom_small">
                <ScopedNotification theme="warning">
                  <p className="slds-text-title">{tf("studio.maintain.revert.reassignHeading", { role: d.label })}</p>
                  {(d.reassign ?? []).length === 0 ? <p>{t("studio.maintain.revert.reassignNone")}</p> : (
                    <ul className="slds-list_dotted">
                      {(d.reassign ?? []).map((u) => (
                        <li key={u.userId}>
                          {u.label} · {STATUS[u.status] ? t(STATUS[u.status]) : u.status}
                          {u.currentRoleId !== "" && <span className="slds-text-color_weak"> · {t("studio.maintain.revert.reassignKeep")}</span>}
                        </li>
                      ))}
                    </ul>
                  )}
                  <p>{t("studio.maintain.revert.reassignNote")}</p>
                </ScopedNotification>
              </div>
            ))}
            {detail.docs.some((d) => d.docType === "role" && d.op === "create") && (
              <ScopedNotification theme="warning" className="slds-m-bottom_small">{t("studio.maintain.revert.createRoleWarn")}</ScopedNotification>
            )}
            {detail.docs.some((d) => d.docType === "field_acl") && (
              <ScopedNotification theme="warning" className="slds-m-bottom_small">{t("studio.maintain.revert.faclWarn")}</ScopedNotification>
            )}
          </>
        )}
        {err && <ScopedNotification theme="error" role="alert" className="slds-m-top_small">{err}</ScopedNotification>}
      </div>
    </InlineConfirm>
  );
}
