// Focus helpers shared by the SLDS overlays (Popover, Panel, Menu).

/** Keyboard-focusable elements. DOM order only — positive tabindex is not honoured. */
export const FOCUSABLE =
  "a[href], button:not([disabled]), input:not([disabled]):not([type='hidden']), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

/**
 * SLDS overlay rule (popovers, panels): focus the first focusable element that isn't the close
 * button; the close button when it is the only one.
 */
export function focusFirst(container: HTMLElement | null, closeClass: string) {
  const all = Array.from(container?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? []);
  (all.find((el) => !el.classList.contains(closeClass)) ?? all[0])?.focus({ preventScroll: true });
}

/**
 * Focus the first focusable element after `el` (and outside it) in document order, staying inside an
 * open modal dialog when `el` is in one. Returns false when there is none.
 */
export function focusNextAfter(el: Element): boolean {
  const scope = el.closest("[aria-modal='true']") ?? el.ownerDocument;
  const next = Array.from(scope.querySelectorAll<HTMLElement>(FOCUSABLE)).find(
    (c) =>
      !el.contains(c) &&
      el.compareDocumentPosition(c) & Node.DOCUMENT_POSITION_FOLLOWING &&
      !c.hasAttribute("data-radix-focus-guard") &&
      !c.closest("[data-radix-menu-content], [hidden], [inert]") &&
      c.checkVisibility?.() !== false,
  );
  next?.focus();
  return next != null;
}
