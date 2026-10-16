"use client";

// SLDS text input (blueprint `input`, SLDS 2.264.1) inside the form-element frame. Covers the
// blueprint's left/right icon, left icon + clear button, clear with spinner (icon group), fixed
// text addons, readonly, disabled and error states. The ref lands on the native <input>, so
// react-hook-form's `register()` spreads straight onto it.
import clsx from "clsx";
import { CircleX, type LucideIcon } from "lucide-react";
import { forwardRef, useRef, type InputHTMLAttributes, type ReactNode } from "react";
import { t } from "@/lib/i18n";
import { FormElement, describedBy, setRef, useFieldIds, type FieldProps } from "./FormElement";

export interface InputProps extends FieldProps, Omit<InputHTMLAttributes<HTMLInputElement>, "size"> {
  iconLeft?: LucideIcon;
  /** Decorative glyph on the right; ignored when `clearable`/`loading` own that side. */
  iconRight?: LucideIcon;
  /** Clear button (blueprint "Left Icon & Clear Button"); works controlled and with register(). */
  clearable?: boolean;
  onClear?: () => void;
  /** Spinner before the clear button (blueprint "Clear Button with Spinner"). */
  loading?: boolean;
  /** Fixed text addons (blueprint "Fixed text"), e.g. "₫" / "VND". */
  fixedTextLeft?: ReactNode;
  fixedTextRight?: ReactNode;
  /** Right-align with tabular digits; defaults on for type="number". */
  numeric?: boolean;
}

export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  {
    label,
    labelHidden,
    error,
    help,
    fieldLevelHelp,
    layout,
    oneColumn,
    editing,
    iconLeft: IconLeft,
    iconRight: IconRight,
    clearable,
    onClear,
    loading,
    fixedTextLeft,
    fixedTextRight,
    numeric,
    id,
    type = "text",
    className,
    required,
    "aria-describedby": ariaDescribedBy,
    "aria-labelledby": ariaLabelledBy,
    ...rest
  },
  ref,
) {
  const { controlId, labelId, helpId } = useFieldIds(id);
  const inner = useRef<HTMLInputElement | null>(null);
  const editable = !rest.disabled && !rest.readOnly;
  const showClear = clearable && editable;
  const group = Boolean(loading); // the spinner forces the icon-group shape
  const right = Boolean(showClear || loading || IconRight);
  const hasAddon = fixedTextLeft != null || fixedTextRight != null;
  const preId = `${controlId}-addon-pre`;
  const postId = `${controlId}-addon-post`;
  const message = error ?? help;

  // Set the value through the native setter and dispatch `input`, so React's onChange (and with it
  // register()'s handler) sees the change exactly as if the user had deleted the text.
  const clear = () => {
    const el = inner.current;
    if (!el) return;
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set?.call(el, "");
    el.dispatchEvent(new Event("input", { bubbles: true }));
    el.focus();
    onClear?.();
  };

  const clearButton = showClear && (
    <button
      type="button"
      className="slds-button slds-button_icon slds-input__icon slds-input__icon_right"
      title={t("slds.input.clear")}
      onClick={clear}
    >
      <CircleX className="slds-button__icon slds-icon-text-light nx-lucide" aria-hidden="true" />
      <span className="slds-assistive-text">{t("slds.input.clear")}</span>
    </button>
  );

  return (
    <FormElement
      label={label}
      labelHidden={labelHidden}
      htmlFor={controlId}
      labelId={labelId}
      helpId={helpId}
      required={required}
      error={error}
      help={help}
      fieldLevelHelp={fieldLevelHelp}
      layout={layout}
      oneColumn={oneColumn}
      editing={editing}
      controlClassName={clsx(
        (IconLeft || right) && "slds-input-has-icon",
        IconLeft && !right && "slds-input-has-icon_left",
        !IconLeft && right && "slds-input-has-icon_right",
        IconLeft && right && "slds-input-has-icon_left-right",
        group && "slds-input-has-icon_group-right",
        hasAddon && "slds-input-has-fixed-addon",
      )}
    >
      {IconLeft && (
        <IconLeft
          className="slds-icon slds-input__icon slds-input__icon_left slds-icon-text-default nx-lucide"
          aria-hidden="true"
        />
      )}
      {fixedTextLeft != null && (
        <span className="slds-form-element__addon" id={preId}>
          {fixedTextLeft}
        </span>
      )}
      <input
        ref={(node) => {
          inner.current = node;
          setRef(ref, node);
        }}
        id={controlId}
        type={type}
        required={required}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(message != null && message !== "" && helpId, ariaDescribedBy)}
        aria-labelledby={
          hasAddon
            ? describedBy(labelId, fixedTextLeft != null && preId, fixedTextRight != null && postId)
            : ariaLabelledBy
        }
        className={clsx(
          "slds-input",
          (numeric ?? type === "number") && "slds-text-align_right tabular-nums",
          className,
        )}
        {...rest}
      />
      {fixedTextRight != null && (
        <span className="slds-form-element__addon" id={postId}>
          {fixedTextRight}
        </span>
      )}
      {group ? (
        <div className="slds-input__icon-group slds-input__icon-group_right">
          <div role="status" className="slds-spinner slds-spinner_brand slds-spinner_x-small slds-input__spinner">
            <span className="slds-assistive-text">{t("slds.input.loading")}</span>
            <div className="slds-spinner__dot-a" />
            <div className="slds-spinner__dot-b" />
          </div>
          {clearButton}
        </div>
      ) : (
        clearButton ||
        (IconRight && (
          <IconRight
            className="slds-icon slds-input__icon slds-input__icon_right slds-icon-text-default nx-lucide"
            aria-hidden="true"
          />
        ))
      )}
    </FormElement>
  );
});
