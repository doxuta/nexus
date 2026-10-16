// Component tests — SsoProviders admin screen (ENG-AUTH2 #8 FE).
// Pins the SECURITY-CRITICAL copy: the trust_idp_mfa warning ("Bật = tin MFA của IdP, BỎ QUA MFA
// cục bộ cho provider này.") must be PERMANENTLY visible per row AND repeated in the enable
// confirm step; enabling requires that explicit confirm and PATCHes ONLY {trustIdpMfa:true}.
// Also pins the honest 503 SSO_NOT_CONFIGURED state (server switch off ≠ empty list).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { SsoProviders, TRUST_IDP_MFA_WARNING, validateIssuer } from "@/components/screens/admin/SsoProviders";
import { logout } from "@/lib/api";
import type { SsoProvider } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status, headers: { "Content-Type": "application/json" },
  });
}

const GOOGLE: SsoProvider = {
  id: "sp-1",
  tenantId: "t-1",
  issuer: "https://accounts.google.com",
  clientId: "abc.apps.googleusercontent.com",
  displayName: "Google Workspace",
  enabled: true,
  jitProvisioning: true,
  trustIdpMfa: false,
  createdAt: "2026-07-08T09:00:00Z",
  updatedAt: "2026-07-08T09:00:00Z",
};

const OKTA: SsoProvider = {
  ...GOOGLE,
  id: "sp-2",
  issuer: "https://example.okta.com",
  clientId: "okta-cid",
  displayName: "",
  enabled: false,
  jitProvisioning: false,
  trustIdpMfa: true,
};

// URL-routed fetch mock: ensureAuth (DEMO_MODE) needs POST /auth/login before the screen's
// GET /admin/sso/providers; PATCH calls are captured for body assertions.
function routedFetch(providers: SsoProvider[], overrides?: {
  list?: () => Response;
  patch?: (init?: RequestInit) => Response;
  create?: (init?: RequestInit) => Response;
}) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    if (u.includes("/auth/login")) {
      return jsonRes({ data: {
        accessToken: "at-test", refreshToken: "rt-test",
        user: { id: "u1", email: "admin@example.com", fullName: "Admin", isAdmin: true },
      } }, 200);
    }
    if (u.includes("/admin/sso/providers") && method === "GET") {
      return overrides?.list ? overrides.list() : jsonRes({ data: providers }, 200);
    }
    if (u.includes("/admin/sso/providers") && method === "POST") {
      return overrides?.create ? overrides.create(init) : jsonRes({ data: providers[0] }, 201);
    }
    if (u.includes("/admin/sso/providers/") && method === "PATCH") {
      return overrides?.patch ? overrides.patch(init) : jsonRes({ data: providers[0] }, 200);
    }
    if (u.includes("/admin/sso/providers/") && method === "DELETE") {
      return new Response(null, { status: 204 });
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("SsoProviders", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout(); // reset lib/api module token cache between tests
    window.localStorage.clear();
    window.sessionStorage.clear();
    // Authenticated seam: seed a token so ensureAuth() is a no-op — screens under test must not
    // depend on DEMO_MODE auto-login (fail-closed since the 401 tenant-flip fix).
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("renders providers with issuer, clientId and honest state badges", async () => {
    global.fetch = routedFetch([GOOGLE, OKTA]) as unknown as typeof fetch;
    render(<SsoProviders />);

    await waitFor(() => {
      expect(screen.getByText("Google Workspace")).toBeInTheDocument();
    });
    // Okta row has no displayName → falls back to the issuer host, never a fabricated name.
    expect(screen.getByText("example.okta.com")).toBeInTheDocument();
    expect(screen.getByText(/accounts\.google\.com/)).toBeInTheDocument();
    expect(screen.getByText("Đang bật")).toBeInTheDocument();
    expect(screen.getByText("Đã tắt")).toBeInTheDocument();
    expect(screen.getByText("JIT tạo user")).toBeInTheDocument();
    // trust badge only on the provider that actually has the flag
    expect(screen.getByText("Tin MFA IdP — bỏ qua MFA cục bộ")).toBeInTheDocument();
  });

  it("shows the trust_idp_mfa warning copy PERMANENTLY on every row (not hidden behind confirm)", async () => {
    global.fetch = routedFetch([GOOGLE, OKTA]) as unknown as typeof fetch;
    render(<SsoProviders />);

    await waitFor(() => {
      expect(screen.getByText("Google Workspace")).toBeInTheDocument();
    });
    // The exact security copy, one per provider row, visible without any interaction.
    const warnings = screen.getAllByText((_, el) =>
      (el?.textContent ?? "").includes(TRUST_IDP_MFA_WARNING) && el?.tagName === "P",
    );
    expect(warnings.length).toBeGreaterThanOrEqual(2);
    expect(TRUST_IDP_MFA_WARNING).toBe("Bật = tin MFA của IdP, BỎ QUA MFA cục bộ cho provider này.");
  });

  it("enabling trust_idp_mfa requires an explicit confirm, then PATCHes ONLY {trustIdpMfa:true}", async () => {
    const patchBodies: unknown[] = [];
    const fetchMock = routedFetch([GOOGLE], {
      patch: (init) => {
        patchBodies.push(JSON.parse(String(init?.body)));
        return jsonRes({ data: { ...GOOGLE, trustIdpMfa: true } }, 200);
      },
    });
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<SsoProviders />);

    await waitFor(() => {
      expect(screen.getByText("Google Workspace")).toBeInTheDocument();
    });

    // Flip the trust switch ON → NO PATCH yet, the confirm strip appears with the warning.
    fireEvent.click(screen.getByRole("switch", { name: /Tin MFA của IdP cho provider Google Workspace/ }));
    expect(patchBodies).toHaveLength(0);
    expect(screen.getByRole("button", { name: /Tôi hiểu — bật/ })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /Tôi hiểu — bật/ }));
    await waitFor(() => {
      expect(patchBodies).toHaveLength(1);
    });
    // Partial PATCH: trustIdpMfa only — the enabled flag must NOT ride along.
    expect(patchBodies[0]).toEqual({ trustIdpMfa: true });
  });

  it("toggling enabled PATCHes ONLY {enabled:false} without a confirm step", async () => {
    const patchBodies: unknown[] = [];
    const fetchMock = routedFetch([GOOGLE], {
      patch: (init) => {
        patchBodies.push(JSON.parse(String(init?.body)));
        return jsonRes({ data: { ...GOOGLE, enabled: false } }, 200);
      },
    });
    global.fetch = fetchMock as unknown as typeof fetch;
    render(<SsoProviders />);

    await waitFor(() => {
      expect(screen.getByText("Google Workspace")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("switch", { name: /Kích hoạt provider Google Workspace/ }));
    await waitFor(() => {
      expect(patchBodies).toHaveLength(1);
    });
    expect(patchBodies[0]).toEqual({ enabled: false });
  });

  // NOTE (verified live on :8090, 2026-07-08): the REAL backend's GET list answers 200 even when
  // SSO is unconfigured — only Create + /start//callback gate 503 (sso_service.go). This test
  // pins the DEFENSIVE list-path handling (if the BE ever gates list too, the screen must show
  // the server-switch banner, not a lying empty list). The live 503 path is the create form.
  it("503 SSO_NOT_CONFIGURED → honest server-switch banner, NOT a fake empty list", async () => {
    global.fetch = routedFetch([], {
      list: () => jsonRes({ error: "SSO/OIDC chưa được cấu hình trên hệ thống", code: "SSO_NOT_CONFIGURED" }, 503),
    }) as unknown as typeof fetch;
    render(<SsoProviders />);

    await waitFor(() => {
      expect(screen.getByText("SSO/OIDC chưa được cấu hình trên hệ thống")).toBeInTheDocument();
    });
    expect(screen.getByText(/SSO_SECRET_ENC_KEY/)).toBeInTheDocument();
    // The "no providers yet" empty state must NOT show — that would be a lie.
    expect(screen.queryByText("Chưa có SSO provider nào")).not.toBeInTheDocument();
    // Creating would 503 too — the button is disabled, honestly.
    expect(screen.getByRole("button", { name: /Thêm provider/ })).toBeDisabled();
  });

  it("Thêm provider opens a modal: an http issuer is refused client-side, a valid form POSTs once and closes", async () => {
    const posts: unknown[] = [];
    global.fetch = routedFetch([], {
      create: (init) => {
        posts.push(JSON.parse(String(init?.body)));
        return jsonRes({ data: GOOGLE }, 201);
      },
    }) as unknown as typeof fetch;
    render(<SsoProviders />);
    await screen.findByText("Chưa có SSO provider nào");

    fireEvent.click(screen.getByRole("button", { name: /Thêm provider/ }));
    const dialog = await screen.findByRole("dialog", { name: "Thêm SSO provider" });
    fireEvent.change(within(dialog).getByLabelText(/Issuer/), { target: { value: "http://insecure.example.com" } });
    fireEvent.change(within(dialog).getByLabelText(/Client ID/), { target: { value: "cid" } });
    fireEvent.change(within(dialog).getByLabelText(/Client secret/), { target: { value: "s3cret" } });
    // trust_idp_mfa is NOT settable at create time (BE: second decision) — no such control here.
    expect(within(dialog).queryByRole("switch")).not.toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo provider" }));
    expect(await within(dialog).findByText(/Issuer phải dùng https/)).toBeInTheDocument();
    expect(posts).toHaveLength(0);

    fireEvent.change(within(dialog).getByLabelText(/Issuer/), { target: { value: "https://accounts.google.com" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo provider" }));
    await waitFor(() => expect(posts).toHaveLength(1));
    expect(posts[0]).toEqual({
      issuer: "https://accounts.google.com", clientId: "cid", clientSecret: "s3cret", jitProvisioning: false,
    });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("missing Client ID / secret are flagged on those fields (aria-invalid + described error), first one focused", async () => {
    const posts: unknown[] = [];
    global.fetch = routedFetch([], {
      create: (init) => {
        posts.push(JSON.parse(String(init?.body)));
        return jsonRes({ data: GOOGLE }, 201);
      },
    }) as unknown as typeof fetch;
    render(<SsoProviders />);
    await screen.findByText("Chưa có SSO provider nào");
    fireEvent.click(screen.getByRole("button", { name: /Thêm provider/ }));
    const dialog = await screen.findByRole("dialog", { name: "Thêm SSO provider" });

    fireEvent.change(within(dialog).getByLabelText(/Issuer/), { target: { value: "https://accounts.google.com" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo provider" }));

    const cid = within(dialog).getByLabelText(/Client ID/);
    const secret = within(dialog).getByLabelText(/Client secret/);
    await waitFor(() => expect(cid).toHaveAttribute("aria-invalid", "true"));
    expect(cid).toHaveAccessibleDescription("Nhập Client ID.");
    expect(secret).toHaveAttribute("aria-invalid", "true");
    expect(secret).toHaveAccessibleDescription("Nhập Client secret.");
    expect(cid).toHaveFocus();
    expect(posts).toHaveLength(0);

    // Issuer + both missing → all three flagged at once, focus on the first (Issuer).
    fireEvent.change(within(dialog).getByLabelText(/Issuer/), { target: { value: "" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo provider" }));
    await waitFor(() => expect(within(dialog).getByLabelText(/Issuer/)).toHaveFocus());
    expect(within(dialog).getByLabelText(/Issuer/)).toHaveAttribute("aria-invalid", "true");
    expect(cid).toHaveAttribute("aria-invalid", "true");
  });

  it("delete asks for confirm and states the identity-revocation consequence", async () => {
    global.fetch = routedFetch([GOOGLE]) as unknown as typeof fetch;
    render(<SsoProviders />);

    await waitFor(() => {
      expect(screen.getByText("Google Workspace")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: /Xoá provider Google Workspace/ }));
    expect(screen.getByText(/identity bị thu hồi/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Xoá vĩnh viễn/ })).toBeInTheDocument();
  });
});

describe("validateIssuer", () => {
  it("accepts an absolute https URL", () => {
    expect(validateIssuer("https://accounts.google.com")).toBeNull();
    expect(validateIssuer("  https://login.microsoftonline.com/tid/v2.0  ")).toBeNull();
  });

  it("rejects empty, non-URL, and http issuers (mirrors BE SSO_PROVIDER_ISSUER_INVALID)", () => {
    expect(validateIssuer("")).toMatch(/Nhập issuer/);
    expect(validateIssuer("not a url")).toMatch(/https/);
    expect(validateIssuer("http://insecure.example.com")).toMatch(/https/);
  });
});
