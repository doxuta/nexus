"use client";

// SLDS popover (blueprint `popovers/base`, SLDS 2.264.1) on @radix-ui/react-popover.
// Radix owns open/close, Esc, click-outside, focus-in and focus return to the trigger; its content
// is rendered `asChild` so the DOM is the blueprint's:
//   section.slds-popover.slds-nubbin_*[role=dialog] > button.slds-popover__close
//     + div.slds-popover__body > [div.slds-popover__meta] + div.slds-media > [figure] + div.slds-media__body > header + content
//     + footer.slds-popover__footer
// `headerOutside` is the showcase Popover's `headerTitle` form (global-header notifications tray):
//   section > button.slds-popover__close + header.slds-popover__header + div.slds-popover__body > content
// Like Menu, the content is NOT portaled (it stays in an open Modal's stacking context; Radix
// positions it `fixed`, so it still escapes overflow clipping).
// Focus: non-modal by default — Tab past the last control closes the popover instead of trapping
// (Lightning popovers do not lock page scroll); `modal` switches on Radix's focus trap.
import * as PopoverPrimitive from "@radix-ui/react-popover";
import clsx from "clsx";
import { X, type LucideIcon } from "lucide-react";
import { useId, useRef, type CSSProperties, type HTMLAttributes, type ReactElement, type ReactNode, type Ref } from "react";
import { t } from "@/lib/i18n";
import { focusFirst } from "./focus";
import { Icon } from "./Icon";

/** Side of the trigger the overlay is placed on (Radix `side`). */
export type Side = "top" | "right" | "bottom" | "left";
/** Alignment against the trigger along that side (Radix `align`). */
export type Align = "start" | "center" | "end";

const EDGE: Record<Side, Side> = { top: "bottom", bottom: "top", left: "right", right: "left" };

/**
 * Nubbin class for an overlay placed on `side`/`align` of its trigger. SLDS draws corner nubbins
 * 1.5rem in from the edge, so an edge-aligned overlay is shifted by half the trigger's size minus
 * 1.5rem to put the nubbin on the trigger's centre. `sizeVar` is the Radix CSS variable prefix
 * that carries the trigger size (e.g. "--radix-popover-trigger" → `-width` / `-height`).
 */
export function nubbin(side: Side, align: Align, sizeVar: string): { className: string; style?: CSSProperties } {
  const edge = EDGE[side];
  if (align === "center") return { className: `slds-nubbin_${edge}` };
  const across = side === "top" || side === "bottom";
  const corner = across ? (align === "start" ? "left" : "right") : align === "start" ? "top" : "bottom";
  const margin = `margin${corner[0].toUpperCase()}${corner.slice(1)}`;
  return {
    className: `slds-nubbin_${edge}-${corner}`,
    style: { [margin]: `calc(var(${sizeVar}-${across ? "width" : "height"}) / 2 - 1.5rem)` },
  };
}

type NubbedProps = HTMLAttributes<HTMLElement> & {
  as: "div" | "section";
  sizeVar: string;
  "data-side"?: Side;
  "data-align"?: Align;
  ref?: Ref<HTMLElement>;
};

/**
 * The overlay element itself. Radix (asChild) merges the PLACED `data-side`/`data-align` into its
 * props, so the nubbin follows a collision flip instead of the requested side.
 */
export function Nubbed({ as: Tag, sizeVar, className, style, ref, ...rest }: NubbedProps) {
  const n = nubbin(rest["data-side"] ?? "bottom", rest["data-align"] ?? "center", sizeVar);
  // `block` (layout): SLDS makes .slds-popover inline-block, whose baseline gap would push the
  // Radix wrapper taller than the bubble and misplace a top-side overlay.
  return <Tag {...rest} ref={ref as Ref<HTMLDivElement>} className={clsx(className, n.className, "block")} style={{ ...style, ...n.style }} />;
}

export interface PopoverProps {
  /** Button that opens the popover (SLDS: a popover must come with a triggering button). */
  trigger: ReactElement;
  /** Header title (`h2.slds-text-heading_small`); names the dialog via aria-labelledby. */
  title?: ReactNode;
  /** Accessible name when there is no title. */
  label?: string;
  /** Glyph in the media figure ("With icon"). */
  icon?: LucideIcon;
  /** Assistive text / title of the figure icon. */
  iconLabel?: string;
  /** Badge text in `slds-popover__meta` ("With Badge"). */
  badge?: ReactNode;
  /** Footer content, rendered as-is. Buttons: wrap in `div.slds-grid.slds-grid_vertical-align-center`
   *  and give the button `slds-col_bump-left` (blueprint "With Footer"); text goes in directly. */
  footer?: ReactNode;
  // ponytail: no "full-width" — slds-popover_full-width is width:100% of a containing block, and Radix's
  // popper wrapper is min-width:max-content, so it grows to one line of text. Add with a test if needed.
  size?: "small" | "medium" | "large";
  /** `slds-popover_dark`. */
  dark?: boolean;
  /** Close button in the corner (default on; "No Close Button" variant = false). */
  closeButton?: boolean;
  /** `slds-popover__body_small`: capped height, scrolls, focusable region. */
  scrollable?: boolean;
  /** Header as a direct child of the section (showcase `headerTitle`), so it stays put while a
   *  scrollable body scrolls; children then go straight into the body — no media wrapper, so
   *  `icon` is not rendered. */
  headerOutside?: boolean;
  /** Extra classes on `div.slds-popover__body` (showcase `bodyClassName`, e.g. `slds-p-around_none`). */
  bodyClassName?: string;
  /** Placement relative to the trigger; the nubbin points back at it. Blueprint default: right/center. */
  side?: Side;
  align?: Align;
  /** Trap focus inside (Radix modal popover). */
  modal?: boolean;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  className?: string;
  children: ReactNode;
}

export function Popover({
  trigger,
  title,
  label,
  icon,
  iconLabel,
  badge,
  footer,
  size,
  dark,
  closeButton = true,
  scrollable,
  headerOutside,
  bodyClassName,
  side = "right",
  align = "center",
  modal = false,
  open,
  defaultOpen,
  onOpenChange,
  className,
  children,
}: PopoverProps) {
  const id = useId();
  const headingId = `${id}-heading`;
  const bodyId = `${id}-body`;
  const ref = useRef<HTMLDivElement>(null);
  const close = t("slds.popover.close");
  const header = title != null && (
    <header className="slds-popover__header">
      <h2 className="slds-text-heading_small" id={headingId}>
        {title}
      </h2>
    </header>
  );
  return (
    <PopoverPrimitive.Root open={open} defaultOpen={defaultOpen} onOpenChange={onOpenChange} modal={modal}>
      <PopoverPrimitive.Trigger asChild>{trigger}</PopoverPrimitive.Trigger>
      <PopoverPrimitive.Content
        asChild
        ref={ref}
        side={side}
        align={align}
        sideOffset={12}
        onOpenAutoFocus={(e) => {
          // SLDS: focus the first focusable that isn't the close button; the close button otherwise.
          e.preventDefault();
          focusFirst(ref.current, "slds-popover__close");
        }}
      >
        <Nubbed
          as="section"
          sizeVar="--radix-popover-trigger"
          aria-labelledby={title != null ? headingId : undefined}
          aria-label={title != null ? undefined : label}
          aria-describedby={bodyId}
          className={clsx("slds-popover", dark && "slds-popover_dark", size && `slds-popover_${size}`, className)}
        >
          {closeButton && (
            <PopoverPrimitive.Close asChild>
              <button type="button" className="slds-button slds-button_icon slds-button_icon-small slds-float_right slds-popover__close" title={close}>
                <X className="slds-button__icon nx-lucide" aria-hidden="true" />
                <span className="slds-assistive-text">{close}</span>
              </button>
            </PopoverPrimitive.Close>
          )}
          {headerOutside && header}
          <div
            className={clsx("slds-popover__body", scrollable && "slds-popover__body_small", bodyClassName)}
            id={bodyId}
            {...(scrollable && { tabIndex: 0, role: "region" })}
          >
            {badge != null && (
              <div className="slds-popover__meta">
                <span className={clsx("slds-badge", dark && "slds-badge_lightest")}>{badge}</span>
              </div>
            )}
            {headerOutside ? (
              children
            ) : (
              <div className="slds-media">
                {icon && (
                  <div className="slds-media__figure">
                    <Icon icon={icon} size="small" label={iconLabel} />
                  </div>
                )}
                <div className="slds-media__body">
                  {header}
                  {children}
                </div>
              </div>
            )}
          </div>
          {footer != null && <footer className="slds-popover__footer">{footer}</footer>}
        </Nubbed>
      </PopoverPrimitive.Content>
    </PopoverPrimitive.Root>
  );
}
