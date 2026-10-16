// previewSession — "Xem như vai…" state of ONE browser tab (specs/064-preview-as-role-sample-data S9-2).
// No network here. The preview token + the synthetic /auth/me profile live in the tab's
// sessionStorage ONLY: localStorage is shared by every tab, so a preview token there would turn the
// builder's other tabs into the previewed role (and survive the tab). The builder's own tokens stay
// where they are; lib/api.ts getToken() prefers the preview token while one exists.
import type { AuthMe } from "./api";

export const PREVIEW_STORAGE_KEY = "nexus_preview";
const NOTICE_KEY = "nexus_preview_notice";

export interface PreviewRoleRef {
  id: string;
  name: string;
  label?: string;
}

export interface PreviewState {
  token: string;
  sessionId: string;
  role: PreviewRoleRef;
  /** ISO 8601 — the server's `expiresAt`; the banner counts down to it. */
  expiresAt: string;
  /** Where "Thoát" / expiry sends the builder (the Studio URL they started from). */
  returnTo: string;
  appId?: string | null;
  draft?: boolean;
  /** Synthetic /auth/me profile (getMe writes it here, never to the shared `nexus_user`). */
  me?: AuthMe;
}

export function getPreview(): PreviewState | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.sessionStorage.getItem(PREVIEW_STORAGE_KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as Partial<PreviewState> | null;
    return p && typeof p.token === "string" && p.token ? (p as PreviewState) : null;
  } catch {
    return null;
  }
}

export function getPreviewToken(): string | null {
  return getPreview()?.token ?? null;
}

export function setPreview(p: PreviewState): void {
  if (typeof window === "undefined") return;
  window.sessionStorage.setItem(PREVIEW_STORAGE_KEY, JSON.stringify(p));
}

export function clearPreview(): void {
  if (typeof window === "undefined") return;
  window.sessionStorage.removeItem(PREVIEW_STORAGE_KEY);
}

/**
 * Where leaving preview navigates to. Only a same-origin absolute path is honoured — the value comes
 * from sessionStorage, and `location.assign` of a tampered one ("https://…", "//host") would be an
 * open redirect carrying the builder out of the app.
 */
export function previewReturnTo(p: PreviewState | null): string {
  return safeLocalPath(p?.returnTo ?? "");
}

/**
 * Same-origin absolute path or "/studio". Browsers strip tab/CR/LF while parsing a URL, so "/\t/evil.com"
 * becomes "//evil.com": reject control chars and backslashes, then confirm the origin by parsing.
 */
export function safeLocalPath(r: string | null | undefined): string {
  if (!r || !r.startsWith("/") || r.startsWith("//") || /[\u0000-\u001f\\]/.test(r)) return "/studio";
  try {
    return new URL(r, "http://local.invalid").origin === "http://local.invalid" ? r : "/studio";
  } catch {
    return "/studio";
  }
}

/** Attach the synthetic profile to the live preview; no-op when none (a late /me must not resurrect one). */
export function setPreviewMe(me: AuthMe): void {
  const p = getPreview();
  if (p) setPreview({ ...p, me });
}

// "Leaving preview" latch. While set, lib/api.ts getToken() answers null, so NO request in the old
// page can carry the builder's token (a React Query refetch racing the navigation would otherwise run
// the preview-skinned app as the admin). In MEMORY on purpose: leaving is always a hard navigation
// (location.assign), which discards this page's module state — the latch can never outlive the page
// that set it, so it cannot get stuck (a storage-backed flag needs a "Studio mounted" cleanup that a
// missed navigation would never run).
let exiting = false;
export function beginPreviewExit(): void {
  exiting = true;
}
export function endPreviewExit(): void {
  exiting = false;
}
export function isPreviewExiting(): boolean {
  return exiting;
}

/** One-shot flag the Studio reads after a preview ended on its own (expiry / revoked) → a Toast. */
export function setPreviewNotice(kind: "ended"): void {
  if (typeof window !== "undefined") window.sessionStorage.setItem(NOTICE_KEY, kind);
}
export function takePreviewNotice(): "ended" | null {
  if (typeof window === "undefined") return null;
  const v = window.sessionStorage.getItem(NOTICE_KEY);
  if (v) window.sessionStorage.removeItem(NOTICE_KEY);
  return v === "ended" ? "ended" : null;
}
