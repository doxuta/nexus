"use client";

// SLDS checkbox (blueprint `checkbox` base + form-element, SLDS 2.264.1). The native <input> stays in
// the DOM (visually hidden by SLDS) and `.slds-checkbox_faux` draws the box, so Space/click/focus
// are the browser's own. Indeterminate is a DOM property SLDS styles via `:indeterminate`.
// CheckboxGroup = fieldset + legend; the forwarded ref goes on EVERY checkbox, which is how
// react-hook-form's `register()` collects a string[] from same-name checkboxes.
import clsx from "clsx";
import {
  forwardRef,
  useEffect,
  useRef,
  type ChangeEvent,
  type ChangeEventHandler,
  type FocusEventHandler,
  type InputHTMLAttributes,
} from "react";
import { t } from "@/lib/i18n";
import {
  FormElement,
  describedBy,
  setRef,
  useFieldIds,
  type ChoiceOption,
  type FieldProps,
} from "./FormElement";

export interface CheckboxProps extends FieldProps, Omit<InputHTMLAttributes<HTMLInputElement>, "type"> {
  indeterminate?: boolean;
  /**
   * `default`: box with the label beside it (checkbox/base).
   * `standalone`: form-element label above a bare box (checkbox/form-element) — the record-form look.
   */
  variant?: "default" | "standalone";
}

export const Checkbox = forwardRef<HTMLInputElement, CheckboxProps>(function Checkbox(
  {
    label,
    labelHidden,
    error,
    help,
    fieldLevelHelp,
    layout,
    oneColumn,
    editing,
    indeterminate = false,
    variant = "default",
    id,
    className,
    required,
    "aria-describedby": ariaDescribedBy,
    ...rest
  },
  ref,
) {
  const { controlId, helpId } = useFieldIds(id);
  const inner = useRef<HTMLInputElement | null>(null);
  useEffect(() => {
    if (inner.current) inner.current.indeterminate = indeterminate;
  }, [indeterminate]);
  const message = error ?? help;

  const input = (
    <input
      ref={(node) => {
        inner.current = node;
        setRef(ref, node);
      }}
      type="checkbox"
      id={controlId}
      required={required}
      aria-describedby={describedBy(message != null && message !== "" && helpId, ariaDescribedBy)}
      className={className}
      {...rest}
    />
  );
  const frame = { error, help, helpId, layout, oneColumn, editing };

  if (variant === "standalone")
    return (
      <FormElement
        {...frame}
        label={label}
        labelHidden={labelHidden}
        htmlFor={controlId}
        required={required}
        fieldLevelHelp={fieldLevelHelp}
      >
        <span className="slds-checkbox slds-checkbox_standalone">
          {input}
          <span className="slds-checkbox_faux" />
        </span>
      </FormElement>
    );

  return (
    <FormElement {...frame}>
      <div className="slds-checkbox">
        {/* Blueprint: a bare "*" hugging the box, no trailing space (unlike label/legend marks). */}
        {required && (
          <abbr className="slds-required" title={t("slds.formElement.required")} aria-hidden="true">
            *
          </abbr>
        )}
        {input}
        <label className="slds-checkbox__label" htmlFor={controlId}>
          <span className="slds-checkbox_faux" />
          <span className={clsx("slds-form-element__label", labelHidden && "slds-assistive-text")}>{label}</span>
        </label>
        {fieldLevelHelp && <div className="slds-form-element__icon">{fieldLevelHelp}</div>}
      </div>
    </FormElement>
  );
});

export interface CheckboxGroupProps extends Omit<FieldProps, "labelHidden"> {
  options: ChoiceOption[];
  name?: string;
  id?: string;
  /** Controlled checked values; pair with `onValueChange`. */
  value?: string[];
  defaultValue?: string[];
  onValueChange?: (value: string[]) => void;
  /** Native handlers, so `{...register("field")}` spreads straight on. */
  onChange?: ChangeEventHandler<HTMLInputElement>;
  onBlur?: FocusEventHandler<HTMLInputElement>;
  required?: boolean;
  disabled?: boolean;
  className?: string;
}

export const CheckboxGroup = forwardRef<HTMLInputElement, CheckboxGroupProps>(function CheckboxGroup(
  {
    label,
    error,
    help,
    fieldLevelHelp,
    layout,
    oneColumn,
    editing,
    options,
    name,
    id,
    value,
    defaultValue,
    onValueChange,
    onChange,
    onBlur,
    required,
    disabled,
    className,
  },
  ref,
) {
  const { controlId, helpId } = useFieldIds(id);
  const message = error ?? help;

  // Read the next set from the DOM: during onChange the clicked box already shows its new state,
  // so this is right for both controlled and uncontrolled use.
  const handleChange = (e: ChangeEvent<HTMLInputElement>) => {
    onChange?.(e);
    if (!onValueChange) return;
    const boxes = e.currentTarget.closest("fieldset")?.querySelectorAll<HTMLInputElement>('input[type="checkbox"]');
    onValueChange(Array.from(boxes ?? []).filter((b) => b.checked).map((b) => b.value));
  };

  return (
    <FormElement
      as="fieldset"
      label={label}
      required={required}
      error={error}
      help={help}
      helpId={helpId}
      fieldLevelHelp={fieldLevelHelp}
      layout={layout}
      oneColumn={oneColumn}
      editing={editing}
      className={className}
    >
      {options.map((o, i) => {
        const optionId = `${controlId}-${i}`;
        return (
          <div key={o.value} className="slds-checkbox">
            <input
              ref={ref}
              type="checkbox"
              id={optionId}
              name={name}
              value={o.value}
              checked={value ? value.includes(o.value) : undefined}
              defaultChecked={value ? undefined : defaultValue?.includes(o.value)}
              disabled={disabled || o.disabled}
              aria-describedby={message != null && message !== "" ? helpId : undefined}
              onChange={handleChange}
              onBlur={onBlur}
            />
            <label className="slds-checkbox__label" htmlFor={optionId}>
              <span className="slds-checkbox_faux" />
              <span className="slds-form-element__label">{o.label}</span>
            </label>
          </div>
        );
      })}
    </FormElement>
  );
});
