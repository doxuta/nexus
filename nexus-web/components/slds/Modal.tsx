"use client";

// SLDS modal (blueprint `modals/base`) and prompt (blueprint `prompt/base`), SLDS 2.264.1.
// Behaviour is @radix-ui/react-dialog: focus trap, Esc, focus return, scroll lock, aria-hiding the
// page. Radix parts are rendered `asChild` so the DOM is the blueprint's own markup:
//   section.slds-modal.slds-fade-in-open > div.slds-modal__container > close · header · content · footer
//   + div.slds-backdrop.slds-backdrop_open
// Lightning modals close only from the close button, Esc, or a footer action — never from a click
// elsewhere. `.slds-modal` covers the viewport so the dimmed area is the dialog itself, but a toast
// (own React tree, z-index above the modal) still counts as "outside" for Radix, so outside
// interactions are cancelled explicitly: closing an error toast must not throw away a half-filled form.
// Focus return (SLDS modals docs: Esc "moves focus to whatever triggered the modal to open"):
// Radix only returns focus to its own Trigger, so a controlled dialog opened from an onClick
// would drop focus on <body>. useFocusReturn remembers the element focused when the dialog opened
// and goes back to it; if the flow removed it, `returnFocus` names the replacement.
import * as Dialog from "@radix-ui/react-dialog";
import clsx from "clsx";
import { X } from "lucide-react";
import { useRef, type ReactElement, type ReactNode } from "react";
import { t } from "@/lib/i18n";

interface ShellProps {
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  /** Element that opens the dialog (rendered as-is via Radix Trigger asChild). */
  trigger?: ReactElement;
  /**
   * No `trigger`: focus target on close when the element that opened the dialog is gone
   * (e.g. the flow replaced that button). Without it focus falls back to <body>.
   */
  returnFocus?: () => HTMLElement | null | undefined;
}

const stayOpen = (e: Event) => e.preventDefault();

function useFocusReturn(trigger: ReactElement | undefined, returnFocus: ShellProps["returnFocus"]) {
  const opener = useRef<HTMLElement | null>(null);
  return {
    // FocusScope fires this before moving focus in, so activeElement is still the opener.
    rememberOpener: () => {
      const active = document.activeElement;
      // <body> means the opener was already gone (a menu item unmounts before the dialog mounts):
      // it is always "connected", so keeping it would shadow returnFocus.
      opener.current = active instanceof HTMLElement && active !== document.body ? active : null;
    },
    onCloseAutoFocus: (e: Event) => {
      if (trigger) return; // Radix focuses its Trigger
      const target = opener.current?.isConnected ? opener.current : returnFocus?.();
      opener.current = null;
      if (!target) return;
      e.preventDefault();
      target.focus();
    },
  };
}

function Shell({ open, defaultOpen, onOpenChange, trigger, children }: ShellProps & { children: ReactNode }) {
  return (
    <Dialog.Root open={open} defaultOpen={defaultOpen} onOpenChange={onOpenChange}>
      {trigger && <Dialog.Trigger asChild>{trigger}</Dialog.Trigger>}
      <Dialog.Portal>
        {children}
        <Dialog.Overlay asChild>
          <div className="slds-backdrop slds-backdrop_open" role="presentation" />
        </Dialog.Overlay>
      </Dialog.Portal>
    </Dialog.Root>
  );
}

export interface ModalProps extends ShellProps {
  /** Header title. Omit for the headless variant — then `label` names the dialog. */
  title?: ReactNode;
  /** Accessible name for a headless modal (rendered as assistive text). */
  label?: string;
  /** Tagline under the title (may contain links). */
  tagline?: ReactNode;
  size?: "small" | "medium" | "large" | "full";
  /** Footer buttons; omit for the footless variant. */
  footer?: ReactNode;
  /** `slds-modal__footer_directional`: first button left, the rest right (wizards). */
  directionalFooter?: boolean;
  className?: string;
  children?: ReactNode;
}

export function Modal({
  title,
  label,
  tagline,
  size,
  footer,
  directionalFooter,
  className,
  children,
  returnFocus,
  ...shell
}: ModalProps) {
  const close = t("slds.modal.close");
  const headless = title == null;
  const focus = useFocusReturn(shell.trigger, returnFocus);
  return (
    <Shell {...shell}>
      <Dialog.Content
        asChild
        aria-describedby={undefined}
        onInteractOutside={stayOpen}
        onOpenAutoFocus={focus.rememberOpener}
        onCloseAutoFocus={focus.onCloseAutoFocus}
      >
        <section
          role="dialog"
          tabIndex={-1}
          aria-modal="true"
          className={clsx("slds-modal slds-fade-in-open", size && `slds-modal_${size}`, className)}
        >
          <div className="slds-modal__container">
            <Dialog.Close asChild>
              <button type="button" className="slds-button slds-button_icon slds-modal__close">
                <X className="slds-button__icon slds-button__icon_large nx-lucide" aria-hidden="true" />
                <span className="slds-assistive-text">{close}</span>
              </button>
            </Dialog.Close>
            {headless ? (
              <Dialog.Title className="slds-assistive-text">{label}</Dialog.Title>
            ) : (
              <div className="slds-modal__header">
                <Dialog.Title asChild>
                  <h1 className="slds-modal__title slds-hyphenate" tabIndex={-1}>
                    {title}
                  </h1>
                </Dialog.Title>
                {tagline && <p className="slds-m-top_x-small">{tagline}</p>}
              </div>
            )}
            <div
              className={clsx(
                "slds-modal__content slds-p-around_medium",
                headless && "slds-modal__content_headless",
                headless && !footer && "slds-modal__content_footless",
              )}
            >
              {children}
            </div>
            {footer && (
              <div className={clsx("slds-modal__footer", directionalFooter && "slds-modal__footer_directional")}>{footer}</div>
            )}
          </div>
        </section>
      </Dialog.Content>
    </Shell>
  );
}

export interface PromptProps extends ShellProps {
  title: string;
  /** Header colour (`slds-theme_<theme>` + alert texture). */
  theme?: "warning" | "error" | "info";
  /** Footer buttons; default is a single neutral "Đã hiểu" that closes the prompt. */
  footer?: ReactNode;
  children: ReactNode;
}

export function Prompt({ title, theme = "warning", footer, children, returnFocus, ...shell }: PromptProps) {
  const messageRef = useRef<HTMLDivElement>(null);
  const focus = useFocusReturn(shell.trigger, returnFocus);
  return (
    <Shell {...shell}>
      <Dialog.Content
        asChild
        onInteractOutside={stayOpen}
        onOpenAutoFocus={(e) => {
          focus.rememberOpener();
          // SLDS prompt: the message container takes initial focus (docs "Keyboard Interaction").
          e.preventDefault();
          messageRef.current?.focus();
        }}
        onCloseAutoFocus={focus.onCloseAutoFocus}
      >
        <section role="alertdialog" tabIndex={-1} aria-modal="true" className="slds-modal slds-fade-in-open slds-modal_prompt">
          <div className="slds-modal__container">
            <div className={clsx("slds-modal__header slds-theme_alert-texture", `slds-theme_${theme}`)}>
              <Dialog.Title asChild>
                <h1 className="slds-text-heading_medium">{title}</h1>
              </Dialog.Title>
            </div>
            <Dialog.Description asChild>
              <div ref={messageRef} className="slds-modal__content slds-p-around_medium" tabIndex={0}>
                {children}
              </div>
            </Dialog.Description>
            <div className="slds-modal__footer slds-theme_default">
              {footer ?? (
                <Dialog.Close asChild>
                  <button type="button" className="slds-button slds-button_neutral">
                    {t("slds.prompt.ok")}
                  </button>
                </Dialog.Close>
              )}
            </div>
          </div>
        </section>
      </Dialog.Content>
    </Shell>
  );
}

/** Wrap a footer button so clicking it closes the enclosing Modal/Prompt. */
export const ModalClose = Dialog.Close;
