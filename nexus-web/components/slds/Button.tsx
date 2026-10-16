// SLDS button (blueprint `buttons` base + with-icon, SLDS 2.264.1). Markup is the blueprint's
// `<button class="slds-button slds-button_<variant>">`; the sprite `<svg><use>` icons become Lucide
// glyphs carrying the same `slds-button__icon*` classes (sprite is CC BY-ND — never used).
// `stateful` wraps blueprint buttons/stateful (single label, `aria-pressed`); dual-stateful / kinetics are not wrapped (no caller yet).
import clsx from "clsx";
import type { ComponentProps } from "react";
import type { LucideIcon } from "lucide-react";

export type ButtonVariant =
  | "base"
  | "neutral"
  | "brand"
  | "outline-brand"
  | "destructive"
  | "text-destructive"
  | "success"
  | "inverse";

/** SLDS has no text-button size class; `size` sizes the glyphs (`slds-button__icon_<size>`). */
export type ButtonIconGlyphSize = "x-small" | "small" | "large";

/** Classes for `<a>`/`<Link>` rendered as a button (blueprint "… as a Link" variants). */
export function buttonClassName(variant: ButtonVariant = "neutral", opts: { stretch?: boolean } = {}) {
  return clsx("slds-button", variant !== "base" && `slds-button_${variant}`, opts.stretch && "slds-button_stretch");
}

export interface ButtonProps extends ComponentProps<"button"> {
  /** Default "neutral" (same as lightning-button). "base" = bare text-link look. */
  variant?: ButtonVariant;
  iconLeft?: LucideIcon;
  iconRight?: LucideIcon;
  /** Glyph size only — see ButtonIconGlyphSize. */
  size?: ButtonIconGlyphSize;
  /** 100% width, centred label (`slds-button_stretch`). */
  stretch?: boolean;
  /** Toggle button (`slds-button_stateful`): `selected` drives `slds-is-selected` + `aria-pressed`. */
  stateful?: boolean;
  selected?: boolean;
}

export function Button({
  variant = "neutral",
  iconLeft: Left,
  iconRight: Right,
  size,
  stretch,
  stateful,
  selected = false,
  className,
  children,
  type = "button",
  ...rest
}: ButtonProps) {
  // Blueprint with-icon/icon-only: a glyph with no label carries no left/right spacing class.
  const glyph = (side: "left" | "right") =>
    clsx("slds-button__icon", children != null && `slds-button__icon_${side}`, size && `slds-button__icon_${size}`, "nx-lucide");
  return (
    <button type={type} className={clsx(
        buttonClassName(variant, { stretch }),
        stateful && ["slds-button_stateful", selected ? "slds-is-selected" : "slds-not-selected"],
        className,
      )}
      aria-pressed={stateful ? selected : undefined}
      {...rest}
    >
      {Left && <Left aria-hidden="true" className={glyph("left")} />}
      {children}
      {Right && <Right aria-hidden="true" className={glyph("right")} />}
    </button>
  );
}
