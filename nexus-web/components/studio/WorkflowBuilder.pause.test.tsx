// Doctor C10 tells the admin to pause a rule that never idles. A PUT of a rule that still holds a
// pre-vault key is a 409, so the list carries its own switch (PATCH /admin/workflows/:id/active) —
// and it must not resend the actions.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { setWorkflowActive, updateWorkflow, ApiError } from "@/lib/api";

const RULE = {
  id: "rule-keyed",
  name: "Báo đơn cho đối tác",
  entityName: "crm_contacts",
  trigger: "after_create",
  isActive: true,
  condition: null,
  actions: [{ type: "outbound_webhook", url: "https://erp.example.vn/hook" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    setWorkflowActive: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "crm_contacts", label: "Liên hệ" }]),
    listFields: vi.fn(async () => [{ name: "first_name", label: "Tên", fieldType: "text" }]),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


describe("WorkflowBuilder — pause from the list", () => {
  beforeEach(() => {
    vi.mocked(setWorkflowActive).mockClear();
    vi.mocked(updateWorkflow).mockClear();
  });

  it("switches the rule off through its own call, never a PUT", async () => {
    render(<WorkflowBuilder />);
    fireEvent.click(await screen.findByRole("button", { name: "Tạm tắt Báo đơn cho đối tác" }, { timeout: 3000 }));
    await waitFor(() => expect(setWorkflowActive).toHaveBeenCalledWith("rule-keyed", false));
    expect(updateWorkflow).not.toHaveBeenCalled();
    expect(await screen.findByRole("button", { name: "Bật lại Báo đơn cho đối tác" })).toBeTruthy();
  });

  it("shows the refusal and keeps the rule as it was", async () => {
    vi.mocked(setWorkflowActive).mockRejectedValueOnce(new ApiError(422, "WORKFLOW_BEFORE_RULE_LIMIT", "đã đủ quy tắc đang bật"));
    render(<WorkflowBuilder />);
    fireEvent.click(await screen.findByRole("button", { name: "Tạm tắt Báo đơn cho đối tác" }, { timeout: 3000 }));
    expect(await screen.findByText("đã đủ quy tắc đang bật")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Tạm tắt Báo đơn cho đối tác" })).toBeTruthy();
  });
});
