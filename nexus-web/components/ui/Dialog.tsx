"use client";

// Dialog — Nexus Kit primitive (W1a, plan §4.2). Vendored shadcn-style trên
// @radix-ui/react-dialog, styled 100% bằng token Nexus Be (KHÔNG theme mặc định của lib):
// radius --r-card (8px), shadow --sh-dialog (multi-layer rgba 50,50,93), motion
// --dur-2/--ease-out (fade + rise 2px, entrance-only — plan §4.3), scrim bg-black/40 +
// blur nhẹ (khớp QuickCreateModal). Reduced-motion: kill-switch toàn cục ở globals.css.
// Z-scale ngữ nghĩa: dialog z-[100] (trên drawer z-50, dưới tooltip z-[120] / toast z-[200]).
//
// Dùng: <Dialog open onOpenChange><DialogContent><DialogHeader>… Radix lo focus-trap,
// aria-modal, Esc, click-outside. ConfirmDialog (3 variant) sống ở ConfirmDialog.tsx.

import * as DialogPrimitive from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { clsx } from "clsx";

export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogClose = DialogPrimitive.Close;
export const DialogPortal = DialogPrimitive.Portal;

export function DialogOverlay({
  className,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Overlay>) {
  return (
    <DialogPrimitive.Overlay
      className={clsx("nx-overlay-in fixed inset-0 z-[100] bg-black/40 backdrop-blur-[2px]", className)}
      {...props}
    />
  );
}

export function DialogContent({
  className,
  children,
  /** Ẩn nút X góc phải (mặc định hiện). */
  hideClose = false,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Content> & { hideClose?: boolean }) {
  return (
    <DialogPortal>
      <DialogOverlay />
      <DialogPrimitive.Content
        className={clsx(
          "nx-pop-in fixed left-1/2 top-1/2 z-[100] w-full max-w-[440px] -translate-x-1/2 -translate-y-1/2",
          "rounded-card border border-bd bg-panel shadow-dialog outline-none",
          className,
        )}
        {...props}
      >
        {children}
        {!hideClose && (
          <DialogPrimitive.Close
            aria-label="Đóng"
            className="absolute right-3 top-3 grid size-7 place-items-center rounded-input text-fnt transition-colors hover:bg-panel2 hover:text-fg"
          >
            <X className="size-4" strokeWidth={2} />
          </DialogPrimitive.Close>
        )}
      </DialogPrimitive.Content>
    </DialogPortal>
  );
}

export function DialogHeader({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={clsx("flex flex-col gap-1 px-5 pb-3 pt-5", className)} {...props} />;
}

export function DialogTitle({
  className,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Title>) {
  return (
    <DialogPrimitive.Title
      className={clsx("font-display pr-8 text-title font-semibold text-fg", className)}
      {...props}
    />
  );
}

export function DialogDescription({
  className,
  ...props
}: React.ComponentProps<typeof DialogPrimitive.Description>) {
  return (
    <DialogPrimitive.Description
      className={clsx("text-12-5px leading-relaxed text-mut", className)}
      {...props}
    />
  );
}

export function DialogBody({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={clsx("px-5 py-1 text-body text-fg", className)} {...props} />;
}

export function DialogFooter({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      className={clsx("flex items-center justify-end gap-2 px-5 pb-5 pt-4", className)}
      {...props}
    />
  );
}
