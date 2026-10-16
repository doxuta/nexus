"use client";

// SLDS form element (blueprint `form-element`: base / stacked / horizontal / record-detail, SLDS 2.264.1).
// The frame every Nexus field control renders through: label (or legend for groups), required
// marker, field-level-help slot, the `__control` wrapper and the help/error line. Controls
// (Input, Textarea, Select, Checkbox…) own the native element and point `aria-describedby` at
// `helpId` — FormElement only draws the frame, it never guesses the control.
import clsx from "clsx";
import { Pencil, type LucideIcon } from "lucide-react";
import { useId, type HTMLAttributes, type ReactNode, type Ref } from "react";
import { t } from "@/lib/i18n";

/** `base` = label above, no record-form spacing; `stacked` / `horizontal` = record-form layouts. */
export type FormElementLayout = "base" | "stacked" | "horizontal";

/** One choice of a checkbox group, radio group, radio button group or select. */
export interface ChoiceOption {
  value: string;
  label: string;
  disabled?: boolean;
  /** Radio button group only: Lucide glyph drawn left of the label. */
  icon?: LucideIcon;
}

/** Props shared by every labelled control (Input, Textarea, Select, groups). */
export interface FieldProps {
  label: ReactNode;
  /** Keep the label for assistive tech only (`slds-assistive-text`). */
  labelHidden?: boolean;
  /** Error text — adds `slds-has-error` and replaces `help`. */
  error?: ReactNode;
  /** Inline help under the control, e.g. "VD: 0912 345 678". */
  help?: ReactNode;
  /** Slot rendered in `slds-form-element__icon` next to the label (help button + tooltip). */
  fieldLevelHelp?: ReactNode;
  layout?: FormElementLayout;
  /** Horizontal record forms: span both columns (`slds-form-element_1-col`). */
  oneColumn?: boolean;
  /**
   * Record-detail edit mode (`slds-is-editing`, blueprint record-detail edit-stacked /
   * horizontal edit-horizontal). Opt-in: simple stacked/horizontal forms must not get it.
   */
  editing?: boolean;
}

export interface FormElementProps
  extends Omit<FieldProps, "label">,
    Omit<HTMLAttributes<HTMLElement>, "children"> {
  /** Omit for a bare checkbox, whose label sits inside the control. */
  label?: ReactNode;
  /** id of the native control — renders `<label for>`; omit and the label is a `<span>`. */
  htmlFor?: string;
  labelId?: string;
  /** id of the help/error line; controls reference it from `aria-describedby`. */
  helpId?: string;
  required?: boolean;
  /** Group of checkboxes/radios: `<fieldset>` + `<legend>`. */
  as?: "div" | "fieldset";
  /**
   * View mode: children render as `slds-form-element__static`, the label becomes a span.
   * `true` = record-detail/checkbox view mode (`slds-form-element_readonly`, bottom border);
   * `"static"` = the input/textarea blueprints' static state (no readonly modifier).
   */
  readOnly?: boolean | "static";
  /** View mode only: shows the hover pencil (`slds-form-element_edit`). */
  onEdit?: () => void;
  /** radio-button-group puts its error line inside `__control`; everything else after it. */
  helpInControl?: boolean;
  controlClassName?: string;
  children?: ReactNode;
}

export function RequiredMark() {
  return (
    <abbr className="slds-required" title={t("slds.formElement.required")} aria-hidden="true">
      *{" "}
    </abbr>
  );
}

/** Stable ids for a field: the control, its label and its help/error line. */
export function useFieldIds(id?: string) {
  const auto = useId();
  const controlId = id ?? auto;
  return { controlId, labelId: `${controlId}-label`, helpId: `${controlId}-help` };
}

/** Joins `aria-describedby` ids, dropping empties. */
export function describedBy(...ids: (string | false | undefined)[]) {
  return ids.filter(Boolean).join(" ") || undefined;
}

/** Hands a DOM node to a forwarded ref (callback or object) — RHF's `register` ref is a callback. */
export function setRef<T>(ref: Ref<T> | undefined, node: T | null) {
  if (typeof ref === "function") ref(node);
  else if (ref) ref.current = node;
}

export function FormElement({
  label,
  labelHidden,
  htmlFor,
  labelId,
  helpId,
  required,
  error,
  help,
  fieldLevelHelp,
  layout = "base",
  oneColumn,
  editing,
  as: Root = "div",
  readOnly,
  onEdit,
  helpInControl,
  controlClassName,
  className,
  children,
  ...rest
}: FormElementProps) {
  const labelClass = clsx(
    "slds-form-element__label",
    Root === "fieldset" && "slds-form-element__legend",
    labelHidden && "slds-assistive-text",
  );
  const labelContent = (
    <>
      {/* Record-detail view mode keeps the mark; only the input/textarea "static" state drops it. */}
      {required && readOnly !== "static" && <RequiredMark />}
      {label}
    </>
  );
  let labelNode: ReactNode = null;
  if (label !== undefined) {
    if (Root === "fieldset") labelNode = <legend className={labelClass}>{labelContent}</legend>;
    else if (htmlFor && !readOnly)
      labelNode = (
        <label className={labelClass} htmlFor={htmlFor} id={labelId}>
          {labelContent}
        </label>
      );
    else
      labelNode = (
        <span className={labelClass} id={labelId}>
          {labelContent}
        </span>
      );
  }

  const message = error ?? help;
  const helpNode =
    message !== undefined && message !== null && message !== "" ? (
      <div className="slds-form-element__help" id={helpId}>
        {message}
      </div>
    ) : null;

  const editTitle = `${t("slds.formElement.edit")}${typeof label === "string" ? `: ${label}` : ""}`;

  return (
    <Root
      className={clsx(
        "slds-form-element",
        onEdit && readOnly && "slds-form-element_edit",
        readOnly === true && "slds-form-element_readonly",
        layout === "stacked" && "slds-form-element_stacked",
        layout === "horizontal" && "slds-form-element_horizontal",
        onEdit && readOnly && "slds-hint-parent",
        editing && "slds-is-editing",
        oneColumn && "slds-form-element_1-col",
        Root === "fieldset" && required && "slds-is-required",
        error && "slds-has-error",
        className,
      )}
      {...rest}
    >
      {labelNode}
      {fieldLevelHelp && <div className="slds-form-element__icon">{fieldLevelHelp}</div>}
      <div className={clsx("slds-form-element__control", controlClassName)}>
        {readOnly ? <div className="slds-form-element__static">{children}</div> : children}
        {readOnly && onEdit && (
          <button type="button" className="slds-button slds-button_icon" title={editTitle} onClick={onEdit}>
            <Pencil className="slds-button__icon slds-button__icon_hint nx-lucide" aria-hidden="true" />
            <span className="slds-assistive-text">{editTitle}</span>
          </button>
        )}
        {helpInControl && helpNode}
      </div>
      {!helpInControl && helpNode}
    </Root>
  );
}
