// adminReq → shared apiRequest (specs/065 lát A, #290).
// The admin screens used a private fetch wrapper with no 401 recovery: a token that expired
// mid-session made every action say "Yêu cầu xác thực không hợp lệ". Pins: 401 → ONE
// /auth/refresh → retry once; a second 401 does not loop; and the new client functions hit the
// exact method + path (the orphan gate is blind to both — FR-035).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Call = { url: string; method: string; auth?: string };

const originalFetch = global.fetch;
const jsonRes = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function mockFetch(handler: (c: Call) => Response): Call[] {
  const calls: Call[] = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const h = (init?.headers ?? {}) as Record<string, string>;
    const c = { url: String(url), method: init?.method ?? "GET", auth: h["Authorization"] };
    calls.push(c);
    return handler(c);
  }) as unknown as typeof fetch;
  return calls;
}

const path = (c: Call) => `${c.method} ${c.url.split("/api/v1")[1] ?? c.url}`;

describe("admin requests recover a 401 via the shared client", () => {
  beforeEach(() => {
    vi.resetModules();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "old-access");
    window.localStorage.setItem("nexus_refresh", "rt-1");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("401 → exactly one /auth/refresh → retried with the new access token", async () => {
    const calls = mockFetch((c) => {
      if (c.url.endsWith("/auth/refresh")) return jsonRes({ data: { accessToken: "new-access", refreshToken: "rt-2" } }, 200);
      if (c.auth === "Bearer old-access") return jsonRes({ error: "Yêu cầu xác thực không hợp lệ", code: "UNAUTHORIZED" }, 401);
      return jsonRes({ data: { users: [] } }, 200);
    });
    const admin = await import("@/lib/admin");
    await expect(admin.listUsers()).resolves.toEqual([]);
    expect(calls.map(path)).toEqual(["GET /admin/users", "POST /auth/refresh", "GET /admin/users"]);
    expect(calls[2].auth).toBe("Bearer new-access");
  });

  it("a second 401 after the retry surfaces (no loop)", async () => {
    const calls = mockFetch((c) => {
      if (c.url.endsWith("/auth/refresh")) return jsonRes({ data: { accessToken: "new-access", refreshToken: "rt-2" } }, 200);
      return jsonRes({ error: "nope", code: "UNAUTHORIZED" }, 401);
    });
    const admin = await import("@/lib/admin");
    await expect(admin.listUsers()).rejects.toMatchObject({ status: 401 });
    expect(calls.filter((c) => c.url.endsWith("/auth/refresh"))).toHaveLength(1);
    expect(calls).toHaveLength(3);
  });

  it("listUserSessions / endUserSession pin method + path", async () => {
    const calls = mockFetch((c) =>
      c.method === "DELETE" ? new Response(null, { status: 204 }) : jsonRes({ data: [{ id: "s1", lastUsedAt: "x", expiresAt: "y" }] }, 200),
    );
    const admin = await import("@/lib/admin");
    await expect(admin.listUserSessions("u1")).resolves.toHaveLength(1);
    await admin.endUserSession("u1", "s1");
    expect(calls.map(path)).toEqual(["GET /admin/users/u1/sessions", "DELETE /admin/users/u1/sessions/s1"]);
  });
});
