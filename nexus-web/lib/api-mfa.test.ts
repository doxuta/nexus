// Tests for the ENG-AUTH2 #7 MFA client surface in lib/api.ts.
// P0 regression pinned here: pre-fix login() called setToken(r.data.accessToken)
// UNCONDITIONALLY — an MFA-enabled account's response ({mfaPending, pendingToken}, NO
// accessToken) stored `undefined` and locked the user out. The discriminated LoginResult
// must return the pending shape WITHOUT touching token storage.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  login, verifyMfa, mfaEnroll, mfaActivate, mfaDisable, logout, getToken, getMe, getCachedMe,
  ApiError,
} from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

describe("MFA api surface", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    // Reset the module-level token cache from any previous test: logout() clears
    // _accessToken/_refreshToken in its finally block (fetch mocked so no network).
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

  // ── login() discriminated union — the P0 fix ──────────────────────────────────────

  it("login: MFA-pending response → {status:'mfa', pendingToken} and NO token is stored", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { mfaPending: true, pendingToken: "pt-opaque-1" } }, 200),
    ) as unknown as typeof fetch;

    const res = await login("northwind", "mfa@example.com", "correct-pw");
    expect(res).toEqual({ status: "mfa", pendingToken: "pt-opaque-1" });
    // The lockout bug: this used to store `undefined` as the access token.
    expect(getToken()).toBeNull();
    expect(window.localStorage.getItem("nexus_token")).toBeNull();
    expect(window.sessionStorage.getItem("nexus_token")).toBeNull();
    expect(window.localStorage.getItem("nexus_user")).toBeNull();
  });

  it("login: non-MFA response → {status:'ok'} and the token pair + user cache are stored", async () => {
    const user = { id: "u1", email: "a@example.com", fullName: "A", isAdmin: false };
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { accessToken: "at-1", refreshToken: "rt-1", user } }, 200),
    ) as unknown as typeof fetch;

    const res = await login("northwind", "a@example.com", "pw");
    expect(res.status).toBe("ok");
    if (res.status === "ok") expect(res.user.email).toBe("a@example.com");
    expect(getToken()).toBe("at-1");
    // rememberMe defaults false → sessionStorage scope
    expect(window.sessionStorage.getItem("nexus_token")).toBe("at-1");
    expect(window.sessionStorage.getItem("nexus_refresh")).toBe("rt-1");
    expect(JSON.parse(window.localStorage.getItem("nexus_user") ?? "{}").id).toBe("u1");
  });

  it("login: mfaPending WITHOUT pendingToken (malformed server reply) → throws, nothing stored", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { mfaPending: true } }, 200),
    ) as unknown as typeof fetch;

    await expect(login("northwind", "x@example.com", "pw")).rejects.toMatchObject({
      code: "MFA_PENDING_MALFORMED",
    });
    expect(getToken()).toBeNull();
  });

  it("login: 200 with NEITHER mfaPending NOR accessToken (malformed reply) → throws defensively (NIT-1)", async () => {
    // Symmetric guard to the MFA_PENDING_MALFORMED case above — a response that is neither
    // shape must never silently resolve {status:"ok"} with no session.
    global.fetch = vi.fn(async () => jsonRes({ data: {} }, 200)) as unknown as typeof fetch;

    await expect(login("northwind", "y@example.com", "pw")).rejects.toMatchObject({
      status: 500,
      code: "LOGIN_RESPONSE_MALFORMED",
    });
    expect(getToken()).toBeNull();
    expect(window.localStorage.getItem("nexus_user")).toBeNull();
  });

  // ── verifyMfa() — step 2 completes the session exactly like a normal login ─────────

  it("verifyMfa: success → stores the token pair + caches the user (same as login)", async () => {
    const user = { id: "u2", email: "mfa@example.com", fullName: "M", isAdmin: true };
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body ?? "{}"));
      expect(body).toEqual({ pendingToken: "pt-1", code: "123456" });
      return jsonRes({ data: { accessToken: "at-2", refreshToken: "rt-2", user } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const me = await verifyMfa("pt-1", "123456");
    expect(me.id).toBe("u2");
    expect(getToken()).toBe("at-2");
    expect(window.sessionStorage.getItem("nexus_refresh")).toBe("rt-2");
    expect(JSON.parse(window.localStorage.getItem("nexus_user") ?? "{}").id).toBe("u2");
    const url = String(fetchMock.mock.calls[0][0]);
    expect(url).toContain("/auth/mfa/verify");
  });

  it("verifyMfa: 401 MFA_CODE_INVALID → throws ApiError with the code, token stays unset", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Mã xác thực không đúng hoặc đã được sử dụng", code: "MFA_CODE_INVALID" }, 401),
    ) as unknown as typeof fetch;

    await expect(verifyMfa("pt-1", "000000")).rejects.toMatchObject({
      status: 401,
      code: "MFA_CODE_INVALID",
    });
    expect(getToken()).toBeNull();
  });

  it("verifyMfa: 429 with Retry-After header → details.retryAfter is merged", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Quá nhiều", code: "RATE_LIMITED" }, 429, { "Retry-After": "30" }),
    ) as unknown as typeof fetch;

    try {
      await verifyMfa("pt-1", "111111");
      expect.unreachable("should have thrown");
    } catch (e) {
      expect(e).toBeInstanceOf(ApiError);
      expect((e as ApiError).details.retryAfter).toBe(30);
    }
  });

  // ── Self-service trio ───────────────────────────────────────────────────────────────

  it("mfaEnroll: sends {password} to /auth/mfa/enroll and returns {secret, otpauthUrl}", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body))).toEqual({ password: "pw" });
      return jsonRes({ data: { secret: "ABCD2345", otpauthUrl: "otpauth://totp/Nexus:a?secret=ABCD2345&issuer=Nexus" } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const out = await mfaEnroll("pw");
    expect(out.secret).toBe("ABCD2345");
    expect(out.otpauthUrl).toContain("otpauth://totp/");
    expect(String(fetchMock.mock.calls[0][0])).toContain("/auth/mfa/enroll");
  });

  it("mfaEnroll: 401 INVALID_CREDENTIALS (wrong step-up password) is surfaced", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Mật khẩu không đúng", code: "INVALID_CREDENTIALS" }, 401),
    ) as unknown as typeof fetch;

    await expect(mfaEnroll("wrong")).rejects.toMatchObject({ status: 401, code: "INVALID_CREDENTIALS" });
  });

  it("mfaActivate: sends {password, code} and returns the backup code list", async () => {
    const codes = Array.from({ length: 10 }, (_, i) => `code-${i}`);
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body))).toEqual({ password: "pw", code: "123456" });
      return jsonRes({ data: { backupCodes: codes } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    await expect(mfaActivate("pw", "123456")).resolves.toEqual(codes);
    expect(String(fetchMock.mock.calls[0][0])).toContain("/auth/mfa/activate");
  });

  it("mfaDisable: resolves on the BE's 200 {data:{ok:true}} reply", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body))).toEqual({ password: "pw", code: "654321" });
      return jsonRes({ data: { ok: true } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    await expect(mfaDisable("pw", "654321")).resolves.toBeUndefined();
    expect(String(fetchMock.mock.calls[0][0])).toContain("/auth/mfa/disable");
  });

  it("mfaActivate: 422 MFA_PASSWORD_REQUIRED (missing step-up password) is surfaced", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Cần mật khẩu", code: "MFA_PASSWORD_REQUIRED" }, 422),
    ) as unknown as typeof fetch;

    await expect(mfaActivate("", "123456")).rejects.toMatchObject({
      status: 422,
      code: "MFA_PASSWORD_REQUIRED",
    });
  });

  // ── FE-CATCHUP-1: /auth/me now carries the honest mfaEnabled flag ──────────────────

  it("getMe: parses mfaEnabled from /auth/me AND re-caches it in nexus_user (no stale flag)", async () => {
    const fetchMock = vi.fn(async (url: RequestInfo | URL) =>
      String(url).includes("/auth/me")
        ? jsonRes({ data: { id: "u1", email: "a@example.com", fullName: "A", isAdmin: false, mfaEnabled: true } }, 200)
        : jsonRes({ error: "unexpected", code: "UNEXPECTED" }, 500),
    );
    global.fetch = fetchMock as unknown as typeof fetch;

    const me = await getMe();
    expect(String(fetchMock.mock.calls[0][0])).toContain("/auth/me");
    expect(me.mfaEnabled).toBe(true);
    // getMe() writes the payload back to localStorage — the cached copy carries the flag too.
    expect(getCachedMe()?.mfaEnabled).toBe(true);
    expect(JSON.parse(window.localStorage.getItem("nexus_user") ?? "{}").mfaEnabled).toBe(true);
  });

  it("getMe: mfaEnabled:false round-trips as false (and an older BE omitting it → undefined)", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ data: { id: "u1", email: "a@example.com", fullName: "A", isAdmin: false, mfaEnabled: false } }, 200),
    ) as unknown as typeof fetch;
    await expect(getMe()).resolves.toMatchObject({ mfaEnabled: false });
    expect(getCachedMe()?.mfaEnabled).toBe(false);

    global.fetch = vi.fn(async () =>
      jsonRes({ data: { id: "u1", email: "a@example.com", fullName: "A", isAdmin: false } }, 200),
    ) as unknown as typeof fetch;
    const older = await getMe();
    expect(older.mfaEnabled).toBeUndefined(); // "not told" — never coerced to false
  });
});
