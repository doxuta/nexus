"use client";

// SLDS progress indicator (blueprints `progress-indicator/base` + `/vertical`, SLDS 2.264.1).
// Horizontal:
//   div.slds-progress[.slds-progress_shade] > ol.slds-progress__list
//     > li.slds-progress__item[.slds-is-completed|.slds-is-active|.slds-has-error]
//       > div.slds-progress__marker[.slds-progress__marker_icon][tabindex=0] > [svg.slds-icon.slds-icon_xx-small] + span.slds-assistive-text
//   + div.slds-progress-bar.slds-progress-bar_x-small[role=progressbar]   (ProgressBar, value = current / (n - 1))
// Each marker is focusable and names its step in a tooltip (blueprint "Tooltip", top/centre) —
// Radix owns hover/focus/Esc via the Tooltip wrapper.
// Vertical (docs: "steps are not interactive"): markers are plain div / icon span (no tabindex, no
// tooltip), the step name sits beside them in div.slds-progress__item_content (an optional weak
// description line goes under the name, the content being free text in the blueprint), and the bar is a bare
// div[role=progressbar] carrying only assistive text. It also gets an aria-label, which the
// blueprint omits (an unnamed progressbar fails axe).
// Completed/error glyphs are Lucide CircleCheck / CircleX (sprite success/error are CC BY-ND).
import clsx from "clsx";
import { CircleCheck, CircleX } from "lucide-react";
import type { ReactNode } from "react";
import { t } from "@/lib/i18n";
import { ProgressBar } from "./ProgressBar";
import { Tooltip } from "./Tooltip";

export interface ProgressStep {
  id: string;
  label: string;
  /** Vertical only: a weak second line under the name (who acts on the step, its deadline…). */
  description?: ReactNode;
  /** Explicit state, for a path that is not a straight line (a branch not taken stays `upcoming`). */
  state?: "completed" | "active" | "error" | "upcoming";
}

export interface ProgressIndicatorProps {
  steps: ProgressStep[];
  /** 0-based index of the current step; steps before it are completed. steps.length = all done. */
  current: number;
  /** The current step failed (`slds-has-error` instead of `slds-is-active`). */
  error?: boolean;
  vertical?: boolean;
  /** `slds-progress_shade`: for a grey background. */
  shade?: boolean;
  /** Vertical only: green success checks (`slds-progress__marker_icon-success`). */
  success?: boolean;
  /** Accessible name of the progress bar; default "Tiến độ". */
  label?: string;
  className?: string;
}

type State = "completed" | "active" | "error" | "upcoming";

const ITEM_CLASS: Record<State, string | undefined> = {
  completed: "slds-is-completed",
  active: "slds-is-active",
  error: "slds-has-error",
  upcoming: undefined,
};

const STATE_LABEL = {
  completed: "slds.progressIndicator.completed",
  active: "slds.progressIndicator.active",
  error: "slds.progressIndicator.error",
} as const;

export function ProgressIndicator({ steps, current, error, vertical, shade, success, label, className }: ProgressIndicatorProps) {
  const stateOf = (i: number): State => (i < current ? "completed" : i > current ? "upcoming" : error ? "error" : "active");
  const pct = steps.length > 1 ? Math.round((Math.min(current, steps.length - 1) / (steps.length - 1)) * 100) : current >= steps.length ? 100 : 0;
  const barLabel = label ?? t("slds.progressBar.progress");
  return (
    <div className={clsx("slds-progress", vertical && "slds-progress_vertical", shade && "slds-progress_shade", className)}>
      <ol className="slds-progress__list">
        {steps.map((step, i) => {
          const state = step.state ?? stateOf(i);
          return (
            <li key={step.id} className={clsx("slds-progress__item", ITEM_CLASS[state])}>
              {vertical ? (
                <>
                  <VerticalMarker state={state} success={success} />
                  <div className="slds-progress__item_content slds-grid slds-grid_align-spread">
                    {step.description ? (
                      <div>
                        {step.label}
                        <p className="slds-text-body_small slds-text-color_weak">{step.description}</p>
                      </div>
                    ) : (
                      step.label
                    )}
                  </div>
                </>
              ) : (
                <HorizontalMarker state={state} label={step.label} />
              )}
            </li>
          );
        })}
      </ol>
      {vertical ? (
        <div aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} aria-label={barLabel} role="progressbar">
          <span className="slds-assistive-text">{`${t("slds.progressBar.progress")}: ${pct}%`}</span>
        </div>
      ) : (
        <ProgressBar value={pct} size="x-small" label={barLabel} />
      )}
    </div>
  );
}

function HorizontalMarker({ state, label }: { state: State; label: string }) {
  const Glyph = state === "completed" ? CircleCheck : state === "error" ? CircleX : null;
  const text = state === "upcoming" ? label : `${label} - ${t(STATE_LABEL[state])}`;
  const marker = (
    <div className={clsx("slds-progress__marker", Glyph && "slds-progress__marker_icon")} tabIndex={0}>
      {Glyph && <Glyph aria-hidden="true" className="slds-icon slds-icon_xx-small nx-lucide" />}
      <span className="slds-assistive-text">{text}</span>
    </div>
  );
  return (
    <Tooltip side="top" align="center" trigger={marker}>
      {label}
    </Tooltip>
  );
}

function VerticalMarker({ state, success }: { state: State; success?: boolean }) {
  if (state === "upcoming") return <div className="slds-progress__marker" />;
  if (state === "active") {
    return (
      <div className="slds-progress__marker">
        <span className="slds-assistive-text">{t(STATE_LABEL.active)}</span>
      </div>
    );
  }
  const done = state === "completed";
  const Glyph = done ? CircleCheck : CircleX;
  const text = t(STATE_LABEL[state]);
  return (
    <span
      className={clsx(
        "slds-icon_container slds-progress__marker slds-progress__marker_icon",
        done ? "slds-icon-utility-success" : "slds-icon-utility-error",
        done && success && "slds-progress__marker_icon-success",
      )}
      title={text}
    >
      <Glyph aria-hidden="true" className="slds-icon slds-icon_xx-small nx-lucide" />
      <span className="slds-assistive-text">{text}</span>
    </span>
  );
}
