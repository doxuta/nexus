"use client";

// "Giao cho khách: phiên bản 1" (specs/077 f5-handoff-deliver). The one-way action of the handover page: the button, and
// an InlineConfirm right under it (never a dialog). It calls POST /admin/publish/baseline exactly once per confirm and
// nothing else — in particular no PATCH of the project stage. While rows still block, the button stays focusable
// (aria-disabled, not `disabled`) and a click sends focus to the first blocking row instead of doing nothing.
import { Send } from "lucide-react";
import { useId, useRef, useState } from "react";
import { Button } from "@/components/slds/Button";
import { Checkbox } from "@/components/slds/Checkbox";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { Textarea } from "@/components/slds/Textarea";
import { toast } from "@/components/ui/Toaster";
import { ApiError, publishBaseline } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t, tf } from "@/lib/i18n";
import { InlineConfirm } from "../InlineConfirm";

const NOTE_MAX = 500;

export interface DeliverConfirmProps {
  /** ids of the blocking rows, in page order */
  blocked: string[];
  /** sample rows that will be delivered with the app; > 0 asks for an explicit tick */
  keptSamples: number;
  /** a customer owner that can log in, when known */
  ownerEmail?: string;
  onFocusBlocked?: (rowId: string) => void;
  /** delivered (201) or found already delivered (409): the page re-reads the state and shows the delivered screen */
  onDelivered: () => void;
}

export function DeliverConfirm({ blocked, keptSamples, ownerEmail, onFocusBlocked, onDelivered }: DeliverConfirmProps) {
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState("");
  const [kept, setKept] = useState(false);
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false); // delivered: stay locked until the page swaps to the result screen
  const [error, setError] = useState<string | null>(null);
  const [noteError, setNoteError] = useState<string | null>(null);
  const inFlight = useRef(false); // state is stale between two fast clicks
  const opener = useRef<HTMLButtonElement>(null);
  const hintId = useId();
  const isBlocked = blocked.length > 0;
  const tooLong = note.length > NOTE_MAX;

  const close = () => {
    setOpen(false);
    setError(null);
    setNoteError(null);
    opener.current?.focus();
  };

  async function deliver() {
    if (inFlight.current || done || tooLong) return;
    inFlight.current = true;
    setBusy(true);
    setError(null);
    setNoteError(null);
    try {
      const res = await publishBaseline(note.trim() || undefined);
      toast.success(res.propagationSeconds ? tf("studio.build.handover.done.toast", { n: res.propagationSeconds }) : t("studio.build.handover.done.toastShort"));
      setDone(true);
      onDelivered();
    } catch (e) {
      if (e instanceof ApiError && e.code === "ALREADY_DELIVERED") {
        setDone(true); // someone else delivered: show that screen, no red
        onDelivered();
      } else if (e instanceof ApiError && e.code === "TENANT_MAINTENANCE") {
        const until = typeof e.details?.until === "string" ? new Date(e.details.until) : null;
        setError(until && !isNaN(until.getTime()) ? tf("studio.build.handover.err.maintenance", { time: until.toLocaleTimeString("vi-VN", { hour: "2-digit", minute: "2-digit" }) }) : errorMessage(e));
      }
      else if (e instanceof ApiError && e.status === 403) setError(t("studio.build.handover.err.forbidden"));
      else if (e instanceof ApiError && e.status === 422) setNoteError(errorMessage(e));
      else setError(errorMessage(e));
    } finally {
      inFlight.current = false;
      setBusy(false);
    }
  }

  return (
    <div className="slds-m-top_medium">
      <Button
        ref={opener}
        variant="brand"
        iconLeft={Send}
        aria-disabled={isBlocked ? "true" : undefined}
        aria-describedby={isBlocked ? hintId : undefined}
        onClick={() => (isBlocked ? onFocusBlocked?.(blocked[0]) : setOpen(true))}
      >
        {t("studio.build.handover.deliver")}
      </Button>
      {isBlocked && (
        <p id={hintId} className="slds-text-body_small slds-text-color_weak slds-m-top_x-small">
          {tf("studio.build.handover.deliver.blocked", { n: blocked.length })}
        </p>
      )}
      {open && !isBlocked && (
        <div className="slds-m-top_small">
          <InlineConfirm
            tone="warning"
            message={t("studio.build.handover.confirm.message")}
            confirmLabel={t("studio.build.handover.confirm.yes")}
            cancelLabel={t("studio.build.handover.confirm.no")}
            onConfirm={() => void deliver()}
            onCancel={close}
            busy={busy || done}
            confirmDisabled={tooLong || (keptSamples > 0 && !kept)}
          >
            {ownerEmail && <p className="slds-m-bottom_small">{tf("studio.build.handover.confirm.owner", { email: ownerEmail })}</p>}
            <Textarea
              label={t("studio.build.handover.confirm.noteLabel")}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              error={tooLong ? t("studio.build.handover.confirm.noteTooLong") : (noteError ?? undefined)}
            />
            {keptSamples > 0 && (
              <Checkbox
                className="slds-m-top_small"
                label={tf("studio.build.handover.confirm.kept", { k: keptSamples })}
                checked={kept}
                onChange={(e) => setKept(e.target.checked)}
              />
            )}
            {error && (
              <ScopedNotification theme="error" role="alert" className="slds-m-top_small">
                {error}
              </ScopedNotification>
            )}
          </InlineConfirm>
        </div>
      )}
    </div>
  );
}
