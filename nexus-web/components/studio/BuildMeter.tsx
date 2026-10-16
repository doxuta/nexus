// Eight-segment build meter of a project row (specs/073 FR-091, architecture 5.x "slds-grid + màu hook"). One image with a
// spoken label; no number is printed. Each segment is an SLDS small progress bar (0 or 100%), so colour comes from SLDS.
import clsx from "clsx";
import type { BuildStepState } from "@/lib/buildSteps";
import { tf } from "@/lib/i18n";

export function BuildMeter({ steps, className }: { steps: BuildStepState[]; className?: string }) {
  const done = steps.filter((s) => s.complete === true).length;
  return (
    <div role="img" aria-label={tf("studio.projects.meter", { n: done, total: steps.length })} className={clsx("slds-grid slds-grid_vertical-align-center", className)}>
      {steps.map((s, i) => (
        <div
          key={s.key}
          data-segment={s.complete === true ? "done" : "idle"}
          aria-hidden="true"
          className={clsx("slds-progress-bar slds-progress-bar_small min-w-0 flex-1", i > 0 && "slds-m-left_xx-small")}
        >
          <span className="slds-progress-bar__value slds-progress-bar__value_success block" style={{ width: s.complete === true ? "100%" : "0%" }} />
        </div>
      ))}
    </div>
  );
}
