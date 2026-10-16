// 401-recovery contract of lib/api.ts (dogfood finding 2026-07-15 — the mid-session tenant flip):
//   • REFRESH-FIRST: a 401 with a refresh token present must POST /auth/refresh and retry with
//     the new access token — NEVER /auth/login, even in DEMO_MODE. The old code let DEMO_MODE
//     short-circuit to the seed auto-login, silently re-authenticating an expired lab/customer
//     session as the seed tenant's ADMIN and writing there (cross-tenant flip).
//   • Seed auto-login is a COLD-START bootstrap only: no token, no refresh, no cached nexus_user.
//     An expired real session (nexus_user cache present) must surface NEED_LOGIN, not seed-login.
//   • DEMO_MODE is fail-closed: only NEXT_PUBLIC_DEMO_MODE="true" enables it (old `!== "false"`
//     default meant every build that forgot the env shipped seed-creds auto-login).
// Fresh module instance per test (vi.resetModules + dynamic import) because DEMO_MODE is a
// module-load const read from the env.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Call = { url: string; init?: RequestInit };

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

// Sequenced fetch mock: pops one handler per call, records every call.
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

async function freshApi(demoMode: string | undefined, seedCreds = true) {
  vi.resetModules();
  if (demoMode === undefined) {
    vi.stubEnv("NEXT_PUBLIC_DEMO_MODE", "");
  } else {
    vi.stubEnv("NEXT_PUBLIC_DEMO_MODE", demoMode);
  }
  // Seed identity is env-sourced (never a source literal) — auto-login needs all three.
  vi.stubEnv("NEXT_PUBLIC_DEMO_TENANT", seedCreds ? "northwind" : "");
  vi.stubEnv("NEXT_PUBLIC_DEMO_EMAIL", seedCreds ? "admin@example.com" : "");
  vi.stubEnv("NEXT_PUBLIC_DEMO_PASSWORD", seedCreds ? "test-seed-pw" : "");
  return import("@/lib/api");
}

describe("401 recovery — refresh-first, seed-login only on cold start", () => {
  const originalFetch = global.fetch;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    global.fetch = originalFetch;
    vi.unstubAllEnvs();
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("DEMO_MODE is fail-closed: unset env → disabled", async () => {
    const api = await freshApi(undefined);
    expect(api.DEMO_MODE).toBe(false);
    const apiOn = await freshApi("true");
    expect(apiOn.DEMO_MODE).toBe(true);
  });

  it("401 with a refresh token → /auth/refresh + retry, NEVER /auth/login (even in DEMO_MODE)", async () => {
    const api = await freshApi("true"); // demo mode ON — the flip scenario
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "expired-access");
    window.localStorage.setItem("nexus_refresh", "live-refresh");
    // A prior LAB session exists — exactly the dogfood situation.
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-lab", tenantId: "t-lab", email: "lab@example.com" }));

    const calls = mockFetchSequence([
      () => jsonRes({ error: "expired", code: "UNAUTHORIZED" }, 401), // data route, stale token
      (c) => {
        expect(c.url).toContain("/auth/refresh");
        expect(JSON.parse(String(c.init?.body))).toEqual({ refreshToken: "live-refresh" });
        return jsonRes({ data: { accessToken: "new-access", refreshToken: "new-refresh" } }, 200);
      },
      (c) => {
        // Retry carries the ROTATED token — same session, same tenant.
        expect((c.init?.headers as Record<string, string>).Authorization).toBe("Bearer new-access");
        return jsonRes({ data: [] }, 200);
      },
    ]);

    await expect(api.listEntities()).resolves.toEqual([]);
    expect(calls).toHaveLength(3);
    expect(calls.some((c) => c.url.includes("/auth/login"))).toBe(false);
    // Rotated pair stored.
    expect(window.localStorage.getItem("nexus_token")).toBe("new-access");
    expect(window.localStorage.getItem("nexus_refresh")).toBe("new-refresh");
  });

  it("DEMO_MODE cold start (no session trace) → seed auto-login bootstraps, then retries", async () => {
    const api = await freshApi("true");
    // No tokens, no nexus_user — a fresh dev profile.
    const calls = mockFetchSequence([
      () => jsonRes({ error: "no token", code: "UNAUTHORIZED" }, 401),
      (c) => {
        expect(c.url).toContain("/auth/login");
        const body = JSON.parse(String(c.init?.body));
        expect(body.tenant).toBe("northwind");
        return jsonRes({ data: { accessToken: "seed-access", refreshToken: "seed-refresh", user: { id: "u", tenantId: "t-northwind", email: "admin@example.com" } } }, 200);
      },
      () => jsonRes({ data: [] }, 200),
    ]);

    await expect(api.listEntities()).resolves.toEqual([]);
    expect(calls).toHaveLength(3);
  });

  it("expired access + live refresh on /auth/me → refreshes and retries (no /login bounce)", async () => {
    const api = await freshApi("true");
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "expired-access");
    window.localStorage.setItem("nexus_refresh", "live-refresh");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-lab", tenantId: "t-lab", email: "lab@example.com" }));

    const calls = mockFetchSequence([
      () => jsonRes({ error: "expired", code: "UNAUTHORIZED" }, 401), // GET /auth/me, stale token
      () => jsonRes({ data: { accessToken: "new-access", refreshToken: "new-refresh" } }, 200),
      () => jsonRes({ data: { id: "u-lab", tenantId: "t-lab", email: "lab@example.com", fullName: "Lab", isAdmin: true } }, 200),
    ]);

    await expect(api.getMe()).resolves.toMatchObject({ email: "lab@example.com" });
    expect(calls[1].url).toContain("/auth/refresh");
    expect(calls.some((c) => c.url.includes("/auth/login"))).toBe(false);
  });

  it("DEMO_MODE without env seed creds → fail-closed, no auto-login", async () => {
    const api = await freshApi("true", false); // demo on, but no NEXT_PUBLIC_DEMO_* identity
    const calls = mockFetchSequence([
      () => jsonRes({ error: "no token", code: "UNAUTHORIZED" }, 401),
    ]);

    await expect(api.listEntities()).rejects.toMatchObject({ code: "NEED_LOGIN" });
    expect(calls).toHaveLength(1);
    expect(calls.some((c) => c.url.includes("/auth/login"))).toBe(false);
  });

  // The SECOND door into the cold-start room, found 2026-07-29 during the tay-mơ walkthrough:
  // logout() removes nexus_user, which is the exact flag the guard keys on — so on any DEMO_MODE
  // box the logout button undid itself and put you straight back in as seed ADMIN. Nothing was
  // leaked (DEMO_MODE is opt-in, prod sets false), but LUẬT NGHIỆM THU SỐ 0 — every acceptance
  // ends logged in as a NON-admin — was physically unperformable on the machine that matters.
  it("explicit logout survives DEMO_MODE: no seed auto-login until someone logs in for real", async () => {
    const api = await freshApi("true");
    // A live seed session, exactly as the demo box carries it.
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u", tenantId: "t-northwind", email: "admin@example.com" }));
    window.sessionStorage.setItem("nexus_token", "live-access");

    const calls = mockFetchSequence([
      (c) => { expect(c.url).toContain("/auth/logout"); return jsonRes({}, 200); },
      () => jsonRes({ error: "no token", code: "UNAUTHORIZED" }, 401),
    ]);

    await api.logout();
    // The cold-start guard's own signal is now gone — pre-fix, this alone re-opened auto-login.
    expect(window.localStorage.getItem("nexus_user")).toBeNull();

    // The next call must land on the login screen, NOT back inside as the seed admin.
    await expect(api.listEntities()).rejects.toMatchObject({ code: "NEED_LOGIN" });
    expect(calls.some((c) => c.url.includes("/auth/login"))).toBe(false);
  });

  it("a real login lifts the opt-out — the fresh-profile dev convenience is untouched", async () => {
    const api = await freshApi("true");
    window.localStorage.setItem("nexus_logged_out", "1"); // as if the user had just logged out

    mockFetchSequence([
      (c) => {
        expect(c.url).toContain("/auth/login");
        const body = JSON.parse(String(c.init?.body));
        expect(body.email).toBe("test@example.com"); // a REAL account, not the seed
        return jsonRes({ data: { accessToken: "a", refreshToken: "r", user: { id: "u2", tenantId: "t-northwind", email: "test@example.com" } } }, 200);
      },
    ]);

    await expect(api.login("northwind", "test@example.com", "pw")).resolves.toMatchObject({ status: "ok" });
    // Cleared in applyTokenPair (not login) so SSO exchange and MFA verify lift it too.
    expect(window.localStorage.getItem("nexus_logged_out")).toBeNull();
  });

  it("DEMO_MODE with an EXPIRED real session (user cache, no refresh) → NEED_LOGIN, no seed login", async () => {
    const api = await freshApi("true");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-lab", tenantId: "t-lab", email: "lab@example.com" }));
    // No access token, no refresh token — fully expired session.
    const calls = mockFetchSequence([
      () => jsonRes({ error: "no token", code: "UNAUTHORIZED" }, 401),
      // recover401 → no refresh → ensureAuth → hadSession → prod branch throws. No further fetch.
    ]);

    await expect(api.listEntities()).rejects.toMatchObject({ code: "NEED_LOGIN" });
    expect(calls).toHaveLength(1);
    expect(calls.some((c) => c.url.includes("/auth/login"))).toBe(false);
  });

  // AuthProvider runs ensureAuth on EVERY route, the public landing included. Without "/" in the
  // public list an anonymous visitor (prod build, or a demo box after logout) was sent to /login
  // before the landing could paint, so its login CTA was unreachable.
  it("anonymous visitor on the public landing '/' stays there; a private route still bounces", async () => {
    const api = await freshApi(undefined);
    window.history.replaceState(null, "", "/");
    await expect(api.ensureAuth()).rejects.toMatchObject({ code: "NEED_LOGIN" });
    expect(window.location.pathname).toBe("/");

    window.history.replaceState(null, "", "/home");
    await expect(api.ensureAuth()).rejects.toMatchObject({ code: "NEED_LOGIN" });
    expect(window.location.pathname).toBe("/login");
  });
});
