// specs/055-session-list-kill — the lib/api.ts ends of the login-session wire:
//   • logout() must send the refresh token: /auth/logout revokes the token IN THE BODY, and the old
//     `{}` body revoked nothing — the "logged out" browser stayed a live session for 7 days and
//     would now show up in the device list.
//   • listMySessions() reads GET /auth/sessions, and a stale access token there refreshes and
//     retries like /auth/me instead of failing the /profile card.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Call = { url: string; init?: RequestInit };

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function mockFetchSequence(handlers: Array<(c: Call) => Response>): Call[] {
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

async function freshApi() {
  vi.resetModules();
  return import("@/lib/api");
}

describe("login sessions — lib/api", () => {
  const originalFetch = global.fetch;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "access-1");
    window.localStorage.setItem("nexus_refresh", "refresh-1");
  });

  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("logout() posts THIS browser's refresh token so the server actually revokes it", async () => {
    const api = await freshApi();
    const calls = mockFetchSequence([() => new Response(null, { status: 204 })]);
    await api.logout();
    expect(calls).toHaveLength(1);
    expect(calls[0].url).toContain("/auth/logout");
    expect(JSON.parse(String(calls[0].init?.body))).toEqual({ refreshToken: "refresh-1" });
    expect(window.localStorage.getItem("nexus_refresh")).toBeNull();
  });

  it("listMySessions() returns the rows of GET /auth/sessions ([] for a null body)", async () => {
    const api = await freshApi();
    const row = { id: "s-1", browser: "Chrome", os: "macOS", ip: "113.161.42.x", lastUsedAt: "2026-09-30T01:00:00Z", expiresAt: "2026-10-07T01:00:00Z" };
    const calls = mockFetchSequence([
      () => jsonRes({ data: [row] }, 200),
      () => jsonRes({ data: null }, 200),
    ]);
    await expect(api.listMySessions()).resolves.toEqual([row]);
    expect(calls[0].url).toMatch(/\/auth\/sessions$/);
    expect(calls[0].init?.method).toBe("GET");
    await expect(api.listMySessions()).resolves.toEqual([]);
  });

  it("endMySession(id) DELETEs /auth/sessions/:id (id URL-encoded) and resolves on 204", async () => {
    const api = await freshApi();
    const calls = mockFetchSequence([() => new Response(null, { status: 204 })]);
    await expect(api.endMySession("s 1/x")).resolves.toBeUndefined();
    expect(calls[0].init?.method).toBe("DELETE");
    expect(calls[0].url).toMatch(/\/auth\/sessions\/s%201%2Fx$/);
  });

  it("a stale access token on /auth/sessions refreshes and retries (like /auth/me)", async () => {
    const api = await freshApi();
    const calls = mockFetchSequence([
      () => jsonRes({ error: "expired", code: "UNAUTHORIZED" }, 401),
      (c) => {
        expect(c.url).toContain("/auth/refresh");
        return jsonRes({ data: { accessToken: "access-2", refreshToken: "refresh-2" } }, 200);
      },
      (c) => {
        expect(new Headers(c.init?.headers).get("Authorization")).toBe("Bearer access-2");
        return jsonRes({ data: [] }, 200);
      },
    ]);
    await expect(api.listMySessions()).resolves.toEqual([]);
    expect(calls).toHaveLength(3);
  });
});
