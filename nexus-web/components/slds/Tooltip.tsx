"use client";

// SLDS tooltip (blueprint `tooltips/base`, SLDS 2.264.1) on @radix-ui/react-tooltip.
// Radix owns hover-with-delay, open on keyboard focus, Esc, one-at-a-time and the trigger's
// aria-describedby. Its content is rendered `asChild`, so the bubble is the blueprint's
//   div.slds-popover.slds-popover_tooltip.slds-nubbin_* > div.slds-popover__body
// Deviation forced by Radix: role="tooltip" (and the id the trigger points at) sit on Radix's
// visually-hidden text copy, which it renders inside the bubble; the visible body is aria-hidden so
// the text is announced once. Not portaled, like Menu/Popover (stays above an open Modal).
import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import type { ReactElement } from "react";
import { Nubbed, type Align, type Side } from "./Popover";

export interface TooltipProps {
  /** Focusable element the tooltip describes (button, link, icon button). */
  trigger: ReactElement;
  /** Tooltip text — plain text, as SLDS tooltips are. */
  children: string;
  /** Placement relative to the trigger; the nubbin points back at it. Blueprint default: top/start. */
  side?: Side;
  align?: Align;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
}

export function Tooltip({ trigger, children, side = "top", align = "start", open, defaultOpen, onOpenChange }: TooltipProps) {
  return (
    <TooltipPrimitive.Provider>
      <TooltipPrimitive.Root open={open} defaultOpen={defaultOpen} onOpenChange={onOpenChange}>
        <TooltipPrimitive.Trigger asChild>{trigger}</TooltipPrimitive.Trigger>
        <TooltipPrimitive.Content asChild side={side} align={align} sideOffset={12} aria-label={children}>
          <Nubbed as="div" sizeVar="--radix-tooltip-trigger" className="slds-popover slds-popover_tooltip">
            <div className="slds-popover__body" aria-hidden="true">
              {children}
            </div>
          </Nubbed>
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Root>
    </TooltipPrimitive.Provider>
  );
}
