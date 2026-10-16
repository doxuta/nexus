// Hand-over of a "describe it in words" brief from /studio/moi to the assistant of the new project (specs/073 T526, FR-103).
// The brief lives in sessionStorage under one key, tagged with the slug it was written for: another project never reads it,
// and reading it removes it. Never in the URL. Every storage access is guarded (private mode / quota must not break creation).
export const PENDING_BRIEF_KEY = "nexus.studio.pendingBrief";

/** F4 `ai4` flips this once the assistant actually reads the brief; until then "Tả bằng lời" is hidden in the new-project form. */
export const assistantReadsBrief = false;

export function savePendingBrief(p: { slug: string; brief: string }): void {
  try {
    sessionStorage.setItem(PENDING_BRIEF_KEY, JSON.stringify({ slug: p.slug, brief: p.brief }));
  } catch {
    /* storage unavailable: the brief is lost, the project is still created */
  }
}

/** Returns the brief only for the matching slug, and removes it on every read (a mismatch is removed too: it is stale). */
export function takePendingBrief(slug: string): string | null {
  try {
    const raw = sessionStorage.getItem(PENDING_BRIEF_KEY);
    if (raw === null) return null;
    sessionStorage.removeItem(PENDING_BRIEF_KEY);
    const v = JSON.parse(raw) as { slug?: unknown; brief?: unknown };
    return v.slug === slug && typeof v.brief === "string" ? v.brief : null;
  } catch {
    return null;
  }
}

export function clearPendingBrief(): void {
  try {
    sessionStorage.removeItem(PENDING_BRIEF_KEY);
  } catch {
    /* nothing to clear */
  }
}
