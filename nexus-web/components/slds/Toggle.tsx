"use client";

// SLDS toggle (blueprint `checkbox-toggle`, SLDS 2.264.1): a native checkbox with role="switch"
// inside label.slds-checkbox_toggle; the faux track carries the on/off text and describes the
// switch. Space/click are the browser's own. Ref → native <input>.
// `help` is a div.slds-form-element__help under the toggle (form-element help, as in the checkbox
// blueprint) and joins the switch's description after the on/off text.
import clsx from "clsx";
import { forwardRef, useId, type InputHTMLAttributes, type ReactNode } from "react";
import { t } from "@/lib/i18n";

export interface ToggleProps extends Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "role"> {
  label: ReactNode;
  /** Text shown when on / off; default "Bật" / "Tắt". */
  onText?: string;
  offText?: string;
  /** Help line under the toggle, read with the switch (aria-describedby). */
  help?: ReactNode;
}

export const Toggle = forwardRef<HTMLInputElement, ToggleProps>(function Toggle(
  { label, onText, offText, help, className, "aria-describedby": describedBy, ...rest },
  ref,
) {
  const fauxId = useId();
  const helpId = useId();
  return (
    <div className={clsx("slds-form-element", className)}>
      <label className="slds-checkbox_toggle slds-grid">
        <span className="slds-form-element__label slds-m-bottom_none">{label}</span>
        <input
          ref={ref}
          type="checkbox"
          role="switch"
          aria-describedby={[fauxId, help != null && helpId, describedBy].filter(Boolean).join(" ")}
          {...rest}
        />
        <span id={fauxId} className="slds-checkbox_faux_container">
          <span className="slds-checkbox_faux" />
          <span className="slds-checkbox_on">{onText ?? t("slds.toggle.on")}</span>
          <span className="slds-checkbox_off">{offText ?? t("slds.toggle.off")}</span>
        </span>
      </label>
      {help != null && (
        <div id={helpId} className="slds-form-element__help">
          {help}
        </div>
      )}
    </div>
  );
});
