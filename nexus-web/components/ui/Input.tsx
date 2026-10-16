"use client";

// Input — Nexus Kit primitive (Phase 5b). Recipe = the hand-rolled inputs already shipped
// (CreateAppDialog/AppsManager): h-9 · rounded-input · border-bd · bg-panel · 13px ·
// placeholder --fnt. The global :focus-visible ring in globals.css handles keyboard focus;
// here we only swap border-color on focus (mouse focus included), matching current markup.
//   invalid: red border + aria-invalid — wire 422 details.field errors to it
//   mono:    font-mono 12.5px for slugs/IDs/codes (the CreateAppDialog slug recipe)
// ponytail: no size variants — every form input in the app is h-9; add one when a real
// second size shows up.

import { clsx } from "clsx";

export function Input({
  className,
  invalid = false,
  mono = false,
  ...props
}: React.ComponentProps<"input"> & { invalid?: boolean; mono?: boolean }) {
  return (
    <input
      aria-invalid={invalid || undefined}
      className={clsx(
        "h-9 w-full rounded-input border bg-panel px-3 text-fg placeholder:text-fnt focus:outline-none disabled:opacity-60",
        mono ? "font-mono text-12-5px" : "text-body",
        invalid ? "border-dg/50 focus:border-dg" : "border-bd focus:border-ac",
        className,
      )}
      {...props}
    />
  );
}
