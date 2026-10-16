import { CheckCircle2, CircleDashed } from "lucide-react";
import type { ReactNode } from "react";
import { Icon } from "@/components/slds/Icon";
import type { BuildStepKey } from "@/lib/buildSteps";
import { t } from "@/lib/i18n";
import type { I18nKey } from "@/lib/i18n/vi";
import { StatusText } from "./StatusText";

const k = (step: BuildStepKey, tail: "label" | "desc" | "criterion") => `studio.step.${step}.${tail}` as I18nKey;

/**
 * Opens each build step: the step name (an `h2` that takes focus when the page changes, FR-017) then the inferred
 * status sentence that leads the work area: "Đã đủ. {facts}" / "Chưa đủ. {facts} {criterion}" (status as coloured text).
 * `complete = null` (nothing known, e.g. the Nexus workshop) prints the step's static sentence instead. The static
 * sentence is also the tooltip, as `title=s.d` in demo A.
 * The action(s) of the step sit at the end of the line.
 */
export function StepHeader({ step, actions }: { step: { key: BuildStepKey; complete: boolean | null; sublabel?: string }; actions?: ReactNode }) {
  const sublabel = step.sublabel ? `${step.sublabel}.` : "";
  return (
    <div className="slds-media slds-media_center slds-p-around_medium">
      {step.complete !== null && (
        <div className="slds-media__figure">
          <Icon icon={step.complete ? CheckCircle2 : CircleDashed} tone={step.complete ? "success" : "default"} />
        </div>
      )}
      <div className="slds-media__body">
        <h2 className="slds-assistive-text" tabIndex={-1}>
          {t(k(step.key, "label"))}
        </h2>
        <p title={t(k(step.key, "desc"))}>
          {step.complete === null ? (
            t(k(step.key, "desc"))
          ) : (
            <>
              <StatusText tone={step.complete ? "success" : "warn"}>{t(step.complete ? "studio.step.done" : "studio.step.todo")}</StatusText>
              {". "}
              {sublabel}
              {!step.complete && <> {t(k(step.key, "criterion"))}</>}
            </>
          )}
        </p>
      </div>
      {actions && <div className="slds-media__figure slds-media__figure_reverse">{actions}</div>}
    </div>
  );
}
