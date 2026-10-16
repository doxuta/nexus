"use client";

// The "before" half of the Tự động panel's KHI card (specs/048 FE-before-save-hooks-2), on SLDS:
// `scoped-notifications` (warning) for the always-on "this runs inside every save" note and
// `radio-button-group` for "Áp dụng khi" (create / update / both — before_save only). Plugged into
// WorkflowBuilder under the trigger radio group; owns no state.
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { ScopedNotification } from "@/components/slds/ScopedNotification";
import type { BeforeTrigger, SaveOn } from "@/lib/workflowBefore";
import { t } from "@/lib/i18n";

// "" (both) has no radio value of its own on the wire — condition.saveOn is simply absent.
const BOTH = "both";

export function BeforeRuleEditor({
  trigger,
  saveOn,
  onSaveOnChange,
  saveOnError,
  mutating,
}: {
  /** The rule carries a "Cập nhật trường" — adds the note about required fields (spec Nợ 8). */
  mutating?: boolean;
  trigger: BeforeTrigger;
  saveOn: SaveOn;
  onSaveOnChange: (saveOn: SaveOn) => void;
  /** Server refusal of condition.saveOn (WORKFLOW_SAVE_ON_INVALID). */
  saveOnError?: string;
}) {
  return (
    <div className="slds-m-top_medium flex flex-col gap-3">
      <ScopedNotification theme="warning">{t(`workflow.before.syncWarning.${trigger}`)}</ScopedNotification>
      {mutating && <ScopedNotification theme="info">{t("workflow.before.mutate.requiredNote")}</ScopedNotification>}
      {trigger === "before_save" && (
        <RadioButtonGroup
          label={t("workflow.before.saveOn.label")}
          options={[
            { value: BOTH, label: t("workflow.before.saveOn.both") },
            { value: "create", label: t("workflow.before.saveOn.create") },
            { value: "update", label: t("workflow.before.saveOn.update") },
          ]}
          value={saveOn || BOTH}
          onValueChange={(v) => onSaveOnChange(v === BOTH ? "" : (v as SaveOn))}
          error={saveOnError}
        />
      )}
    </div>
  );
}
