// Home refresh-token rotation across browser tabs (specs/062-customer-workspace L1c, D-19).
//
// With "Ghi nhớ đăng nhập" ON every tab shares ONE login — one refresh-token chain — through
// localStorage, but each tab keeps its own in-memory copy of the pair. A tab that 401s and presents
// the refresh token another tab already rotated away is read by the server as a stolen token (E5):
// every session of the user is revoked and a bogus "security alert" mail goes out. The fix is to
// serialize rotation across tabs (Web Locks) and to re-read the shared storage INSIDE the lock: if
// the stored refresh token differs from the one this tab holds, another tab already rotated — adopt
// that pair, send nothing.
//
// A "tab" here = its own copy of lib/api.ts (vi.resetModules) over the SAME localStorage, plus a
// fake of navigator.locks shared by all tabs; the fetch fake plays the server (rotates, and counts a
// presented already-rotated token as "reuse").
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

type Api = typeof import("@/lib/api");

interface ServerState {
  live: string; // the one refresh token the server accepts
  rotated: Set<string>;
  n: number;
  validAccess: string; // the one access token data routes accept
  refreshCalls: number;
  reuse: number;
}

// Access tokens are decodable JWTs now (the refresh path compares uid/tid/sid and exp). Unsigned —
// the fake server matches them by string, the client only reads the claims.
function mk(label: string, expInSec = 900, who: { uid: string; tid: string; sid: string } = { uid: "u1", tid: "t1", sid: "s1" }): string {
  const b64 = (o: unknown) => btoa(JSON.stringify(o)).replace(/=+$/, "").replace(/\+/g, "-").replace(/\//g, "_");
  return `${b64({ alg: "none" })}.${b64({ ...who, jti: label, exp: Math.floor(Date.now() / 1000) + expInSec })}.sig`;
}

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function installServer(server: ServerState): void {
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    if (u.includes("/auth/refresh")) {
      server.refreshCalls++;
      // Yield so a missing lock really interleaves two rotations.
      await new Promise((r) => setTimeout(r, 5));
      const rt = JSON.parse(String(init?.body)).refreshToken as string;
      if (rt === server.live) {
        server.rotated.add(rt);
        server.n++;
        server.live = `R${server.n}`;
        server.validAccess = mk(`A${server.n}`);
        return jsonRes({ data: { accessToken: server.validAccess, refreshToken: server.live } }, 200);
      }
      if (server.rotated.has(rt)) server.reuse++;
      return jsonRes({ error: "invalid", code: "INVALID_REFRESH" }, 401);
    }
    const auth = (init?.headers as Record<string, string> | undefined)?.Authorization;
    if (auth === `Bearer ${server.validAccess}`) return jsonRes({ data: [] }, 200);
    return jsonRes({ error: "expired", code: "UNAUTHORIZED" }, 401);
  }) as unknown as typeof fetch;
}

// One mutex per lock name, shared by every "tab" like the browser's lock manager is.
function installFakeLocks(): void {
  const tails = new Map<string, Promise<unknown>>();
  Object.defineProperty(navigator, "locks", {
    configurable: true,
    value: {
      request: async (name: string, cb: () => Promise<unknown>) => {
        const prev = tails.get(name) ?? Promise.resolve();
        const run = prev.then(() => cb());
        tails.set(name, run.catch(() => undefined));
        return run;
      },
    },
  });
}

function removeLocks(): void {
  Object.defineProperty(navigator, "locks", { configurable: true, value: undefined });
}

async function newTab(): Promise<Api> {
  vi.resetModules();
  const api = await import("@/lib/api");
  // The tab has been open for a while: its in-memory pair is the one currently in storage.
  api.getToken();
  api.getRefresh();
  return api;
}

describe("home refresh rotation across tabs (D-19)", () => {
  const originalFetch = global.fetch;
  let server: ServerState;

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", mk("A0"));
    window.localStorage.setItem("nexus_refresh", "R0");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u1", tenantId: "t1", email: "a@b.c" }));
    server = { live: "R0", rotated: new Set(), n: 0, validAccess: "A-none", refreshCalls: 0, reuse: 0 };
    installServer(server);
    installFakeLocks();
  });

  afterEach(() => {
    global.fetch = originalFetch;
    removeLocks();
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("tab B 401s after tab A rotated: B adopts the stored pair — no POST /auth/refresh, no reuse", async () => {
    const a = await newTab();
    const b = await newTab();

    await expect(a.listEntities()).resolves.toEqual([]); // A: 401 → rotate R0→R1
    expect(server.refreshCalls).toBe(1);

    await expect(b.listEntities()).resolves.toEqual([]); // B still holds A0/R0 → 401 → adopts A1/R1
    expect(server.refreshCalls).toBe(1); // B did NOT call the server
    expect(server.reuse).toBe(0);
    expect(window.localStorage.getItem("nexus_refresh")).toBe("R1");

    // A rotates again with its latest token (R1), B adopts again.
    server.validAccess = "A-none2"; // access expires
    await expect(a.listEntities()).resolves.toEqual([]);
    expect(server.refreshCalls).toBe(2);
    await expect(b.listEntities()).resolves.toEqual([]);
    expect(server.refreshCalls).toBe(2);
    expect(server.reuse).toBe(0);
    expect(window.localStorage.getItem("nexus_refresh")).toBe("R2");
  });

  it("both tabs 401 at once: exactly ONE POST /auth/refresh, zero reuse", async () => {
    const a = await newTab();
    const b = await newTab();
    await expect(Promise.all([a.listEntities(), b.listEntities()])).resolves.toEqual([[], []]);
    expect(server.refreshCalls).toBe(1);
    expect(server.reuse).toBe(0);
    expect(window.localStorage.getItem("nexus_refresh")).toBe("R1");
  });

  it("no navigator.locks: the sequential scenario still reads storage inside silentRefresh — zero reuse", async () => {
    removeLocks();
    const a = await newTab();
    const b = await newTab();
    await a.listEntities();
    await b.listEntities();
    expect(server.refreshCalls).toBe(1);
    expect(server.reuse).toBe(0);
  });

  it("tab B gets a stored pair whose access token has ALSO expired (both tabs idle): B rotates the stored chain — one call, no reuse", async () => {
    const a = await newTab();
    const b = await newTab();
    // Tab A rotated R0→R1 a while ago; the A1 it got has expired since (nobody used the app for >15 min).
    server.rotated.add("R0");
    server.live = "R1";
    server.n = 1; // the next rotation issues R2/A2
    window.localStorage.setItem("nexus_token", mk("A1", -60));
    window.localStorage.setItem("nexus_refresh", "R1");
    void a;

    await expect(b.listEntities()).resolves.toEqual([]); // B holds A0/R0: 401 → stored A1 expired → rotates R1
    expect(server.refreshCalls).toBe(1);
    expect(server.reuse).toBe(0);
    expect(window.localStorage.getItem("nexus_refresh")).toBe("R2");
  });

  it("storage carries ANOTHER login (other user/tenant/session): B never adopts it and rotates its own chain", async () => {
    const b = await newTab(); // holds A0/R0 of u1/t1/s1
    // A second login in another tab (other customer) overwrote the shared localStorage.
    window.localStorage.setItem("nexus_token", mk("X0", 900, { uid: "u2", tid: "t2", sid: "s2" }));
    window.localStorage.setItem("nexus_refresh", "RX");

    await expect(b.listEntities()).resolves.toEqual([]); // 401 → presents ITS OWN R0, not RX
    expect(server.refreshCalls).toBe(1);
    expect(server.reuse).toBe(0);
    expect(window.localStorage.getItem("nexus_refresh")).toBe("R1");
    expect(JSON.parse(atob(b.getToken()!.split(".")[1])).tid).toBe("t1");
  });
});
