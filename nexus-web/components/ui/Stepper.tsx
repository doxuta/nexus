"use client";

// Stepper — Nexus Kit primitive (W8, plan §4.2). Horizontal step indicator for multi-step flows
// (deploy wizard: chọn nguồn → xem trước → cài đặt). Token-only, no lib. Three per-step states:
//   • done     — accent fill + check, clickable to go back (onStepClick)
//   • current  — accent ring + number, bold label
//   • upcoming — muted number + label, not interactive
// Connector line between steps fills accent up to the current step. transition-colors only
// (respects the global reduced-motion kill-switch — no bespoke keyframes).

import { Check } from "lucide-react";
import { clsx } from "clsx";

export interface StepperStep {
  /** Short label under the node. */
  label: string;
  /** Optional one-line hint under the label. */
  hint?: string;
}

export function Stepper({
  steps,
  current,
  onStepClick,
  className,
}: {
  steps: StepperStep[];
  /** 0-based index of the active step. */
  current: number;
  /** Called with a step index when a DONE step node is clicked (go back). Omit to disable. */
  onStepClick?: (index: number) => void;
  className?: string;
}) {
  return (
    <ol className={clsx("flex items-start", className)} role="list">
      {steps.map((step, i) => {
        const done = i < current;
        const active = i === current;
        const clickable = done && !!onStepClick;
        const isLast = i === steps.length - 1;

        return (
          <li key={i} className="flex flex-1 items-start last:flex-none">
            <div className="flex min-w-0 flex-col items-center">
              <button
                type="button"
                disabled={!clickable}
                onClick={clickable ? () => onStepClick!(i) : undefined}
                aria-current={active ? "step" : undefined}
                aria-label={`Bước ${i + 1}: ${step.label}`}
                className={clsx(
                  "grid size-8 shrink-0 place-items-center rounded-full border text-12-5px font-semibold transition-colors",
                  done && "border-ac bg-ac text-acf",
                  active && "border-ac bg-acd text-ac-hi ring-2 ring-ac/25",
                  !done && !active && "border-bd bg-panel2 text-fnt",
                  clickable && "cursor-pointer hover:bg-ac2",
                  !clickable && "cursor-default",
                )}
              >
                {done ? <Check className="size-4" strokeWidth={2.5} /> : i + 1}
              </button>
              <div className="mt-1.5 max-w-[120px] text-center">
                <p
                  className={clsx(
                    "text-sub leading-tight transition-colors",
                    active ? "font-semibold text-fg" : done ? "font-medium text-fg" : "text-fnt",
                  )}
                >
                  {step.label}
                </p>
                {step.hint && <p className="mt-0.5 text-10-5px leading-tight text-fnt">{step.hint}</p>}
              </div>
            </div>
            {!isLast && (
              <div
                aria-hidden
                className={clsx(
                  "mx-1.5 mt-4 h-[2px] flex-1 rounded-full transition-colors sm:mx-3",
                  i < current ? "bg-ac" : "bg-bd",
                )}
              />
            )}
          </li>
        );
      })}
    </ol>
  );
}
