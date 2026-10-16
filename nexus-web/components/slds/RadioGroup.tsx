"use client";

// SLDS radio group + radio button group (blueprints `radio-group`, `radio-button-group`, SLDS
// 2.264.1). Native radios sharing one name, so arrow keys, Space and the single tab stop are the
// browser's own. The forwarded ref goes on EVERY radio — react-hook-form's `register()` callback
// collects them — and `value`/`onValueChange` give a controlled mode. `labelHidden` puts the legend
// in slds-assistive-text (a matrix cell whose row + column headers already name the group).
import { forwardRef, type ChangeEventHandler, type FocusEventHandler, type ReactNode } from "react";
import { FormElement, useFieldIds, type ChoiceOption, type FieldProps } from "./FormElement";

export interface RadioGroupProps extends FieldProps {
  options: ChoiceOption[];
  /** Shared input name; defaults to the group id so the browser always groups the radios. */
  name?: string;
  id?: string;
  value?: string;
  defaultValue?: string;
  onValueChange?: (value: string) => void;
  onChange?: ChangeEventHandler<HTMLInputElement>;
  onBlur?: FocusEventHandler<HTMLInputElement>;
  required?: boolean;
  disabled?: boolean;
  className?: string;
}

const Choices = forwardRef<HTMLInputElement, RadioGroupProps & { look: "radio" | "button" }>(function Choices(
  {
    look,
    id,
    label,
    labelHidden,
    error,
    help,
    fieldLevelHelp,
    layout,
    oneColumn,
    editing,
    options,
    name,
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
  const describedBy = message != null && message !== "" ? helpId : undefined;

  const radios: ReactNode[] = options.map((o, i) => {
    const optionId = `${controlId}-${i}`;
    const input = (
      <input
        ref={ref}
        type="radio"
        id={optionId}
        name={name ?? controlId}
        value={o.value}
        checked={value !== undefined ? value === o.value : undefined}
        defaultChecked={value !== undefined ? undefined : defaultValue === o.value}
        disabled={disabled || o.disabled}
        aria-describedby={describedBy}
        onChange={(e) => {
          onChange?.(e);
          onValueChange?.(o.value);
        }}
        onBlur={onBlur}
      />
    );
    if (look === "button") {
      const Glyph = o.icon;
      return (
        <span key={o.value} className="slds-button slds-radio_button">
          {input}
          <label className="slds-radio_button__label" htmlFor={optionId}>
            <span className="slds-radio_faux">
              {Glyph && (
                <Glyph
                  className="slds-button__icon slds-button__icon_left slds-m-bottom_xxx-small nx-lucide"
                  aria-hidden="true"
                />
              )}
              {o.label}
            </span>
          </label>
        </span>
      );
    }
    return (
      <span key={o.value} className="slds-radio">
        {input}
        <label className="slds-radio__label" htmlFor={optionId}>
          <span className="slds-radio_faux" />
          <span className="slds-form-element__label">{o.label}</span>
        </label>
      </span>
    );
  });

  return (
    <FormElement
      as="fieldset"
      role="radiogroup"
      aria-required={required || undefined}
      label={label}
      labelHidden={labelHidden}
      required={required}
      error={error}
      help={help}
      helpId={helpId}
      helpInControl={look === "button"}
      fieldLevelHelp={fieldLevelHelp}
      layout={layout}
      oneColumn={oneColumn}
      editing={editing}
      className={className}
    >
      {look === "button" ? <div className="slds-radio_button-group">{radios}</div> : radios}
    </FormElement>
  );
});

export const RadioGroup = forwardRef<HTMLInputElement, RadioGroupProps>(function RadioGroup(props, ref) {
  return <Choices look="radio" {...props} ref={ref} />;
});

/** Segmented single choice (e.g. Ngày / Tuần / Tháng); options may carry a Lucide `icon`. */
export const RadioButtonGroup = forwardRef<HTMLInputElement, RadioGroupProps>(function RadioButtonGroup(props, ref) {
  return <Choices look="button" {...props} ref={ref} />;
});
