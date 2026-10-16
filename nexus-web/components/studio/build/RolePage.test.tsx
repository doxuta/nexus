// RolePage (specs/074 lát f2-role-page, T176 + T457): one role on its own URL, full matrix, field rights column, save bar (FR-072).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { logout } from "@/lib/api";

vi.mock("next/navigation", () => ({
  usePathname: () => "/studio/du-an/acme/dung/nguoi-dung-quyen/vai/r-staff",
  useRouter: () => ({ push: vi.fn(), replace: vi.fn(), back: vi.fn() }),
}));
vi.mock("@/lib/studioContext", () => ({
  useStudioIdentity: () => ({ status: "ready", me: { isPlatformOperator: true, workspace: null }, home: "northwind", sessionReadable: true, matches: () => true, retry: vi.fn() }),
  homeSlug: () => "northwind",
}));
vi.mock("@/lib/workspace", async (orig) => ({ ...(await orig<typeof import("@/lib/workspace")>()), inWorkspace: () => false }));

const viewAsRole = vi.hoisted(() => vi.fn());
vi.mock("@/lib/studioViewAs", () => ({ previewInWorkspace: false, viewAsRole }));

import { RolePage } from "./RolePage";

const jsonRes = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
interface Call { method: string; url: string }

function routed(calls: Call[], opts: { actions?: unknown[]; fieldGrants?: unknown[]; putStatus?: number; delivered?: boolean; apps?: unknown[] } = {}) {
  const actions = opts.actions ?? [{ entityName: "crm_accounts", action: "view", accessLevel: "team" }];
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    calls.push({ method, url: u });
    if (u.endsWith("/admin/apps")) return jsonRes({ data: { apps: opts.apps ?? [] } });
    if (u.endsWith("/admin/publish/state")) return jsonRes({ data: { delivered: opts.delivered ?? false, draftableTypes: [], directTypes: ["role"] } });
    if (u.includes("/admin/field-acl")) return jsonRes({ data: { defaultPolicy: "allow", grants: opts.fieldGrants ?? [] } });
    if (u.endsWith("/metadata/entities")) return jsonRes({ data: [{ name: "crm_accounts", label: "Khách hàng" }] });
    if (u.match(/\/metadata\/fields\//)) return jsonRes({ data: [] });
    if (u.endsWith("/admin/roles/r-staff") && method === "GET")
      return jsonRes({ data: { role: { id: "r-staff", name: "crm_staff", label: "CRM — Nhân viên", tenantId: "t", actions } } });
    if (u.endsWith("/admin/roles/r-staff") && method === "PUT")
      return opts.putStatus ? jsonRes({ error: "Vai này đang bị khoá", code: "ROLE_LOCKED" }, opts.putStatus) : jsonRes({ data: { role: { id: "r-staff" } } });
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

const view = () => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
    <RolePage slug="acme" roleId="r-staff" />
  </QueryClientProvider>
);
async function openMenu(name: string) {
  screen.getByRole("button", { name }).focus();
  await userEvent.keyboard("{Enter}");
  return screen.findByRole("menu");
}

describe("RolePage", () => {
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

  it("opens the role of the URL with the way back, and counts its field rights in the right column", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls, {
      fieldGrants: [
        { roleId: "r-staff", entityName: "crm_accounts", fieldName: "phone", canView: false, canEdit: false },
        { roleId: "r-staff", entityName: "crm_accounts", fieldName: "email", canView: true, canEdit: false },
      ],
    }) as unknown as typeof fetch;
    render(view());
    expect(await screen.findByRole("grid", { name: "Ma trận phân quyền" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Người dùng & quyền" })).toHaveAttribute("href", "/studio/du-an/acme/dung/nguoi-dung-quyen");
    const aside = await screen.findByRole("complementary", { name: "Quyền theo trường của vai này (2)" });
    expect(within(aside).getByText("crm_accounts.phone")).toBeInTheDocument();
    expect(within(aside).getByText("Không thấy")).toBeInTheDocument();
    expect(within(aside).getByText("Chỉ xem")).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("/admin/field-acl") && c.url.includes("roleId=r-staff"))).toBe(true);
  });

  it("a role with no grant says it sees no table and links back to the matrix", async () => {
    global.fetch = routed([], { actions: [] }) as unknown as typeof fetch;
    render(view());
    expect(await screen.findByText("Vai này chưa thấy bảng nào.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Đặt quyền trong ma trận" })).toHaveAttribute("href", "#ma-tran");
  });

  it("save bar: only after a change, live text, does not hide the matrix end; a failed save keeps the bar with the engine's words", async () => {
    global.fetch = routed([], { putStatus: 422 }) as unknown as typeof fetch;
    const { container } = render(view());
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    expect(screen.queryByRole("button", { name: "Lưu" })).toBeNull();
    const menu = await openMenu("Tạo · Khách hàng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    expect(screen.getByText("Có thay đổi chưa lưu")).toBeInTheDocument();
    expect(container.querySelector("[data-testid=unsaved-bar]")).not.toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Vai này đang bị khoá"));
    expect(screen.getByRole("button", { name: "Lưu" })).toBeInTheDocument();
  });

  it("delivered: Lưu asks 'Áp ngay cho người dùng thật' in the page first, PUT only after confirm, no 'vào nháp' button (T378)", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls, { delivered: true }) as unknown as typeof fetch;
    render(view());
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    expect(await screen.findByText("Áp ngay cho người dùng thật")).toBeInTheDocument();
    const menu = await openMenu("Tạo · Khách hàng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    expect(await screen.findByText(/Áp ngay cho người dùng thật: Lưu ma trận quyền của vai trò/)).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("button", { name: /nháp/i })).toBeNull();
    expect(calls.some((c) => c.method === "PUT")).toBe(false);
    await userEvent.click(screen.getByRole("button", { name: "Áp ngay" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
  });

  it("not delivered: Lưu writes at once, no label", async () => {
    const calls: Call[] = [];
    global.fetch = routed(calls) as unknown as typeof fetch;
    render(view());
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const menu = await openMenu("Tạo · Khách hàng: Không có");
    await userEvent.click(within(menu).getByRole("menuitemcheckbox", { name: "Chỉ mình" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    expect(screen.queryByText("Áp ngay cho người dùng thật")).toBeNull();
  });

  it("Xem như vai này: calls viewAsRole of the Studio with this role and the default app", async () => {
    global.fetch = routed([], { apps: [{ id: "a1", name: "crm", label: "CRM", isDefault: true }] }) as unknown as typeof fetch;
    render(view());
    await userEvent.click(await screen.findByRole("button", { name: "Xem như vai này" }));
    await waitFor(() => expect(viewAsRole).toHaveBeenCalledWith({ roleId: "r-staff", appId: "a1", appSlug: "crm" }));
  });
});
