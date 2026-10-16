// FE-BPM-VERSIONS (specs/043 T234-T237): tabs, draft / publish, the "hồ sơ còn chạy bản cũ" notice + the
// move modal (dry run first), the stuck-hồ sơ tab and the N in the delete confirm. lib/api mocked at the seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalBuilder } from "./ApprovalBuilder";
import * as api from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(), listEntities: vi.fn(), listAdminUsers: vi.fn(), listRoles: vi.fn(), listFields: vi.fn(),
    listApprovalProcesses: vi.fn(), getApprovalProcess: vi.fn(), createApprovalProcess: vi.fn(), updateApprovalProcess: vi.fn(),
    deleteApprovalProcess: vi.fn(), saveApprovalDraft: vi.fn(), discardApprovalDraft: vi.fn(), publishApprovalProcess: vi.fn(),
    listApprovalVersions: vi.fn(), getApprovalVersion: vi.fn(), getApprovalImpact: vi.fn(), repinApprovalInstances: vi.fn(),
    listApprovalProcessInstances: vi.fn(), retryApprovalInstance: vi.fn(), cancelApprovalInstance: vi.fn(), overrideApproval: vi.fn(),
  };
});

const m = <T extends keyof typeof api>(k: T) => vi.mocked(api[k] as never) as unknown as ReturnType<typeof vi.fn>;

const P: api.ApprovalProcessSummary = {
  id: "p1", name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", isActive: true, version: 2,
  steps: [{ id: "s1", approverType: "role", approverId: "r-dir", slaSeconds: 3600 }],
};
const WITH_DRAFT: api.ApprovalProcessSummary = { ...P, draft: { name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", steps: P.steps, baseVersion: 2 } };
const IMPACT = (old: number): api.ApprovalImpact => ({ version: 2, pendingTotal: old, pendingOnOldDefinition: old, byNode: [] });

beforeEach(() => {
  for (const k of Object.keys(api)) { const f = (api as Record<string, unknown>)[k]; if (vi.isMockFunction(f)) f.mockReset(); }
  m("ensureAuth").mockResolvedValue(undefined);
  m("listApprovalProcesses").mockResolvedValue([P]);
  m("listEntities").mockResolvedValue([{ name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" }]);
  m("listAdminUsers").mockResolvedValue([]);
  m("listRoles").mockResolvedValue([{ id: "r-dir", name: "director", label: "Giám đốc" }]);
  m("listFields").mockResolvedValue([]);
  m("getApprovalImpact").mockResolvedValue(IMPACT(0));
  m("listApprovalVersions").mockResolvedValue([
    { id: "v2", processId: "p1", version: 2, note: "thêm bước", createdBy: null, createdAt: "2026-10-01T00:00:00Z" },
    { id: "v1", processId: "p1", version: 1, note: null, createdBy: null, createdAt: "2026-09-01T00:00:00Z" },
  ]);
  m("listApprovalProcessInstances").mockResolvedValue([]);
});

function renderBuilder() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><ApprovalBuilder /></QueryClientProvider>);
}

describe("ApprovalBuilder — versions", () => {
  it("shows the three tabs and the active version, and lists versions with the active one marked", async () => {
    renderBuilder();
    await screen.findByRole("tab", { name: "Phiên bản" });
    expect(screen.getByRole("tab", { name: "Định nghĩa" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Hồ sơ đang chạy" })).toBeTruthy();
    await userEvent.click(screen.getByRole("tab", { name: "Phiên bản" }));
    const grid = await screen.findByRole("grid", { name: "Lịch sử phiên bản" });
    expect(within(grid).getByText("Đang dùng")).toBeTruthy();
    expect(within(grid).getAllByText(/Phiên bản [12]/)).toHaveLength(2);
  });

  it("saves a draft, then publishes it with a note", async () => {
    m("saveApprovalDraft").mockResolvedValue(WITH_DRAFT);
    m("publishApprovalProcess").mockResolvedValue({ process: { ...P, version: 3 }, version: 3, impact: IMPACT(0) });
    renderBuilder();
    await userEvent.click(await screen.findByRole("button", { name: "Lưu nháp" }));
    await waitFor(() => expect(api.saveApprovalDraft).toHaveBeenCalledWith("p1", expect.objectContaining({ name: "Duyệt hợp đồng" })));
    await userEvent.click(await screen.findByRole("button", { name: "Xuất bản" }));
    await userEvent.type(await screen.findByLabelText(/Ghi chú phiên bản/), "thêm bước");
    expect(screen.queryByRole("dialog")).toBeNull(); // in place, no dialog
    const [confirm] = screen.getAllByRole("button", { name: "Xuất bản" });
    await userEvent.click(confirm);
    await waitFor(() => expect(api.publishApprovalProcess).toHaveBeenCalledWith("p1", "thêm bước"));
    expect(await screen.findByText("Đã xuất bản phiên bản 3")).toBeTruthy();
  });

  it("discards a draft after a confirm", async () => {
    m("listApprovalProcesses").mockResolvedValue([WITH_DRAFT]);
    m("discardApprovalDraft").mockResolvedValue(undefined);
    m("getApprovalProcess").mockResolvedValue(P);
    renderBuilder();
    expect(await screen.findByText(/Bạn đang sửa bản nháp/)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Bỏ nháp" }));
    await userEvent.click(await screen.findByRole("button", { name: "Bỏ bản nháp" }));
    await waitFor(() => expect(api.discardApprovalDraft).toHaveBeenCalledWith("p1"));
    await waitFor(() => expect(screen.queryByText(/Bạn đang sửa bản nháp/)).toBeNull());
  });

  it("warns about hồ sơ on the old version and moves them: preview (dry run) first, then apply", async () => {
    m("getApprovalImpact").mockResolvedValue(IMPACT(2));
    const row = { fromVersion: 1, toVersion: 2, fromNode: "duyet1", toNode: "duyet1" };
    m("repinApprovalInstances")
      .mockResolvedValueOnce({ results: [{ instanceId: "aaaaaaaa-1", ok: true, ...row }, { instanceId: "bbbbbbbb-2", ok: false, reason: "bước đã bị xoá", ...row }], remaining: 0 })
      .mockResolvedValueOnce({ results: [{ instanceId: "aaaaaaaa-1", ok: true, ...row }], remaining: 0 });
    renderBuilder();
    expect(await screen.findByText(/Còn 2 hồ sơ đang chờ vẫn chạy phiên bản cũ/)).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Chuyển sang bản mới" }));
    const dialog = await screen.findByRole("region", { name: "Chuyển hồ sơ đang chờ sang phiên bản 2" });
    const apply = within(dialog).getByRole("button", { name: "Áp dụng" });
    expect((apply as HTMLButtonElement).disabled).toBe(true); // nothing applied before a preview
    await userEvent.click(within(dialog).getByRole("button", { name: "Xem trước" }));
    await waitFor(() => expect(api.repinApprovalInstances).toHaveBeenNthCalledWith(1, "p1", expect.objectContaining({ dryRun: true, toVersion: 2 })));
    expect(await within(dialog).findByText("1 hồ sơ chuyển được · 1 ở lại bản cũ")).toBeTruthy();
    expect(within(dialog).getByText(/bước đã bị xoá/)).toBeTruthy();
    await userEvent.click(within(dialog).getByRole("button", { name: "Áp dụng" }));
    await waitFor(() => expect(api.repinApprovalInstances).toHaveBeenNthCalledWith(2, "p1", expect.objectContaining({ dryRun: false })));
  });

  it("says how many hồ sơ are waiting in the delete confirm", async () => {
    m("getApprovalImpact").mockResolvedValue(IMPACT(3));
    renderBuilder();
    await userEvent.click(await screen.findByRole("button", { name: "Xoá" }));
    expect(await screen.findByText(/Còn 3 hồ sơ đang chờ\. Xoá quy trình không huỷ chúng/)).toBeTruthy();
  });

  it("lists stuck hồ sơ and retries one", async () => {
    m("listApprovalProcessInstances").mockResolvedValue([{
      id: "iiiiiiii-1", entityName: "crm_contracts", recordId: "rrrrrrrr-1", status: "pending", processVersion: 1, nodeId: "n1",
      stepName: "Gọi cổng thanh toán", slaDueAt: null, escalated: false, createdAt: "2026-10-01T00:00:00Z",
      lastError: "HTTP 502", stuck: true, effectFailed: true,
    }]);
    m("retryApprovalInstance").mockResolvedValue(undefined);
    renderBuilder();
    await userEvent.click(await screen.findByRole("tab", { name: "Hồ sơ đang chạy" }));
    expect(await screen.findByText("HTTP 502")).toBeTruthy();
    expect(screen.getByText("Kẹt")).toBeTruthy();
    await userEvent.click(screen.getByRole("button", { name: "Thử lại hồ sơ iiiiiiii" }));
    await waitFor(() => expect(api.retryApprovalInstance).toHaveBeenCalledWith("iiiiiiii-1"));
  });

  it("override needs a reason", async () => {
    m("listApprovalProcessInstances").mockResolvedValue([{
      id: "jjjjjjjj-1", entityName: "crm_contracts", recordId: "rrrrrrrr-2", status: "pending", processVersion: 2, nodeId: "s1",
      stepName: "Giám đốc duyệt", slaDueAt: null, escalated: false, createdAt: "2026-10-01T00:00:00Z", stuck: false, effectFailed: false,
    }]);
    m("overrideApproval").mockResolvedValue(undefined);
    renderBuilder();
    await userEvent.click(await screen.findByRole("tab", { name: "Hồ sơ đang chạy" }));
    await userEvent.click(await screen.findByRole("button", { name: "Quyết thay hồ sơ jjjjjjjj" }));
    await userEvent.click(await screen.findByRole("button", { name: "Duyệt" }));
    expect(await screen.findByText("Nhập lý do trước khi quyết thay.")).toBeTruthy();
    expect(api.overrideApproval).not.toHaveBeenCalled();
    await userEvent.type(screen.getByLabelText(/Lý do/), "khách gấp");
    await userEvent.click(screen.getByRole("button", { name: "Duyệt" }));
    await waitFor(() => expect(api.overrideApproval).toHaveBeenCalledWith("jjjjjjjj-1", "approve", "khách gấp"));
  });
});
