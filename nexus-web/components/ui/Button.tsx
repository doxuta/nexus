"use client";

// Button — Nexus Kit primitive (Phase 5b). ONE source for every action button; the class
// recipe matches the hand-rolled canon already shipped (Login/Landing/AppsManager:
// primary = bg-ac text-acf hover:bg-ac2, secondary = border-bd bg-panel hover:bg-panel2,
// danger = bg-dg text-white hover:bg-dgf) so adoption is visually a no-op.
// No shadcn default state (design-anti-slop). Focus ring comes from the global
// :focus-visible rule in globals.css — nothing to add here.
//   variant: primary · secondary · ghost · danger
//   size:    sm (h-8, dialog/inline) · md (h-9, screen header/form)
//   loading: subtle Loader2 spinner replaces the icon slot + disables the button
//   icon:    ReactNode slot before the label — callers keep their exact size/strokeWidth

import { clsx } from "clsx";
import { Loader2 } from "lucide-react";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger";
export type ButtonSize = "sm" | "md";

const VARIANT: Record<ButtonVariant, string> = {
  primary: "bg-ac text-acf shadow-card hover:bg-ac2",
  secondary: "border border-bd bg-panel text-fg hover:bg-panel2",
  ghost: "text-mut hover:bg-panel2 hover:text-fg",
  danger: "bg-dg text-white hover:bg-dgf",
};

const SIZE: Record<ButtonSize, string> = {
  sm: "h-8 px-3.5 text-12-5px",
  md: "h-9 px-3.5 text-body",
};

export function Button({
  variant = "primary",
  size = "md",
  loading = false,
  icon,
  className,
  disabled,
  type = "button", // explicit default — a bare <button> inside a <form> submits it
  children,
  ...props
}: React.ComponentProps<"button"> & {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  /** Icon slot rendered before the label. Pass the element as-is (size/strokeWidth yours). */
  icon?: React.ReactNode;
}) {
  return (
    <button
      type={type}
      disabled={disabled || loading}
      className={clsx(
        "inline-flex shrink-0 items-center justify-center gap-1.5 rounded-input font-medium transition-colors",
        "disabled:cursor-not-allowed disabled:opacity-50",
        VARIANT[variant],
        SIZE[size],
        className,
      )}
      {...props}
    >
      {loading ? <Loader2 className="size-3.5 animate-spin" strokeWidth={2} /> : icon}
      {children}
    </button>
  );
}
