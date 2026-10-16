// Queue #302 — RolesList + ACLMatrix inside an app's workshop: only the roles that reach the app, only the
// app's tables as matrix rows, a column bulk-set that cannot leak to ✶ or to other apps' tables, and
// "Xem tất cả" one click away. Outside a workshop nothing changes (RolesList.test.tsx).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { RolesList } from "@/components/screens/admin/RolesList";
import { WorkshopScopeProvider, type WorkshopScope } from "@/lib/workshopScope";
import { logout } from "@/lib/api";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const ROLES = [
  { id: "r-staff", name: "crm_staff", label: "CRM — Nhân viên" },
  { id: "r-lms", name: "lms_teacher", label: "LMS — Giáo viên" },
  { id: "r-mgr", name: "boss", label: "Chủ" },
  { id: "r-new", name: "fresh", label: "Mới tạo" },
];
const GRANTS: Record<string, unknown[]> = {
  "r-staff": [
    { entityName: "crm_accounts", action: "view", accessLevel: "team" },
    { entityName: "lms_classes", action: "view", accessLevel: "owner" },
  ],
  "r-lms": [{ entityName: "lms_classes", action: "view", accessLevel: "all" }],
  "r-mgr": [{ entityName: "*", action: "view", accessLevel: "all" }],
  "r-new": [],
};
const ENTITIES = [
  { name: "crm_accounts", label: "Khách hàng" },
  { name: "lms_classes", label: "Lớp học" },
];
const SCOPE: WorkshopScope = {
  slug: "crm", appLabel: "CRM", entityNames: new Set(["crm_accounts"]),
  panelHref: (p) => `/studio/apps/crm?panel=${p}`,
};

interface Call { method: string; url: string; body?: { actions?: { entityName: string; action: string; accessLevel: string }[] } }

function routedFetch(calls: Call[]) {
  return vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    const method = init?.method ?? "GET";
    const body = init?.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, url: u, body });
    if (u.endsWith("/admin/roles") && method === "GET") return jsonRes({ data: { roles: ROLES } });
    if (u.endsWith("/metadata/entities")) return jsonRes({ data: ENTITIES });
    if (/\/metadata\/fields\//.test(u)) return jsonRes({ data: [] });
    const m = u.match(/\/admin\/roles\/([\w-]+)$/);
    if (m && method === "GET") {
      const r = ROLES.find((x) => x.id === m[1])!;
      return jsonRes({ data: { role: { ...r, tenantId: "t-1", actions: GRANTS[m[1]] ?? [] } } });
    }
    if (m && method === "PUT") return jsonRes({ data: { role: { id: m[1], tenantId: "t-1", ...body } } });
    return jsonRes({ error: `unmocked ${method} ${u}`, code: "TEST_UNMOCKED" }, 500);
  });
}

describe("RolesList inside an app workshop (#302)", () => {
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
  });

  function mount(calls: Call[] = []) {
    global.fetch = routedFetch(calls) as unknown as typeof fetch;
    return render(<WorkshopScopeProvider value={SCOPE}><RolesList /></WorkshopScopeProvider>);
  }

  it("lists only roles that reach the app (own tables, ✶, or no grant yet) and says how many are hidden", async () => {
    mount();
    const list = await screen.findByRole("listbox", { name: "Vai trò" });
    await waitFor(() => expect(within(list).getAllByRole("option")).toHaveLength(3));
    expect(within(list).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "CRM — Nhân viêncrm_staff", "Chủboss", "Mới tạofresh",
    ]);
    expect(screen.getByTestId("scope-hidden-note")).toHaveTextContent("1 vai trò của app khác đang ẩn.");
  });

  it("draws ✶ + the app's tables only; other apps' grants are noted, not shown", async () => {
    mount();
    const grid = await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    const rows = within(grid).getAllByRole("row").slice(1);
    expect(rows.map((r) => within(r).getByRole("rowheader").textContent)).toEqual(["✶ Tất cả entity", "Khách hàng"]);
    expect(screen.getByText(/1 bảng của app khác không hiện ở đây/)).toBeInTheDocument();
  });

  it("a column bulk-set reaches the app's tables only — never ✶ or another app's table — and Lưu keeps hidden grants", async () => {
    const calls: Call[] = [];
    mount(calls);
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    screen.getByRole("button", { name: 'Đặt cột "Xoá" cho tất cả' }).focus();
    await userEvent.keyboard("{Enter}");
    const menu = await screen.findByRole("menu");
    await userEvent.click(within(menu).getByRole("menuitem", { name: "Theo nhóm" }));
    await userEvent.click(screen.getByRole("button", { name: "Lưu phân quyền" }));
    await waitFor(() => expect(calls.some((c) => c.method === "PUT")).toBe(true));
    const sent = calls.find((c) => c.method === "PUT")!.body!.actions!;
    const has = (e: string, a: string) => sent.find((g) => g.entityName === e && g.action === a);
    expect(has("crm_accounts", "delete")?.accessLevel).toBe("team");
    expect(has("*", "delete")).toBeUndefined();
    expect(has("lms_classes", "delete")).toBeUndefined();
    expect(has("lms_classes", "view")?.accessLevel).toBe("owner"); // hidden grant untouched
  });

  it("'Xem tất cả' brings back every role and every table", async () => {
    mount();
    await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    await userEvent.click(screen.getByRole("button", { name: "Xem tất cả" }));
    const list = screen.getByRole("listbox", { name: "Vai trò" });
    expect(within(list).getAllByRole("option")).toHaveLength(4);
    const grid = await screen.findByRole("grid", { name: "Ma trận phân quyền" });
    await waitFor(() => expect(within(grid).getAllByRole("row").slice(1)).toHaveLength(3));
  });
});
