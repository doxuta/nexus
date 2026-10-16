// Component tests — "Cài đặt › Người dùng" of the customer owner (UserList mode="owner", specs/065 lát B).
// Pins: the owner sees every account but may change only staff; read-only rows say whose they are; the form
// has exactly name / role / active-locked (no admin box, no password, no delete, no add, no invite); the
// role picker never offers the reserved owner role; a save sends only what changed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { UserList } from "@/components/screens/admin/UserList";
import { logout } from "@/lib/api";

vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: { id: "u-owner", email: "chu@khach.vn", isAdmin: false, canManageUsers: true } }) }));

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const USERS = [
  { id: "u-owner", email: "chu@khach.vn", fullName: "Chủ Khách", status: "active", roleId: "r-owner", tier: "owner", manageable: false },
  { id: "u-admin", email: "admin@khach.vn", fullName: "Quản Trị", status: "active", tier: "admin", manageable: false },
  { id: "u-nv", email: "nv@khach.vn", fullName: "Nhân Viên", status: "active", roleId: "r-sales", tier: "staff", manageable: true },
];
const ROLES = [
  { id: "r-sales", label: "Nhân viên kinh doanh", assignable: true },
  { id: "r-mgr", label: "Quản lý kinh doanh", assignable: true },
  { id: "r-owner", label: "Chủ khách — toàn quyền dữ liệu", assignable: false },
];

interface Call { method: string; url: string; body?: unknown }
const linkFails = { value: false };

function ownerFetch(calls: Call[]) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ method, url: u, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (u.endsWith("/org-admin/users") && method === "GET") return jsonRes({ data: { users: USERS } });
    if (u.endsWith("/org-admin/roles") && method === "GET") return jsonRes({ data: { roles: ROLES } });
    if (u.endsWith("/org-admin/users") && method === "POST") {
      const b = JSON.parse(String(init?.body));
      if (b.email === "dup@khach.vn") return jsonRes({ error: "email đã tồn tại", code: "USER_ALREADY_EXISTS" }, 409);
      return jsonRes({ data: { user: { id: "u-new", email: b.email, fullName: b.fullName, status: "active", tier: "staff", manageable: true } } }, 201);
    }
    if (u.endsWith("/reset-password-link") && method === "POST") {
      if (u.includes("/u-new/") && linkFails.value) return jsonRes({ error: "boom", code: "X" }, 500);
      return jsonRes({ data: { resetLink: "http://x/reset?token=abc", token: "abc", expiresAt: "2026-10-14T00:00:00Z", ttlMinutes: 30, emailQueued: false } }, 201);
    }
    if (u.includes("/sessions/") && method === "DELETE") return new Response(null, { status: 204 });
    if (u.endsWith("/sessions") && method === "GET") return jsonRes({ data: [{ id: "s1", browser: "Chrome", os: "macOS", ip: "1.2.3.x", lastUsedAt: "2026-10-14T00:00:00Z", expiresAt: "2026-10-21T00:00:00Z", current: false }] });
    const m = u.match(/\/org-admin\/users\/([\w-]+)$/);
    if (m && method === "GET") return jsonRes({ data: { user: USERS.find((x) => x.id === m[1]) } });
    if (m && method === "PUT") return jsonRes({ data: { user: USERS.find((x) => x.id === m[1]) } });
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("UserList — owner mode (/settings/users)", () => {
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

  it("lát C: add staff sends 3 keys, then the invite dialog opens on its own with the link", async () => {
    linkFails.value = false;
    const calls: Call[] = [];
    global.fetch = ownerFetch(calls) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    await screen.findByRole("grid", { name: "Danh sách người dùng" });
    fireEvent.click(screen.getByRole("button", { name: "Thêm người dùng" }));
    const dialog = await screen.findByRole("dialog", { name: "Thêm người dùng" });
    expect(within(dialog).queryByLabelText(/Mật khẩu/)).not.toBeInTheDocument();
    fireEvent.change(within(dialog).getByLabelText(/Họ tên/), { target: { value: "Mới" } });
    fireEvent.change(within(dialog).getByLabelText(/Email/), { target: { value: "moi@khach.vn" } });
    fireEvent.change(within(dialog).getByLabelText("Vai trò"), { target: { value: "r-sales" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo người dùng" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toEqual({ email: "moi@khach.vn", fullName: "Mới", roleId: "r-sales" }));
    expect(await screen.findByDisplayValue("http://x/reset?token=abc")).toBeInTheDocument();
    expect(calls.find((c) => c.url.endsWith("/u-new/reset-password-link"))?.method).toBe("POST");
    expect(calls.every((c) => c.url.includes("/org-admin/"))).toBe(true);
  });

  it("lát C: link failure after create says so and offers 'Gửi link mời'; a duplicate email says so", async () => {
    linkFails.value = true;
    global.fetch = ownerFetch([]) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    await screen.findByRole("grid", { name: "Danh sách người dùng" });
    fireEvent.click(screen.getByRole("button", { name: "Thêm người dùng" }));
    let dialog = await screen.findByRole("dialog", { name: "Thêm người dùng" });
    fireEvent.change(within(dialog).getByLabelText(/Email/), { target: { value: "dup@khach.vn" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo người dùng" }));
    expect(await within(dialog).findByText("Email này đã có trong danh sách người dùng.")).toBeInTheDocument();
    fireEvent.change(within(dialog).getByLabelText(/Email/), { target: { value: "moi@khach.vn" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Tạo người dùng" }));
    expect(await screen.findByText(/Đã tạo — chưa lấy được link mời/)).toBeInTheDocument();
    linkFails.value = false;
    dialog = screen.getByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Gửi link mời" }));
    expect(await screen.findByDisplayValue("http://x/reset?token=abc")).toBeInTheDocument();
  });

  it("lát C: a staff detail shows the sessions card from /org-admin; an admin row has none", async () => {
    const calls: Call[] = [];
    global.fetch = ownerFetch(calls) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    fireEvent.click(await screen.findByRole("button", { name: "Nhân Viên" }));
    await waitFor(() => expect(calls.some((c) => c.method === "GET" && c.url.endsWith("/org-admin/users/u-nv/sessions"))).toBe(true));
    fireEvent.click(await screen.findByRole("button", { name: /^Đăng xuất / })); // ending a session goes through /org-admin
    fireEvent.click(await screen.findByRole("button", { name: "Đăng xuất thiết bị" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/org-admin/users/u-nv/sessions/s1"))).toBe(true));
    expect(calls.every((c) => c.url.includes("/org-admin/"))).toBe(true);
  });

  it("lát C: an admin or owner detail never loads sessions", async () => {
    const calls: Call[] = [];
    global.fetch = ownerFetch(calls) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    fireEvent.click(await screen.findByRole("button", { name: "Quản Trị" }));
    await screen.findByText("Tài khoản này do người dựng quản lý — bạn chỉ xem được.");
    expect(calls.some((c) => c.url.endsWith("/sessions"))).toBe(false);
    expect(screen.queryByRole("button", { name: /Đăng xuất/ })).not.toBeInTheDocument();
  });

  it("lát C: only a manageable row's menu offers 'Gửi link mời'", async () => {
    global.fetch = ownerFetch([]) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    await screen.findByRole("grid", { name: "Danh sách người dùng" });
    await userEvent.click(await screen.findByRole("button", { name: "Thêm thao tác cho Quản Trị" }));
    await screen.findByRole("menuitem", { name: "Xem chi tiết" });
    expect(screen.queryByRole("menuitem", { name: "Gửi link mời" })).not.toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("button", { name: "Thêm thao tác cho Nhân Viên" }));
    expect(await screen.findByRole("menuitem", { name: "Gửi link mời" })).toBeInTheDocument();
  });

  it("lists every account from /org-admin, badges tiers, offers add but no admin-only controls", async () => {
    const calls: Call[] = [];
    global.fetch = ownerFetch(calls) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    const grid = await screen.findByRole("grid", { name: "Danh sách người dùng" });
    await waitFor(() => expect(within(grid).getByText("Nhân Viên")).toBeInTheDocument());
    expect(within(grid).getByText("Chủ khách")).toBeInTheDocument(); // the owner badge
    expect(within(grid).getByText("Quản trị viên")).toBeInTheDocument();
    expect(calls.every((c) => c.url.includes("/org-admin/"))).toBe(true); // never /admin/*
    expect(screen.getByText("3 tài khoản")).toBeInTheDocument();
  });

  it("a staff record: exactly name, role and status; the owner role is not offered; save sends only the change", async () => {
    const calls: Call[] = [];
    global.fetch = ownerFetch(calls) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    fireEvent.click(await screen.findByRole("button", { name: "Nhân Viên" }));

    const role = (await screen.findByLabelText("Vai trò")) as HTMLSelectElement;
    expect(role.value).toBe("r-sales");
    const offered = Array.from(role.options).map((o) => o.textContent);
    expect(offered).toContain("Quản lý kinh doanh");
    expect(offered.join("|")).not.toMatch(/toàn quyền dữ liệu/);
    expect(screen.getByLabelText("Họ tên")).toBeInTheDocument();
    expect(screen.queryByLabelText("Quyền quản trị hệ thống")).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Mật khẩu/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Xoá/ })).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/Chủ khách — tự quản/)).not.toBeInTheDocument();

    fireEvent.change(role, { target: { value: "r-mgr" } });
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ roleId: "r-mgr" }));
    expect(calls.find((c) => c.method === "PUT")?.url).toMatch(/\/org-admin\/users\/u-nv$/);
  });

  it("locking sends status inactive", async () => {
    const calls: Call[] = [];
    global.fetch = ownerFetch(calls) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    fireEvent.click(await screen.findByRole("button", { name: "Nhân Viên" }));
    await screen.findByLabelText("Vai trò");
    fireEvent.click(screen.getByRole("radio", { name: "Khoá" }));
    fireEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")?.body).toEqual({ status: "inactive" }));
  });

  it("a read-only account says whose it is and has no form or save", async () => {
    global.fetch = ownerFetch([]) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    fireEvent.click(await screen.findByRole("button", { name: "Quản Trị" }));
    expect(await screen.findByText("Tài khoản này do người dựng quản lý — bạn chỉ xem được.")).toBeInTheDocument();
    expect(screen.queryByLabelText("Họ tên")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Lưu" })).not.toBeInTheDocument();
  });

  it("a locked row in the list carries the 'Do người dựng quản lý' badge only for staff the owner cannot change", async () => {
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const u = String(url);
      if (u.endsWith("/org-admin/users")) return jsonRes({ data: { users: [{ id: "u-d", email: "d@khach.vn", fullName: "Đã Cất", status: "active", tier: "staff", manageable: false }] } });
      return jsonRes({ data: { roles: ROLES } });
    }) as unknown as typeof fetch;
    render(<UserList mode="owner" />);
    const grid = await screen.findByRole("grid", { name: "Danh sách người dùng" });
    await waitFor(() => expect(within(grid).getByText("Do người dựng quản lý")).toBeInTheDocument());
  });
});
