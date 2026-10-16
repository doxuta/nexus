// Embedded (Studio work surface) form of SsoProviders: specs/078 T045. The admin form stays in SsoProviders.test.tsx.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { SsoProviders } from "@/components/screens/admin/SsoProviders";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";
import type { SsoProvider } from "@/lib/admin";

const jsonRes = (body: unknown, status: number) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const GOOGLE: SsoProvider = {
  id: "sp-1", tenantId: "t-1", issuer: "https://accounts.google.com", clientId: "abc", displayName: "Google Workspace",
  enabled: true, jitProvisioning: true, trustIdpMfa: false, createdAt: "2026-07-08T09:00:00Z", updatedAt: "2026-07-08T09:00:00Z",
};

const DIALOG = '[role="dialog"], [role="alertdialog"]';
const calls: { url: string; method: string; body?: string }[] = [];
function mockFetch() {
  calls.length = 0;
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ url: u, method, body: init?.body as string | undefined });
    if (u.includes("/admin/sso/providers/") && method === "DELETE") return new Response(null, { status: 204 });
    if (u.includes("/admin/sso/providers")) return jsonRes({ data: method === "GET" ? [GOOGLE] : GOOGLE }, method === "POST" ? 201 : 200);
    return jsonRes({ error: "unmocked", code: "TEST_UNMOCKED" }, 500);
  }) as unknown as typeof fetch;
}
const renderEmbedded = () =>
  render(
    <EmbeddedSurface>
      <SsoProviders />
    </EmbeddedSurface>,
  );

describe("SsoProviders embedded", () => {
  const original = global.fetch;
  beforeEach(async () => {
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    mockFetch();
  });
  afterEach(() => {
    global.fetch = original;
    window.localStorage.clear();
  });

  it("opens the create form in the page (no dialog), secret masked and not autofilled", async () => {
    renderEmbedded();
    await screen.findByText("Google Workspace");
    fireEvent.click(screen.getByRole("button", { name: "Thêm cách đăng nhập" }));
    expect(document.querySelector(DIALOG)).toBeNull();
    const secret = screen.getByLabelText(/Client Secret/i, { selector: "input" });
    expect(secret).toHaveAttribute("type", "password");
    expect(secret).toHaveAttribute("autocomplete", "new-password");
  });

  it("drops the secret from the page after a successful save", async () => {
    renderEmbedded();
    await screen.findByText("Google Workspace");
    fireEvent.click(screen.getByRole("button", { name: "Thêm cách đăng nhập" }));
    fireEvent.change(screen.getByLabelText(/Địa chỉ nhà cung cấp/i, { selector: "input" }), { target: { value: "https://idp.example.com" } });
    fireEvent.change(screen.getByLabelText(/Client ID/i, { selector: "input" }), { target: { value: "cid" } });
    fireEvent.change(screen.getByLabelText(/Client Secret/i, { selector: "input" }), { target: { value: "s3cret-value" } });
    fireEvent.submit(document.getElementById("sso-create-form") as HTMLFormElement);
    await waitFor(() => expect(calls.some((c) => c.method === "POST")).toBe(true));
    await waitFor(() => expect(screen.queryByDisplayValue("s3cret-value")).toBeNull());
  });

  it("deletes through an in-page confirm, not a dialog", async () => {
    renderEmbedded();
    await screen.findByText("Google Workspace");
    fireEvent.click(screen.getByRole("button", { name: /Xoá cách đăng nhập Google Workspace/ }));
    expect(document.querySelector(DIALOG)).toBeNull();
    const confirm = await screen.findByRole("button", { name: "Xoá vĩnh viễn" });
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    fireEvent.click(confirm);
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE")).toBe(true));
  });

  it("enabling MFA trust asks in the page and PATCHes only after confirm", async () => {
    renderEmbedded();
    await screen.findByText("Google Workspace");
    const toggles = screen.getAllByRole("switch");
    fireEvent.click(toggles[1]);
    expect(document.querySelector(DIALOG)).toBeNull();
    expect(calls.some((c) => c.method === "PATCH")).toBe(false);
    fireEvent.click(await screen.findByRole("button", { name: "Tôi hiểu — bật" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PATCH")?.body).toBe(JSON.stringify({ trustIdpMfa: true })));
  });

  it("uses plain status text instead of badges", async () => {
    renderEmbedded();
    const row = (await screen.findByText("Google Workspace")).closest("tr") as HTMLElement;
    expect(row.querySelector(".slds-badge")).toBeNull();
    expect(within(row).getByText("Đang bật")).toBeInTheDocument();
  });
});
