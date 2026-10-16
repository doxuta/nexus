// TeamsPage (specs/074 lát f2-access-team-share, T216/T217): the four states, "+ Đội mới" as a form in the page,
// and both deletes asked in the page. Zero dialogs at every step; the customer's /admin/teams keeps its Modal
// (components/screens/admin/TeamsList.test.tsx).
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EmbeddedSurface } from "@/components/slds/EmbeddedSurface";
import { logout } from "@/lib/api";

vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/acme/dung/nguoi-dung-quyen/doi",
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn() }),
}));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-1", isAdmin: true }, isAuthenticated: true }) }));

import { TeamsPage } from "./TeamsPage";

const jsonRes = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
interface Call { method: string; url: string; body?: unknown }
const SALES = { id: "tm-1", tenantId: "t-1", name: "Kinh doanh" };
const USERS = [
  { id: "u-1", email: "admin@example.com", fullName: "Nexus Admin", status: "active" },
  { id: "u-2", email: "test@example.com", fullName: "Trần Sales Rep", status: "active" },
];

function routed(calls: Call[], opts: { teams?: unknown[]; teamsStatus?: number; members?: unknown[] } = {}) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ method, url: u, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (u.endsWith("/admin/teams") && method === "GET") return opts.teamsStatus ? jsonRes({ error: "Máy chủ bận", code: "X" }, opts.teamsStatus) : jsonRes({ data: { teams: opts.teams ?? [] } });
    if (u.endsWith("/admin/teams") && method === "POST") return jsonRes({ data: { team: { id: "tm-new", tenantId: "t-1", name: "Hỗ trợ" } } }, 201);
    if (u.includes("/admin/team-sets")) return jsonRes({ data: { teamSets: [] } });
    if (u.includes("/members") && method === "GET") return jsonRes({ data: { members: opts.members ?? [] } });
    if (u.includes("/members/") && method === "DELETE") return jsonRes({}, 204);
    if (u.endsWith("/admin/users")) return jsonRes({ data: { users: USERS } });
    if (u.endsWith("/admin/teams/tm-1") && method === "GET") return jsonRes({ data: { team: SALES } });
    if (u.endsWith("/admin/teams/tm-1") && method === "DELETE") return jsonRes({}, 204);
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}
const dialogs = () => document.body.querySelectorAll("[role=dialog],[role=alertdialog]").length;
const view = () => render(<EmbeddedSurface><TeamsPage slug="acme" /></EmbeddedSurface>);

describe("TeamsPage", () => {
  let originalFetch: typeof fetch;
  beforeEach(async () => {
    originalFetch = global.fetch;
    await logout();
    window.localStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });
  afterEach(() => {
    global.fetch = originalFetch;
    window.localStorage.clear();
  });

  it("empty: says 'Chưa có đội nào' with '+ Đội đầu tiên', which opens the form in the page and POSTs", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls) as unknown as typeof fetch;
    view();
    expect(await screen.findByText("Chưa có đội nào")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Người dùng & quyền/ })).toHaveAttribute("href", expect.stringContaining("/dung/nguoi-dung-quyen"));
    await userEvent.click(screen.getByRole("button", { name: "+ Đội đầu tiên" }));
    expect(dialogs()).toBe(0);
    await userEvent.type(screen.getByLabelText(/Tên đội/), "Hỗ trợ");
    await userEvent.click(screen.getByRole("button", { name: "Tạo đội" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toEqual({ name: "Hỗ trợ", parentId: null }));
    expect(dialogs()).toBe(0);
  });

  it("rows: '+ Đội mới' opens the form in the page, Esc closes it", async () => {
    global.fetch = routed([], { teams: [SALES] }) as unknown as typeof fetch;
    view();
    await userEvent.click(await screen.findByRole("button", { name: "+ Đội mới" }));
    const form = screen.getByRole("form", { name: "+ Đội mới" });
    expect(dialogs()).toBe(0);
    fireEvent.keyDown(form, { key: "Escape" });
    expect(screen.queryByRole("form", { name: "+ Đội mới" })).toBeNull();
  });

  it("error: shows the server message", async () => {
    global.fetch = routed([], { teamsStatus: 500 }) as unknown as typeof fetch;
    view();
    expect(await screen.findByText("Máy chủ bận")).toBeInTheDocument();
    expect(screen.queryByText("Chưa có đội nào")).toBeNull();
  });

  it("deleting a team asks in the page (no dialog) and then DELETEs", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls, { teams: [SALES] }) as unknown as typeof fetch;
    view();
    await userEvent.click(await screen.findByRole("button", { name: "Kinh doanh" }));
    await userEvent.click(await screen.findByRole("button", { name: "Xoá đội" }));
    expect(dialogs()).toBe(0);
    expect(screen.getByText(/Xoá đội “Kinh doanh”\?/)).toBeInTheDocument();
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Xoá hẳn đội" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/admin/teams/tm-1"))).toBe(true));
  });

  it("removing a member asks in the page (no dialog) and then DELETEs", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls, { teams: [SALES], members: [USERS[1]] }) as unknown as typeof fetch;
    view();
    await userEvent.click(await screen.findByRole("button", { name: "Kinh doanh" }));
    await userEvent.click(await screen.findByRole("button", { name: /Thêm thao tác cho/ }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Xoá khỏi đội" }));
    expect(dialogs()).toBe(0);
    expect(calls.some((c) => c.url.includes("/members/"))).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Bỏ khỏi đội" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/members/u-2"))).toBe(true));
  });
});

describe("Studio words", () => {
  it("every STUDIO_WORDS entry has a studio key", async () => {
    const { STUDIO_WORDS } = await import("@/components/screens/admin/TeamsList");
    const { default: dict } = await import("@/lib/i18n/vi");
    for (const w of STUDIO_WORDS) expect(dict).toHaveProperty([`studio.build.access.team.x.${w}`]);
  });
});
