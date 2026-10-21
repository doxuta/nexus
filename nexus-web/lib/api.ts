// API client — nối FE Studio/app vào backend Nexus thật (P5 E2E wiring).
// Base trỏ backend Go local; JWT lưu in-memory + localStorage. Mọi call qua đây (Bearer + camelCase JSON).
import { fromSlaItem, type RecordSlaItem } from "./recordSla";
import { filtersToParams, type RunFilters } from "./runFilters";
import type { WorkflowRun, WorkflowTestResult } from "./workflowRuns";
import { fromDetailPayload, fromAnyInstance, type ApprovalDetailData } from "./approvalDetail";
import { buildAiChatBody, type AiChatResult, type AiSessionSummary, type AiSessionDetail, type AiUsage, type AiTurnContextInput } from "./aiBuilder";
import type { AssistantDraft, AssistantSessionSummary, AssistantSessionDetail } from "./assistant";
import { clearDraftPreview, previewHeaders } from "./previewMode";
import {
  beginPreviewExit, clearPreview, getPreview, getPreviewToken, isPreviewExiting, previewReturnTo, safeLocalPath, setPreviewMe, setPreviewNotice,
} from "./previewSession";
import {
  WORKSPACE_ENDED_EVENT, beginWorkspaceLeave, clearWorkspace, endWorkspaceLeave, getWorkspace, isWorkspaceLeaving, setWorkspace, type WorkspaceSession, type WorkspaceState,
} from "./workspace";
import { clearStudioLocalState } from "./studioLocalState";
import { rememberProject } from "./studioRecentProjects";
import { safeNext } from "./studioRoutes";

const BASE = process.env.NEXT_PUBLIC_API_BASE || "http://localhost:8080/api/v1";

// ── Token storage ──
// nexus_token     = access token  (localStorage when rememberMe, sessionStorage otherwise)
// nexus_refresh   = refresh token (same scope as access token)
// nexus_user      = cached /auth/me payload (JSON string)
// nexus_remember  = "1" | absent — controls storage scope

let _accessToken: string | null = null;
let _refreshToken: string | null = null;

function getStorage(): Storage | null {
  if (typeof window === "undefined") return null;
  const remember = window.localStorage.getItem("nexus_remember");
  return remember === "1" ? window.localStorage : window.sessionStorage;
}

// The token every request carries. Order: "leaving preview" latch (null — nothing may carry the
// builder's token while the page is being torn down; specs/064 S9-2) › preview token › the builder's
// own token IN THE CURRENT WORKSPACE — the session token while the tab builds inside a customer
// (specs/062 L2, D-10), else the home token. This is the ONE choke point: lib/admin.ts, aiBuilderStream
// and every raw-fetch helper read it, so they are right in a preview tab and in a workspace tab with
// no change of their own. Invariant: inside a workspace this NEVER answers the home token — including
// the moment the workspace ends: the "leaving" latch (lib/workspace.ts) answers null from before the
// state is cleared until the page is replaced, so no late request can slip out as the builder's Nexus login.
export function getToken(): string | null {
  if (isPreviewExiting() || isWorkspaceLeaving()) return null;
  return getPreviewToken() ?? getBuilderToken();
}

/** The builder's OWN access token for the tenant the tab is in (never the preview's) — what POST /admin/preview-sessions needs. */
export function getBuilderToken(): string | null {
  if (isWorkspaceLeaving()) return null;
  return liveWorkspace()?.accessToken ?? getHomeToken();
}

/** The home (Nexus studio) login's access token. In a workspace only opening/resuming a session may use it. */
export function getHomeToken(): string | null {
  if (_accessToken) return _accessToken;
  if (typeof window !== "undefined") {
    _accessToken =
      window.localStorage.getItem("nexus_token") ??
      window.sessionStorage.getItem("nexus_token");
  }
  return _accessToken;
}

// The tab's workspace state, but only while it still belongs to the home login that opened it
// (D-21 / FR-014a): a different user signed in as the home login, or no home token left to resume
// with ⇒ the state is void. Drop it and answer "not in a workspace", so the next request is plainly home.
function liveWorkspace(): WorkspaceState | null {
  const ws = getWorkspace();
  if (!ws) return null;
  if (!getHomeToken() || homeUserId() !== ws.homeUserId) {
    clearWorkspace();
    return null;
  }
  return ws;
}

// `id` of the home user, read straight from the shared nexus_user (a workspace tab never writes it).
function homeUserId(): string | null {
  try {
    const raw = typeof window === "undefined" ? null : window.localStorage.getItem("nexus_user");
    return raw ? ((JSON.parse(raw) as { id?: string }).id ?? null) : null;
  } catch {
    return null;
  }
}

export function getRefresh(): string | null {
  if (_refreshToken) return _refreshToken;
  if (typeof window !== "undefined") {
    _refreshToken =
      window.localStorage.getItem("nexus_refresh") ??
      window.sessionStorage.getItem("nexus_refresh");
  }
  return _refreshToken;
}

function setToken(t: string | null) {
  _accessToken = t;
  if (typeof window !== "undefined") {
    const store = getStorage() ?? window.localStorage;
    if (t) {
      store.setItem("nexus_token", t);
    } else {
      window.localStorage.removeItem("nexus_token");
      window.sessionStorage.removeItem("nexus_token");
    }
  }
}

function setRefresh(t: string | null) {
  _refreshToken = t;
  if (typeof window !== "undefined") {
    const store = getStorage() ?? window.localStorage;
    if (t) {
      store.setItem("nexus_refresh", t);
    } else {
      window.localStorage.removeItem("nexus_refresh");
      window.sessionStorage.removeItem("nexus_refresh");
    }
  }
}

export function setRememberMe(remember: boolean) {
  if (typeof window === "undefined") return;
  if (remember) {
    window.localStorage.setItem("nexus_remember", "1");
  } else {
    window.localStorage.removeItem("nexus_remember");
  }
}

// ── Silent refresh (single-flight in the tab, serialized across tabs) ──
// N concurrent 401s in ONE tab fire exactly ONE POST /auth/refresh; all await the same promise.
// ACROSS tabs (specs/062-customer-workspace L1c, D-19) the rotation runs inside a Web Lock and
// re-reads the shared storage first: with "Ghi nhớ đăng nhập" every tab shares one refresh-token
// chain through localStorage but keeps its own in-memory copy, and presenting a token another tab
// already rotated away is read by the server as theft (E5 — every session revoked + a bogus alert
// mail). If the stored refresh token differs from the one this tab holds, another tab already
// rotated: adopt that pair and send nothing. If refresh fails → tokens wiped → redirect to /login.
let _refreshPromise: Promise<void> | null = null;

// Reads a token key straight from storage, bypassing the tab's in-memory copy (same precedence as
// getToken/getRefresh: localStorage first, then sessionStorage).
function readStored(key: "nexus_token" | "nexus_refresh"): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem(key) ?? window.sessionStorage.getItem(key);
}

// Runs fn under the browser's cross-tab lock when it has one (Web Locks API, native); otherwise just
// runs it — the in-lock storage re-read still covers the sequential case.
function withHomeRefreshLock<T>(fn: () => Promise<T>): Promise<T> {
  const locks = typeof navigator !== "undefined" ? navigator.locks : undefined;
  return locks ? (locks.request("nexus:home-refresh", fn) as Promise<T>) : fn();
}

// Claims of an access token WITHOUT verifying it (the server verifies; here we only compare who a
// stored pair belongs to and whether it is still fresh). null when it is not a decodable JWT.
function jwtClaims(t: string | null): { uid?: string; tid?: string; sid?: string; exp?: number } | null {
  const part = t?.split(".")[1];
  if (!part) return null;
  try {
    const b64 = part.replace(/-/g, "+").replace(/_/g, "/");
    return JSON.parse(atob(b64.padEnd(Math.ceil(b64.length / 4) * 4, "=")));
  } catch {
    return null;
  }
}

// Same login = same user + tenant + login session (sid survives rotation). Anything undecodable or
// missing a claim is "not the same" — then the tab keeps the old, own-chain path.
function sameLogin(a: string | null, b: string | null): boolean {
  const x = jwtClaims(a);
  const y = jwtClaims(b);
  return !!x && !!y && !!x.uid && !!x.tid && !!x.sid && x.uid === y.uid && x.tid === y.tid && x.sid === y.sid;
}

const REFRESH_FETCH_TIMEOUT_MS = 15_000;

async function silentRefresh(): Promise<void> {
  if (_refreshPromise) return _refreshPromise;
  _refreshPromise = (async () => {
    const heldRefresh = getRefresh(); // the token THIS tab would present
    if (!heldRefresh) throw new ApiError(401, "UNAUTHORIZED", "No refresh token");
    try {
      await withHomeRefreshLock(async () => {
        // Inside the lock: has another tab rotated the chain since this tab last read it?
        let presented = heldRefresh;
        const storedRefresh = readStored("nexus_refresh");
        const storedAccess = readStored("nexus_token");
        // Adopt only a pair of the SAME login: a second login in another tab (other customer / other
        // account) overwrites localStorage, and adopting it would flip this tab's tenant mid-session.
        if (storedRefresh && storedRefresh !== heldRefresh && sameLogin(_accessToken, storedAccess)) {
          const exp = jwtClaims(storedAccess)?.exp;
          if (exp && exp * 1000 > Date.now() + 5_000) {
            _accessToken = storedAccess;
            _refreshToken = storedRefresh;
            return;
          }
          // The other tab rotated, but its access token has since expired too (both tabs idle):
          // rotate the stored chain — the held refresh token is already spent.
          presented = storedRefresh;
        }
        const res = await fetch(`${BASE}/auth/refresh`, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ refreshToken: presented }),
          // The lock spans every tab of the origin: a hung request must not hold it for minutes.
          signal: AbortSignal.timeout(REFRESH_FETCH_TIMEOUT_MS),
        });
        const json = await res.json().catch(() => ({}));
        if (!res.ok) throw new ApiError(res.status, json.code ?? "INVALID_REFRESH", json.error ?? "Refresh failed");
        // Rotate: store both new tokens atomically
        setToken(json.data?.accessToken ?? json.accessToken ?? null);
        setRefresh(json.data?.refreshToken ?? json.refreshToken ?? null);
      });
    } catch (e) {
      // Refresh failed — wipe tokens and fire session-expired event (caught by AuthProvider → modal).
      // The workspace goes FIRST (D-21): it is bound to this very login, and a workspace token left in
      // sessionStorage would outlive the wipe and run the NEXT sign-in in this tab as the builder.
      clearWorkspace();
      setToken(null);
      setRefresh(null);
      if (typeof window !== "undefined" && window.location.pathname !== "/login") {
        // Dispatch custom event for SessionExpiredModal to intercept gracefully.
        // Falls back to hard redirect if no listener responds within this tick.
        window.dispatchEvent(new CustomEvent("nexus:session-expired"));
      }
      throw e;
    } finally {
      _refreshPromise = null;
    }
  })();
  return _refreshPromise;
}

// Shared 401 recovery for data routes — REFRESH-FIRST, always. The old code let DEMO_MODE
// short-circuit straight to the seed auto-login, which silently re-authenticated an EXPIRED
// session as the SEED TENANT's admin and retried the request there — a mid-session cross-tenant
// flip (found by dogfood 2026-07-15: a lab-tenant session expired and the next click created an
// app inside the demo tenant). Seed auto-login is a COLD-START convenience only; ensureAuth()
// itself guards that (no prior-session trace). No refresh token and not a cold start →
// silentRefresh throws → session-expired modal → /login, same as before.
async function recover401(): Promise<void> {
  // In a workspace the workspace branch runs FIRST and is the only one: renew exactly THIS session or
  // end loudly. Never silentRefresh-then-carry-on with the home token, never the DEMO seed login (it
  // would flip the tab to the seed tenant under a banner that still names the customer).
  // Leaving (latch set): the workspace is over and the page is being replaced — nothing to renew, and
  // above all nothing to refresh-and-resend as the home login.
  if (isWorkspaceLeaving()) throw workspaceEnded();
  if (liveWorkspace()) return resumeWorkspace();
  // Safety net (specs/064 S9-2): a preview tab NEVER refreshes or seed-logs-in — either would swap
  // the previewed role for the builder's real (admin) session. Callers handle preview 401s first
  // (onPreview401); this only fires if one forgot.
  if (isPreviewMode()) throw previewEnded();
  if (getRefresh()) return silentRefresh();
  if (DEMO_MODE) {
    setToken(null);
    authPromise = null;
    return ensureAuth();
  }
  return silentRefresh();
}

// ── Builder workspace session — "Đang dựng cho <khách>" (specs/062-customer-workspace L2) ──
// POST /admin/workspace-sessions (home token) mints a ≤15-minute session token whose tenant is the
// customer's; the tab keeps it in sessionStorage (lib/workspace.ts) and getToken() answers it first.
// Renewal = RESUME of that same session (never a fresh open): a session closed in another tab or
// revoked server-side answers 404 WORKSPACE_SESSION_ENDED instead of quietly reopening.
interface WorkspaceOpened {
  accessToken: string;
  expiresAt: string;
  session: WorkspaceSession & { resumed?: boolean };
}

// Raw fetch with the HOME token — NOT req(): req's 401 recovery calls recover401, which in a workspace
// calls the resume, which would call this again. A 401 here means the home access token is stale:
// rotate the HOME chain once (silentRefresh, serialized across tabs) and ask again.
async function postWorkspaceSession(body: { tenantSlug: string; resumeSessionId?: string }): Promise<WorkspaceOpened> {
  for (let attempt = 0; ; attempt++) {
    const t = getHomeToken();
    const res = await fetch(`${BASE}/admin/workspace-sessions`, {
      method: "POST",
      headers: { "Content-Type": "application/json", ...previewHeaders("POST"), ...(t ? { Authorization: `Bearer ${t}` } : {}) },
      body: JSON.stringify(body),
    });
    const json = await res.json().catch(() => ({}));
    if (res.status === 401 && attempt === 0) {
      await silentRefresh();
      continue;
    }
    if (!res.ok) throw new ApiError(res.status, json.code ?? "UNKNOWN", json.error ?? res.statusText);
    return json.data as WorkspaceOpened;
  }
}

/**
 * Open (or re-use) the builder's session in customer `slug` and remember it in THIS tab. No navigation: the callers
 * (enterWorkspace here, lib/studioContext enter/switch) decide where the page goes and what is frozen first.
 * `homeId` is the home user the state is bound to (D-21); a switch passes the one it already holds, because from the
 * moment it starts nothing may be sent with the tab's token (the leave latch), not even /auth/me.
 */
export async function openWorkspace(slug: string, homeId?: string): Promise<void> {
  // The home user's id binds the state to THIS login (D-21). nexus_user is there once the studio loaded.
  // Resolved BEFORE the server session is opened: failing after it would leave an open row nobody closes.
  const id = homeId ?? homeUserId() ?? (await getMe()).id;
  if (!id) throw new ApiError(401, "UNAUTHORIZED", "Yêu cầu xác thực không hợp lệ");
  const out = await postWorkspaceSession({ tenantSlug: slug });
  // A view-as-role record and a draft-preview flag belong to the session they were opened in (specs/063 S7-3,
  // specs/073 FR-086): a stale preview token would be the next request's.
  clearPreview();
  clearDraftPreview();
  setWorkspace({ accessToken: out.accessToken, expiresAt: out.expiresAt, homeUserId: id, session: out.session });
  // "Gần đây" of the Studio switcher (FR-083): the one place every way into a session passes. Code and name only.
  rememberProject(id, { slug: out.session.tenantSlug, name: out.session.tenantName });
}

/**
 * Open the builder's session in `slug`, then load `next` (default: the project's own page) with a FULL page load,
 * which drops the QueryClient and every module cache that still holds the home tenant's data. `next` passes safeNext
 * for `slug`: anything outside that project's Studio pages becomes its first page.
 */
export async function enterWorkspace(slug: string, opts: { next?: string } = {}): Promise<void> {
  await openWorkspace(slug);
  window.location.assign(opts.next === undefined ? `/studio/du-an/${slug}` : safeNext(opts.next, slug));
}

function workspaceEnded(): ApiError {
  return new ApiError(401, "WORKSPACE_ENDED", "Phiên dựng đã kết thúc");
}

let _resumePromise: Promise<void> | null = null;

/**
 * Renew THIS tab's session token (single-flight: 3 concurrent 401s → 1 request). A definite "no" from
 * the server (session closed/expired/revoked, the builder lost the flag, the customer was suspended,
 * the home login is dead) ends the workspace: state cleared, `nexus:workspace-ended` fired,
 * WORKSPACE_ENDED thrown — and NO request is ever resent with the home token. A transient failure
 * (network, 429, 5xx) keeps the state and rethrows: the next request tries again.
 */
export function resumeWorkspace(): Promise<void> {
  if (_resumePromise) return _resumePromise;
  _resumePromise = (async () => {
    const ws = liveWorkspace();
    if (!ws) throw workspaceEnded();
    try {
      const out = await postWorkspaceSession({ tenantSlug: ws.session.tenantSlug, resumeSessionId: ws.session.id });
      const cur = getWorkspace();
      if (!cur) throw workspaceEnded(); // left the workspace while the request was in flight
      setWorkspace({ ...cur, accessToken: out.accessToken, expiresAt: out.expiresAt, session: out.session });
    } catch (e) {
      // silentRefresh already cleared the state and announced session-expired: nothing left to end.
      if (!getWorkspace()) throw e;
      if (e instanceof ApiError && [401, 403, 404, 422].includes(e.status)) {
        beginWorkspaceLeave(); // BEFORE the state goes: requests still in flight must not fall back to the home token
        clearWorkspace();
        if (typeof window !== "undefined") window.dispatchEvent(new CustomEvent(WORKSPACE_ENDED_EVENT));
        throw workspaceEnded();
      }
      throw e;
    }
  })().finally(() => {
    _resumePromise = null;
  });
  return _resumePromise;
}

/**
 * DELETE /admin/workspace-sessions/current with the SESSION token — "Về Nexus". Deliberately outside
 * recover401: nothing is announced and nothing falls back to the home token. A 401 usually means the
 * 15-minute token lapsed (sleeping laptop, hidden tab) while the session row is still open, so the
 * session is resumed ONCE (same id, nothing else) to get a fresh token and the DELETE is repeated —
 * otherwise the row would stay open to the 8-hour cap with no close audit (FR-020). If the resume
 * says the session is gone, it is already closed. Never throws — leaving must always work.
 */
export async function closeWorkspace(): Promise<void> {
  const ws = getWorkspace();
  if (!ws) return;
  const del = (token: string) =>
    fetch(`${BASE}/admin/workspace-sessions/current`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${token}`, ...previewHeaders("DELETE") },
      signal: AbortSignal.timeout(5000),
    });
  try {
    const res = await del(ws.accessToken);
    if (res.status === 401) {
      const again = await postWorkspaceSession({ tenantSlug: ws.session.tenantSlug, resumeSessionId: ws.session.id });
      await del(again.accessToken);
    }
  } catch {
    // swallowed on purpose — a dead network must not trap the builder in the customer's workspace
  }
}

/**
 * "Về Nexus": close the session (best effort), forget it, load `next` (default `/studio`, same-origin path only) as the
 * home tenant. lib/studioContext.exitProject is the same thing with the freeze and the cache clear in front.
 */
export async function exitWorkspace(opts: { next?: string } = {}): Promise<void> {
  // closeWorkspace reads the state itself (not getToken), so latching first still lets the DELETE go out.
  beginWorkspaceLeave();
  await closeWorkspace();
  clearWorkspace();
  clearPreview();
  if (typeof window !== "undefined") window.location.assign(safeLocalPath(opts.next));
}

// ── Preview-as-role 401 handling (specs/064 S9-2) ──
// A preview tab runs on a short-lived, server-checked token (builder's login still live, session row
// open, not expired). When it stops standing the right move is to leave preview, never to recover:
// no /auth/refresh (the refresh token is the BUILDER's), no DEMO seed login, and no resend with the
// builder's token — getToken() already answers null once the exit began.
function isPreviewMode(): boolean {
  return isPreviewExiting() || getPreviewToken() !== null;
}

function previewEnded(): ApiError {
  return new ApiError(401, "PREVIEW_ENDED", "Phiên xem thử đã kết thúc");
}

/** Tear the preview down and go back to the Studio page it was started from. Idempotent. */
export function leavePreviewEnded(): void {
  if (isPreviewExiting()) return;
  beginPreviewExit();
  const to = previewReturnTo(getPreview());
  clearPreview();
  setPreviewNotice("ended");
  if (typeof window !== "undefined") window.location.assign(to);
}

/**
 * A 401 while in preview mode. First one: return — the caller resends ONCE with the same preview
 * token (the builder's refresh rotation is not atomic, so one request may hit a dead-session blip;
 * see specs/064 Nợ còn mở). Any later one, or any 401 after the exit began: leave preview + throw.
 */
function onPreview401(retried: boolean): void {
  if (retried || isPreviewExiting() || getPreviewToken() === null) {
    leavePreviewEnded();
    throw previewEnded();
  }
}

// Server error envelope: { error, code, details }. `details` is a small typed map the UI needs —
//   • 422 RECORD_REQUIRED_MISSING → { field: "subject" }  (per-field error; the ONLY value-error that carries a field)
//   • 409 RECORD_VERSION_CONFLICT → { currentVersion: 0 } (optimistic-lock conflict banner)
// Other 422 value errors (picklist/email/range/too_long/pattern) return details:{} → form-level banner only.
// Verified live on :8090 (see ../specs/028-layout-l2-renderer/spec.md + T-RESEARCH cookbook).
export interface ApiErrorDetails {
  field?: string;          // dotted path for composite fields, e.g. "fullname.first"
  usages?: { workflowId: string; workflowName: string; actionIndex: number }[]; // 409 CREDENTIAL_IN_USE
  currentVersion?: number; // 409 conflict: the record's actual server version
  // Seconds from the `Retry-After` response HEADER (429s: auth rate-limit, AI throttles). Merged in
  // by req()/the raw-fetch auth helpers when the browser can read the header — see the E9 M4 note on
  // req(): cross-origin it needs CORS Expose-Headers, so treat it as best-effort (often undefined).
  retryAfter?: number;
  [k: string]: unknown;
}

/** Fired by req() on 503 TENANT_MAINTENANCE; detail = { until: ISO string | null }. */
export const TENANT_MAINTENANCE_EVENT = "nexus:tenant-maintenance";

export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public details: ApiErrorDetails = {},
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function req<T>(method: string, path: string, body?: unknown, _retried = false, asBuilder = false): Promise<T> {
  // Leaving a workspace (latch up): the page is about to be replaced, getToken() answers nothing, and a call that went
  // out anyway could only earn a 401 (specs/073 FR-085, the "cờ trong req()" proposed by ph1a). The session calls
  // (open/close) and the logout use raw fetch. /auth/* is refused too (FR-084(7)): every sign-in path
  // (login, ssoExchange, verifyMfa) lifts the latch itself BEFORE it calls req().
  if (isWorkspaceLeaving()) throw workspaceEnded();
  const headers: Record<string, string> = { "Content-Type": "application/json", ...previewHeaders(method) };
  const t = asBuilder ? getBuilderToken() : getToken();
  if (t) headers["Authorization"] = `Bearer ${t}`;
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  // 401 not yet retried → recover the session, then retry once. /auth/* stays excluded (a retry
  // would loop /auth/refresh or double-submit an MFA/login credential and burn rate-limit
  // attempts) — EXCEPT the read-only /auth/me: it is the app-boot probe, and bouncing it to
  // /login while a perfectly valid refresh token sits in storage broke the "refresh token tự
  // động" promise on every page load after access-token expiry (dogfood 2026-07-15).
  // /auth/sessions (specs/055) is the same kind of authenticated read/act as /auth/me — a stale
  // access token there must refresh, not show "không tải được" on a /profile opened after 15'.
  const recoverable = !path.startsWith("/auth/") || path === "/auth/me" || path.startsWith("/auth/sessions");
  if (res.status === 401 && !asBuilder && isPreviewMode()) {
    onPreview401(_retried); // throws unless this is the first 401 of a live preview
    return req<T>(method, path, body, true);
  }
  if (res.status === 401 && !_retried && recoverable) {
    await recover401();
    return req<T>(method, path, body, true, asBuilder);
  }

  const json = await res.json().catch(() => ({}));
  if (!res.ok) {
    // Forward the server `details` map (422 → {field}, 409 → {currentVersion}); the form layer
    // reads it for per-field errors / the optimistic-lock conflict banner. Dropping it here was
    // the T-RESEARCH blocker — without it LayoutForm can't map validation back to fields.
    // E9 M4: also merge the `Retry-After` HEADER (429 throttles — AI_RATE_LIMITED etc.) into
    // details.retryAfter, same as the raw-fetch auth helpers below. BEST-EFFORT: the backend CORS
    // config has no Expose-Headers, so a cross-origin browser (dev :3000 → :8090 direct) reads null
    // here → undefined; callers must render an honest seconds-free message in that case.
    const retryAfter = retryAfterFromHeader(res);
    // specs/064 S9-2: a write refused because this tab is a preview — PreviewBanner turns it into one Toast.
    if (json.code === "PREVIEW_READ_ONLY" && typeof window !== "undefined") {
      window.dispatchEvent(new CustomEvent("nexus:preview-read-only"));
    }
    // specs/068 slice B: a write refused because this workspace is write-frozen — MaintenanceBanner shows
    // the strip until `details.until`. ONLY this code: any other 503 (RECORD_HOOKS_UNAVAILABLE…) is not
    // a maintenance window and must not raise the banner.
    if (res.status === 503 && json.code === "TENANT_MAINTENANCE" && typeof window !== "undefined") {
      window.dispatchEvent(new CustomEvent(TENANT_MAINTENANCE_EVENT, { detail: { until: (json.details as { until?: string } | undefined)?.until ?? null } }));
    }
    throw new ApiError(
      res.status,
      json.code ?? "UNKNOWN",
      json.error ?? res.statusText,
      {
        ...((json.details ?? {}) as ApiErrorDetails),
        ...(retryAfter !== undefined ? { retryAfter } : {}),
      },
    );
  }
  // specs/063 S7-5 (FR-033): the change was applied but its direct version entry could not be written.
  if (typeof window !== "undefined" && typeof (json as { versionWarning?: unknown }).versionWarning === "string") {
    window.dispatchEvent(new CustomEvent(VERSION_WARNING_EVENT));
  }
  return json as T;
}

/** Fired by req() when a 2xx response carries `versionWarning`; VersionWarningToast turns it into one Toast. */
export const VERSION_WARNING_EVENT = "nexus:version-warning";

/**
 * The shared request path for callers outside this file (lib/admin.ts's admin screens): same
 * Bearer header, error mapping and 401 → refresh → retry-once as every other data route. A private
 * fetch wrapper skips recover401 — that was #290 ("Yêu cầu xác thực không hợp lệ" on an expired
 * access token).
 */
export function apiRequest<T>(method: string, path: string, body?: unknown): Promise<T> {
  return req<T>(method, path, body);
}

// ── Auth ──
export interface AuthMe {
  id: string;
  fullName: string;
  email: string;
  isAdmin: boolean;
  roleId?: string;
  roleName?: string;
  tenantId?: string;
  // FE-CATCHUP-1: GET /auth/me now ALWAYS carries the honest MFA state (true|false) — resolved
  // server-side from the real enrollment row (auth_service_extra.go Me → mfaEnabledFor), so the
  // MFA panel can open knowing on/off instead of probing via enroll-409/activate-422. Optional
  // here because the OTHER producers of this shape on the wire (login, mfa/verify — *bool with
  // omitempty on the BE) deliberately omit it; undefined = "not told", never "off".
  mfaEnabled?: boolean;
  // EASE Nước 6 (../specs/034-customer-admin-tier/spec.md §3): platform-operator flag, carried ONLY by
  // GET /auth/me (same *bool-omitempty discipline as mfaEnabled). Gates the Builder-face UI;
  // the BE re-reads the DB per request and never trusts this. undefined = "not told".
  isPlatformOperator?: boolean;
  // specs/065 (tầng chủ khách): carried ONLY by GET /auth/me. True ⇒ show "Cài đặt › Người dùng". Display only —
  // RequireUserManager re-reads the column per request. undefined = "not told".
  canManageUsers?: boolean;
  // specs/064 S9-2: carried ONLY by GET /auth/me for a preview token (omitempty on the BE) — the
  // synthetic role profile's session block. Its presence means "this tab is previewing a role".
  preview?: {
    sessionId: string;
    roleId: string;
    roleName: string;
    /** The real builder's name (the audit trail names them, the banner can too). */
    actorName: string;
    expiresAt: string;
    draft: boolean;
  };
  // specs/062 L2: carried ONLY by GET /auth/me for a builder workspace-session token (omitempty on the
  // BE) — the customer the builder is working in. The profile's tenantId is that customer's.
  workspace?: {
    sessionId: string;
    tenantId: string;
    tenantSlug: string;
    tenantName: string;
    expiresAt: string;
  };
}

// POST /auth/login has TWO success shapes (ENG-AUTH2 #7, api-conventions §3b.3):
//   • non-MFA account:  { data: { accessToken, refreshToken, user } }        → status "ok"
//   • MFA-enabled:      { data: { mfaPending: true, pendingToken } }         → status "mfa"
// The MFA shape carries NO token pair — the pendingToken is an opaque single-use 5-minute
// bridge credential for POST /auth/mfa/verify (NOT a JWT, NOT a session). The pre-MFA code
// here called setToken(r.data.accessToken) unconditionally, which stored `undefined` on the
// MFA shape and locked MFA users out with no second step — hence the discriminated union.
export type LoginResult =
  | { status: "ok"; user: AuthMe }
  | { status: "mfa"; pendingToken: string };

// Shared by login() and verifyMfa(): a completed authentication hands back the same
// { accessToken, refreshToken, user } envelope — store the pair + cache the inline user.
function applyTokenPair(d: { accessToken?: string; refreshToken?: string; user?: AuthMe }): void {
  setToken(d.accessToken ?? null);
  if (d.refreshToken) setRefresh(d.refreshToken);
  if (typeof window !== "undefined") {
    // A real authentication just completed — lift the demo opt-out that logout() set (see
    // LOGOUT_OPT_OUT). Cleared HERE, not in login(), because SSO exchange and MFA verify are
    // authentications too; clearing in login() would leave the sentinel stuck for those paths.
    window.localStorage.removeItem(LOGOUT_OPT_OUT);
  }
  if (d.user && typeof window !== "undefined") {
    window.localStorage.setItem("nexus_user", JSON.stringify(d.user));
  }
}

// The two-shape login envelope (POST /auth/login AND POST /auth/sso/exchange — the BE returns
// the exact same LoginOutput for both, api-conventions §3b.3 / §3c).
interface LoginEnvelope {
  accessToken?: string; refreshToken?: string; user?: AuthMe;
  mfaPending?: boolean; pendingToken?: string;
}

// Shared by login() and ssoExchange(): map the envelope onto the discriminated union.
// The P0 rule lives HERE once: the mfa branch must NEVER touch token storage (pre-fix code
// stored `undefined` as the access token and locked MFA users out).
function resolveLoginResult(d: LoginEnvelope, fallbackUser?: AuthMe): LoginResult {
  if (d.mfaPending) {
    // Credentials were CORRECT but a second factor is required. Do NOT touch token storage —
    // the caller must collect a TOTP/backup code and call verifyMfa(pendingToken, code).
    if (!d.pendingToken) {
      // Defensive: a mfaPending response without its bridge credential is a server fault.
      throw new ApiError(500, "MFA_PENDING_MALFORMED", "Phản hồi MFA thiếu pendingToken");
    }
    return { status: "mfa", pendingToken: d.pendingToken };
  }
  if (!d.accessToken) {
    // NIT-1: symmetric with the MFA_PENDING_MALFORMED guard above — a 200 that is neither
    // MFA-pending nor carries a token pair is a malformed server reply. Never silently return
    // {status:"ok"} with no session (the caller would treat it as logged-in with no token).
    throw new ApiError(500, "LOGIN_RESPONSE_MALFORMED", "Phản hồi đăng nhập không hợp lệ (thiếu accessToken)");
  }
  applyTokenPair(d);
  return { status: "ok", user: d.user ?? fallbackUser ?? ({} as AuthMe) };
}

export async function login(
  tenant: string,
  email: string,
  password: string,
  rememberMe = false,
): Promise<LoginResult> {
  // D-21: a new sign-in never inherits the workspace of whoever was signed in before — cleared before
  // the request so even /auth/login does not carry the old builder's session token.
  clearWorkspace();
  clearStudioLocalState(); // FR-087: the Studio's memory of customers belongs to whoever was signed in before
  endWorkspaceLeave(); // a deliberate fresh sign-in lifts the latch of a workspace left in this page
  setRememberMe(rememberMe);
  const r = await req<{ data: LoginEnvelope }>("POST", "/auth/login", { tenant, email, password });
  // Remember WHICH workspace this session belongs to — the backend never echoes the slug, but the
  // UI must show it (dogfood: a session silently switched tenants and nothing on screen said so).
  if (typeof window !== "undefined") window.localStorage.setItem("nexus_tenant", tenant.trim().toLowerCase());
  return resolveLoginResult(r.data, { email } as AuthMe);
}

// The workspace slug of the CURRENT session, "" when unknown (e.g. an SSO session — the slug
// never transits /auth/me). Read at render time by the sidebar footers' tenant chip.
export function tenantSlug(): string {
  if (typeof window === "undefined") return "";
  return liveWorkspace()?.session.tenantSlug ?? window.localStorage.getItem("nexus_tenant") ?? "";
}

// POST /auth/sso/exchange {code} — #48 SSO-FE-REDIRECT. Redeems the one-time handoff code the
// backend /auth/sso/callback 302s to /auth/sso/complete?code=… with (single-use, 120s TTL; the
// token pair itself NEVER rides in a URL). The response envelope is IDENTICAL to POST /auth/login
// — token pair OR {mfaPending, pendingToken} — so it resolves through the same mapping, including
// the no-token-storage-on-mfa rule. Any useless code (unknown / already used / expired / user or
// provider disabled) → 401 SSO_HANDOFF_INVALID (deliberately coarse, no existence leak).
export async function ssoExchange(code: string): Promise<LoginResult> {
  clearWorkspace(); // D-21, same as login()
  clearStudioLocalState();
  endWorkspaceLeave();
  const r = await req<{ data: LoginEnvelope }>("POST", "/auth/sso/exchange", { code });
  return resolveLoginResult(r.data);
}

// POST /auth/mfa/verify (PUBLIC, rate-limited) — step 2 of the two-step login. Redeems the
// pendingToken from login()'s "mfa" branch for a real token pair; the response body is
// IDENTICAL in shape to a normal login success, so we store it the same way.
// Errors (api-conventions §3b.3): 401 MFA_CODE_INVALID (wrong/replayed code — pendingToken
// is NOT burned, the user may retry), 401 MFA_PENDING_INVALID (expired/used/forged token —
// caller must restart from step 1), 429 RATE_LIMITED (Retry-After), 503 MFA_UNAVAILABLE.
export async function verifyMfa(pendingToken: string, code: string): Promise<AuthMe> {
  clearWorkspace(); // D-21, same as login()
  clearStudioLocalState();
  endWorkspaceLeave();
  const r = await req<{ data: { accessToken?: string; refreshToken?: string; user?: AuthMe } }>(
    "POST", "/auth/mfa/verify", { pendingToken, code },
  );
  applyTokenPair(r.data);
  return r.data.user ?? ({} as AuthMe);
}

// ── MFA self-service (ENG-AUTH2 #7, api-conventions §3b.2) — protected, Bearer required ──
// All three are step-up re-authenticated (MFA-STEPUP): the account password rides in the body.
// Shared error map: 422 MFA_PASSWORD_REQUIRED (missing password) · 401 INVALID_CREDENTIALS
// (wrong password — counts toward a 5/15min lockout) · 429 MFA_TOO_MANY_ATTEMPTS (Retry-After)
// · 503 MFA_UNAVAILABLE (MFA_SECRET_ENC_KEY absent — honest-disabled, not a crash).
// NOTE: req() deliberately never silent-refreshes /auth/* paths, so an EXPIRED access token
// here surfaces as 401 UNAUTHORIZED (distinct code from INVALID_CREDENTIALS) — the panel maps
// it to a "phiên hết hạn" message instead of blaming the password.
export interface MfaEnrollment { secret: string; otpauthUrl: string }

// POST /auth/mfa/enroll {password} → { secret, otpauthUrl }. Generates (or REgenerates —
// calling again before activate overwrites) a TOTP secret. MFA is NOT active until activate.
// Enrolling while already enabled → 409 MFA_ALREADY_ENABLED (the panel uses this as the
// "already enrolled" state discriminator — there is no GET status endpoint on the BE).
export async function mfaEnroll(password: string): Promise<MfaEnrollment> {
  const r = await req<{ data: MfaEnrollment }>("POST", "/auth/mfa/enroll", { password });
  return r.data;
}

// POST /auth/mfa/activate {password, code} → { backupCodes: [...10 single-use codes] }.
// The plaintext codes are returned ONCE — the caller must show them and demand a save-confirm.
// Wrong code → 401 MFA_CODE_INVALID; no prior enroll → 422 MFA_NOT_ENROLLED.
export async function mfaActivate(password: string, code: string): Promise<string[]> {
  const r = await req<{ data: { backupCodes: string[] } }>(
    "POST", "/auth/mfa/activate", { password, code },
  );
  return r.data?.backupCodes ?? [];
}

// POST /auth/mfa/disable {password, code} — code is a valid TOTP or an unused backup code.
// Wipes the enrollment + every backup code. Not enrolled → 422 MFA_NOT_ENROLLED.
// (BE returns 200 {data:{ok:true}} — verified in auth_handler.go, not the 204 the spec sketch said.)
export async function mfaDisable(password: string, code: string): Promise<void> {
  await req("POST", "/auth/mfa/disable", { password, code });
}

// Marker written by an explicit logout, read by ensureAuth's demo auto-login guard, removed by
// applyTokenPair. Lives beside nexus_user / nexus_tenant / nexus_remember / nexus_branding.
const LOGOUT_OPT_OUT = "nexus_logged_out";

/**
 * logoutEverywhere — POST /auth/logout-all: END every login session of the current user, on every
 * device, then clear this browser's own session via the normal logout path.
 *
 * Since specs/055 this is immediate: each device's access token carries its session id (`sid`),
 * and the server refuses a token of an ended session on its very next request — no 15-minute tail.
 *
 * The local logout runs even if the revoke call fails: a user who asked to be signed out must end
 * up signed out here regardless, and the thrown error still surfaces so they know the OTHER
 * devices were not handled and can retry.
 */
export async function logoutEverywhere(): Promise<void> {
  try {
    if (getWorkspace()) {
      // /auth/logout-all refuses a session token (403): leave the workspace first, then ask as the HOME login.
      beginWorkspaceLeave();
      await closeWorkspace();
      clearWorkspace();
      const res = await fetch(`${BASE}/auth/logout-all`, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...previewHeaders("POST"), Authorization: `Bearer ${getHomeToken() ?? ""}` },
        body: "{}",
      });
      if (!res.ok) {
        const json = await res.json().catch(() => ({}));
        throw new ApiError(res.status, json.code ?? "UNKNOWN", json.error ?? res.statusText);
      }
    } else {
      await req("POST", "/auth/logout-all", {});
    }
  } finally {
    await logout();
  }
}

export async function logout(): Promise<void> {
  // A preview tab has no session of its own to sign out of: "Đăng xuất" there means leave the preview.
  // Falling through would POST /auth/logout with the preview token (403) and then WIPE the builder's
  // real tokens, which every tab of the browser shares (specs/064 S9-2).
  if (getPreview()) {
    const to = await endPreviewSession("exit");
    if (typeof window !== "undefined") window.location.assign(to);
    return;
  }
  // FR-018: leaving a workspace comes first, with the session token; only then is the home login signed out.
  if (getWorkspace()) {
    beginWorkspaceLeave();
    await closeWorkspace();
    clearWorkspace();
  }
  try {
    // The HOME login signs itself out. Raw fetch (not req): the leaving latch just made getToken() null
    // so nothing in this page can reuse the home token — except this one call, on purpose. /auth/* is
    // never retried by req(), so nothing is lost by bypassing it.
    const t = getHomeToken();
    const rt = getRefresh();
    // /auth/logout ends the session of the refresh token IN THE BODY. Posting {} ended nothing, so a browser
    // that "logged out" stayed a live session for the whole refresh TTL (7 days) — and, since
    // specs/055, kept showing up under /profile › Phiên đăng nhập.
    if (t || rt) {
      await fetch(`${BASE}/auth/logout`, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...previewHeaders("POST"), ...(t ? { Authorization: `Bearer ${t}` } : {}) },
        body: JSON.stringify({ refreshToken: rt ?? "" }),
      });
    }
  } catch {
    // Ignore logout API errors — still clear local state
  } finally {
    setToken(null);
    setRefresh(null);
    clearDraftPreview();
    clearStudioLocalState(); // FR-087: the Studio's per-user remembered state is swept; only the {slug,name} recent list stays (see lib/studioLocalState)
    if (typeof window !== "undefined") {
      window.localStorage.removeItem("nexus_user");
      window.localStorage.removeItem("nexus_tenant");
      window.localStorage.removeItem("nexus_remember");
      // AO-THUONG-HIEU: drop the cached branding so a shared machine doesn't show the previous
      // tenant's logo/colors on the next login (typing an org re-brands live anyway).
      window.localStorage.removeItem("nexus_branding");
      window.sessionStorage.removeItem("nexus_token");
      window.sessionStorage.removeItem("nexus_refresh");
      // LAST write, after every removal above: "a human asked to leave". Without it, wiping
      // nexus_user hands DEMO_MODE auto-login its trigger and the logout undoes itself.
      window.localStorage.setItem(LOGOUT_OPT_OUT, "1");
    }
  }
}

export function isAuthed() { return !!getToken(); }

// ── Login sessions — "thiết bị đang đăng nhập" (specs/055-session-list-kill) ──
// One row per login (a device chain that survives token refreshes). `ip` is already masked by the
// BE ("113.161.42.x"); browser/os are derived there from the User-Agent ("" when unknown).
export interface LoginSession {
  id: string;
  browser?: string;
  os?: string;
  userAgent?: string;
  ip?: string;
  lastUsedAt: string;
  expiresAt: string;
  /** The session THIS browser is using ("Phiên này") — from the access token's `sid` claim. */
  current?: boolean;
}

/** GET /auth/sessions — the signed-in user's own live sessions, most recently used first. */
export async function listMySessions(): Promise<LoginSession[]> {
  const r = await req<{ data: LoginSession[] | null }>("GET", "/auth/sessions");
  return r.data ?? [];
}

/**
 * DELETE /auth/sessions/:id — sign ONE of my devices out. That device is refused on its very next
 * request (and cannot refresh); this browser and the others are untouched. A session that is not
 * (or no longer) mine and alive → 404 SESSION_NOT_FOUND.
 */
export async function endMySession(id: string): Promise<void> {
  await req("DELETE", `/auth/sessions/${encodeURIComponent(id)}`);
}

export async function getMe(): Promise<AuthMe> {
  const r = await req<{ data: AuthMe }>("GET", "/auth/me");
  if (typeof window !== "undefined") {
    // A preview profile goes to THIS tab's preview state — `nexus_user` is localStorage, shared by
    // every tab, and the builder's other tabs read it (specs/064 S9-2).
    if (r.data.preview) setPreviewMe(r.data);
    // Same for a workspace profile (specs/062 L2): it is THIS tab's, the shared nexus_user is the home
    // login's. Keyed on the response, not on the tab state: a session that ended while /auth/me was in
    // flight must still never write the customer's profile over the builder's.
    else if (r.data.workspace) {
      const w = getWorkspace();
      if (w) setWorkspace({ ...w, me: r.data });
    } else if (!isPreviewMode()) window.localStorage.setItem("nexus_user", JSON.stringify(r.data));
  }
  return r.data;
}

export function getCachedMe(): AuthMe | null {
  if (typeof window === "undefined") return null;
  // Fixed HERE, once, for every reader (recents, pins, dataGrid keys, RunFilterBar, AuthProvider):
  // a preview tab must never see the builder's cached profile — it would key "Vừa xem"/pins by the
  // admin's id and show records the previewed role may not see.
  const pv = getPreview();
  if (pv) return pv.me ?? null;
  // Same inside a workspace: the profile of the customer's tenant, not the home user's cached one.
  const w = liveWorkspace();
  if (w) return w.me ?? null;
  const s = window.localStorage.getItem("nexus_user");
  if (!s) return null;
  try { return JSON.parse(s) as AuthMe; } catch { return null; }
}

/**
 * Whose browser memory this tab reads and writes (specs/062 L2b): `key` = `${tenantId}:${userId}` — the
 * namespace of pins, recents, list prefs, active app and last paths. `legacyUserId` is the pre-L2b
 * namespace (bare user id), set ONLY for the home tenant of a plain login (never in a workspace or
 * preview): that is the one place old values may be adopted (lib/scopedStorage.ts).
 *
 * In a workspace the tenant is the customer's and the user is the HOME builder (stable from the first
 * paint — the workspace /auth/me profile arrives later and must not change the key under the page).
 * A cached profile without a tenantId keeps its bare user id: nothing to scope by, nothing lost.
 */
export function getStorageScope(): { key: string; legacyUserId: string | null } {
  const ws = typeof window === "undefined" ? null : liveWorkspace();
  if (ws) return { key: `${ws.session.tenantId}:${ws.homeUserId}`, legacyUserId: null };
  const me = getCachedMe();
  const uid = me?.id ?? me?.email;
  if (!me || !uid) return { key: "anon", legacyUserId: null };
  const key = me.tenantId ? `${me.tenantId}:${uid}` : uid;
  return { key, legacyUserId: getPreview() || key === uid ? null : uid };
}

// ── Preview as a role (specs/064 S9-2) ──
export interface PreviewSessionStart {
  sessionId: string;
  token: string;
  expiresAt: string;
  role: { id: string; name: string; label?: string };
  appId: string | null;
  draft: boolean;
}

/** POST /admin/preview-sessions — always with the BUILDER's token, never a preview one. */
export async function startPreviewSession(roleId: string, appId?: string, draft?: boolean): Promise<PreviewSessionStart> {
  // In a workspace the builder token is the SESSION's. If the workspace is void (another home login in this tab),
  // getBuilderToken() clears it and answers the HOME token: refuse, never mint a preview with that (specs/077 T151).
  const inWs = getWorkspace() !== null;
  getBuilderToken();
  if (inWs && getWorkspace() === null) throw workspaceEnded();
  const body: Record<string, unknown> = { roleId };
  if (appId) body.appId = appId;
  // Send draft only when the caller decided (true OR false): absent = the server default (on when the app is delivered and has a draft).
  if (draft !== undefined) body.draft = draft;
  const r = await req<{ data: PreviewSessionStart }>("POST", "/admin/preview-sessions", body, false, true);
  return r.data;
}

/**
 * DELETE /preview-sessions/current — end this tab's preview and clear it locally. Best-effort on the
 * wire (a dead network or an already-ended session must not trap the builder in preview; the row
 * expires on its own), certain locally. Raises the exit latch FIRST so nothing else in the page
 * sends the builder's token meanwhile, and returns the URL to navigate to.
 */
export async function endPreviewSession(reason: "exit" | "left"): Promise<string> {
  const p = getPreview();
  if (!p) return "/studio";
  beginPreviewExit();
  try {
    await fetch(`${BASE}/preview-sessions/current?reason=${reason}`, {
      method: "DELETE",
      headers: { Authorization: `Bearer ${p.token}`, ...previewHeaders("DELETE") },
      signal: AbortSignal.timeout(5000),
    });
  } catch {
    // swallowed on purpose — see above
  } finally {
    clearPreview();
  }
  return previewReturnTo(p);
}

export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  await req("PUT", "/auth/me/password", { currentPassword, newPassword });
}

// ── Forgot / reset password (ENG-AUTH2 #1, docs/api-conventions.md §3a) ──
// Both are PUBLIC pre-auth endpoints (no Bearer token, no silent-refresh dance needed), so they
// use a raw fetch instead of the shared req() helper. Retry-After parsing is shared: since E9 M4
// the shared req() ALSO merges this header into ApiError.details.retryAfter (function declaration
// → hoisted, safe to call from req() above).
function retryAfterFromHeader(res: Response): number | undefined {
  const raw = res.headers.get("Retry-After");
  const n = raw ? Number(raw) : NaN;
  return Number.isFinite(n) && n > 0 ? n : undefined;
}

// POST /auth/forgot-password {tenant, email} — server ALWAYS returns 200 {data:{ok:true}} for a
// well-formed request, whether or not the tenant/email exist (anti-enumeration). The only errors
// a well-formed caller can see here are infra faults (500) or rate-limiting (429, IP-keyed).
export async function forgotPassword(tenant: string, email: string): Promise<void> {
  const res = await fetch(`${BASE}/auth/forgot-password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ tenant, email }),
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) {
    const retryAfter = retryAfterFromHeader(res);
    throw new ApiError(
      res.status,
      json.code ?? "UNKNOWN",
      json.error ?? res.statusText,
      { ...(json.details ?? {}), ...(retryAfter !== undefined ? { retryAfter } : {}) },
    );
  }
}

// POST /auth/reset-password {token, newPassword} — deliberately NO tenant field; the server
// derives tenant from the token row itself. An invalid/expired/already-used token → 400
// RESET_TOKEN_INVALID. A new password violating the shared policy (RM-17: ≥8 chars, not all
// whitespace, ≤72 bytes) → 422 PASSWORD_TOO_WEAK (same code/shape as PUT /auth/me/password).
export async function resetPassword(token: string, newPassword: string): Promise<void> {
  const res = await fetch(`${BASE}/auth/reset-password`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token, newPassword }),
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) {
    const retryAfter = retryAfterFromHeader(res);
    throw new ApiError(
      res.status,
      json.code ?? "UNKNOWN",
      json.error ?? res.statusText,
      { ...(json.details ?? {}), ...(retryAfter !== undefined ? { retryAfter } : {}) },
    );
  }
}

// ── SSO login entry (ENG-AUTH2 #8, api-conventions §3c.2) — PUBLIC, redirect flow ──
// GET /auth/sso/:tenant/start?provider=<id-hoặc-issuer> là một OAuth authorization-code redirect:
// PHẢI điều hướng cả trang (window.location) chứ KHÔNG fetch — response là 302 tới IdP và cookie
// bind `sso_state` (login-CSRF) chỉ đi theo top-level navigation. `provider` là BẮT BUỘC: backend
// (services.SSOService.resolveProvider) trả 404 SSO_PROVIDER_NOT_FOUND khi param rỗng — KHÔNG có
// "provider mặc định". Endpoint liệt kê provider là admin-only nên pre-login KHÔNG thể enumerate;
// người dùng nhập issuer hoặc ID do quản trị viên cấp (Login.tsx ghi chú giới hạn này trung thực).
// #48 SSO-FE-REDIRECT: khi BE có SSO_FRONTEND_BASE, /callback 302 về /auth/sso/complete?code=…
// (one-time handoff, token KHÔNG nằm trong URL) → trang đó gọi ssoExchange(); lỗi callback 302
// về /login?ssoError=<CODE>. Legacy mode (env vắng): /callback vẫn trả JSON thẳng từ backend.
export function ssoStartUrl(tenant: string, provider: string): string {
  const qs = new URLSearchParams({ provider: provider.trim() });
  return `${BASE}/auth/sso/${encodeURIComponent(tenant.trim())}/start?${qs.toString()}`;
}

// SSO-PROVIDER-PUBLIC-LIST: enabled providers of a tenant for the pre-auth login page —
// lets it render branded buttons ("Đăng nhập bằng Google") instead of a manual issuer field.
// Unknown tenant → [] (anti-enumeration fold server-side); SSO disabled → 503 (caller treats
// any failure as "no buttons" and falls back to the manual panel).
export interface PublicSSOProvider { id: string; displayName: string; issuer: string }
export async function ssoPublicProviders(tenant: string): Promise<PublicSSOProvider[]> {
  const res = await fetch(`${BASE}/auth/sso/${encodeURIComponent(tenant.trim())}/providers`, {
    headers: { Accept: "application/json" },
  });
  if (!res.ok) return [];
  const j = (await res.json()) as { data?: { providers?: PublicSSOProvider[] } };
  return j.data?.providers ?? [];
}

// DEMO mode (mặc định): auto-login bằng tài khoản seed để xem nhanh không cần đăng nhập.
// Demo auto-login là OPT-IN (fail-closed): chỉ bật khi NEXT_PUBLIC_DEMO_MODE=true tường minh.
// Default cũ (`!== "false"`) là fail-OPEN — build nào quên set env là mang theo seed-creds
// auto-login, và mọi 401 flip user sang tenant demo (xem recover401). Máy demo local đã set
// NEXT_PUBLIC_DEMO_MODE=true trong .env.local nên tiện-ích dev giữ nguyên.
// (NEXT_PUBLIC_* được inline lúc build; Dockerfile prod đặt =false.)
export const DEMO_MODE = process.env.NEXT_PUBLIC_DEMO_MODE === "true";
// Seed identity comes from the env, NEVER a source literal — a hardcoded password ships in every
// bundle (even DEMO_MODE=false ones; top-level consts survive dead-code elimination). Missing any
// of the three → auto-login silently disabled, normal /login flow (fail-closed). Demo box sets
// all four NEXT_PUBLIC_DEMO_* vars in .env.local.
const DEMO = {
  tenant: process.env.NEXT_PUBLIC_DEMO_TENANT ?? "",
  email: process.env.NEXT_PUBLIC_DEMO_EMAIL ?? "",
  password: process.env.NEXT_PUBLIC_DEMO_PASSWORD ?? "",
};
let authPromise: Promise<void> | null = null;
export async function ensureAuth(): Promise<void> {
  // Leaving preview: not a cold start — no seed auto-login, no bounce to /login (we are navigating).
  if (isPreviewExiting()) throw previewEnded();
  if (isAuthed()) return;
  // Cold-start guard: seed auto-login may only BOOTSTRAP a browser with no prior session. A
  // cached nexus_user (with no live token) means a REAL session expired here — auto-logging-in
  // would silently swap that user/tenant for the seed admin.
  const hadSession = typeof window !== "undefined" && !!window.localStorage.getItem("nexus_user");
  // ...and the SECOND door into that same room, which the guard above missed for months: logout()
  // deliberately removes nexus_user, so it handed auto-login the exact condition it fires on. Net
  // effect on any DEMO_MODE box (this one): "Đăng xuất" bounced straight back in as seed admin —
  // the button was a no-op, and LUẬT NGHIỆM THU SỐ 0 (every acceptance ends logged in as a
  // NON-admin) was physically unperformable. Two correct ends, an unwritten join — the campaign's
  // own bug-class (docs/NEXUS-MOT-XUONG-2026-07-29.md §0b). An explicit logout is now remembered
  // until a real authentication clears it (applyTokenPair); a genuinely fresh profile has neither
  // key, so the dev convenience is untouched.
  const optedOut = typeof window !== "undefined" && !!window.localStorage.getItem(LOGOUT_OPT_OUT);
  if (DEMO_MODE && !hadSession && !optedOut && DEMO.tenant && DEMO.email && DEMO.password) {
    // Auto-login can only complete one-step: if the seed account somehow has MFA enabled,
    // surface it honestly instead of resolving "authed" without a token.
    if (!authPromise) authPromise = login(DEMO.tenant, DEMO.email, DEMO.password).then((res) => {
      if (res.status === "mfa") {
        throw new ApiError(401, "MFA_REQUIRED", "Tài khoản demo bật MFA — không thể auto-login; đăng nhập thủ công tại /login");
      }
    }).catch((e) => { authPromise = null; throw e; });
    return authPromise;
  }
  // PROD: bắt buộc đăng nhập thật. Đẩy về /login — TRỪ các trang public pre-auth: chúng chạy
  // khi CHƯA có token by design. Đặc biệt /auth/sso/complete: bootstrap của AuthProvider chạy
  // song song với cú exchange handoff — bounce ở đây sẽ giết luồng SSO redirect (#48) trước khi
  // token kịp về (và trước đó đã âm thầm đá người dùng khỏi /reset-password, /forgot-password).
  if (typeof window !== "undefined" && !PUBLIC_AUTH_PATHS.includes(window.location.pathname)) {
    window.location.href = "/login";
  }
  throw new ApiError(401, "NEED_LOGIN", "Cần đăng nhập");
}

// Public pre-auth routes that must never be kicked to /login by the bootstrap guard above.
// "/" is the public landing (app/(marketing)); signed-in visitors leave it via MarketingAuthGate.
const PUBLIC_AUTH_PATHS = ["/", "/login", "/forgot-password", "/reset-password", "/auth/sso/complete"];

// ── Permissions ──
// GET /metadata/permissions/me — returns the caller's effective grant matrix.
// Admin: [{ entityName:"*", action:"*", accessLevel:"all" }] (wildcard = all-access).
// Non-admin: per-entity grants. Empty array = deny-all (fail-closed).
export type PermAccessLevel = "none" | "owner" | "team" | "all" | `field:${string}`;
export type PermAction = "view" | "create" | "edit" | "delete" | "*";
export interface PermGrant { entityName: string; action: PermAction; accessLevel: PermAccessLevel }

export async function getMyPermissions(): Promise<PermGrant[]> {
  const r = await req<{ data: PermGrant[] }>("GET", "/metadata/permissions/me");
  return r.data ?? [];
}

// ── Notifications ──
// Live P0 endpoints (verified in router.go): GET /notifications, GET /notifications/unread-count,
// POST /notifications/:id/read, POST /notifications/read-all
export interface ApiNotification {
  id: string;
  type: string;
  /** Optional headline. The BE column is nullable (`Title *string`, json omitempty) and several
   *  producers — the approval arm, the SLA breach scan — set only `message`. Declaring it required
   *  is what let the renderer show an empty line for those. */
  title?: string;
  /** The sentence every producer writes. It was in the payload all along and NO consumer read it,
   *  so the bell showed a type badge and a timestamp and never said what happened. */
  message: string;
  href?: string;
  isRead: boolean;
  createdAt: string;
}
export interface NotificationList { notifications: ApiNotification[]; total: number }

export async function listNotifications(limit = 20): Promise<NotificationList> {
  // Backend shape: { data: ApiNotification[], pagination: { total, ... } }
  // NOT { data: { notifications: [], total: 0 } } — array is at data root.
  const r = await req<{ data: ApiNotification[]; pagination?: { total?: number } }>("GET", `/notifications?limit=${limit}`);
  const arr = Array.isArray(r.data) ? r.data : [];
  return { notifications: arr, total: r.pagination?.total ?? arr.length };
}

export async function getNotificationUnreadCount(): Promise<number> {
  const r = await req<{ data: { count: number } }>("GET", "/notifications/unread-count");
  return r.data?.count ?? 0;
}

export async function markNotificationRead(id: string): Promise<void> {
  await req("POST", `/notifications/${id}/read`, {});
}

export async function markAllNotificationsRead(): Promise<void> {
  await req("POST", "/notifications/read-all", {});
}

// ── Metadata ──
export interface EntityMeta {
  id?: string; name: string; label: string; tableName: string;
  // sys_entities.options: a JSON STRING on GET /metadata/entities, an object on admin replies.
  // `labelField` (#272) = the field that names a row of this table.
  options?: string | Record<string, unknown> | null;
  // specs/063 S7-9: only on the builder's draft-overlay list; "deleted" = marked "sẽ xoá khi xuất bản",
  // "new" = created after the last publish (the customer does not see it yet).
  draftState?: "new" | "deleted";
}
export async function listEntities(): Promise<EntityMeta[]> {
  const r = await req<{ data: EntityMeta[] }>("GET", "/metadata/entities");
  return r.data ?? [];
}

export interface FieldMeta {
  id?: string; name: string; label?: string; fieldType: string; isRequired?: boolean; isUnique?: boolean;
  options?: Record<string, unknown>;
  // Slice 1.2: sys_fields.default_value as served by GET /metadata/fields/:entity (omitempty) —
  // ALWAYS a string on the wire; the record engine coerces it per type at create time.
  // Set via PUT /admin/fields/:id {defaultValue} (POST /admin/fields does NOT accept it).
  defaultValue?: string;
  // W5 (mig 000033): admin display position — the endpoint SERVES fields sorted
  // `field_order ASC, name ASC`, so array order can be trusted as admin-intended order.
  fieldOrder?: number;
  // W5: per-FIELD admin description (≤500 runes) — shown in the Studio inspector and as a
  // subtle hint on App forms; distinct from a layout cell's per-VIEW helpText.
  description?: string;
  // field-lock-signal (DAILY-QUEUE #12): the CALLER's role may view this field but not edit it
  // (sys_field_acl can_edit=0). Per caller, ADDITIVE: present only when true — admins and
  // unrestricted roles get no key. A form renders it as text (no input) and never submits it;
  // a write that carries a new value for it is refused 403 FIELD_FORBIDDEN regardless. A HIDDEN
  // field (can_view=0) is not served at all — there is deliberately no key that says "hidden".
  readOnly?: boolean;
  // #272: this is the field that NAMES a row of its table (the builder's choice, else the derived
  // default: name/title/label/... then the first required text field). ADDITIVE: present only on that one field. lib/recordLabel.ts reads it.
  isLabel?: boolean;
  // #479: with isLabel — the builder CHOSE it (not the derived default). Ranking: this > a relation's own labelField > an inferred isLabel.
  isLabelExplicit?: boolean;
  // specs/063-draft-publish S7-8: set only on the builder's draft-overlay read of a delivered workspace.
  // "new" = born after the last publish (the customer does not see it yet); "edited" = has an unpublished edit;
  // pendingRequired = "Bắt buộc" is on in the draft but not live yet.
  // S7-9: "deleted" = the builder marked it "sẽ xoá khi xuất bản" (the customer still has it).
  draftState?: "new" | "edited" | "deleted";
  pendingRequired?: boolean;
}
export async function listFields(entity: string): Promise<FieldMeta[]> {
  const r = await req<{ data: FieldMeta[] }>("GET", `/metadata/fields/${entity}`);
  return r.data ?? [];
}

// ── Layout (L1 contract) — GET /metadata/layouts/:entity drives the dynamic renderer (L2).
// Verified live shape (see ../specs/028-layout-l2-renderer/spec.md §3):
//   data: { entityName, systemFields[], views: { edit|detail|list: LayoutView } }
//   • edit / detail view → { isDefault, version, sections: [{ title, columns, fields: LayoutCell[] }] }
//   • list view          → { isDefault, version, columns: [{ field, sortable, width }], defaultSort: { field, order } }
// A layout references fields BY NAME only — type/required/options come from listFields(); the
// per-cell object lets a layout OVERRIDE label/required/readonly/grid-span on top of the field meta.

// CELL: bare field name | per-cell override object | spacer (empty grid slot to keep columns aligned).
// W5: object cells also carry `helpText` (≤200 runes, lowercase key per contract §2.3) — per-VIEW
// helper text rendered under the widget; validated server-side (422 LAYOUT_INVALID beyond bounds).
// specs/054: `visibleWhen` shows the cell only when another field ON THE SAME layout meets a
// condition. The evaluator (Phase 2 FE-layout-visible-when) must pass the golden file
// nexus-platform/internal/services/testdata/visible_when_cases.json, same as the Go one.
export interface VisibleWhen {
  field: string;
  op: "eq" | "neq" | "in" | "notIn" | "empty" | "notEmpty";
  value?: string | number | boolean | Array<string | number | boolean>;
}
export type LayoutCell =
  | string
  | { field: string; labelOverride?: string; required?: boolean; readonly?: boolean; fullWidth?: boolean; colspan?: number; helpText?: string; visibleWhen?: VisibleWhen }
  | { spacer: true };

// A spacer cell narrows to this; use isSpacerCell() / cellField() helpers below to discriminate.
export interface SpacerCell { spacer: true }
export interface FieldCell {
  field: string; labelOverride?: string; required?: boolean; readonly?: boolean; fullWidth?: boolean; colspan?: number; helpText?: string;
  visibleWhen?: VisibleWhen;
}
export function isSpacerCell(cell: LayoutCell): cell is SpacerCell {
  return typeof cell === "object" && (cell as SpacerCell).spacer === true;
}
// Normalize any cell to its field-spec form (string → { field }); spacer → null.
export function cellField(cell: LayoutCell): FieldCell | null {
  if (typeof cell === "string") return { field: cell };
  if (isSpacerCell(cell)) return null;
  return cell as FieldCell;
}

// Section grid (edit/detail). `columns` = 1..4 (W5 widened 3→4; renderer caps to 1 on mobile).
export interface LayoutSection { title?: string; columns?: 1 | 2 | 3 | 4; collapsed?: boolean; fields: LayoutCell[] }

// List column. `sortable` is READ-ONLY for clients: the server stamps it on every served column at
// serve time from the field's real sort capability (system column / unique / relation / indexed —
// the same allowlist `?sort=` enforces), overriding anything stored. Writers (builder, packs) do
// not send it; the server tolerates but ignores the key on save (DAILY-QUEUE #4).
export interface LayoutListColumn { field: string; labelOverride?: string; width?: number; sortable?: boolean }
export interface LayoutDefaultSort { field: string; order: "asc" | "desc" }

export interface LayoutView {
  isDefault?: boolean;
  /** specs/063 S7-3: set only while the builder reads over the draft overlay of a delivered workspace. */
  draftState?: "edited" | "new";
  version?: number;
  sections?: LayoutSection[];        // edit | detail
  columns?: LayoutListColumn[];      // list
  defaultSort?: LayoutDefaultSort;   // list
  // DECLARED view fields (DAILY-QUEUE #5) — which picklist the Kanban board groups by and which
  // date field the Calendar lays out on. Absent = the renderer keeps its first-of-kind guess.
  // The server validates the type on save (picklist / date|datetime) and drops the key at serve
  // time when field ACL hides that field from this user. That is NOT a guarantee the name is
  // usable: a field deleted or retyped after the layout was saved still arrives here. The
  // renderers re-check name AND type (pickKanbanField, CalendarView's isDateField) and fall back
  // to the guess — do not remove those checks on the strength of the server-side validation.
  kanbanField?: string;              // list
  calendarField?: string;            // list
}
export type LayoutViewKind = "edit" | "detail" | "list";
export interface EntityLayout {
  entityName: string;
  systemFields?: string[];                          // audit hint rendered as a trailing block in detail
  views: Record<LayoutViewKind, LayoutView>;
}
export async function getLayout(entity: string): Promise<EntityLayout | null> {
  const r = await req<{ data: EntityLayout }>("GET", `/metadata/layouts/${entity}`);
  return r.data ?? null;
}

// ── Workflow / Report (EPIC 4/6 admin) — list read (rỗng cho tới khi tạo) ──
export interface WorkflowSummary { id: string; name: string; entityName?: string; trigger?: string; isActive?: boolean; /** Set only while the builder reads over the draft overlay of a delivered workspace (specs/063). */ draftState?: "live" | "edited" | "new" | "deleted"; [k: string]: unknown }
export async function listWorkflows(): Promise<WorkflowSummary[]> {
  const r = await req<{ data: { workflows: WorkflowSummary[] } }>("GET", "/admin/workflows");
  return r.data?.workflows ?? [];
}
/** True when the record carries a change the customer has not seen yet. */
export const hasDraftChange = (s?: string): boolean => s === "edited" || s === "new" || s === "deleted";
export interface ReportSummary { id: string; name: string; entityName?: string; type?: string; config?: Record<string, unknown>; isActive?: boolean; draftState?: DraftState; [k: string]: unknown }
// GET /reports — non-admin catalog (auth-only, tenant-scoped). Verified live 2026-07-01.
export async function listReports(): Promise<ReportSummary[]> {
  const r = await req<{ data: { reports: ReportSummary[] } }>("GET", "/reports");
  return r.data?.reports ?? [];
}
// Report run response — two shapes from the server:
//   tabular:        { type:"tabular", columns:[{name,label}], rows:[{id,...}], total:N }
//   summary/matrix: { type:"summary"|"matrix", groups?:[{key,value,count}], value?, count? }
// specs/049 grid path (optional keys — present only when the config has subGroups/columnGroup/having):
//   tree:  levels[] + groups[].children + total (+ having echo)
//   grid:  rowAxis + columnAxis + rows (row totals) + columns (column totals) + sparse cells + total;
//          groups = rows, for readers that only know the one-axis shape.
// Types only (F0) — the viewer reads the SHAPE to say honestly what it does not draw yet.
export interface ReportRunColumn { name: string; label: string }
export interface ReportRunRow { id: string; [k: string]: unknown }
export interface ReportAxis { field: string; bucket?: string }
export interface ReportHaving { on: "value" | "count"; op: string; values: string[] }
export interface ReportGroupRow extends AggGroupRow { children?: ReportGroupRow[] }
export interface ReportGridCell { r: number; c: number; value: string; count: number }
export type ReportRunData =
  | { type: "tabular"; columns: ReportRunColumn[]; rows: ReportRunRow[]; total: number; draftState?: DraftState }
  | {
      type: "summary" | "matrix"; draftState?: DraftState; groups?: ReportGroupRow[]; value?: string; count?: number; currencyCode?: string;
      levels?: ReportAxis[]; total?: { value: string; count: number }; having?: ReportHaving | null;
      rowAxis?: ReportAxis; columnAxis?: ReportAxis; rows?: ReportGroupRow[]; columns?: ReportGroupRow[];
      cells?: ReportGridCell[];
    };
// GET /reports/:id/run — executes the report query. Verified live 2026-07-01.
// On error (e.g. groupBy field not indexed) backend returns 422 with {code,error}; the
// req() helper throws ApiError — catch it in the component and show an error state.
// W4: optional ?dateFrom/?dateTo/?ownerId run overlay (api-conventions §4.4) — ANDed onto
// the saved config at request time, nothing persisted. Invalid dates → 422 RUN_DATE_INVALID.
export async function runReport(id: string, filters?: RunFilters): Promise<ReportRunData> {
  const qs = new URLSearchParams(filters ? filtersToParams(filters) : {}).toString();
  const r = await req<{ data: ReportRunData }>("GET", `/reports/${id}/run${qs ? `?${qs}` : ""}`);
  return r.data;
}

// GET /reports/:id/export.csv — W4 server-side CSV with the SAME overlay params as /run
// (parity guaranteed by the backend: both go through ParseRunOverlay + the same query).
// Auth rides the Bearer header (no ?token=), so we fetch → blob → anchor download.
// A pre-stream error (404/422/403) arrives as the normal JSON envelope → ApiError.
export async function downloadReportCSV(id: string, filename: string, filters?: RunFilters): Promise<void> {
  const qs = new URLSearchParams(filters ? filtersToParams(filters) : {}).toString();
  const t = getToken();
  const res = await fetch(`${BASE}/reports/${id}/export.csv${qs ? `?${qs}` : ""}`, {
    headers: { ...(t ? { Authorization: `Bearer ${t}` } : {}), ...previewHeaders("GET") },
  });
  if (!res.ok) {
    const json = await res.json().catch(() => ({}));
    throw new ApiError(res.status, json.code ?? "EXPORT_FAILED", json.error ?? res.statusText, json.details ?? {});
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

// ── Dashboard (EPIC 6 M2) — saved dashboards of widgets; run computes each widget via the aggregate engine ──
// W4: `bucket` (day|week|month|quarter|year, stored normalized lowercase) turns a date groupBy
// into a calendar time series — empty buckets are omitted server-side, FE zero-fills (lib/timeBuckets).
// One filter predicate stored on a widget (same dialect as report/list filters). Round-tripped
// verbatim by GET/run — modeled here so the full-replace PUT persist path never drops it.
export interface DashboardWidgetFilter { field: string; op?: string; values?: string[] }
export interface DashboardWidgetDef {
  id: string; title: string; type: string; entity: string; metric: string; field?: string; groupBy?: string;
  bucket?: string;
  // DASH-WIDGET-SCHEMA: react-grid-layout coords persisted on the shared dashboard definition.
  // Both ≥1 = a positioned/sized tile; both 0 (or absent) = the "layout unset" sentinel. The
  // backend rejects a single-zero extent (422 WIDGET_LAYOUT_INVALID) — see lib/dashWidgetPayload.
  x?: number; y?: number; w?: number; h?: number;
  filters?: DashboardWidgetFilter[];
  // Queue #294: a pinned report's relationship chain (same shape as report config.joins), so
  // groupBy/field may be "<relationship>.<field>" tokens and the card shows the report's number.
  joins?: { relationship: string; optional?: boolean }[];
}
export interface DashboardSummary { id: string; name: string; description?: string | null; widgets: DashboardWidgetDef[]; draftState?: DraftState }

/** A dashboard as the OPEN catalog returns it. Its widgets carry only {id, type} — enough to count
 *  and preview by shape, and deliberately nothing that names an entity, metric, field or filter:
 *  GET /dashboards is authenticated but UNGRANTED by design, so a caller with no access to those
 *  tables must not learn what they are from a discovery call. Use getAdminDashboard(id) when you
 *  actually need the widget definitions (and expect the admin gate). */
export interface DashboardCatalogItem {
  id: string; name: string; description?: string | null;
  widgets: { id: string; type: string }[];
  draftState?: DraftState;
}
// GET /dashboards — non-admin catalog (auth-only, tenant-scoped). Verified live 2026-07-01.
export async function listDashboards(): Promise<DashboardCatalogItem[]> {
  const r = await req<{ data: { dashboards: DashboardCatalogItem[] } }>("GET", "/dashboards");
  return r.data?.dashboards ?? [];
}
// Each run-widget carries either a `result` (scalar value+count, or grouped groups[]) or an `error`/`code`.
// label: display name of the key (specs/049 S6 — relation/user/picklist axes); drill keeps `key`.
export interface AggGroupRow { key: string; value: string; count: number; label?: string }
// currencyCode: the aggregated field's ISO currency when it is a currency metric (else ""/absent);
// lets the client format the value in the field's own currency, not a hardcoded VNĐ (DASH-CURRENCY).
export interface WidgetResult { count: number; grouped: boolean; value?: string; groups?: AggGroupRow[]; currencyCode?: string }
export interface WidgetRun { widgetId: string; title: string; type: string; result?: WidgetResult | null; error?: string; code?: string }
export interface DashboardRun { dashboard: DashboardSummary; widgets: WidgetRun[] }
// W4 run overlay (api-conventions §4.4): ?dateFrom/?dateTo → created_at bounds, ?ownerId →
// created_by — ANDed onto EVERY widget's saved filters server-side, nothing persisted.
// The canonical filter model lives in lib/runFilters (RunFilters); this alias keeps the
// pre-W4 name importable.
export type DashboardGlobalFilters = RunFilters;

export async function runDashboard(id: string, filters?: RunFilters): Promise<DashboardRun> {
  const qs = new URLSearchParams(filters ? filtersToParams(filters) : {}).toString();
  const r = await req<{ data: DashboardRun }>("GET", `/dashboards/${id}/run${qs ? `?${qs}` : ""}`);
  return r.data;
}

// ── Records (dynamic entity) ──
// A relation field value comes back as a bare id string normally; with ?expand=field it is resolved
// to { id, label } (RM-07 — handler/dynrepo/expand.go). FieldDisplay shows label, never the raw UUID.
// Verified live: GET /contacts/:id            → data.account = "ce3fcd24-…"
//                GET /contacts/:id?expand=account → data.account = { id: "ce3fcd24-…", label: "VietBuild JSC" }
export interface RelationRef { id: string; label: string }
export function isRelationRef(v: unknown): v is RelationRef {
  return typeof v === "object" && v !== null && "id" in v && "label" in v;
}

// Live record shape (verified): { id, tenantId, data:{…}, version, createdAt, updatedAt }.
// `version` powers optimistic-lock (echoed back on update); `createdAt/updatedAt` feed the detail
// systemFields block. `data` is the flat { fieldName: value } map (relation values per RelationRef above).
export interface ApiRecord {
  id: string;
  data: Record<string, unknown>;
  version: number;
  tenantId?: string;
  createdAt?: string;
  updatedAt?: string;
  // The record's CURRENT owner (obj_*.owner_id when set, else created_by) — the SAME value the
  // ACL row filter enforces on. Optional because an older backend omits the key entirely; a
  // screen must feature-detect rather than render an empty owner. Added 2026-07-29: the read path
  // never returned it, so "Đổi chủ" was a button whose effect nothing could show.
  ownerId?: string;
  // true when the caller's VIEW scope reaches this record ONLY through a manual share (specs/051 FR-009,
  // GET /:entity/:id only). It does not decide whether the record can be edited — canEdit does (#43): a
  // share takes no edit right away (view=own, edit=team on a shared team record keeps Sửa). The page uses
  // it to word the read-only notice, to hide Xoá (a DELETE on it is refused 403 RECORD_SHARED_READ_ONLY)
  // and to keep Trao đổi read-only (a note's floor is the view scope without the share).
  // Absent in every other case (own record, AccessAll, admin, list rows).
  sharedWithMe?: boolean;
  // false when the caller's EDIT scope does not reach this record: their VIEW scope does (view=all,
  // edit=own: a colleague's lead), the record reaches them only through a share and their edit scope
  // does not cover it, or they hold no edit grant — GET /:entity/:id only (DAILY-QUEUE #334, #41).
  // Absent means "no restriction known" (own record, edit=all, admin, a shared record the edit scope
  // reaches), never "forbidden": the page hides Sửa / Chia sẻ / Đổi chủ and the form turns Lưu off on
  // `=== false`; the save itself would be refused 403 FORBIDDEN (#18) or RECORD_SHARED_READ_ONLY.
  canEdit?: boolean;
}
// Pagination block returned by the list endpoint in OFFSET mode (?page=&limit=).
export interface ListPagination { page: number; limit: number; total: number; totalPages: number }
// HIDDEN-COL-SIGNAL — additive `meta` block on the list envelope (api-conventions §14).
// `hiddenColumns` = count of DISTINCT declared fields of the entity hidden from the CALLER's role
// by field-ACL (can_view=0) — the same per-request set the server strips from every row. The
// server emits `meta` ONLY when the count > 0; it is ABSENT for admins (who see every column) and
// when field-level security is off — so a chip driven by it can never be a false positive.
export interface ListMeta { hiddenColumns?: number }
export interface ListPage { records: ApiRecord[]; pagination: ListPagination; meta?: ListMeta }
// Pagination-aware list fetch — returns BOTH the rows and the pagination block so callers
// can drive page++ "load more". hasMore is derived: pagination.page < pagination.totalPages.
// Guards a missing pagination block (defaults to a single page) so it never throws on odd responses.
// `meta` (hiddenColumns, §14) is passed through verbatim when present — absent means "nothing hidden".
export async function listRecordsPage(entity: string, query = ""): Promise<ListPage> {
  const r = await req<{ data: ApiRecord[]; pagination?: Partial<ListPagination>; meta?: ListMeta }>(
    "GET", `/${entity}${query ? `?${query}` : ""}`,
  );
  const records = r.data ?? [];
  const p = r.pagination ?? {};
  const pagination: ListPagination = {
    page: p.page ?? 1,
    limit: p.limit ?? records.length,
    total: p.total ?? records.length,
    totalPages: p.totalPages ?? 1,
  };
  return { records, pagination, meta: r.meta };
}
// Pagination block returned by the list endpoint in CURSOR / keyset mode (?after=&limit=). No
// `total`/`totalPages` (cursor mode avoids COUNT(*) → O(1) per page). `nextCursor` is the opaque
// base64 cursor for the NEXT page; null on the last page (api-conventions "Chế độ 2 — Cursor/Keyset").
export interface CursorPage { records: ApiRecord[]; limit: number; nextCursor: string | null; meta?: ListMeta }
// Cursor-aware list fetch (contract §5.7). Pass an `after` cursor to page forward; omit it for the
// first page. Parses `pagination.nextCursor` (null = last page). Malformed/expired cursor → the
// server throws ApiError 400 INVALID_CURSOR; a non-indexed sort → 422 (SORT_NOT_ALLOWED /
// CURSOR_REQUIRES_INDEXED_SORT) — callers should have picked the mode via lib/pagination first.
//
// First page: pass `paginate=cursor` (no `after`, no `page`) — the backend answers with a cursor-shaped
// page 1 (specs/039-cursor-bootstrap). The record list pages through it via lib/pagination
// fetchListPage (FE-cursor-bootstrap-a). `meta` (hiddenColumns, §14) rides the cursor envelope too.
export async function listRecordsCursorPage(entity: string, query = ""): Promise<CursorPage> {
  const r = await req<{ data: ApiRecord[]; pagination?: { limit?: number; nextCursor?: string | null }; meta?: ListMeta }>(
    "GET", `/${entity}${query ? `?${query}` : ""}`,
  );
  const records = r.data ?? [];
  const p = r.pagination ?? {};
  const nc = p.nextCursor;
  return {
    records,
    limit: p.limit ?? records.length,
    nextCursor: typeof nc === "string" && nc.length > 0 ? nc : null,
    meta: r.meta,
  };
}
// Back-compat: existing callers (FE-01 refetch, LiveAccounts, widgets relation picker) just
// want the rows — delegate to listRecordsPage and drop the pagination block.
export async function listRecords(entity: string, query = ""): Promise<ApiRecord[]> {
  const { records } = await listRecordsPage(entity, query);
  return records;
}
// GET /:entity/:id — read ONE record (detail + edit prefill). `expand` resolves relation fields to
// { id, label } (RM-07) so the UI shows a name instead of a raw UUID — pass the relation field names.
// Verified live: GET /contacts/:id?expand=account → data.account = { id, label }. Returns {id,tenantId,data,version,…}.
// expand → resolve relation/lookup fields to {id,label}; rollup → compute aggregate fields
// (SUM/COUNT/AVG/MIN/MAX) read-time (opt-in so a plain GET never pays the child-aggregate cost).
export async function getRecord(entity: string, id: string, expand?: string[], rollup?: string[]): Promise<ApiRecord> {
  const parts: string[] = [];
  if (expand && expand.length) parts.push(`expand=${expand.map(encodeURIComponent).join(",")}`);
  if (rollup && rollup.length) parts.push(`rollup=${rollup.map(encodeURIComponent).join(",")}`);
  const qs = parts.length ? `?${parts.join("&")}` : "";
  const r = await req<{ data: ApiRecord }>("GET", `/${entity}/${id}${qs}`);
  return r.data;
}
// POST /:entity — create a record. `data` is a flat {fieldName: value} map (relation = id string).
// On 422 the thrown ApiError carries .code + .details: RECORD_REQUIRED_MISSING → details.field (per-field);
// other value errors (type/email/range/too_long/pattern/picklist) → details:{} (form-level banner only).
// Returns { id } on success (verified live: POST /contacts → { data: { id } }, 201).
export async function createRecord(entity: string, data: Record<string, unknown>): Promise<{ id: string }> {
  const r = await req<{ data: { id: string } }>("POST", `/${entity}`, data);
  return r.data;
}
// PUT /:entity/:id — update a record (partial). Pass the record's `version` for optimistic locking;
// on a stale version the server throws ApiError 409 RECORD_VERSION_CONFLICT with details.currentVersion
// (verified live). Omit `version` to skip the check. Same per-type value shapes as createRecord.
export async function updateRecord(entity: string, id: string, data: Record<string, unknown>, version?: number): Promise<void> {
  await req("PUT", `/${entity}/${id}`, version !== undefined ? { ...data, version } : data);
}
// DELETE /:entity/:id — soft-delete a record.
export async function deleteRecord(entity: string, id: string): Promise<void> {
  await req("DELETE", `/${entity}/${id}`, undefined);
}
// PUT /:entity/:id/owner — reassign the record's owner (TRỤ 2 mục 2 "Giao việc & đổi chủ",
// handlers/dynamic_record_handler.go SetOwner). Authorization is SERVER-side (manager with
// edit-all, or admin — RecordService.SetOwner): 403 FORBIDDEN otherwise, 422
// RECORD_OWNER_INVALID when userId is not an ACTIVE user of this tenant, 404 RECORD_NOT_FOUND.
// The FE must NOT pre-hide the action on a permission guess — call and show the real error.
export async function setRecordOwner(entity: string, id: string, userId: string): Promise<void> {
  await req("PUT", `/${entity}/${id}/owner`, { userId });
}

// ── Manual record shares — read-only (specs/051-record-manual-share, api-conventions §31) ──
// All three need the edit grant AND the record in the caller's edit scope (server-side): 403
// RECORD_SHARED_READ_ONLY for someone who only sees it through a share, 404 when out of scope.
// POST: 422 RECORD_SHARE_USER_INVALID (not an active user of this tenant) / RECORD_SHARE_SELF;
// sharing again with someone already shared is 201, never a duplicate. DELETE of a share that is
// not live: 404 RECORD_SHARE_NOT_FOUND.
export interface RecordShare {
  // A share names ONE grantee: a person (userId) or a whole team (teamId, DAILY-QUEUE #224).
  userId?: string;
  teamId?: string;
  teamName?: string;
  userName?: string;
  userEmail?: string;
  sharedBy: string;
  createdAt: string;
}
export async function listRecordShares(entity: string, id: string): Promise<RecordShare[]> {
  const r = await req<{ data: RecordShare[] }>("GET", `/${entity}/${id}/shares`);
  return r.data ?? [];
}
export async function shareRecord(entity: string, id: string, userId: string): Promise<RecordShare> {
  const r = await req<{ data: RecordShare }>("POST", `/${entity}/${id}/shares`, { userId });
  return r.data;
}
// Share with a whole team: its members (and the members of any team above it) can open the record,
// read-only. 422 RECORD_SHARE_TEAM_INVALID when the team is not one of this tenant's.
export async function shareRecordWithTeam(entity: string, id: string, teamId: string): Promise<RecordShare> {
  const r = await req<{ data: RecordShare }>("POST", `/${entity}/${id}/shares`, { teamId });
  return r.data;
}
// granteeId is the person's OR the team's id — the server keys a revoke on either.
export async function unshareRecord(entity: string, id: string, granteeId: string): Promise<void> {
  await req("DELETE", `/${entity}/${id}/shares/${encodeURIComponent(granteeId)}`, undefined);
}

// GET /teams/directory — the tenant's teams (id, name) for ANY signed-in user: the share dialog's team
// picker (/admin/teams is admin-only, but whoever may edit a record may share it).
export interface TeamDirectoryEntry { id: string; name: string }
export async function listTeamDirectory(): Promise<TeamDirectoryEntry[]> {
  const r = await req<{ data: TeamDirectoryEntry[] }>("GET", "/teams/directory");
  return r.data ?? [];
}

// ── Recycle bin — soft-deleted list + restore (W9b, api-conventions §13.1) ──
// GET /:entity/deleted — mirror of the live list (same offset envelope + filters) but filtered to
// deleted_at IS NOT NULL. Offset-only (no cursor/expand). Rides the same ACL/A5/field-ACL as the
// live list, so a caller only ever sees records they could have seen alive.
export async function listDeletedPage(entity: string, query = ""): Promise<ListPage> {
  const r = await req<{ data: ApiRecord[]; pagination?: Partial<ListPagination> }>(
    "GET", `/${entity}/deleted${query ? `?${query}` : ""}`,
  );
  const records = r.data ?? [];
  const p = r.pagination ?? {};
  const pagination: ListPagination = {
    page: p.page ?? 1,
    limit: p.limit ?? records.length,
    total: p.total ?? records.length,
    totalPages: p.totalPages ?? 1,
  };
  return { records, pagination };
}
// POST /:entity/:id/restore — clear deleted_at (needs the `delete` grant). Returns { id } on 200.
// Throws ApiError: 404 RECORD_NOT_FOUND (not soft-deleted / cross-tenant / out of A5 scope),
// 409 RECORD_DUPLICATE + details.field (a live record re-took a unique slot), 403 FORBIDDEN.
export async function restoreRecord(entity: string, id: string): Promise<{ id: string }> {
  const r = await req<{ data: { id: string } }>("POST", `/${entity}/${id}/restore`, {});
  return r.data;
}

// POST /:entity/:id/purge — delete ONE soft-deleted record for good (specs/056, api-conventions §13.1; builder tier only).
// `dryRun` is sent EXPLICITLY on every call: the server's own default is "dry run", and a client must never lean on it.
// Throws ApiError: 409 RECORD_PURGE_LIVE_CHILDREN (details.blockers), 409 RECORD_NOT_IN_RECYCLE_BIN, 422 RECORD_PURGE_TOO_LARGE
// (details.max), 503 RECORD_WRITE_CONFLICT (retry), 403 FORBIDDEN, 404 RECORD_NOT_FOUND (or no route: operator tier not wired).
export interface RecordPurgeReport {
  entity: string; id: string; dryRun: boolean;
  records: { entity: string; id: string }[];
  cleared: { entity: string; id: string; field: string }[];
  blocked: { entity: string; field: string; count: number }[];
  /** PHYSICAL tables (obj_*, sys_files…): a count source only, never shown by name. */
  tables: { table: string; rows: number }[];
  totalRows: number; blobsRemoved: number; blobsNote?: string;
}
export async function purgeRecord(entity: string, id: string, opts: { dryRun: boolean }): Promise<RecordPurgeReport> {
  // `..` / `.` survive encodeURIComponent and fetch would collapse them into another route.
  if (/^\.+$/.test(entity) || /^\.+$/.test(id)) throw new TypeError("purgeRecord: invalid entity or id");
  const r = await req<{ data: RecordPurgeReport }>("POST", `/${encodeURIComponent(entity)}/${encodeURIComponent(id)}/purge`, { dryRun: opts.dryRun === true });
  const d = r.data;
  return { ...d, records: d.records ?? [], cleared: d.cleared ?? [], blocked: d.blocked ?? [], tables: d.tables ?? [] };
}

// ── Saved views — sys_views CRUD (W9b, api-conventions §13.2) ──
// A view is an FE-defined UI preset (filters/sort/density/columns) stored per entity. `config` is an
// opaque JSON object (FE owns the shape — see lib/viewConfig.ts). userId null == SHARED (tenant-wide,
// admin-managed); non-null == PERSONAL (owner-only). `shared` is the boolean convenience the server
// derives from userId.
export interface SavedView {
  id: string;
  userId: string | null;
  shared: boolean;
  entityName: string;
  name: string;
  config: Record<string, unknown>;
  createdAt?: string;
  updatedAt?: string;
}
// GET /:entity/views — visible views (shared ∪ caller's personal), newest-first.
export async function listViews(entity: string): Promise<SavedView[]> {
  const r = await req<{ data: SavedView[] }>("GET", `/${entity}/views`);
  return r.data ?? [];
}
// POST /:entity/views — create. `shared:true` requires admin (else 403 VIEW_FORBIDDEN).
export async function createView(
  entity: string,
  body: { name: string; config: Record<string, unknown>; shared?: boolean },
): Promise<{ id: string }> {
  const r = await req<{ data: { id: string } }>("POST", `/${entity}/views`, body);
  return r.data;
}
// PUT /:entity/views/:id — full-replace of the mutable part { name, config }. Ownership immutable.
export async function updateView(
  entity: string,
  id: string,
  body: { name: string; config: Record<string, unknown> },
): Promise<{ id: string }> {
  const r = await req<{ data: { id: string } }>("PUT", `/${entity}/views/${id}`, body);
  return r.data;
}
// DELETE /:entity/views/:id — soft-delete a view (204).
export async function deleteView(entity: string, id: string): Promise<void> {
  await req("DELETE", `/${entity}/views/${id}`, undefined);
}

// ── Mass ops — POST /:entity/bulk-update | /:entity/bulk-delete (ENG-MASSOP) ──
// One shared `values` patch (bulk-update) or a soft-delete (bulk-delete) applied to ≤500 ids
// (server MAX_BULK_OPS) per call, executed server-side through the SAME single-record engine
// path per row (ACL row-scope, field-ACL, validation, workflow, audit). Always 200 with per-row
// outcomes when the batch ran — including values-level defects, which fail each row with the
// same RECORD_* code a single PUT returns; request-level defects throw ApiError 422
// (BULK_EMPTY / BULK_TOO_MANY / BULK_VALUES_EMPTY). bulk-update needs the `edit` grant,
// bulk-delete the `delete` grant (403 FORBIDDEN otherwise).
export interface MassOpRowResult {
  id: string;
  ok: boolean;
  code?: string;                     // present when ok === false (same codes as single update/delete)
  error?: string;
  details?: Record<string, unknown>; // e.g. { field: "email" } for RECORD_DUPLICATE
}

// FE-normalized shape: the server counts succeeded rows as `updated` (bulk-update) or
// `deleted` (bulk-delete); both fold into `succeeded` here so UI code handles ONE shape.
export interface MassOpResult {
  results: MassOpRowResult[];
  succeeded: number;
  failed: number;
}

// Raw per-call server envelope (data.*) before normalization.
interface MassOpEnvelope {
  results?: MassOpRowResult[];
  updated?: number;
  deleted?: number;
  failed?: number;
}

// Server default cap per call — a larger selection is chunked client-side, envelopes merged.
const MASS_OP_CHUNK = 500;

/**
 * Studio scope gate (specs/078 FR-131): checked before EVERY chunk, not only the first. Once it says no (or the
 * signal aborted) the remaining chunks are never sent; `partial` is what already landed.
 */
export interface MassOpGuard {
  assertScope?: () => boolean;
  signal?: AbortSignal;
}

export class MassOpStoppedError extends Error {
  constructor(
    readonly partial: MassOpResult,
    readonly total: number,
  ) {
    super("MASS_OP_STOPPED");
  }
}

async function massOpChunks(
  ids: string[],
  run: (chunk: string[]) => Promise<MassOpEnvelope>,
  guard: MassOpGuard = {},
): Promise<MassOpResult> {
  const total: MassOpResult = { results: [], succeeded: 0, failed: 0 };
  for (let i = 0; i < ids.length; i += MASS_OP_CHUNK) {
    if (guard.signal?.aborted || (guard.assertScope && !guard.assertScope())) throw new MassOpStoppedError(total, ids.length);
    const part = await run(ids.slice(i, i + MASS_OP_CHUNK));
    total.results.push(...(part.results ?? []));
    total.succeeded += part.updated ?? part.deleted ?? 0;
    total.failed += part.failed ?? 0;
  }
  return total;
}

export async function bulkUpdateRecords(
  entity: string,
  ids: string[],
  values: Record<string, unknown>,
  guard?: MassOpGuard,
): Promise<MassOpResult> {
  return massOpChunks(
    ids,
    async (chunk) => {
      const r = await req<{ data: MassOpEnvelope }>("POST", `/${entity}/bulk-update`, { ids: chunk, values });
      return r.data;
    },
    guard,
  );
}

export async function bulkDeleteRecords(entity: string, ids: string[], guard?: MassOpGuard): Promise<MassOpResult> {
  return massOpChunks(
    ids,
    async (chunk) => {
      const r = await req<{ data: MassOpEnvelope }>("POST", `/${entity}/bulk-delete`, { ids: chunk });
      return r.data;
    },
    guard,
  );
}

// ── Bulk (multi-row) create — POST /:entity/bulk (ENG-BATCH #1, ../specs/009-bulk-create/spec.md §2) ──
// Same per-type value shapes as createRecord/updateRecord (currency → {amount,currencyCode}, etc.)
// — the caller builds each row's payload via lib/recordPayload's buildPayload, same as LayoutForm.
export type BulkMode = "atomic" | "partial";

export interface BulkRowResult {
  index: number;                    // position in the submitted `records` array
  status: "created" | "failed" | "skipped";
  id?: string;                      // present when status === "created"
  code?: string;                    // present when status !== "created" (same codes as single create)
  error?: string;
  details?: Record<string, unknown>; // e.g. { field: "email" } for a per-field validation error
}

export interface BulkResult {
  mode: BulkMode;
  created: number;
  failed: number;
  skipped?: number;                 // atomic-abort only — rows that were themselves valid but
                                     // blocked by another row's failure (code BULK_ATOMIC_ABORTED)
  results: BulkRowResult[];
}

// POST /:entity/bulk?mode=atomic|partial. UNLIKE every other endpoint, a per-row envelope is a
// LEGITIMATE response even when the HTTP status is non-2xx: mode=atomic aborts the whole batch
// with 422 while still returning {data:{mode,created,failed,skipped,results}} — that is not a
// generic error, it is "0 rows written, here is exactly why for each row" and the caller
// (BatchCreateForm) needs the full results array to highlight rows. So this bypasses req<T>()'s
// generic "!res.ok → throw ApiError" rule and discriminates on BODY SHAPE instead: a `data.results`
// array present → return it as a normal BulkResult (201 all-created, 200 partial-mixed, and 422
// atomic-abort all take this path). Anything else (422 BULK_EMPTY/BULK_TOO_LARGE/BULK_INVALID_MODE,
// 401/403, or an infra 5xx that CreateBulk bubbles straight to respondError's {error,code,details}
// shape with no `data` — see ../specs/009-bulk-create/spec.md §10 "infra-vs-validation classification") throws
// ApiError exactly like req<T>() does, so the caller's catch block still works.
export async function createRecordsBulk(
  entity: string,
  records: Record<string, unknown>[],
  mode: BulkMode = "atomic",
  _retried = false,
): Promise<BulkResult> {
  return postBulk(`/${entity}/bulk`, records, mode, _retried);
}

// POST /admin/sample-data/records/:entity?mode= (specs/064 S9-4, builderGated) — the SAME contract as
// /:entity/bulk (shared body-shape discrimination below), but the created rows are marked as sample
// data (so the "Dữ liệu mẫu" card counts them and they can be wiped later) and fire no automation.
export async function createSampleRecords(
  entity: string,
  records: Record<string, unknown>[],
  mode: BulkMode = "atomic",
): Promise<BulkResult> {
  return postBulk(`/admin/sample-data/records/${entity}`, records, mode, false);
}

async function postBulk(
  path: string,
  records: Record<string, unknown>[],
  mode: BulkMode,
  _retried: boolean,
): Promise<BulkResult> {
  const headers: Record<string, string> = { "Content-Type": "application/json", ...previewHeaders("POST") };
  const t = getToken();
  if (t) headers["Authorization"] = `Bearer ${t}`;
  const res = await fetch(`${BASE}${path}?mode=${mode}`, {
    method: "POST",
    headers,
    body: JSON.stringify({ records }),
  });

  // Same recover-and-retry-once contract as req<T>() (checked BEFORE the body-shape
  // discriminator below — a 401 body never carries `data`, so order doesn't actually matter for
  // correctness here, but keeping it first matches req<T>()'s structure for readability).
  if (res.status === 401 && isPreviewMode()) {
    onPreview401(_retried);
    return postBulk(path, records, mode, true);
  }
  if (res.status === 401 && !_retried) {
    await recover401();
    return postBulk(path, records, mode, true);
  }

  const json = await res.json().catch(() => ({}));
  if (json && typeof json === "object" && json.data && Array.isArray(json.data.results)) {
    return json.data as BulkResult;
  }
  throw new ApiError(
    res.status,
    json.code ?? "UNKNOWN",
    json.error ?? res.statusText,
    (json.details ?? {}) as ApiErrorDetails,
  );
}

// GET /admin/sample-data?entities=a,b (specs/064 S9-4, builderGated) — how many sample rows each table
// holds. Without `entities` the server answers for every table that has marks. `count` = live + inBin;
// `bySource` splits by origin (rows seeded before the feature existed carry no mark and are not counted).
export interface SampleEntitySummary {
  entity: string;
  label: string;
  count: number;
  live: number;
  inBin: number;
  bySource: { generator: number; ai: number; pack: number };
}
export interface SampleSummary { total: number; entities: SampleEntitySummary[] }

export async function getSampleDataSummary(entities?: string[]): Promise<SampleSummary> {
  const qs = entities && entities.length > 0 ? `?entities=${encodeURIComponent(entities.join(","))}` : "";
  const r = await req<{ data: SampleSummary }>("GET", `/admin/sample-data${qs}`);
  return { total: r.data?.total ?? 0, entities: r.data?.entities ?? [] };
}

// POST /admin/sample-data/generate (specs/064 S9-5, builderGated) — Vietnamese sample rows for the chosen
// tables, built from their field definitions (no AI), parents first. `created` per table, `failed` rows the
// record layer refused (bulk-create code), `skipped` tables that cannot be filled (a SAMPLE_* reason),
// `addedParents` required parent tables the server generated on top of the choice. 409 SAMPLE_BUSY while
// another sample batch of the tenant runs. Not idempotent: calling again seeds more rows.
export interface SampleGenerateResult {
  batchId: string;
  created: { entity: string; count: number }[];
  failed: { entity: string; index: number; code: string; error: string }[];
  skipped: { entity: string; code: string }[];
  addedParents: string[];
}

// source: "fields" = from each table's field definitions; "pack" = the sample rows of the built-in
// pack installed here (S9-6); "auto" = pack where it has the table, fields for the rest.
export type SampleSource = "auto" | "fields" | "pack";

export async function generateSampleData(
  entities: string[],
  perEntity: number,
  source: SampleSource = "fields",
): Promise<SampleGenerateResult> {
  const r = await req<{ data: SampleGenerateResult }>("POST", "/admin/sample-data/generate", {
    entities,
    perEntity,
    source,
  });
  return {
    batchId: r.data?.batchId ?? "",
    created: r.data?.created ?? [],
    failed: r.data?.failed ?? [],
    skipped: r.data?.skipped ?? [],
    addedParents: r.data?.addedParents ?? [],
  };
}

// POST /admin/sample-data/wipe (specs/064 S9-7, builderGated) — hard-deletes every sample row (live and in
// the bin) of the given tables (omitted = every table with marks) that no real data points at. `expectTotal`
// is the number of rows the builder saw: the server refuses with 409 SAMPLE_WIPE_STALE {details.current}
// when it reads another number (nothing deleted), 422 SAMPLE_WIPE_CONFIRM_REQUIRED without it, 409
// SAMPLE_BUSY while another sample batch runs. `kept` lists the rows left alone — `reason` is
// referenced_by_real | field_default | too_large | busy — with what blocks each (`blockedBy`).
export interface SampleWipeKept {
  entity: string;
  recordId: string;
  reason: "referenced_by_real" | "field_default" | "too_large" | "busy";
  blockedBy: { entity: string; field: string; count: number; kind?: string }[];
}
export interface SampleWipeResult {
  deleted: { entity: string; count: number }[];
  kept: SampleWipeKept[];
  blobsRemoved: number;
  blobsNote?: string;
  sequencesReset: { entity: string; field: string }[];
}

export async function wipeSampleData(expectTotal: number, entities?: string[]): Promise<SampleWipeResult> {
  const r = await req<{ data: SampleWipeResult }>("POST", "/admin/sample-data/wipe", { expectTotal, entities });
  return {
    deleted: r.data?.deleted ?? [],
    kept: (r.data?.kept ?? []).map((k) => ({ ...k, blockedBy: k.blockedBy ?? [] })),
    blobsRemoved: r.data?.blobsRemoved ?? 0,
    blobsNote: r.data?.blobsNote,
    sequencesReset: r.data?.sequencesReset ?? [],
  };
}

// ── Per-record audit history (EPIC 5) ──
// GET /audit-log/:entityName/:recordId — paginated change history for ONE record.
// Returns the same AuditEntry shape as the global audit log; cursor pagination via `?after=&limit=`.
export async function listRecordHistory(
  entity: string,
  recordId: string,
  limit = 20,
  after?: string,
): Promise<{ entries: AuditEntry[]; nextCursor?: string }> {
  const p = new URLSearchParams({ limit: String(limit) });
  if (after) p.set("after", after);
  const r = await req<{ data: AuditEntry[]; pagination?: { nextCursor?: string } }>(
    "GET", `/audit-log/${entity}/${recordId}?${p}`,
  );
  return { entries: r.data ?? [], nextCursor: r.pagination?.nextCursor };
}

// ── File operations (EPIC 3) ──
// Files live in a record's file/image field as an array [{id, name}] in the record's .data.
// There is NO GET /:entity/:id/files list endpoint — list comes from the field value in the record.
// Upload: POST /:entity/:id/files multipart — REQUIRED form param: `field` = the field name,
//   `file` = the file blob. Returns {data:{id, name, size, url}}.
// Download: GET /files/:id — returns file content with auth header (Bearer).
// Delete: DELETE /files/:id → 204.
export interface ApiFile {
  id: string;
  name: string;        // backend returns "name", not "filename"
  size?: number;
  url?: string;        // relative path like /api/v1/files/:id
}
// POST /:entity/:id/files (multipart/form-data) — upload a file to a specific field on a record.
// The form must include: `field` (field name, required by backend) and `file` (the File blob).
export async function uploadFile(
  entity: string,
  recordId: string,
  file: File,
  fieldName: string,   // required — backend returns FILE_FIELD_REQUIRED if omitted
): Promise<ApiFile> {
  const t = getToken();
  const fd = new FormData();
  fd.append("file", file);
  fd.append("field", fieldName);   // backend param name is "field", not "fieldName"
  const res = await fetch(`${BASE}/${entity}/${recordId}/files`, {
    method: "POST",
    headers: { ...(t ? { Authorization: `Bearer ${t}` } : {}), ...previewHeaders("POST") },
    body: fd,
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, json.code ?? "UNKNOWN", json.error ?? res.statusText, json.details ?? {});
  return json.data as ApiFile;
}
// GET /files/:id — download a file (returns raw content, auth via Bearer header).
// Use fetch with auth for download, or build a blob URL from this response.
export function fileDownloadUrl(fileId: string): string {
  // Backend does NOT accept ?token= — use fetch+blob for programmatic download.
  return `${BASE}/files/${fileId}`;
}
// Download a file as a blob and trigger browser save.
export async function downloadFile(fileId: string, filename: string): Promise<void> {
  const t = getToken();
  const res = await fetch(`${BASE}/files/${fileId}`, {
    headers: { ...(t ? { Authorization: `Bearer ${t}` } : {}), ...previewHeaders("GET") },
  });
  if (!res.ok) throw new ApiError(res.status, "DOWNLOAD_FAILED", `Cannot download file ${fileId}`);
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}
// DELETE /files/:id → 204.
export async function deleteFile(fileId: string): Promise<void> {
  await req("DELETE", `/files/${fileId}`, undefined);
}

// ── Relationships (N-N) — EPIC 3 ──
// GET /:entity/:id/relationships/:relName — list N-N related records.
// With ?expand=true → returns full {id, tenantId, data:{...}} records, not just ids.
// Returns RELATIONSHIP_NOT_FOUND (404) if the relName is not registered as N-N in sys_relationships.
export async function listRelationships(
  entity: string,
  recordId: string,
  relName: string,
  expand = true,
): Promise<ApiRecord[]> {
  const qs = expand ? "?expand=true" : "";
  const r = await req<{ data: ApiRecord[] }>("GET", `/${entity}/${recordId}/relationships/${relName}${qs}`);
  return r.data ?? [];
}
// POST /:entity/:id/relationships/:relName — link a related record in N-N.
// Body: {targetId: "<uuid>"} (NOT {id}).
export async function addRelationship(
  entity: string,
  recordId: string,
  relName: string,
  relatedId: string,
): Promise<void> {
  await req("POST", `/${entity}/${recordId}/relationships/${relName}`, { targetId: relatedId });
}
// DELETE /:entity/:id/relationships/:relName/:relatedId — unlink a N-N record.
export async function removeRelationship(
  entity: string,
  recordId: string,
  relName: string,
  relatedId: string,
): Promise<void> {
  await req("DELETE", `/${entity}/${recordId}/relationships/${relName}/${relatedId}`, undefined);
}

// ── Approvals (EPIC 4) ──
// Per-record: GET /:entity/:id/approvals → {data:{instances:[]}}.
// ⚠️ W6 correction (verified live 2026-07-03): this endpoint serializes bpm.Instance
// PascalCase (like /approvals/pending), NOT camelCase as previously documented here —
// the old typed parse silently produced undefined fields ("Bước NaN" bug). Normalize
// through fromAnyInstance so both dialects land in ApprovalDetailData.
// Empty array when no approval process is configured for this record.
export async function listRecordApprovals(entity: string, recordId: string): Promise<ApprovalDetailData[]> {
  const r = await req<{ data: { instances: Array<Record<string, unknown>> } }>(
    "GET", `/${entity}/${recordId}/approvals`,
  );
  return (r.data?.instances ?? []).map(fromAnyInstance);
}
/** listRecordSLA — GET /:entity/:id/approvals?lane=independent: the tracking-lane clocks for the SLA card (specs/043 T282). */
export async function listRecordSLA(entity: string, recordId: string): Promise<RecordSlaItem[]> {
  const r = await req<{ data: { instances: Array<Record<string, unknown>> } }>(
    "GET", `/${entity}/${recordId}/approvals?lane=independent`,
  );
  return (r.data?.instances ?? []).map(fromSlaItem);
}
// Approve / reject an approval instance by its id. `expectedStep` is the step (currentStep) the
// screen RENDERED: a click that arrives after another one moved the instance on is refused with
// 409 APPROVAL_CONFLICT instead of deciding a step the user never saw (specs/043 S3b). Omitted =
// the server decides on whatever step it reads.
export async function approveRecord(approvalId: string, note?: string, expectedStep?: number): Promise<void> {
  await req("POST", `/approvals/${approvalId}/approve`, { note: note ?? "", expectedStep });
}
export async function rejectRecord(approvalId: string, note: string, expectedStep?: number): Promise<void> {
  await req("POST", `/approvals/${approvalId}/reject`, { note, expectedStep });
}
/**
 * overrideApproval — POST /admin/approval-instances/:id/override (specs/043 S4, FR-023).
 * An ADMIN who holds no seat on a step with several approvers decides it in their place; the note
 * is the reason and is mandatory both ways (422 APPROVAL_OVERRIDE_NOTE_REQUIRED). Admin-gated.
 */
export async function overrideApproval(approvalId: string, decision: "approve" | "reject", note: string, expectedStep?: number): Promise<void> {
  await req("POST", `/admin/approval-instances/${approvalId}/override`, { decision, note, expectedStep });
}

/**
 * submitForApproval — POST /:entity/:id/approvals/submit ("Gửi duyệt").
 *
 * The verb that did not exist: approvals could only fire on save, so the system decided for you
 * and the sender had no way to ask — and therefore no way to take it back. Throws ApiError with
 * code APPROVAL_NO_PROCESS when nothing is configured to receive this record, which the UI must
 * show as a real answer rather than swallowing into a generic failure.
 */
export async function submitForApproval(entity: string, recordId: string, processId?: string): Promise<void> {
  await req("POST", `/${entity}/${recordId}/approvals/submit`, processId ? { processId } : {});
}

/** One manual process this record may be sent through (GET /:entity/:id/approvals/startable, specs/043 S9b). */
export interface StartableProcess { processId: string; name: string }

/** listStartableProcesses — the choices for the "Gửi duyệt" picker; empty = nothing manual is armed for this record. */
export async function listStartableProcesses(entity: string, recordId: string): Promise<StartableProcess[]> {
  const r = await req<{ data: { processes: StartableProcess[] | null } }>("GET", `/${entity}/${recordId}/approvals/startable`);
  return r.data?.processes ?? [];
}

/** recallApproval — POST /approvals/:id/recall: the SENDER withdraws a still-pending request. */
export async function recallApproval(approvalId: string, note?: string): Promise<void> {
  await req("POST", `/approvals/${approvalId}/recall`, { note: note ?? "" });
}

/**
 * delegateApproval — POST /approvals/:id/delegate: the current approver hands THIS step to a
 * named person. The step does not advance and the deadline does not restart — the same decision
 * is still owed, by someone else.
 */
export async function delegateApproval(approvalId: string, toUserId: string, note?: string): Promise<void> {
  await req("POST", `/approvals/${approvalId}/delegate`, { toUserId, note: note ?? "" });
}
// ── Record notes — tab "Trao đổi" (RECORD-NOTES, handlers/note_handler.go) ──
// Free-text notes any user can attach to ANY record of ANY entity (engine capability, not a
// CRM-only feature). Every operation rides the SAME parent-record gate as GET /:entity/:id —
// a caller who cannot view the record gets the exact same 404, never a note (no existence leak).
// Wire shape (noteDTO, camelCase): createdByName is OMITTED (not "") when the author-label batch
// fetch is unwired or fails — fall back to the raw id, never invent a name.
export interface RecordNote {
  id: string;
  body: string;
  createdBy: string;
  createdByName?: string;
  createdAt: string;
  updatedAt: string;
}
export interface RecordNotesPage { notes: RecordNote[]; pagination: ListPagination }
// GET /:entity/:id/notes — newest first, simple offset pagination (?page=&limit=).
export async function listRecordNotes(
  entity: string,
  recordId: string,
  page = 1,
  limit = 20,
): Promise<RecordNotesPage> {
  const r = await req<{ data: RecordNote[]; pagination?: Partial<ListPagination> }>(
    "GET", `/${entity}/${recordId}/notes?page=${page}&limit=${limit}`,
  );
  const notes = r.data ?? [];
  const p = r.pagination ?? {};
  return {
    notes,
    pagination: {
      page: p.page ?? page,
      limit: p.limit ?? limit,
      total: p.total ?? notes.length,
      totalPages: p.totalPages ?? 1,
    },
  };
}
// POST /:entity/:id/notes {body} → 201 {data: note}. Errors: 422 RECORD_NOTE_EMPTY (blank after
// trim) · 422 RECORD_NOTE_TOO_LONG (>5000 rune) · 404 when the caller cannot view the record.
export async function createRecordNote(entity: string, recordId: string, body: string): Promise<RecordNote> {
  const r = await req<{ data: RecordNote }>("POST", `/${entity}/${recordId}/notes`, { body });
  return r.data;
}
// DELETE /:entity/:id/notes/:noteId → 204. Delete rule is server-side (note author OR entity
// delete-grant): 403 FORBIDDEN otherwise · 404 RECORD_NOTE_NOT_FOUND. Never pre-hide the button
// on a guess — call and surface the real error.
export async function deleteRecordNote(entity: string, recordId: string, noteId: string): Promise<void> {
  await req("DELETE", `/${entity}/${recordId}/notes/${noteId}`, undefined);
}

// ── Approvals inbox (EPIC 4 — global pending queue) ──
// GET /approvals/pending — all pending approvals across entities for the current user.
// Note: this endpoint returns PascalCase JSON (verified deviation from camelCase convention).
export interface ApprovalItem {
  ID: string;
  TenantID: string;
  EntityName: string;
  RecordID: string;
  CurrentStep: number;
  Status: string;
  // Approver for current step — PascalCase per API deviation (verified live returns PascalCase)
  CurrentApproverType?: string;
  CurrentApproverID?: string;
  SLADueAt?: string;
  Escalated?: boolean;
  // History uses lowercase keys (verified live: { step, action, actor, at })
  History?: Array<{ step: number; actor: string; action: string; at: string; note?: string }>;
}
export async function listPendingApprovals(): Promise<ApprovalItem[]> {
  // W0-QW3: live envelope is { data: { approvals: [] } } (verified 2026-06-30, see
  // ApprovalsInbox/lib/admin.ts) — the old `{ data: [] }` parse here silently returned []
  // on the real shape. Handle both, mirroring lib/admin.ts.
  const r = await req<{ data: { approvals: ApprovalItem[] } | ApprovalItem[] }>("GET", "/approvals/pending");
  const d = r.data as { approvals?: ApprovalItem[] } | ApprovalItem[];
  if (Array.isArray(d)) return d;
  return d?.approvals ?? [];
}

// ── specs/043 S5: "Việc của tôi" — work-item inbox (handlers/work_item_handler.go) ──
// Protected (not admin-gated), camelCase. Names not codes: the server resolves process/step/record/assignee labels.
export type WorkItemView = "mine" | "pool" | "done";
export interface WorkItem {
  id: string;
  instanceId: string;
  processName: string;
  nodeName: string;
  kind: "approval" | "task" | "rework" | string;
  status: string;
  entityName: string;
  entityLabel: string;
  recordId: string;
  recordLabel: string;
  assigneeType: string;
  assigneeName: string;
  claimedBy?: string;
  claimedByMe: boolean;
  dueAt: string | null;
  dueBasis?: string;
  overdue: boolean;
  paused: boolean;
  createdAt: string;
  completedAt?: string;
  requiredFields?: string[];
  returnReason?: string;
}
export interface WorkItemCounts { mine: number; pool: number; overdue: number }

export async function listWorkItems(view: WorkItemView, cursor?: string): Promise<{ items: WorkItem[]; nextCursor: string | null }> {
  const q = new URLSearchParams({ view });
  if (cursor) q.set("cursor", cursor);
  const r = await req<{ data: { items: WorkItem[]; nextCursor: string | null } }>("GET", `/work-items?${q}`);
  return { items: r.data?.items ?? [], nextCursor: r.data?.nextCursor ?? null };
}
export async function workItemCounts(): Promise<WorkItemCounts> {
  const r = await req<{ data: WorkItemCounts }>("GET", "/work-items/counts");
  return { mine: r.data?.mine ?? 0, pool: r.data?.pool ?? 0, overdue: r.data?.overdue ?? 0 };
}
export async function claimWorkItem(id: string): Promise<void> {
  await req("POST", `/work-items/${id}/claim`);
}
export async function releaseWorkItem(id: string): Promise<void> {
  await req("POST", `/work-items/${id}/release`);
}
export async function completeWorkItem(id: string, note?: string): Promise<void> {
  await req("POST", `/work-items/${id}/complete`, { note: note ?? "" });
}

// ── W6: Approval detail — GET /approvals/:id (api-conventions §9.5) ──
// Protected (NOT admin-gated). camelCase per convention (unlike /approvals/pending — PascalCase).
// ACL mirrors pending: admin sees any instance in tenant; non-admin only when current approver
// of a pending instance. Everything else → 404 APPROVAL_INSTANCE_NOT_FOUND (no-leak).
export async function getApprovalDetail(id: string): Promise<ApprovalDetailData> {
  const r = await req<{ data: { approval: Record<string, unknown> } }>("GET", `/approvals/${id}`);
  return fromDetailPayload(r.data?.approval ?? {});
}

// ── W6: Workflow run history + dry-run (api-conventions §9.3 / §9.4) ──
// Types live in lib/workflowRuns.ts (pure shaping module — same pattern as runFilters).
export interface WorkflowRunsPage {
  runs: WorkflowRun[];
  limit: number;
  offset: number;
  total: number;
}
// GET /admin/workflows/:id/runs — per-rule, newest-first. limit default 20, hard cap 100.
export async function listWorkflowRuns(
  workflowId: string,
  limit = 20,
  offset = 0,
): Promise<WorkflowRunsPage> {
  const r = await req<{
    data: { runs: WorkflowRun[]; pagination?: { limit?: number; offset?: number; total?: number } };
  }>("GET", `/admin/workflows/${workflowId}/runs?limit=${limit}&offset=${offset}`);
  const runs = r.data?.runs ?? [];
  const p = r.data?.pagination ?? {};
  return {
    runs,
    limit: p.limit ?? limit,
    offset: p.offset ?? offset,
    total: p.total ?? runs.length,
  };
}
// GET /admin/workflows/runs — recent runs across ALL rules of the tenant (no total).
// workflowIds narrows the read to those rules (the automation panel of ONE app, queue #297);
// undefined = every rule of the tenant. Server caps the list at 100 (422 WORKFLOW_RUNS_FILTER_INVALID).
export async function listRecentWorkflowRuns(limit = 20, workflowIds?: string[]): Promise<WorkflowRun[]> {
  const only = workflowIds ? `&workflowIds=${encodeURIComponent(workflowIds.join(","))}` : "";
  const r = await req<{ data: { runs: WorkflowRun[] } }>(
    "GET", `/admin/workflows/runs?limit=${limit}${only}`,
  );
  return r.data?.runs ?? [];
}
// POST /admin/workflows/:id/test — dry-run trace, ZERO side effect (§9.4). Evaluates the SAVED
// rule against a real record: no mutation, no email/webhook/notification, no run row written.
// Errors: 404 WORKFLOW_NOT_FOUND · 422 WORKFLOW_TEST_RECORD_REQUIRED · 404 RECORD_NOT_FOUND.
// specs/048 S6: `sim` simulates the write — op create|update (default: the rule's own) and the
// fields it changes; recordId may be empty on op "create". 422 WORKFLOW_TEST_OP_INVALID ·
// WORKFLOW_TEST_CHANGES_INVALID (details.field = the change a save would refuse).
export async function testWorkflow(
  workflowId: string,
  recordId?: string,
  // withEntityRules (#442): judge a before_save rule inside the table's whole rule chain.
  sim?: { op?: "create" | "update"; changes?: Record<string, unknown>; withEntityRules?: boolean },
): Promise<WorkflowTestResult> {
  const r = await req<{ data: { test: WorkflowTestResult } }>(
    "POST", `/admin/workflows/${workflowId}/test`, { recordId, ...sim },
  );
  return r.data.test;
}

// ── Import (EPIC 3) ──
// BE contract (handlers/import_handler.go + services/record_import.go):
//   response = { data: { total, inserted, failed, valid, dryRun, errors: [{row, field, code, message}] } }
//   (`inserted`, NOT `created` — previous FE type mismatched and rendered undefined).
// Rows skipped by dedup carry code "RECORD_DUPLICATE" inside errors[] and count into `failed`.
// specs/061: `valid` = rows that pass every check (what a real run writes); `dryRun` echoes the
// preview flag. errors[].field names the field of a cell error.
export interface ImportResult {
  total: number;
  inserted: number;
  failed: number;
  valid?: number;
  dryRun?: boolean;
  // specs/048 S7a: history jobs the server could not queue after the rows were stored (0/absent = ok).
  auditJobsFailed?: number;
  errors?: Array<{ row: number; field?: string; code?: string; message: string }>;
}
export interface ImportOptions {
  // dedupField: name of a declared field — rows whose value for it already exists in the DB, or
  // duplicates an earlier CSV row, are SKIPPED (reported as RECORD_DUPLICATE errors) instead of
  // aborting the whole batch with 409 IMPORT_BATCH_DUPLICATE on a unique collision.
  dedupField?: string;
  // dryRun (specs/061 FR-010): the "Xem trước" step — every check runs, nothing is written.
  dryRun?: boolean;
  // matchBy (specs/061 FR-012): relation field → field of its target entity the column's cells
  // name ("Khớp theo"); absent = the relation's name field. A cell holding the id always matches.
  matchBy?: Record<string, string>;
  // runAutomations (specs/048 S7b): fire the after-create rules and approvals of every imported row, as a
  // hand-made create does. Off by default — a big file must not mail/webhook by surprise.
  runAutomations?: boolean;
}
// POST /:entity/import — import CSV records (multipart/form-data).
export async function importRecords(
  entity: string,
  file: File,
  mapping: Record<string, string>, // csvColumn → fieldName
  opts: ImportOptions = {},
): Promise<ImportResult> {
  const t = getToken();
  const fd = new FormData();
  fd.append("file", file);
  // A skipped column ("— Bỏ qua —") is mapped to "" on screen: it is not a field, so it stays off the
  // wire — the server refuses a mapping to a field named "" (RECORD_UNKNOWN_FIELD).
  fd.append("mapping", JSON.stringify(Object.fromEntries(Object.entries(mapping).filter(([, field]) => field))));
  if (opts.dedupField) fd.append("dedupField", opts.dedupField);
  if (opts.dryRun) fd.append("dryRun", "true");
  if (opts.runAutomations) fd.append("runAutomations", "true");
  if (opts.matchBy && Object.keys(opts.matchBy).length > 0) fd.append("matchBy", JSON.stringify(opts.matchBy));
  const res = await fetch(`${BASE}/${entity}/import`, {
    method: "POST",
    headers: { ...(t ? { Authorization: `Bearer ${t}` } : {}), ...previewHeaders("POST") },
    body: fd,
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new ApiError(res.status, json.code ?? "UNKNOWN", json.error ?? res.statusText, json.details ?? {});
  return (json.data ?? json) as ImportResult;
}

// ── Export ──
// GET /:entity/export.csv requires Authorization: Bearer header.
// ?token= is NOT supported (returns 401). Must use fetch-with-auth → blob → save.
export async function exportCsv(entity: string, query = ""): Promise<void> {
  const t = getToken();
  const qs = query ? `?${query}` : "";
  const res = await fetch(`${BASE}/${entity}/export.csv${qs}`, {
    headers: { ...(t ? { Authorization: `Bearer ${t}` } : {}), ...previewHeaders("GET") },
  });
  if (!res.ok) {
    const json = await res.json().catch(() => ({}));
    throw new ApiError(res.status, json.code ?? "EXPORT_FAILED", json.error ?? res.statusText);
  }
  const blob = await res.blob();
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = `${entity}-export.csv`;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
}

// ── Aggregation (EPIC 6) ──
export interface AggGroup { key: string; value: string; count: number }
export async function aggregate(
  entity: string, metric: string, field?: string, groupBy?: string,
): Promise<{ value?: string; count: number; groups?: AggGroup[] }> {
  const p = new URLSearchParams({ metric });
  if (field) p.set("field", field);
  if (groupBy) p.set("groupBy", groupBy);
  const r = await req<{ data: { value?: string; count: number; groups?: AggGroup[] } }>("GET", `/${entity}/aggregate?${p}`);
  return r.data;
}

// ── AI Builder (EPIC 9 M3) — conversational NL→metadata + server-saved sessions, admin-only ──
// POST /admin/ai/builder/chat — one agentic turn. M3 CONTRACT: the server now owns the history, so the
// request carries only a SINGLE new user message + an optional sessionId (NOT the M2 client-held
// messages[]). Omit sessionId to start a new thread — the response's `sessionId` is the freshly-minted
// id to reuse for follow-ups. A foreign/deleted sessionId → 404 SESSION_NOT_FOUND. The agent PROPOSES
// a draft; nothing is applied here (apply goes through createEntity/addField). Errors: 503 AI_DISABLED
// (feature off → FE honest-disabled), 502 AI_GATEWAY_ERROR, 504 AI_TIMEOUT — all thrown as ApiError.
// E9 M4 (spec §12): the turn can also be REFUSED with 429 + one of AI_RATE_LIMITED (Retry-After =
// seconds left in the window) · AI_CONCURRENCY_LIMIT (Retry-After: 5) · AI_BUDGET_EXCEEDED
// (Retry-After = seconds to next UTC midnight). req() forwards Retry-After as details.retryAfter
// when the browser can read it (best-effort — see the CORS note on req()). The turn is NOT
// persisted server-side on a 429, so re-sending the same message is always safe.
// A turn can take several seconds (real LLM + tool loop); the backend extends its own write deadline.
export async function chatWithAiBuilder(message: string, sessionId?: string, context?: AiTurnContextInput): Promise<AiChatResult> {
  const r = await req<{ data: AiChatResult }>("POST", "/admin/ai/builder/chat", buildAiChatBody(message, sessionId, context));
  return r.data;
}

// GET /admin/ai/builder/sessions — the caller's OWN sessions (tenant+user scoped), newest first.
// Always available even when AI_ENABLED=false (only the chat turn needs the gateway).
export async function listAiSessions(): Promise<AiSessionSummary[]> {
  const r = await req<{ data: AiSessionSummary[] }>("GET", "/admin/ai/builder/sessions");
  return r.data ?? [];
}
// GET /admin/ai/builder/sessions/:id — session header + persisted messages (each assistant message's
// draft already decoded for re-render). 404 SESSION_NOT_FOUND if not owned (no existence leak).
export async function getAiSession(id: string): Promise<AiSessionDetail> {
  const r = await req<{ data: AiSessionDetail }>("GET", `/admin/ai/builder/sessions/${id}`);
  return r.data;
}
// DELETE /admin/ai/builder/sessions/:id → 204 (soft-delete, ownership-guarded; 404 if not owned).
export async function deleteAiSession(id: string): Promise<void> {
  await req("DELETE", `/admin/ai/builder/sessions/${id}`, undefined);
}

// ── ⌘K AI Assistant (end-user ask-data) — EVERY authenticated user, NOT admin-gated ──
// POST /assistant/ask — one read-only Q&A turn about the caller's OWN visible data. The
// assistant's tools run under the caller's real RecordScope + ACL server-side, so it can never
// answer beyond what the user could browse manually. Request: a SINGLE new message + optional
// sessionId (server owns history, same shape as the AI Builder chat); omit sessionId to start a
// thread — reuse the returned one for follow-ups. reply is markdown (render via MarkdownLite,
// never innerHTML). Errors (ApiError): 503 AI_DISABLED (feature off → honest-disabled),
// 429 AI_RATE_LIMITED / AI_CONCURRENCY_LIMIT / AI_BUDGET_EXCEEDED (per-TENANT budget — details
// .retryAfter carries Retry-After when the browser can read it, see req()), 502/504 gateway.
export interface AssistantAskResult {
  sessionId: string;
  reply: string;
  usage: AiUsage;
  // "ok" = a final answer; "max_steps" = the tool loop hit its bound with an empty/partial reply
  // (the FE must tell the user to narrow the question rather than show a blank bubble); "refused" = the
  // model declined, reply empty (the FE says so).
  outcome?: string;
  // ⌘K-can-act HITL record proposals (kinds record_create/record_update ONLY — never metadata).
  // NOTHING was written server-side: the palette renders confirm cards and applies via the NORMAL
  // createRecord/updateRecord under the user's own token. null/absent = none this turn.
  drafts?: AssistantDraft[] | null;
}

// SEARCH-UNIFIED (Wave 3.5): server-side cross-entity search. One request fans out over EVERY
// entity the caller may view (ACL + row scope enforced server-side) — replaces the ⌘K palette's
// old client-side top-5 fan-out that could never reach the 6th+ entity.
export interface SearchHitGroup {
  entity: string;
  label: string;
  hits: { id: string; data: Record<string, unknown> }[];
}

export async function searchAll(term: string): Promise<SearchHitGroup[]> {
  const r = await req<{ data: { groups: SearchHitGroup[] } }>(
    "GET", `/search?q=${encodeURIComponent(term)}`,
  );
  return r.data.groups ?? [];
}

// GET /ai/health — honest on/off probe (MỘT XƯỞNG RỔ-4 P2). The endpoint has existed since the
// EASE campaign and had ZERO callers, which is exactly the failure it was built to prevent: ⌘K
// invited the user to type, they composed a whole question, and only THEN ate a 503 AI_DISABLED.
// Any authenticated user may call it; it leaks nothing beyond the boolean.
// Never throws: a probe failure must not break the palette — unknown state renders as "on"
// (the ask turn still surfaces an honest 503 notice) so a network blip cannot hide a working AI.
export async function aiHealth(): Promise<boolean> {
  try {
    const r = await req<{ data: { enabled: boolean } }>("GET", "/ai/health");
    return r.data?.enabled !== false;
  } catch {
    return true;
  }
}

// GET /users/directory — danh bạ người dùng cho MỌI user đã đăng nhập (MỘT XƯỞNG, wave 4).
// Vì sao cần: nút "Đổi chủ" cho phép ai có `edit@all` (quản lý) đổi chủ bản ghi, nhưng dialog chọn
// người trước đây gọi `listAdminUsers` — endpoint ADMIN-ONLY. Nút có, quyền có, thao tác KHÔNG hoàn
// thành được: đúng bug-class §0b ở một hình dạng khác (hai đầu đều đúng, chỗ gặp nhau thì hỏng).
// Backend cố ý chỉ trả id/fullName/email — không status, không isAdmin, không roleId, không MFA.
export interface DirectoryUser { id: string; fullName: string; email: string }

export async function listUserDirectory(q = "", limit = 50): Promise<DirectoryUser[]> {
  const qs = new URLSearchParams({ limit: String(limit) });
  if (q.trim()) qs.set("q", q.trim());
  const r = await req<{ data: DirectoryUser[] }>("GET", `/users/directory?${qs.toString()}`);
  return r.data ?? [];
}

export async function assistantAsk(message: string, sessionId?: string): Promise<AssistantAskResult> {
  const body: { message: string; sessionId?: string } = { message };
  if (sessionId) body.sessionId = sessionId;
  const r = await req<{ data: AssistantAskResult }>("POST", "/assistant/ask", body);
  return r.data;
}

// GET /assistant/sessions — the caller's OWN ⌘K threads (tenant+user scoped, rail
// feature='assistant' — never the Builder's), newest first, server-capped (≈20). Available even
// when AI_ENABLED=false (only the ask turn needs the gateway).
export async function listAssistantSessions(): Promise<AssistantSessionSummary[]> {
  const r = await req<{ data: AssistantSessionSummary[] }>("GET", "/assistant/sessions");
  return r.data ?? [];
}
// GET /assistant/sessions/:id — thread header + messages, each assistant row's drafts already
// decoded for confirm-card re-render on resume. 404 SESSION_NOT_FOUND if not owned (no leak).
export async function getAssistantSession(id: string): Promise<AssistantSessionDetail> {
  const r = await req<{ data: AssistantSessionDetail }>("GET", `/assistant/sessions/${id}`);
  return r.data;
}
// DELETE /assistant/sessions/:id → 204 (soft-delete, ownership-guarded; 404 if not owned).
export async function deleteAssistantSession(id: string): Promise<void> {
  await req("DELETE", `/assistant/sessions/${id}`, undefined);
}

// ── Studio MUTATIONS (admin) — Studio is Nexus's internal builder, so it writes real metadata ──
export interface NewFieldInput {
  name: string; label: string; fieldType: string; isRequired?: boolean; isUnique?: boolean;
  options?: Record<string, unknown>;
  // W5: omit fieldOrder → backend appends (max active order + 10); description ≤500 runes.
  fieldOrder?: number; description?: string;
}
// POST /admin/entities — registers the entity + generates obj_<name> and (optionally) its fields.
// aclSeed (MỘT XƯỞNG TRỤ 2 mục 4): khi tạo bảng DƯỚI một app, backend tự đảm bảo app có 2 vai mặc
// định và cấp quyền bảng mới vào đúng 2 vai đó. Không có nó, bảng dựng tay là VÔ HÌNH với mọi
// người không phải admin (ACL default-deny) — mà người dựng không bao giờ phát hiện, vì `is_admin`
// bỏ qua toàn bộ ACL. Seed là FAIL-OPEN: bảng vẫn được tạo, nhưng `warning` PHẢI hiện ra —
// nuốt im lặng ở đây là tái tạo đúng bug-class chiến dịch đang diệt.
export interface AclSeedSignal {
  appScoped: boolean;
  seeded: boolean;
  warning?: string;
}

export interface CreateEntityResult {
  entity: EntityMeta;
  aclSeed: AclSeedSignal;
}

export async function createEntity(
  name: string, label: string, description = "", fields: NewFieldInput[] = [], appId?: string,
): Promise<EntityMeta> {
  return (await createEntityWithAcl(name, label, description, fields, appId)).entity;
}

// Bản đầy đủ — dùng khi caller cần biết bảng mới có ai ngoài admin thấy được không.
export async function createEntityWithAcl(
  name: string, label: string, description = "", fields: NewFieldInput[] = [], appId?: string,
): Promise<CreateEntityResult> {
  const body: Record<string, unknown> = { name, label, description, fields };
  if (appId) body.appId = appId;
  const r = await req<{ data: { entity: EntityMeta; aclSeed?: AclSeedSignal } }>(
    "POST", "/admin/entities", body,
  );
  return {
    entity: r.data.entity,
    aclSeed: r.data.aclSeed ?? { appScoped: false, seeded: false },
  };
}
// PUT /admin/entities/:name — update entity label, description, or options.
export async function updateEntity(name: string, payload: { label?: string; description?: string; options?: Record<string, unknown> }): Promise<EntityMeta> {
  const r = await req<{ data: { entity: EntityMeta } }>("PUT", `/admin/entities/${name}`, payload);
  return r.data.entity;
}
// DELETE /admin/entities/:name → 204.
export async function deleteEntity(name: string): Promise<void> {
  await req("DELETE", `/admin/entities/${name}`, undefined);
}

// POST /admin/fields — adds one field to an existing entity (ALTERs obj_* or stores in JSON).
export async function addField(entityName: string, field: NewFieldInput): Promise<FieldMeta> {
  const r = await req<{ data: { field: FieldMeta } }>("POST", "/admin/fields", { entityName, ...field });
  return r.data.field;
}
// PUT /admin/fields/:id — update label, isRequired, defaultValue, options, description, fieldOrder.
// Apply-when-non-nil semantics: omitted keys keep their stored value (api-conventions §10.1).
export async function updateField(id: string, payload: { label?: string; isRequired?: boolean; defaultValue?: unknown; options?: Record<string, unknown>; description?: string; fieldOrder?: number }): Promise<FieldMeta> {
  const r = await req<{ data: { field: FieldMeta } }>("PUT", `/admin/fields/${id}`, payload);
  return r.data.field;
}
// POST /admin/fields/:id/recompute — specs/083 "Tính lại": recompute a stored roll-up on every row of
// its table; answers how many rows changed. 422 FIELD_ROLLUP_NOT_STORED when the field keeps no total.
export async function recomputeStoredRollup(id: string): Promise<{ updated: number }> {
  const r = await req<{ data: { updated: number } }>("POST", `/admin/fields/${id}/recompute`, {});
  return r.data;
}
export async function deleteField(id: string): Promise<void> {
  await req("DELETE", `/admin/fields/${id}`, undefined);
}

// PUT /admin/fields/reorder — W5 (api-conventions §10.2). Renumbers ONE entity's display order in
// ONE transaction, all-or-nothing: id #i gets field_order (i+1)*10. Send the FULL active field-id
// list in display order. 422 FIELD_REORDER_INVALID (empty/dupes/>200) or
// FIELD_REORDER_UNKNOWN_FIELD (details.field = offending id) → the caller must roll back its
// optimistic local order. Re-sending an identical order is a no-op 200.
export async function reorderFields(entityName: string, order: string[]): Promise<{ entityName: string; updated: number }> {
  const r = await req<{ data: { entityName: string; updated: number } }>("PUT", "/admin/fields/reorder", { entityName, order });
  return r.data;
}

// POST /admin/formula/validate — W5 (api-conventions §10.3). Zero-write dry-run through the SAME
// validator the field-create path uses, so the live editor sees exactly what save would return:
// 200 { valid, fields (referenced, deduped), resultType } or a thrown ApiError with one of the
// FIELD_FORMULA_* codes (INVALID / UNKNOWN_REF / NON_NUMERIC_REF / NESTED).
export async function validateFormula(entityName: string, formula: string, resultType?: string): Promise<{ valid: boolean; fields: string[]; resultType: string }> {
  const body: Record<string, unknown> = { entityName, formula };
  if (resultType) body.resultType = resultType;
  const r = await req<{ data: { valid: boolean; fields: string[]; resultType: string } }>("POST", "/admin/formula/validate", body);
  return r.data;
}

// ── Admin Layouts ──
// POST /admin/layouts — create a layout for an entity.
// viewType: "edit" | "detail" | "list"
// layoutJson for edit/detail: { version:1, sections:[{title, columns:1|2|3, fields:[...]}] }
// layoutJson for list: { version:1, columns:[...], defaultSort:{field:"created_at"|"updated_at"|"id", order} }
// IMPORTANT: version must be 1; layout CREATE returns {data:{id,entityName,tenantId,viewType}} (no layoutJson echo).
export interface AdminLayoutInput {
  entityName: string;
  viewType: "edit" | "detail" | "list";
  layoutJson: Record<string, unknown>;
  isDefault?: boolean;
}
export interface AdminLayoutRef { id: string; entityName: string; tenantId: string; viewType: string }
export async function createLayout(payload: AdminLayoutInput): Promise<AdminLayoutRef> {
  const r = await req<{ data: AdminLayoutRef }>("POST", "/admin/layouts", payload);
  return r.data;
}
// PUT /admin/layouts/:id — update a layout's layoutJson. Only layoutJson accepted in body.
export async function updateLayout(id: string, layoutJson: Record<string, unknown>): Promise<AdminLayoutRef> {
  const r = await req<{ data: AdminLayoutRef }>("PUT", `/admin/layouts/${id}`, { layoutJson });
  return r.data;
}
// DELETE /admin/layouts/:id → 204.
export async function deleteLayout(id: string): Promise<void> {
  await req("DELETE", `/admin/layouts/${id}`, undefined);
}

// ── Admin Relationships ──
// POST /admin/relationships — create a N-N relationship between two entities.
// GET /metadata/relationships[?entity=] — list registered relationships. Since ENG-REL2 #4
// (migration 000028) this returns EVERY type ("1-1" | "1-N" | "N-N"), not just N-N: each 1-N
// relation field now auto-registers a registry row carrying inverseField/childFkColumn.
// Consumers that implement N-N semantics (Related tab link/unlink, ERD N:N edges) MUST filter
// relationshipType === "N-N" — the backend treats a 1-N name as not-found on the N-N routes.
export interface AdminRelationshipInput { name: string; entityA: string; entityB: string }
// ENG-RELVIEW: per-relationship related-list ("subpanel") view config, stored on
// sys_relationships.view_config (migration 000044). Absent = the RelatedTab keeps its default
// auto-derived rendering. columns/sort.field are validated server-side against the CHILD
// entity's fields (sort additionally against the RM-09 sort allowlist — unique/relation/
// indexed fields + created_at/updated_at/id).
export interface RelViewConfig {
  label?: string;                                 // panel title override (≤100 chars)
  order?: number;                                 // panel position among related lists (asc)
  columns?: string[];                             // child-entity field names to render as a table
  sort?: { field: string; dir?: "asc" | "desc" }; // server-side sort of the child list
  allowCreate?: boolean;                          // show "+ Tạo" (child form prefilled with parent FK)
  allowLink?: boolean;                            // N-N: show/hide the "Liên kết" picker (default true)
}
export interface AdminRelationship {
  id: string;
  name: string;
  parentEntityName: string;
  childEntityName: string;
  relationshipType: string; // "1-1" | "1-N" | "N-N"
  joinTableName?: string;   // N-N only (SQL NULL → omitted for 1-N/1-1 rows)
  inverseField?: string;    // 1-N/1-1 only — parent-side field name (ENG-REL2 #4)
  childFkColumn?: string;   // 1-N/1-1 only — physical <field>_id column on the child
  viewConfig?: RelViewConfig; // ENG-RELVIEW — parsed object (never an escaped JSON string)
}
export async function createRelationship(payload: AdminRelationshipInput): Promise<AdminRelationship> {
  const r = await req<{ data: AdminRelationship }>("POST", "/admin/relationships", payload);
  return r.data;
}
// PUT /admin/relationships/:id/view-config (ENG-RELVIEW) — the body IS the config document;
// `null` clears it (back to the default rendering). Errors: 422 RELVIEW_INVALID /
// RELVIEW_COLUMN_UNKNOWN / RELVIEW_SORT_NOT_SORTABLE, 404 RELATIONSHIP_NOT_FOUND.
export async function setRelationshipViewConfig(
  id: string,
  config: RelViewConfig | null,
): Promise<RelViewConfig | null> {
  const r = await req<{ data: { viewConfig: RelViewConfig | null } }>(
    "PUT", `/admin/relationships/${id}/view-config`, config,
  );
  return r.data?.viewConfig ?? null;
}

// ── W2 relationship lifecycle (../specs/031-relationship-lifecycle/spec.md, verified live 2026-07-02) ──
// GET /admin/relationships/:id/impact — read-only pre-flight for the guarded delete: counts
// child records still holding the FK (1-N/1-1) or join rows (N-N) + computed fields that
// reference the relationship. `deletable === false` ⇔ dependentRollups is non-empty — the
// DELETE below would 409, so the UI must BLOCK (not just warn) until the rollups are removed.
export interface RelationDependent { entityName: string; fieldName: string }
export interface RelationshipImpact {
  relationship: AdminRelationship;
  affectedRecords: number;
  dependentRollups: RelationDependent[];
  dependentLookups: RelationDependent[];
  deletable: boolean;
}
export async function getRelationshipImpact(id: string): Promise<RelationshipImpact> {
  const r = await req<{ data: RelationshipImpact }>("GET", `/admin/relationships/${id}/impact`);
  return r.data;
}
// DELETE /admin/relationships/:id → 204. Soft-delete only: 1-N/1-1 reuses the DeleteField flow
// (field + registry row soft-deleted, FK values on child records KEPT/inert), N-N tombstones
// the registry row and keeps the join table for a grace window. Errors: 409
// RELATIONSHIP_HAS_DEPENDENTS (details.dependentRollups) when a rollup still references it,
// 404 RELATIONSHIP_NOT_FOUND (already deleted / cross-tenant).
export async function deleteRelationshipById(id: string): Promise<void> {
  await req("DELETE", `/admin/relationships/${id}`, undefined);
}
// NOTE: PATCH /admin/relationships/:id exists but every structural key (name, cardinality,
// relatedEntity target, inverseField) is IMMUTABLE (422 FIELD_RELATION_OPTIONS_IMMUTABLE) —
// the relation map renders them read-only instead of exposing a fake edit form. The one editable
// thing, the link-field label (1-N/1-1), is saved from the link page's right column via
// PUT /admin/fields/:id {label} (updateField); N-N has no label yet. No function here calls PATCH.
// List registered relationships of ALL types (optionally only those involving one entity) —
// drives the App Related tab discovery + the Studio ERD edges (filter by relationshipType).
export async function listAllRelationships(entity?: string): Promise<AdminRelationship[]> {
  const qs = entity ? `?entity=${encodeURIComponent(entity)}` : "";
  const r = await req<{ data: AdminRelationship[] }>("GET", `/metadata/relationships${qs}`);
  return r.data ?? [];
}

// ── Parent-side related records (ENG-REL2 #4 inverse-field registry, FE-INVERSE-RELATED) ──
// A 1-N `relation` field lives on the CHILD entity (e.g. crm_contacts.account → crm_accounts).
// Viewing a PARENT record needs the reverse question: "which child records point at me?" — the
// list filter dialect matches against sys_fields.NAME, never the physical `<field>_id` column
// (verified live: GET /crm_contacts?account=<id> → 200; GET /crm_contacts?account_id=<id> → 422
// LIST_UNKNOWN_FILTER — docs/api-conventions.md §6.1, record_orchestration.go List()). Callers
// derive fkField from an AdminRelationship row's childFkColumn by stripping the trailing "_id"
// (childFkColumn = "<fieldName>_id" per §6.1's naming rule).
export interface ChildRecordsPage { records: ApiRecord[]; total: number }
export async function listChildRecords(
  childEntity: string,
  fkField: string,
  parentId: string,
  limit = 5,
  // ENG-RELVIEW: optional server-side sort from the relationship's viewConfig.sort. The
  // field was validated at config time against the RM-09 sort allowlist, so it never 422s
  // here. Absent → the engine default (created_at desc) — byte-identical to before.
  sort?: { field: string; dir?: "asc" | "desc" },
  // DP-2.1: relation fields to resolve to {id,label} (same ?expand= contract as getRecord) —
  // related-list cells and row labels show names, not UUIDs.
  expand?: string[],
): Promise<ChildRecordsPage> {
  let qs = `${encodeURIComponent(fkField)}=${encodeURIComponent(parentId)}&limit=${limit}`;
  // dir omitted in the stored config → asc (SQL's own ORDER BY default).
  if (sort?.field) qs += `&sort=${encodeURIComponent(sort.field)}&order=${sort.dir === "desc" ? "desc" : "asc"}`;
  if (expand && expand.length) qs += `&expand=${expand.map(encodeURIComponent).join(",")}`;
  const { records, pagination } = await listRecordsPage(childEntity, qs);
  return { records, total: pagination.total };
}

// ── Reverse polymorphic list (ACTIVITY-CORE #3a) — rows whose `polymorphic_relation` field points
// AT one record ("every activity attached to account X"). Unlike a 1-N relation, the filter is on
// the field's two PHYSICAL generated columns `<f>_type` / `<f>_id` (composite-indexed), NOT on the
// field name: the bare name holds a JSON {type,id} object with no scalar projection. Verified live
// path: record_orchestration.go List() routes a `<poly>_type` / `<poly>_id` key to its gen-column
// (services/polymorphic_relation_integration_test.go TestPolymorphic_TimelineFilter).
//
// A bare ?key=value is read as [eq] by the shared filter parser (dynamic_record_handler.go), so no
// bracket syntax is needed here. sort/order are passed EXPLICITLY: `created_at` is in the RM-09
// sort allowlist and "newest first" is the whole point of a timeline — it must not depend on
// whatever the engine's default ordering happens to be.
//
// Goes through the ORDINARY record-list path, so tenant scope, ACL, row-scope and field-ACL all
// apply by construction: a caller sees exactly the rows they would see on that entity's own list.
export async function listPolymorphicRecords(
  entity: string,
  fieldName: string,
  targetEntity: string,
  targetId: string,
  limit = 5,
): Promise<ChildRecordsPage> {
  const f = encodeURIComponent(fieldName);
  const qs = `${f}_type=${encodeURIComponent(targetEntity)}&${f}_id=${encodeURIComponent(targetId)}`
    + `&limit=${limit}&sort=created_at&order=desc`;
  const { records, pagination } = await listRecordsPage(entity, qs);
  return { records, total: pagination.total };
}

// ── Admin Workflows ──
// POST /admin/workflows | /admin/reports | /admin/dashboards — save a builder's definition.
// specs/048: before_save / before_delete run inside the save/delete and can refuse it (action `veto`);
// condition.saveOn narrows a before_save rule to creates or updates (absent = both). A before_save
// `field_update` (S4) may carry `onlyIfEmpty` (fill only when empty) and `valueSource`: absent/constant
// = `value` as is, field = `value` names the field to copy, template = `value` is "{{field}}" text.
// A before_save `normalize` (S5) rewrites a text/email `field` with `ops`: 1-4 distinct operations,
// applied in the order listed.
export type WorkflowTrigger =
  | "after_create" | "after_update" | "after_delete" | "scheduled" | "date_offset" | "before_save" | "before_delete";
export type WorkflowValueSource = "constant" | "field" | "template";
export type WorkflowNormalizeOp = "trim" | "lower" | "upper" | "collapse_spaces";
export interface WorkflowDetail extends WorkflowSummary {
  trigger?: WorkflowTrigger;
  condition?: Record<string, unknown> & { saveOn?: "create" | "update" };
  actions?: (Record<string, unknown> & { onlyIfEmpty?: boolean; valueSource?: WorkflowValueSource; ops?: WorkflowNormalizeOp[] })[];
}
export async function createWorkflow(payload: Record<string, unknown>): Promise<WorkflowDetail> {
  const r = await req<{ data: { workflow: WorkflowDetail } }>("POST", "/admin/workflows", payload);
  return r.data.workflow;
}
export async function getWorkflow(id: string): Promise<WorkflowDetail> {
  const r = await req<{ data: { workflow: WorkflowDetail } }>("GET", `/admin/workflows/${id}`);
  return r.data.workflow;
}
export async function updateWorkflow(id: string, payload: Record<string, unknown>): Promise<WorkflowDetail> {
  const r = await req<{ data: { workflow: WorkflowDetail } }>("PUT", `/admin/workflows/${id}`, payload);
  return r.data.workflow;
}
/** Pause / resume one rule without resending its actions (a PUT of a rule that still holds a
 * pre-vault key is a 409 — this is the way out Doctor C10 names). */
export async function setWorkflowActive(id: string, isActive: boolean): Promise<WorkflowDetail> {
  const r = await req<{ data: { workflow: WorkflowDetail } }>("PATCH", `/admin/workflows/${id}/active`, { isActive });
  return r.data.workflow;
}
// GET /admin/workflows/scheduler — is the background timer on? (#291) A "scheduled" rule never
// fires while it is off, so the builder warns before saving one.
export async function getWorkflowSchedulerStatus(): Promise<{ enabled: boolean; intervalSeconds: number }> {
  const r = await req<{ data: { enabled: boolean; intervalSeconds: number } }>("GET", "/admin/workflows/scheduler");
  return r.data;
}
export async function deleteWorkflow(id: string): Promise<void> {
  await req("DELETE", `/admin/workflows/${id}`, undefined);
}
// POST /admin/workflows/:id/disable-now — "Tắt ngay cho khách" (specs/063 FR-017): switches the LIVE rule
// off at once, touching no draft; the one way the builder writes a delivered workspace's rule directly.
export async function disableWorkflowNow(id: string): Promise<WorkflowDetail> {
  const r = await req<{ data: { workflow: WorkflowDetail } }>("POST", `/admin/workflows/${id}/disable-now`, {});
  return r.data.workflow;
}

// ── Draft → publish (specs/063-draft-publish, api-conventions §39) ──
// Once a customer workspace is "delivered" (first publish done), the builder's edits of the types in
// `draftableTypes` are saved as DRAFTS the customer cannot see until Publish; `directTypes` still apply at
// once but leave a trace. Everything here is builder-only (403 for anyone else — the bar hides itself).
export type DraftState = "live" | "edited" | "new" | "deleted";
export interface PublishState {
  delivered: boolean;
  headVersion: { versionNo: number; kind: string; publishedAt: string; publishedBy: string } | null;
  pendingCount: number;
  /** Fingerprint of the pending set — echo it back on publish / discard-all (DRAFTS_CHANGED otherwise). */
  changesetHash: string;
  draftableTypes: string[];
  directTypes: string[];
  propagationSeconds: number;
  /** S7-9: the newest version whose field / table deletes did not finish, with what is left; null = nothing to retry. */
  schemaRetry?: { versionId: string; versionNo: number; ops: SchemaOp[] } | null;
}
/** One deferred field / table delete of a publish (specs/063 S7-9). */
export interface SchemaOp { docType: "field" | "entity"; docKey: string; label: string; applyStatus: "applied" | "pending" | "failed"; error?: string }
export interface DraftChange {
  id: string;
  docType: string;
  docKey: string;
  label: string;
  op: "create" | "update" | "delete";
  entityName: string;
  updatedAt: string;
  updatedBy: string;
  /** The live document moved since the draft was started: publish refuses unless forced. */
  conflict: boolean;
  /** Where an update differs from the live document (<= 50 dotted paths); `moreChanged` counts the rest. [] for create / delete. */
  changedPaths: string[];
  moreChanged: number;
  /** S7-9: what a pending delete leaves dangling (e.g. a report that uses the field). A warning, never a block. */
  warnings?: string[];
  /** false = a table / field born after the last publish: live but hidden from the customer, no draft to drop; Publish releases it. Absent on an older server = discardable. */
  discardable?: boolean;
}
export interface PublishVersion {
  id: string;
  seq: number;
  /** null for an "applied at once" entry. */
  versionNo: number | null;
  kind: "baseline" | "publish" | "rollback" | "direct";
  note: string;
  docCount: number;
  createdAt: string;
  createdBy: string;
  createdByName: string;
  rollbackOf: number | null;
  schemaStatus: string;
}
export interface PublishVersionDoc {
  docType: string;
  docKey: string;
  label: string;
  op: "create" | "update" | "delete" | "snapshot";
  changedPaths: string[];
  moreChanged: number;
  /** Secrets are already masked server-side ("[đã che]"). */
  before: unknown;
  after: unknown;
  applyStatus: "applied" | "pending" | "failed";
  applyError?: string;
  /** S7-7b: on a "role deleted" entry, who the undo would give the role back to (same tenant only). */
  reassign?: { userId: string; label: string; status: string; currentRoleId: string }[];
}
export interface PublishResult {
  versionNo: number;
  kind: "baseline" | "publish" | "rollback";
  docCount: number;
  publishedAt: string;
  propagationSeconds: number;
  /** S7-9: present only when the publish carried a field / table delete. "failed" ⇒ show Thử lại. */
  schemaStatus?: "done" | "failed";
  schemaOps?: SchemaOp[];
}
export async function getPublishState(): Promise<PublishState> {
  const r = await req<{ data: PublishState }>("GET", "/admin/publish/state");
  return r.data;
}
export async function listDraftChanges(): Promise<{ changes: DraftChange[]; changesetHash: string }> {
  const r = await req<{ data: DraftChange[]; meta: { changesetHash: string } }>("GET", "/admin/publish/changes");
  return { changes: r.data ?? [], changesetHash: r.meta?.changesetHash ?? "" };
}
export async function listVersions(limit = 20, offset = 0): Promise<{ versions: PublishVersion[]; total: number }> {
  const r = await req<{ data: PublishVersion[]; pagination: { total: number } }>("GET", `/admin/publish/versions?limit=${limit}&offset=${offset}`);
  return { versions: r.data ?? [], total: r.pagination?.total ?? 0 };
}
export async function getVersion(id: string): Promise<PublishVersion & { createdByLabel: string; docs: PublishVersionDoc[] }> {
  const r = await req<{ data: PublishVersion & { createdByLabel: string; docs: PublishVersionDoc[] } }>("GET", `/admin/publish/versions/${encodeURIComponent(id)}`);
  return r.data;
}
/** "Xuất bản lần đầu": snapshots the workspace as version 1 and turns draft mode ON for good. */
export async function publishBaseline(note?: string): Promise<PublishResult> {
  const r = await req<{ data: PublishResult }>("POST", "/admin/publish/baseline", note ? { note } : {});
  return r.data;
}
export async function publishDrafts(changesetHash: string, opts: { note?: string; force?: boolean } = {}): Promise<PublishResult> {
  const r = await req<{ data: PublishResult }>("POST", "/admin/publish", { changesetHash, note: opts.note, force: opts.force });
  return r.data;
}
/** S7-9: re-run the deferred field / table deletes of a version whose schema_status is "failed" (the server reuses the sweep instant it recorded). 409 SCHEMA_RETRY_NOT_NEEDED when nothing is left. */
export async function retrySchemaOps(versionId: string): Promise<{ schemaStatus: "done" | "failed"; schemaOps: SchemaOp[] }> {
  const r = await req<{ data: { schemaStatus: "done" | "failed"; schemaOps: SchemaOp[] } }>("POST", `/admin/publish/versions/${encodeURIComponent(versionId)}/retry-schema`, {});
  return r.data;
}
/** specs/063 S7-7: what "Quay lại bản này" would change. `op` is relative to the LIVE document. No document body, so no secret. */
export interface RollbackPlan {
  targetVersionNo: number;
  headVersionNo: number;
  changes: { docType: string; docKey: string; label: string; op: "create" | "update" | "delete" }[];
  /** Types the target never captured: left as they are; the builder must accept it (`acceptUntouched`). */
  untouchedTypes: string[];
  /** Rules whose webhook signing key goes back to an older value. */
  secretRestores: { docType: string; docKey: string; label: string }[];
  /** Rules that are off now and on in the target; `outbound` = they send a webhook or an email. */
  reactivates: { docKey: string; label: string; outbound: boolean }[];
  /** Documents whose latest change after the target was an applied-at-once edit by someone else. */
  overridesDirect: { docType: string; docKey: string; label: string; byLabel: string; at: string }[];
  keptAsIs: { docType: string; docKey: string; label: string }[];
  /** Fields and tables a published delete removed and this rollback brings back; a table carries how many records return with it. */
  schemaRestores: { docType: string; docKey: string; label: string; sweptCount?: number }[];
  /** Tables, fields and N-N relations a publish after the target released: the rollback takes the target's cutoff, so the customer stops seeing them (nothing is deleted). */
  rehides?: { docType: string; docKey: string; label: string }[];
  planToken: string;
}
export async function getRollbackPlan(versionId: string): Promise<RollbackPlan> {
  const r = await req<{ data: RollbackPlan }>("GET", `/admin/publish/versions/${encodeURIComponent(versionId)}/rollback-plan`);
  return r.data;
}
/** 409 DRAFTS_PENDING / HEAD_MOVED / PLAN_CHANGED / ROLLBACK_NAME_TAKEN / ROLLBACK_UNIQUE_CONFLICT · 422 ROLLBACK_NO_SNAPSHOT (details.docTypes) / ROLLBACK_USE_REVERT. */
export async function rollbackToVersion(versionId: string, body: { expectedHeadVersion: number; expectedPlanToken?: string; note?: string; acceptUntouched?: boolean }): Promise<PublishResult> {
  const r = await req<{ data: PublishResult }>("POST", `/admin/publish/versions/${encodeURIComponent(versionId)}/rollback`, body);
  return r.data;
}
/** specs/063 S7-7b: undo ONE applied-at-once entry (role, grants, field permissions, app create/delete). 409 REVERT_STALE / ROLLBACK_NAME_TAKEN · 422 REVERT_USE_ROLLBACK / REVERT_UNSUPPORTED · 404 VERSION_NOT_FOUND. */
export async function revertDirectEntry(versionId: string): Promise<{ revertedId: string; entry: PublishVersion | null; reassigned: number }> {
  const r = await req<{ data: { revertedId: string; entry: PublishVersion | null; reassigned: number } }>("POST", `/admin/publish/versions/${encodeURIComponent(versionId)}/revert`, {});
  return r.data;
}
export async function discardDraft(id: string): Promise<void> {
  await req("DELETE", `/admin/publish/changes/${id}`, undefined);
}
export async function discardAllDrafts(changesetHash: string): Promise<void> {
  await req("DELETE", `/admin/publish/changes?changesetHash=${encodeURIComponent(changesetHash)}`, undefined);
}

// ── Admin Approval Processes ──
// Trigger accepts only: "after_create" | "after_update" (verified live 2026-07-01).
/** `id` = the step's stable id (specs/043 S0): GET always returns one (legacy rows read as s1, s2…);
 * send it back unchanged on PUT so the step keeps it across reorder/delete. Omit on a new step —
 * the server assigns one. */
/** specs/043 S8: where a rejection of a step goes — "@end" (final, the default), "@submitter" (back to the submitter to fix and send again) or the id of an EARLIER step; resumeAt only with "@submitter". */
export interface ApprovalOnReject { to: string; resumeAt?: "rejector" | "start" }
/** specs/043 S6a/S6b: a step's holder named by RULE — team pool / work queue / person in a record field / the submitter. strategy applies to a team only. */
export interface ApprovalAssignment { type: "user" | "role" | "team" | "queue" | "field" | "submitter"; id?: string; field?: string; strategy?: "pool" | "each_member" | "round_robin" | "load_based" }
export interface ApprovalStep {
  id?: string; name?: string; approverType: "user" | "role"; approverId: string; slaSeconds?: number; /** specs/043 S1: absent = working hours */ slaClock?: "business" | "wall"; onReject?: ApprovalOnReject;
  /** S4/S6b: a step held by several people INSTEAD of approverType/approverId (stored as a graph). */
  approvers?: { type: "user" | "role"; id: string }[]; completion?: { mode: "all" | "any" | "quorum"; percent?: number };
  /** S6a: replaces approverType/approverId (those are "" / ignored when this is set). */
  assignment?: ApprovalAssignment;
}
/** One node of a stored graph (specs/043 S3a2) — a step plus kind/next… as the server returns it. */
export interface ApprovalGraphNode extends Partial<ApprovalStep> { kind: string; next?: string; [k: string]: unknown }
export interface ApprovalGraph { nodes: ApprovalGraphNode[]; start?: string }
/** specs/043 S6a/S13: process-level settings. Studio edits fallbackAssignee only; the rest rides back unchanged (a PUT without them would be refused or wipe them). */
export interface ApprovalOptions { fallbackAssignee?: { type: "user" | "role"; id: string }; lane?: string; calendarId?: string; pauseWhen?: unknown }
/** A step held by SEVERAL people (specs/043 S4) — sent INSTEAD of approverType/approverId. The server
 * stores a chain that has one as a graph; completion "all" = everyone must approve, "any" = the first
 * decision decides, "quorum" = percent% of the people (S6b). */
export interface ApprovalParallelStep { id?: string; name?: string; approvers: { type: "user" | "role"; id: string }[]; completion?: { mode: "all" | "any" | "quorum"; /** 1..100, with quorum only (specs/043 S6b) */ percent?: number }; slaSeconds?: number; slaClock?: "business" | "wall"; onReject?: ApprovalOnReject }
/** The terminal write-back a decision performs (WAVE 20). Absent = decision changes no field. */
export interface ApprovalOutcome { statusField: string; approvedValue?: string; rejectedValue?: string; /** specs/043 S8: written when a rejection returns the record to its submitter ("Cần bổ sung") */ returnedValue?: string }
/** The unpublished edit of a process (specs/043 S10): the same fields as a save, plus what it was written over. */
export interface ApprovalDraftDoc { name: string; entityName: string; trigger: string; steps?: ApprovalStep[]; criteria?: Record<string, unknown> | null; outcome?: ApprovalOutcome | null; graph?: ApprovalGraph | null; options?: ApprovalOptions | null; isActive?: boolean; baseVersion: number }
export interface ApprovalProcessSummary {
  id: string; name: string; entityName: string; trigger: string; isActive: boolean; steps?: ApprovalStep[]; criteria?: Record<string, unknown> | null; outcome?: ApprovalOutcome | null;
  /** graph form (steps is [] beside it) */
  graph?: ApprovalGraph | null; options?: ApprovalOptions | null;
  /** specs/043 S10: the active (published) version number, the unpublished edit when there is one, and when it was saved. The fields above are always the ACTIVE version. */
  version?: number; draft?: ApprovalDraftDoc | null; draftUpdatedAt?: string | null;
}
export async function listApprovalProcesses(): Promise<ApprovalProcessSummary[]> {
  const r = await req<{ data: { processes: ApprovalProcessSummary[] } }>("GET", "/admin/approval-processes");
  return r.data?.processes ?? [];
}
export async function getApprovalProcess(id: string): Promise<ApprovalProcessSummary> {
  const r = await req<{ data: { process: ApprovalProcessSummary } }>("GET", `/admin/approval-processes/${id}`);
  return r.data.process;
}
type ApprovalProcessBody<S> = { name: string; entityName: string; trigger: string; criteria?: Record<string, unknown>; steps: S[]; options?: ApprovalOptions; isActive?: boolean; outcome?: ApprovalOutcome };
// Two overloads: the Studio builder keeps its plain-step payload type; the AI Apply path may also
// send a many-approver step (specs/043 S4). The narrow signature is LAST on purpose — it is the one
// `Parameters<>`/mocks see, so existing callers and tests keep their types.
export function createApprovalProcess(payload: ApprovalProcessBody<ApprovalStep | ApprovalParallelStep>): Promise<ApprovalProcessSummary>;
export function createApprovalProcess(payload: ApprovalProcessBody<ApprovalStep>): Promise<ApprovalProcessSummary>;
export async function createApprovalProcess(payload: ApprovalProcessBody<ApprovalStep | ApprovalParallelStep>): Promise<ApprovalProcessSummary> {
  const r = await req<{ data: { process: ApprovalProcessSummary } }>("POST", "/admin/approval-processes", payload);
  return r.data.process;
}
/** Full-replace PUT. A linear body over a stored graph is 409 APPROVAL_PROCESS_V2_OVERWRITE unless `force` (?force=true); send `graph` instead of `steps` to keep the graph form. */
export async function updateApprovalProcess(id: string, payload: { name: string; entityName: string; trigger: string; criteria?: Record<string, unknown>; steps: ApprovalStep[]; graph?: ApprovalGraph; options?: ApprovalOptions; isActive?: boolean; outcome?: ApprovalOutcome }, opts?: { force?: boolean }): Promise<ApprovalProcessSummary> {
  const r = await req<{ data: { process: ApprovalProcessSummary } }>("PUT", `/admin/approval-processes/${id}${opts?.force ? "?force=true" : ""}`, payload);
  return r.data.process;
}
export async function deleteApprovalProcess(id: string): Promise<void> {
  await req("DELETE", `/admin/approval-processes/${id}`, undefined);
}

// ── Approval process versions, repin, stuck hồ sơ (specs/043 S10/S11/S7 — FE-BPM-VERSIONS) ──
export type ApprovalDraftPayload = Parameters<typeof updateApprovalProcess>[1];
/** PUT …/draft: store an unpublished edit (same full-replace body as a save). No hồ sơ starts from it. */
export async function saveApprovalDraft(id: string, payload: ApprovalDraftPayload): Promise<ApprovalProcessSummary> {
  const r = await req<{ data: { process: ApprovalProcessSummary } }>("PUT", `/admin/approval-processes/${id}/draft`, payload);
  return r.data.process;
}
/** DELETE …/draft: drop the unpublished edit; 422 APPROVAL_DRAFT_NOT_FOUND when there is none. */
export async function discardApprovalDraft(id: string): Promise<void> {
  await req("DELETE", `/admin/approval-processes/${id}/draft`, undefined);
}
/** How many hồ sơ of a process are waiting, and how many still run an older version. */
export interface ApprovalImpact { version: number; pendingTotal: number; pendingOnOldDefinition: number; byNode: { nodeId: string; name: string; count: number }[] }
/** POST …/publish: the draft becomes the next version. 422 APPROVAL_VERSION_NO_DRAFT / APPROVAL_DRAFT_STALE. `impact` is null when the server could not read it. */
export async function publishApprovalProcess(id: string, note?: string): Promise<{ process: ApprovalProcessSummary; version: number; impact: ApprovalImpact | null }> {
  const r = await req<{ data: { process: ApprovalProcessSummary; version: number; impact: ApprovalImpact | null } }>("POST", `/admin/approval-processes/${id}/publish`, { note: note ?? "" });
  return r.data;
}
export interface ApprovalVersionRow { id: string; processId: string; version: number; note: string | null; createdBy: string | null; createdAt: string; definition?: Record<string, unknown> }
/** GET …/versions: newest first, without the definitions. */
export async function listApprovalVersions(id: string): Promise<ApprovalVersionRow[]> {
  const r = await req<{ data: { versions: ApprovalVersionRow[] } }>("GET", `/admin/approval-processes/${id}/versions`);
  return r.data?.versions ?? [];
}
/** GET …/versions/:v: one version with its definition. */
export async function getApprovalVersion(id: string, version: number): Promise<ApprovalVersionRow> {
  const r = await req<{ data: { version: ApprovalVersionRow } }>("GET", `/admin/approval-processes/${id}/versions/${version}`);
  return r.data.version;
}
export async function getApprovalImpact(id: string): Promise<ApprovalImpact> {
  const r = await req<{ data: ApprovalImpact }>("GET", `/admin/approval-processes/${id}/impact`);
  return r.data;
}
export interface ApprovalRepinResult { instanceId: string; ok: boolean; reason?: string; fromVersion: number; toVersion: number; fromNode: string; toNode: string }
export interface ApprovalRepinOutcome { results: ApprovalRepinResult[]; remaining: number; nextAfter?: string }
/** POST …/repin. `dryRun` defaults to TRUE on the server: moving hồ sơ is applied only with an explicit `dryRun:false`. */
export async function repinApprovalInstances(id: string, body: { instanceIds?: string[]; toVersion?: number; dryRun: boolean; after?: string }): Promise<ApprovalRepinOutcome> {
  const r = await req<{ data: ApprovalRepinOutcome }>("POST", `/admin/approval-processes/${id}/repin`, body);
  return r.data;
}
export interface ApprovalInstanceRow {
  id: string; entityName: string; recordId: string; status: string; processVersion: number; nodeId: string; stepName: string;
  slaDueAt: string | null; escalated: boolean; createdAt: string; lastError?: string; stuck: boolean; effectFailed: boolean;
}
/** GET …/instances: the hồ sơ of a process; `stuck` keeps only those stalled on an automatic step. */
export async function listApprovalProcessInstances(id: string, opts: { stuck?: boolean; status?: string; version?: number } = {}): Promise<ApprovalInstanceRow[]> {
  const q = new URLSearchParams();
  if (opts.stuck) q.set("stuck", "true");
  if (opts.status) q.set("status", opts.status);
  if (opts.version != null) q.set("version", String(opts.version));
  const qs = q.toString();
  const r = await req<{ data: { instances: ApprovalInstanceRow[] } }>("GET", `/admin/approval-processes/${id}/instances${qs ? `?${qs}` : ""}`);
  return r.data?.instances ?? [];
}
/** POST /admin/approval-instances/:id/retry — "Thử lại" a hồ sơ stalled on an automatic step. 409 APPROVAL_NOT_STUCK. */
export async function retryApprovalInstance(instanceId: string, note?: string): Promise<void> {
  await req("POST", `/admin/approval-instances/${instanceId}/retry`, { note: note ?? "" });
}
/** POST /admin/approval-instances/:id/cancel — the Nexus team stops a hồ sơ for good. */
export async function cancelApprovalInstance(instanceId: string, note?: string): Promise<void> {
  await req("POST", `/admin/approval-instances/${instanceId}/cancel`, { note: note ?? "" });
}

// ── Admin Reports ──
// tabular: config.columns must have at least 1 element.
export async function createReport(payload: Record<string, unknown>): Promise<ReportSummary> {
  const r = await req<{ data: { report: ReportSummary } }>("POST", "/admin/reports", payload);
  return r.data.report;
}
export async function getAdminReport(id: string): Promise<ReportSummary> {
  const r = await req<{ data: { report: ReportSummary } }>("GET", `/admin/reports/${id}`);
  return r.data.report;
}
export async function updateReport(id: string, payload: Record<string, unknown>): Promise<ReportSummary> {
  const r = await req<{ data: { report: ReportSummary } }>("PUT", `/admin/reports/${id}`, payload);
  return r.data.report;
}
export async function deleteReport(id: string): Promise<void> {
  await req("DELETE", `/admin/reports/${id}`, undefined);
}
// GET /admin/reports — admin list (same shape as GET /reports but admin-scoped).
// Caller: components/studio/build/ReportsPage (specs/074 f2-report-list-view).
export async function listAdminReports(): Promise<ReportSummary[]> {
  const r = await req<{ data: { reports: ReportSummary[] } }>("GET", "/admin/reports");
  return r.data?.reports ?? [];
}

// ── Admin Dashboards ──
// Widget types: "kpi" | "bar" | "line" | "table" | "funnel" (funnel + x/y/w/h layout keys landed
// 2026-07-03). PUT is a FULL-REPLACE — send the COMPLETE widget array or unmodeled fields are lost.
export async function createDashboard(name: string, widgets: DashboardWidgetDef[], description?: string): Promise<DashboardSummary> {
  const r = await req<{ data: { dashboard: DashboardSummary } }>("POST", "/admin/dashboards", { name, description, widgets });
  return r.data.dashboard;
}
export async function getAdminDashboard(id: string): Promise<DashboardSummary> {
  const r = await req<{ data: { dashboard: DashboardSummary } }>("GET", `/admin/dashboards/${id}`);
  return r.data.dashboard;
}
export async function updateDashboard(id: string, name: string, widgets: DashboardWidgetDef[], description?: string): Promise<DashboardSummary> {
  const r = await req<{ data: { dashboard: DashboardSummary } }>("PUT", `/admin/dashboards/${id}`, { name, description, widgets });
  return r.data.dashboard;
}
export async function deleteDashboard(id: string): Promise<void> {
  await req("DELETE", `/admin/dashboards/${id}`, undefined);
}
export async function listAdminDashboards(): Promise<DashboardSummary[]> {
  const r = await req<{ data: { dashboards: DashboardSummary[] } }>("GET", "/admin/dashboards");
  return r.data?.dashboards ?? [];
}

// ── Admin Verticals (W8 — api-conventions §12) ──
// GET /admin/verticals/builtin → { data: { builtins: [{ slug, manifest, entityCount }] } }
//   (FE-CATCHUP-2 BREAKING: items were bare slug strings until 2026-07-08; now each entry carries
//   the pack's REAL self-describing manifest from the in-code registry — confirmed against
//   handlers.AdminVerticalHandler.ListBuiltin + vertical_pack.BuiltinInfos. The FE shows
//   name/version/description from HERE, never from a static catalog mirror.)
// POST /admin/verticals/install → { builtin: "crm"|"clinic" } or { pack: {...} }, ?dryRun=true.
// GET /admin/verticals/export?entities=a,b&includeData= → { data: { pack: {...} } }
// GET /admin/verticals/installs → { data: { installs: [InstallRecord] } }
import type { InstallPlan, InstallResult, InstallRecord, Manifest, Pack } from "@/lib/verticalPlan";

/** The two ways to name a pack for install/dry-run: a shipped builtin slug OR an inline pack. */
export type PackSource = { builtin: string } | { pack: Pack };

/** One shipped builtin pack as listed by GET /admin/verticals/builtin (BuiltinInfo on the BE). */
export interface BuiltinVertical {
  slug: string;
  /** The pack's REAL manifest (name/version/description/author) — same header install ledgers. */
  manifest: Manifest;
  /** How many entities installing this pack would create. */
  entityCount: number;
}

export async function listBuiltinVerticals(): Promise<BuiltinVertical[]> {
  const r = await req<{ data: { builtins?: Array<{
    slug?: string; manifest?: Manifest | null; entityCount?: number;
  }> } }>("GET", "/admin/verticals/builtin");
  // Defensive normalisation: shipped builtins always carry a manifest (BuiltinInfos builds it
  // from the registry), but a null/absent one must degrade to a named slug-only entry — the
  // library then simply shows no version rather than inventing one or crashing.
  return (r.data?.builtins ?? [])
    .filter((b) => typeof b?.slug === "string" && b.slug !== "")
    .map((b) => ({
      slug: b.slug as string,
      manifest: b.manifest ?? { name: b.slug as string, version: "" },
      entityCount: typeof b.entityCount === "number" ? b.entityCount : 0,
    }));
}

/**
 * installVertical — legacy fire-and-forget install used by older call sites. Swallows
 * RECORD_DUPLICATE (idempotent). New W8 wizard code uses `installPack` (returns the result and does
 * NOT swallow 409 — the whole point of the conflict path is to surface it).
 */
export async function installVertical(payload: { builtin: string } | { pack: Record<string, unknown> }): Promise<void> {
  try {
    await req("POST", "/admin/verticals/install", payload);
  } catch (e) {
    // RECORD_DUPLICATE = already installed — idempotent, not an error.
    if (e instanceof ApiError && e.code === "RECORD_DUPLICATE") return;
    throw e;
  }
}

/**
 * Install options (EASE Nước 4 — ../specs/027-handover-wizard/spec.md §3.1). `targetTenant` (slug, resolved
 * server-side, operator-gated MS-C) routes the install into ANOTHER workspace; omitted = current
 * tenant, byte-identical to the pre-MS-C behavior. `dryRun` flips the response envelope to a plan.
 */
export interface InstallOptions {
  dryRun?: boolean;
  /** Target workspace slug — omit for the current tenant (unchanged legacy behavior). */
  targetTenant?: string;
}

function installQuery(opts: InstallOptions): string {
  const params = new URLSearchParams();
  if (opts.dryRun) params.set("dryRun", "true");
  if (opts.targetTenant) params.set("targetTenant", opts.targetTenant);
  const qs = params.toString();
  return qs ? `?${qs}` : "";
}

/** dryRunInstall — POST install?dryRun=true → the plan the deploy wizard renders. Never writes.
 *  Pass `targetTenant` to plan against ANOTHER workspace (operator-gated on the BE). */
export async function dryRunInstall(
  source: PackSource, opts: Omit<InstallOptions, "dryRun"> = {},
): Promise<InstallPlan> {
  const r = await req<{ data: { plan: InstallPlan } }>(
    "POST", `/admin/verticals/install${installQuery({ ...opts, dryRun: true })}`,
    { ...source, dryRun: true },
  );
  return r.data.plan;
}

/**
 * installPack — install (POST) → InstallResult, or the dry-run plan when `opts.dryRun` is set
 * (overloads keep the return type honest). Does NOT swallow any error, so the wizard can surface
 * RECORD_DUPLICATE / VERTICAL_MANIFEST_INVALID_VERSION / TENANT_NOT_FOUND etc. to the admin.
 */
export async function installPack(source: PackSource, opts: InstallOptions & { dryRun: true }): Promise<InstallPlan>;
export async function installPack(source: PackSource, opts?: InstallOptions): Promise<InstallResult>;
export async function installPack(
  source: PackSource, opts: InstallOptions = {},
): Promise<InstallPlan | InstallResult> {
  if (opts.dryRun) return dryRunInstall(source, { targetTenant: opts.targetTenant });
  const r = await req<{ data: { result: InstallResult } }>(
    "POST", `/admin/verticals/install${installQuery(opts)}`, source,
  );
  return r.data.result;
}

// ── KHUON-SONG-P1: provenance diff (../specs/011-pack-provenance/spec.md §3) ─────────────────────────────

/** One pack item's comparison outcome against its as-installed provenance content. */
export interface PackDiffItem {
  itemType: string;
  itemKey: string;
  packVersion: string;
  status: "unchanged" | "modified" | "missing" | "undiffable";
  changedPaths?: string[];
  /** "Luôn giữ" flag (specs/069 FR-024): a marker beside the status, never a status itself. */
  protected?: boolean;
}

export interface PackDiffReport {
  slug: string;
  versions: string[];
  /** true = the pack was installed BEFORE provenance existed — nothing to diff (honest state). */
  untracked: boolean;
  items: PackDiffItem[];
  generatedAt: string;
}

/** diffPackInstall — GET the read-only "Đối chiếu khuôn" report for one installed pack. */
export async function diffPackInstall(slug: string): Promise<PackDiffReport> {
  const r = await req<{ data: PackDiffReport }>("GET", `/admin/verticals/installs/${encodeURIComponent(slug)}/diff`);
  return r.data;
}

// ── specs/069 L1: read-only 3-way upgrade plan ───────────────────────────────────────────────────

export type UpgradeStatus =
  | "added" | "update_safe" | "unchanged" | "in_sync" | "kept_custom" | "conflict" | "conflict_no_base"
  | "deleted_by_builder" | "conflict_deleted" | "removed_in_pack" | "removed_in_pack_modified"
  | "protected" | "undiffable" | "ambiguous_name" | "unsupported" | "draft_pending";

export interface UpgradeItem {
  itemType: string;
  itemKey: string;
  label: string;
  parentKey?: string;
  status: UpgradeStatus;
  /** What the builder may choose for this item (keep | apply | restore | delete | merge). */
  actions: string[];
  defaultAction: string;
  adopt?: boolean;
  theirsPaths: string[];
  oursPaths: string[];
  overlap: string[];
  /** L6: three-way merge of a both-sides-changed item, and the paths taken from the pack. Present only when "merge" is offered. */
  merged?: unknown;
  theirsTakenPaths?: string[];
  /** The three versions — present only for items that need a decision, secrets already stripped. */
  base?: unknown;
  theirs?: unknown;
  live?: unknown;
  liveId?: string;
  renamedTo?: string;
  sharedWith: string[];
  warnings: { code: string; level: "confirm" | "info"; text: string }[];
  reason?: string;
  /** Delivered projects only (specs/069 L7): how a change reaches the customer. */
  applyMode?: "draft" | "immediate_traced" | "immediate_untraced" | "immediate_hidden";
}

export interface UpgradePlan {
  slug: string;
  fromVersions: string[];
  toVersion: string;
  source: "builtin" | "upload";
  untracked: boolean;
  delivered: boolean;
  planHash: string;
  summary: Record<string, number>;
  warnings: string[];
  items: UpgradeItem[];
}

/**
 * planPackUpgrade — POST …/installs/:slug/upgrade-plan. Read-only: writes no config, no baseline,
 * no history. `source` is exactly one of a shipped pack or an uploaded file.
 */
export async function planPackUpgrade(
  slug: string, source: PackSource, opts: { targetTenant?: string } = {},
): Promise<UpgradePlan> {
  const qs = opts.targetTenant ? `?targetTenant=${encodeURIComponent(opts.targetTenant)}` : "";
  const r = await req<{ data: { plan: UpgradePlan } }>(
    "POST", `/admin/verticals/installs/${encodeURIComponent(slug)}/upgrade-plan${qs}`, source,
  );
  return r.data.plan;
}

// ── specs/069 L2a: apply the reviewed plan + upgrade history ────────────────────────────────────

export interface UpgradeDecision { itemType: string; itemKey: string; action: string }
export interface UpgradeResult {
  itemType: string; itemKey: string; action: string;
  /** applied | merged | restored | deleted | adopted | protected | kept | drafted | failed | not_run */
  outcome: string; code?: string; message?: string; liveId?: string;
}
export interface UpgradeRun {
  id: string; slug: string; fromVersions: string; toVersion: string; source: string; mode: string;
  /** running | done | partial | failed | interrupted */
  status: string; decisions: UpgradeDecision[]; results: UpgradeResult[]; counts: Record<string, number>;
  startedAt: string; finishedAt?: string; replicaLagSeconds?: number;
}

/**
 * applyPackUpgrade — POST …/installs/:slug/upgrade. `planHash` is the hash of the plan the builder
 * reviewed; the server recomputes under its run lock and answers 409 PACK_UPGRADE_PLAN_STALE
 * (details.planHash) when anything moved. `decisions` is only a choice among the offered actions.
 */
export async function applyPackUpgrade(
  slug: string, source: PackSource, planHash: string, decisions: UpgradeDecision[],
  opts: { targetTenant?: string; confirmImmediate?: boolean } = {},
): Promise<UpgradeRun> {
  const qs = opts.targetTenant ? `?targetTenant=${encodeURIComponent(opts.targetTenant)}` : "";
  const r = await req<{ data: { run: UpgradeRun } }>(
    "POST", `/admin/verticals/installs/${encodeURIComponent(slug)}/upgrade${qs}`,
    { ...source, planHash, decisions, ...(opts.confirmImmediate ? { confirmImmediate: true } : {}) },
  );
  return r.data.run;
}

/** listPackUpgrades — GET the upgrade history of one pack, newest first (limit 1..50, default 10). */
export async function listPackUpgrades(slug: string, limit = 10, opts: { targetTenant?: string } = {}): Promise<UpgradeRun[]> {
  const p = new URLSearchParams({ limit: String(limit) });
  if (opts.targetTenant) p.set("targetTenant", opts.targetTenant);
  const r = await req<{ data: { upgrades: UpgradeRun[] } }>("GET", `/admin/verticals/installs/${encodeURIComponent(slug)}/upgrades?${p}`);
  return r.data?.upgrades ?? [];
}

/** listPackInstalls — GET the append-only install ledger (tenant-scoped, newest-first). */
export async function listPackInstalls(): Promise<InstallRecord[]> {
  const r = await req<{ data: { installs: InstallRecord[] } }>("GET", "/admin/verticals/installs");
  return r.data?.installs ?? [];
}

/**
 * exportVertical — GET export → a downloadable pack + a `warnings` list. `entityNames` empty ⇒
 * export all entities of the tenant. `includeData` embeds sample records AND their N-N join rows
 * (Slice 2.2 PACK-EXPORT-FULL; metadata-only by default).
 *
 * `warnings` names everything the pack could NOT faithfully carry (a workflow on an excluded
 * entity, an unresolvable assign team, a report with joins, a webhook secret traveling in JSON,
 * a truncated sample dump…) so the UI can show an honest included/excluded manifest instead of
 * silently dropping. Empty array = the pack is complete.
 */
export async function exportVertical(
  entityNames: string[] = [], includeData = false,
  // Slice 2.3 PACK-EXPORT-APPSLUG: identify the source app so the pack's manifest carries a real
  // slug/name/icon and re-install mints a correctly-named sidebar app. Omit for a generic pack.
  app?: { slug: string; name?: string; icon?: string; description?: string },
): Promise<{ pack: Pack; warnings: string[] }> {
  const params = new URLSearchParams();
  if (entityNames.length) params.set("entities", entityNames.join(","));
  if (includeData) params.set("includeData", "true");
  if (app?.slug) {
    params.set("appSlug", app.slug);
    if (app.name) params.set("appName", app.name);
    if (app.icon) params.set("appIcon", app.icon);
    if (app.description) params.set("appDescription", app.description);
  }
  const qs = params.toString();
  const r = await req<{ data: { pack: Pack; warnings?: string[] } }>("GET", `/admin/verticals/export${qs ? `?${qs}` : ""}`);
  return { pack: r.data.pack, warnings: r.data.warnings ?? [] };
}

/** A table the picked ones need but did not name (rollup child, relation target) — queue #296. */
export interface EntityDependency { name: string; label: string; reason: string }
/** GET /admin/verticals/dependencies — tables to pre-tick so the handover pack does not break. */
export async function entityDependencies(entityNames: string[]): Promise<EntityDependency[]> {
  if (entityNames.length === 0) return [];
  const r = await req<{ data: EntityDependency[] }>("GET", `/admin/verticals/dependencies?entities=${encodeURIComponent(entityNames.join(","))}`);
  return r.data ?? [];
}

// ── PROJECT REGISTRY (operator-only; specs/062-customer-workspace L5, specs/073 so1a) ──
// GET /admin/projects is the ONE source of the project board, the switcher and the "Cần anh xem" band: nothing here
// asks /admin/tenants, /admin/health or …/backups per customer (no N+1). `stage` is the server's word; the client never
// derives it. Fields marked optional arrive when their lane lands (065 usableOwnerCount, 066 health.ops, 068 backup /
// maintenanceUntil): absent means "not told", and the UI shows no line for it.
export type ProjectStage = "building" | "trial" | "live" | "needs_attention";
export type ProjectHealthStatus = "unknown" | "ok" | "warn" | "critical";
export interface ProjectHealth {
  status: ProjectHealthStatus;
  critical: number;
  warn: number;
  checks: { checkId: string; severity: string; count: number }[];
  appsNotReady: { appId: string; name: string; reasons: string[] }[];
  checkedAt: string | null;
  /** 066: background-job health, kept apart from `status` on purpose. */
  ops?: { status: ProjectHealthStatus; checkedAt: string | null };
}
export interface ProjectCounts {
  apps: number; entities: number; customLayouts: number; workflows: number; approvals: number;
  roles: number; users: number; staffUsers: number; reports: number; dashboards: number;
}
export interface ProjectRow {
  tenantId: string; slug: string; name: string; tenantStatus: string;
  industry: string | null; origin: string | null; originPack: string | null;
  stage: ProjectStage;
  createdAt: string; trialSince: string | null; deliveredAt: string | null;
  branded: boolean;
  health: ProjectHealth;
  counts: ProjectCounts;
  lastActiveAt: string | null;
  usableOwnerCount?: number;
  maintenanceUntil?: string | null;
  backup?: { lastReadyAt: string | null; lastFailed: boolean };
}
/** GET /admin/projects — every live customer with stage, counts and the stored health snapshot. */
export async function listProjects(): Promise<ProjectRow[]> {
  const r = await req<{ data: ProjectRow[] | null }>("GET", "/admin/projects");
  return r.data ?? [];
}
/** PATCH /admin/projects/:tenantId — only these two keys exist. `industry: null` (or blank) clears it; `trial` is the
 *  "Chạy thử" switch. 422 PROJECT_TRIAL_AFTER_DELIVERY, PROJECT_INDUSTRY_TOO_LONG. */
export async function patchProject(tenantId: string, patch: { industry?: string | null; trial?: boolean }): Promise<ProjectRow> {
  const r = await req<{ data: ProjectRow }>("PATCH", `/admin/projects/${encodeURIComponent(tenantId)}`, patch);
  return r.data;
}
/** POST /admin/projects/:tenantId/health-check — "Khám lại": runs the check now and stores the snapshot. */
export async function healthCheckProject(tenantId: string): Promise<ProjectHealth> {
  const r = await req<{ data: ProjectHealth }>("POST", `/admin/projects/${encodeURIComponent(tenantId)}/health-check`);
  return r.data;
}

export interface CreateProjectInput {
  name: string; slug: string; industry?: string; adminEmail: string; adminFullName?: string;
  adminPassword?: string; generatePassword?: boolean;
  start: { mode: "blank" | "pack" | "ai"; pack?: string };
}
export interface CreatedProject {
  slug: string; name: string; adminEmail: string;
  /** Show-once plaintext (only with generatePassword): the caller keeps it in component state, never in a cache. */
  password: string | null;
  start: { mode: string; status: "done" | "failed" | "skipped"; error?: { code: string; message: string } };
}
/** POST /admin/projects — operator-only: workspace + first owner + starter, in one call. A failed starter or session
 *  comes back INSIDE the 201 (start.status). Plain async call on purpose: no mutation cache may hold the password.
 *  The builder token in `data.workspace` is dropped here; "Mở dự án" opens a fresh session via enterWorkspace. */
export async function createProject(input: CreateProjectInput): Promise<CreatedProject> {
  const r = await req<{ data: { project: { slug: string; name: string }; admin: { email: string }; start: CreatedProject["start"]; oneTimeCredentials?: { password: string } } }>("POST", "/admin/projects", input);
  const d = r.data;
  return { slug: d.project.slug, name: d.project.name, adminEmail: d.admin.email, password: d.oneTimeCredentials?.password ?? null, start: d.start };
}

// ── TENANT-PROVISION (operator-only: create + list customer workspaces) ──
export interface TenantSummary {
  id: string; slug: string; name: string; status: string; createdAt: string; usableAdminCount: number;
  /** Active customer owners (specs/065) and the role-less admins that "Chuyển sang chủ khách" would convert. */
  usableOwnerCount: number; convertibleAdminCount: number;
  /** Set when the workspace has been retired (soft-deleted). Retired workspaces stay in the
   *  registry because purge only accepts one — but they must NEVER be offered as a deploy target:
   *  a retired workspace refuses logins and inbound signals, so installing into one hands the
   *  customer a dead workspace. Filter on this anywhere a target is chosen. */
  retiredAt?: string;
  /** Set while the workspace is write-frozen (specs/068 slice B) and the deadline has not passed. */
  maintenanceUntil?: string;
}
/** GET /admin/tenants — operator-only registry for the deploy target selector. Throws ApiError
 *  (code FORBIDDEN) when the caller is not a platform operator, so the UI hides the affordance. */
export async function listTenants(): Promise<TenantSummary[]> {
  const r = await req<{ data: TenantSummary[] }>("GET", "/admin/tenants");
  return r.data ?? [];
}
/** POST /admin/tenants/:id/owner-tier — turn the workspace's role-less admins into customer owners (specs/065). Idempotent. */
export interface OwnerTierEnabled { converted: number; skippedWithRole: number; ownerRoleId: string; revokeFailed: number }
export async function setOwnerTier(id: string): Promise<OwnerTierEnabled> {
  const r = await req<{ data: OwnerTierEnabled }>("POST", `/admin/tenants/${encodeURIComponent(id)}/owner-tier`);
  return r.data;
}
/** DELETE /admin/tenants/:id/owner-tier — return the customer owners to admins. Idempotent. */
export interface OwnerTierReverted { reverted: number; revokeFailed: number }
export async function revertOwnerTier(id: string): Promise<OwnerTierReverted> {
  const r = await req<{ data: OwnerTierReverted }>("DELETE", `/admin/tenants/${encodeURIComponent(id)}/owner-tier`);
  return r.data;
}
/**
 * retireTenant — DELETE /admin/tenants/:id: soft-delete a customer workspace.
 *
 * Soft, and that is enough: login and inbound webhooks both stop working immediately (the receive
 * path resolves the tenant first, so even a correctly-signed signal is refused). The data stays,
 * recoverable by an operator with database access. There is no un-delete in the API on purpose —
 * reviving credentials, hooks and data in one click is a far bigger blast radius than removing them.
 *
 * The server refuses TENANT_SELF_RETIRE if you aim it at the workspace you are standing in.
 */
export async function retireTenant(id: string): Promise<void> {
  await req("DELETE", `/admin/tenants/${encodeURIComponent(id)}`);
}

export interface PurgeReport {
  tenantId: string; slug: string; dryRun: boolean; totalRows: number;
  tables: { table: string; rows: number }[];
  /** specs/068: the customer's own backup zips removed with it (dry run: 0 + a note saying how many would go). */
  backupsRemoved?: number;
  /** Set when some backup files could not be removed (or are not wired): they are STILL on disk. */
  backupsNote?: string;
}

// ── Per-customer backups (specs/068-tenant-backup-restore, lát A; operator-only) ──
// One zip = one customer's data + its attachment bytes, no passwords or keys. The routes sit behind
// RequirePlatformOperator exactly like /admin/tenants, so a non-operator gets 403 FORBIDDEN here.
export type BackupKind = "manual" | "scheduled" | "pre_restore";
export type BackupStatus = "running" | "ready" | "failed";
export interface BackupWarning { code: string; count: number; sample?: string[] }
export type NotRestorableReason = "SCHEMA_MISMATCH" | "NOT_READY" | "FILE_MISSING" | "TENANT_RETIRED" | "SELF";
export interface TenantBackup {
  id: string; tenantId: string; kind: BackupKind; status: BackupStatus; note: string | null;
  sizeBytes: number | null; sha256: string | null; schemaVersion: number; formatVersion: number;
  rowCount: number | null; tableCount: number | null; fileCount: number | null; blobBytes: number | null;
  warnings: BackupWarning[];
  error: { code: string; message: string } | null;
  createdAt: string; finishedAt: string | null;
  createdBy: { id: string; label: string } | null;
  restorable: boolean; notRestorableReason: NotRestorableReason | null;
  /** Only on a `pre_restore` row (specs/068 lát D): the restore it belongs to. */
  restore?: {
    sourceBackupId: string; status: "running" | "done" | "failed";
    /** lock | prebackup | apply | doctor — written by the server at each step */
    step: "lock" | "prebackup" | "apply" | "doctor" | null;
    committedAt: string | null; report: RestoreReport | null; finishedAt: string | null;
  } | null;
}
/** restore_report once a restore ends (specs/068 lát D). */
export interface RestoreReport {
  sourceBackupId: string; preRestoreBackupId: string;
  deleted?: Record<string, number>; inserted?: Record<string, number>;
  rolesLost: number; membershipsSkipped: number; sharesSkipped: number; webhookKeysDropped: number;
  blobsWritten: number; blobsRemoved: number;
  doctor?: { fixed: number; remainingCritical: number };
  accessChanges?: AccessChanges;
  resumedAfterCrash?: boolean;
}
export interface TenantBackupList {
  data: TenantBackup[];
  meta: { currentSchemaVersion: number; maintenanceUntil: string | null; nextCursor: string | null };
}
const tenantBackupsPath = (tenantId: string) => `/admin/tenants/${encodeURIComponent(tenantId)}/backups`;
/** GET /admin/tenants/:id/backups — newest first; pass `after` = the previous `meta.nextCursor`. */
export async function listTenantBackups(tenantId: string, opts: { limit?: number; after?: string } = {}): Promise<TenantBackupList> {
  const qs = new URLSearchParams();
  if (opts.limit) qs.set("limit", String(opts.limit));
  if (opts.after) qs.set("after", opts.after);
  const q = qs.toString();
  const r = await req<TenantBackupList>("GET", `${tenantBackupsPath(tenantId)}${q ? `?${q}` : ""}`);
  return { data: r.data ?? [], meta: r.meta ?? { currentSchemaVersion: 0, maintenanceUntil: null, nextCursor: null } };
}
/** POST /admin/tenants/:id/backups — 202; the zip is written in the background (status "running"). */
export async function createTenantBackup(tenantId: string, note?: string): Promise<TenantBackup> {
  const r = await req<{ data: TenantBackup }>("POST", tenantBackupsPath(tenantId), note?.trim() ? { note: note.trim() } : {});
  return r.data;
}
/** POST /admin/tenants/:id/maintenance — freeze the customer's writes for 1..120 minutes. */
export async function setTenantMaintenance(tenantId: string, minutes: number): Promise<{ maintenanceUntil: string }> {
  const r = await req<{ data: { maintenanceUntil: string } }>("POST", `/admin/tenants/${encodeURIComponent(tenantId)}/maintenance`, { minutes });
  return r.data;
}
/** DELETE /admin/tenants/:id/maintenance — lift the freeze (204, idempotent). */
export async function clearTenantMaintenance(tenantId: string): Promise<void> {
  await req<unknown>("DELETE", `/admin/tenants/${encodeURIComponent(tenantId)}/maintenance`);
}
/** GET /admin/tenants/:id/backups/:backupId — the poll target. */
export async function getTenantBackup(tenantId: string, backupId: string): Promise<TenantBackup> {
  const r = await req<{ data: TenantBackup }>("GET", `${tenantBackupsPath(tenantId)}/${encodeURIComponent(backupId)}`);
  return r.data;
}
/** DELETE /admin/tenants/:id/backups/:backupId — 204; 409 BACKUP_DELETE_IN_USE while it is being written / restored. */
export async function deleteTenantBackup(tenantId: string, backupId: string): Promise<void> {
  await req<unknown>("DELETE", `${tenantBackupsPath(tenantId)}/${encodeURIComponent(backupId)}`);
}
// ── Automatic backup schedule (specs/068 lát E): no saved row = enabled, keep 7 ──
export interface TenantBackupSchedule {
  enabled: boolean; keepScheduled: number; hourLocal: number;
  lastScheduledAt: string | null; nextDueAt: string | null;
}
const tenantSchedulePath =(tenantId: string) => `/admin/tenants/${encodeURIComponent(tenantId)}/backup-schedule`;
/** GET /admin/tenants/:id/backup-schedule */
export async function getTenantBackupSchedule(tenantId: string): Promise<TenantBackupSchedule> {
  const r = await req<{ data: TenantBackupSchedule }>("GET", tenantSchedulePath(tenantId));
  return r.data;
}
/** PUT /admin/tenants/:id/backup-schedule — both fields required; keepScheduled 1..30 (else 422 BACKUP_SCHEDULE_INVALID). */
export async function putTenantBackupSchedule(tenantId: string, enabled: boolean, keepScheduled: number): Promise<TenantBackupSchedule> {
  const r = await req<{ data: TenantBackupSchedule }>("PUT", tenantSchedulePath(tenantId), { enabled, keepScheduled });
  return r.data;
}
/** GET that answers bytes (a zip): Authorization header (never a token in the URL), the leave latch, one 401 recovery,
 *  an AbortSignal. The Blob is returned to the caller and kept nowhere here (specs/079 FR-103). */
async function reqBlob(path: string, signal?: AbortSignal, _retried = false): Promise<Blob> {
  if (isWorkspaceLeaving()) throw workspaceEnded();
  const t = getToken();
  const res = await fetch(`${BASE}${path}`, { headers: { ...(t ? { Authorization: `Bearer ${t}` } : {}), ...previewHeaders("GET") }, signal });
  if (res.status === 401 && !_retried) {
    await recover401();
    return reqBlob(path, signal, true);
  }
  if (!res.ok) {
    const json = await res.json().catch(() => ({}));
    throw new ApiError(res.status, json.code ?? "DOWNLOAD_FAILED", json.error ?? "Không tải được bản sao lưu", json.details ?? {});
  }
  return res.blob();
}
/** GET …/:backupId/file — fetch → blob → object URL → save (the route needs the Bearer header, so a
 *  plain <a href> cannot do it). Throws ApiError with the server's code (BACKUP_NOT_READY /
 *  BACKUP_FILE_MISSING…) so the UI can show the server's own sentence. */
export async function downloadTenantBackup(tenantId: string, backupId: string, filename: string, signal?: AbortSignal): Promise<{ sizeBytes: number }> {
  const blob = await reqBlob(`${tenantBackupsPath(tenantId)}/${encodeURIComponent(backupId)}/file`, signal);
  // Changed project / left the page while the bytes came: nothing is clicked (specs/079 FR-103).
  if (signal?.aborted) throw new DOMException("aborted", "AbortError");
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
  return { sizeBytes: blob.size };
}
// ── Restore check (specs/068 lát C): POST /admin/tenants/:id/restores, read-only ──
export interface AccessChanges {
  total: number;
  roleGrantsWidened: number; fieldAclReopened: number; sharesRegranted: number; membershipsReadded: number;
  sensitiveFieldsCleared: number; workflowsReenabled: number; outboundDestinationsReenabled: number;
  /** host names / key names only — never a full URL */
  outboundHosts: string[];
  usersAffected: number; apiKeysAffected: number;
}
export type RestoreWarningCode = "ROLE_LOST" | "MEMBERSHIP_SKIPPED" | "PENDING_JOBS_KEPT" | "BLOB_MISSING_IN_BACKUP" | "WEBHOOK_KEY_DROPPED" | "ACCESS_WIDENED";
export interface RestorePlan {
  backupId: string; backupCreatedAt: string; integrity: { ok: boolean };
  schema: { backup: number; current: number; ok: boolean };
  groups: { group: "data" | "config" | "access" | "files"; label: string;
            tables: { table: string; label: string; rowsInBackup: number; rowsNow: number }[] }[];
  blobs: { toWrite: number; bytesToWrite: number; toRemove: number };
  warnings: { code: RestoreWarningCode; count: number }[];
  accessChanges: AccessChanges;
  kept: string[];
  preRestoreEstimate: { rows: number; files: number; bytes: number; freeBytes: number };
}
/** POST /admin/tenants/:id/restores with `dryRun:true` (explicit, never rely on the server default): validates the backup and returns
 *  what a restore would change. Writes nothing. Refusals are ApiError with the server's code
 *  (TENANT_BACKUP_CORRUPT, TENANT_RESTORE_SCHEMA_MISMATCH, …). */
export async function previewTenantRestore(tenantId: string, backupId: string): Promise<RestorePlan> {
  const r = await req<{ data: RestorePlan }>("POST", `/admin/tenants/${encodeURIComponent(tenantId)}/restores`, { backupId, dryRun: true });
  return r.data;
}
/** POST /admin/tenants/:id/restores with `dryRun:false` + the typed customer slug — 202. The restore
 *  runs on the server (freeze, self backup, one transaction); follow it with getTenantBackup(tenantId,
 *  restoreId) and read `restore.step` / `restore.status`. Refusals: TENANT_RESTORE_CONFIRM_MISMATCH,
 *  BACKUP_IN_PROGRESS, BACKUP_DISK_LOW, plus every code of the check. */
export async function startTenantRestore(tenantId: string, backupId: string, confirmSlug: string): Promise<{ restoreId: string; status: "running" }> {
  const r = await req<{ data: { restoreId: string; status: "running" } }>("POST", `/admin/tenants/${encodeURIComponent(tenantId)}/restores`,
    { backupId, confirmSlug, dryRun: false });
  return r.data;
}
/**
 * purgeTenant — POST /admin/tenants/:id/purge: PERMANENTLY delete a retired workspace's data.
 *
 * This is the other half of retire, and the only call in this client with no undo of any kind:
 * it removes every row the customer owns across every tenant-scoped table, then the tenant itself.
 *
 * `dryRun` defaults to TRUE here for the same reason the server defaults it that way — a caller
 * that forgets the flag must get a COUNT, never a deletion. The server additionally refuses unless
 * the workspace is already retired (TENANT_PURGE_NOT_RETIRED) and `confirmSlug` matches the real
 * slug exactly (TENANT_PURGE_CONFIRM_MISMATCH); the UI does not re-implement those checks.
 */
export async function purgeTenant(id: string, confirmSlug: string, dryRun = true): Promise<PurgeReport> {
  const r = await req<{ data: PurgeReport }>("POST", `/admin/tenants/${encodeURIComponent(id)}/purge`, {
    confirmSlug, dryRun,
  });
  return r.data;
}

export interface ProvisionTenantInput {
  slug: string; name?: string; adminEmail: string; adminFullName?: string;
  adminPassword?: string; generatePassword?: boolean;
}
export interface ProvisionedTenant {
  tenant: { id: string; slug: string; name: string; status: string; createdAt: string };
  admin: { id: string; email: string; fullName?: string; isAdmin: boolean; canManageUsers?: boolean; roleId?: string };
  // Present ONLY when generatePassword was set — the plaintext is returned ONCE and never re-fetchable.
  oneTimeCredentials?: { password: string };
}
/** POST /admin/tenants — operator-only: create a customer workspace + first admin atomically. */
export async function provisionTenant(input: ProvisionTenantInput): Promise<ProvisionedTenant> {
  const r = await req<{ data: ProvisionedTenant }>("POST", "/admin/tenants", input);
  return r.data;
}

// ── Admin Users / Roles (for approver pickers) ──
export interface AdminUser { id: string; tenantId: string; email: string; fullName: string; status: string; isAdmin: boolean; canManageUsers?: boolean; roleId?: string }
export async function listAdminUsers(): Promise<AdminUser[]> {
  const r = await req<{ data: { users: AdminUser[] } }>("GET", "/admin/users");
  return r.data?.users ?? [];
}
// listRoles / getRole already exist above; they cover the admin roles picker.

// ── Audit log (RM-19) — tenant-wide change history (admin) ──
export interface AuditEntry {
  id: string; actorUserId: string; entityName: string; recordId: string; action: string;
  /** "Họ tên (Tên xưởng)" of a Nexus builder who worked in this workspace — computed by the server at read time
   *  (specs/062 L3); absent for the customer's own users. The UI never reads diff.after.by itself. */
  actorLabel?: string;
  diff?: { before?: Record<string, unknown>; after?: Record<string, unknown> };
  at: string;
}

// ── Roles (admin ACL) ──
// Shape grant khớp Go struct auth.RoleAction (verified live :8090 + DB acl_role_actions):
//   action ∈ view|create|edit|delete · accessLevel ∈ none|owner|team|all|field:<trường Người dùng> (#283: "hồ sơ mà trường đó là tôi") · entityName ('*' = mọi entity)
// LƯU Ý QUAN TRỌNG (verified bằng curl): PUT /admin/roles/:id là REPLACE-SET, KHÔNG phải patch —
// phải gửi TOÀN BỘ ma trận mỗi lần lưu, nếu không các grant khác bị xoá im lặng.
export type RoleAccessLevel = "none" | "owner" | "team" | "all" | `field:${string}`;
export type RoleActionVerb = "view" | "create" | "edit" | "delete";
export interface RoleGrant {
  entityName: string;
  action: RoleActionVerb;
  accessLevel: RoleAccessLevel;
}
export interface RoleSummary { id: string; name: string; label?: string; actions?: RoleGrant[] }
export interface RoleDetail extends RoleSummary { tenantId: string; actions: RoleGrant[] }

// GET danh sách vai trò (KHÔNG kèm grants — backend ListRoles trả "safe view" cho list page).
export async function listRoles(): Promise<RoleSummary[]> {
  const r = await req<{ data: { roles: RoleSummary[] } }>("GET", "/admin/roles");
  return r.data?.roles ?? [];
}

// GET 1 vai trò KÈM grants (backend GetRole load actions từ acl_role_actions) — endpoint đọc ma trận thật.
export async function getRole(id: string): Promise<RoleDetail> {
  const r = await req<{ data: { role: RoleDetail } }>("GET", `/admin/roles/${id}`);
  return r.data.role;
}

// PUT thay toàn bộ grants của 1 vai trò (replace-set). Gửi đủ ma trận. Trả role đã cập nhật (echo actions).
export async function updateRolePermissions(
  id: string,
  payload: { name: string; label?: string; actions: RoleGrant[] },
): Promise<RoleDetail> {
  const r = await req<{ data: { role: RoleDetail } }>("PUT", `/admin/roles/${id}`, payload);
  return r.data.role;
}

// ── Apps (App container — ../specs/010-app-container/spec.md §3, Phase 2) ──
// An app is a LENS over tenant-global entities/dashboards: an ordered nav of references,
// NOT an ownership container. URLs stay flat (/:entity) — the active app only changes the
// sidebar context (spec §0 locked decision #1). All shapes verified live on :8090 2026-07-10.

/** App summary as served to EVERY user by GET /apps (App Switcher). icon/color may be null. */
export interface AppSummary {
  id: string;
  name: string;          // slug — ^[a-z][a-z0-9_]{0,59}$, immutable after create
  label: string;
  icon: string | null;   // lucide icon name (curated set — lib/apps.tsx maps name → component)
  color: string | null;  // theme tone token key ("ac" | "cy" | …), NOT a hex — no new colors
  isDefault: boolean;
  sortOrder: number;
  /** Dashboard the app's home page leads with ("" / absent = none). */
  homeDashboardId?: string;
  /** Work queues the app home shows (builder's explicit pick); empty/absent = derived from the menu tables. */
  homeQueueIds?: string[];
  /** True when the signed-in user's role is the one this app is the default for (GET /apps only). */
  defaultForCaller?: boolean;
  /** Set only while the builder reads over the draft overlay of a delivered workspace (specs/063 S7-4). */
  draftState?: DraftState;
}

/** One resolved nav entry from GET /apps/:name/nav — ALREADY ACL-filtered server-side (INV-3). */
export interface AppNavEntry {
  type: "entity" | "dashboard";
  refId: string;
  label: string;         // override label or the entity/dashboard's own
  icon?: string;
  href: string;          // ready to use: "/<entityName>" | "/dashboards/<id>"
}
export interface AppNav { app: AppSummary; items: AppNavEntry[] }

// GET /apps — tenant's apps sorted by sortOrder (any authenticated user; app list is NOT an
// ACL boundary — only the nav inside is filtered).
export async function appsCatalog(): Promise<AppSummary[]> {
  const r = await req<{ data: { apps: AppSummary[] } }>("GET", "/apps");
  return r.data?.apps ?? [];
}

// GET /apps/:name/nav — the app's ordered nav, entity items hidden when !CanViewEntity.
export async function appNav(name: string): Promise<AppNav> {
  const r = await req<{ data: AppNav }>("GET", `/apps/${encodeURIComponent(name)}/nav`);
  return r.data;
}

/** Full admin row (GET /admin/apps[/:id]) — adds description/navCount/audit fields. */
export interface AdminApp extends AppSummary {
  description: string | null;
  navCount: number;
  /** Roles that land in this app at sign-in (admin read only). */
  defaultRoleIds?: string[];
  createdAt?: string;
  updatedAt?: string;
}

/** Raw nav row from GET /admin/apps/:id — unresolved (refId, no href), for the nav editor. */
export interface AdminAppNavRow {
  id: string;
  itemType: "entity" | "dashboard";
  refId: string;
  label: string | null;  // null = use the entity/dashboard's own label
  sortOrder: number;
}
/** TRỤ 2 mục 4 item 5 — "App sẵn sàng thao tác" honesty gate (services.AppService.Readiness,
 * docs/NEXUS-MOT-XUONG-2026-07-29.md). Ready is true only when every nav entity has ≥1 field,
 * a non-admin role can view them, AND a non-admin user actually holds that role — field count
 * alone (the old AppOverview.tsx inference) says nothing about whether a real employee can get in.
 * Optional: the handler OMITS this key (not null) when WithReadiness was never wired (old backend)
 * or when the computation itself failed — always feature-detect with `readiness &&`, never assume. */
export interface AppReadiness {
  entityCount: number;
  entitiesMissingFields: string[];
  allEntitiesHaveFields: boolean;
  hasNonAdminViewRole: boolean;
  hasNonAdminUserAssigned: boolean;
  ready: boolean;
}
export interface AdminAppDetail { app: AdminApp; items: AdminAppNavRow[]; readiness?: AppReadiness }

export async function adminApps(): Promise<AdminApp[]> {
  const r = await req<{ data: { apps: AdminApp[] } }>("GET", "/admin/apps");
  return r.data?.apps ?? [];
}

export async function adminApp(id: string): Promise<AdminAppDetail> {
  const r = await req<{ data: AdminAppDetail }>("GET", `/admin/apps/${id}`);
  // Defensive: empty nav must be [], never null (Go nil-slice marshals as null).
  return { ...r.data, items: r.data?.items ?? [] };
}

export interface CreateAppPayload {
  name: string;
  label: string;
  icon?: string;
  color?: string;
  description?: string;
  isDefault?: boolean;
  sortOrder?: number;
}
// Errors: 409 APP_ALREADY_EXISTS · 422 APP_NAME_INVALID / APP_LABEL_REQUIRED / APP_FIELD_TOO_LONG.
export async function createApp(payload: CreateAppPayload): Promise<AdminApp> {
  const r = await req<{ data: { app: AdminApp } }>("POST", "/admin/apps", payload);
  return r.data.app;
}

/** ⚠️ PUT /admin/apps/:id is FULL-REPLACE: every mutable field must be sent — a missing field
 * RESETS it (description/icon/color → NULL, sortOrder → 0, isDefault → false). Callers must
 * read-modify-write; this type makes all mutable fields required so a partial payload is a
 * compile error, not a silent reset. Slug (`name`) is immutable — not part of the payload. */
export interface UpdateAppPayload {
  label: string;
  description: string | null;
  icon: string | null;
  color: string | null;
  isDefault: boolean;
  sortOrder: number;
  /** Optional on purpose: omitted ⇒ the server KEEPS the app's home dashboard ("" clears it). */
  homeDashboardId?: string;
  /** Optional: omitted ⇒ KEEP; [] clears. 422 APP_HOME_QUEUE_INVALID when an id is not a queue. */
  homeQueueIds?: string[];
  /** Optional: omitted ⇒ KEEP; [] clears. 422 APP_DEFAULT_ROLE_INVALID when an id is not a role. */
  defaultRoleIds?: string[];
}
export async function updateApp(id: string, payload: UpdateAppPayload): Promise<AdminApp> {
  const r = await req<{ data: { app: AdminApp } }>("PUT", `/admin/apps/${id}`, payload);
  return r.data.app;
}

/** The server stores the app home + per-role default (queue #289/#483) and the customer face reads them
 *  (HomeScreen, resolveActiveApp) - the Studio panel only draws while this is true (specs/074 T336a). */
export const APP_HOME_BACKEND = true;

/** Save only the app-home fields; the PUT is full-replace so every other mutable field is carried from the row. */
export async function saveAppHome(
  app: AdminApp,
  home: { homeDashboardId: string; homeQueueIds: string[]; defaultRoleIds: string[] },
): Promise<AdminApp> {
  return updateApp(app.id, {
    label: app.label, description: app.description, icon: app.icon, color: app.color,
    isDefault: app.isDefault, sortOrder: app.sortOrder, ...home,
  });
}

// DELETE /admin/apps/:id → 204. Soft-delete of the LENS only — entities/dashboards stay intact.
export async function deleteApp(id: string): Promise<void> {
  await req("DELETE", `/admin/apps/${id}`, undefined);
}

/** Replace-all nav payload item — array order IS the nav order; label omitted = inherit. */
export interface AppNavReplaceItem {
  itemType: "entity" | "dashboard";
  refId: string;
  label?: string;
}
// PUT /admin/apps/:id/nav — body is the BARE array (not an object). 422 APP_NAV_REF_INVALID
// when a refId doesn't exist in this tenant.
export async function replaceAppNav(id: string, items: AppNavReplaceItem[]): Promise<AdminAppNavRow[]> {
  const r = await req<{ data: { items: AdminAppNavRow[] } }>("PUT", `/admin/apps/${id}/nav`, items);
  return r.data?.items ?? [];
}

// ── App ACL backfill — POST /admin/apps/:id/acl-backfill (TRỤ 2 mục 4 gap-closer) ──
// Re-applies the SAME idempotent default-role seed CreateEntity runs at entity-creation time,
// for EVERY entity in the app's nav — closes the "bảng cũ đang 403" hole for entities that
// predate that wiring. ?dryRun=true computes the IDENTICAL seeded/skipped classification without
// writing anything (failed is always [] there — nothing was attempted). skipped entries are
// type-prefixed by the BE: "dashboard:<refId>" (nothing to seed) / "orphan:<refId>" (nav ref
// whose entity is gone). Always 200 with the visible result — surface failed[] ROW BY ROW with
// its reason; a bulk grant-expansion must never swallow a failure.
export interface AclBackfillFailure { entity: string; reason: string }
export interface AclBackfillResult {
  seeded: string[];
  skipped: string[];
  failed: AclBackfillFailure[];
}
export async function appAclBackfill(id: string, dryRun: boolean): Promise<AclBackfillResult> {
  const r = await req<{
    data: { seeded?: string[] | null; skipped?: string[] | null; failed?: AclBackfillFailure[] | null };
  }>("POST", `/admin/apps/${id}/acl-backfill?dryRun=${dryRun ? "true" : "false"}`);
  // Defensive: Go nil-slices marshal as null (service pre-inits to [], but stay symmetric with adminApp).
  return { seeded: r.data?.seeded ?? [], skipped: r.data?.skipped ?? [], failed: r.data?.failed ?? [] };
}

// ── Nexus Doctor (DOCTOR-CORE — ../specs/008-nexus-doctor/spec.md §2.3) — admin drift scanner ──
// Contract built AGAINST THE SPEC while the BE lands in parallel; E2E verify after both merge.
// scan is READ-ONLY absolute (the Nexus rebuildAuditTables lesson); fix re-detects per item.

export type DoctorSeverity = "info" | "warn" | "critical";

/** One drifted item inside a finding — key (identifier) + human detail. Bounded server-side. */
export interface DoctorItem { key: string; detail: string }

export interface DoctorFinding {
  checkId: string;
  severity: DoctorSeverity;
  /** true = eligible for the "Sửa tất cả (an toàn)" batch; false = needs explicit confirm. */
  autoFixSafe: boolean;
  /** true = no automated fix exists (A3/B5/C8) — render a "chỉ báo cáo" tag, never a fix button. */
  reportOnly?: boolean;
  count: number;
  items: DoctorItem[];
  /** true when items were cut at the server LIMIT — count is still the real total. */
  truncated?: boolean;
}

export interface DoctorScanData { findings: DoctorFinding[]; scannedAt: string }

export interface DoctorFixResult {
  checkId: string;
  fixed: number;
  skipped: number;
  failed: number;
  errors: string[];
}

// POST /admin/doctor/scan (no body) → { data: { findings, scannedAt } }.
// A clean check may be ABSENT or present with count:0 — callers must treat both as clean.
export async function doctorScan(): Promise<DoctorScanData> {
  const r = await req<{ data: { findings?: DoctorFinding[] | null; scannedAt?: string } }>(
    "POST", "/admin/doctor/scan",
  );
  // Defensive: Go nil-slices marshal as null (same as adminApp items).
  const findings = (r.data?.findings ?? []).map((f) => ({ ...f, items: f.items ?? [] }));
  return { findings, scannedAt: r.data?.scannedAt ?? "" };
}

// POST /admin/doctor/fix { checkIds, confirm? } → { data: { results } }.
// 422 DOCTOR_CHECK_UNKNOWN (bad id) · 422 DOCTOR_CONFIRM_REQUIRED (needs-confirm check, confirm≠true)
// · 422 DOCTOR_REPORT_ONLY (report-only check named — no automated fix exists).
export async function doctorFix(checkIds: string[], confirm?: boolean): Promise<DoctorFixResult[]> {
  const body: { checkIds: string[]; confirm?: boolean } = { checkIds };
  if (confirm !== undefined) body.confirm = confirm;
  const r = await req<{ data: { results?: DoctorFixResult[] | null } }>("POST", "/admin/doctor/fix", body);
  return (r.data?.results ?? []).map((x) => ({ ...x, errors: x.errors ?? [] }));
}

export interface DoctorFlushData {
  /** Names of the in-proc caches actually flushed (honest subset: meta / acl / workflow / users). */
  flushed: string[];
  /** Static replica caveat: in-proc caches flush THIS instance only; replicas wait out their TTL. */
  note: string;
}

// POST /admin/doctor/flush-caches (no body) → { data: { flushed, note } } (DOCTOR-OPS).
export async function doctorFlushCaches(): Promise<DoctorFlushData> {
  const r = await req<{ data: { flushed?: string[] | null; note?: string } }>(
    "POST", "/admin/doctor/flush-caches",
  );
  return { flushed: r.data?.flushed ?? [], note: r.data?.note ?? "" };
}

export { BASE };

// ── VIEC-HOM-NAY: role-based work queues (../specs/012-work-queues/spec.md) ──────────────────────────────

/** One home-screen queue card: metadata + live caller-scoped count + deep-link. */
export interface WorkQueueView {
  id: string;
  name: string;
  label: string;
  entityName: string;
  actionVerb: "open" | "approve" | "claim";
  /** null = counting failed (stale config) — render "—", never hide the card. */
  count: number | null;
  href: string;
}

/** Admin-side full queue row. */
export interface WorkQueue {
  id: string;
  name: string;
  label: string;
  entityName: string;
  roleId?: string;
  filters: { field: string; op: string; value: string }[];
  sortField?: string;
  sortOrder: string;
  actionVerb: string;
  position: number;
  isActive: boolean;
}

export interface WorkQueueInput {
  name: string;
  label: string;
  entityName: string;
  roleId?: string;
  filters: { field: string; op: string; value: string }[];
  sortField?: string;
  sortOrder?: string;
  actionVerb?: string;
  position?: number;
  isActive?: boolean;
}

/** listWorkQueues — GET /work-queues: the caller's visible queues with live counts. */
export async function listWorkQueues(): Promise<WorkQueueView[]> {
  const r = await req<{ data: { queues: WorkQueueView[] } }>("GET", "/work-queues");
  return r.data?.queues ?? [];
}

/** listWorkQueuesAdmin — GET /admin/work-queues (all queues incl. inactive). */
export async function listWorkQueuesAdmin(): Promise<WorkQueue[]> {
  const r = await req<{ data: { queues: WorkQueue[] } }>("GET", "/admin/work-queues");
  return r.data?.queues ?? [];
}

export async function createWorkQueue(input: WorkQueueInput): Promise<WorkQueue> {
  const r = await req<{ data: { queue: WorkQueue } }>("POST", "/admin/work-queues", input);
  return r.data.queue;
}

export async function updateWorkQueue(id: string, input: WorkQueueInput): Promise<WorkQueue> {
  const r = await req<{ data: { queue: WorkQueue } }>("PUT", `/admin/work-queues/${encodeURIComponent(id)}`, input);
  return r.data.queue;
}

export async function deleteWorkQueue(id: string): Promise<void> {
  await req("DELETE", `/admin/work-queues/${encodeURIComponent(id)}`, undefined);
}

// ── CONG-TIN-HIEU — inbound webhooks (../specs/015-inbound-hooks/spec.md) ─────────────────────────────────

/** One payload→field extraction rule of an inbound hook. */
export interface HookMapping {
  source: string; // dot-path into the JSON payload ("data.amount", "items.0.sku")
  field: string;
}

/** Bank-credit matching step (DONG-TIEN p2): find the ref in the transfer memo, flip the
 *  target record's status; optional amount cross-check. */
export interface HookPaymentMatch {
  targetEntity: string;
  refField: string;   // unique field on the target entity
  refPattern: string; // regex that finds the reference inside the memo
  memoSource: string; // dot-path to the transfer memo in the payload
  statusField: string;
  statusValue: string;
  amountSource?: string;
  amountField?: string;
}

export interface InboundHook {
  id: string;
  name: string;
  entityName: string;
  secret: string; // spec §6: admin-readable, per-hook, rotatable
  mapping: HookMapping[];
  matchField?: string;
  paymentMatch?: HookPaymentMatch;
  isActive: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface InboundHookInput {
  name: string;
  entityName: string;
  mapping: HookMapping[];
  matchField?: string;
  paymentMatch?: HookPaymentMatch;
  isActive?: boolean;
}

export interface HookDelivery {
  id: string;
  hookId: string;
  status: "created" | "updated" | "error";
  recordId?: string;
  error?: string;
  payloadHead?: string;
  /** p2 match outcome — unmatched rows are the hộp chờ. */
  matchStatus?: "matched" | "unmatched";
  matchRecordId?: string;
  createdAt: string;
}

export async function listInboundHooks(): Promise<InboundHook[]> {
  const r = await req<{ data: { hooks: InboundHook[] } }>("GET", "/admin/inbound-hooks");
  return r.data.hooks ?? [];
}

export async function createInboundHook(input: InboundHookInput): Promise<InboundHook> {
  const r = await req<{ data: { hook: InboundHook } }>("POST", "/admin/inbound-hooks", input);
  return r.data.hook;
}

export async function updateInboundHook(id: string, input: InboundHookInput): Promise<InboundHook> {
  const r = await req<{ data: { hook: InboundHook } }>("PUT", `/admin/inbound-hooks/${encodeURIComponent(id)}`, input);
  return r.data.hook;
}

export async function rotateInboundHookSecret(id: string): Promise<InboundHook> {
  const r = await req<{ data: { hook: InboundHook } }>("POST", `/admin/inbound-hooks/${encodeURIComponent(id)}/rotate`);
  return r.data.hook;
}

export async function deleteInboundHook(id: string): Promise<void> {
  await req("DELETE", `/admin/inbound-hooks/${encodeURIComponent(id)}`, undefined);
}

export async function listHookDeliveries(id: string): Promise<HookDelivery[]> {
  const r = await req<{ data: { deliveries: HookDelivery[] } }>(
    "GET", `/admin/inbound-hooks/${encodeURIComponent(id)}/deliveries`,
  );
  return r.data.deliveries ?? [];
}

/** The public receive URL for a hook — what the partner system posts to. */
export function inboundHookUrl(hookName: string): string {
  const tenant = tenantSlug() || ":tenant";
  return `${BASE}/hooks/${tenant}/${hookName}`;
}

// ── DONG-TIEN-KHEP-KIN p1 — VietQR payment (../specs/016-payment-flow/spec.md) ────────────────────────────

/** Per-tenant bank config for VietQR. Empty object = not configured. */
export interface PaymentConfig {
  bankBin?: string; // 6-digit NAPAS BIN
  accountNumber?: string;
  accountName?: string;
}

export interface PaymentQR {
  payload: string; // EMVCo string — render with the `qrcode` lib client-side
  amount: number;
  memo: string;
  bank: PaymentConfig;
}

export async function getTenantPayment(): Promise<PaymentConfig> {
  const r = await req<{ data: { payment: PaymentConfig } }>("GET", "/admin/tenant/payment");
  return r.data.payment ?? {};
}

// "Is payment set up?" for ANY authenticated user — a boolean, no account details.
// Minting a QR has always been user-level, but the button that offers it was gated on the ADMIN
// config read above: every non-admin got 403, so the button never appeared for the salespeople and
// support agents who collect the money, and each record view spent a request finding that out.
export async function getPaymentConfigured(): Promise<boolean> {
  const r = await req<{ data: { configured?: boolean } }>("GET", "/tenant/payment-status");
  return r.data.configured === true;
}

/** Full-replace; send {} to clear. */
export async function updateTenantPayment(p: PaymentConfig): Promise<PaymentConfig> {
  const r = await req<{ data: { payment: PaymentConfig } }>("PUT", "/admin/tenant/payment", p);
  return r.data.payment ?? {};
}

/** Payment QR for one record — ACL-scoped server-side (view access required). */
export async function getPaymentQR(entity: string, id: string, amountField: string, memo: string): Promise<PaymentQR> {
  const q = new URLSearchParams({ amountField, memo });
  const r = await req<{ data: PaymentQR }>(
    "GET", `/${encodeURIComponent(entity)}/${encodeURIComponent(id)}/payment-qr?${q.toString()}`,
  );
  return r.data;
}

// ── AO-THUONG-HIEU p1 — per-tenant white-label branding (../specs/013-tenant-branding/spec.md) ────────────

/** White-label document. All fields optional — absent = default Nexus branding. */
export interface Branding {
  appName?: string;
  logoUrl?: string;
  accentColor?: string; // #RRGGBB
}

/** Public read for the login page / app boot — fail-soft {} for unknown tenants, never throws
 *  on shape (the server always answers 200 with one envelope). */
export async function getPublicBranding(tenant: string): Promise<Branding> {
  const r = await req<{ data: { branding: Branding } }>(
    "GET", `/auth/branding/${encodeURIComponent(tenant)}`,
  );
  return r.data.branding ?? {};
}

/**
 * The branding document plus, for the BUILDER on a delivered workspace, its draft state (specs/063 S7-3):
 * "edited" = the document shown is an unpublished draft, "live" = no draft. Absent for everyone else
 * and while the tenant is not delivered.
 */
export interface BrandingDoc {
  branding: Branding;
  draftState?: "live" | "edited";
}

export async function getTenantBrandingDoc(): Promise<BrandingDoc> {
  const r = await req<{ data: { branding: Branding; draftState?: "live" | "edited" } }>("GET", "/admin/tenant/branding");
  return { branding: r.data.branding ?? {}, draftState: r.data.draftState };
}

export async function getTenantBranding(): Promise<Branding> {
  return (await getTenantBrandingDoc()).branding;
}

/** Full-replace; send {} to reset to default Nexus branding. draftState "edited" ⇒ it went to the draft layer. */
export async function updateTenantBrandingDoc(b: Branding): Promise<BrandingDoc> {
  const r = await req<{ data: { branding: Branding; draftState?: "live" | "edited" } }>("PUT", "/admin/tenant/branding", b);
  return { branding: r.data.branding ?? {}, draftState: r.data.draftState };
}

export async function updateTenantBranding(b: Branding): Promise<Branding> {
  return (await updateTenantBrandingDoc(b)).branding;
}
