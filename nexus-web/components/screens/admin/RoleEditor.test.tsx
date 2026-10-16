// RoleEditor (specs/074 T175): the open role = header + full matrix + delete. Used by /admin/roles (Prompt) and the Studio role page.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RoleEditor } from "@/components/screens/admin/RoleEditor";
import { UnsavedBar } from "@/components/studio/UnsavedBar";
import { NavigationBlockerProvider } from "@/lib/navigation-blocker";
import { logout } from "@/lib/api";

const jsonRes = (body: unknown, status: number) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
interface Call { method: string; url: string; body?: unknown }

function routed(calls: Call[]) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ method, url: u, body: init?.body ? JSON.parse(String(init.body)) : undefined });
    if (u.endsWith("/metadata/entities")) return jsonRes({ data: [{ name: "crm_accounts", label: "Khách hàng" }] }, 200);
    if (u.match(/\/metadata\/fields\//)) return jsonRes({ data: [] }, 200);
    if (u.endsWith("/admin/roles/r-staff") && method === "GET")
      return jsonRes({ data: { role: { id: "r-staff", name: "crm_staff", label: "CRM — Nhân viên", tenantId: "t", actions: [{ entityName: "crm_accounts", action: "view", accessLevel: "team" }] } } }, 200);
    if (u.endsWith("/admin/roles/r-staff") && method === "PUT") return jsonRes({ data: { role: { id: "r-staff" } } }, 200);
    if (u.endsWith("/admin/roles/r-staff") && method === "DELETE") return new Response(null, { status: 204 });
    if (u.includes("/admin/roles/")) return jsonRes({ error: "not found", code: "NOT_FOUND" }, 404);
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

async function openMenu(name: string | RegExp) {
  screen.getByRole("button", { name }).focus();
  await userEvent.keyboard("{Enter}");
  return screen.findByRole("menu");
}

describe("RoleEditor", () => {
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

  it("says 'Không tìm thấy vai này.' for an id that is not a role (404)", async () => {
    global.fetch = routed([]) as unknown as typeof fetch;
    render(<RoleEditor roleId="r-gone" />);
    expect(await screen.findByText("Không tìm thấy vai này.")).toBeInTheDocument();
    expect(screen.queryByRole("grid")).toBeNull();
  });

  it("with renderBar: Lưu/Huỷ appear only after a change; Lưu PUTs; Huỷ puts the cell back", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls) as unknown as typeof fetch;
    render(
      <RoleEditor
        roleId="r-staff"
        renderBar={(s) => <UnsavedBar dirty={s.dirty} saving={s.saving} error={s.error} onSave={s.save} onCancel={s.reset} />}
      />,
    );
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    expect(screen.queryByRole("button", { name: "Lưu" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Lưu phân quyền" })).toBeNull();

    let menu = await openMenu("Tạo · Khách hàng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    expect(screen.getByText("Có thay đổi chưa lưu")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Lưu" })).toBeNull());

    menu = await openMenu("Tạo · Khách hàng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT" && c.url.endsWith("/admin/roles/r-staff"))).toBe(true));
    await waitFor(() => expect(screen.queryByRole("button", { name: "Lưu" })).toBeNull());
  });

  it("leaving by a link while unsaved is stopped by the navigation blocker", async () => {
    global.fetch = routed([]) as unknown as typeof fetch;
    const confirm = vi.fn(() => false);
    window.confirm = confirm;
    render(
      <NavigationBlockerProvider>
        {/* eslint-disable-next-line @next/next/no-html-link-for-pages -- plain anchor on purpose: the blocker listens to document clicks */}
        <a href="/elsewhere">đi chỗ khác</a>
        <RoleEditor roleId="r-staff" />
      </NavigationBlockerProvider>,
    );
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const menu = await openMenu("Tạo · Khách hàng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    await userEvent.click(screen.getByRole("link", { name: "đi chỗ khác" }));
    expect(confirm).toHaveBeenCalled();
  });

  it("inlineDelete: confirms in the page (no dialog), then DELETEs and reports onDeleted", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls) as unknown as typeof fetch;
    const onDeleted = vi.fn();
    render(<RoleEditor roleId="r-staff" inlineDelete onDeleted={onDeleted} />);
    await userEvent.click(await screen.findByRole("button", { name: "Xoá vai trò" }));
    expect(screen.queryByRole("alertdialog")).toBeNull();
    expect(screen.getByText(/Xoá vai CRM — Nhân viên\?/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Xoá vai" }));
    await waitFor(() => expect(calls.some((c) => c.method === "DELETE")).toBe(true));
    await waitFor(() => expect(onDeleted).toHaveBeenCalledTimes(1));
  });
});
