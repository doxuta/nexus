// specs/043 S9b / FE-BPM-QUEUE-WF T241: the rule action "Khởi động quy trình duyệt" (start_approval) is picked in
// the builder — a Combobox of the active MANUAL processes of the rule's own entity — and a server refusal
// (WF_START_APPROVAL_INVALID, details.field "/actions/N") lands on that action's card.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-sa", name: "Gửi duyệt khi đổi trạng thái", entityName: "crm_quotes", trigger: "after_update", isActive: true,
  condition: { match: "and", rules: [{ field: "status", op: "changedTo", value: "Sent" }] },
  actions: [{ type: "start_approval", processId: "p-manual" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "crm_quotes", label: "Báo giá" }]),
    listFields: vi.fn(async () => [{ name: "status", label: "Trạng thái", fieldType: "picklist" }]),
    listApprovalProcesses: vi.fn(async () => [
      { id: "p-manual", name: "Duyệt báo giá lớn", entityName: "crm_quotes", trigger: "manual", isActive: true },
      { id: "p-auto", name: "Tự duyệt khi tạo", entityName: "crm_quotes", trigger: "on_create", isActive: true },
      { id: "p-other", name: "Duyệt hợp đồng", entityName: "crm_contracts", trigger: "manual", isActive: true },
      { id: "p-off", name: "Quy trình đang tắt", entityName: "crm_quotes", trigger: "manual", isActive: false },
    ]),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});

beforeEach(() => vi.mocked(updateWorkflow).mockClear());

const save = () => fireEvent.click(screen.getAllByRole("button").find((b) => b.textContent?.trim() === "Cập nhật")!);

describe("WorkflowBuilder — start_approval action", () => {
  it("shows the saved process, offers only active manual processes of the entity, and re-saves processId", async () => {
    render(<WorkflowBuilder />);
    const box = await screen.findByRole("combobox", { name: "Quy trình duyệt" }, { timeout: 3000 });
    await waitFor(() => expect(box).toHaveTextContent("Duyệt báo giá lớn"));
    fireEvent.click(box);
    expect(await screen.findByRole("option", { name: /Duyệt báo giá lớn/ })).toBeInTheDocument();
    for (const gone of [/Tự duyệt khi tạo/, /Duyệt hợp đồng/, /đang tắt/]) {
      expect(screen.queryByRole("option", { name: gone })).toBeNull();
    }
    fireEvent.keyDown(box, { key: "Escape" });
    await touchRule();
    save();
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    const body = vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] };
    expect(body.actions[0]).toEqual({ type: "start_approval", processId: "p-manual" });
  });

  it("pins WF_START_APPROVAL_INVALID on the action card", async () => {
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "WF_START_APPROVAL_INVALID", "quy trình duyệt \"p-manual\" đang tắt — bật nó hoặc chọn quy trình khác", { field: "/actions/0" }),
    );
    render(<WorkflowBuilder />);
    await screen.findByRole("combobox", { name: "Quy trình duyệt" }, { timeout: 3000 });
    await touchRule();
    save();
    const msgs = await screen.findAllByText(/đang tắt — bật nó hoặc chọn quy trình khác/);
    // once on the card (inline), once in the footer
    expect(msgs.length).toBeGreaterThanOrEqual(2);
  });

  it("is offered in the 'Thêm hành động' menu", async () => {
    render(<WorkflowBuilder />);
    await screen.findByRole("combobox", { name: "Quy trình duyệt" }, { timeout: 3000 });
    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    expect(await screen.findByRole("menuitem", { name: "Khởi động quy trình duyệt" })).toBeInTheDocument();
  });
});
