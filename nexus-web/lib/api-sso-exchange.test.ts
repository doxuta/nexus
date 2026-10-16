// Tests for ssoExchange() (#48 SSO-FE-REDIRECT) — the POST /auth/sso/exchange client.
// Contract: same LoginOutput envelope as POST /auth/login, so the same discriminated union
// AND the same P0 rule apply: the mfa branch must NEVER touch token storage (the pre-fix
// login() stored `undefined` as the access token and locked MFA users out).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { ssoExchange, logout, getToken } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("ssoExchange", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    // Reset the module-level token cache from any previous test (fetch mocked so no network).
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("POSTs {code} to /auth/sso/exchange and a token-pair reply → {status:'ok'} + session stored", async () => {
    const user = { id: "u1", email: "sso@example.com", fullName: "S", isAdmin: false };
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body ?? "{}"))).toEqual({ code: "handoff-raw-1" });
      return jsonRes({ data: { accessToken: "at-sso", refreshToken: "rt-sso", user } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const res = await ssoExchange("handoff-raw-1");
    expect(res.status).toBe("ok");
    if (res.status === "ok") expect(res.user.email).toBe("sso@example.com");
    expect(String(fetchMock.mock.calls[0][0])).toContain("/auth/sso/exchange");
    // Session stored exactly like a password login (sessionStorage — no rememberMe here).
    expect(getToken()).toBe("at-sso");
    expect(window.sessionStorage.getItem("nexus_token")).toBe("at-sso");
    expect(window.sessionStorage.getItem("nexus_refresh")).toBe("rt-sso");
    expect(JSON.parse(window.localStorage.getItem("nexus_user") ?? "{}").id).toBe("u1");
  });

  it("MFA-pending reply → {status:'mfa', pendingToken} and NO token is stored (P0 rule)", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { mfaPending: true, pendingToken: "pt-sso-1" } }, 200),
    ) as unknown as typeof fetch;

    const res = await ssoExchange("handoff-raw-2");
    expect(res).toEqual({ status: "mfa", pendingToken: "pt-sso-1" });
    expect(getToken()).toBeNull();
    expect(window.localStorage.getItem("nexus_token")).toBeNull();
    expect(window.sessionStorage.getItem("nexus_token")).toBeNull();
    expect(window.localStorage.getItem("nexus_user")).toBeNull();
  });

  it("mfaPending WITHOUT pendingToken (malformed reply) → throws, nothing stored", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { mfaPending: true } }, 200),
    ) as unknown as typeof fetch;

    await expect(ssoExchange("handoff-raw-3")).rejects.toMatchObject({
      code: "MFA_PENDING_MALFORMED",
    });
    expect(getToken()).toBeNull();
  });

  it("200 with NEITHER mfaPending NOR accessToken → throws defensively, no phantom session", async () => {
    global.fetch = vi.fn(async () => jsonRes({ data: {} }, 200)) as unknown as typeof fetch;

    await expect(ssoExchange("handoff-raw-4")).rejects.toMatchObject({
      status: 500,
      code: "LOGIN_RESPONSE_MALFORMED",
    });
    expect(getToken()).toBeNull();
  });

  it("401 SSO_HANDOFF_INVALID (unknown/used/expired code) surfaces as ApiError, no token", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Mã hoàn tất SSO không hợp lệ", code: "SSO_HANDOFF_INVALID" }, 401),
    ) as unknown as typeof fetch;

    await expect(ssoExchange("burned-code")).rejects.toMatchObject({
      status: 401,
      code: "SSO_HANDOFF_INVALID",
    });
    expect(getToken()).toBeNull();
  });
});
