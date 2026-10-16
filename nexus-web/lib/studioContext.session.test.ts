// specs/073 ph1a T421/T423/T433 (FR-085, FR-086, FR-021): enter / exit / switch a build session from the Studio seam,
// on the real lib/api + lib/workspace with only the network mocked. The order is the point: the question is never
// "did it end up in the right place" but "what could the tab send on the way".
import { QueryClient } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { endPreviewExit, getPreview, PREVIEW_STORAGE_KEY, setPreview } from "@/lib/previewSession";
import { endPreviewIfAny, enterProject, exitProject, switchProject, transitionPhase, endTransition } from "@/lib/studioContext";
import { endWorkspaceLeave, getWorkspace, isWorkspaceLeaving, setWorkspace } from "@/lib/workspace";
import { getToken } from "@/lib/api";

type Call = { method: string; path: string; auth: string | undefined; body: Record<string, unknown> | null; phase: string };
const json = (body: unknown, status: number) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const session = (slug: string) => ({ id: `ws-${slug}`, tenantId: `t-${slug}`, tenantSlug: slug, tenantName: `Khách ${slug}`, openedAt: "2098-01-01T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" });
const opened = (slug: string) => ({ data: { accessToken: `WS-${slug}`, expiresAt: "2099-01-01T00:00:00Z", session: { ...session(slug), resumed: false } } });

let calls: Call[];
let route: Record<string, () => Response | Promise<Response>>;
function network() {
  calls = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const path = String(url).replace(/^https?:\/\/[^/]+\/api\/v1/, "").split("?")[0];
    const method = init?.method ?? "GET";
    calls.push({ method, path, auth: (init?.headers as Record<string, string> | undefined)?.Authorization, body: init?.body ? JSON.parse(String(init.body)) : null, phase: transitionPhase() });
    const h = route[`${method} ${path}`];
    if (!h) throw new Error(`unexpected fetch: ${method} ${path}`);
    return h();
  }) as unknown as typeof fetch;
}
const inSessionOf = (slug: string) =>
  setWorkspace({ accessToken: `WS-${slug}`, expiresAt: "2099-01-01T00:00:00Z", homeUserId: "u-admin", session: session(slug) });

const assign = vi.fn();
const reload = vi.fn();
const realLocation = window.location;
const FIRST_B = "/studio/du-an/b/dung/du-lieu";
let qc: QueryClient;
let order: string[];
let confirmLeave: Mock<() => boolean>;
const deps = () => ({ qc, confirmLeave });

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "HOME-TOK");
  window.localStorage.setItem("nexus_refresh", "HOME-REFRESH");
  window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true }));
  endWorkspaceLeave();
  endPreviewExit(); // a test that ended a live preview leaves its latch up
  endTransition();
  vi.clearAllMocks();
  Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, reload, pathname: "/studio", href: "http://localhost/studio" } });
  route = {};
  network();
  qc = new QueryClient();
  order = [];
  vi.spyOn(qc, "cancelQueries").mockImplementation(async () => void order.push("cancel"));
  vi.spyOn(qc, "clear").mockImplementation(() => void order.push("clear"));
  assign.mockImplementation(() => void order.push("assign"));
  confirmLeave = vi.fn<() => boolean>(() => true);
});
afterEach(() => Object.defineProperty(window, "location", { configurable: true, value: realLocation }));

describe("switchProject (FR-085)", () => {
  beforeEach(() => {
    inSessionOf("a");
    route["DELETE /admin/workspace-sessions/current"] = () => new Response(null, { status: 204 });
    route["POST /admin/workspace-sessions"] = () => json(opened("b"), 201);
  });

  it("freezes first, then sends ONLY the close of A (A's token) and the open of B (home token), then clears the cache and loads fresh", async () => {
    await switchProject("b", FIRST_B, deps());
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(["DELETE /admin/workspace-sessions/current", "POST /admin/workspace-sessions"]);
    expect(calls[0].auth).toBe("Bearer WS-a");
    expect(calls[1].auth).toBe("Bearer HOME-TOK");
    expect(calls[1].body).toEqual({ tenantSlug: "b" });
    expect(calls.every((c) => c.phase === "running")).toBe(true); // the surface was already gone for both
    expect(getWorkspace()?.session.tenantSlug).toBe("b");
    expect(order).toEqual(["cancel", "clear", "assign"]);
    expect(assign).toHaveBeenCalledWith(FIRST_B);
  });

  it("from the first synchronous step on, nothing in the page can carry any token (the leave latch)", async () => {
    const done = switchProject("b", FIRST_B, deps());
    expect(transitionPhase()).toBe("running");
    expect(isWorkspaceLeaving()).toBe(true);
    expect(getToken()).toBeNull();
    await done;
    expect(getToken()).toBeNull(); // still: only the page being replaced lifts it
  });

  it("a refused 'leave without saving' touches nothing: zero calls, session A intact, no freeze", async () => {
    confirmLeave.mockReturnValue(false);
    await switchProject("b", FIRST_B, deps());
    expect(calls).toHaveLength(0);
    expect(getWorkspace()?.session.tenantSlug).toBe("a");
    expect(transitionPhase()).toBe("idle");
    expect(isWorkspaceLeaving()).toBe(false);
    expect(order).toEqual([]);
  });

  it("the question about unsaved changes comes before any freeze", async () => {
    confirmLeave.mockImplementation(() => {
      order.push(`confirm:${transitionPhase()}`);
      return true;
    });
    await switchProject("b", FIRST_B, deps());
    expect(order[0]).toBe("confirm:idle");
  });

  it("opening B fails: stays frozen as 'failed', A is already closed and is NOT restored, nothing loads", async () => {
    route["POST /admin/workspace-sessions"] = () => json({ error: "x", code: "TENANT_NOT_FOUND" }, 404);
    await expect(switchProject("b", FIRST_B, deps())).rejects.toMatchObject({ status: 404 });
    expect(transitionPhase()).toBe("failed");
    expect(getWorkspace()).toBeNull();
    expect(assign).not.toHaveBeenCalled();
    expect(order).not.toContain("clear");
  });

  it("a dead network on the close does not block the switch (leaving always works)", async () => {
    route["DELETE /admin/workspace-sessions/current"] = () => {
      throw new TypeError("Failed to fetch");
    };
    await switchProject("b", FIRST_B, deps());
    expect(getWorkspace()?.session.tenantSlug).toBe("b");
    expect(assign).toHaveBeenCalledWith(FIRST_B);
  });

  it("opening B fails: a leftover view-as-role record is already wiped (openWorkspace never got to do it), so 'Thử lại' reloads a clean tab", async () => {
    window.sessionStorage.setItem(PREVIEW_STORAGE_KEY, JSON.stringify({ role: { id: "r", name: "vai" } }));
    route["POST /admin/workspace-sessions"] = () => json({ error: "x", code: "TENANT_NOT_FOUND" }, 404);
    await expect(switchProject("b", FIRST_B, deps())).rejects.toMatchObject({ status: 404 });
    expect(window.sessionStorage.getItem(PREVIEW_STORAGE_KEY)).toBeNull();
  });

  it("a hostile next lands on the first page of B, never outside", async () => {
    await switchProject("b", "https://evil.example/x", deps());
    expect(assign).toHaveBeenCalledWith(FIRST_B);
  });
});

describe("exitProject", () => {
  beforeEach(() => {
    inSessionOf("a");
    route["DELETE /admin/workspace-sessions/current"] = () => new Response(null, { status: 204 });
  });

  it("freezes, closes with the session token, clears the cache, then loads next", async () => {
    await exitProject("/studio/du-an/a/van-hanh/sao-luu", deps());
    expect(calls.map((c) => `${c.method} ${c.path} ${c.auth}`)).toEqual(["DELETE /admin/workspace-sessions/current Bearer WS-a"]);
    expect(calls[0].phase).toBe("running");
    expect(getWorkspace()).toBeNull();
    expect(order).toEqual(["cancel", "clear", "assign"]);
    expect(assign).toHaveBeenCalledWith("/studio/du-an/a/van-hanh/sao-luu");
  });

  it("from the first synchronous step on, nothing in the page can carry any token (the leave latch)", async () => {
    const done = exitProject("/studio", deps());
    expect(transitionPhase()).toBe("running");
    expect(isWorkspaceLeaving()).toBe(true);
    expect(getToken()).toBeNull();
    await done;
    expect(getToken()).toBeNull(); // still: only the page being replaced lifts it
  });

  it("a leftover view-as-role record without a token is wiped too (the tab must not come back previewing)", async () => {
    window.sessionStorage.setItem(PREVIEW_STORAGE_KEY, JSON.stringify({ role: { id: "r", name: "vai" } }));
    expect(getPreview()).toBeNull(); // not a live preview: endPreviewIfAny lets exit go on
    await exitProject("/studio", deps());
    expect(window.sessionStorage.getItem(PREVIEW_STORAGE_KEY)).toBeNull();
    expect(assign).toHaveBeenCalledWith("/studio");
  });

  it("a live view-as-role tab ends the preview first and does NOT close the session in the same page", async () => {
    setPreview({ token: "PREV", sessionId: "p1", role: { id: "r", name: "vai" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio" });
    route["DELETE /preview-sessions/current"] = () => new Response(null, { status: 204 });
    await exitProject("/studio", deps());
    await vi.waitFor(() => expect(reload).toHaveBeenCalled());
    expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(["DELETE /preview-sessions/current"]);
    expect(assign).not.toHaveBeenCalled();
    expect(getWorkspace()?.session.tenantSlug).toBe("a");
  });

  it.each(["//evil.com", "https://evil.com", "/\t/evil.com", "\\\\evil", ""])("next %j becomes /studio", async (next) => {
    await exitProject(next, deps());
    expect(assign).toHaveBeenCalledWith("/studio");
  });

  it("a refused confirm closes nothing", async () => {
    confirmLeave.mockReturnValue(false);
    await exitProject("/studio", deps());
    expect(calls).toHaveLength(0);
    expect(getWorkspace()?.session.tenantSlug).toBe("a");
  });
});

describe("enterProject", () => {
  beforeEach(() => {
    route["POST /admin/workspace-sessions"] = () => json(opened("b"), 201);
  });

  it("opens with the HOME token, writes the session, latches, clears the cache and loads the project", async () => {
    await enterProject("b", FIRST_B, deps());
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({ method: "POST", path: "/admin/workspace-sessions", auth: "Bearer HOME-TOK", body: { tenantSlug: "b" } });
    expect(getWorkspace()).toMatchObject({ homeUserId: "u-admin", session: { tenantSlug: "b" } });
    expect(isWorkspaceLeaving()).toBe(true); // between the session being written and the page leaving no request may go out
    expect(order).toEqual(["cancel", "clear", "assign"]);
    expect(assign).toHaveBeenCalledWith(FIRST_B);
  });

  it("opening fails: nothing changed, so the page that asked stays usable and can say why (no freeze, no latch, home token still answers)", async () => {
    route["POST /admin/workspace-sessions"] = () => json({ error: "x", code: "TENANT_NOT_FOUND" }, 404);
    await expect(enterProject("zzz", FIRST_B, deps())).rejects.toMatchObject({ status: 404, code: "TENANT_NOT_FOUND" });
    expect(transitionPhase()).toBe("idle");
    expect(isWorkspaceLeaving()).toBe(false);
    expect(getToken()).toBe("HOME-TOK");
    expect(getWorkspace()).toBeNull();
    expect(assign).not.toHaveBeenCalled();
  });

  it("a hostile next lands on the first page of the project", async () => {
    await enterProject("b", "//evil.com", deps());
    expect(assign).toHaveBeenCalledWith(FIRST_B);
  });

  it("a refused confirm opens nothing", async () => {
    confirmLeave.mockReturnValue(false);
    await enterProject("b", FIRST_B, deps());
    expect(calls).toHaveLength(0);
  });
});

describe("endPreviewIfAny (FR-086)", () => {
  it("without a preview: false, no call", () => {
    expect(endPreviewIfAny()).toBe(false);
    expect(calls).toHaveLength(0);
  });

  it("with a preview: ends it on the server with the preview token, clears it, reloads; true", async () => {
    setPreview({ token: "PREV", sessionId: "p1", role: { id: "r", name: "vai" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio" });
    route["DELETE /preview-sessions/current"] = () => new Response(null, { status: 204 });
    expect(endPreviewIfAny()).toBe(true);
    await vi.waitFor(() => expect(reload).toHaveBeenCalled());
    expect(calls.map((c) => `${c.method} ${c.path} ${c.auth}`)).toEqual(["DELETE /preview-sessions/current Bearer PREV"]);
    expect(getPreview()).toBeNull();
  });

  it("a switch from a tab that still previews ends the preview and does NOT run the switch in the same page", async () => {
    inSessionOf("a");
    setPreview({ token: "PREV", sessionId: "p1", role: { id: "r", name: "vai" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio" });
    route["DELETE /preview-sessions/current"] = () => new Response(null, { status: 204 });
    await switchProject("b", FIRST_B, deps());
    await vi.waitFor(() => expect(reload).toHaveBeenCalled());
    expect(calls.map((c) => c.path)).toEqual(["/preview-sessions/current"]);
    expect(assign).not.toHaveBeenCalled();
  });
});
