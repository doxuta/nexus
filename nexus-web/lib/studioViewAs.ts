// "Xem như vai…" from the Studio (specs/077 F5 f5-trial-view-as; F2 calls this too - one owner, no copies).
// Start a preview session as the role, keep it in THIS tab's sessionStorage, then become the customer app in
// the same tab. Every failure before the navigation ends the session and throws: no half-open preview.
import { endPreviewSession, startPreviewSession } from "@/lib/api";
import { clearDraftPreview, setDraftPreview } from "@/lib/previewMode";
import { clearPreview, getPreview, setPreview } from "@/lib/previewSession";
import { activeProjectSlug } from "@/lib/studioContext";
import { safeNext, studioHref } from "@/lib/studioRoutes";

// On since #400 (preview inside a customer workspace) landed; kept as the one switch the UI gates read.
export const previewInWorkspace = true;

export async function viewAsRole(a: { roleId: string; appId: string; appSlug: string; draft?: boolean }): Promise<void> {
  const S = activeProjectSlug();
  // Rebuilt from known parts, never copied from location.search/hash.
  const returnTo = safeNext(studioHref(S, "dung/chay-thu", {}, { app: a.appSlug }), S);
  clearDraftPreview(); // a leftover draft flag would make "Bản đang chạy" read the draft
  const s = await startPreviewSession(a.roleId, a.appId, a.draft);
  try {
    setPreview({ token: s.token, sessionId: s.sessionId, role: s.role, expiresAt: s.expiresAt, appId: s.appId, draft: s.draft, returnTo });
    if (getPreview()?.sessionId !== s.sessionId) throw new Error("studioViewAs.viewAsRole: preview state not stored");
  } catch (e) {
    await endPreviewSession("exit").catch(() => undefined);
    throw e;
  }
  window.location.assign(`/home?app=${encodeURIComponent(a.appSlug)}`);
}

/** "Xem thử bản nháp": this tab becomes the read-only draft preview. Never starts a session (viewAsRole's path). */
export function openDraftPreview(a: { appSlug: string }): void {
  const S = activeProjectSlug();
  const returnTo = safeNext(studioHref(S, "dung/chay-thu", {}, { app: a.appSlug }), S);
  clearPreview(); // a role-preview token left here would make the tab a role preview, not the draft one
  if (!setDraftPreview(returnTo)) throw new Error("studioViewAs.openDraftPreview: draft flag not stored");
  window.location.assign(`/home?app=${encodeURIComponent(a.appSlug)}`);
}
