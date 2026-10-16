"use client";

// DropdownMenu — Nexus Kit primitive (W1a, plan §4.2). Vendored trên
// @radix-ui/react-dropdown-menu, token-only (panel/bd/shadow-pop, radius --r-card,
// motion --dur-2). Thay dần các menu tự chế (density menu LayoutList, user-menu Topbar…)
// ở các wave sau — W1a chỉ đặt primitive. Z-[90] cùng tầng Popover.

import * as DropdownMenuPrimitive from "@radix-ui/react-dropdown-menu";
import { Check } from "lucide-react";
import { clsx } from "clsx";

export const DropdownMenu = DropdownMenuPrimitive.Root;
export const DropdownMenuTrigger = DropdownMenuPrimitive.Trigger;
export const DropdownMenuGroup = DropdownMenuPrimitive.Group;
export const DropdownMenuSub = DropdownMenuPrimitive.Sub;
export const DropdownMenuSubTrigger = DropdownMenuPrimitive.SubTrigger;
export const DropdownMenuSubContent = DropdownMenuPrimitive.SubContent;

export function DropdownMenuContent({
  className,
  sideOffset = 6,
  collisionPadding = 8,
  ...props
}: React.ComponentProps<typeof DropdownMenuPrimitive.Content>) {
  return (
    <DropdownMenuPrimitive.Portal>
      <DropdownMenuPrimitive.Content
        sideOffset={sideOffset}
        collisionPadding={collisionPadding}
        className={clsx(
          "nx-pop-in z-[90] min-w-[180px] rounded-card border border-bd bg-panel py-1 shadow-pop outline-none",
          className,
        )}
        {...props}
      />
    </DropdownMenuPrimitive.Portal>
  );
}

export function DropdownMenuItem({
  className,
  destructive = false,
  ...props
}: React.ComponentProps<typeof DropdownMenuPrimitive.Item> & { destructive?: boolean }) {
  return (
    <DropdownMenuPrimitive.Item
      className={clsx(
        "flex cursor-default select-none items-center gap-2 px-3 py-1.5 text-12-5px outline-none",
        "data-[disabled]:pointer-events-none data-[disabled]:opacity-50",
        destructive
          ? "text-dgf data-[highlighted]:bg-dgd"
          : "text-fg data-[highlighted]:bg-panel2",
        className,
      )}
      {...props}
    />
  );
}

/** Item có dấu ✓ khi checked — cho menu chọn 1 trong N (density, sort…). */
export function DropdownMenuCheckItem({
  className,
  checked = false,
  children,
  ...props
}: React.ComponentProps<typeof DropdownMenuPrimitive.Item> & { checked?: boolean }) {
  return (
    <DropdownMenuPrimitive.Item
      className={clsx(
        "flex cursor-default select-none items-center gap-2 px-3 py-1.5 text-12-5px outline-none",
        "data-[disabled]:pointer-events-none data-[disabled]:opacity-50 data-[highlighted]:bg-panel2",
        checked ? "font-medium text-ac-hi" : "text-fg",
        className,
      )}
      {...props}
    >
      {checked ? (
        <Check className="size-3.5 shrink-0" strokeWidth={2.5} />
      ) : (
        <span className="size-3.5 shrink-0" />
      )}
      {children}
    </DropdownMenuPrimitive.Item>
  );
}

export function DropdownMenuLabel({
  className,
  ...props
}: React.ComponentProps<typeof DropdownMenuPrimitive.Label>) {
  return (
    <DropdownMenuPrimitive.Label
      className={clsx("px-3 pb-1 pt-1.5 text-10-5px font-semibold uppercase tracking-wide text-fnt", className)}
      {...props}
    />
  );
}

export function DropdownMenuSeparator({
  className,
  ...props
}: React.ComponentProps<typeof DropdownMenuPrimitive.Separator>) {
  return (
    <DropdownMenuPrimitive.Separator
      className={clsx("my-1 h-px bg-bd2", className)}
      {...props}
    />
  );
}
