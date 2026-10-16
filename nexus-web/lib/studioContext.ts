"use client";

// lib/studioContext.ts — the seam between the Studio project frame and the browser's session state (specs/073 kh1a).
// The frame never reads sessionStorage / localStorage / lib/api itself: it asks here, and the build-session slice
// (ph1a) changes the bodies marked "ph1a" below without touching a component.
//
// Identity rule (FR-026): what the TAB stores (nexus_tenant, nexus_workspace) is a hint, not proof of which tenant
// the token in hand belongs to — another tab can overwrite nexus_tenant, and storage can be blocked. Only GET /auth/me,
// answered for the very token we send, is evidence. useStudioIdentity() therefore says "matches" only after the server
// answered, and stays closed on loading, error, or storage that cannot be read.
import { useQuery } from "@tanstack/react-query";
import type { QueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState, useSyncExternalStore } from "react";
import { closeWorkspace, endPreviewSession, getMe, openWorkspace } from "@/lib/api";
import { clearPreview, getPreview, safeLocalPath } from "@/lib/previewSession";
import { currentProjectSlug, projectTokenMatches, safeNext, type TokenIdentity } from "@/lib/studioRoutes";
import { beginWorkspaceLeave, clearWorkspace, getWorkspace, subscribeWorkspace, WORKSPACE_STORAGE_KEY } from "@/lib/workspace";

// ponytail: the home tenant's code is not in any response; nexus_tenant is empty after a single sign-on login or
// when storage is blocked. Ceiling: wrong if the home tenant is not called "northwind". Upgrade: GET /auth/me returns the slug.
// Only ever used for a user /auth/me confirms as the operator at home (projectTokenMatches).
export const HOME_SLUG_FALLBACK = "northwind";

let home: string | undefined;

/** The home project's code, read ONCE per page load (a change made from another tab reloads the page instead). Never "". */
export function homeSlug(): string {
  if (home !== undefined) return home;
  try {
    home = (window.localStorage.getItem("nexus_tenant") ?? "").trim().toLowerCase() || HOME_SLUG_FALLBACK;
  } catch {
    home = HOME_SLUG_FALLBACK;
  }
  return home;
}

export function resetStudioContextForTests(): void {
  home = undefined;
}

/** false when sessionStorage cannot be read: then "no session" would be a guess, not a fact. */
export function sessionReadable(): boolean {
  try {
    window.sessionStorage.getItem(WORKSPACE_STORAGE_KEY);
    return true;
  } catch {
    return false;
  }
}

export interface WorkspaceView {
  tenantSlug: string;
  tenantName: string;
}

/** The build session this tab holds, null when none (or unreadable: check sessionReadable()). */
export function workspaceOf(): WorkspaceView | null {
  const s = getWorkspace()?.session;
  return s ? { tenantSlug: s.tenantSlug, tenantName: s.tenantName } : null;
}

function readSessionRaw(): string | null {
  try {
    return window.sessionStorage.getItem(WORKSPACE_STORAGE_KEY);
  } catch {
    return null; // unreadable storage: the identity gate (sessionReadable) is what refuses to guess, not the label
  }
}

/**
 * The build session of this tab, live: null on the server and on the first client render (no hydration mismatch), the
 * real state right after, and updated when the session is set or cleared in this tab. `homeUserId` is the operator the
 * session belongs to (the key of their "recent projects").
 */
export function useWorkspaceView(): (WorkspaceView & { homeUserId: string }) | null {
  const raw = useSyncExternalStore(subscribeWorkspace, readSessionRaw, () => null);
  return useMemo(() => {
    const w = raw ? getWorkspace() : null;
    return w ? { tenantSlug: w.session.tenantSlug, tenantName: w.session.tenantName, homeUserId: w.homeUserId } : null;
  }, [raw]);
}

/** The project the token in this tab belongs to by its own storage: session slug, else the home project. */
export function activeProjectSlug(): string {
  return workspaceOf()?.tenantSlug ?? homeSlug();
}

/** The project a link written in a shared builder points at: the one in the URL, else this tab's (client only). */
export function projectSlugAt(pathname: string | null): string {
  return currentProjectSlug(pathname ?? "") ?? activeProjectSlug();
}

// ── Context switch freeze (FR-085) ──
// While a switch/exit is in flight the work surface is UNMOUNTED (not just hidden): a request still in the air, an
// autosave or a refetch would otherwise run with whatever token getToken() answers between "session cleared" and
// "page left" — the home token under customer A's screen. Three phases: idle · running (spinner) · failed (the
// frame shows "Thử lại" / "Về danh sách dự án", both full loads; the old work surface is never mounted again).
type Phase = "idle" | "running" | "failed";
let phase: Phase = "idle";
const phaseListeners = new Set<() => void>();
const setPhase = (p: Phase) => {
  phase = p;
  phaseListeners.forEach((fn) => fn());
};
export const beginTransition = () => setPhase("running");
export const endTransition = () => setPhase("idle");
export const transitionPhase = (): Phase => phase;
function usePhase(): Phase {
  return useSyncExternalStore(
    (fn) => {
      phaseListeners.add(fn);
      return () => phaseListeners.delete(fn);
    },
    () => phase,
    () => "idle" as Phase,
  );
}
/** true while the work surface must not be mounted (running or failed). */
export const useStudioTransition = (): boolean => usePhase() !== "idle";
export const useStudioTransitionFailed = (): boolean => usePhase() === "failed";

// ── Build sessions: enter / exit / switch (FR-020, FR-021, FR-085, FR-086) ──
// Every way of changing which project this tab builds for goes through here, in this order:
//   confirm unsaved changes (a "no" touches nothing) → freeze + leave latch (synchronously, before any await: from
//   here no request in the page carries any token) → cancel queries → end a view-as-role tab → close A → forget A →
//   open B → clear the cache → load the page afresh (never router.push: the heap would keep A's data).
// A failure after A was closed stays frozen: the old surface cannot be re-mounted on a token that no longer exists.
export interface SessionDeps {
  qc: QueryClient;
  /** NavigationBlockerProvider.confirmLeave: false = the user kept their unsaved changes. */
  confirmLeave: () => boolean;
}

/**
 * Ends a "view as role" tab state before anything else (FR-086, specs/064 FR-016/013): the preview is ended on the
 * server with its own token, forgotten, and the page reloaded — no request carries the builder's token meanwhile.
 * true = a reload is on its way and the caller must stop.
 */
export function endPreviewIfAny(): boolean {
  if (!getPreview()) return false;
  void endPreviewSession("left").then(() => window.location.reload());
  return true;
}

/** "Về danh sách dự án" / "Chuyển sang xưởng Nexus": close the session, then load `next` as the home project. */
export async function exitProject(next: string, { qc, confirmLeave }: SessionDeps): Promise<void> {
  if (!confirmLeave()) return;
  beginTransition();
  beginWorkspaceLeave();
  if (endPreviewIfAny()) return;
  try {
    await qc.cancelQueries();
    await closeWorkspace();
    clearWorkspace();
    clearPreview();
    qc.clear();
    window.location.assign(safeLocalPath(next));
  } catch (e) {
    setPhase("failed");
    throw e;
  }
}

/**
 * "Mở dự án": no session to close, so there is nothing to freeze — if opening fails nothing has changed and the page
 * that asked stays usable and says why. Once the session is written the latch stops every request until the page loads.
 */
export async function enterProject(slug: string, next: string, { qc, confirmLeave }: SessionDeps): Promise<void> {
  if (!confirmLeave()) return;
  if (endPreviewIfAny()) return;
  await openWorkspace(slug);
  beginWorkspaceLeave();
  await qc.cancelQueries();
  qc.clear();
  window.location.assign(safeNext(next, slug));
}

/** "Chuyển sang dự án B": FR-085 order, close A before opening B. */
export async function switchProject(slug: string, next: string, { qc, confirmLeave }: SessionDeps): Promise<void> {
  if (!confirmLeave()) return;
  const homeId = getWorkspace()?.homeUserId; // read now: after the latch even /auth/me cannot be asked
  beginTransition();
  beginWorkspaceLeave();
  if (endPreviewIfAny()) return;
  try {
    await qc.cancelQueries();
    await closeWorkspace();
    clearWorkspace();
    clearPreview();
    await openWorkspace(slug, homeId);
    qc.clear();
    window.location.assign(safeNext(next, slug));
  } catch (e) {
    setPhase("failed");
    throw e;
  }
}

/**
 * The only way out of an identity/context change (FR-025c): cancel what is in flight, drop the whole cache (customer
 * A's 2-minute cache must not be readable in the Nexus workshop under the same ["entities"] key), then load the page
 * afresh — never router.push/replace, which would keep the JS heap.
 */
export async function clearAndAssign(qc: QueryClient, href: string): Promise<void> {
  await qc.cancelQueries();
  qc.clear();
  window.location.assign(href);
}

export interface StudioIdentity {
  status: "loading" | "error" | "ready";
  me: TokenIdentity | undefined;
  home: string;
  /** false: sessionStorage threw, so the session state cannot be trusted and nothing matches. */
  sessionReadable: boolean;
  /** projectTokenMatches for S, with all of the above applied. */
  matches: (S: string) => boolean;
  retry: () => void;
}

/** GET /auth/me for the token in this tab, once per page (no cache across loads: identity must be fresh). */
export function useStudioIdentity(opts: { enabled?: boolean } = {}): StudioIdentity {
  const enabled = opts.enabled ?? true;
  const [readable] = useState(sessionReadable);
  const q = useQuery({
    queryKey: ["studio", "identity"],
    queryFn: getMe,
    enabled,
    staleTime: Infinity,
    retry: false,
    placeholderData: undefined,
  });
  const status = q.isError ? "error" : q.data ? "ready" : "loading";
  const me = q.data as TokenIdentity | undefined;
  return {
    status,
    me,
    home: homeSlug(),
    sessionReadable: readable,
    matches: (S) => status === "ready" && readable && projectTokenMatches(S, me, homeSlug()),
    retry: () => void q.refetch(),
  };
}

/** The "sub" claim of a JWT, undefined when the value is not a decodable token. */
function tokenSubject(jwt: string | null): string | undefined {
  try {
    const sub = JSON.parse(atob((jwt ?? "").split(".")[1].replace(/-/g, "+").replace(/_/g, "/"))).sub;
    return sub == null ? undefined : String(sub);
  } catch {
    return undefined;
  }
}

/**
 * Storage event handler: the identity changed in another tab, so reload and let the gate read it again.
 * A silent refresh rewrites nexus_token with the SAME user (lib/api.ts setToken), so a token change counts only
 * when it is cleared (sign-out) or belongs to another user. nexus_workspace is not listened to: it lives in
 * sessionStorage, which never fires `storage` across tabs.
 */
export function onIdentityStorage(e: StorageEvent, reload: () => void = () => window.location.reload()): void {
  if (e.key === "nexus_tenant") {
    if (e.newValue !== e.oldValue) reload();
  } else if (e.key === "nexus_token") {
    if (!e.newValue) reload();
    else {
      const was = tokenSubject(e.oldValue);
      const now = tokenSubject(e.newValue);
      if (was !== undefined && now !== undefined && was !== now) reload();
    }
  }
}

/** Mount once in the frame. */
export function useReloadOnIdentityStorage(): void {
  useEffect(() => {
    const fn = (e: StorageEvent) => onIdentityStorage(e);
    window.addEventListener("storage", fn);
    return () => window.removeEventListener("storage", fn);
  }, []);
}
