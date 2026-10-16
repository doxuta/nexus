// Draft-preview request flag (specs/063-draft-publish). The server overlays the builder's UNPUBLISHED
// drafts on a read route shared with the customer's app ONLY when the caller is the builder AND the
// request carries X-Nexus-Draft: 1 (anyone else's header is ignored silently). Routes under
// /api/v1/admin/* overlay by themselves; this header is for the shared ones (reports, layouts, menus…)
// that later slices make draftable.
//
// ONE helper, used by req() and by every raw fetch() in lib/api.ts (except /auth/*): a raw fetch that
// forgot it would read the live version while the rest of the screen shows the draft. The header goes
// on GETs only — Studio writes real records on the Data tab, and a draft-view screen's save must never
// be tagged as a draft preview (S7-3 refuses such a write with 403 PREVIEW_READ_ONLY).
//
// Three ways a request asks for the draft:
//   • the whole of /studio (the builder's own workspace);
//   • one customer-app screen, by its own `?draft=1` query (S7-2: "Xem bản nháp" on a report or
//     dashboard). Per SCREEN, not per tab: leave the screen and you are back on the live app.
//   • one whole TAB, by `?preview=draft` ("Xem thử bản nháp", S7-3): the flag lives in THIS tab's
//     sessionStorage (never localStorage — it must not turn the builder's other tabs into previews),
//     every request of the tab carries the header on EVERY method (the server answers a write with
//     403 PREVIEW_READ_ONLY — the tab never writes for real), the banner shows and the write buttons lock.
//     The flag dies with a sign-in, a sign-out and the opening/closing of a customer workspace session.
import { useSyncExternalStore } from "react";
import { getPreview, safeLocalPath } from "./previewSession";

export const DRAFT_HEADER = "X-Nexus-Draft";

/** sessionStorage key of the draft-preview tab flag: JSON `{ returnTo }`. */
export const DRAFT_PREVIEW_KEY = "nexus_draft_preview";

const listeners = new Set<() => void>();
const notify = () => listeners.forEach((fn) => fn());


/**
 * True while THIS tab previews the draft. Opening the tab with `?preview=draft[&return=/studio/…]`
 * sets the flag the first time it is asked (every request asks first, so none escapes unmarked) and
 * removes the two params from the address bar.
 * `return` is honoured only as a same-origin absolute path — it is navigated to by "Thoát".
 */
export function inDraftPreviewTab(): boolean {
  if (typeof window === "undefined") return false;
  try {
    const q = new URLSearchParams(window.location?.search ?? "");
    if (q.get("preview") === "draft" && window.sessionStorage.getItem(DRAFT_PREVIEW_KEY) === null) {
      window.sessionStorage.setItem(DRAFT_PREVIEW_KEY, JSON.stringify({ returnTo: safeLocalPath(q.get("return")) }));
      // Consume the query: left in the URL, a sign-out or a "Thoát" followed by one more call would
      // re-arm the flag from it.
      q.delete("preview");
      q.delete("return");
      const rest = q.toString();
      window.history.replaceState(window.history.state, "", window.location.pathname + (rest ? `?${rest}` : "") + window.location.hash);
      notify();
    }
    return window.sessionStorage.getItem(DRAFT_PREVIEW_KEY) !== null;
  } catch {
    return false;
  }
}

/** Where "Thoát" goes: the Studio screen the tab was opened from, else /studio. */
export function draftPreviewReturnTo(): string {
  try {
    const raw = typeof window === "undefined" ? null : window.sessionStorage.getItem(DRAFT_PREVIEW_KEY);
    return safeLocalPath((raw ? (JSON.parse(raw) as { returnTo?: string }) : {}).returnTo ?? null);
  } catch {
    return "/studio";
  }
}

/** Write the tab flag directly (no query round-trip) and read it back; false = not stored, the caller must not navigate. */
export function setDraftPreview(returnTo: string): boolean {
  try {
    window.sessionStorage.setItem(DRAFT_PREVIEW_KEY, JSON.stringify({ returnTo: safeLocalPath(returnTo) }));
    const ok = window.sessionStorage.getItem(DRAFT_PREVIEW_KEY) !== null;
    if (ok) notify();
    return ok;
  } catch {
    return false;
  }
}

/** Drop the tab flag (sign-in, sign-out, workspace session opened/closed, "Thoát"). */
export function clearDraftPreview(): void {
  if (typeof window === "undefined") return;
  if (window.sessionStorage.getItem(DRAFT_PREVIEW_KEY) === null) return;
  window.sessionStorage.removeItem(DRAFT_PREVIEW_KEY);
  notify();
}

const subscribe = (fn: () => void) => {
  listeners.add(fn);
  return () => listeners.delete(fn);
};

/** Reactive `inDraftPreviewTab()` (false on the server and on the first client render). */
export function useDraftPreviewTab(): boolean {
  return useSyncExternalStore(subscribe, inDraftPreviewTab, () => false);
}

/**
 * True when this tab must not write: it previews a role (specs/064) or the draft (S7-3). The server
 * refuses the writes anyway (403 PREVIEW_READ_ONLY); this is what keeps the builder from being offered a
 * button that can only fail. Locking is for the record surfaces of the customer app; the server is the
 * wall.
 */
export function usePreviewReadOnly(): boolean {
  const draft = useDraftPreviewTab();
  return draft || getPreview() !== null;
}

/** True while the builder is on a /studio screen. */
export function inStudio(): boolean {
  if (typeof window === "undefined") return false;
  const p = window.location?.pathname ?? "";
  return p === "/studio" || p.startsWith("/studio/");
}

/** True while the current screen was opened with `?draft=1` ("Xem bản nháp"). */
export function onDraftScreen(): boolean {
  if (typeof window === "undefined") return false;
  return new URLSearchParams(window.location?.search ?? "").get("draft") === "1";
}

let forceDraftRead = false;
/**
 * Runs `fn` so that the GET it starts (req() builds its headers synchronously, before its first await) asks for the
 * builder's draft overlay even outside /studio. Per call, never per tab; writes are never tagged.
 */
export function withDraftRead<T>(fn: () => T): T {
  forceDraftRead = true;
  try {
    return fn();
  } finally {
    forceDraftRead = false;
  }
}

let forceLiveRead = false;
/** Runs `fn` so that the GET it starts carries NO draft header even inside /studio: the builder reads what the customer sees. */
export function withLiveRead<T>(fn: () => T): T {
  forceLiveRead = true;
  try {
    return fn();
  } finally {
    forceLiveRead = false;
  }
}

/** Extra headers a request of this HTTP method carries for the draft preview. {} for a live screen. */
export function previewHeaders(method: string): Record<string, string> {
  if (forceLiveRead && method.toUpperCase() === "GET") return {};
  if (forceDraftRead && method.toUpperCase() === "GET") return { [DRAFT_HEADER]: "1" };
  // Never inside /studio: a stale tab flag (Back button, typed URL) must not turn Studio writes into 403s.
  if (!inStudio() && inDraftPreviewTab()) return { [DRAFT_HEADER]: "1" }; // every method: the server turns a write into 403
  if (method.toUpperCase() !== "GET") return {};
  return inStudio() || onDraftScreen() ? { [DRAFT_HEADER]: "1" } : {};
}

/**
 * Address of the customer app opened as "Xem thử bản nháp" for app `appName`: the tab sets its own
 * flag from `?preview=draft` and goes back to `returnTo` on "Thoát".
 */
export function draftPreviewHref(appName: string, returnTo: string): string {
  return `/home?app=${encodeURIComponent(appName)}&preview=draft&return=${encodeURIComponent(returnTo)}`;
}
