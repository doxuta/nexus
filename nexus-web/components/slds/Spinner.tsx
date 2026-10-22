"use client";

// SLDS spinner (blueprint `spinners/base`, SLDS 2.264.1):
// `<div role="status" class="slds-spinner slds-spinner_<size>"><span class="slds-assistive-text"/>
//  <div class="slds-spinner__dot-a"/><div class="slds-spinner__dot-b"/></div>`, optionally inside
// `div.slds-spinner_container` (overlay over the nearest positioned ancestor, or the viewport when fixed).
// Without `inline` the spinner is absolutely centred — give its parent `position: relative` and a height.
import clsx from "clsx";
import { t } from "@/lib/i18n";

export type SpinnerSize = "xx-small" | "x-small" | "small" | "medium" | "large";

export interface SpinnerProps {
  size?: SpinnerSize;
  /** "brand" = blue dots, "inverse" = white dots for dark backgrounds. */
  variant?: "base" | "brand" | "inverse";
  /** Back into the document flow (`slds-spinner_inline`). */
  inline?: boolean;
  /** Show only after 300ms (`slds-spinner_delayed`) — avoids a flash on fast loads. */
  delayed?: boolean;
  /** Wrap in the overlay container; "fixed" covers the viewport. */
  container?: boolean | "fixed";
  /** Assistive text; default "Đang tải". */
  label?: string;
  className?: string;
}

export function Spinner({ size = "medium", variant = "base", inline, delayed, container, label, className }: SpinnerProps) {
  const spinner = (
    <div
      role="status"
      className={clsx(
        "slds-spinner",
        `slds-spinner_${size}`,
        variant !== "base" && `slds-spinner_${variant}`,
        inline && "slds-spinner_inline",
        delayed && "slds-spinner_delayed",
        !container && className,
      )}
    >
      {/* The server renders Vietnamese (it cannot know ?lang or the browser's language, lib/i18n getLocale); a page
          that is still loading is server-rendered with this spinner, so its text may differ once, harmlessly. */}
      <span className="slds-assistive-text" suppressHydrationWarning>{label ?? t("slds.spinner.loading")}</span>
      <div className="slds-spinner__dot-a" />
      <div className="slds-spinner__dot-b" />
    </div>
  );
  if (!container) return spinner;
  return <div className={clsx("slds-spinner_container", container === "fixed" && "slds-is-fixed", className)}>{spinner}</div>;
}
