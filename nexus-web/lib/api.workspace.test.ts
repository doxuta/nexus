// lib/api.ts while the tab builds inside a customer workspace — specs/062-customer-workspace L2 (T032).
// The contract that matters: while in a workspace NO data request ever carries the HOME token (the
// banner says "Đang dựng cho <khách>"; a request that silently ran against the builder's own studio is
// a data incident), a dead session ends loudly instead of falling back, and a different sign-in in the
// same tab never inherits the previous builder's workspace token (D-21).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Call = { method: string; url: string; auth: string | undefined; body: Record<string, unknown> | null };

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}
const noContent = () => new Response(null, { status: 204 });

const SESSION = {
  id: "ws-1", tenantId: "t-x", tenantSlug: "x", tenantName: "Khách X",
  openedAt: "2098-12-31T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z",
};
const WS = {
  accessToken: "WS-1",
  expiresAt: "2099-01-01T00:00:00Z",
  homeUserId: "u-admin",
  session: SESSION,
};
const opened = (token: string) => ({ data: { accessToken: token, expiresAt: "2099-01-01T00:00:00Z", session: { ...SESSION, resumed: true } } });

type Handler = (c: Call) => Response | Promise<Response>;

/** Records every call; `route` answers by "METHOD path" (path = after /api/v1). */
function server(route: Record<string, Handler | Handler[]>): Call[] {
  const calls: Call[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const path = u.replace(/^https?:\/\/[^/]+\/api\/v1/, "").split("?")[0];
    const call: Call = {
      method: init?.method ?? "GET",
      url: u,
      auth: (init?.headers as Record<string, string> | undefined)?.Authorization,
      body: init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : null,
    };
    calls.push(call);
    const h = route[`${call.method} ${path}`];
    const fn = Array.isArray(h) ? h.shift() : h;
    if (!fn) throw new Error(`unexpected fetch: ${call.method} ${path}`);
    return fn(call);
  }) as unknown as typeof fetch;
  return calls;
}

async function fresh(env?: Record<string, string>) {
  vi.resetModules();
  vi.unstubAllEnvs();
  for (const [k, v] of Object.entries(env ?? {})) vi.stubEnv(k, v);
  const api = await import("@/lib/api");
  const ws = await import("@/lib/workspace");
  return { api, ws };
}

/** Data requests = everything that is not the session/refresh plumbing. */
const dataCalls = (calls: Call[]) => calls.filter((c) => !c.url.includes("/auth/") && !c.url.includes("/workspace-sessions"));

describe("lib/api in a workspace", () => {
  const originalFetch = global.fetch;
  const originalLocation = window.location;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "HOME-TOK");
    window.localStorage.setItem("nexus_refresh", "HOME-REFRESH");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true }));
    assign = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, assign, pathname: "/studio", href: "http://localhost/studio" },
    });
  });

  afterEach(() => {
    global.fetch = originalFetch;
    vi.unstubAllEnvs();
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  function enter(ws: { setWorkspace: (w: typeof WS) => void }) {
    ws.setWorkspace(WS);
  }

  it("getToken answers the session token; getHomeToken the home one; no workspace → the home token as before", async () => {
    const { api, ws } = await fresh();
    expect(api.getToken()).toBe("HOME-TOK");
    enter(ws);
    expect(api.getToken()).toBe("WS-1");
    expect(api.getBuilderToken()).toBe("WS-1");
    expect(api.getHomeToken()).toBe("HOME-TOK");
    expect(api.tenantSlug()).toBe("x");
  });

  it("a data request carries the session token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({ "GET /metadata/permissions/me": () => jsonRes({ data: [] }, 200) });
    await api.getMyPermissions();
    expect(calls[0].auth).toBe("Bearer WS-1");
  });

  // specs/064 S9-8 (T122): "xem như vai" opened inside a customer's workspace is minted with the SESSION token
  // (the one the server turns into a builderGated caller of the customer tenant), never the home token; while the
  // preview runs the data requests carry the preview token and the builder token is still the session one.
  it("startPreviewSession in a workspace carries the session token; the preview then wins getToken, getBuilderToken stays the session's", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "POST /admin/preview-sessions": () =>
        jsonRes({ data: { sessionId: "s-1", token: "PV-1", expiresAt: "2099-01-01T00:00:00Z", role: { id: "r-1", name: "rep" }, appId: null, draft: false } }, 201),
    });
    const r = await api.startPreviewSession("r-1");
    expect(calls[0].auth).toBe("Bearer WS-1");
    expect(r.token).toBe("PV-1");
    const ps = await import("@/lib/previewSession");
    ps.setPreview({ token: "PV-1", sessionId: "s-1", role: { id: "r-1", name: "rep" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio" });
    expect(api.getToken()).toBe("PV-1");
    expect(api.getBuilderToken()).toBe("WS-1");
  });

  // specs/077 T151: a tab whose workspace is void (another home user signed in) would fall back to the HOME token;
  // "xem như" must then refuse instead of minting a preview with it.
  it("startPreviewSession in a void workspace refuses and never sends the home token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-other" }));
    const calls = server({});
    const ended = vi.fn();
    window.addEventListener("nexus:workspace-ended", ended);
    await expect(api.startPreviewSession("r-1", "app-1")).rejects.toMatchObject({ code: "WORKSPACE_ENDED" });
    window.removeEventListener("nexus:workspace-ended", ended);
    expect(calls).toHaveLength(0);
    // refusing is not ending: the banner/leave flow of nexus:workspace-ended must not fire from "xem như" (#790)
    expect(ended).not.toHaveBeenCalled();
  });

  it("401 → ONE resume shared by 3 concurrent requests, then each retries with the NEW session token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "GET /metadata/permissions/me": (c) => (c.auth === "Bearer WS-2" ? jsonRes({ data: [] }, 200) : jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401)),
      "POST /admin/workspace-sessions": async () => {
        await new Promise((r) => setTimeout(r, 5));
        return jsonRes(opened("WS-2"), 200);
      },
    });
    await Promise.all([api.getMyPermissions(), api.getMyPermissions(), api.getMyPermissions()]);
    const resumes = calls.filter((c) => c.url.endsWith("/admin/workspace-sessions"));
    expect(resumes).toHaveLength(1);
    expect(resumes[0].auth).toBe("Bearer HOME-TOK");
    expect(resumes[0].body).toEqual({ tenantSlug: "x", resumeSessionId: "ws-1" });
    expect(api.getToken()).toBe("WS-2");
    // 3 first tries on WS-1, 3 retries on WS-2 — none on the home token.
    expect(dataCalls(calls).map((c) => c.auth).sort()).toEqual(Array(3).fill("Bearer WS-1").concat(Array(3).fill("Bearer WS-2")));
  });

  it("home token 401 on the resume → silent refresh of the HOME chain, then resume once more", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "GET /metadata/permissions/me": (c) => (c.auth === "Bearer WS-2" ? jsonRes({ data: [] }, 200) : jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401)),
      "POST /admin/workspace-sessions": [
        () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
        (c) => (c.auth === "Bearer HOME-NEW" ? jsonRes(opened("WS-2"), 200) : jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401)),
      ],
      "POST /auth/refresh": () => jsonRes({ data: { accessToken: "HOME-NEW", refreshToken: "HOME-REFRESH-2" } }, 200),
    });
    await api.getMyPermissions();
    expect(calls.filter((c) => c.url.endsWith("/auth/refresh"))).toHaveLength(1);
    expect(window.localStorage.getItem("nexus_token")).toBe("HOME-NEW"); // the refresh wrote the HOME slot
    expect(api.getToken()).toBe("WS-2");
    expect(ws.getWorkspace()?.accessToken).toBe("WS-2");
  });

  it("resume 404 WORKSPACE_SESSION_ENDED → ended event, WORKSPACE_ENDED, and 0 data requests carry the home token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const ended = vi.fn();
    window.addEventListener(ws.WORKSPACE_ENDED_EVENT, ended);
    const calls = server({
      "GET /metadata/permissions/me": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /admin/workspace-sessions": () => jsonRes({ error: "Phiên dựng đã kết thúc", code: "WORKSPACE_SESSION_ENDED" }, 404),
    });
    await expect(api.getMyPermissions()).rejects.toMatchObject({ status: 401, code: "WORKSPACE_ENDED" });
    window.removeEventListener(ws.WORKSPACE_ENDED_EVENT, ended);
    expect(ended).toHaveBeenCalledTimes(1);
    expect(ws.inWorkspace()).toBe(false);
    expect(dataCalls(calls).every((c) => c.auth === "Bearer WS-1")).toBe(true);
    expect(dataCalls(calls)).toHaveLength(1); // no retry on any token
  });

  // ── FR-014: the window AFTER the workspace ended, until the page is replaced ──

  it("a request that 401s AFTER the session ended is not refreshed or resent with the home token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    let release: () => void = () => {};
    const late = new Promise<void>((r) => { release = r; });
    const calls = server({
      "GET /metadata/permissions/me": [
        async () => { await late; return jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401); }, // B, sent earlier with WS-1
      ],
      "GET /metadata/entities": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401), // A
      "POST /admin/workspace-sessions": () => jsonRes({ error: "x", code: "WORKSPACE_SESSION_ENDED" }, 404),
      "POST /auth/refresh": () => jsonRes({ data: { accessToken: "HOME-NEW", refreshToken: "R2" } }, 200),
    });
    const b = api.getMyPermissions();
    const bSettled = b.catch((e) => e);
    await expect(api.listEntities()).rejects.toMatchObject({ code: "WORKSPACE_ENDED" }); // A ends the workspace
    expect(ws.inWorkspace()).toBe(false);
    release(); // B's 401 only lands now: state already cleared
    await expect(bSettled).resolves.toMatchObject({ code: "WORKSPACE_ENDED" });
    expect(calls.some((c) => c.url.includes("/auth/refresh"))).toBe(false);
    expect(dataCalls(calls).every((c) => c.auth === "Bearer WS-1")).toBe(true);
  });

  it("a NEW request after the session ended never reaches the network (and so never carries the home token)", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "GET /metadata/permissions/me": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /admin/workspace-sessions": () => jsonRes({ error: "x", code: "WORKSPACE_SESSION_ENDED" }, 404),
      "GET /metadata/entities": () => jsonRes({ data: [] }, 200),
    });
    await expect(api.getMyPermissions()).rejects.toMatchObject({ code: "WORKSPACE_ENDED" });
    expect(api.getToken()).toBeNull();
    expect(api.getBuilderToken()).toBeNull();
    const before = calls.length;
    await expect(api.listEntities()).rejects.toMatchObject({ code: "WORKSPACE_ENDED" });
    expect(calls).toHaveLength(before); // refused before fetch: specs/073 FR-085
  });

  it("the guard does not outlive a fresh sign-in: login() lifts the latch and requests flow again", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    server({
      "DELETE /admin/workspace-sessions/current": noContent,
      "POST /auth/login": () => jsonRes({ data: { accessToken: "NEW-TOK", refreshToken: "NEW-R", user: { id: "u-2", fullName: "B", email: "b@x", isAdmin: false } } }, 200),
      "GET /metadata/entities": () => jsonRes({ data: [] }, 200),
    });
    await api.exitWorkspace();
    await expect(api.listEntities()).rejects.toMatchObject({ code: "WORKSPACE_ENDED" });
    await api.login("northwind", "b@x", "pw");
    await expect(api.listEntities()).resolves.toBeDefined();
  });

  it("exitWorkspace: from the moment it starts nothing carries the home token, yet the DELETE still goes out with the session token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "DELETE /admin/workspace-sessions/current": noContent,
      "GET /metadata/entities": () => jsonRes({ data: [] }, 200),
    });
    const exiting = api.exitWorkspace();
    expect(api.getToken()).toBeNull();
    // A request racing the exit: refused before the network (specs/073 FR-085 — not even a token-less call goes out,
    // it could only earn a 401 and a page that is about to be replaced has no use for the answer).
    await expect(api.listEntities()).rejects.toMatchObject({ code: "WORKSPACE_ENDED" });
    await exiting;
    expect(calls.find((c) => c.method === "DELETE")?.auth).toBe("Bearer WS-1");
    expect(dataCalls(calls)).toEqual([]);
  });

  it("the guard covers /auth/* too: GET /auth/me during the exit never reaches the network (FR-084(7))", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({ "DELETE /admin/workspace-sessions/current": noContent, "GET /auth/me": () => jsonRes({ data: {} }, 200) });
    const exiting = api.exitWorkspace();
    await expect(api.getMe()).rejects.toMatchObject({ code: "WORKSPACE_ENDED" });
    await exiting;
    expect(calls.some((c) => c.url.includes("/auth/me"))).toBe(false);
  });

  it("a fresh login() after leaving lifts the latch (SPA sign-in in the same page keeps working)", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    server({
      "DELETE /admin/workspace-sessions/current": noContent,
      "POST /auth/logout": () => jsonRes({ data: { ok: true } }, 200),
      "POST /auth/login": () => jsonRes({ data: { accessToken: "NEW-TOK", refreshToken: "NEW-R", user: { id: "u-2", fullName: "B", email: "b@x", isAdmin: false } } }, 200),
    });
    await api.logout();
    expect(api.getToken()).toBeNull();
    await api.login("northwind", "b@x", "pw");
    expect(api.getToken()).toBe("NEW-TOK");
  });

  it("DEMO_MODE on: a dead session still ends — it never auto-logs-in as the seed admin", async () => {
    const { api, ws } = await fresh({
      NEXT_PUBLIC_DEMO_MODE: "true", NEXT_PUBLIC_DEMO_TENANT: "northwind", NEXT_PUBLIC_DEMO_EMAIL: "a@b.c", NEXT_PUBLIC_DEMO_PASSWORD: "pw",
    });
    enter(ws);
    const calls = server({
      "GET /metadata/permissions/me": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /admin/workspace-sessions": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /auth/refresh": () => jsonRes({ error: "x", code: "INVALID_REFRESH" }, 401),
    });
    await expect(api.getMyPermissions()).rejects.toBeTruthy();
    expect(calls.some((c) => c.url.includes("/auth/login"))).toBe(false);
    expect(dataCalls(calls)).toHaveLength(1);
    expect(ws.inWorkspace()).toBe(false);
  });

  it("a transient network error on the resume does NOT end the session and does NOT fall back to the home token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "GET /metadata/permissions/me": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /admin/workspace-sessions": () => { throw new TypeError("Failed to fetch"); },
    });
    await expect(api.getMyPermissions()).rejects.toBeInstanceOf(TypeError);
    expect(ws.inWorkspace()).toBe(true);
    expect(dataCalls(calls).every((c) => c.auth === "Bearer WS-1")).toBe(true);
  });

  it("getMe in a workspace stores the profile in the tab's state, never in the shared nexus_user", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const me = { id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true, tenantId: "t-x", workspace: { sessionId: "ws-1", tenantId: "t-x", tenantSlug: "x", tenantName: "Khách X", expiresAt: SESSION.expiresAt } };
    server({ "GET /auth/me": () => jsonRes({ data: me }, 200) });
    await api.getMe();
    expect(JSON.parse(window.localStorage.getItem("nexus_user")!).tenantId).toBeUndefined();
    expect(ws.getWorkspace()?.me?.tenantId).toBe("t-x");
    expect(api.getCachedMe()?.tenantId).toBe("t-x");
  });

  it("enterWorkspace opens with the HOME token, remembers the builder, and loads the studio fresh", async () => {
    const { api, ws } = await fresh();
    const calls = server({
      "POST /admin/workspace-sessions": () => jsonRes({ data: { accessToken: "WS-1", expiresAt: WS.expiresAt, session: { ...SESSION, resumed: false } } }, 201),
    });
    await api.enterWorkspace("x");
    expect(calls[0].auth).toBe("Bearer HOME-TOK");
    expect(calls[0].body).toEqual({ tenantSlug: "x" });
    expect(ws.getWorkspace()).toMatchObject({ accessToken: "WS-1", homeUserId: "u-admin", session: { tenantSlug: "x" } });
    expect(assign).toHaveBeenCalledWith("/studio/du-an/x");
    expect(window.localStorage.getItem(ws.WORKSPACE_STORAGE_KEY)).toBeNull();
  });

  it("opening a session remembers the project for the home user, as code and name only (specs/073 FR-083)", async () => {
    const { api } = await fresh();
    server({
      "POST /admin/workspace-sessions": () => jsonRes({ data: { accessToken: "WS-1", expiresAt: WS.expiresAt, session: { ...SESSION, resumed: false } } }, 201),
    });
    await api.openWorkspace("x");
    const raw = window.localStorage.getItem("nexus.studio.recentProjects:u-admin") ?? "";
    expect(JSON.parse(raw)).toEqual([{ slug: "x", name: "Khách X" }]);
    expect(raw).not.toMatch(/WS-1|t-x/); // no token, no tenant id
  });

  it("a failed open remembers nothing", async () => {
    const { api } = await fresh();
    server({ "POST /admin/workspace-sessions": () => jsonRes({ error: "x", code: "TENANT_NOT_FOUND" }, 404) });
    await expect(api.openWorkspace("zzz")).rejects.toMatchObject({ status: 404 });
    expect(window.localStorage.getItem("nexus.studio.recentProjects:u-admin")).toBeNull();
  });

  it("enterWorkspace takes next through safeNext for that project: a good one stays, anything outside becomes the first page", async () => {
    const { api } = await fresh();
    server({ "POST /admin/workspace-sessions": () => jsonRes({ data: { accessToken: "WS-1", expiresAt: WS.expiresAt, session: { ...SESSION, resumed: false } } }, 201) });
    await api.enterWorkspace("x", { next: "/studio/du-an/x/dung/tu-dong" });
    expect(assign).toHaveBeenLastCalledWith("/studio/du-an/x/dung/tu-dong");
    for (const bad of ["//evil.com", "https://evil.com", "/studio/du-an/other/dung/du-lieu", "/login"]) {
      await api.enterWorkspace("x", { next: bad });
      expect(assign).toHaveBeenLastCalledWith("/studio/du-an/x/dung/du-lieu");
    }
  });

  it("enterWorkspace removes a view-as-role record: it belongs to the session it was opened in (FR-086)", async () => {
    const { api } = await fresh();
    const prev = await import("@/lib/previewSession");
    prev.setPreview({ token: "PREV", sessionId: "p1", role: { id: "r", name: "vai" }, expiresAt: WS.expiresAt, returnTo: "/studio" });
    server({ "POST /admin/workspace-sessions": () => jsonRes({ data: { accessToken: "WS-1", expiresAt: WS.expiresAt, session: { ...SESSION, resumed: false } } }, 201) });
    await api.enterWorkspace("x");
    expect(prev.getPreview()).toBeNull();
  });

  it("exitWorkspace takes next as a same-origin path only", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    server({ "DELETE /admin/workspace-sessions/current": noContent });
    await api.exitWorkspace({ next: "/studio/du-an/x/van-hanh/sao-luu" });
    expect(assign).toHaveBeenLastCalledWith("/studio/du-an/x/van-hanh/sao-luu");
    enter(ws);
    await api.exitWorkspace({ next: "//evil.com" });
    expect(assign).toHaveBeenLastCalledWith("/studio");
  });

  it("enterWorkspace failure leaves no half state and surfaces the server code", async () => {
    const { api, ws } = await fresh();
    server({ "POST /admin/workspace-sessions": () => jsonRes({ error: "Không tìm thấy workspace", code: "TENANT_NOT_FOUND" }, 404) });
    await expect(api.enterWorkspace("zzz")).rejects.toMatchObject({ status: 404, code: "TENANT_NOT_FOUND" });
    expect(ws.inWorkspace()).toBe(false);
    expect(assign).not.toHaveBeenCalled();
  });

  it("exitWorkspace closes with the session token, clears, and goes back to the library", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({ "DELETE /admin/workspace-sessions/current": noContent });
    await api.exitWorkspace();
    expect(calls).toHaveLength(1);
    expect(calls[0].auth).toBe("Bearer WS-1");
    expect(ws.inWorkspace()).toBe(false);
    expect(assign).toHaveBeenCalledWith("/studio");
  });

  it("closeWorkspace getting 401 whose session is really gone: one resume says 404, no ended event, exit carries on", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const ended = vi.fn();
    window.addEventListener(ws.WORKSPACE_ENDED_EVENT, ended);
    const calls = server({
      "DELETE /admin/workspace-sessions/current": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /admin/workspace-sessions": () => jsonRes({ error: "x", code: "WORKSPACE_SESSION_ENDED" }, 404),
    });
    await api.exitWorkspace();
    window.removeEventListener(ws.WORKSPACE_ENDED_EVENT, ended);
    expect(calls.map((c) => c.method)).toEqual(["DELETE", "POST"]); // no /auth/refresh, no second DELETE
    expect(ended).not.toHaveBeenCalled();
    expect(ws.inWorkspace()).toBe(false);
    expect(assign).toHaveBeenCalledWith("/studio");
  });

  it("closeWorkspace with a lapsed session token resumes the SAME session once and closes it with the fresh token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "DELETE /admin/workspace-sessions/current": [
        () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
        () => noContent(),
      ],
      "POST /admin/workspace-sessions": () => jsonRes(opened("WS-2"), 200),
    });
    await api.exitWorkspace();
    expect(calls.map((c) => `${c.method} ${c.auth}`)).toEqual(["DELETE Bearer WS-1", "POST Bearer HOME-TOK", "DELETE Bearer WS-2"]);
    expect(calls[1].body).toEqual({ tenantSlug: "x", resumeSessionId: "ws-1" });
    expect(assign).toHaveBeenCalledWith("/studio");
  });

  it("logoutEverywhere in a workspace leaves it first and asks /auth/logout-all as the HOME login", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "DELETE /admin/workspace-sessions/current": noContent,
      "POST /auth/logout-all": () => jsonRes({ data: { ok: true } }, 200),
      "POST /auth/logout": () => jsonRes({ data: { ok: true } }, 200),
    });
    await api.logoutEverywhere();
    expect(calls.map((c) => `${c.method} ${c.url.replace(/^.*\/api\/v1/, "")} ${c.auth}`)).toEqual([
      "DELETE /admin/workspace-sessions/current Bearer WS-1",
      "POST /auth/logout-all Bearer HOME-TOK",
      "POST /auth/logout Bearer HOME-TOK",
    ]);
  });

  it("enterWorkspace resolves the home user BEFORE opening a server session (no orphan row on failure)", async () => {
    window.localStorage.removeItem("nexus_user");
    const { api } = await fresh();
    const calls = server({ "GET /auth/me": () => jsonRes({ data: {} }, 200) });
    await expect(api.enterWorkspace("x")).rejects.toMatchObject({ status: 401 });
    expect(calls.some((c) => c.url.includes("/workspace-sessions"))).toBe(false);
  });

  it("logout in a workspace closes the session FIRST, then signs the home login out", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "DELETE /admin/workspace-sessions/current": noContent,
      "POST /auth/logout": () => jsonRes({ data: { ok: true } }, 200),
    });
    await api.logout();
    expect(calls.map((c) => `${c.method} ${c.url.replace(/^.*\/api\/v1/, "")}`)).toEqual([
      "DELETE /admin/workspace-sessions/current",
      "POST /auth/logout",
    ]);
    expect(calls[0].auth).toBe("Bearer WS-1");
    expect(calls[1].auth).toBe("Bearer HOME-TOK");
    expect(ws.inWorkspace()).toBe(false);
  });

  // ── D-21 / FR-014a: the workspace belongs to the home login that opened it ──

  it("login() as someone else wipes the workspace first: the next request carries the NEW home token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    const calls = server({
      "POST /auth/login": () => jsonRes({ data: { accessToken: "OTHER-TOK", refreshToken: "OTHER-R", user: { id: "u-other", email: "o@x.vn", fullName: "O", isAdmin: false } } }, 200),
      "GET /metadata/permissions/me": () => jsonRes({ data: [] }, 200),
    });
    await api.login("northwind", "o@x.vn", "pw", true);
    expect(ws.inWorkspace()).toBe(false);
    await api.getMyPermissions();
    expect(calls.find((c) => c.url.includes("/auth/login"))!.auth).not.toBe("Bearer WS-1");
    expect(dataCalls(calls)[0].auth).toBe("Bearer OTHER-TOK");
  });

  it("verifyMfa() wipes the workspace too", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    server({
      "POST /auth/mfa/verify": () => jsonRes({ data: { accessToken: "OTHER-TOK", refreshToken: "OTHER-R", user: { id: "u-other" } } }, 200),
    });
    await api.verifyMfa("pending", "123456");
    expect(ws.inWorkspace()).toBe(false);
    expect(api.getToken()).toBe("OTHER-TOK");
  });

  it("silentRefresh failing (even a network error) wipes the workspace BEFORE session-expired is announced", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    let inWsAtExpiry: boolean | null = null;
    window.addEventListener("nexus:session-expired", () => { inWsAtExpiry = ws.inWorkspace(); }, { once: true });
    server({
      "GET /metadata/permissions/me": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /admin/workspace-sessions": () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      "POST /auth/refresh": () => { throw new TypeError("Failed to fetch"); },
    });
    await expect(api.getMyPermissions()).rejects.toBeTruthy();
    expect(inWsAtExpiry).toBe(false);
    expect(ws.inWorkspace()).toBe(false);
  });

  it("the signed-in home user is not the builder who opened it → getToken drops the workspace and answers the home token", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-other", email: "o@x.vn" }));
    expect(api.getToken()).toBe("HOME-TOK");
    expect(ws.inWorkspace()).toBe(false);
  });

  it("no home token left at all → the workspace is dropped (nothing to resume it with)", async () => {
    const { api, ws } = await fresh();
    enter(ws);
    window.localStorage.removeItem("nexus_token");
    expect(api.getToken()).toBeNull();
    expect(ws.inWorkspace()).toBe(false);
  });
});
