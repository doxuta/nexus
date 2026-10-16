// SLDS button icon (blueprint `button-icons` base / transparent-container / bordered-* / brand /
// inverse / stateful, SLDS 2.264.1). Markup is the blueprint's
// `<button class="slds-button slds-button_icon …" title><svg class="slds-button__icon"/><span class="slds-assistive-text"/></button>`;
// the sprite glyph is a Lucide icon (sprite is CC BY-ND — never used).
// `ref` and Radix Slot props (aria-expanded, data-state, handlers) pass through `...rest`, so it can be
// a `DropdownMenu.Trigger asChild` / `Popover.Trigger asChild` child as-is.
import clsx from "clsx";
import { ChevronDown, type LucideIcon } from "lucide-react";
import type { ComponentProps } from "react";

export type ButtonIconVariant = "bare" | "container" | "border" | "border-filled" | "border-inverse" | "brand" | "inverse";
/** Container size (`slds-button_icon-<size>`); omitted = SLDS default 2rem. */
export type ButtonIconSize = "xx-small" | "x-small" | "small" | "large";
/** Glyph size (`slds-button__icon_<size>`); omitted = SLDS default .875rem. */
export type ButtonIconGlyphSize = "x-small" | "small" | "large";

const VARIANT: Record<ButtonIconVariant, string | undefined> = {
  bare: undefined,
  container: "slds-button_icon-container",
  border: "slds-button_icon-border",
  "border-filled": "slds-button_icon-border-filled",
  "border-inverse": "slds-button_icon-border-inverse",
  brand: "slds-button_icon-brand",
  inverse: "slds-button_icon-inverse",
};

// "With dropdown" examples — each variant's blueprint uses its own class set (copied verbatim, including
// bordered-inverse → slds-button_icon-more). Brand has no with-dropdown blueprint: it keeps its own class.
const MORE: Record<ButtonIconVariant, string | undefined> = {
  bare: "slds-button_icon-container-more",
  container: "slds-button_icon-container-more",
  border: "slds-button_icon-container-more slds-button_icon-border",
  "border-filled": "slds-button_icon-more",
  "border-inverse": "slds-button_icon-more",
  brand: VARIANT.brand,
  inverse: "slds-button_icon-container-more slds-button_icon-inverse",
};

export interface ButtonIconProps extends Omit<ComponentProps<"button">, "children"> {
  icon: LucideIcon;
  /** Required: becomes both the hover `title` and the `.slds-assistive-text` for screen readers. */
  label: string;
  /** Default "bare" (plain glyph, no container). */
  variant?: ButtonIconVariant;
  size?: ButtonIconSize;
  iconSize?: ButtonIconGlyphSize;
  /** "With dropdown": adds the x-small chevron glyph + aria-haspopup (a Radix trigger overrides the aria). */
  more?: boolean;
  /** Stateful button icon: `slds-is-selected` + aria-pressed. Leave undefined for a plain action. */
  selected?: boolean;
  state?: "error" | "warning";
  /** Glyph only shows full colour on hover of an ancestor with `slds-hint-parent`. */
  hint?: boolean;
}

export function ButtonIcon({
  icon: Glyph,
  label,
  variant = "bare",
  size,
  iconSize,
  more,
  selected,
  state,
  hint,
  className,
  title,
  type = "button",
  ...rest
}: ButtonIconProps) {
  const inverse = variant === "inverse" || variant === "border-inverse";
  return (
    <button
      type={type}
      title={title ?? label}
      aria-haspopup={more ? "true" : undefined}
      aria-expanded={more ? false : undefined}
      aria-pressed={selected}
      className={clsx(
        "slds-button slds-button_icon",
        more ? MORE[variant] : VARIANT[variant],
        size && `slds-button_icon-${size}`,
        state && `slds-button_icon-${state}`,
        selected && "slds-is-selected",
        className,
      )}
      {...rest}
    >
      <Glyph
        aria-hidden="true"
        className={clsx(
          "slds-button__icon",
          iconSize && `slds-button__icon_${iconSize}`,
          hint && (inverse ? "slds-button__icon_inverse-hint" : "slds-button__icon_hint"),
          "nx-lucide",
        )}
      />
      {more && <ChevronDown aria-hidden="true" className="slds-button__icon slds-button__icon_x-small nx-lucide" />}
      <span className="slds-assistive-text">{label}</span>
    </button>
  );
}
