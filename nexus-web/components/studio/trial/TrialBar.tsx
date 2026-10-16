"use client";

// Chạy thử › "Bản nào": Bản đang chạy | Bản nháp for "Xem như", and "Xem thử bản nháp" (specs/077 f5-trial-draft-toggle).
// Shown only when the project is delivered AND has unpublished changes. The two ways never call each other:
// the toggle only feeds viewAsRole (in TrialRolesTable); the button only calls openDraftPreview.
import { useState } from "react";
import { Button } from "@/components/slds/Button";
import { RadioButtonGroup } from "@/components/slds/RadioGroup";
import { t } from "@/lib/i18n";
import { openDraftPreview, previewInWorkspace } from "@/lib/studioViewAs";
import { inWorkspace } from "@/lib/workspace";
import { usePublishState } from "../usePublishState";

export function TrialBar({ slug, app, draft, onDraftChange }: { slug: string; app: { id: string; name: string } | null; draft: boolean; onDraftChange: (draft: boolean) => void }) {
  const state = usePublishState(slug);
  const [problem, setProblem] = useState(false);
  const s = state.data;
  if (!app || !s?.delivered || s.pendingCount <= 0 || (inWorkspace() && !previewInWorkspace)) return null;

  function open() {
    setProblem(false);
    try {
      openDraftPreview({ appSlug: app!.name });
    } catch {
      setProblem(true);
    }
  }

  return (
    <section className="slds-m-top_medium" aria-labelledby="trial-draft-h">
      <h2 id="trial-draft-h" className="slds-text-heading_small slds-m-bottom_x-small">{t("studio.build.trial.draft.title")}</h2>
      <div className="flex flex-wrap items-end gap-4">
        <div>
          <RadioButtonGroup
            label={t("studio.build.trial.draft.title")}
            labelHidden
            options={[
              { value: "live", label: t("studio.build.trial.draft.live") },
              { value: "draft", label: t("studio.build.trial.draft.draft") },
            ]}
            value={draft ? "draft" : "live"}
            onValueChange={(v) => onDraftChange(v === "draft")}
          />
          <p data-testid="trial-draft-explain-role" className="slds-text-body_small slds-text-color_weak slds-m-top_xx-small">{t("studio.build.trial.draft.explainRole")}</p>
        </div>
        <div>
          <Button variant="neutral" onClick={open}>{t("studio.build.trial.draft.open")}</Button>
          <p data-testid="trial-draft-explain-tab" className="slds-text-body_small slds-text-color_weak slds-m-top_xx-small">{t("studio.build.trial.draft.explainTab")}</p>
        </div>
      </div>
      {problem && <p role="alert" className="slds-text-color_error slds-text-body_small">{t("studio.build.trial.draft.err")}</p>}
    </section>
  );
}
