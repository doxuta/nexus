"use client";

// Confirmation IN the page (specs/073 FR-065): a scoped notification with the question, optional extra content
// (e.g. a "type the code again" field, `children`), and a safe/dangerous button pair. Never window.confirm, never a modal.
// The safe button takes focus first and Esc cancels. `children` is part of the contract from day one so later features
// (assistant cards, sample-data wipe) do not add props of their own.
import { useEffect, useId, useRef, type KeyboardEvent, type ReactNode } from "react";
import { Button } from "@/components/slds/Button";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import { t } from "@/lib/i18n";

export interface InlineConfirmProps {
  /** error = destructive (red confirm button); warning / info = brand confirm button. */
  tone: "error" | "warning" | "info";
  /** Must name what is affected ("Xoá dự án Acme?"). */
  message: string;
  confirmLabel: string;
  /** Label of the safe button; defaults to "Huỷ". */
  cancelLabel?: string;
  onConfirm: () => void;
  onCancel: () => void;
  busy?: boolean;
  /** Keeps the dangerous button off until the extra content (e.g. the typed code) is right. */
  confirmDisabled?: boolean;
  children?: ReactNode;
}

export function InlineConfirm({ tone, message, confirmLabel, cancelLabel, onConfirm, onCancel, busy, confirmDisabled, children }: InlineConfirmProps) {
  const safe = useRef<HTMLButtonElement>(null);
  const box = useRef<HTMLDivElement>(null);
  const messageId = useId();
  useEffect(() => {
    safe.current?.focus();
  }, []);
  // The buttons disable while busy and would drop focus (and Esc with it): park focus on the box until it ends.
  useEffect(() => {
    if (busy) box.current?.focus();
    else if (box.current?.contains(document.activeElement) || document.activeElement === document.body) safe.current?.focus();
  }, [busy]);
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape" && !busy) {
      e.stopPropagation();
      onCancel();
    }
  };
  return (
    <div ref={box} tabIndex={-1} onKeyDown={onKeyDown}>
      <div id={messageId}>
        <ScopedNotification theme={tone}>{message}</ScopedNotification>
      </div>
      {children != null && (
        <div data-confirm-extra className="slds-m-top_small">
          {children}
        </div>
      )}
      <div className="slds-m-top_small slds-grid slds-grid_align-end">
        <Button ref={safe} aria-describedby={messageId} variant="neutral" onClick={onCancel} disabled={busy}>
          {cancelLabel ?? t("studio.confirm.cancel")}
        </Button>
        <Button aria-describedby={messageId} variant={tone === "error" ? "destructive" : "brand"} className="slds-m-left_x-small" onClick={onConfirm} disabled={busy || confirmDisabled}>
          {confirmLabel}
        </Button>
      </div>
    </div>
  );
}
