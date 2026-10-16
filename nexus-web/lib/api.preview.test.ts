// lib/api.ts while a tab is previewing a role — specs/064 T042 (the hot spot of S9-2).
// The contract that matters: while previewing, NO request carries the BUILDER's token (the preview-
// skinned app would otherwise run as the admin, bypassing ACL — the whole point of the feature),
// and an ended/expired preview never silently falls back to the builder or to a refresh.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Call = { url: string; init?: RequestInit };

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function mockFetch(handlers: Array<(c: Call) => Response>): Call[] {
  const calls: Call[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: String(url), init };
    calls.push(call);
    const h = handlers.shift();
    if (!h) throw new Error(`unexpected fetch #${calls.length}: ${call.url}`);
    return h(call);
  }) as unknown as typeof fetch;
  return calls;
}

const auth = (c: Call) => (c.init?.headers as Record<string, string> | undefined)?.Authorization;

const PREVIEW = {
  token: "PREVIEW-TOK",
  sessionId: "s-1",
  role: { id: "r-1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" },
  expiresAt: "2099-01-01T00:00:00Z",
  returnTo: "/studio/apps/crm",
};

async function fresh() {
  vi.resetModules();
  const api = await import("@/lib/api");
  const ps = await import("@/lib/previewSession");
  return { api, ps };
}

describe("lib/api while previewing a role", () => {
  const originalFetch = global.fetch;
  const originalLocation = window.location;
  let assign: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    // The builder's session: real tokens in localStorage (remember-me) + a cached profile.
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "BUILDER-TOK");
    window.localStorage.setItem("nexus_refresh", "BUILDER-REFRESH");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true }));
    assign = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, assign, pathname: "/home", href: "http://localhost/home" },
    });
  });

  afterEach(() => {
    global.fetch = originalFetch;
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("getToken prefers the preview token over the builder's; getBuilderToken still answers the builder's", async () => {
    const { api, ps } = await fresh();
    expect(api.getToken()).toBe("BUILDER-TOK"); // no preview → unchanged
    ps.setPreview(PREVIEW);
    expect(api.getToken()).toBe("PREVIEW-TOK");
    expect(api.getBuilderToken()).toBe("BUILDER-TOK");
  });

  it("while the exit latch is set getToken answers null (nothing may carry the builder's token)", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    ps.beginPreviewExit();
    expect(api.getToken()).toBeNull();
    ps.clearPreview(); // preview gone, latch still up → still null, NOT the builder's token
    expect(api.getToken()).toBeNull();
    expect(api.isAuthed()).toBe(false);
  });

  it("a data request carries the preview token", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const calls = mockFetch([() => jsonRes({ data: [] }, 200)]);
    await api.getMyPermissions();
    expect(auth(calls[0])).toBe("Bearer PREVIEW-TOK");
  });

  it("401 while previewing: ONE retry with the SAME preview token, then exit — 0 /auth/refresh, never the builder's token", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const calls = mockFetch([
      () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
    ]);
    await expect(api.getMyPermissions()).rejects.toMatchObject({ status: 401, code: "PREVIEW_ENDED" });
    expect(calls).toHaveLength(2);
    expect(calls.every((c) => auth(c) === "Bearer PREVIEW-TOK")).toBe(true);
    expect(calls.some((c) => c.url.includes("/auth/refresh") || c.url.includes("/auth/login"))).toBe(false);
    expect(ps.getPreview()).toBeNull();
    expect(assign).toHaveBeenCalledTimes(1);
    expect(assign).toHaveBeenCalledWith("/studio/apps/crm");
    expect(ps.takePreviewNotice()).toBe("ended");
    expect(ps.isPreviewExiting()).toBe(true);
    expect(api.getToken()).toBeNull();
  });

  it("the retry window recovers (the builder's refresh-rotation blip): 401 then 200 keeps the preview", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const calls = mockFetch([
      () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      () => jsonRes({ data: [{ entityName: "x", action: "view", accessLevel: "all" }] }, 200),
    ]);
    await expect(api.getMyPermissions()).resolves.toHaveLength(1);
    expect(calls.map(auth)).toEqual(["Bearer PREVIEW-TOK", "Bearer PREVIEW-TOK"]);
    expect(ps.getPreview()).not.toBeNull();
    expect(assign).not.toHaveBeenCalled();
  });

  it("a 401 on a request that goes out after the exit began is NOT retried and does not refresh", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    ps.beginPreviewExit();
    const calls = mockFetch([() => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401)]);
    await expect(api.getMyPermissions()).rejects.toMatchObject({ code: "PREVIEW_ENDED" });
    expect(calls).toHaveLength(1);
    expect(auth(calls[0])).toBeUndefined(); // no Authorization at all — harmless
  });

  it("never auto-logs-in as the seed admin while leaving preview (DEMO_MODE bootstrap is skipped)", async () => {
    vi.stubEnv("NEXT_PUBLIC_DEMO_MODE", "true");
    vi.stubEnv("NEXT_PUBLIC_DEMO_TENANT", "northwind");
    vi.stubEnv("NEXT_PUBLIC_DEMO_EMAIL", "admin@example.com");
    vi.stubEnv("NEXT_PUBLIC_DEMO_PASSWORD", "pw");
    const { api, ps } = await fresh();
    window.localStorage.clear();
    ps.beginPreviewExit();
    const calls = mockFetch([]);
    await expect(api.ensureAuth()).rejects.toMatchObject({ code: "PREVIEW_ENDED" });
    expect(calls).toHaveLength(0);
    vi.unstubAllEnvs();
  });

  it("bulk create (the second 401 recovery site) follows the same rule", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const calls = mockFetch([
      () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
      () => jsonRes({ error: "x", code: "UNAUTHORIZED" }, 401),
    ]);
    await expect(api.createSampleRecords("crm_accounts", [{ name: "a" }])).rejects.toMatchObject({ code: "PREVIEW_ENDED" });
    expect(calls).toHaveLength(2);
    expect(calls.every((c) => auth(c) === "Bearer PREVIEW-TOK")).toBe(true);
    expect(calls.some((c) => c.url.includes("/auth/refresh"))).toBe(false);
    expect(assign).toHaveBeenCalledTimes(1);
  });

  it("getMe while previewing does NOT write the shared nexus_user; the profile goes to the tab's preview state", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const me = { id: "principal", fullName: "Xem như: Nhân viên kinh doanh", email: "", isAdmin: false, preview: { sessionId: "s-1" } };
    mockFetch([() => jsonRes({ data: me }, 200)]);
    await api.getMe();
    expect(JSON.parse(window.localStorage.getItem("nexus_user")!).id).toBe("u-admin"); // untouched
    expect(ps.getPreview()?.me?.id).toBe("principal");
  });

  it("getCachedMe while previewing is the preview profile — never the builder's cached nexus_user", async () => {
    const { api, ps } = await fresh();
    expect(api.getCachedMe()?.id).toBe("u-admin"); // no preview → unchanged
    ps.setPreview(PREVIEW);
    expect(api.getCachedMe()).toBeNull(); // preview but /me not back yet → null, not the admin
    ps.setPreviewMe({ id: "principal", fullName: "Xem như", email: "", isAdmin: false });
    expect(api.getCachedMe()?.id).toBe("principal");
  });

  it("startPreviewSession sends the BUILDER's token and returns the session", async () => {
    const { api } = await fresh();
    const calls = mockFetch([() => jsonRes({ data: { sessionId: "s-9", token: "T", expiresAt: "2099-01-01T00:00:00Z", role: { id: "r-1", name: "x" }, appId: null, draft: false } }, 201)]);
    const r = await api.startPreviewSession("r-1", "app-1");
    expect(calls[0].url).toContain("/admin/preview-sessions");
    expect(calls[0].init?.method).toBe("POST");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ roleId: "r-1", appId: "app-1" });
    expect(auth(calls[0])).toBe("Bearer BUILDER-TOK");
    expect(r.token).toBe("T");
  });

  // specs/064 S9-9: draft is sent only when the caller decided (true OR false); absent = server default.
  it.each([
    [undefined, { roleId: "r-1" }],
    [true, { roleId: "r-1", draft: true }],
    [false, { roleId: "r-1", draft: false }],
  ])("startPreviewSession(draft=%s) body", async (draft, body) => {
    const { api } = await fresh();
    const calls = mockFetch([() => jsonRes({ data: { sessionId: "s-9", token: "T", expiresAt: "2099-01-01T00:00:00Z", role: { id: "r-1", name: "x" }, appId: null, draft: Boolean(draft) } }, 201)]);
    await api.startPreviewSession("r-1", undefined, draft);
    expect(JSON.parse(String(calls[0].init?.body))).toEqual(body);
  });

  it("endPreviewSession: DELETE with the preview token, clears the preview even when the API fails, returns returnTo", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const calls = mockFetch([() => { throw new Error("network down"); }]);
    const to = await api.endPreviewSession("exit");
    expect(calls[0].url).toContain("/preview-sessions/current?reason=exit");
    expect(calls[0].init?.method).toBe("DELETE");
    expect(auth(calls[0])).toBe("Bearer PREVIEW-TOK");
    expect(to).toBe("/studio/apps/crm");
    expect(ps.getPreview()).toBeNull();
    expect(ps.isPreviewExiting()).toBe(true);
    expect(api.getToken()).toBeNull();
  });

  it("a write refused with 403 PREVIEW_READ_ONLY raises the event the banner turns into a Toast", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const seen = vi.fn();
    window.addEventListener("nexus:preview-read-only", seen);
    mockFetch([() => jsonRes({ error: "x", code: "PREVIEW_READ_ONLY", details: { method: "POST" } }, 403)]);
    await expect(api.apiRequest("POST", "/crm_accounts", {})).rejects.toMatchObject({ status: 403, code: "PREVIEW_READ_ONLY" });
    expect(seen).toHaveBeenCalledTimes(1);
    window.removeEventListener("nexus:preview-read-only", seen);
  });

  it("logout() inside a preview tab ends the PREVIEW — it must not wipe the builder's real tokens shared by every tab", async () => {
    const { api, ps } = await fresh();
    ps.setPreview(PREVIEW);
    const calls = mockFetch([() => new Response(null, { status: 204 })]);
    await api.logout();
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toContain("/preview-sessions/current?reason=exit");
    expect(calls.some((c) => c.url.includes("/auth/logout"))).toBe(false);
    expect(window.localStorage.getItem("nexus_token")).toBe("BUILDER-TOK");
    expect(window.localStorage.getItem("nexus_refresh")).toBe("BUILDER-REFRESH");
    expect(ps.getPreview()).toBeNull();
    expect(assign).toHaveBeenCalledWith("/studio/apps/crm");
  });

  it("a tampered returnTo never becomes an off-site navigation (only same-origin absolute paths)", async () => {
    const { api, ps } = await fresh();
    for (const bad of ["https://evil.example/x", "//evil.example/x", "/\\evil.example", "javascript:alert(1)", ""]) {
      ps.setPreview({ ...PREVIEW, returnTo: bad });
      mockFetch([() => new Response(null, { status: 204 })]);
      expect(await api.endPreviewSession("exit")).toBe("/studio");
      ps.endPreviewExit();
    }
    ps.setPreview({ ...PREVIEW, returnTo: "/studio/apps/crm?panel=x" });
    mockFetch([() => new Response(null, { status: 204 })]);
    expect(await api.endPreviewSession("exit")).toBe("/studio/apps/crm?panel=x");
  });

  it("endPreviewSession with no preview is a quiet no-op", async () => {
    const { api } = await fresh();
    const calls = mockFetch([]);
    expect(await api.endPreviewSession("left")).toBe("/studio");
    expect(calls).toHaveLength(0);
  });
});
