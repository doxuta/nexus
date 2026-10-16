"use client";

// SLDS docked panel (blueprint `panels/base`, SLDS 2.264.1). Markup copied from the blueprint;
// the sprite glyphs are swapped for Lucide (CC BY-ND sprite is never used). `.slds-panel_docked`
// is display:none until `.slds-is-open`, fills its parent's height and scrolls itself
// (`.slds-panel{overflow-y:auto}`) — the parent decides where it docks (a flex row).
// Focus (panels docs, Accessibility): opening moves focus to the first focusable that isn't the close
// button; closing returns it to the trigger. Only on open/close transitions — a panel that is open
// when the page loads does not take focus. Drill-in/back focus stays with the caller: it owns the
// content swap, so only it knows the drill-in trigger to return to.
import clsx from "clsx";
import { ChevronLeft, X } from "lucide-react";
import { useEffect, useRef, useSyncExternalStore, type KeyboardEvent, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { useEmbedded } from "./EmbeddedSurface";
import { FOCUSABLE, focusFirst } from "./focus";

// From 1280px a `responsive` panel stays docked in the layout; below it becomes a modal overlay.
const WIDE = "(min-width: 1280px)";
const subscribeWide = (cb: () => void) => {
  const mq = window.matchMedia(WIDE);
  mq.addEventListener("change", cb);
  return () => mq.removeEventListener("change", cb);
};
const isWide = () => window.matchMedia(WIDE).matches;

export interface PanelProps {
  open: boolean;
  title: string;
  /** Renders the collapse button in the header when given. */
  onClose?: () => void;
  /** Drilled-in state: renders the back button before the title. */
  onBack?: () => void;
  position?: "left" | "right";
  size?: "small" | "medium" | "large";
  /** `slds-panel__header_align-center` variant. */
  centeredTitle?: boolean;
  /** Secondary header actions (e.g. a <Menu>), placed before the collapse button. */
  actions?: ReactNode;
  /** Also finds the trigger to return focus to when the panel mounted open (`[aria-controls=id]`). */
  id?: string;
  /** Docked beside the content from 1280px; below that an overlay (backdrop, focus trap, Esc closes,
   *  needs `onClose`). Off by default: existing panels keep their docked place at every width. */
  responsive?: boolean;
  className?: string;
  children?: ReactNode;
}

export function Panel({
  open,
  title,
  onClose,
  onBack,
  position = "left",
  size = "medium",
  centeredTitle,
  actions,
  id,
  responsive,
  className,
  children,
}: PanelProps) {
  const wide = useSyncExternalStore(subscribeWide, isWide, () => true);
  const overlay = !!responsive && !wide && open;
  const flat = useEmbedded() && !overlay;
  const ref = useRef<HTMLDivElement>(null);
  const wasOpen = useRef(open);
  const returnTo = useRef<HTMLElement | null>(null);
  useEffect(() => {
    if (open === wasOpen.current) return;
    wasOpen.current = open;
    const panel = ref.current;
    if (!panel) return;
    const active = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (open) {
      returnTo.current = active === document.body ? null : active;
      focusFirst(panel, "slds-panel__close");
      return;
    }
    const trigger = returnTo.current ?? (id ? document.querySelector<HTMLElement>(`[aria-controls="${CSS.escape(id)}"]`) : null);
    returnTo.current = null;
    // Only when focus was inside the now-hidden panel (or already fell to <body>); closing from the
    // trigger or from elsewhere on the page leaves focus where the user put it.
    if (!active || active === document.body || panel.contains(active)) trigger?.focus();
  }, [open, id]);
  // Overlay can start without an open transition (mounted open while narrow, or a docked open panel
  // crossing below 1280px): pull focus in, and let Esc close from anywhere while the modal is up.
  useEffect(() => {
    if (!overlay) return;
    const panel = ref.current;
    if (panel && !panel.contains(document.activeElement)) {
      const active = document.activeElement instanceof HTMLElement && document.activeElement !== document.body ? document.activeElement : null;
      returnTo.current ??= active;
      focusFirst(panel, "slds-panel__close");
    }
    const onEsc = (e: globalThis.KeyboardEvent) => {
      if (e.key !== "Escape") return;
      e.stopPropagation();
      onClose?.();
    };
    document.addEventListener("keydown", onEsc);
    return () => document.removeEventListener("keydown", onEsc);
  }, [overlay, onClose]);
  const collapse = `${t("slds.panel.collapse")} ${title}`;
  const back = t("action.back");
  const closeButton = onClose && (
    <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-panel__close" title={collapse} onClick={onClose}>
      <X className="slds-button__icon nx-lucide" aria-hidden="true" />
      <span className="slds-assistive-text">{collapse}</span>
    </button>
  );
  // Overlay keys: Tab/Shift+Tab wrap inside the panel (Esc is the document listener above).
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!overlay) return;
    if (e.key !== "Tab") return;
    const items = Array.from(e.currentTarget.querySelectorAll<HTMLElement>(FOCUSABLE));
    if (items.length === 0) return;
    const first = items[0];
    const last = items[items.length - 1];
    const here = document.activeElement;
    if (e.shiftKey && (here === first || !e.currentTarget.contains(here))) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && (here === last || !e.currentTarget.contains(here))) {
      e.preventDefault();
      first.focus();
    }
  };
  const panel = (
    <div
      ref={ref}
      id={id}
      role={overlay ? "dialog" : undefined}
      aria-modal={overlay ? "true" : undefined}
      aria-label={overlay ? title : undefined}
      onKeyDown={overlay ? onKeyDown : undefined}
      className={clsx(
        // Embedded (specs/073 FR-005): a 300px column inside the work surface, ruled on all sides and
        // spaced from the edge, hidden while closed; the overlay (narrow screens) keeps the docked look.
        flat ? "slds-panel w-[300px] max-lg:w-full max-lg:ml-0 max-lg:mt-4 shrink-0 self-start slds-border_top slds-border_right slds-border_bottom slds-border_left slds-m-left_medium" : "slds-panel slds-panel_docked",
        flat && !open && "hidden",
        overlay && "fixed inset-y-0 z-[9001] max-w-full",
        overlay && (position === "right" ? "right-0" : "left-0"),
        !flat && `slds-size_${size}`,
        !flat && `slds-panel_docked-${position}`,
        open && "slds-is-open",
        className,
      )}
    >
      <div className={clsx("slds-panel__header", centeredTitle && "slds-panel__header_align-center")}>
        {onBack && (
          <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-panel__back" title={back} onClick={onBack}>
            <ChevronLeft className="slds-button__icon nx-lucide" aria-hidden="true" />
            <span className="slds-assistive-text">{back}</span>
          </button>
        )}
        <h2 className="slds-panel__header-title slds-text-heading_small slds-truncate" title={title}>
          {title}
        </h2>
        {(actions || closeButton) && (
          <div className="slds-panel__header-actions">
            {actions}
            {closeButton}
          </div>
        )}
      </div>
      <div className="slds-panel__body">{children}</div>
    </div>
  );
  if (!responsive) return panel;
  // Fragment shape is fixed while `responsive`, so crossing 1280px does not remount the panel.
  return (
    <>
      {overlay && <div className="slds-backdrop slds-backdrop_open" role="presentation" onClick={onClose} />}
      {panel}
    </>
  );
}
