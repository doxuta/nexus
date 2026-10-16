"use client";

// SLDS progress bar (blueprint `progress-bar` base + vertical, SLDS 2.264.1):
// `<div class="slds-progress-bar" role="progressbar" aria-valuemin/max/now aria-label>
//   <span class="slds-progress-bar__value" style="width:N%"><span class="slds-assistive-text"/></span></div>`.
// The inline width/height % is the blueprint's own markup (it is the value, not styling).
// Descriptive variant: a title row above names the bar through aria-labelledby (useId — the blueprint
// reuses one id twice; that part is not copied).
import clsx from "clsx";
import { useId } from "react";
import { t } from "@/lib/i18n";

export interface ProgressBarProps {
  /** Percent complete; clamped to 0–100. */
  value: number;
  /** Accessible name when there is no `title`; default "Tiến độ". */
  label?: string;
  /** Descriptive variant: visible title row with "Hoàn thành N%" on the right. */
  title?: string;
  /** Thickness: x-small 2px · small 4px · medium 8px · large 12px (default 1rem). */
  size?: "x-small" | "small" | "medium" | "large";
  circular?: boolean;
  vertical?: boolean;
  /** Green fill (`slds-progress-bar__value_success`). */
  success?: boolean;
  className?: string;
}

export function ProgressBar({ value, label, title, size, circular, vertical, success, className }: ProgressBarProps) {
  const id = useId();
  const pct = Math.round(Math.min(100, Math.max(0, value)));
  const bar = (
    <div
      className={clsx(
        "slds-progress-bar",
        size && `slds-progress-bar_${size}`,
        circular && "slds-progress-bar_circular",
        vertical && "slds-progress-bar_vertical",
        !title && className,
      )}
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={pct}
      aria-labelledby={title ? id : undefined}
      aria-label={title ? undefined : (label ?? t("slds.progressBar.progress"))}
    >
      <span
        className={clsx("slds-progress-bar__value", success && "slds-progress-bar__value_success")}
        style={vertical ? { height: `${pct}%` } : { width: `${pct}%` }}
      >
        <span className="slds-assistive-text">{`${t("slds.progressBar.progress")}: ${pct}%`}</span>
      </span>
    </div>
  );
  if (!title) return bar;
  return (
    <div className={className}>
      <div className="slds-grid slds-grid_align-spread slds-p-bottom_x-small" id={id}>
        <span>{title}</span>
        <span aria-hidden="true">
          <strong>{`${t("slds.progressBar.complete")} ${pct}%`}</strong>
        </span>
      </div>
      {bar}
    </div>
  );
}
