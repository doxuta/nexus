"use client";

// SLDS toast (blueprint `toast/base`, SLDS 2.264.1) + a Toaster that stacks them in
// `.slds-notify_container` (fixed, top centre). Glyphs are Lucide in the blueprint's utility
// container — the Icon wrapper is not used here because the blueprint svg carries no
// `slds-icon-text-*` tone: the glyph takes the theme's text colour.
//
// The API mirrors components/ui/Toaster.tsx (toast.success(msg), toast.error(msg), …,
// toast.withAction(msg, action)) so that file can become a one-line re-export of this one.
import clsx from "clsx";
import { CircleCheck, CircleX, Info, TriangleAlert, X, type LucideIcon } from "lucide-react";
import { useCallback, useEffect, useState } from "react";
import { t } from "@/lib/i18n";

export type ToastVariant = "info" | "success" | "warning" | "error";

export interface ToastAction {
  label: string;
  onClick: () => void;
}

export interface ToastProps {
  variant?: ToastVariant;
  /** Main line (`h2.slds-text-heading_small`). */
  title: string;
  /** Optional detail paragraph under the title. */
  message?: string;
  /** Inline link after the title (blueprint "Select Leads to Merge"); clicking it also closes. */
  action?: ToastAction;
  onClose: () => void;
}

export const GLYPH: Record<ToastVariant, LucideIcon> = {
  info: Info,
  success: CircleCheck,
  warning: TriangleAlert,
  error: CircleX,
};

export const VARIANT_LABEL = {
  info: "slds.toast.info",
  success: "slds.toast.success",
  warning: "slds.toast.warning",
  error: "slds.toast.error",
} as const;

export function Toast({ variant = "info", title, message, action, onClose }: ToastProps) {
  const Glyph = GLYPH[variant];
  const close = t("action.close");
  return (
    <div className={clsx("slds-notify slds-notify_toast pointer-events-auto", `slds-theme_${variant}`)} role="status">
      <span className="slds-assistive-text">{t(VARIANT_LABEL[variant])}</span>
      <span className={clsx("slds-icon_container slds-m-right_small slds-no-flex slds-align-top", `slds-icon-utility-${variant}`)}>
        <Glyph className="slds-icon slds-icon_small nx-lucide" aria-hidden="true" />
      </span>
      <div className="slds-notify__content">
        <h2 className="slds-text-heading_small">
          {title}
          {action && (
            <>
              {" "}
              <a
                href="#"
                onClick={(e) => {
                  e.preventDefault();
                  action.onClick();
                  onClose();
                }}
              >
                {action.label}
              </a>
            </>
          )}
        </h2>
        {message && <p>{message}</p>}
      </div>
      <div className="slds-notify__close">
        <button type="button" className="slds-button slds-button_icon slds-button_icon-inverse" title={close} onClick={onClose}>
          <X className="slds-button__icon nx-lucide" aria-hidden="true" />
          <span className="slds-assistive-text">{close}</span>
        </button>
      </div>
    </div>
  );
}

export interface ToastOptions extends Omit<ToastProps, "onClose"> {
  /** ms before auto-hide; 0 = stays until closed. Default (kept from the old ui/Toaster):
   *  success/info 4s, warning 5s, error 6s, 8s with an action. */
  duration?: number;
}

interface QueuedToast extends ToastOptions {
  id: number;
}

// Module-level sink so toast() works outside React; the mounted <Toaster /> owns it.
let push: ((opts: ToastOptions) => void) | null = null;
const emit = (opts: ToastOptions) => push?.(opts);

export const toast = Object.assign(emit, {
  success: (message: string, duration?: number) => emit({ variant: "success", title: message, duration }),
  info: (message: string, duration?: number) => emit({ variant: "info", title: message, duration }),
  warning: (message: string, duration?: number) => emit({ variant: "warning", title: message, duration }),
  error: (message: string, duration?: number) => emit({ variant: "error", title: message, duration }),
  /** Success toast with an inline action (e.g. undo) — clicking it runs the action and closes. */
  withAction: (message: string, action: ToastAction, duration?: number) =>
    emit({ variant: "success", title: message, action, duration }),
});

const DEFAULT_MS: Record<ToastVariant, number> = { info: 4000, success: 4000, warning: 5000, error: 6000 };

function defaultDuration({ variant = "info", action }: ToastOptions): number {
  return action ? 8000 : DEFAULT_MS[variant];
}

// Auto-hide waits while the pointer is over the toast or focus is inside it (WCAG 2.2.1): a keyboard
// user who tabs to "Hoàn tác" must not lose the action, nor their focus to <body>.
function TimedToast({ item, onDismiss }: { item: QueuedToast; onDismiss: (id: number) => void }) {
  const { id, duration, ...props } = item;
  const ms = duration ?? defaultDuration(item);
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const paused = hovered || focused;
  useEffect(() => {
    if (!ms || paused) return;
    // ponytail: resuming restarts the full duration instead of the remaining time — more generous, no clock.
    const timer = setTimeout(() => onDismiss(id), ms);
    return () => clearTimeout(timer);
  }, [id, ms, paused, onDismiss]);
  return (
    <div
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
      onFocus={() => setFocused(true)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget)) setFocused(false);
      }}
    >
      <Toast {...props} onClose={() => onDismiss(id)} />
    </div>
  );
}

let seq = 0;

export function Toaster() {
  const [items, setItems] = useState<QueuedToast[]>([]);

  useEffect(() => {
    // ponytail: newest 5 kept — sticky errors from a failing loop must not bury the page.
    push = (opts) => setItems((prev) => [...prev.slice(-4), { ...opts, id: ++seq }]);
    return () => {
      push = null;
    };
  }, []);

  const dismiss = useCallback((id: number) => setItems((prev) => prev.filter((x) => x.id !== id)), []);

  // The container spans the viewport width; only the toasts themselves take clicks.
  return (
    <div className="slds-notify_container pointer-events-none" aria-live="polite">
      {items.map((item) => (
        <TimedToast key={item.id} item={item} onDismiss={dismiss} />
      ))}
    </div>
  );
}
