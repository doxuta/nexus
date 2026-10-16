// Queue #302 — UserList inside an app's workshop: people whose role reaches the app (plus admins and the
// role-less), the "n người dùng của app khác đang ẩn" note, "Xem tất cả". Standalone behaviour: UserList.test.tsx.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { UserList } from "@/components/screens/admin/UserList";
import { WorkshopScopeProvider, type WorkshopScope } from "@/lib/workshopScope";
import { logout } from "@/lib/api";
import type { AdminUser } from "@/lib/admin";

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;
const jsonRes = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

const u = (id: string, name: string, extra: Partial<AdminUser> = {}): AdminUser =>
  ({ id, tenantId: "t-1", email: `${id}@x.vn`, fullName: name, status: "active", isAdmin: false, ...extra });
const USERS = [
  u("u-admin", "Quản trị", { isAdmin: true }),
  u("u-crm", "Nhân viên CRM", { roleId: "r-crm" }),
  u("u-lms", "Giáo viên LMS", { roleId: "r-lms" }),
  u("u-none", "Chưa có vai"),
];
const ROLES = [
  { id: "r-crm", name: "crm_staff", label: "CRM — Nhân viên" },
  { id: "r-lms", name: "lms_teacher", label: "LMS — Giáo viên" },
];
const GRANTS: Record<string, unknown[]> = {
  "r-crm": [{ entityName: "crm_accounts", action: "view", accessLevel: "team" }],
  "r-lms": [{ entityName: "lms_classes", action: "view", accessLevel: "all" }],
};
const SCOPE: WorkshopScope = {
  slug: "crm", appLabel: "CRM", entityNames: new Set(["crm_accounts"]),
  panelHref: (p) => `/studio/apps/crm?panel=${p}`,
};

describe("UserList inside an app workshop (#302)", () => {
  let originalFetch: FetchFn;
  beforeEach(async () => {
    originalFetch = global.fetch as unknown as FetchFn;
    await logout();
    window.localStorage.clear();
    window.sessionStorage.clear();
    window.localStorage.setItem("nexus_remember", "1");
    window.localStorage.setItem("nexus_token", "test-token");
    global.fetch = vi.fn(async (url: RequestInfo | URL) => {
      const s = String(url);
      if (s.endsWith("/admin/users")) return jsonRes({ data: { users: USERS } });
      if (s.endsWith("/admin/roles")) return jsonRes({ data: { roles: ROLES } });
      const m = s.match(/\/admin\/roles\/([\w-]+)$/);
      if (m) return jsonRes({ data: { role: { ...ROLES.find((r) => r.id === m[1]), tenantId: "t-1", actions: GRANTS[m[1]] } } });
      return jsonRes({ error: `unmocked ${s}`, code: "TEST_UNMOCKED" }, 500);
    }) as unknown as typeof fetch;
  });
  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
  });

  it("hides people whose role belongs to another app, keeps admins and the role-less, and can show everyone", async () => {
    render(<WorkshopScopeProvider value={SCOPE}><UserList /></WorkshopScopeProvider>);
    const grid = await screen.findByRole("grid", { name: "Danh sách người dùng" });
    await waitFor(() => expect(within(grid).queryByText("Giáo viên LMS")).not.toBeInTheDocument());
    expect(within(grid).getByText("Nhân viên CRM")).toBeInTheDocument();
    expect(within(grid).getByText("Quản trị")).toBeInTheDocument();
    expect(within(grid).getByText("Chưa có vai")).toBeInTheDocument();
    expect(screen.getByTestId("scope-hidden-note")).toHaveTextContent("1 người dùng của app khác đang ẩn.");
    // The title counts the filtered list, so it must not claim the whole tenant.
    expect(screen.getByText("3 người dùng của app này")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Xem tất cả" }));
    expect(within(grid).getByText("Giáo viên LMS")).toBeInTheDocument();
    expect(screen.getByText("4 tài khoản trong tenant")).toBeInTheDocument();
  });
});
