"use client";

// SLDS visual picker (blueprint `visual-picker` coverable-content / vertical, SLDS 2.264.1):
//   fieldset.slds-form-element[role=radiogroup] > legend.slds-form-element__legend.slds-form-element__label
//     + div.slds-form-element__control > div.slds-visual-picker.slds-visual-picker_<size|vertical>
//       > input[type=radio|checkbox] + label[for] > span.slds-visual-picker__figure … + span.slds-visual-picker__body
// Native inputs (visually hidden by SLDS), so Tab / arrow keys / Space are the browser's own and the
// checked look is pure CSS (`input:checked ~ label`). The sprite glyphs become Lucide: the selected
// check keeps the blueprint's `slds-icon-action-check` square (white glyph via nx-lucide_tile).
// Deviations: the checkbox variant drops the blueprint's role=radiogroup (a fieldset is already a
// group; radiogroup would announce checkboxes as radios). Coverable options with a description use
// the non-coverable body (heading_small + text-title) — the blueprint's coverable body has one line.
import clsx from "clsx";
import { Check, type LucideIcon } from "lucide-react";
import type { CSSProperties } from "react";
import { FormElement, useFieldIds } from "./FormElement";

export interface VisualPickerOption {
  value: string;
  label: string;
  description?: string;
  /** Coverable variant: glyph shown until chosen. */
  icon?: LucideIcon;
  /** Object tile colour (`slds-icon-standard-<name>`); omit for a grey utility glyph. */
  iconName?: string;
  disabled?: boolean;
}

interface VisualPickerBaseProps {
  label: string;
  options: VisualPickerOption[];
  /** "coverable" (default) = icon tile + label; "vertical" = full-width text rows (blueprint vertical). */
  variant?: "coverable" | "vertical";
  /** Coverable tile size (default medium). */
  size?: "small" | "medium" | "large";
  name?: string;
  id?: string;
  error?: string;
  help?: string;
  required?: boolean;
  disabled?: boolean;
  className?: string;
}

export type VisualPickerProps = VisualPickerBaseProps &
  (
    | { multiple?: false; value?: string | null; onChange?: (value: string) => void }
    | { multiple: true; value?: string[]; onChange?: (value: string[]) => void }
  );

export function VisualPicker(props: VisualPickerProps) {
  const { label, options, variant = "coverable", size = "medium", name, id, error, help, required, disabled, className } = props;
  const { controlId, helpId } = useFieldIds(id);
  const chosen = props.multiple ? (props.value ?? []) : props.value == null ? [] : [props.value];
  const message = error ?? help;
  const toggle = (value: string, on: boolean) => {
    if (props.multiple) props.onChange?.(on ? [...chosen, value] : chosen.filter((v) => v !== value));
    else if (on) props.onChange?.(value);
  };
  return (
    <FormElement
      as="fieldset"
      role={props.multiple ? undefined : "radiogroup"}
      label={label}
      required={required}
      error={error}
      help={help}
      helpId={helpId}
      className={className}
    >
      {options.map((o, i) => {
        const optionId = `${controlId}-${i}`;
        return (
          <div
            key={o.value}
            className={clsx("slds-visual-picker", variant === "vertical" ? "slds-visual-picker_vertical" : `slds-visual-picker_${size}`)}
          >
            <input
              type={props.multiple ? "checkbox" : "radio"}
              id={optionId}
              name={name ?? controlId}
              value={o.value}
              checked={props.value === undefined ? undefined : chosen.includes(o.value)}
              disabled={disabled || o.disabled}
              required={required && !props.multiple}
              aria-describedby={message ? helpId : undefined}
              onChange={(e) => toggle(o.value, e.target.checked)}
            />
            <label htmlFor={optionId}>
              {variant === "vertical" ? (
                <>
                  <span className="slds-visual-picker__figure slds-visual-picker__text slds-align_absolute-center">
                    {/* w-full: the figure centres its flex child, so a short text block drifted to the
                        middle; the blueprint's text is left-aligned (only its long sample hid this). */}
                    <span className="w-full">
                      <span className="slds-text-heading_medium slds-m-bottom_x-small">{o.label}</span>
                      {o.description && <span className="slds-text-title">{o.description}</span>}
                    </span>
                  </span>
                  <span className="slds-icon_container slds-visual-picker__text-check">
                    {/* SLDS has no colour rule for slds-icon-text-check: the sprite keeps the .slds-icon
                        default white fill (invisible until the blue corner shows). The Lucide glue colours
                        .slds-icon from this hook, else currentColor, which painted a dark check on every
                        unselected row. */}
                    <Check
                      aria-hidden="true"
                      className="slds-icon slds-icon-text-check slds-icon_x-small nx-lucide"
                      style={{ "--slds-c-icon-color-foreground": "var(--slds-g-color-neutral-base-100)" } as CSSProperties}
                    />
                  </span>
                </>
              ) : (
                <>
                  <span className="slds-visual-picker__figure slds-visual-picker__icon slds-align_absolute-center">
                    <span className="slds-is-selected">
                      <span className="slds-icon_container">
                        <Check aria-hidden="true" className="slds-icon slds-icon_large slds-icon-action-check nx-lucide nx-lucide_tile" />
                      </span>
                    </span>
                    {o.icon && (
                      <span className="slds-is-not-selected">
                        <span className="slds-icon_container">
                          <o.icon
                            aria-hidden="true"
                            className={clsx(
                              "slds-icon slds-icon_large nx-lucide",
                              o.iconName ? `slds-icon-standard-${o.iconName} nx-lucide_tile` : "slds-icon-text-default",
                            )}
                          />
                        </span>
                      </span>
                    )}
                  </span>
                  <span className="slds-visual-picker__body">
                    {o.description ? (
                      <>
                        <span className="slds-text-heading_small">{o.label}</span>
                        <span className="slds-text-title">{o.description}</span>
                      </>
                    ) : (
                      <span className="slds-text-title">{o.label}</span>
                    )}
                  </span>
                </>
              )}
            </label>
          </div>
        );
      })}
    </FormElement>
  );
}
