"use client";

// SLDS textarea (blueprint `textarea`, SLDS 2.264.1). The blueprint's "read only" state is not a
// readonly <textarea> but static text (span label, `__control.slds-border_bottom`, `__static > p`):
// that is `staticValue`. `readOnly` stays the native attribute. The ref lands on the native
// <textarea>, so react-hook-form's `register()` works in both editable and readOnly states.
import clsx from "clsx";
import { forwardRef, Fragment, type TextareaHTMLAttributes } from "react";
import { FormElement, describedBy, useFieldIds, type FieldProps } from "./FormElement";

export interface TextareaProps extends FieldProps, TextareaHTMLAttributes<HTMLTextAreaElement> {
  /** View mode: renders this text statically (blueprint "Read only"); newlines become <br>. */
  staticValue?: string;
}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(function Textarea(
  {
    label,
    labelHidden,
    error,
    help,
    fieldLevelHelp,
    layout,
    oneColumn,
    editing,
    id,
    className,
    required,
    staticValue,
    "aria-describedby": ariaDescribedBy,
    ...rest
  },
  ref,
) {
  const { controlId, helpId } = useFieldIds(id);
  const message = error ?? help;
  const frame = { label, labelHidden, error, help, fieldLevelHelp, layout, oneColumn, editing, helpId, required };

  if (staticValue !== undefined)
    return (
      <FormElement {...frame} readOnly="static" controlClassName="slds-border_bottom">
        <p>
          {staticValue.split("\n").map((line, i) => (
            <Fragment key={i}>
              {i > 0 && <br />}
              {line}
            </Fragment>
          ))}
        </p>
      </FormElement>
    );

  return (
    <FormElement {...frame} htmlFor={controlId}>
      <textarea
        ref={ref}
        id={controlId}
        required={required}
        aria-invalid={error ? true : undefined}
        aria-describedby={describedBy(message != null && message !== "" && helpId, ariaDescribedBy)}
        className={clsx("slds-textarea", className)}
        {...rest}
      />
    </FormElement>
  );
});
