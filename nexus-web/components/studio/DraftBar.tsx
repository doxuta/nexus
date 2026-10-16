"use client";

// DraftBar — what is LEFT of the old draft → publish bar (specs/073 nhap1b, FR-114). Review and publish of a delivered project
// moved to ProjectHeader (status line + "Xuất bản" → PublishReview). Only the one-way "Xuất bản lần đầu" stays here, because it is
// the single place that calls POST /admin/publish/baseline until f5-handoff-deliver (Studio "Giao") lands; remove this file then.
// Delivered, not the builder (403) or backend off: renders nothing.
import { useState } from "react";
import { Rocket } from "lucide-react";
import { ApiError, publishBaseline } from "@/lib/api";
import { emitDraftsChanged } from "@/lib/draftEvents";
import { tf, t } from "@/lib/i18n";
import { toast } from "@/components/ui/Toaster";
import { Button } from "@/components/slds/Button";
import { Modal } from "@/components/slds/Modal";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { usePublishState } from "./usePublishState";

export function DraftBar({ slug }: { slug: string }) {
  const state = usePublishState(slug).data;
  const [firstOpen, setFirstOpen] = useState(false);
  if (!state || state.delivered) return null;
  return (
    <div role="group" aria-label={t("draftBar.region")} className="slds-grid slds-grid_vertical-align-center slds-m-right_small">
      <Button variant="neutral" iconLeft={Rocket} onClick={() => setFirstOpen(true)}>{t("draftBar.publishFirst")}</Button>
      {firstOpen && <FirstPublishModal onClose={() => setFirstOpen(false)} onDone={() => { setFirstOpen(false); emitDraftsChanged(); }} />}
    </div>
  );
}

function FirstPublishModal({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  async function confirm() {
    setBusy(true);
    setErr(null);
    try {
      const res = await publishBaseline();
      toast.success(tf("draftBar.firstDone", { n: res.docCount }));
      onDone();
    } catch (e) {
      setErr(e instanceof ApiError ? e.message : tf("draftBar.failed", { msg: String(e) }));
      setBusy(false);
    }
  }
  return (
    <Modal
      open
      onOpenChange={(o) => { if (!o) onClose(); }}
      title={t("draftBar.firstTitle")}
      size="small"
      footer={
        <>
          <Button onClick={onClose} disabled={busy}>{t("draftBar.cancel")}</Button>
          <Button variant="brand" onClick={confirm} disabled={busy}>{busy ? t("draftBar.submitting") : t("draftBar.firstConfirm")}</Button>
        </>
      }
    >
      <p>{t("draftBar.firstBody")}</p>
      {err && <ScopedNotification theme="error" className="slds-m-top_small">{err}</ScopedNotification>}
    </Modal>
  );
}
