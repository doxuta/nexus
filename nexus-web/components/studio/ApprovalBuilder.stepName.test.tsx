// ApprovalBuilder — a step's name follows its approver (DAILY-QUEUE #69 review).
// Studio has no step-name input (FE-approval-step-name-input, Phase 2 on SLDS), but it loads and
// saves whole step objects, so a name set by a pack or the AI Builder rides along. Since #69 the
// approval screen leads each step with that name — so re-pointing a step at another approver must
// drop the old name, or a non-admin reads "Trưởng phòng duyệt" over a step someone else now owns.
// lib/api mocked at the module seam; the builder's state + save path are real code.
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


const PROCESS: ApprovalProcessSummary = {
  id: "p1", name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", isActive: true,
  steps: [{ name: "Trưởng phòng duyệt", approverType: "role", approverId: "r-mgr", slaSeconds: 3600 }],
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listApprovalProcesses).mockReset().mockResolvedValue([PROCESS]);
  vi.mocked(listEntities).mockReset().mockResolvedValue([
    { name: "crm_contracts", label: "Hợp đồng", tableName: "obj_crm_contracts" },
  ]);
  vi.mocked(listAdminUsers).mockReset().mockResolvedValue([
    { id: "u-1", email: "an@example.com", fullName: "Nguyễn An" },
  ] as Awaited<ReturnType<typeof listAdminUsers>>);
  vi.mocked(listRoles).mockReset().mockResolvedValue([
    { id: "r-mgr", name: "manager", label: "Trưởng phòng" },
    { id: "r-dir", name: "director", label: "Giám đốc" },
  ]);
  vi.mocked(listFields).mockReset().mockResolvedValue([]);
  vi.mocked(updateApprovalProcess).mockReset().mockImplementation(async (_id, p) => ({ ...PROCESS, ...p }));
});

async function renderLoaded() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <ApprovalBuilder />
    </QueryClientProvider>,
  );
  // The first process auto-loads into the editor; its role step's approver lookup shows the
  // manager role as the selected value.
  await screen.findByText("Trưởng phòng");
}

// Opens the step's approver lookup from its selected value and chooses another (or the same) option.
async function repoint(current: string, next: RegExp) {
  fireEvent.click(screen.getByText(current).closest('[role="combobox"]')!);
  fireEvent.click(await screen.findByRole("option", { name: next }));
}

async function savedSteps() {
  fireEvent.click(screen.getByRole("button", { name: /Cập nhật/ }));
  await waitFor(() => expect(updateApprovalProcess).toHaveBeenCalledTimes(1));
  return vi.mocked(updateApprovalProcess).mock.calls[0][1].steps;
}

describe("ApprovalBuilder — step name follows the approver", () => {
  it("drops the name when the step is re-pointed at another role", async () => {
    await renderLoaded();
    await repoint("Trưởng phòng", /Giám đốc/);
    const steps = await savedSteps();
    expect(steps[0]).toEqual({ approverType: "role", approverId: "r-dir", slaSeconds: 3600 });
  });

  it("drops the name when the step switches to a user approver", async () => {
    await renderLoaded();
    fireEvent.change(screen.getByLabelText("Người nhận là"), { target: { value: "user" } });
    fireEvent.change(screen.getByPlaceholderText("Tìm người phê duyệt…"), { target: { value: "an" } });
    fireEvent.click(await screen.findByRole("option", { name: /Nguyễn An/ }));
    const steps = await savedSteps();
    expect(steps[0]).toEqual({ approverType: "user", approverId: "u-1", slaSeconds: 3600 });
  });

  it("keeps the name when only the SLA changes", async () => {
    await renderLoaded();
    fireEvent.change(screen.getByRole("spinbutton"), { target: { value: "4" } });
    const steps = await savedSteps();
    expect(steps[0]).toEqual({ name: "Trưởng phòng duyệt", approverType: "role", approverId: "r-mgr", slaSeconds: 14400 });
  });

  it("keeps the name when the same approver is picked again", async () => {
    await renderLoaded();
    await repoint("Trưởng phòng", /Trưởng phòng/);
    const steps = await savedSteps();
    expect(steps[0].name).toBe("Trưởng phòng duyệt");
  });
});
