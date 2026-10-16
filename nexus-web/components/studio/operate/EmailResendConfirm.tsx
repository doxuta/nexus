"use client";

// "Gửi lại" for a failed email inside the Studio (specs/078 lát u7-email-gui-lai, FR-131): two phases in the page, no
// modal. Phase 1 asks the server for the PREVIEW ({dryRun:true}: masked recipient, rendered subject, whether the rule
// changed since the send); phase 2 sends, and only if the customer scope is still the one the admin looked at.
import { useEffect, useRef, useState } from "react";
import { resendEmail, type EmailLogEntry, type EmailResendPreview } from "@/lib/admin";
import { resendRefusal } from "@/lib/emailResend";
import { t, tf } from "@/lib/i18n";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Spinner } from "@/components/slds/Spinner";
import { InlineConfirm } from "@/components/studio/InlineConfirm";
import { useScopeGate } from "./useScopeGate";

export function EmailResendConfirm({ entry, onClose, onChanged }: {
  entry: EmailLogEntry;
  onClose: () => void;
  /** The list is stale (sent, or the server says the row changed): refetch it. */
  onChanged: () => void;
}) {
  const guard = useScopeGate(true);
  const [preview, setPreview] = useState<EmailResendPreview | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  useEffect(() => {
    let live = true;
    resendEmail(entry.id)
      .then((p) => { if (live) setPreview(p); })
      .catch((e) => {
        if (!live) return;
        const r = resendRefusal(e);
        setErr(r.text);
        if (r.reload) onChanged();
      });
    return () => { live = false; };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- one preview per opened row
  }, [entry.id]);

  const send = async () => {
    if (sending) return;
    // FR-131: the customer changed between the two phases ⇒ no second call at all.
    if (!guard.assertScope?.()) { onClose(); return; }
    setSending(true);
    setErr(null);
    try {
      await resendEmail(entry.id, false);
      if (!alive.current) return;
      onChanged();
      onClose();
    } catch (e) {
      if (!alive.current) return;
      const r = resendRefusal(e);
      setErr(r.text);
      if (r.reload) onChanged();
      setSending(false);
    }
  };

  if (err && !preview) {
    return (
      <ScopedNotification theme="error">
        {err} <Button variant="base" onClick={onClose}>{t("studio.confirm.cancel")}</Button>
      </ScopedNotification>
    );
  }
  if (!preview) {
    return <div className="slds-is-relative slds-p-around_medium"><Spinner size="small" label={t("page.emailLog.resend.loading")} /></div>;
  }
  return (
    <InlineConfirm
      tone="warning"
      message={tf("studio.operate.email.resend.message", { to: preview.recipientMasked, subject: preview.subject })}
      confirmLabel={t("page.emailLog.resend.confirm")}
      onConfirm={() => void send()}
      onCancel={onClose}
      busy={sending}
    >
      {preview.ruleChangedSinceSend && <ScopedNotification theme="warning">{t("studio.operate.email.resend.ruleChanged")}</ScopedNotification>}
      <p className="slds-m-top_x-small">{t("page.emailLog.resend.duplicate")}</p>
      {err && <ScopedNotification theme="error">{err}</ScopedNotification>}
    </InlineConfirm>
  );
}
