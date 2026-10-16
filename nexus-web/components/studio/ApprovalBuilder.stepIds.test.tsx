// ApprovalBuilder — a step keeps its stable id across saves in one session (specs/043 S0, FR-002).
// The server assigns an id to every step that arrives without one, by position (bpm.NormalizeSteps:
// s<i+1>, or the next free number). Studio used to keep its pre-save steps after a save, so a step
// it had just added stayed id-less locally; the next save re-sent it without an id and the server
// numbered it by its NEW position — e.g. taking over the id of a step deleted in between. Later
// version mapping (specs/043 S10/S11) keys on these ids, so a silent rename maps history wrongly.
// lib/api mocked at the module seam with the server's real id rule; the builder is real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalBuilder } from "./ApprovalBuilder";
import {
  ensureAuth, listEntities, listAdminUsers, listRoles, listFields,
  listApprovalProcesses, createApprovalProcess, updateApprovalProcess,
  type ApprovalProcessSummary, type ApprovalStep,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listEntities: vi.fn(),
    listAdminUsers: vi.fn(),
    listRoles: vi.fn(),
    listFields: vi.fn(),
    listApprovalProcesses: vi.fn(),
    createApprovalProcess: vi.fn(),
    updateApprovalProcess: vi.fn(),
    deleteApprovalProcess: vi.fn(),
  };
});

// Mirror of bpm.NormalizeSteps: keep every explicit id; give each missing one s<i+1>, or the next
// number no other step uses.
function normalize(steps: ApprovalStep[]): ApprovalStep[] {
  const used = new Set(steps.flatMap((s) => (s.id ? [s.id] : [])));
  return steps.map((s, i) => {
    if (s.id) return s;
    let n = i + 1;
    while (used.has(`s${n}`)) n++;
    used.add(`s${n}`);
    return { ...s, id: `s${n}` };
  });
}

const PROCESS: ApprovalProcessSummary = {
  id: "p1", name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", isActive: true,
  steps: [
    { id: "s1", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 },
    { id: "s2", approverType: "role", approverId: "r-dir", slaSeconds: 3600 },
  ],
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" },
  ]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([
    { id: "u-1", email: "an@example.com", fullName: "Nguyễn An" },
    { id: "u-2", email: "binh@example.com", fullName: "Trần Bình" },
  ] as Awaited<ReturnType<typeof listAdminUsers>>);
  vi.mocked(listRoles).mockReset().mockResolvedValue([
    { id: "r-mgr", name: "manager", label: "Trưởng phòng" },
    { id: "r-dir", name: "director", label: "Giám đốc" },
  ]);
  vi.mocked(listFields).mockReset().mockResolvedValue([]);
  vi.mocked(createApprovalProcess).mockReset().mockImplementation(async (p) =>
    ({ ...PROCESS, ...p, id: "p-new", steps: normalize(p.steps) }) as ApprovalProcessSummary);
  vi.mocked(updateApprovalProcess).mockReset().mockImplementation(async (id, p) =>
    ({ ...PROCESS, ...p, id, steps: normalize(p.steps) }) as ApprovalProcessSummary);
});

function renderBuilder() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <ApprovalBuilder />
    </QueryClientProvider>,
  );
}

// Picks a user for the (only) step still showing the empty approver lookup (SLDS combobox: a click
// lists every user, then choose the option).
async function pickUser(name: RegExp) {
  fireEvent.click(screen.getByPlaceholderText("Tìm người phê duyệt…"));
  fireEvent.click(await screen.findByRole("option", { name }));
}

async function save(label: RegExp, mock: typeof createApprovalProcess | typeof updateApprovalProcess, calls: number) {
  fireEvent.click(screen.getByRole("button", { name: label }));
  await waitFor(() => expect(mock).toHaveBeenCalledTimes(calls));
  await screen.findByText("Đã lưu");
}

describe("ApprovalBuilder — step ids stay put across saves in one session", () => {
  it("update: add a step, save, remove another, save — the added step keeps the id it was given", async () => {
    vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([PROCESS]);
    renderBuilder();
    await screen.findAllByText("Giám đốc");

    fireEvent.click(screen.getByRole("button", { name: /Thêm bước/ }));
    await pickUser(/Nguyễn An/);
    await save(/Cập nhật/, updateApprovalProcess, 1); // server stores the new step as s3

    fireEvent.click(screen.getByRole("button", { name: "Xoá bước 2" }));
    await save(/Cập nhật/, updateApprovalProcess, 2);

    const sent = vi.mocked(updateApprovalProcess).mock.calls[1][1].steps;
    expect(sent.map((s) => [s.id, s.approverId])).toEqual([["s1", "r-mgr"], ["s3", "u-1"]]);
  });

  it("create: create two steps, remove the first, save — the survivor stays s2", async () => {
    vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([]);
    renderBuilder();
    fireEvent.click(await screen.findByRole("button", { name: /Tạo quy trình/ }));
    await pickUser(/Nguyễn An/);
    fireEvent.click(screen.getByRole("button", { name: /Thêm bước/ }));
    await pickUser(/Trần Bình/);
    await save(/Tạo quy trình/, createApprovalProcess, 1); // server stores s1, s2

    fireEvent.click(screen.getByRole("button", { name: "Xoá bước 1" }));
    await save(/Cập nhật/, updateApprovalProcess, 1);

    const sent = vi.mocked(updateApprovalProcess).mock.calls[0][1].steps;
    expect(sent.map((s) => [s.id, s.approverId])).toEqual([["s2", "u-2"]]);
  });
});
