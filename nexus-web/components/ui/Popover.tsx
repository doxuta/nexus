"use client";

// Popover — Nexus Kit primitive (W1a, plan §4.2). Vendored trên @radix-ui/react-popover,
// token-only: panel + border-bd + shadow-pop + radius --r-card, motion --dur-2 fade+rise.
// Nền cho Combobox / cardinality picker / filter row (các wave sau). Z: z-[90] — trên
// drawer (z-50), dưới dialog (z-[100]).

import * as PopoverPrimitive from "@radix-ui/react-popover";
import { clsx } from "clsx";

export const Popover = PopoverPrimitive.Root;
export const PopoverTrigger = PopoverPrimitive.Trigger;
export const PopoverAnchor = PopoverPrimitive.Anchor;
export const PopoverClose = PopoverPrimitive.Close;

export function PopoverContent({
  className,
  align = "center",
  sideOffset = 6,
  collisionPadding = 8,
  ...props
}: React.ComponentProps<typeof PopoverPrimitive.Content>) {
  return (
    <PopoverPrimitive.Portal>
      <PopoverPrimitive.Content
        align={align}
        sideOffset={sideOffset}
        collisionPadding={collisionPadding}
        className={clsx(
          "nx-pop-in z-[90] w-72 rounded-card border border-bd bg-panel p-3 shadow-pop outline-none",
          className,
        )}
        {...props}
      />
    </PopoverPrimitive.Portal>
  );
}
