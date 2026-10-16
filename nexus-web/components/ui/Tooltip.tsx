"use client";

// Tooltip — Nexus Kit primitive (W1a, plan §4.2). Vendored trên @radix-ui/react-tooltip,
// style KHỚP tooltip HelpHint cũ (border-bd bg-panel text-fg shadow-pop, 12px, max-w 240) —
// HelpHint nay ngồi TRÊN primitive này. Motion --dur-1 fade (micro 120ms, plan §4.3);
// reduced-motion: kill-switch toàn cục. Z-[120] — tầng tooltip (trên dialog z-[100]).
//
// 2 cách dùng:
//   • Cao cấp: <Tooltip content="…"><button>…</button></Tooltip> — 90% trường hợp.
//   • Thô (composition): TooltipRoot/TooltipTrigger/TooltipContent khi cần điều khiển sâu.

import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import { clsx } from "clsx";

export const TooltipProvider = TooltipPrimitive.Provider;
export const TooltipRoot = TooltipPrimitive.Root;
export const TooltipTrigger = TooltipPrimitive.Trigger;

export function TooltipContent({
  className,
  side = "bottom",
  sideOffset = 6,
  collisionPadding = 8,
  ...props
}: React.ComponentProps<typeof TooltipPrimitive.Content>) {
  return (
    <TooltipPrimitive.Portal>
      <TooltipPrimitive.Content
        side={side}
        sideOffset={sideOffset}
        collisionPadding={collisionPadding}
        className={clsx(
          "nx-tooltip-in z-[120] max-w-[240px] rounded-input border border-bd bg-panel px-2.5 py-1.5 text-sub leading-snug text-fg shadow-pop",
          className,
        )}
        {...props}
      />
    </TooltipPrimitive.Portal>
  );
}

/** Wrapper gọn cho 90% trường hợp: bọc trigger, truyền content. */
export function Tooltip({
  content,
  side = "bottom",
  delayDuration = 150,
  className,
  children,
}: {
  content: React.ReactNode;
  side?: "top" | "bottom" | "left" | "right";
  delayDuration?: number;
  className?: string;
  children: React.ReactNode;
}) {
  if (!content) return <>{children}</>;
  return (
    <TooltipProvider delayDuration={delayDuration} skipDelayDuration={300}>
      <TooltipRoot>
        <TooltipTrigger asChild>{children}</TooltipTrigger>
        <TooltipContent side={side} className={className}>
          {content}
        </TooltipContent>
      </TooltipRoot>
    </TooltipProvider>
  );
}
