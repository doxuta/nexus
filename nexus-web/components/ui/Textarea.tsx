"use client";

// Textarea — Nexus Kit primitive (Phase 5b). Same recipe as Input, multi-line
// (the CreateAppDialog description textarea): resize-none, px-3 py-2, 13px.

import { clsx } from "clsx";

export function Textarea({
  className,
  invalid = false,
  ...props
}: React.ComponentProps<"textarea"> & { invalid?: boolean }) {
  return (
    <textarea
      aria-invalid={invalid || undefined}
      className={clsx(
        "w-full resize-none rounded-input border bg-panel px-3 py-2 text-body text-fg placeholder:text-fnt focus:outline-none disabled:opacity-60",
        invalid ? "border-dg/50 focus:border-dg" : "border-bd focus:border-ac",
        className,
      )}
      {...props}
    />
  );
}
