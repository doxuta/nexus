// specs/073 ph1b T424 / T434 (FR-082, FR-025c, FR-085, US8 AC5): the "Đang dựng cho" strip of the Studio.
// It reads the real session layer (lib/workspace, lib/api, lib/studioContext); only the network and the location are faked.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const nav = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn(), confirm: true }));
vi.mock("next/navigation", () => ({ useRouter: () => ({ push: nav.push, replace: nav.replace }), usePathname: () => "/studio/du-an/x/dung/du-lieu" }));
vi.mock("@/lib/navigation-blocker", () => ({ useConfirmLeave: () => () => nav.confirm }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { info: vi.fn(), success: vi.fn(), error: vi.fn(), warning: vi.fn() } }));

import { toast } from "@/components/ui/Toaster";
import { endTransition, transitionPhase } from "@/lib/studioContext";
import { WORKSPACE_ENDED_EVENT, clearWorkspace, endWorkspaceLeave, setWorkspace, takeWorkspaceEndedNotice } from "@/lib/workspace";
import { StudioSessionBanner } from "./StudioSessionBanner";

const qc = new QueryClient();
const banner = () => (
  <QueryClientProvider client={qc}>
    <StudioSessionBanner />
  </QueryClientProvider>
);
const SESSION = {
  id: "ws-1", tenantId: "t-x", tenantSlug: "x", tenantName: "Khách X",
  openedAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 8 * 3600_000).toISOString(),
};
const enter = () =>
  setWorkspace({ accessToken: "WS-TOK", expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(), homeUserId: "u-admin", session: SESSION });

const assign = vi.fn();
const realLocation = window.location;
let seen: Array<{ method: string; path: string; auth: string | undefined }>;

beforeEach(() => {
  window.localStorage.clear();
  window.sessionStorage.clear();
  window.localStorage.setItem("nexus_token", "HOME-TOK");
  window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true }));
  clearWorkspace();
  endWorkspaceLeave();
  endTransition();
  nav.confirm = true;
  vi.clearAllMocks();
  seen = [];
  global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    seen.push({
      method: init?.method ?? "GET",
      path: String(url).replace(/^https?:\/\/[^/]+\/api\/v1/, "").split("?")[0],
      auth: (init?.headers as Record<string, string> | undefined)?.Authorization,
    });
    return new Response(null, { status: 204 });
  }) as unknown as typeof fetch;
  Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, pathname: "/studio/du-an/x/dung/du-lieu" } });
});
afterEach(() => {
  endTransition();
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

describe("StudioSessionBanner", () => {
  it("renders nothing without a session", () => {
    const { container } = render(banner());
    expect(container).toBeEmptyDOMElement();
  });

  it("names the customer and offers 'Về danh sách dự án' (not 'Về Nexus')", () => {
    enter();
    render(banner());
    expect(screen.getByRole("alert")).toHaveTextContent("Đang dựng cho: Khách X (x)");
    expect(screen.getByRole("button", { name: "Về danh sách dự án" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Về Nexus" })).toBeNull();
  });

  it("'Về danh sách dự án': freezes the surface, sends ONLY the close of the session, clears the cache, loads /studio", async () => {
    enter();
    const order: string[] = [];
    vi.spyOn(qc, "cancelQueries").mockImplementation(async () => {
      order.push("cancel");
    });
    vi.spyOn(qc, "clear").mockImplementation(() => {
      order.push("clear");
    });
    assign.mockImplementation(() => order.push("assign"));
    render(banner());
    fireEvent.click(screen.getByRole("button", { name: "Về danh sách dự án" }));
    expect(transitionPhase()).toBe("running"); // synchronous: the work surface goes before any await
    await waitFor(() => expect(assign).toHaveBeenCalledWith("/studio"));
    expect(seen).toEqual([{ method: "DELETE", path: "/admin/workspace-sessions/current", auth: "Bearer WS-TOK" }]);
    expect(order).toEqual(["cancel", "clear", "assign"]);
  });

  it("a refused 'leave without saving' touches nothing and the button works again", async () => {
    enter();
    nav.confirm = false;
    render(banner());
    const exit = screen.getByRole("button", { name: "Về danh sách dự án" });
    fireEvent.click(exit);
    await waitFor(() => expect(exit).not.toBeDisabled());
    expect(seen).toEqual([]);
    expect(assign).not.toHaveBeenCalled();
    expect(transitionPhase()).toBe("idle");
  });

  it("session ended in another tab: freeze, cancel, clear, full load of /studio (never the router); the landing page toasts once", async () => {
    enter();
    const order: string[] = [];
    vi.spyOn(qc, "cancelQueries").mockImplementation(async () => {
      order.push("cancel");
    });
    vi.spyOn(qc, "clear").mockImplementation(() => {
      order.push("clear");
    });
    assign.mockImplementation(() => order.push("assign"));
    const { unmount } = render(banner());
    // lib/api clears the state BEFORE it fires the event: the banner must still know whose session ended.
    await act(async () => {
      clearWorkspace();
      window.dispatchEvent(new CustomEvent(WORKSPACE_ENDED_EVENT));
    });
    expect(transitionPhase()).toBe("running");
    expect(order).toEqual(["cancel", "clear", "assign"]);
    expect(assign).toHaveBeenCalledTimes(1);
    expect(assign).toHaveBeenCalledWith("/studio");
    expect(nav.push).not.toHaveBeenCalled();
    expect(nav.replace).not.toHaveBeenCalled();
    unmount();
    endTransition();
    render(banner()); // the page the tab lands on
    expect(toast.warning).toHaveBeenCalledWith("Phiên dựng cho Khách X đã kết thúc — thao tác vừa rồi chưa được lưu");
    expect(takeWorkspaceEndedNotice()).toBeNull(); // one-shot
  });
});
