import { QueryClient, QueryClientProvider, useQuery } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

const adminApps = vi.fn();
const adminApp = vi.fn();
const listEntities = vi.fn();
const getSampleDataSummary = vi.fn();
const listAdminUsers = vi.fn();
const listRoles = vi.fn();
const wipeSampleData = vi.fn();
const getPublishState = vi.fn();
vi.mock("@/lib/api", async (orig) => ({
  ...(await orig<typeof import("@/lib/api")>()),
  adminApps: () => adminApps(),
  adminApp: (id: string) => adminApp(id),
  listEntities: () => listEntities(),
  getSampleDataSummary: () => getSampleDataSummary(),
  listAdminUsers: () => listAdminUsers(),
  listRoles: () => listRoles(),
  wipeSampleData: (n: number, e?: string[]) => wipeSampleData(n, e),
  getPublishState: () => getPublishState(),
  publishBaseline: vi.fn(),
}));
vi.mock("../useProjectQuery", () => ({
  useProjectQuery: (slug: string, key: unknown[], fn: () => Promise<unknown>) => useQuery({ queryKey: ["project", slug, ...key], queryFn: fn, staleTime: 0 }),
}));
vi.mock("@/lib/studioContext", () => ({ homeSlug: () => "northwind" }));

import { HandoffPage } from "./HandoffPage";

const RD = { entityCount: 1, entitiesMissingFields: [] as string[], allEntitiesHaveFields: true, hasNonAdminViewRole: true, hasNonAdminUserAssigned: true, ready: true };
const ui = (slug = "acme") =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <HandoffPage slug={slug} />
    </QueryClientProvider>,
  );

beforeEach(() => {
  vi.clearAllMocks();
  getPublishState.mockResolvedValue({ delivered: false, headVersion: null, pendingCount: 0 });
  adminApps.mockResolvedValue([{ id: "a1", name: "crm", label: "CRM" }]);
  adminApp.mockResolvedValue({ app: {}, items: [], readiness: RD });
  listEntities.mockResolvedValue([{ id: "e1", name: "student", label: "Học viên" }]);
  getSampleDataSummary.mockResolvedValue({ total: 0, entities: [] });
  listAdminUsers.mockResolvedValue([{ id: "u1", tenantId: "t", email: "chu@khach.vn", fullName: "", status: "active", isAdmin: false, canManageUsers: true, roleId: "r1" }]);
  listRoles.mockResolvedValue([{ id: "r1", name: "tenant_owner", label: "Chủ khách" }]);
});

describe("HandoffPage", () => {
  it("workshop: empty state", () => {
    ui("northwind");
    expect(screen.getByText("Dự án của xưởng Nexus không giao cho khách.")).toBeInTheDocument();
  });

  it("ready app: ok rows, an enabled deliver button, no card nesting", async () => {
    const { container } = ui();
    expect(await screen.findByText("App CRM sẵn sàng cho nhân viên.")).toBeInTheDocument();
    expect(screen.getByText("Không còn dữ liệu mẫu.")).toBeInTheDocument();
    const b = screen.getByRole("button", { name: "Giao cho khách: phiên bản 1" });
    expect(b.getAttribute("aria-disabled")).toBeNull();
    expect(container.querySelectorAll(".slds-card, .slds-badge, [role=progressbar]")).toHaveLength(0);
  });

  it("app with a table without fields: names the table by label, one link to that table", async () => {
    adminApp.mockResolvedValue({ app: {}, items: [], readiness: { ...RD, ready: false, allEntitiesHaveFields: false, entitiesMissingFields: ["student"] } });
    ui();
    expect(await screen.findByText("App CRM chưa sẵn sàng: bảng Học viên chưa có trường.")).toBeInTheDocument();
    const row = document.querySelector('[data-row="app:a1"]') as HTMLElement;
    const link = within(row).getByRole("link", { name: "Thêm trường CRM" });
    expect(link.getAttribute("href")).toContain("/dung/du-lieu/student");
    expect(screen.getByText("Còn 1 việc chặn")).toBeInTheDocument();
  });

  const SUM = (total: number) => ({ total, entities: total ? [{ entity: "student", label: "Học viên", count: total, live: total, inBin: 0, bySource: { generator: total, ai: 0, pack: 0 } }] : [] });

  it("sample rows block and offer Xoá ngay", async () => {
    getSampleDataSummary.mockResolvedValue(SUM(4));
    ui();
    expect(await screen.findByText(/Còn 4 bản ghi mẫu/)).toBeInTheDocument();
    const row = document.querySelector('[data-row="samples"]') as HTMLElement;
    expect(within(row).getByRole("button", { name: "Xoá ngay" })).toBeInTheDocument();
    expect(screen.getByText("Còn 1 việc chặn")).toBeInTheDocument();
  });

  it("Xoá ngay opens the confirm under the row and sends the whole-project total; success turns the row ok", async () => {
    getSampleDataSummary.mockResolvedValueOnce(SUM(4)).mockResolvedValue(SUM(0));
    wipeSampleData.mockResolvedValue({ deleted: [{ entity: "student", count: 4 }], kept: [], blobsRemoved: 0, sequencesReset: [] });
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xoá ngay" }));
    const row = document.querySelector('[data-row="samples"]') as HTMLElement;
    fireEvent.click(within(row).getByRole("button", { name: /^Xoá 4/ }));
    await waitFor(() => expect(wipeSampleData).toHaveBeenCalledWith(4, ["student"]));
    await waitFor(() => expect(screen.getByText("Không còn dữ liệu mẫu.")).toBeInTheDocument());
    expect(screen.queryByText(/việc chặn/)).toBeNull();
  });

  it("rows kept by the wipe: list shown, still blocking until the tick, then a reminder", async () => {
    getSampleDataSummary.mockResolvedValueOnce(SUM(4)).mockResolvedValue(SUM(1));
    wipeSampleData.mockResolvedValue({
      deleted: [{ entity: "student", count: 2 }],
      kept: [{ entity: "student", recordId: "11111111-aaaa", reason: "referenced_by_real", blockedBy: [{ entity: "invoice", field: "student_id", count: 1 }] }],
      blobsRemoved: 0, sequencesReset: [],
    });
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Xoá ngay" }));
    fireEvent.click(await screen.findByRole("button", { name: /^Xoá 4/ }));
    expect(await screen.findByText(/Còn 1 bản ghi mẫu/)).toBeInTheDocument();
    expect(screen.getByTestId("sample-kept-11111111-aaaa")).toBeInTheDocument();
    expect(screen.getByText("Còn 1 việc chặn")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("checkbox", { name: /vẫn giữ các bản ghi mẫu/ }));
    await waitFor(() => expect(screen.queryByText(/việc chặn/)).toBeNull());
  });

  it("app list failing is unknown + blocking, and Thử lại reads it again", async () => {
    adminApps.mockRejectedValueOnce(new Error("x"));
    ui();
    expect(await screen.findByText("Chưa kiểm được danh sách app.")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    await waitFor(() => expect(screen.getByText("App CRM sẵn sàng cho nhân viên.")).toBeInTheDocument());
    expect(adminApps).toHaveBeenCalledTimes(2);
  });

  it("one app detail failing does not hide the other app", async () => {
    adminApps.mockResolvedValue([{ id: "a1", name: "crm", label: "CRM" }, { id: "a2", name: "lms", label: "LMS" }]);
    adminApp.mockImplementation(async (id: string) => {
      if (id === "a2") throw new Error("boom");
      return { app: {}, items: [], readiness: RD };
    });
    ui();
    expect(await screen.findByText("App CRM sẵn sàng cho nhân viên.")).toBeInTheDocument();
    expect(screen.getByText("Chưa kiểm được độ sẵn sàng của app LMS.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Thử lại LMS" })).toBeInTheDocument();
  });

  it("owner row shows the account and role, and never blocks", async () => {
    ui();
    expect(await screen.findByText("Chủ khách đăng nhập bằng chu@khach.vn, vai Chủ khách.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Đổi vai" })).toBeInTheDocument();
    expect(screen.queryByText(/việc chặn/)).toBeNull();
  });

  it("no usable owner (only a locked one): reminder, still no block", async () => {
    listAdminUsers.mockResolvedValue([{ id: "u1", tenantId: "t", email: "chu@khach.vn", fullName: "", status: "inactive", isAdmin: false, canManageUsers: true }]);
    ui();
    expect(await screen.findByText("Chưa có chủ khách đăng nhập được.")).toBeInTheDocument();
    expect(screen.queryByText(/việc chặn/)).toBeNull();
  });

  it("owner read failing says it could not check, never 'no owner', and Thử lại reads again", async () => {
    listAdminUsers.mockRejectedValueOnce(new Error("x"));
    ui();
    expect(await screen.findByText("Chưa kiểm được chủ khách.")).toBeInTheDocument();
    expect(screen.queryByText("Chưa có chủ khách đăng nhập được.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    await waitFor(() => expect(screen.getByText("Chủ khách đăng nhập bằng chu@khach.vn, vai Chủ khách.")).toBeInTheDocument());
  });

  it("blocking row: deliver button is aria-disabled with the count", async () => {
    adminApp.mockResolvedValue({ app: {}, items: [], readiness: { ...RD, ready: false, allEntitiesHaveFields: false, entitiesMissingFields: ["student"] } });
    ui();
    const b = await screen.findByRole("button", { name: "Giao cho khách: phiên bản 1" });
    expect(b.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Còn 1 việc chưa xong ở trên.")).toHaveLength(1);
  });

  it("delivered: result screen, no checklist and no deliver button", async () => {
    getPublishState.mockResolvedValue({ delivered: true, headVersion: { versionNo: 1, kind: "baseline", publishedAt: "2026-10-14T03:00:00Z", publishedBy: "u" }, pendingCount: 0 });
    ui();
    expect(await screen.findByText(/App đã giao cho khách\. Khách đang dùng phiên bản 1, giao lúc/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Giao cho khách/ })).toBeNull();
    expect(screen.queryByText("App CRM sẵn sàng cho nhân viên.")).toBeNull();
    expect(screen.queryByText("Chưa có chủ khách đăng nhập được: khách chưa tự thêm nhân viên được.")).toBeNull();
  });

  it("delivered with no usable owner: warning + link to Người dùng; read failure never prints that warning", async () => {
    getPublishState.mockResolvedValue({ delivered: true, headVersion: { versionNo: 1, kind: "baseline", publishedAt: "2026-10-14T03:00:00Z", publishedBy: "u" }, pendingCount: 0 });
    listAdminUsers.mockResolvedValue([{ id: "u1", tenantId: "t", email: "chu@khach.vn", fullName: "", status: "inactive", isAdmin: false, canManageUsers: true }]);
    ui();
    expect(await screen.findByText("Chưa có chủ khách đăng nhập được: khách chưa tự thêm nhân viên được.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Mở Người dùng" }).getAttribute("href")).toContain("/van-hanh/nguoi-dung");
  });

  it("delivered, owner read failing: could-not-check + Thử lại, no false warning", async () => {
    getPublishState.mockResolvedValue({ delivered: true, headVersion: { versionNo: 1, kind: "baseline", publishedAt: "2026-10-14T03:00:00Z", publishedBy: "u" }, pendingCount: 0 });
    listAdminUsers.mockRejectedValue(new Error("x"));
    ui();
    expect(await screen.findByText("Chưa kiểm được chủ khách.")).toBeInTheDocument();
    expect(screen.queryByText(/khách chưa tự thêm nhân viên/)).toBeNull();
    expect(screen.getByRole("button", { name: "Thử lại" })).toBeInTheDocument();
  });

  it("clicking the blocked deliver button moves focus to the first blocking row's link", async () => {
    adminApp.mockResolvedValue({ app: {}, items: [], readiness: { ...RD, ready: false, allEntitiesHaveFields: false, entitiesMissingFields: ["student"] } });
    ui();
    fireEvent.click(await screen.findByRole("button", { name: "Giao cho khách: phiên bản 1" }));
    const row = document.querySelector('[data-row="app:a1"]') as HTMLElement;
    expect(within(row).getByRole("link", { name: "Thêm trường CRM" })).toHaveFocus();
  });

  it("delivered screen takes focus; delivering invalidates project, projects and studio caches", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(qc, "invalidateQueries");
    const { publishBaseline } = await import("@/lib/api");
    (publishBaseline as ReturnType<typeof vi.fn>).mockResolvedValue({ versionNo: 1, kind: "baseline", docCount: 1, publishedAt: "", propagationSeconds: 30 });
    render(
      <QueryClientProvider client={qc}>
        <HandoffPage slug="acme" />
      </QueryClientProvider>,
    );
    fireEvent.click(await screen.findByRole("button", { name: "Giao cho khách: phiên bản 1" }));
    fireEvent.click(screen.getByRole("button", { name: "Giao phiên bản 1" }));
    await waitFor(() => expect(spy).toHaveBeenCalledWith({ queryKey: ["studio"] }));
    expect(spy).toHaveBeenCalledWith({ queryKey: ["project", "acme"] });
    expect(spy).toHaveBeenCalledWith({ queryKey: ["projects"] });
    getPublishState.mockResolvedValue({ delivered: true, headVersion: { versionNo: 1, kind: "baseline", publishedAt: "2026-10-14T03:00:00Z", publishedBy: "u" }, pendingCount: 0 });
    await qc.invalidateQueries({ queryKey: ["project", "acme"] });
    const head = await screen.findByText(/App đã giao cho khách\./);
    expect(head.parentElement).toHaveFocus();
  });

  it("state read failing: Thử lại, never guesses 'not delivered'", async () => {
    getPublishState.mockRejectedValueOnce(new Error("x"));
    ui();
    expect(await screen.findByText("Chưa đọc được trạng thái giao.")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Giao cho khách/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(await screen.findByRole("button", { name: "Giao cho khách: phiên bản 1" })).toBeInTheDocument();
  });
});
