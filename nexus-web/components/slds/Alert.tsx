"use client"; // GLYPH/VARIANT_LABEL come from the client module Toast.tsx

// SLDS alert banner (blueprint `alert/base`, SLDS 2.264.1) — system-wide state (offline, session,
// maintenance) shown above the page:
//   div.slds-notify.slds-notify_alert[.slds-alert_<level>][role=alert]
//     > span.slds-assistive-text + span.slds-icon_container.slds-icon-utility-<name>.slds-m-right_x-small > svg.slds-icon.slds-icon_x-small
//     + h2 (message, links inline) + div.slds-notify__close > button.slds-button_icon.slds-button_icon-small[.slds-button_icon-inverse]
// The default level has no modifier class (the `slds-theme_*` / alert-texture states are deprecated
// and not copied). Warning sits on a light background, so its close button is not inverse.
// The glyph takes the banner's text colour, like the blueprint svg (no slds-icon-text-* tone).
import clsx from "clsx";
import { WifiOff, X, type LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";
import { GLYPH, VARIANT_LABEL } from "./Toast";

export type AlertVariant = "info" | "warning" | "error" | "offline";

const LEVEL: Record<AlertVariant, { glyph: LucideIcon; label: Parameters<typeof t>[0] }> = {
  info: { glyph: GLYPH.info, label: VARIANT_LABEL.info },
  warning: { glyph: GLYPH.warning, label: VARIANT_LABEL.warning },
  error: { glyph: GLYPH.error, label: VARIANT_LABEL.error },
  offline: { glyph: WifiOff, label: "slds.alert.offline" },
};

export interface AlertProps {
  /** Default "info" (blueprint default, no modifier class). */
  variant?: AlertVariant;
  /** Message; links go inline (`<a href>` — SLDS underlines them in the banner colour). */
  children: ReactNode;
  /** Glyph override, e.g. UserRound for "logged in as…". */
  icon?: LucideIcon;
  /** Utility name for the icon container class (`slds-icon-utility-<name>`); default = the level. */
  iconName?: string;
  /** Controls rendered NEXT TO the message, outside its <h2> (a button inside a heading pollutes its accessible name). */
  action?: ReactNode;
  /** Shows the close button when set. */
  onClose?: () => void;
  className?: string;
}

export function Alert({ variant = "info", children, icon, iconName, action, onClose, className }: AlertProps) {
  const { glyph, label } = LEVEL[variant];
  const Glyph = icon ?? glyph;
  const close = t("action.close");
  return (
    <div className={clsx("slds-notify slds-notify_alert", variant !== "info" && `slds-alert_${variant}`, className)} role="alert">
      <span className="slds-assistive-text">{t(label)}</span>
      <span className={clsx("slds-icon_container slds-m-right_x-small", `slds-icon-utility-${iconName ?? variant}`)}>
        <Glyph aria-hidden="true" className="slds-icon slds-icon_x-small nx-lucide" />
      </span>
      <h2>{children}</h2>
      {action}
      {onClose && (
        <div className="slds-notify__close">
          <button
            type="button"
            className={clsx("slds-button slds-button_icon slds-button_icon-small", variant !== "warning" && "slds-button_icon-inverse")}
            title={close}
            onClick={onClose}
          >
            <X aria-hidden="true" className="slds-button__icon nx-lucide" />
            <span className="slds-assistive-text">{close}</span>
          </button>
        </div>
      )}
    </div>
  );
}
