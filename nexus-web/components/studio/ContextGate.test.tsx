// specs/073 kh1a T142/T172/T173 + ph1a T421 (FR-020, FR-021, FR-026, FR-028, FR-085): the seven cases of the context
// gate, plus the ways it must stay closed. Since ph1a it runs on the REAL session layer (lib/studioContext, lib/api,
// lib/workspace); only the network and window.location are mocked, so what a click sends is what is asserted.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { NavigationBlockerProvider, useNavigationBlocker } from "@/lib/navigation-blocker";
import { setPreview } from "@/lib/previewSession";
import { endTransition, resetStudioContextForTests } from "@/lib/studioContext";
import { endWorkspaceLeave, setWorkspace } from "@/lib/workspace";

type Me = { isPlatformOperator?: boolean; workspace?: { tenantSlug: string; tenantName?: string } };
type Call = { method: string; path: string; auth: string | undefined };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const sessionOf = (slug: string, name = `Khách ${slug}`) => ({ id: `ws-${slug}`, tenantId: `t-${slug}`, tenantSlug: slug, tenantName: name, openedAt: "2098-01-01T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" });
const opened = (slug: string) => json({ data: { accessToken: `WS-${slug}`, expiresAt: "2099-01-01T00:00:00Z", session: { ...sessionOf(slug), resumed: false } } }, 201);

let calls: Call[];
let me: Me | (() => Response);
let route: Record<string, () => Response | Promise<Response>>;

function network() {
  calls = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const path = String(url).replace(/^https?:\/\/[^/]+\/api\/v1/, "").split("?")[0];
    const method = init?.method ?? "GET";
    calls.push({ method, path, auth: (init?.headers as Record<string, string> | undefined)?.Authorization });
    if (method === "GET" && path === "/auth/me") return typeof me === "function" ? me() : json({ data: { id: "u-admin", tenantId: "t", ...me } });
    const h = route[`${method} ${path}`];
    if (!h) throw new Error(`unexpected fetch: ${method} ${path}`);
    return h();
  }) as unknown as typeof fetch;
}
/** The tab holds a build session for `slug` (and /auth/me confirms it). */
function inSession(slug: string, name?: string) {
  setWorkspace({ accessToken: `WS-${slug}`, expiresAt: "2099-01-01T00:00:00Z", homeUserId: "u-admin", session: sessionOf(slug, name) });
  me = { isPlatformOperator: true, workspace: { tenantSlug: slug, tenantName: name ?? `Khách ${slug}` } };
}

import { ContextGate } from "./ContextGate";

const H = "northwind";
const first = (s: string) => `/studio/du-an/${s}/dung/du-lieu`;
const assign = vi.fn();
const realLocation = window.location;

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.localStorage.setItem("nexus_remember", "1");
  window.localStorage.setItem("nexus_token", "HOME-TOK");
  window.localStorage.setItem("nexus_tenant", H);
  window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true }));
  resetStudioContextForTests();
  endWorkspaceLeave();
  endTransition();
  assign.mockReset();
  Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, reload: vi.fn(), pathname: first("acme"), search: "" } });
  me = { isPlatformOperator: true };
  route = {};
  network();
});
afterEach(() => {
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

const ui = (props: { slug: string; ctx: "phien" | "nha"; next?: string }) => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const wrap = (children: ReactNode) => <QueryClientProvider client={qc}>{children}</QueryClientProvider>;
  return wrap(
    <ContextGate {...props}>
      <div>nội dung trang</div>
    </ContextGate>,
  );
};
const page = () => screen.queryByText("nội dung trang");
const wrote = () => calls.filter((c) => c.method !== "GET").map((c) => `${c.method} ${c.path}`);

describe("the seven cases (FR-020)", () => {
  it("1. S = H, no session: shows the page", async () => {
    render(ui({ slug: H, ctx: "phien" }));
    expect(await screen.findByText("nội dung trang")).toBeInTheDocument();
    expect(wrote()).toEqual([]);
  });

  it("2. S = H, tab is building for W: explains, offers ONE button, never closes the session on load; a click closes it and loads the workshop", async () => {
    inSession("acme", "Acme");
    route["DELETE /admin/workspace-sessions/current"] = () => new Response(null, { status: 204 });
    render(ui({ slug: H, ctx: "phien", next: first(H) }));
    expect(await screen.findByText("Tab này đang dựng cho Acme. Mở xưởng Nexus sẽ đóng phiên dựng.")).toBeInTheDocument();
    expect(page()).toBeNull();
    expect(screen.getAllByRole("button")).toHaveLength(1);
    expect(wrote()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Đóng phiên và mở xưởng Nexus" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(first(H)));
    expect(wrote()).toEqual(["DELETE /admin/workspace-sessions/current"]);
    expect(calls.find((c) => c.method === "DELETE")?.auth).toBe("Bearer WS-acme");
  });

  it("3. phien page, session is S: shows the page", async () => {
    inSession("acme");
    render(ui({ slug: "acme", ctx: "phien" }));
    expect(await screen.findByText("nội dung trang")).toBeInTheDocument();
  });

  it("4. phien page, session is another project: offers the switch; a click closes the old one and opens the new one, nothing else", async () => {
    inSession("other", "Công ty Khác");
    route["DELETE /admin/workspace-sessions/current"] = () => new Response(null, { status: 204 });
    route["POST /admin/workspace-sessions"] = () => opened("acme");
    render(ui({ slug: "acme", ctx: "phien" }));
    expect(await screen.findByText("Tab này đang dựng cho Công ty Khác.")).toBeInTheDocument();
    expect(page()).toBeNull();
    expect(wrote()).toEqual([]);
    fireEvent.click(screen.getByRole("button", { name: "Chuyển sang dự án acme" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(first("acme")));
    expect(wrote()).toEqual(["DELETE /admin/workspace-sessions/current", "POST /admin/workspace-sessions"]);
    expect(calls.filter((c) => c.method === "POST")[0].auth).toBe("Bearer HOME-TOK");
  });

  it("5. phien page, no session: asks, and only a click opens the project", async () => {
    route["POST /admin/workspace-sessions"] = () => opened("acme");
    render(ui({ slug: "acme", ctx: "phien" }));
    expect(await screen.findByText("Mở dự án acme để dựng.")).toBeInTheDocument();
    expect(page()).toBeNull();
    expect(wrote()).toEqual([]); // never on load
    fireEvent.click(screen.getByRole("button", { name: "Mở dự án" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(first("acme")));
    expect(wrote()).toEqual(["POST /admin/workspace-sessions"]);
  });

  it("5a. the user keeps their unsaved changes: nothing is sent and the button works again (a later click goes through)", async () => {
    route["POST /admin/workspace-sessions"] = () => opened("acme");
    let agree = false;
    const confirm = vi.fn(() => agree);
    vi.stubGlobal("confirm", confirm);
    function Dirty() {
      useNavigationBlocker().setBlocked(true);
      return null;
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <NavigationBlockerProvider>
          <Dirty />
          <ContextGate slug="acme" ctx="phien">
            <div>nội dung trang</div>
          </ContextGate>
        </NavigationBlockerProvider>
      </QueryClientProvider>,
    );
    const open = await screen.findByRole("button", { name: "Mở dự án" });
    fireEvent.click(open);
    await waitFor(() => expect(confirm).toHaveBeenCalledTimes(1));
    expect(wrote()).toEqual([]);
    await waitFor(() => expect(screen.getByRole("button", { name: "Mở dự án" })).toBeEnabled());
    agree = true;
    fireEvent.click(screen.getByRole("button", { name: "Mở dự án" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(first("acme")));
    expect(wrote()).toEqual(["POST /admin/workspace-sessions"]);
  });

  it("5b. 404 TENANT_NOT_FOUND on open: says the project is not found, links back to the list, the tab is not left in a half state", async () => {
    route["POST /admin/workspace-sessions"] = () => json({ error: "x", code: "TENANT_NOT_FOUND" }, 404);
    render(ui({ slug: "acme", ctx: "phien" }));
    fireEvent.click(await screen.findByRole("button", { name: "Mở dự án" }));
    expect(await screen.findByText("Không tìm thấy dự án acme, hoặc dự án đã ngưng.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Về danh sách dự án" })).toHaveAttribute("href", "/studio");
    expect(screen.queryByRole("button")).toBeNull();
    expect(assign).not.toHaveBeenCalled();
    expect(window.sessionStorage.getItem("nexus_workspace")).toBeNull();
  });

  it("5c. a failed open that is not a 404 keeps the button so the user can try again", async () => {
    route["POST /admin/workspace-sessions"] = () => json({ error: "Lỗi máy chủ", code: "INTERNAL" }, 500);
    render(ui({ slug: "acme", ctx: "phien" }));
    fireEvent.click(await screen.findByRole("button", { name: "Mở dự án" }));
    await waitFor(() => expect(wrote()).toEqual(["POST /admin/workspace-sessions"]));
    expect(await screen.findByRole("button", { name: "Mở dự án" })).not.toBeDisabled();
    expect(assign).not.toHaveBeenCalled();
  });

  it("6. nha page while a session is open: explains and offers the workshop; the click closes the session and loads the same page at home", async () => {
    inSession("acme", "Acme");
    route["DELETE /admin/workspace-sessions/current"] = () => new Response(null, { status: 204 });
    render(ui({ slug: "acme", ctx: "nha" }));
    expect(
      await screen.findByText("Việc này làm ở xưởng Nexus, bằng quyền người dựng Nexus. Phiên dựng cho Acme sẽ đóng; mọi thay đổi đã lưu vẫn còn."),
    ).toBeInTheDocument();
    expect(page()).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Chuyển sang xưởng Nexus" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(first("acme")));
    expect(wrote()).toEqual(["DELETE /admin/workspace-sessions/current"]);
  });

  it("7. nha page, no session: shows the page", async () => {
    render(ui({ slug: "acme", ctx: "nha" }));
    expect(await screen.findByText("nội dung trang")).toBeInTheDocument();
  });

  it("a block is one info notification and one primary button", async () => {
    inSession("other");
    const { container } = render(ui({ slug: "acme", ctx: "phien" }));
    await screen.findByRole("button");
    expect(container.querySelectorAll(".slds-scoped-notification")).toHaveLength(1);
    expect(container.querySelectorAll(".slds-button_brand")).toHaveLength(1);
  });
});

describe("next goes through safeNext (T142, FR-021)", () => {
  const bad = ["//evil.com", "https://evil.com", "/login", "/studio/du-an/other/dung/du-lieu", "/studio/du-an/acme/../other", "/studio/du-an/acmeX/y", "/studio/du-an/acme/%2e%2e/x"];
  it.each(bad)("%s becomes the first page of the target project", async (next) => {
    route["POST /admin/workspace-sessions"] = () => opened("acme");
    render(ui({ slug: "acme", ctx: "phien", next }));
    fireEvent.click(await screen.findByRole("button", { name: "Mở dự án" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(first("acme")));
    expect(assign).toHaveBeenCalledTimes(1);
  });

  it("a good next is kept", async () => {
    const next = "/studio/du-an/acme/dung/tu-dong?bang=orders";
    route["POST /admin/workspace-sessions"] = () => opened("acme");
    render(ui({ slug: "acme", ctx: "phien", next }));
    fireEvent.click(await screen.findByRole("button", { name: "Mở dự án" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith(next));
  });

  it("without a next prop the page the user typed is the target (and still goes through safeNext)", async () => {
    Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, pathname: "/studio/du-an/acme/dung/tu-dong", search: "?bang=orders" } });
    route["POST /admin/workspace-sessions"] = () => opened("acme");
    render(ui({ slug: "acme", ctx: "phien" }));
    fireEvent.click(await screen.findByRole("button", { name: "Mở dự án" }));
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/studio/du-an/acme/dung/tu-dong?bang=orders"));
  });
});

describe("closed when unsure (T172)", () => {
  it("case 0: a view-as-role tab is ended before anything is decided: the first call is the preview's own end, and nothing else carries a token", async () => {
    setPreview({ token: "PREV", sessionId: "p1", role: { id: "r", name: "vai" }, expiresAt: "2099-01-01T00:00:00Z", returnTo: "/studio" });
    route["DELETE /preview-sessions/current"] = () => new Response(null, { status: 204 });
    render(ui({ slug: H, ctx: "phien" }));
    await waitFor(() => expect(calls.length).toBeGreaterThan(0));
    expect(calls[0]).toMatchObject({ method: "DELETE", path: "/preview-sessions/current", auth: "Bearer PREV" });
    expect(calls.slice(1).every((c) => !c.auth)).toBe(true); // the latch: no builder token, no preview token
    expect(page()).toBeNull();
  });

  it("while /auth/me is loading the page is not shown", () => {
    me = () => new Response(null);
    global.fetch = vi.fn(() => new Promise<Response>(() => {})) as unknown as typeof fetch;
    render(ui({ slug: H, ctx: "phien" }));
    expect(page()).toBeNull();
    expect(screen.getByRole("status")).toBeInTheDocument();
  });

  it("when /auth/me fails the page is not shown and Thử lại asks again", async () => {
    me = () => json({ error: "x", code: "INTERNAL" }, 500);
    render(ui({ slug: H, ctx: "phien" }));
    const retry = await screen.findByRole("button", { name: "Thử lại" });
    expect(page()).toBeNull();
    const before = calls.filter((c) => c.path === "/auth/me").length;
    fireEvent.click(retry);
    await waitFor(() => expect(calls.filter((c) => c.path === "/auth/me").length).toBe(before + 1));
  });

  it("sessionStorage that cannot be read closes the gate (never 'Xưởng Nexus' by guess)", async () => {
    vi.spyOn(window, "sessionStorage", "get").mockReturnValue({
      ...window.sessionStorage,
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {},
      removeItem: () => {},
    } as unknown as Storage);
    render(ui({ slug: H, ctx: "phien" }));
    expect(await screen.findByText(/Chưa xác nhận được tab này đang ở dự án nào/)).toBeInTheDocument();
    expect(page()).toBeNull();
  });

  it("a user the server does not confirm as the operator at home gets no page", async () => {
    me = {};
    render(ui({ slug: H, ctx: "phien" }));
    expect(await screen.findByText("Chưa mở được dự án khách trong trình duyệt.")).toBeInTheDocument();
    expect(page()).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("slug from the URL is checked before anything else (T173, FR-028)", () => {
  it.each(["A", "a b", "a/../b", "a".repeat(65), "%2e%2e", ""])("%j: no page, no network at all", (slug) => {
    render(ui({ slug, ctx: "phien" }));
    expect(page()).toBeNull();
    expect(screen.getByText("Không tìm thấy dự án, hoặc dự án đã ngưng.")).toBeInTheDocument();
    expect(calls).toHaveLength(0);
  });
});
