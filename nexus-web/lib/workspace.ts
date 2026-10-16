// workspace — "Đang dựng cho <khách>" state of ONE browser tab (specs/062-customer-workspace L2, D-10).
// No network here (lib/api.ts owns open/resume/close). The builder's workspace-session token + the
// session facts + the synthetic /auth/me profile live in the tab's sessionStorage ONLY: localStorage
// is shared by every tab (and survives it), so a session token there would turn the builder's other
// tabs — and the next person to sit at this browser — into an admin of the customer.
// lib/api.ts getToken() answers this token first while a state exists; the home token is only ever
// used to resume the session (getHomeToken()).
import type { AuthMe } from "./api";
import { clearDraftPreview } from "./previewMode";

export const WORKSPACE_STORAGE_KEY = "nexus_workspace";
/** Fired (window) when the session ended under the tab: closed elsewhere, revoked, expired, flag removed. */
export const WORKSPACE_ENDED_EVENT = "nexus:workspace-ended";

export interface WorkspaceSession {
  id: string;
  tenantId: string;
  tenantSlug: string;
  tenantName: string;
  openedAt: string;
  /** ISO 8601 — the 8-hour hard cap of the session (the token itself expires sooner). */
  expiresAt: string;
}

export interface WorkspaceState {
  /** The session token every request carries while the tab is in the workspace (≤ 15 min). */
  accessToken: string;
  /** ISO 8601 — when THIS token expires; the banner renews it 60 s before. */
  expiresAt: string;
  /** `id` of the home user (the builder) who opened it; a different signed-in user voids the state (D-21). */
  homeUserId: string;
  session: WorkspaceSession;
  /** The workspace's /auth/me profile (getMe writes it here, never to the shared `nexus_user`). */
  me?: AuthMe;
}

const listeners = new Set<() => void>();
const notify = () => listeners.forEach((fn) => fn());

/** Subscribe to set/clear in THIS tab (sessionStorage fires no event in the tab that wrote it). */
export function subscribeWorkspace(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function getWorkspace(): WorkspaceState | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.sessionStorage.getItem(WORKSPACE_STORAGE_KEY);
    if (!raw) return null;
    const w = JSON.parse(raw) as Partial<WorkspaceState> | null;
    return w && typeof w.accessToken === "string" && w.accessToken && w.session ? (w as WorkspaceState) : null;
  } catch {
    return null;
  }
}

export function setWorkspace(w: WorkspaceState): void {
  if (typeof window === "undefined") return;
  window.sessionStorage.setItem(WORKSPACE_STORAGE_KEY, JSON.stringify(w));
  notify();
}

export function clearWorkspace(): void {
  if (typeof window === "undefined") return;
  window.sessionStorage.removeItem(WORKSPACE_STORAGE_KEY);
  clearDraftPreview(); // a draft-preview tab never outlives the session it was opened in (specs/063 S7-3)
  notify();
}

// "Leaving the workspace" latch (FR-014, same idea as specs/064 beginPreviewExit). Set BEFORE the state is
// cleared on every way out (session ended under the tab, "Về Nexus", logout): from then on lib/api.ts
// answers no token at all and refuses to recover a 401, so a request still in flight in the old page
// (AI Builder "Áp dụng tất cả", a React Query refetch) can never fall back to the builder's HOME token
// and write into the Nexus tenant while the page still shows the customer's. In MEMORY on purpose: leaving
// is a hard navigation that discards it; a fresh sign-in in the same page lifts it (login/verifyMfa/ssoExchange).
let leaving = false;
export function beginWorkspaceLeave(): void {
  leaving = true;
}
export function endWorkspaceLeave(): void {
  leaving = false;
}
export function isWorkspaceLeaving(): boolean {
  return leaving;
}

const NOTICE_KEY = "nexus_workspace_notice";

/** One-shot: the customer's name, left for the page the tab lands on after a session ended under it (→ one Toast). */
export function setWorkspaceEndedNotice(tenantName: string): void {
  if (typeof window !== "undefined") window.sessionStorage.setItem(NOTICE_KEY, tenantName);
}
export function takeWorkspaceEndedNotice(): string | null {
  if (typeof window === "undefined") return null;
  const v = window.sessionStorage.getItem(NOTICE_KEY);
  if (v !== null) window.sessionStorage.removeItem(NOTICE_KEY);
  return v;
}

/** True while this tab builds inside a customer workspace — the one switch for "same tab" links (D-20). */
export function inWorkspace(): boolean {
  return getWorkspace() !== null;
}

/**
 * Attributes for an <a> that opens ANOTHER NEXUS PAGE in a new tab (D-20, FR-017a). A `noopener` tab
 * does not inherit this tab's sessionStorage, so it would run as the builder's HOME login — "Người
 * dùng" on a customer's workshop tree would create a user in Nexus. In a workspace session: none (same
 * tab, banner and session intact). Every such link in the app goes through this (review rule T101).
 */
export function newTabProps(): { target?: "_blank"; rel?: string } {
  return inWorkspace() ? {} : { target: "_blank", rel: "noopener noreferrer" };
}

/** Name of the customer the tab is building in, "" outside a session. */
export function workspaceTenantName(): string {
  return getWorkspace()?.session.tenantName ?? "";
}
