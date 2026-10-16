// SLDS icon (blueprint `icons`, SLDS 2.264.1): the coloured container is SLDS CSS (BSD-3); the glyph
// is Lucide (ISC). The SLDS sprite under assets/icons is CC BY-ND and is never used
// (campaign D3, THIRD_PARTY_NOTICES.md). Lucide draws with strokes, so globals.css forces
// `fill:none; stroke:currentColor` on `.nx-lucide` — SLDS's `.slds-icon { fill }` would paint it solid.
import clsx from "clsx";
import type { LucideIcon } from "lucide-react";

/** Object-type tile colours (`slds-icon-standard-<name>`), or a bare glyph for utility icons. */
export type IconCategory = "standard" | "utility" | "action" | "custom";
export type IconSize = "xx-small" | "x-small" | "small" | "medium" | "large";

export interface IconProps {
  icon: LucideIcon;
  /** Container colour name, e.g. "account", "opportunity" (standard) or "custom12" (custom). */
  name?: string;
  category?: IconCategory;
  size?: IconSize;
  /** Accessible text; omit for decorative icons. */
  label?: string;
  /** Utility icons only: SLDS text colour modifier (default | warning | error | success | light). */
  tone?: "default" | "warning" | "error" | "success" | "light";
  className?: string;
}

const SIZE_CLASS: Record<IconSize, string | undefined> = {
  "xx-small": "slds-icon_xx-small",
  "x-small": "slds-icon_x-small",
  small: "slds-icon_small",
  medium: undefined, // SLDS default 2rem
  large: "slds-icon_large",
};

export function Icon({ icon: Glyph, name, category = "utility", size = "small", label, tone, className }: IconProps) {
  const tile = category !== "utility" && name;
  return (
    <span
      className={clsx(
        "slds-icon_container",
        tile && (category === "custom" ? `slds-icon-custom-${name}` : `slds-icon-${category}-${name}`),
        category === "utility" && name && `slds-icon-utility-${name}`,
        className,
      )}
      title={label}
    >
      <Glyph
        aria-hidden="true"
        className={clsx(
          "slds-icon nx-lucide",
          SIZE_CLASS[size],
          !tile && `slds-icon-text-${tone ?? "default"}`,
          tile && "nx-lucide_tile",
        )}
      />
      {label && <span className="slds-assistive-text">{label}</span>}
    </span>
  );
}
