"use client";

// SchemaRetry — "Xoá chưa xong · Thử lại" for a published version whose deferred field / table deletes did not all run
// (specs/079 f7-lich-su-hoan-tac-thu-lai, specs/063 S7-9). Failed ⇒ red status text + a button that opens a confirm IN the
// page naming what is left to delete; only the confirm sends POST …/retry-schema. Pending ⇒ "Đang xoá…". Anything else ⇒ nothing.
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { ApiError, retrySchemaOps } from "@/lib/api";
import { maintainErrorText } from "@/lib/maintainErrors";
import { t, tf } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { InlineConfirm } from "../InlineConfirm";
import { StatusText } from "../StatusText";
import { usePublishVersion } from "./queries";

export interface SchemaRetryProps {
  project: string;
  versionId: string;
  /** The version's `schemaStatus`. */
  status: string;
  /** Button label: "Thử lại" on the timeline row, "Thử lại phần sửa cấu trúc bảng" on the version page. */
  label: string;
  onDone: () => void;
}

function Confirm({ project, versionId, onCancel, onDone }: Omit<SchemaRetryProps, "status" | "label"> & { onCancel: () => void }) {
  const qc = useQueryClient();
  const q = usePublishVersion(project, versionId);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const deletes = (q.data?.docs ?? []).filter((d) => d.op === "delete" && (d.docType === "field" || d.docType === "entity") && d.applyStatus !== "applied");
  const what = deletes.map((d) => d.label).join(", ");

  async function run() {
    setBusy(true);
    setErr(null);
    try {
      const res = await retrySchemaOps(versionId);
      if (res.schemaStatus !== "done") {
        setErr(t("studio.publish.retryStill"));
        setBusy(false);
        void qc.invalidateQueries(); // the server may have deleted part of it: refresh the list of what is left
        return;
      }
      toast.success(t("studio.publish.retryDone"));
    } catch (e) {
      // 409 = another tab finished it first: same end state, nothing to report.
      if (!(e instanceof ApiError && e.code === "SCHEMA_RETRY_NOT_NEEDED")) {
        setErr(maintainErrorText(e));
        setBusy(false);
        return;
      }
    }
    void qc.invalidateQueries();
    onDone();
  }

  return (
    <InlineConfirm tone="warning" message={what ? tf("studio.maintain.history.retryConfirm", { what }) : t("studio.maintain.history.retryConfirmGeneric")}
      confirmLabel={t("studio.maintain.history.retrySubmit")} onConfirm={() => void run()} onCancel={onCancel} busy={busy}>
      {err ? <ScopedNotification theme="error" role="alert">{err}</ScopedNotification> : undefined}
    </InlineConfirm>
  );
}

export function SchemaRetry({ project, versionId, status, label, onDone }: SchemaRetryProps) {
  const [open, setOpen] = useState(false);
  const btn = useRef<HTMLButtonElement>(null);
  const wasOpen = useRef(false);
  // Keyboard users: give focus back to the button once the confirm closes (cancel / Esc / done).
  useEffect(() => {
    if (wasOpen.current && !open) btn.current?.focus();
    wasOpen.current = open;
  }, [open]);
  if (status === "pending") return <StatusText tone="warn">{t("studio.maintain.history.retryPending")}</StatusText>;
  if (status !== "failed") return null;
  return (
    <div>
      <p>
        <StatusText tone="error">{t("studio.maintain.history.retryFailed")}</StatusText>{" "}
        <Button ref={btn} variant="neutral" hidden={open} onClick={() => setOpen(true)}>{label}</Button>
      </p>
      {open && <div className="slds-m-top_x-small"><Confirm project={project} versionId={versionId} onCancel={() => setOpen(false)} onDone={() => { setOpen(false); onDone(); }} /></div>}
    </div>
  );
}
