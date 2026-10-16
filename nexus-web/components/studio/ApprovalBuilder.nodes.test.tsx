// ApprovalBuilder — FE-BPM-PROPS-b (specs/043 T224/T226/T227): a branching process is edited as nodes,
// the wait node's SLA must say who is told, pauseWhen and the pinned lane ride in the save, and the
// pause condition is editable. lib/api mocked at the module seam; the builder is real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalBuilder } from "./ApprovalBuilder";
import {
  ensureAuth, listEntities, listAdminUsers, listRoles, listFields, listWorkQueuesAdmin,
  listApprovalProcesses, updateApprovalProcess, type ApprovalProcessSummary,
} from "@/lib/api";
import { listTeams } from "@/lib/admin";

vi.mock("@/lib/admin", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/admin")>()), listTeams: vi.fn(), listBusinessCalendars: vi.fn().mockResolvedValue([{ id: "c1", name: "Giờ hành chính" }]) }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(), listEntities: vi.fn(), listAdminUsers: vi.fn(), listRoles: vi.fn(), listFields: vi.fn(),
    listWorkQueuesAdmin: vi.fn(), listApprovalProcesses: vi.fn(), createApprovalProcess: vi.fn(), updateApprovalProcess: vi.fn(), deleteApprovalProcess: vi.fn(),
  };
});

const WAIT_NO_NOTIFY: ApprovalProcessSummary = {
  id: "p3", name: "Chờ khách", entityName: "crm_contracts", trigger: "after_update", isActive: true, steps: [],
  options: { lane: "independent", calendarId: "c1", pauseWhen: { match: "and", rules: [{ field: "stage", op: "eq", value: "Chờ khách" }] } },
  graph: { nodes: [
    { id: "w", kind: "wait", until: { match: "and", rules: [{ field: "stage", op: "eq", value: "Đã trả lời" }] }, sla: { amount: 4, unit: "business_hours" } },
    { id: "e", kind: "end", result: "approved" },
  ] },
};

function setup(p: ApprovalProcessSummary) {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([p]);
  vi.mocked(listEntities).mockReset().mockResolvedValue([{ name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" }]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([]);
  vi.mocked(listRoles).mockReset().mockResolvedValue([]);
  vi.mocked(listTeams).mockReset().mockResolvedValue([]);
  vi.mocked(listWorkQueuesAdmin).mockReset().mockResolvedValue([]);
  vi.mocked(listFields).mockReset().mockResolvedValue([{ name: "stage", label: "Giai đoạn", fieldType: "text" }]);
  vi.mocked(updateApprovalProcess).mockReset().mockImplementation(async (id, b) => ({ ...p, ...b, id }) as ApprovalProcessSummary);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><ApprovalBuilder /></QueryClientProvider>);
}

beforeEach(() => vi.clearAllMocks());

describe("ApprovalBuilder — wait node, SLA, options (PROPS-b)", () => {
  it("refuses a wait SLA nobody is told about, pinned to its step, without calling the API", async () => {
    setup(WAIT_NO_NOTIFY);
    await screen.findByText("Chờ đến khi hồ sơ thoả");
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
    expect(await screen.findByText("Bước 1: chọn người được báo khi quá hạn.")).toBeTruthy();
    expect(updateApprovalProcess).not.toHaveBeenCalled();
  });

  it("once notify is chosen the save carries sla, lane, calendar and pauseWhen back", async () => {
    setup(WAIT_NO_NOTIFY);
    fireEvent.change(await screen.findByLabelText("Báo cho (khi quá hạn)"), { target: { value: "submitter" } });
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
    await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(1));
    const body = vi.mocked(updateApprovalProcess).mock.calls[0][1];
    expect(body.graph?.nodes[0]).toEqual({
      id: "w", kind: "wait", until: { match: "and", rules: [{ field: "stage", op: "eq", value: "Đã trả lời" }] },
      sla: { amount: 4, unit: "business_hours", notify: { type: "submitter" } },
    });
    expect(body.options).toEqual({ lane: "independent", calendarId: "c1", pauseWhen: WAIT_NO_NOTIFY.options?.pauseWhen });
  });
});
