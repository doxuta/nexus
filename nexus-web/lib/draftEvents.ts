// A tiny in-page bus: "the set of pending drafts changed" (specs/063-draft-publish). The DraftBar
// (toolbar) and the editors (WorkflowBuilder …) are separate trees that must stay in step without
// sharing state: an editor saving a draft tells the bar to recount, and a publish / discard in the bar
// tells every editor to reload its list so the Nháp / Mới / Sẽ xoá badges are not stale.
//
// `source` names the emitter, so an editor can ignore its own announcement (it has just refreshed
// itself) while everyone else reacts.
const EVENT = "nexus:drafts-changed";

/** Announce that drafts were saved, discarded, published or switched off. */
export function emitDraftsChanged(source = ""): void {
  if (typeof window !== "undefined") window.dispatchEvent(new CustomEvent<string>(EVENT, { detail: source }));
}

// "Open the Publish review" (specs/076 FR-127): the assistant card sends the builder to the ONE review popover that
// ProjectHeader owns. Only a mounted opener handles it; `hasPublishReviewOpener` lets a card promise the button only when it works.
let openers = 0;
const OPEN_EVENT = "nexus:open-publish-review";

export function hasPublishReviewOpener(): boolean {
  return openers > 0;
}

// For useSyncExternalStore: a card re-renders when the opener appears or goes away, instead of reading it once at render.
const watchers = new Set<() => void>();
export function watchPublishReviewOpener(cb: () => void): () => void {
  watchers.add(cb);
  return () => { watchers.delete(cb); };
}

/** ProjectHeader registers while its Xuất bản button exists. */
export function onOpenPublishReview(cb: () => void): () => void {
  if (typeof window === "undefined") return () => {};
  openers++;
  watchers.forEach((w) => w());
  window.addEventListener(OPEN_EVENT, cb);
  return () => { openers--; window.removeEventListener(OPEN_EVENT, cb); watchers.forEach((w) => w()); };
}

export function openPublishReview(): boolean {
  if (typeof window === "undefined" || openers === 0) return false;
  window.dispatchEvent(new Event(OPEN_EVENT));
  return true;
}

/** Subscribe; returns the unsubscribe function (use as a useEffect cleanup). */
export function onDraftsChanged(cb: (source: string) => void): () => void {
  if (typeof window === "undefined") return () => {};
  const handler = (e: Event) => cb(String((e as CustomEvent<string>).detail ?? ""));
  window.addEventListener(EVENT, handler);
  return () => window.removeEventListener(EVENT, handler);
}
