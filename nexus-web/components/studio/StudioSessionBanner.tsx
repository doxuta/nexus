"use client";

// The Studio's "Đang dựng cho: <khách> (<mã>)" strip (specs/073 ph1b FR-082, D-UI-25): the 062 WorkspaceBanner itself
// (it keeps renewing the session and is the only thing telling the builder that what is edited is not Nexus's), placed
// right under the global header, with the two things that differ in the Studio:
//   • its button reads "Về danh sách dự án" and leaves through the frozen exit of lib/studioContext (the work surface
//     is unmounted before the session is closed, FR-085) instead of the bare exitWorkspace();
//   • a session that ended under the tab (closed elsewhere, revoked, expired) freezes the surface, cancels and CLEARS the
//     query cache and loads /studio afresh (FR-025c): customer A's cached lists must not be readable in the workshop,
//     so never router.push. The toast on the landing page comes from the banner's one-shot notice.
import { useQueryClient } from "@tanstack/react-query";
import { WorkspaceBanner } from "@/components/slds/shell/WorkspaceBanner";
import { t } from "@/lib/i18n";
import { useConfirmLeave } from "@/lib/navigation-blocker";
import { beginTransition, clearAndAssign, exitProject } from "@/lib/studioContext";

export function StudioSessionBanner() {
  const qc = useQueryClient();
  const confirmLeave = useConfirmLeave();
  return (
    <WorkspaceBanner
      exitLabel={t("studio.session.backToProjects")}
      onExit={() => exitProject("/studio", { qc, confirmLeave })}
      onEnded={() => {
        beginTransition();
        void clearAndAssign(qc, "/studio");
      }}
    />
  );
}
