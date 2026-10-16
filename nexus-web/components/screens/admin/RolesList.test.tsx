// Component tests — /admin/roles (RolesList + ACLMatrix) on SLDS (specs/038 T048).
// Pins the permission screen's contract: roles in the split view (first one opened), the matrix rows =
// ✶ + every live entity + a stale grant still visible, a cell / a whole column set from its menu,
// Lưu PUTs the replace-set, a dirty matrix asks before switching role, create + delete roles.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RolesList } from "@/components/screens/admin/RolesList";
import { logout } from "@/lib/api";
import { toast } from "@/components/ui/Toaster";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const ROLES = [
  { id: "r-staff", name: "crm_staff", label: "CRM — Nhân viên" },
  { id: "r-mgr", name: "crm_manager", label: "CRM — Quản lý" },
];
const GRANTS: Record<string, unknown[]> = {
  "r-staff": [
    { entityName: "crm_accounts", action: "view", accessLevel: "team" },
    { entityName: "old_thing", action: "view", accessLevel: "owner" },
  ],
  "r-mgr": [{ entityName: "*", action: "view", accessLevel: "all" }],
};
const ENTITIES = [
  { name: "crm_accounts", label: "Khách hàng" },
  { name: "crm_leads", label: "Khách hàng tiềm năng" },
];

// #283: crm_leads has a Người dùng field; crm_accounts has none.
const FIELDS: Record<string, unknown[]> = {
  crm_leads: [
    { name: "ten", label: "Tên", fieldType: "text" },
    { name: "bac_si", label: "Bác sĩ phụ trách", fieldType: "user" },
  ],
};

interface Call { method: string; url: string; body?: unknown }

function routedFetch(calls: Call[], roles = ROLES) {
  let list = [...roles];
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: u, body });
    if (u.endsWith("/admin/roles") && method === "GET") return jsonRes({ data: { roles: list } }, 200);
    if (u.endsWith("/admin/roles") && method === "POST") {
      const role = { id: "r-new", tenantId: "t-1", ...body };
      list = [...list, role];
      return jsonRes({ data: { role } }, 201);
    }
    if (u.endsWith("/metadata/entities")) return jsonRes({ data: ENTITIES }, 200);
    const mf = u.match(/\/metadata\/fields\/([\w-]+)$/);
    if (mf) return jsonRes({ data: FIELDS[mf[1]] ?? [] }, 200);
    const m = u.match(/\/admin\/roles\/([\w-]+)$/);
    if (m && method === "GET") {
      const r = list.find((x) => x.id === m[1])!;
      return jsonRes({ data: { role: { ...r, tenantId: "t-1", actions: GRANTS[m[1]] ?? [] } } }, 200);
    }
    if (m && method === "PUT") return jsonRes({ data: { role: { id: m[1], tenantId: "t-1", ...body } } }, 200);
    if (m && method === "DELETE") {
      list = list.filter((x) => x.id !== m[1]);
      return new Response(null, { status: 204 });
    }
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

async function openMenu(name: string | RegExp) {
  screen.getByRole("button", { name }).focus();
  await userEvent.keyboard("{Enter}");
  return screen.findByRole("menu");
}

describe("RolesList — /admin/roles", () => {
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

  it("opens the first role; rows are ✶ + live entities + a stale grant", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<RolesList />);
    const list = await screen.findByRole("listbox", { name: "Vai trò" });
    const options = await within(list).findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual(["CRM — Nhân viêncrm_staff", "CRM — Quản lýcrm_manager"]);
    await waitFor(() => expect(options[0]).toHaveAttribute("aria-current", "page"));

    const grid = await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const rows = within(grid).getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getByRole("rowheader").textContent)).toEqual([
      "✶ Tất cả entity", "Khách hàng", "Khách hàng tiềm năng", "old_thing (đã xoá?)",
    ]);
    expect(within(grid).getByRole("button", { name: "Xem · Khách hàng: Theo nhóm" })).toBeInTheDocument();
  });

  it("sets a cell and a column from their menus, then Lưu PUTs the replace-set", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const save = screen.getByRole("button", { name: "Lưu phân quyền" });
    expect(save).toBeDisabled();

    let menu = await openMenu("Tạo · Khách hàng tiềm năng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    menu = await openMenu('Đặt cột "Xoá" cho tất cả');
    await userEvent.click(within(menu).getByRole("menuitem", { name: "Theo nhóm" }));

    expect(screen.getByText("Có thay đổi chưa lưu")).toBeInTheDocument();
    await userEvent.click(save);
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")).toBeDefined());
    const put = calls.find((c) => c.method === "PUT")!;
    expect(put.url).toMatch(/\/admin\/roles\/r-staff$/);
    const body = put.body as { name: string; label: string; actions: { entityName: string; action: string; accessLevel: string }[] };
    expect(body.name).toBe("crm_staff");
    expect(body.label).toBe("CRM — Nhân viên");
    const key = (g: { entityName: string; action: string; accessLevel: string }) => `${g.entityName}:${g.action}:${g.accessLevel}`;
    expect(body.actions.map(key).sort()).toEqual([
      "*:delete:team", "crm_accounts:delete:team", "crm_accounts:view:team", "crm_leads:create:owner",
      "crm_leads:delete:team", "old_thing:delete:team", "old_thing:view:owner",
    ]);
    await waitFor(() => expect(screen.getByRole("button", { name: "Lưu phân quyền" })).toBeDisabled());
  });

  it("a cell menu states the current level (checked item); the column menu has no current level", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const menu = await openMenu("Xem · Khách hàng: Theo nhóm");
    const items = within(menu).getAllByRole("menuitemcheckbox");
    expect(items.map((i) => [i.textContent, i.getAttribute("aria-checked")])).toEqual([
      ["Không có", "false"], ["Chỉ mình", "false"], ["Theo nhóm", "true"], ["Tất cả", "false"],
    ]);
    await userEvent.keyboard("{Escape}");
    const column = await openMenu('Đặt cột "Xem" cho tất cả');
    expect(within(column).queryAllByRole("menuitemcheckbox")).toHaveLength(0);
    expect(within(column).getAllByRole("menuitem")).toHaveLength(4);
  });

  it("a table with a Người dùng field offers 'hồ sơ có trường … là tôi'; saving PUTs field:<name>", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    // The field list loads after the matrix; wait for the extra option to appear.
    let menu = await openMenu("Xem · Khách hàng tiềm năng: Không có");
    await waitFor(() => expect(within(menu).queryByRole("menuitemcheckbox", { name: "Hồ sơ có trường “Bác sĩ phụ trách” là tôi" })).toBeInTheDocument());
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Hồ sơ có trường “Bác sĩ phụ trách” là tôi" }));
    // The cell now says so, and a table without a Người dùng field offers no such option.
    expect(screen.getByRole("button", { name: "Xem · Khách hàng tiềm năng: Hồ sơ có trường “Bác sĩ phụ trách” là tôi" })).toBeInTheDocument();
    menu = await openMenu("Xem · Khách hàng: Theo nhóm");
    expect(within(menu).queryByRole("menuitemcheckbox", { name: /Hồ sơ có trường/ })).not.toBeInTheDocument();
    await userEvent.keyboard("{Escape}");

    await userEvent.click(screen.getByRole("button", { name: "Lưu phân quyền" }));
    await waitFor(() => expect(calls.find((c) => c.method === "PUT")).toBeDefined());
    const body = calls.find((c) => c.method === "PUT")!.body as { actions: { entityName: string; action: string; accessLevel: string }[] };
    expect(body.actions).toContainEqual({ entityName: "crm_leads", action: "view", accessLevel: "field:bac_si" });
  });

  it("picking a level with the keyboard keeps focus on that cell's trigger", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const trigger = screen.getByRole("button", { name: "Xem · Khách hàng tiềm năng: Không có" });
    trigger.focus();
    await userEvent.keyboard("{Enter}");
    await screen.findByRole("menu");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    await waitFor(() => expect(screen.queryByRole("menu")).not.toBeInTheDocument());
    // The same element (not a remounted copy), now naming the new level, holds focus.
    expect(trigger.isConnected).toBe(true);
    expect(trigger).toHaveAccessibleName("Xem · Khách hàng tiềm năng: Chỉ mình");
    await waitFor(() => expect(document.activeElement).toBe(trigger));

    // A whole-column set from the header menu keeps focus on the header trigger too.
    const header = screen.getByRole("button", { name: 'Đặt cột "Xoá" cho tất cả' });
    header.focus();
    await userEvent.keyboard("{Enter}");
    await screen.findByRole("menu");
    await userEvent.keyboard("{ArrowDown}{ArrowDown}{Enter}");
    await waitFor(() => expect(screen.queryByRole("menu")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Xoá · Khách hàng: Theo nhóm" })).toBeInTheDocument();
    expect(header.isConnected).toBe(true);
    await waitFor(() => expect(document.activeElement).toBe(header));
  });

  it("the role list is one tab stop; arrows / Home / End move, Space opens the role", async () => {
    global.fetch = routedFetch([], [...ROLES, { id: "r-new", name: "kho_staff", label: "Kho — Nhân viên" }]) as unknown as typeof fetch;
    render(<RolesList />);
    const list = await screen.findByRole("listbox", { name: "Vai trò" });
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const options = within(list).getAllByRole("option");
    expect(options.map((o) => o.tabIndex)).toEqual([0, -1, -1]);

    options[0].focus();
    await userEvent.keyboard("{ArrowDown}");
    expect(document.activeElement).toBe(options[1]);
    await userEvent.keyboard("{End}");
    expect(document.activeElement).toBe(options[2]);
    await userEvent.keyboard("{ArrowDown}"); // stops at the last option
    expect(document.activeElement).toBe(options[2]);
    await userEvent.keyboard("{Home}");
    expect(document.activeElement).toBe(options[0]);
    await userEvent.keyboard("{ArrowUp}"); // stops at the first option
    expect(document.activeElement).toBe(options[0]);
    // Moving focus does not open a role; Space does.
    await userEvent.keyboard("{End}");
    expect(options[0]).toHaveAttribute("aria-selected", "true");
    await userEvent.keyboard(" ");
    await waitFor(() => expect(options[2]).toHaveAttribute("aria-current", "page"));
    expect(options[2]).toHaveAttribute("aria-selected", "true");
    expect(options.map((o) => o.tabIndex)).toEqual([-1, -1, 0]);
  });

  it("a dirty matrix asks before switching role", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const menu = await openMenu("Xem · Khách hàng: Theo nhóm");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Tất cả" }));

    await userEvent.click(screen.getByRole("option", { name: /CRM — Quản lý/ }));
    const prompt = await screen.findByRole("alertdialog", { name: "Bỏ thay đổi chưa lưu?" });
    await userEvent.click(within(prompt).getByRole("button", { name: "Ở lại" }));
    expect(screen.getByRole("option", { name: /CRM — Nhân viên/ })).toHaveAttribute("aria-current", "page");
    expect(screen.getByRole("button", { name: "Xem · Khách hàng: Tất cả" })).toBeInTheDocument();

    await userEvent.click(screen.getByRole("option", { name: /CRM — Quản lý/ }));
    await userEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "Bỏ thay đổi" }));
    await waitFor(() => expect(screen.getByRole("option", { name: /CRM — Quản lý/ })).toHaveAttribute("aria-current", "page"));
    // The manager's ✶ view=all drives every entity row (inherited, marked as such).
    expect(await screen.findByRole("button", { name: "Xem · Khách hàng: Tất cả (kế thừa từ hàng ✶)" })).toBeInTheDocument();
  });

  it("creates a role from the modal and opens it; deletes a role after the prompt", async () => {
    const calls: Call[] = [];
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });

    await userEvent.click(screen.getByRole("button", { name: "Tạo vai trò mới" }));
    const dialog = await screen.findByRole("dialog", { name: "Tạo vai trò" });
    await userEvent.type(within(dialog).getByLabelText(/Mã vai trò/), "kho_staff");
    await userEvent.type(within(dialog).getByLabelText(/Nhãn hiển thị/), "Kho — Nhân viên");
    await userEvent.click(within(dialog).getByRole("button", { name: "Tạo" }));
    await waitFor(() => expect(calls.find((c) => c.method === "POST")?.body).toEqual({ name: "kho_staff", label: "Kho — Nhân viên", actions: [] }));
    await waitFor(() => expect(screen.getByRole("option", { name: /Kho — Nhân viên/ })).toHaveAttribute("aria-current", "page"));

    await userEvent.click(screen.getByRole("button", { name: "Xoá vai trò" }));
    const prompt = await screen.findByRole("alertdialog", { name: "Xoá vai trò “Kho — Nhân viên”?" });
    await userEvent.click(within(prompt).getByRole("button", { name: "Xoá" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE" && c.url.endsWith("/admin/roles/r-new"))).toBe(true));
    await waitFor(() => expect(screen.queryByRole("option", { name: /Kho — Nhân viên/ })).not.toBeInTheDocument());
    expect(screen.getByRole("option", { name: /CRM — Nhân viên/ })).toHaveAttribute("aria-current", "page");
  });
  it("a refused reserved role name (422 ROLE_NAME_RESERVED) shows the server's own sentence (specs/065)", async () => {
    const SENTENCE = "tên vai này được hệ thống giữ riêng cho chủ khách";
    const inner = routedFetch([]);
    global.fetch = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      if (String(url).endsWith("/admin/roles") && (init?.method ?? "GET") === "POST") {
        return jsonRes({ error: SENTENCE, code: "ROLE_NAME_RESERVED" }, 422);
      }
      return inner(url, init);
    }) as unknown as typeof fetch;
    const spy = vi.spyOn(toast, "error").mockImplementation(() => undefined as never);
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    await userEvent.click(screen.getByRole("button", { name: "Tạo vai trò mới" }));
    const dialog = await screen.findByRole("dialog", { name: "Tạo vai trò" });
    await userEvent.type(within(dialog).getByLabelText(/Mã vai trò/), "tenant_owner");
    await userEvent.type(within(dialog).getByLabelText(/Nhãn hiển thị/), "Chủ");
    await userEvent.click(within(dialog).getByRole("button", { name: "Tạo" }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith(SENTENCE));
    spy.mockRestore();
  });
  it("guest /admin/roles still creates in a Modal and edits beside the list (specs/074 T177)", async () => {
    global.fetch = routedFetch([]) as unknown as typeof fetch;
    render(<RolesList />);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    await userEvent.click(screen.getByRole("button", { name: "Tạo vai trò mới" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
  });
});
