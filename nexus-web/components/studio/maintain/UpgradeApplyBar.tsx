"use client";

// UpgradeApplyBar (specs/079 f7-nang-cap-ap, FR-140): the apply bar docked to the bottom of the work column. Only the
// count sentence and the button; the question "áp nâng cấp?" opens in the page above the bar (InlineConfirm), and the
// page hides this bar meanwhile. Same sticky wrapper as UnsavedBar: the docked footer is position:fixed by SLDS, here
// it takes its own height in the flow so it never covers the last row nor slides under the assistant pane.
import { DockedFormFooter } from "@/components/slds/DockedFormFooter";
import { fill } from "@/components/slds/DataTable";
import { t } from "@/lib/i18n";

export interface UpgradeApplyBarProps {
  /** Items the chosen actions will write. */
  writes: number;
  /** Items already equal to the new pack that apply only records. */
  adoptable: number;
  applying: boolean;
  onApply: () => void;
  /** Back to the server's default choice for every item. */
  onReset: () => void;
}

export function UpgradeApplyBar({ writes, adoptable, applying, onApply, onReset }: UpgradeApplyBarProps) {
  return (
    <div className="slds-theme_default slds-border_top sticky bottom-0 z-10 [&_.slds-docked-form-footer]:static [&_.slds-docked-form-footer]:flex-wrap [&_.slds-docked-form-footer]:gap-y-1 [&_.slds-docked-form-footer_button]:whitespace-nowrap" data-testid="upgrade-apply-bar">
      <DockedFormFooter
        onCancel={onReset}
        cancelLabel={t("studio.maintain.upgrade.resetChoices")}
        onSave={onApply}
        saving={applying || writes + adoptable === 0}
        saveLabel={fill(t("studio.maintain.upgrade.applyBtn"), { n: String(writes) })}
      >
        <span role="status">
          <span>{fill(t("studio.maintain.upgrade.applyCount"), { n: String(writes) })}</span>
          {adoptable > 0 && <>{" "}<span>{fill(t("studio.maintain.upgrade.adopt"), { n: String(adoptable) })}</span></>}
        </span>
      </DockedFormFooter>
    </div>
  );
}
