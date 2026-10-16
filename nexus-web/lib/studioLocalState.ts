// lib/studioLocalState.ts — what the Studio keeps in the browser that names a customer or a project (specs/073 FR-087).
// A shared machine would otherwise show the next person the codes and names the previous operator worked on. Wiped on
// sign-out and on every fresh sign-in (lib/api.ts, beside clearWorkspace). The one thing that stays is the per-user
// "recent projects" list: it is keyed by the home user id and holds only {slug, name} (studioRecentProjects).
// Deliberate trade-off (FR-087): convenience over secrecy for that one list — on a shared machine the codes and names of
// up to 8 customers of the previous operator stay readable in localStorage (devtools, same-origin script), though the UI
// of another operator never shows them (different key).
// Prefix match, not a fixed list: a Studio key added later is covered without remembering to add it here.

import { clearPendingBrief } from "./studioAssistantBrief";

const STUDIO_PREFIX = "nexus.studio.";
const KEEP_PREFIX = "nexus.studio.recentProjects:";
// The remembered path per face (ModeSwitch): bare before specs/062 L2b, `:${tenantId}:${userId}` after.
const LAST_BUILDER_PREFIX = "nexus.lastPath.builder";
// Which assistant cards were applied, per project + conversation (specs/076 FR-110): names the project, so it goes too.
const AI_APPLY_PREFIX = "nexus.ai-apply-state.v1";

function sweep(store: Storage): void {
  const doomed: string[] = [];
  for (let i = 0; i < store.length; i++) {
    const k = store.key(i);
    if (k === null) continue;
    if (k.startsWith(LAST_BUILDER_PREFIX) || k.startsWith(AI_APPLY_PREFIX) || (k.startsWith(STUDIO_PREFIX) && !k.startsWith(KEEP_PREFIX))) doomed.push(k);
  }
  doomed.forEach((k) => store.removeItem(k));
}

export function clearStudioLocalState(): void {
  if (typeof window === "undefined") return;
  clearPendingBrief(); // the brief's own owner clears its key; the sweep below stays as the net for later keys
  for (const get of [() => window.localStorage, () => window.sessionStorage]) {
    try {
      sweep(get());
    } catch {
      // storage blocked: nothing was written either
    }
  }
}
