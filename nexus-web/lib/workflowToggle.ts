// lib/workflowToggle.ts — switch ONE rule on/off from a list row (specs/074 lát f2-rule-toggle-error, used by the table page's related column).
// Goes through PATCH /admin/workflows/:id/active (no actions resent), so a rule holding a pre-vault key can still be switched off.
// 409 / 422 come back as a sentence the row can show (with a link to the rule), never as a thrown error.
import { ApiError, setWorkflowActive, type DraftState } from "@/lib/api";
import { errorMessage } from "@/lib/errorMessages";
import { t } from "@/lib/i18n";

export type ToggleResult = { ok: true; isActive: boolean } | { ok: false; message: string; needsOpen: boolean };

export async function toggleRuleActive(id: string, next: boolean): Promise<ToggleResult> {
  try {
    const wf = await setWorkflowActive(id, next);
    return { ok: true, isActive: wf.isActive ?? next };
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) return { ok: false, message: t("studio.build.automation.toggle.legacySecret"), needsOpen: true };
    if (e instanceof ApiError && e.status === 422) return { ok: false, message: t("studio.build.automation.toggle.invalid"), needsOpen: true };
    return { ok: false, message: errorMessage(e), needsOpen: false };
  }
}

// Rule on/off routing for a delivered workspace (specs/074 lát f2-draft-labels-rules T348, specs/063 FR-017).
// Switching ON is a draft edit (PUT, customer sees it at Publish). Switching OFF a rule the customer is running must
// stop it NOW — that is `disableWorkflowNow`, not a draft that leaves the rule running until Publish.

export function switchOffNeedsDisableNow(rule: { draftState?: DraftState; isActive?: boolean }): boolean {
  return !!rule.isActive && (rule.draftState === "live" || rule.draftState === "edited");
}
