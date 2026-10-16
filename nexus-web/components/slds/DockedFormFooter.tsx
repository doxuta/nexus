"use client";

// SLDS docked form footer (blueprint `docked-form-footer/base`, SLDS 2.264.1): the Huỷ / Lưu bar
// pinned to the bottom of the viewport (`position:fixed` is SLDS's own) under a record form.
//   div.slds-docked-form-footer > [button.slds-button_icon-error] + button.slds-button_neutral + button.slds-button_brand
// "With error popover": the error button toggles (and, when errors first appear, opens by itself)
//   section.slds-popover.slds-popover_error[role=dialog] > button.slds-popover__close
//     + header.slds-popover__header > div.slds-media > figure(error icon) + body > h2.slds-text-heading_medium
//     + div.slds-popover__body
// on @radix-ui/react-popover (Esc, click-outside, focus return), content asChild like Popover.tsx and
// not portaled. Placement top/start: the nubbin points down at the error button.
// ErrorPopover is exported for the same pattern on a table row (data-tables/inline-edit "Error - Row
// level on save": button.slds-button_icon-error in the row's error cell + the same error popover),
// portaled there (`portal`) so the table's sticky header row cannot paint over it.
import * as PopoverPrimitive from "@radix-ui/react-popover";
import { CircleX, X } from "lucide-react";
import { useId, useRef, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { Button } from "./Button";
import { ButtonIcon, type ButtonIconGlyphSize } from "./ButtonIcon";
import { focusFirst } from "./focus";
import { Nubbed } from "./Popover";

export interface DockedFormFooterProps {
  onCancel: () => void;
  /** Click handler for Lưu; omit when `form` makes it a submit button. */
  onSave?: () => void;
  /** id of the <form> Lưu submits (the footer sits outside the form, docked to the viewport). */
  form?: string;
  /** Disables Lưu (request in flight). */
  saving?: boolean;
  /** Errors to resolve: shows the error button, whose popover lists them. */
  errors?: ReactNode;
  cancelLabel?: string;
  saveLabel?: string;
  /** Status text shown left of the buttons (e.g. "Đang xem trước: …"). */
  children?: ReactNode;
}

export function DockedFormFooter({ onCancel, onSave, form, saving, errors, cancelLabel, saveLabel, children }: DockedFormFooterProps) {
  return (
    <div className="slds-docked-form-footer">
      {children != null && children !== false && <span className="slds-text-body_regular slds-m-right_small">{children}</span>}
      {errors != null && errors !== false && (
        <ErrorPopover defaultOpen iconSize="large">
          {errors}
        </ErrorPopover>
      )}
      <Button variant="neutral" onClick={onCancel}>
        {cancelLabel ?? t("action.cancel")}
      </Button>
      <Button variant="brand" type={form ? "submit" : "button"} form={form} onClick={onSave} disabled={saving}>
        {saveLabel ?? t("action.save")}
      </Button>
    </div>
  );
}

export interface ErrorPopoverProps {
  /** Trigger's assistive text / title; default the popover heading ("Có lỗi cần sửa"). */
  label?: string;
  /** Open on mount (docked footer: "notify with a popover when errors are found"). */
  defaultOpen?: boolean;
  iconSize?: ButtonIconGlyphSize;
  /** Extra classes on the trigger button, e.g. the row error's `slds-m-horizontal_xxx-small`. */
  className?: string;
  /** Render the popover in a body portal: needed inside a fixed-layout table, whose tbody is its
   *  own stacking context (SLDS translateZ(0)) under a sticky header row. */
  portal?: boolean;
  /** The errors to resolve (popover body). */
  children: ReactNode;
}

/** Error button + "Resolve error" popover listing what to fix. */
export function ErrorPopover({ label, defaultOpen, iconSize, className, portal, children }: ErrorPopoverProps) {
  const id = useId();
  const ref = useRef<HTMLDivElement>(null);
  const title = t("slds.dockedFormFooter.errors");
  const close = t("slds.popover.close");
  return (
    // Docked footer: mounted only while `errors` is set, so defaultOpen = "notify with a popover when
    // errors are found" (docs.mdx) and onOpenAutoFocus moves focus in. ponytail: it does not reopen for
    // a later failed save while the same errors stay set; give the footer a new `key` per save if needed.
    <PopoverPrimitive.Root defaultOpen={defaultOpen}>
      <PopoverPrimitive.Trigger asChild>
        <ButtonIcon icon={CircleX} label={label ?? title} state="error" iconSize={iconSize} className={className} />
      </PopoverPrimitive.Trigger>
      <MaybePortal portal={portal}>
        <PopoverPrimitive.Content
          asChild
          ref={ref}
          side="top"
          align="start"
          sideOffset={12}
          onOpenAutoFocus={(e) => {
            e.preventDefault();
            focusFirst(ref.current, "slds-popover__close");
          }}
        >
          <Nubbed
            as="section"
            sizeVar="--radix-popover-trigger"
            aria-labelledby={`${id}-heading`}
            aria-describedby={`${id}-body`}
            className="slds-popover slds-popover_error"
          >
            <PopoverPrimitive.Close asChild>
              <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-float_right slds-popover__close slds-button_icon-inverse" title={close}>
                <X aria-hidden="true" className="slds-button__icon nx-lucide" />
                <span className="slds-assistive-text">{close}</span>
              </button>
            </PopoverPrimitive.Close>
            <header className="slds-popover__header">
              <div className="slds-media slds-media_center slds-has-flexi-truncate">
                <div className="slds-media__figure">
                  <span className="slds-icon_container slds-icon-utility-error">
                    <CircleX aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
                  </span>
                </div>
                <div className="slds-media__body">
                  <h2 className="slds-truncate slds-text-heading_medium" id={`${id}-heading`} title={title}>
                    {title}
                  </h2>
                </div>
              </div>
            </header>
            <div className="slds-popover__body" id={`${id}-body`}>
              {children}
            </div>
          </Nubbed>
        </PopoverPrimitive.Content>
      </MaybePortal>
    </PopoverPrimitive.Root>
  );
}

function MaybePortal({ portal, children }: { portal?: boolean; children: ReactNode }) {
  return portal ? <PopoverPrimitive.Portal>{children}</PopoverPrimitive.Portal> : <>{children}</>;
}
