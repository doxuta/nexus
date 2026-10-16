"use client";

// SLDS select (blueprint `select`, SLDS 2.264.1): a native <select> in `slds-select_container`.
// Single selects open with an empty option ("Chọn…") so a form never silently submits the first
// choice. `multiple` follows "select-multiple-narrow" (no container, no placeholder); the
// dual-list "select-multiple" example belongs to DuelingPicklist. Ref → native <select>.
import clsx from "clsx";
import { forwardRef, type SelectHTMLAttributes } from "react";
import { t } from "@/lib/i18n";
import { FormElement, describedBy, useFieldIds, type ChoiceOption, type FieldProps } from "./FormElement";

export interface SelectProps extends FieldProps, SelectHTMLAttributes<HTMLSelectElement> {
  options?: ChoiceOption[];
  /** Text of the leading empty option; `false` removes it. Ignored for `multiple`. */
  placeholder?: string | false;
}

export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  {
    label,
    labelHidden,
    error,
    help,
    fieldLevelHelp,
    layout,
    oneColumn,
    editing,
    options,
    placeholder,
    id,
    className,
    required,
    multiple,
    children,
    "aria-describedby": ariaDescribedBy,
    ...rest
  },
  ref,
) {
  const { controlId, helpId } = useFieldIds(id);
  const message = error ?? help;
  const empty = multiple || placeholder === false ? null : (placeholder ?? t("slds.select.placeholder"));

  const select = (
    <select
      ref={ref}
      id={controlId}
      required={required}
      multiple={multiple}
      aria-invalid={error ? true : undefined}
      aria-describedby={describedBy(message != null && message !== "" && helpId, ariaDescribedBy)}
      className={clsx("slds-select", className)}
      {...rest}
    >
      {empty !== null && <option value="">{empty}</option>}
      {options?.map((o) => (
        <option key={o.value} value={o.value} disabled={o.disabled}>
          {o.label}
        </option>
      ))}
      {children}
    </select>
  );

  return (
    <FormElement
      label={label}
      labelHidden={labelHidden}
      htmlFor={controlId}
      helpId={helpId}
      required={required}
      error={error}
      help={help}
      fieldLevelHelp={fieldLevelHelp}
      layout={layout}
      oneColumn={oneColumn}
      editing={editing}
    >
      {multiple ? select : <div className="slds-select_container">{select}</div>}
    </FormElement>
  );
});
