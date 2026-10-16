// Component tests — /admin/users (UserList) on SLDS (specs/038 T048).
// Pins what an admin does on the screen: find a user, open the record and see the role, change it
// (PUT carries the new roleId), create a user from the modal (an unassigned role is sent as absent,
// not ""), and delete behind the prompt (anti-lock-out refusals surface, the record stays open).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { UserList } from "@/components/screens/admin/UserList";
import { logout } from "@/lib/api";
import type { AdminUser } from "@/lib/admin";

// The detail view asks who is looking (specs/065: only the builder sees the "Chủ khách" switch).
const session = vi.hoisted(() => ({ user: { id: "u-admin", email: "admin@example.com", isAdmin: true, isPlatformOperator: false } as Record<string, unknown> }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: session.user }) }));

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const ADMIN: AdminUser = { id: "u-admin", tenantId: "t-1", email: "admin@example.com", fullName: "Nexus Admin", status: "active", isAdmin: true };
const REP: AdminUser = { id: "u-rep", tenantId: "t-1", email: "test@example.com", fullName: "Trần Sales Rep", status: "active", isAdmin: false, roleId: "r-sales" };
const ROLES = [
  { id: "r-sales", name: "sales_staff", label: "Nhân viên kinh doanh" },
  { id: "r-mgr", name: "sales_manager", label: "Quản lý kinh doanh" },
];

interface Call { method: string; url: string; body?: unknown }

function routedFetch(calls: Call[], onDelete?: () => Response) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ method, url: u, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (u.endsWith("/admin/users") && method === "GET") return jsonRes({ data: { users: [ADMIN, REP] } }, 200);
    if (u.endsWith("/admin/roles") && method === "GET") return jsonRes({ data: { roles: ROLES } }, 200);
    if (u.endsWith("/admin/users/u-rep") && method === "GET") return jsonRes({ data: { user: REP } }, 200);
    if (u.endsWith("/admin/users/u-rep/user-manager") && method === "PUT") {
      const enabled = (JSON.parse(String(init?.body)) as { enabled: boolean }).enabled;
      return jsonRes({ data: { user: { ...REP, canManageUsers: enabled } } }, 200);
    }
    if (u.endsWith("/admin/users/u-rep") && method === "PUT") return jsonRes({ data: { user: REP } }, 200);
    if (u.endsWith("/admin/users/u-rep") && method === "DELETE") return onDelete ? onDelete() : new Response(null, { status: 204 });
    if (u.endsWith("/admin/users") && method === "POST") return jsonRes({ data: { user: { ...REP, id: "u-new" } } }, 201);
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("UserList — /admin/users", () => {
  let originalFetch: FetchFn;

  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    global.fetch = vi.fn(async () => new Response(null, { status: 204 })) as unknown as typeof fetch;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
    window.sessionStorage.clear();
  });

  it("lists users with their role, and the search narrows by name or email", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<UserList />);
    const grid = await screen.findByRole("grid", { name: "Danh sách người dùng" });
    await waitFor(() => expect(within(grid).getByText("Trần Sales Rep")).toBeInTheDocument());
    expect(within(grid).getByText("Nhân viên kinh doanh")).toBeInTheDocument();
    expect(within(grid).getByText("Quản trị viên")).toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText("Tìm theo tên, email…"), { target: { value: "test@" } });
    expect(within(grid).queryByText("Nexus Admin")).not.toBeInTheDocument();
    expect(within(grid).getByText("Trần Sales Rep")).toBeInTheDocument();
  });

  it("a user without a name (the API omits fullName) shows as unnamed and survives the search", async () => {
    const NAMELESS = { id: "u-x", tenantId: "t-1", email: "an.canhan@gmail.com", status: "active", isAdmin: true } as unknown as AdminUser;
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.endsWith("/admin/users")) return jsonRes({ data: { users: [REP, NAMELESS] } }, 200);
      return jsonRes({ data: { roles: ROLES } }, 200);
    }) as unknown as typeof fetch;
    render(<UserList />);
    const grid = await screen.findByRole("grid", { name: "Danh sách người dùng" });
    await waitFor(() => expect(within(grid).getByText("(chưa đặt tên)")).toBeInTheDocument());

    fireEvent.change(screen.getByPlaceholderText("Tìm theo tên, email…"), { target: { value: "an.canhan" } });
    expect(within(grid).getByText("(chưa đặt tên)")).toBeInTheDocument();
    expect(within(grid).queryByText("Trần Sales Rep")).not.toBeInTheDocument();
  });

  it("opens the record, shows the role, and saves a changed role", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<UserList />);
    fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));

    const heading = await screen.findByRole("heading", { level: 1 });
    expect(heading).toHaveTextContent("Trần Sales Rep");
    const role = (await screen.findByLabelText("Vai trò")) as HTMLSelectElement;
    expect(role.value).toBe("r-sales");

    fireEvent.change(role, { target: { value: "r-mgr" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    // Only what changed goes out (specs/065 FR-003) — not the whole form.
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ roleId: "r-mgr" }));
    // Saved → back on the list.
    await screen.findByRole("grid", { name: "Danh sách người dùng" });
  });

  it("choosing 'Không vai' sends roleId '' so the role is really cleared (was: undefined, dropped)", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<UserList />);
    fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
    const role = (await screen.findByLabelText("Vai trò")) as HTMLSelectElement;
    fireEvent.change(role, { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ roleId: "" }));
  });

  it("renaming only sends just fullName (no isAdmin / roleId / status → no logout, no alert)", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<UserList />);
    fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
    const name = (await screen.findByLabelText("Họ tên")) as HTMLInputElement;
    fireEvent.change(name, { target: { value: "Trần Rep Mới" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ fullName: "Trần Rep Mới" }));
  });

  it("saving with nothing changed makes no PUT", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<UserList />);
    fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
    await screen.findByLabelText("Vai trò");
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await screen.findByRole("grid", { name: "Danh sách người dùng" });
    expect(calls.some((c) => c.method === "PUT")).toBe(false);
  });

  it("the detail page carries a sessions card that lists and ends the person's device", async () => {
    const calls: Call[] = [];
    const base = routedFetch(calls);
    let sessions = [{ id: "s9", browser: "Firefox", os: "Windows", ip: "1.2.3.x", lastUsedAt: "2026-10-07T01:00:00Z", expiresAt: "2026-10-14T01:00:00Z" }];
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      const method = init?.method ?? "GET";
      if (u.endsWith("/admin/users/u-rep/sessions") && method === "GET") {
        calls.push({ method, url: u });
        return jsonRes({ data: sessions }, 200);
      }
      if (u.endsWith("/admin/users/u-rep/sessions/s9") && method === "DELETE") {
        calls.push({ method, url: u });
        sessions = [];
        return new Response(null, { status: 204 });
      }
      return base(url, init);
    }) as unknown as typeof fetch;
    render(<UserList />);
    fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
    await screen.findByText("Firefox · Windows");
    fireEvent.click(screen.getByRole("button", { name: "Đăng xuất Firefox · Windows" }));
    const dialog = await screen.findByRole("dialog", { name: "Đăng xuất thiết bị này?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Đăng xuất thiết bị" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/sessions/s9"))).toBe(true));
    // The admin never gets "log everyone out" here — that is the Profile card's own button.
    expect(screen.queryByRole("button", { name: "Đăng xuất mọi thiết bị" })).not.toBeInTheDocument();
  });

  it("creates a user from the modal; no role is sent as absent", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<UserList />);
    await screen.findByRole("grid", { name: "Danh sách người dùng" });
    fireEvent.click(screen.getByRole("button", { name: "Thêm người dùng" }));

    const dialog = await screen.findByRole("dialog", { name: "Thêm người dùng" });
    fireEvent.change(within(dialog).getByLabelText(/Họ tên/), { target: { value: "Lê Mới" } });
    fireEvent.change(within(dialog).getByLabelText(/Email/), { target: { value: "moi@example.com" } });
    fireEvent.change(within(dialog).getByLabelText(/Mật khẩu/), { target: { value: "Demo-Pass-2026!" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo người dùng" }));

    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toEqual({
      email: "moi@example.com", password: "Demo-Pass-2026!", fullName: "Lê Mới", status: "active", isAdmin: false,
    }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Thêm người dùng" })).not.toBeInTheDocument());
  });

  it("the row menu mints an invite link and shows it with the hand-over badge", async () => {
    const calls: Call[] = [];
    const base = routedFetch(calls);
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      if (String(url).endsWith("/admin/users/u-rep/reset-password-link")) {
        calls.push({ method: init?.method ?? "GET", url: String(url) });
        return jsonRes({ data: { resetLink: "http://fe/reset-password?token=abc", token: "abc", expiresAt: "2026-09-30T00:30:00Z", ttlMinutes: 30, emailQueued: false } }, 201);
      }
      return base(url, init);
    }) as unknown as typeof fetch;
    render(<UserList />);
    const trigger = await screen.findByRole("button", { name: "Thêm thao tác cho Trần Sales Rep" });
    trigger.focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(await screen.findByRole("menuitem", { name: "Tạo link đặt mật khẩu (mời)" }));

    const dialog = await screen.findByRole("dialog", { name: "Link đặt mật khẩu · lời mời" });
    await waitFor(() => expect(within(dialog).getByLabelText("Link đặt lại mật khẩu")).toHaveValue("http://fe/reset-password?token=abc"));
    expect(within(dialog).getByText("Đưa link thủ công")).toBeInTheDocument();
    expect(calls.filter((c) => c.url.endsWith("/reset-password-link"))).toHaveLength(1);
  });

  it("closing the invite dialog (Esc or Đóng) returns focus to the row's action menu button", async () => {
    const base = routedFetch([]);
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      if (String(url).endsWith("/admin/users/u-rep/reset-password-link")) {
        return jsonRes({ data: { resetLink: "http://fe/reset-password?token=abc", token: "abc", expiresAt: "2026-09-30T00:30:00Z", ttlMinutes: 30, emailQueued: false } }, 201);
      }
      return base(url, init);
    }) as unknown as typeof fetch;
    render(<UserList />);
    const trigger = await screen.findByRole("button", { name: "Thêm thao tác cho Trần Sales Rep" });

    const openInvite = async () => {
      trigger.focus();
      await userEvent.keyboard("{Enter}");
      await screen.findByRole("menu");
      await userEvent.keyboard("{ArrowDown}{Enter}"); // Mở · Tạo link đặt mật khẩu (mời)
      return screen.findByRole("dialog", { name: "Link đặt mật khẩu · lời mời" });
    };

    await openInvite();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(document.activeElement).toBe(trigger));

    const dialog = await openInvite();
    await userEvent.click(within(dialog).getByRole("button", { name: "Đóng" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(document.activeElement).toBe(trigger));
  });

  it("delete asks first; a refused delete keeps the record open", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls, () =>
      jsonRes({ error: "không thể xoá quản trị viên cuối cùng", code: "VALIDATION" }, 422),
    ) as unknown as typeof fetch;
    render(<UserList />);
    fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
    fireEvent.click(await screen.findByRole("button", { name: "Xoá người dùng" }));

    const prompt = await screen.findByRole("alertdialog", { name: "Xoá người dùng?" });
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    fireEvent.click(within(prompt).getByRole("button", { name: "Xoá" }));

    await waitFor(() => expect(calls.some((c) => c.method === "DELETE")).toBe(true));
    await waitFor(() => expect(screen.getByText("không thể xoá quản trị viên cuối cùng")).toBeInTheDocument());
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Trần Sales Rep");
  });

  // specs/065 lát B: the builder switches the customer-owner capability from the record.
  describe("Chủ khách switch", () => {
    afterEach(() => { session.user = { id: "u-admin", email: "admin@example.com", isAdmin: true, isPlatformOperator: false }; });

    it("a builder sees it on a non-admin account, with both caveats, and PUTs /user-manager", async () => {
      session.user = { id: "u-op", email: "admin@example.com", isAdmin: true, isPlatformOperator: true };
      const calls: Call[] = [];
      global.fetch = routedFetch(calls) as unknown as typeof fetch;
      render(<UserList />);
      fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
      const box = await screen.findByLabelText("Chủ khách — tự quản người dùng trong app");
      expect(box).not.toBeChecked();
      expect(screen.getByText(/nằm ngoài tầm của chủ khách/)).toBeInTheDocument();
      expect(screen.getByText(/Tắt chỉ gỡ quyền quản người dùng — vai giữ nguyên/)).toBeInTheDocument();

      fireEvent.click(box);
      await waitFor(() => expect(calls.find((c) => c.url.endsWith("/user-manager"))?.body).toEqual({ enabled: true }));
      expect(calls.find((c) => c.url.endsWith("/user-manager"))?.method).toBe("PUT");
      await waitFor(() => expect(screen.getByLabelText("Chủ khách — tự quản người dùng trong app")).toBeChecked());
    });

    it("a tenant admin who is not the builder does not see it", async () => {
      global.fetch = routedFetch([]) as unknown as typeof fetch;
      render(<UserList />);
      fireEvent.click(await screen.findByRole("button", { name: "Trần Sales Rep" }));
      await screen.findByLabelText("Vai trò");
      expect(screen.queryByLabelText("Chủ khách — tự quản người dùng trong app")).not.toBeInTheDocument();
    });

    it("the builder does not see it on an admin account (the tiers exclude each other)", async () => {
      session.user = { id: "u-op", email: "admin@example.com", isAdmin: true, isPlatformOperator: true };
      global.fetch = vi.fn(async (url: RequestInfo | URL) => {
        const u = String(url);
        if (u.endsWith("/sessions")) return jsonRes({ data: [] }, 200);
        if (u.endsWith("/admin/users/u-admin")) return jsonRes({ data: { user: ADMIN } }, 200);
        if (u.endsWith("/admin/users")) return jsonRes({ data: { users: [ADMIN, REP] } }, 200);
        return jsonRes({ data: { roles: ROLES } }, 200);
      }) as unknown as typeof fetch;
      render(<UserList />);
      fireEvent.click(await screen.findByRole("button", { name: "Nexus Admin" }));
      await screen.findByLabelText("Vai trò");
      expect(screen.queryByLabelText("Chủ khách — tự quản người dùng trong app")).not.toBeInTheDocument();
    });
  });
});
