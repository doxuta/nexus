// Tests for the SSO provider admin api surface (lib/admin.ts — ENG-AUTH2 #8, api-conventions
// §3c.4) and the public login-entry URL builder (lib/api.ts ssoStartUrl).
// Contract pins (verified against nexus-platform/internal/handlers/sso_handler.go):
//   • GET/POST/PATCH use the GENERIC { data: ... } envelope (NOT named-noun like users/roles).
//   • PATCH is PARTIAL: only provided keys ride in the body (absent = unchanged server-side);
//     an empty patch fails CLIENT-SIDE with the same 422 SSO_PROVIDER_FIELD_REQUIRED the BE
//     would return — and never hits the network.
//   • clientSecret is sent ONCE at create and never comes back in any response.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  listSsoProviders, createSsoProvider, updateSsoProvider, deleteSsoProvider,
  type SsoProvider,
} from "@/lib/admin";
import { ApiError, ssoStartUrl } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const P: SsoProvider = {
  id: "sp-1",
  tenantId: "t-1",
  issuer: "https://accounts.google.com",
  clientId: "abc123.apps.googleusercontent.com",
  displayName: "Google Workspace",
  enabled: true,
  jitProvisioning: false,
  trustIdpMfa: false,
  createdAt: "2026-07-08T09:00:00Z",
  updatedAt: "2026-07-08T09:00:00Z",
};

describe("SSO provider admin api surface", () => {
  let originalFetch: FetchFn;

  beforeEach(() => {
    originalFetch = global.fetch as unknown as FetchFn;
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
  });

  // ── list ────────────────────────────────────────────────────────────────────────────

  it("listSsoProviders: GET /admin/sso/providers, generic {data:[...]} envelope, no clientSecret in shape", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => jsonRes({ data: [P] }, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    const out = await listSsoProviders();
    expect(out).toEqual([P]);
    // The BE never returns the secret (ClientSecretEnc has json:"-") — the client type has no
    // such key either; pin that the parsed object carries none.
    expect(Object.keys(out[0])).not.toContain("clientSecret");

    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/admin/sso/providers");
    expect(init?.method).toBe("GET");
    expect(init?.body).toBeUndefined();
  });

  it("listSsoProviders: defensive — missing data → []", async () => {
    global.fetch = vi.fn(async () => jsonRes({}, 200)) as unknown as typeof fetch;
    await expect(listSsoProviders()).resolves.toEqual([]);
  });

  // ── create ──────────────────────────────────────────────────────────────────────────

  it("createSsoProvider: POST body carries exactly the create contract fields; returns {data}", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      const body = JSON.parse(String(init?.body ?? "{}"));
      expect(body).toEqual({
        issuer: "https://login.microsoftonline.com/tid/v2.0",
        clientId: "cid-1",
        clientSecret: "s3cret",
        displayName: "Azure AD",
        jitProvisioning: true,
      });
      return jsonRes({ data: { ...P, id: "sp-2", issuer: body.issuer, jitProvisioning: true } }, 201);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const created = await createSsoProvider({
      issuer: "https://login.microsoftonline.com/tid/v2.0",
      clientId: "cid-1",
      clientSecret: "s3cret",
      displayName: "Azure AD",
      jitProvisioning: true,
    });
    expect(created.id).toBe("sp-2");
    expect(created.jitProvisioning).toBe(true);

    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/admin/sso/providers");
    expect(init?.method).toBe("POST");
  });

  it("createSsoProvider: 409 SSO_PROVIDER_ALREADY_EXISTS surfaces as ApiError with the code", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Trùng issuer", code: "SSO_PROVIDER_ALREADY_EXISTS" }, 409),
    ) as unknown as typeof fetch;

    await expect(
      createSsoProvider({ issuer: "https://accounts.google.com", clientId: "c", clientSecret: "s" }),
    ).rejects.toMatchObject({ status: 409, code: "SSO_PROVIDER_ALREADY_EXISTS" });
  });

  it("createSsoProvider: 503 SSO_NOT_CONFIGURED (server switch off) surfaces honestly", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "SSO/OIDC chưa được cấu hình trên hệ thống", code: "SSO_NOT_CONFIGURED" }, 503),
    ) as unknown as typeof fetch;

    await expect(
      createSsoProvider({ issuer: "https://accounts.google.com", clientId: "c", clientSecret: "s" }),
    ).rejects.toMatchObject({ status: 503, code: "SSO_NOT_CONFIGURED" });
  });

  // ── patch (partial — the only mutable surface) ─────────────────────────────────────

  it("updateSsoProvider: {enabled:false} → body is EXACTLY {enabled:false} (no trustIdpMfa key)", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body))).toEqual({ enabled: false });
      return jsonRes({ data: { ...P, enabled: false } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const out = await updateSsoProvider("sp-1", { enabled: false });
    expect(out.enabled).toBe(false);

    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/admin/sso/providers/sp-1");
    expect(init?.method).toBe("PATCH");
  });

  it("updateSsoProvider: {trustIdpMfa:true} → body is EXACTLY {trustIdpMfa:true} (no enabled key)", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body))).toEqual({ trustIdpMfa: true });
      return jsonRes({ data: { ...P, trustIdpMfa: true } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    const out = await updateSsoProvider("sp-1", { trustIdpMfa: true });
    expect(out.trustIdpMfa).toBe(true);
  });

  it("updateSsoProvider: both flags → both keys ride together", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, init?: RequestInit) => {
      expect(JSON.parse(String(init?.body))).toEqual({ enabled: true, trustIdpMfa: false });
      return jsonRes({ data: { ...P, enabled: true, trustIdpMfa: false } }, 200);
    });
    global.fetch = fetchMock as unknown as typeof fetch;

    await updateSsoProvider("sp-1", { enabled: true, trustIdpMfa: false });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("updateSsoProvider: EMPTY patch → client-side 422 SSO_PROVIDER_FIELD_REQUIRED, network never touched", async () => {
    const fetchMock = vi.fn(async () => jsonRes({}, 200));
    global.fetch = fetchMock as unknown as typeof fetch;

    await expect(updateSsoProvider("sp-1", {})).rejects.toMatchObject({
      status: 422,
      code: "SSO_PROVIDER_FIELD_REQUIRED",
    });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("updateSsoProvider: 404 SSO_PROVIDER_NOT_FOUND (other tenant / deleted) surfaces", async () => {
    global.fetch = vi.fn(async () =>
      jsonRes({ error: "Không tìm thấy SSO provider", code: "SSO_PROVIDER_NOT_FOUND" }, 404),
    ) as unknown as typeof fetch;

    await expect(updateSsoProvider("ghost", { enabled: true })).rejects.toBeInstanceOf(ApiError);
    await expect(updateSsoProvider("ghost", { enabled: true })).rejects.toMatchObject({
      status: 404,
      code: "SSO_PROVIDER_NOT_FOUND",
    });
  });

  // ── delete ──────────────────────────────────────────────────────────────────────────

  it("deleteSsoProvider: DELETE /admin/sso/providers/:id, 204 empty body → resolves", async () => {
    const fetchMock = vi.fn(async (_url: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }));
    global.fetch = fetchMock as unknown as typeof fetch;

    await expect(deleteSsoProvider("sp-1")).resolves.toBeUndefined();
    const [url, init] = fetchMock.mock.calls[0];
    expect(String(url)).toContain("/admin/sso/providers/sp-1");
    expect(init?.method).toBe("DELETE");
  });
});

// ── ssoStartUrl (lib/api.ts) — public login entry, full-page redirect target ───────────

describe("ssoStartUrl", () => {
  it("builds /auth/sso/:tenant/start?provider= with both parts encoded", () => {
    const url = ssoStartUrl("northwind", "https://accounts.google.com");
    expect(url).toContain("/auth/sso/northwind/start?provider=https%3A%2F%2Faccounts.google.com");
  });

  it("encodes the tenant slug and trims whitespace", () => {
    const url = ssoStartUrl("  kh a  ", "  sp-1  ");
    expect(url).toContain("/auth/sso/kh%20a/start?provider=sp-1");
  });
});
