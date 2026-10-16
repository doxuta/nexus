// specs/073 kh1b1 T171 (FR-025b/c, FR-085): one project's local state and cache never reach another context.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useEffect, useState, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

let pathname = "/studio/du-an/a/dung/du-lieu";
const router = { push: vi.fn(), replace: vi.fn() };
vi.mock("next/navigation", () => ({ usePathname: () => pathname, useRouter: () => router, useSearchParams: () => new URLSearchParams() }));
vi.mock("@/components/shell/ModeSwitch", () => ({ useFacePaths: () => ({ appHref: "/" }) }));
vi.mock("./DraftBar", () => ({ DraftBar: () => null })); // the delivery bar does its own GET /admin/publish/state; this suite counts session calls only
vi.mock("@/lib/studioContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/studioContext")>()),
  useStudioIdentity: () => ({ status: "ready", me: { workspace: { tenantSlug: pathname.split("/")[3], tenantName: "Khách" } }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
}));

import * as api from "@/lib/api";
import * as studio from "@/lib/studioContext";
import * as workspace from "@/lib/workspace";
import { WORKSPACE_ENDED_EVENT } from "@/lib/workspace";
import { beginTransition, endTransition } from "@/lib/studioContext";
import { ProjectFrame } from "./ProjectFrame";
import { StudioSessionBanner } from "./StudioSessionBanner";

function Draft() {
  const [v, setV] = useState("");
  return <input aria-label="ô đang gõ" value={v} onChange={(e) => setV(e.target.value)} />;
}
const qc = new QueryClient();
const frame = (slug: string, body: ReactNode = <Draft />) => (
  <QueryClientProvider client={qc}>
    <ProjectFrame slug={slug}>{body}</ProjectFrame>
  </QueryClientProvider>
);

const assign = vi.fn();
const realLocation = window.location;
beforeEach(() => {
  pathname = "/studio/du-an/a/dung/du-lieu";
  vi.clearAllMocks();
  Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, pathname } });
});
afterEach(() => {
  endTransition();
  Object.defineProperty(window, "location", { configurable: true, value: realLocation });
});

describe("ProjectFrame isolation", () => {
  it("changing the project drops the surface's local state", () => {
    const { rerender } = render(frame("a"));
    fireEvent.change(screen.getByLabelText("ô đang gõ"), { target: { value: "dở dang" } });
    expect(screen.getByLabelText("ô đang gõ")).toHaveValue("dở dang");
    pathname = "/studio/du-an/b/dung/du-lieu";
    rerender(frame("b"));
    expect(screen.getByLabelText("ô đang gõ")).toHaveValue("");
  });

  it("changing the page inside one project keeps what was typed", () => {
    const { rerender } = render(frame("a"));
    fireEvent.change(screen.getByLabelText("ô đang gõ"), { target: { value: "giữ" } });
    pathname = "/studio/du-an/a/dung/man-hinh";
    rerender(frame("a"));
    expect(screen.getByLabelText("ô đang gõ")).toHaveValue("giữ");
  });

  it("while a context switch is in flight the surface is unmounted: spinner only, no builder", () => {
    render(frame("a"));
    expect(screen.getByLabelText("ô đang gõ")).toBeInTheDocument();
    act(() => beginTransition());
    expect(screen.queryByLabelText("ô đang gõ")).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent("Đang chuyển…");
    expect(screen.queryByRole("navigation", { name: "Các bước dựng" })).not.toBeInTheDocument();
    act(() => endTransition());
    expect(screen.getByLabelText("ô đang gõ")).toBeInTheDocument();
  });

  it("the session ending elsewhere: freeze, cancelQueries then clear, then a full page load, never the router", async () => {
    const order: string[] = [];
    vi.spyOn(qc, "cancelQueries").mockImplementation(async () => {
      order.push("cancel");
    });
    vi.spyOn(qc, "clear").mockImplementation(() => {
      order.push("clear");
    });
    assign.mockImplementation(() => order.push("assign"));
    // The strip under the header owns this event (StudioShell mounts it next to every frame).
    render(
      <>
        <QueryClientProvider client={qc}>
          <StudioSessionBanner />
        </QueryClientProvider>
        {frame("a")}
      </>,
    );
    await act(async () => {
      window.dispatchEvent(new CustomEvent(WORKSPACE_ENDED_EVENT));
    });
    expect(order).toEqual(["cancel", "clear", "assign"]);
    expect(assign).toHaveBeenCalledTimes(1); // one owner of the event: the banner, not the frame as well
    expect(assign).toHaveBeenCalledWith("/studio");
    expect(router.push).not.toHaveBeenCalled();
    expect(router.replace).not.toHaveBeenCalled();
    expect(screen.queryByLabelText("ô đang gõ")).not.toBeInTheDocument();
  });
});

// ── ph1a T432 (FR-085): the real session layer under a mounted builder ──
// A fake builder that does what real ones do (an autosave on a timer, a request already in the air that comes back 401
// and would be retried) sits in customer A's frame. Switching to B must reach the network with exactly the close of A
// and the open of B, and the builder must be gone before the first of them.
describe("ProjectFrame under a real context switch (T432)", () => {
  type Seen = { method: string; path: string; auth: string | undefined };
  let seen: Seen[];
  let release: (name: string) => void;
  let gates: Record<string, Promise<void>>;
  let dataHolds: Array<(r: Response) => void>;
  const FIRST_B = "/studio/du-an/b/dung/du-lieu";
  const reload = vi.fn();
  const res = (status: number, body?: unknown) =>
    new Response(body === undefined ? null : JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

  function Builder() {
    // The autosave: every 15 ms while mounted. A builder that survives the freeze shows up as an extra call.
    useEffect(() => {
      const timer = window.setInterval(() => void api.listEntities().catch(() => undefined), 15);
      return () => window.clearInterval(timer);
    }, []);
    return <input aria-label="ô đang gõ" defaultValue="" />;
  }

  beforeEach(() => {
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "HOME-TOK");
    window.localStorage.setItem("nexus_refresh", "HOME-REFRESH");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u-admin", fullName: "Admin", email: "admin@example.com", isAdmin: true }));
    workspace.endWorkspaceLeave();
    workspace.setWorkspace({
      accessToken: "WS-a",
      expiresAt: "2099-01-01T00:00:00Z",
      homeUserId: "u-admin",
      session: { id: "ws-a", tenantId: "t-a", tenantSlug: "a", tenantName: "Khách A", openedAt: "2098-01-01T00:00:00Z", expiresAt: "2099-01-01T08:00:00Z" },
    });
    Object.defineProperty(window, "location", { configurable: true, value: { ...realLocation, assign, reload, pathname } });
    reload.mockClear();
    seen = [];
    dataHolds = [];
    const open: Record<string, () => void> = {};
    gates = {};
    for (const k of ["close", "open"]) gates[k] = new Promise<void>((r) => (open[k] = r));
    release = (k) => open[k]();
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      const path = String(url).replace(/^https?:\/\/[^/]+\/api\/v1/, "").split("?")[0];
      const method = init?.method ?? "GET";
      seen.push({ method, path, auth: (init?.headers as Record<string, string> | undefined)?.Authorization });
      if (method === "DELETE" && path === "/admin/workspace-sessions/current") {
        await gates.close;
        return res(204);
      }
      if (method === "POST" && path === "/admin/workspace-sessions") {
        await gates.open;
        if (refuseOpen) return res(404, { error: "x", code: "TENANT_NOT_FOUND" });
        return res(201, { data: { accessToken: "WS-b", expiresAt: "2099-01-01T00:00:00Z", session: { id: "ws-b", tenantId: "t-b", tenantSlug: "b", tenantName: "Khách B", openedAt: "", expiresAt: "2099-01-01T08:00:00Z" } } });
      }
      if (path === "/metadata/entities") {
        // The FIRST data request stays in the air (it will come back 401); the autosave ones return at once.
        if (seen.filter((s) => s.path === "/metadata/entities").length === 1) return new Promise<Response>((r) => dataHolds.push(r));
        return res(200, { data: [] });
      }
      throw new Error(`unexpected fetch: ${method} ${path}`);
    }) as unknown as typeof fetch;
    refuseOpen = false;
  });
  let refuseOpen = false;

  const mountA = () => render(frame("a", <Builder />));
  const switchTo = (confirmLeave = () => true) => studio.switchProject("b", FIRST_B, { qc, confirmLeave });
  // publish/state is the header's own read (no retry since specs/076 FR-130: it settles at once, here as an unexpected-fetch error); it is not a session call.
  const sessionCalls = () => seen.filter((s) => s.path !== "/metadata/entities" && s.path !== "/ai/health" && s.path !== "/admin/publish/state");

  it("the builder is unmounted before the first call, and only the close of A and the open of B are sent — even when an in-flight request comes back 401", async () => {
    mountA();
    await waitFor(() => expect(dataHolds).toHaveLength(1)); // a request is in the air
    let done!: Promise<void>;
    act(() => {
      done = switchTo();
    });
    expect(screen.queryByLabelText("ô đang gõ")).not.toBeInTheDocument(); // gone synchronously, before any await
    await new Promise((r) => setTimeout(r, 10));
    const before = seen.length;
    await new Promise((r) => setTimeout(r, 80)); // five autosave ticks would fire here if the builder were still mounted
    expect(seen.slice(before).map((s) => `${s.method} ${s.path}`)).toEqual([]);
    // the request that was already in the air now answers 401 → req() would call recover401 and resend with another token
    dataHolds[0](res(401, { error: "x", code: "UNAUTHORIZED" }));
    await new Promise((r) => setTimeout(r, 30));
    release("close");
    release("open");
    await act(async () => {
      await done;
    });
    expect(sessionCalls().map((s) => `${s.method} ${s.path}`)).toEqual(["DELETE /admin/workspace-sessions/current", "POST /admin/workspace-sessions"]);
    expect(sessionCalls()[0].auth).toBe("Bearer WS-a");
    expect(sessionCalls()[1].auth).toBe("Bearer HOME-TOK");
    // from the freeze on: nothing but the two session calls (the one data request sent before is the one in the air)
    expect(seen.slice(seen.indexOf(sessionCalls()[0])).map((s) => s.path)).toEqual(["/admin/workspace-sessions/current", "/admin/workspace-sessions"]);
    expect(seen.filter((s) => s.path.startsWith("/auth/"))).toEqual([]); // no refresh, no /auth/me
    expect(assign).toHaveBeenCalledWith(FIRST_B);
  });

  it("exit: the builder is gone before the close, and a request in the air that comes back 401 AFTER the session is forgotten is not recovered as the home login", async () => {
    mountA();
    await waitFor(() => expect(dataHolds).toHaveLength(1));
    let done!: Promise<void>;
    act(() => {
      done = studio.exitProject("/studio", { qc, confirmLeave: () => true });
    });
    expect(screen.queryByLabelText("ô đang gõ")).not.toBeInTheDocument();
    expect(workspace.isWorkspaceLeaving()).toBe(true); // latched synchronously, before the close
    release("close");
    await act(async () => {
      await done;
    });
    expect(workspace.getWorkspace()).toBeNull(); // the session is forgotten: without the latch the home token would answer now
    dataHolds[0](res(401, { error: "x", code: "UNAUTHORIZED" }));
    await new Promise((r) => setTimeout(r, 40));
    expect(seen.slice(seen.findIndex((s) => s.method === "DELETE")).map((s) => `${s.method} ${s.path}`)).toEqual(["DELETE /admin/workspace-sessions/current"]);
    expect(seen.filter((s) => s.path.startsWith("/auth/"))).toEqual([]); // no refresh, no resend
    expect(workspace.isWorkspaceLeaving()).toBe(true);
    expect(assign).toHaveBeenCalledWith("/studio");
  });

  it("'leave without saving' refused: zero calls, the builder stays, session A stays", async () => {
    mountA();
    await waitFor(() => expect(dataHolds).toHaveLength(1));
    await act(async () => {
      await switchTo(() => false);
    });
    expect(screen.getByLabelText("ô đang gõ")).toBeInTheDocument();
    expect(sessionCalls()).toEqual([]);
    expect(workspace.getWorkspace()?.session.tenantSlug).toBe("a");
  });

  it("opening B fails: the frame stays on the failed screen, the old builder is not mounted again, both ways out load the page afresh", async () => {
    refuseOpen = true;
    mountA();
    await waitFor(() => expect(dataHolds).toHaveLength(1));
    release("close");
    release("open");
    await act(async () => {
      await switchTo().catch(() => undefined);
    });
    expect(screen.queryByLabelText("ô đang gõ")).not.toBeInTheDocument();
    expect(screen.getByText(/Chưa đổi được dự án đang dựng/)).toBeInTheDocument();
    expect(assign).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Thử lại" })).toHaveFocus(); // the clicked button is gone: focus must not fall to <body>
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(reload).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Về danh sách dự án" }));
    expect(assign).toHaveBeenCalledWith("/studio");
    expect(router.push).not.toHaveBeenCalled();
  });
});
