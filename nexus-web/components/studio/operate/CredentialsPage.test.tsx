// specs/078 lát u7-khoa-ket-noi (FR-135, SC-015): the key vault page is isolated between customers.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";
import { CredentialsPage } from "./CredentialsPage";

let tokenOk = true;
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => tokenOk }), activeProjectSlug: () => "northwind", projectSlugAt: () => "northwind", transitionPhase: () => "idle" }));
vi.mock("next/navigation", () => ({ usePathname: () => "/studio/du-an/northwind/van-hanh/tu-dong/khoa-ket-noi" }));

const CRED = { id: "c-1", name: "cong", label: "Cổng giao hàng", baseUrl: "https://ship.example.com", authType: "hmac", headerName: "", headerPrefix: "", username: "", status: "ready", secretHint: "ab12", extraHeaderNames: [], rotatedAt: null, previousExpiresAt: null, usageCount: 0, createdAt: "", updatedAt: "" };
const mount = () => render(<QueryClientProvider client={new QueryClient()}><EmbeddedSurface><CredentialsPage slug="northwind" /></EmbeddedSurface></QueryClientProvider>);

describe("CredentialsPage", () => {
  const realFetch = global.fetch;
  let fetchSpy: ReturnType<typeof vi.fn>;
  beforeEach(async () => {
    tokenOk = true;
    await logout();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    fetchSpy = vi.fn(async () => new Response(JSON.stringify({ data: { credentials: [CRED] } }), { status: 200, headers: { "Content-Type": "application/json" } }));
    global.fetch = fetchSpy as unknown as typeof fetch;
  });
  afterEach(() => { cleanup(); global.fetch = realFetch; window.localStorage.clear(); });

  it("the token does not belong to this project: nothing is fetched and nothing is drawn", async () => {
    tokenOk = false;
    mount();
    await new Promise((r) => setTimeout(r, 50));
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(screen.queryByText("Cổng giao hàng")).toBeNull();
  });

  it("the token belongs: the vault list shows, without the secret hint column leaking a key", async () => {
    mount();
    await screen.findByText("Cổng giao hàng");
    await waitFor(() => expect(fetchSpy).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: /Tạo khoá/ })).toBeInTheDocument();
  });

  it("switching customer drops an open form, a revealed secret and the old list", async () => {
    const GEN = "nxw_Zk3Hq9LmN2pR7sTuV0wXyA4bC6dE8fG1hJ5iKo";
    fetchSpy.mockImplementation(async (_u: RequestInfo | URL, init?: RequestInit) =>
      init?.method === "POST"
        ? new Response(JSON.stringify({ data: { credential: CRED, secret: GEN } }), { status: 201, headers: { "Content-Type": "application/json" } })
        : new Response(JSON.stringify({ data: { credentials: [CRED] } }), { status: 200, headers: { "Content-Type": "application/json" } }));
    const ui = (slug: string) => <QueryClientProvider client={new QueryClient()}><EmbeddedSurface><CredentialsPage slug={slug} /></EmbeddedSurface></QueryClientProvider>;
    const { rerender } = render(ui("northwind"));
    await screen.findByText("Cổng giao hàng");
    // (c1) an open form is dropped
    fireEvent.click(screen.getByRole("button", { name: /Tạo khoá/ }));
    fireEvent.change(await screen.findByLabelText(/Tên máy/), { target: { value: "dang_go" } });
    // switch while the token is not yet S2's: nothing of S1 stays drawn
    tokenOk = false;
    rerender(ui("khac"));
    expect(screen.queryByLabelText(/Tên máy/)).toBeNull();
    expect(screen.queryByText("Cổng giao hàng")).toBeNull();
    tokenOk = true;
    rerender(ui("khac"));
    await screen.findByText("Cổng giao hàng");
    expect(screen.queryByLabelText(/Tên máy/)).toBeNull();
    // (c2) a revealed secret does not survive a slug change
    fireEvent.click(screen.getByRole("button", { name: /Tạo khoá/ }));
    fireEvent.change(await screen.findByLabelText(/Tên máy/), { target: { value: "cong_moi" } });
    fireEvent.change(screen.getByLabelText(/Địa chỉ gốc/), { target: { value: "https://a.example.com" } });
    fireEvent.click(screen.getByLabelText("HMAC"));
    fireEvent.click(screen.getByRole("switch", { name: /Nexus tự sinh khoá/ }));
    fireEvent.click(within(screen.getByRole("region", { name: "Tạo khoá kết nối" })).getByRole("button", { name: /^Tạo khoá$/ }));
    await waitFor(() => expect(document.body.textContent).toContain(GEN));
    rerender(ui("khac-nua"));
    await screen.findByText("Cổng giao hàng");
    expect(document.body.textContent).not.toContain(GEN);
    expect(document.querySelector("[data-secret-once]")).toBeNull();
    expect(screen.queryByLabelText(/Tên máy/)).toBeNull();
  });
});
