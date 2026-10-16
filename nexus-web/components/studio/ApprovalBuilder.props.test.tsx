// ApprovalBuilder — step properties of FE-BPM-PROPS-a (specs/043 T222/T223/T225/T286, T295):
// a graph-stored process is read (was "0 bước"), a step can be held by a team / several people with a
// quorum, a rejection can return to the submitter with a value written on the record, a 422 lands on
// its step, and a 409 "would erase the graph" becomes a sentence plus an explicit ?force=true save.
// lib/api mocked at the module seam; the builder is real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalBuilder } from "./ApprovalBuilder";
import {
  ApiError, ensureAuth, listEntities, listAdminUsers, listRoles, listFields, listWorkQueuesAdmin,
  listApprovalProcesses, updateApprovalProcess, type ApprovalProcessSummary,
} from "@/lib/api";
import { listTeams } from "@/lib/admin";

vi.mock("@/lib/admin", async (importOriginal) => ({ ...(await importOriginal<typeof import("@/lib/admin")>()), listTeams: vi.fn() }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(), listEntities: vi.fn(), listAdminUsers: vi.fn(), listRoles: vi.fn(), listFields: vi.fn(),
    listWorkQueuesAdmin: vi.fn(), listApprovalProcesses: vi.fn(), createApprovalProcess: vi.fn(), updateApprovalProcess: vi.fn(), deleteApprovalProcess: vi.fn(),
  };
});

const LINEAR: ApprovalProcessSummary = {
  id: "p1", name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", isActive: true,
  steps: [{ id: "s1", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 }, { id: "s2", approverType: "role", approverId: "r-dir", slaSeconds: 3600 }],
};
const GRAPH: ApprovalProcessSummary = {
  id: "p2", name: "Duyệt song song", entityName: "crm_contracts", trigger: "manual", isActive: true, steps: [],
  options: { fallbackAssignee: { type: "role", id: "r-dir" }, lane: "exclusive" },
  graph: { nodes: [
    { id: "a", kind: "approval", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 },
    { id: "b", kind: "approval", approvers: [{ type: "user", id: "u-1" }, { type: "role", id: "r-dir" }], completion: { mode: "quorum", percent: 60 }, slaSeconds: 7200 },
  ] },
};

function setup(list: ApprovalProcessSummary[]) {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue(list);
  vi.mocked(listEntities).mockReset().mockResolvedValue([{ name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" }]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([{ id: "u-1", email: "an@example.com", fullName: "Nguyễn An" }] as Awaited<ReturnType<typeof listAdminUsers>>);
  vi.mocked(listRoles).mockReset().mockResolvedValue([{ id: "r-mgr", name: "manager", label: "Trưởng phòng" }, { id: "r-dir", name: "director", label: "Giám đốc" }]);
  vi.mocked(listTeams).mockReset().mockResolvedValue([{ id: "t-1", tenantId: "x", name: "Ban duyệt" }]);
  vi.mocked(listWorkQueuesAdmin).mockReset().mockResolvedValue([]);
  vi.mocked(listFields).mockReset().mockResolvedValue([]);
  vi.mocked(updateApprovalProcess).mockReset().mockImplementation(async (id, p) => ({ ...LINEAR, ...p, id }) as ApprovalProcessSummary);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><ApprovalBuilder /></QueryClientProvider>);
}

beforeEach(() => vi.clearAllMocks());

async function save() {
  fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
  await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalled());
}

describe("ApprovalBuilder — graph-stored process", () => {
  it("shows the nodes as steps (not '0 bước') and saves them back as a graph with options intact", async () => {
    setup([GRAPH]);
    await screen.findByText("Bước 2");
    expect(screen.getByText("Hợp đồng · 2 bước")).toBeTruthy();
    expect((screen.getAllByLabelText("Tỷ lệ cần duyệt")[0] as HTMLInputElement).value).toBe("60");
    await save();
    const body = vi.mocked(updateApprovalProcess).mock.calls[0][1];
    expect(body.graph?.nodes.map((n) => [n.id, n.kind])).toEqual([["a", "approval"], ["b", "approval"]]);
    expect(body.graph?.nodes[1]).toMatchObject({ approvers: [{ type: "user", id: "u-1" }, { type: "role", id: "r-dir" }], completion: { mode: "quorum", percent: 60 } });
    expect(body.options).toEqual({ lane: "exclusive", fallbackAssignee: { type: "role", id: "r-dir" } });
    expect(vi.mocked(updateApprovalProcess).mock.calls[0][2]).toBeUndefined();
  });

  it("a branching graph opens as editable nodes and goes back unchanged", async () => {
    const graph = { nodes: [{ id: "g", kind: "gateway", branches: [], default: "e" }, { id: "e", kind: "end", result: "approved" }] };
    setup([{ ...GRAPH, graph }]);
    await screen.findByText("Nhánh mặc định (khi không nhánh nào khớp)");
    await save();
    expect(vi.mocked(updateApprovalProcess).mock.calls[0][1].graph).toEqual(graph);
  });
});

describe("ApprovalBuilder — step properties", () => {
  it("team step with a strategy is sent as an assignment (no approverType)", async () => {
    setup([LINEAR]);
    await screen.findByText("Bước 2");
    fireEvent.change(screen.getAllByLabelText("Người nhận là")[0], { target: { value: "team" } });
    fireEvent.click(screen.getByPlaceholderText("Tìm nhóm…"));
    fireEvent.click(await screen.findByRole("option", { name: /Ban duyệt/ }));
    fireEvent.change(screen.getByLabelText("Cách chia việc trong nhóm"), { target: { value: "round_robin" } });
    await save();
    const first = vi.mocked(updateApprovalProcess).mock.calls[0][1].steps[0];
    expect(first).toEqual({ id: "s1", slaSeconds: 3600, assignment: { type: "team", id: "t-1", strategy: "round_robin" } });
  });

  it("many people + quorum 60%, then 'back to the submitter' with a returned value", async () => {
    setup([{ ...LINEAR, outcome: { statusField: "stage", approvedValue: "Duyệt", rejectedValue: "Từ chối" } }]);
    await screen.findByText("Bước 2");
    fireEvent.change(screen.getAllByLabelText("Người nhận là")[1], { target: { value: "many" } });
    fireEvent.click(screen.getAllByPlaceholderText("Tìm người phê duyệt…")[0]);
    fireEvent.click(await screen.findByRole("option", { name: /Nguyễn An/ }));
    // second approver: switch it to a role
    fireEvent.click(screen.getAllByRole("radio", { name: "Vai trò" })[1]);
    fireEvent.click(screen.getByPlaceholderText("Tìm vai trò…"));
    fireEvent.click(await screen.findByRole("option", { name: /Giám đốc/ }));
    fireEvent.change(screen.getByLabelText("Khi nào xong bước"), { target: { value: "quorum" } });
    fireEvent.change(screen.getByLabelText("Tỷ lệ cần duyệt"), { target: { value: "60" } });
    fireEvent.change(screen.getAllByLabelText("Khi từ chối")[1], { target: { value: "@submitter" } });
    fireEvent.change(screen.getByLabelText("Giá trị ghi lên hồ sơ khi trả về"), { target: { value: "Cần bổ sung" } });
    await save();
    const body = vi.mocked(updateApprovalProcess).mock.calls[0][1];
    expect(body.steps[1]).toEqual({
      id: "s2", slaSeconds: 3600,
      approvers: [{ type: "user", id: "u-1" }, { type: "role", id: "r-dir" }],
      completion: { mode: "quorum", percent: 60 }, onReject: { to: "@submitter", resumeAt: "rejector" },
    });
    expect(body.outcome).toEqual({ statusField: "stage", approvedValue: "Duyệt", rejectedValue: "Từ chối", returnedValue: "Cần bổ sung" });
  });

  it("an incomplete step is refused on its own card, no request sent", async () => {
    setup([LINEAR]);
    await screen.findByText("Bước 2");
    fireEvent.change(screen.getAllByLabelText("Người nhận là")[0], { target: { value: "team" } });
    fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
    await screen.findByText("Bước 1: chọn người nhận.");
    expect(updateApprovalProcess).not.toHaveBeenCalled();
  });

  it("moving a step keeps both ids and a rejection that no longer points back is reset", async () => {
    setup([{ ...LINEAR, steps: [LINEAR.steps![0], { ...LINEAR.steps![1], onReject: { to: "s1" } }] }]);
    await screen.findByText("Bước 2");
    fireEvent.click(screen.getByRole("button", { name: "Đưa bước 2 lên" }));
    await save();
    const steps = vi.mocked(updateApprovalProcess).mock.calls[0][1].steps;
    expect(steps.map((s) => s.id)).toEqual(["s2", "s1"]);
    expect(steps[0].onReject).toBeUndefined();
  });
});

describe("ApprovalBuilder — server refusals", () => {
  it("a 422 on steps[1] shows on step 2", async () => {
    setup([LINEAR]);
    await screen.findByText("Bước 2");
    vi.mocked(updateApprovalProcess).mockRejectedValueOnce(new ApiError(422, "APPROVAL_STEP_INVALID", "bước 2: người nhận không còn", {}));
    await save();
    expect(await screen.findByText("bước 2: người nhận không còn")).toBeTruthy();
  });

  it("409 V2_OVERWRITE: a sentence and an explicit button that saves with force", async () => {
    setup([LINEAR]);
    await screen.findByText("Bước 2");
    vi.mocked(updateApprovalProcess).mockRejectedValueOnce(new ApiError(409, "APPROVAL_PROCESS_V2_OVERWRITE", "x", { field: "steps" }));
    await save();
    await screen.findByText(/Lưu theo dạng từng bước một sẽ xoá phần đó/);
    expect(updateApprovalProcess).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Đổi về dạng tuyến tính (xoá đồ thị)" }));
    await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(2));
    expect(vi.mocked(updateApprovalProcess).mock.calls[1][2]).toEqual({ force: true });
  });
});
