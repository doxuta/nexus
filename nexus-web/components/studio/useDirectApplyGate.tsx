"use client";

// useDirectApplyGate (specs/074 lát f2-draft-labels-rules T349): ONE helper for the kinds of change that have no draft
// ("untraced": approval process, work queue, inbound gate, working calendar). On a delivered project a save reaches real
// users at once and leaves no history, so the builder shows the label and asks in place before saving. The class comes
// from classOf over GET /admin/publish/state — no table of types here.
import { useState, type ReactNode } from "react";
import { classOf } from "@/lib/draftClass";
import { t } from "@/lib/i18n";
import { InlineConfirm } from "./InlineConfirm";
import { StatusText } from "./StatusText";
import { usePublishState } from "./usePublishState";

export interface DirectApplyGate {
  /** Run `save` now (not delivered / draftable) or after the in-place confirmation. */
  ask: (save: () => void) => void;
  /** The warning label; null while it does not apply. */
  notice: ReactNode;
  /** The in-place question; null unless a save is waiting. */
  confirm: ReactNode;
}

export interface DirectApplyText { message: string; confirm: string }
const AUTOMATION_TEXT: DirectApplyText = {
  message: t("studio.build.automation.draft.applyNow.message"),
  confirm: t("studio.build.automation.draft.applyNow.confirm"),
};

/** `text` overrides the question (specs/075 L5a: "Áp ngay cho người dùng thật: {mô tả}"). A traced `direct` type asks too, with its own label. */
export function useDirectApplyGate(slug: string, docType: string, text: DirectApplyText = AUTOMATION_TEXT): DirectApplyGate {
  const state = usePublishState(slug).data;
  const [pending, setPending] = useState<(() => void) | null>(null);
  const cls = classOf(docType, state);
  const applies = cls === "untraced" || cls === "direct";
  return {
    ask: (save) => (applies ? setPending(() => save) : save()),
    notice: applies ? <StatusText tone="warn">{t(cls === "direct" ? "studio.status.liveDirect" : "studio.status.liveDirectNoHistory")}</StatusText> : null,
    confirm: pending ? (
      <InlineConfirm
        tone="warning"
        message={text.message}
        confirmLabel={text.confirm}
        onConfirm={() => { const go = pending; setPending(null); go(); }}
        onCancel={() => setPending(null)}
      />
    ) : null,
  };
}
