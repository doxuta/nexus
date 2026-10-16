// ApprovalBuilder — a process whose entry criteria are OR (stored via the API since specs/043 S2b)
// survives a save from this screen. The screen authors AND only; it used to re-send every criteria
// block as match "and", so renaming a step silently turned "tổng > 50 triệu HOẶC chiết khấu > 20%"
// into "VÀ" and half the quotes stopped needing approval. lib/api mocked at the module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalBuilder } from "./ApprovalBuilder";
import {
  ensureAuth, listEntities, listAdminUsers, listRoles, listFields,
  listApprovalProcesses, updateApprovalProcess, type ApprovalProcessSummary,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(), listEntities: vi.fn(), listAdminUsers: vi.fn(), listRoles: vi.fn(),
    listFields: vi.fn(), listApprovalProcesses: vi.fn(), createApprovalProcess: vi.fn(),
    updateApprovalProcess: vi.fn(), deleteApprovalProcess: vi.fn(),
  };
});

const OR_CRITERIA = {
  match: "or",
  rules: [{ field: "total", op: "gt", value: "50000000" }, { field: "discount", op: "gt", value: "20" }],
};

const PROCESS = {
  id: "p1", name: "Duyệt báo giá lớn", entityName: "crm_quotes", trigger: "after_update", isActive: true,
  criteria: OR_CRITERIA,
  steps: [{ id: "s1", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 }],
} as ApprovalProcessSummary;

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listEntities).mockReset().mockResolvedValue([{ name: "crm_quotes", label: "Báo giá", tableName: "obj_crm_quotes" }]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([]);
  vi.mocked(listRoles).mockReset().mockResolvedValue([{ id: "r-mgr", name: "manager", label: "Trưởng phòng" }]);
  vi.mocked(listFields).mockReset().mockResolvedValue([]);
  vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([PROCESS]);
  vi.mocked(updateApprovalProcess).mockReset().mockImplementation(async (id, p) => ({ ...PROCESS, ...p, id }) as ApprovalProcessSummary);
});

describe("ApprovalBuilder — stored 'changed' criteria", () => {
  // "vừa đổi" (S2b) takes no value: the row shows no value box, and the save sends the rule without one.
  it("keeps its value box inactive and saves it without a value", async () => {
    vi.mocked(listApprovalProcesses).mockResolvedValue([
      { ...PROCESS, trigger: "after_update", criteria: { match: "and", rules: [{ field: "note", op: "changed" }] } } as ApprovalProcessSummary,
    ]);
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><ApprovalBuilder /></QueryClientProvider>);
    await screen.findByRole("button", { name: /Cập nhật/ });
    expect((screen.getByRole("textbox", { name: "Giá trị" }) as HTMLInputElement).disabled).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
    await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(1));
    expect(vi.mocked(updateApprovalProcess).mock.calls[0][1].criteria).toEqual({ match: "and", rules: [{ field: "note", op: "changed" }] });
  });
});

describe("ApprovalBuilder — stored OR criteria", () => {
  it("says HOẶC and sends match 'or' back on save", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><ApprovalBuilder /></QueryClientProvider>);
    expect(await screen.findByText(/BẤT KỲ điều kiện nào \(HOẶC\)/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
    await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(1));
    expect(vi.mocked(updateApprovalProcess).mock.calls[0][1].criteria).toEqual(OR_CRITERIA);
  });
});
