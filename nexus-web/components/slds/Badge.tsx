// SLDS badge (blueprint `badges/base`, SLDS 2.264.1): `<span class="slds-badge [slds-badge_inverse|_lightest]
// [slds-theme_success|warning|error]">`. Icons follow the blueprint wrapper
// `span.slds-badge__icon_<side> > span.slds-icon_container.slds-current-color > svg.slds-icon_xx-small`
// with a Lucide glyph (sprite is CC BY-ND); slds-current-color makes it take the badge's text colour.
// Embedded (specs/073 FR-005): `<span class="nx-status nx-status_{tone}">` coloured text, no icon, same words:
// theme success/warning/error -> success/warn/error, no theme -> weak. Outside: unchanged.
"use client";

import clsx from "clsx";
import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";
import { useEmbedded } from "./EmbeddedSurface";

export interface BadgeProps {
  variant?: "default" | "lightest" | "inverse";
  /** Status colours (blueprint states). */
  theme?: "success" | "warning" | "error";
  iconLeft?: LucideIcon;
  iconRight?: LucideIcon;
  /** Optional hover title on the icon container. */
  iconTitle?: string;
  className?: string;
  children: ReactNode;
}

const EMBEDDED_TONE = { success: "success", warning: "warn", error: "error" } as const;

export function Badge({ variant = "default", theme, iconLeft, iconRight, iconTitle, className, children }: BadgeProps) {
  const embedded = useEmbedded();
  if (embedded) return <span className={clsx("nx-status", `nx-status_${theme ? EMBEDDED_TONE[theme] : "weak"}`, className)}>{children}</span>;
  const icon = (Glyph: LucideIcon, side: "left" | "right") => (
    <span className={clsx("slds-badge__icon", `slds-badge__icon_${side}`, variant === "inverse" && "slds-badge__icon_inverse")}>
      <span className="slds-icon_container slds-current-color" title={iconTitle}>
        <Glyph aria-hidden="true" className="slds-icon slds-icon_xx-small nx-lucide" />
      </span>
    </span>
  );
  return (
    <span className={clsx("slds-badge", variant !== "default" && `slds-badge_${variant}`, theme && `slds-theme_${theme}`, className)}>
      {iconLeft && icon(iconLeft, "left")}
      {children}
      {iconRight && icon(iconRight, "right")}
    </span>
  );
}
