"use client"; // GLYPH/VARIANT_LABEL come from the client module Toast.tsx

// SLDS scoped notification (blueprint `scoped-notifications/base`, SLDS 2.264.1) — a message about
// one region of the page (a record, a form), not the whole app (that is Alert):
//   div.slds-scoped-notification.slds-media.slds-media_center.<theme>[role=status]
//     > div.slds-media__figure > span.slds-icon_container.slds-icon-utility-<level>[title]
//         > svg.slds-icon.slds-icon_small[.slds-icon-text-default] + span.slds-assistive-text
//     + div.slds-media__body > p
// Themes: light (default, grey glyph) / dark use the info glyph; info/success/warning/error use
// `slds-theme_<level>` and that level's glyph, taking the theme's text colour.
import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";
import { GLYPH, VARIANT_LABEL, type ToastVariant } from "./Toast";

export type ScopedNotificationTheme = "light" | "dark" | ToastVariant;

export interface ScopedNotificationProps {
  /** Default "light". */
  theme?: ScopedNotificationTheme;
  /** Message; inline links allowed. */
  children: ReactNode;
  /** Glyph override (light/dark themes); the container keeps the level's colour class. */
  icon?: LucideIcon;
  className?: string;
  /** Default "status" (polite); use "alert" for a failure of a one-way action. */
  role?: "status" | "alert";
}

export function ScopedNotification({ theme = "light", children, icon, className, role = "status" }: ScopedNotificationProps) {
  const level = theme === "light" || theme === "dark" ? "info" : theme;
  const Glyph = icon ?? GLYPH[level];
  const label = t(VARIANT_LABEL[level]);
  return (
    <div
      className={clsx(
        "slds-scoped-notification slds-media slds-media_center",
        level === theme ? `slds-theme_${theme}` : `slds-scoped-notification_${theme}`,
        className,
      )}
      role={role}
    >
      <div className="slds-media__figure">
        <span className={clsx("slds-icon_container", `slds-icon-utility-${level}`)} title={label}>
          <Glyph aria-hidden="true" className={clsx("slds-icon slds-icon_small nx-lucide", theme === "light" && "slds-icon-text-default")} />
          <span className="slds-assistive-text">{label}</span>
        </span>
      </div>
      <div className="slds-media__body">
        <p>{children}</p>
      </div>
    </div>
  );
}
