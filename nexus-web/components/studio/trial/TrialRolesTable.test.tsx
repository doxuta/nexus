import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const listRoles = vi.fn();
const viewAsRole = vi.fn();
const getRole = vi.fn();
const listEntities = vi.fn();
const inWorkspace = vi.fn(() => false);
const flag = { previewInWorkspace: true };
const listAuditFiltered = vi.fn();
vi.mock("@/lib/admin", async (orig) => ({ ...(await orig<typeof import("@/lib/admin")>()), listAuditFiltered: (...a: unknown[]) => listAuditFiltered(...a) }));
vi.mock("@/lib/api", async (orig) => ({ ...(await orig<typeof import("@/lib/api")>()), listRoles: () => listRoles(), getRole: (id: string) => getRole(id), listEntities: () => listEntities() }));
vi.mock("@/lib/studioViewAs", () => ({ viewAsRole: (...a: unknown[]) => viewAsRole(...a), get previewInWorkspace() { return flag.previewInWorkspace; } }));
vi.mock("@/lib/workspace", () => ({ inWorkspace: () => inWorkspace() }));
// The identity gate is covered by useProjectQuery's own tests; here the query just runs.
vi.mock("../useProjectQuery", () => ({
  useProjectQuery: (slug: string, key: unknown[], fn: () => Promise<unknown>) => useQuery({ queryKey: ["project", slug, ...key], queryFn: fn, staleTime: 0 }),
}));

import { ApiError } from "@/lib/api";
import { TrialRolesTable } from "./TrialRolesTable";

const APP = { id: "a1", name: "crm", label: "CRM" };
const ROLES = [
  { id: "r1", name: "crm_sales_rep", label: "Nhân viên kinh doanh" },
  { id: "r2", name: "crm_manager", label: "Quản lý" },
  { id: "r3", name: "front_desk", label: "Lễ tân" },
];

const TABLES = ["a", "b", "c", "d", "e"].map((n) => ({ name: n, label: `Bảng ${n}`, tableName: n }));
const g = (entityName: string, action: "view" | "create" | "edit" | "delete", accessLevel: "all" | "none" = "all") => ({ entityName, action, accessLevel });

function ui(app: typeof APP | null = APP, slug = "northwind") {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const el = (s: string) => (
    <QueryClientProvider client={qc}>
      <TrialRolesTable slug={s} app={app} />
    </QueryClientProvider>
  );
  const r = render(el(slug));
  return { ...r, rerenderSlug: (s: string) => r.rerender(el(s)) };
}

beforeEach(() => {
  vi.clearAllMocks();
  inWorkspace.mockReturnValue(false);
  flag.previewInWorkspace = true;
  listRoles.mockResolvedValue(ROLES);
  viewAsRole.mockResolvedValue(undefined);
  listAuditFiltered.mockResolvedValue({ entries: [], limit: 100 });
  listEntities.mockResolvedValue(TABLES);
  getRole.mockImplementation(async (id: string) => ({ id, name: id, tenantId: "t", actions: [] }));
});

describe("TrialRolesTable", () => {
  it("one row and one 'Xem như' button per role, each with an accessible name", async () => {
    ui();
    expect(await screen.findByRole("button", { name: "Xem như Lễ tân" })).toBeTruthy();
    expect(screen.getAllByRole("button", { name: /^Xem như / })).toHaveLength(3);
    expect(screen.getAllByRole("row")).toHaveLength(4); // header + 3
  });

  it("click calls viewAsRole with the role and the app of the table", async () => {
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Quản lý" }));
    await waitFor(() => expect(viewAsRole).toHaveBeenCalledWith({ roleId: "r2", appId: "a1", appSlug: "crm" }));
  });

  it("double click starts exactly one session; other buttons lock and the busy one says 'Đang mở…'", async () => {
    viewAsRole.mockReturnValue(new Promise(() => {}));
    ui();
    const b = await screen.findByRole("button", { name: "Xem như Lễ tân" });
    fireEvent.click(b);
    fireEvent.click(b);
    expect(viewAsRole).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Đang mở…")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Xem như Quản lý" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("forwards the draft choice to viewAsRole", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><TrialRolesTable slug="northwind" app={APP} draft={false} /></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Quản lý" }));
    await waitFor(() => expect(viewAsRole).toHaveBeenCalledWith({ roleId: "r2", appId: "a1", appSlug: "crm", draft: false }));
  });

  it("PREVIEW_DRAFT_UNAVAILABLE: no red message, button usable again", async () => {
    viewAsRole.mockRejectedValue(new ApiError(422, "PREVIEW_DRAFT_UNAVAILABLE", "x"));
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Lễ tân" }));
    await waitFor(() => expect((screen.getByRole("button", { name: "Xem như Lễ tân" }) as HTMLButtonElement).disabled).toBe(false));
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("PREVIEW_ROLE_NOT_FOUND shows the row sentence and reloads the list", async () => {
    viewAsRole.mockRejectedValue(new ApiError(422, "PREVIEW_ROLE_NOT_FOUND", "x"));
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Lễ tân" }));
    expect(await screen.findByText("Vai này không còn trong dự án.")).toBeTruthy();
    await waitFor(() => expect(listRoles).toHaveBeenCalledTimes(2));
  });

  it("403 says only the builder can do this; unlocks the buttons", async () => {
    viewAsRole.mockRejectedValue(new ApiError(403, "FORBIDDEN", "x"));
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Lễ tân" }));
    expect(await screen.findByText("Việc này chỉ người dựng Nexus làm được.")).toBeTruthy();
    expect((screen.getByRole("button", { name: "Xem như Quản lý" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("network / 5xx: generic sentence plus 'Thử lại' that tries the same role again", async () => {
    viewAsRole.mockRejectedValueOnce(new Error("boom"));
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Lễ tân" }));
    expect(await screen.findByText("Chưa mở được app xem thử.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    await waitFor(() => expect(viewAsRole).toHaveBeenCalledTimes(2));
    expect(viewAsRole).toHaveBeenLastCalledWith({ roleId: "r3", appId: "a1", appSlug: "crm" });
  });

  it("no roles: says so and points to the access step", async () => {
    listRoles.mockResolvedValue([]);
    ui();
    expect(await screen.findByText("Chưa có vai nào. Tạo vai ở bước Người dùng & quyền.")).toBeTruthy();
    expect(screen.queryAllByRole("button", { name: /^Xem như / })).toHaveLength(0);
  });

  it("no app: no 'Xem như' button, honest sentence", async () => {
    ui(null);
    expect(await screen.findByText(/Chưa có app nào để xem/)).toBeTruthy();
    expect(screen.queryAllByRole("button", { name: /^Xem như / })).toHaveLength(0);
  });

  it("inside a customer workspace with the flag on: buttons show, no 'not yet' sentence, opens with the project's app", async () => {
    inWorkspace.mockReturnValue(true);
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Lễ tân" }));
    await waitFor(() => expect(viewAsRole).toHaveBeenCalledWith({ roleId: "r3", appId: "a1", appSlug: "crm" }));
    expect(screen.queryByText("Xem như vai chưa có trong bản Nexus này.")).toBeNull();
  });

  it("workspace ended mid-click: business sentence, no retry button", async () => {
    inWorkspace.mockReturnValue(true);
    viewAsRole.mockRejectedValueOnce(new ApiError(401, "WORKSPACE_ENDED", "x"));
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xem như Lễ tân" }));
    expect(await screen.findByText("Phiên dựng đã kết thúc. Tải lại trang để vào lại dự án.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Thử lại" })).toBeNull();
  });

  it("inside a customer workspace with the flag off: no button", async () => {
    inWorkspace.mockReturnValue(true);
    flag.previewInWorkspace = false;
    ui();
    await screen.findByText("Lễ tân");
    expect(screen.queryAllByRole("button", { name: /^Xem như / })).toHaveLength(0);
    expect(screen.queryByText("Xem như vai chưa có trong bản Nexus này.")).toBeNull();
  });

  it("the project-owner role carries a note, other roles do not", async () => {
    listRoles.mockResolvedValue([...ROLES, { id: "r9", name: "tenant_owner", label: "Chủ khách" }]);
    ui();
    expect(await screen.findByText("Vai chủ khách. Xem thử vẫn chỉ xem, không ghi được.")).toBeTruthy();
    expect(screen.getAllByText(/Vai chủ khách\./)).toHaveLength(1);
  });

  it("load error shows the retry state", async () => {
    listRoles.mockRejectedValue(new Error("down"));
    ui();
    expect(await screen.findByRole("button", { name: "Thử lại" })).toBeTruthy();
  });

  describe("seen-as note (f5-trial-seen-note)", () => {
    const row = (roleId: unknown, roleName: unknown, at: string) => ({
      id: at, actorUserId: "u", entityName: "__preview__", recordId: at, action: "create", at,
      diff: { after: { roleId, roleName } },
    });

    it("two roles seen: sentence names them and the column shows a date only for those", async () => {
      listAuditFiltered.mockResolvedValue({
        entries: [row("r3", "Lễ tân", "2026-10-14T03:00:00Z"), row("r2", "Quản lý", "2026-10-13T03:00:00Z"), row("r3", "Lễ tân", "2026-10-12T03:00:00Z")],
        limit: 100,
      });
      ui();
      expect(await screen.findByText("Đã xem như: Lễ tân, Quản lý.")).toBeTruthy();
      expect(screen.getByText("Lần xem gần nhất")).toBeTruthy();
      expect(listAuditFiltered).toHaveBeenCalledWith({ entityName: "__preview__", action: "create", limit: 100 });
    });

    it("no audit rows: no sentence and no column", async () => {
      ui();
      await screen.findByRole("button", { name: "Xem như Lễ tân" });
      expect(screen.queryByText(/Đã xem như/)).toBeNull();
      expect(screen.queryByText("Lần xem gần nhất")).toBeNull();
    });

    it("rows without a known role are dropped; audit failure never breaks the table", async () => {
      listAuditFiltered.mockResolvedValue({ entries: [row(undefined, undefined, "2026-10-14T03:00:00Z"), row("gone", "Vai cũ", "2026-10-14T04:00:00Z")], limit: 100 });
      ui();
      await screen.findByRole("button", { name: "Xem như Lễ tân" });
      expect(screen.queryByText(/Đã xem như/)).toBeNull();
      listAuditFiltered.mockRejectedValue(new Error("403"));
      ui();
      expect((await screen.findAllByRole("button", { name: "Xem như Lễ tân" })).length).toBeGreaterThan(0);
    });

    it("no manual 'seen' control", async () => {
      listAuditFiltered.mockResolvedValue({ entries: [row("r1", "x", "2026-10-14T03:00:00Z")], limit: 100 });
      ui();
      await screen.findByText(/Đã xem như/);
      expect(screen.queryByRole("button", { name: /đánh dấu/i })).toBeNull();
      expect(screen.queryByText(/\d\/3/)).toBeNull();
    });
  });
});

describe("TrialRolesTable: column 'Thấy'", () => {
  it("lists up to 3 seen tables then '+n bảng', and the unseen ones the same way; explicit none beats *", async () => {
    getRole.mockImplementation(async (id: string) => ({
      id, name: id, tenantId: "t",
      actions: id === "r1" ? [g("*", "view"), g("a", "view", "none"), g("b", "view", "none")] : id === "r2" ? [g("a", "view")] : [],
    }));
    ui();
    const row1 = (await screen.findByRole("row", { name: /Nhân viên kinh doanh/ }));
    await waitFor(() => expect(row1.textContent).toContain("Thấy Bảng c, Bảng d, Bảng e."));
    expect(row1.textContent).toContain("Không thấy Bảng a, Bảng b.");
    const row2 = screen.getByRole("row", { name: /Quản lý/ });
    await waitFor(() => expect(row2.textContent).toContain("Thấy Bảng a."));
    expect(row2.textContent).toContain("Không thấy Bảng b, Bảng c, Bảng d, +1 bảng.");
  });

  it("view-only counts as seen; a role with no grants says 'Chưa thấy bảng nào' with a link to the role page", async () => {
    getRole.mockImplementation(async (id: string) => ({ id, name: id, tenantId: "t", actions: id === "r1" ? [g("a", "view")] : [] }));
    ui();
    const link = (await screen.findAllByRole("link", { name: "Đặt quyền" }))[0];
    expect(link.getAttribute("href")).toBe("/studio/du-an/northwind/dung/nguoi-dung-quyen/vai/r2");
    expect(screen.getAllByText(/Chưa thấy bảng nào/)).toHaveLength(2);
    expect(screen.queryByText(/^Vai .* thấy/)).toBeNull();
    expect(screen.getByText("Đổi ở bước Người dùng & quyền.")).toBeTruthy();
  });

  it("getRole once per role, not again on re-render; project switch refetches and drops the old project", async () => {
    const v = ui();
    await waitFor(() => expect(getRole).toHaveBeenCalledTimes(3));
    v.rerenderSlug("northwind");
    expect(getRole).toHaveBeenCalledTimes(3);
    v.rerenderSlug("acme");
    await waitFor(() => expect(getRole).toHaveBeenCalledTimes(6));
  });
});
