"use client";

// Tabs — Nexus Kit primitive (W1a, plan §4.2). Vendored trên @radix-ui/react-tabs.
// Style underline (border-b-2 + text-ac-hi active) — khớp pattern tab tự chế đang có ở
// SharingAdmin/ApprovalsInbox, nay chuẩn hoá + keyboard (←→) + aria free từ Radix.
// Token-only; active dùng text-ac-hi (chữ accent trên nền — đạt ≥4.5:1 cả dark, W0-QW11).

import * as TabsPrimitive from "@radix-ui/react-tabs";
import { clsx } from "clsx";

export const Tabs = TabsPrimitive.Root;

export function TabsList({
  className,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.List>) {
  return (
    <TabsPrimitive.List
      className={clsx("flex items-center gap-1 border-b border-bd", className)}
      {...props}
    />
  );
}

export function TabsTrigger({
  className,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Trigger>) {
  return (
    <TabsPrimitive.Trigger
      className={clsx(
        "-mb-px flex items-center gap-1.5 border-b-2 border-transparent px-3 py-2 text-12-5px font-medium text-mut outline-none transition-colors",
        "hover:text-fg data-[state=active]:border-ac data-[state=active]:text-ac-hi",
        "disabled:pointer-events-none disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}

export function TabsContent({
  className,
  ...props
}: React.ComponentProps<typeof TabsPrimitive.Content>) {
  return (
    <TabsPrimitive.Content className={clsx("pt-4 outline-none", className)} {...props} />
  );
}
