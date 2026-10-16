// specs/059 (DAILY-QUEUE #271), journey 09-30: the Nha khoa builder typed "{{email_bn}}" into a free
// text box because nothing listed the patient's email. The email action now PICKS the recipient —
// the linked patient's email — and the saved rule carries the engine's {{relation.field}} token.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-1",
  name: "Nhắc hẹn",
  entityName: "lich_hen",
  trigger: "after_create",
  isActive: true,
  condition: null,
  actions: [{ type: "email_alert", to: "", subject: "Nhắc hẹn", body: "Chào" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "lich_hen", label: "Lịch hẹn" }, { name: "benh_nhan", label: "Bệnh nhân" }]),
    listFields: vi.fn(async (entity: string) =>
      entity === "benh_nhan"
        ? [
            { name: "ho_ten", label: "Họ tên", fieldType: "text" },
            { name: "email", label: "Email", fieldType: "email" },
          ]
        : [
            { name: "tieu_de", label: "Tiêu đề", fieldType: "text" },
            { name: "benh_nhan", label: "Bệnh nhân", fieldType: "relation", options: { relatedEntity: "benh_nhan" } },
          ],
    ),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


describe("WorkflowBuilder — email to the linked record", () => {
  it("picks the patient's email and a patient field, and saves the engine's tokens", async () => {
    render(<WorkflowBuilder />);
    const pick = await screen.findByRole("button", { name: "Chọn người nhận" }, { timeout: 3000 });
    await userEvent.click(pick);
    await userEvent.click(await screen.findByRole("menuitem", { name: "Bệnh nhân › Email" }));
    expect(screen.getByRole("textbox", { name: "Gửi tới" })).toHaveValue("{{benh_nhan.email}}");

    await userEvent.click(screen.getByRole("button", { name: "Chèn trường vào tiêu đề" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Bệnh nhân › Họ tên" }));
    expect(screen.getByRole("textbox", { name: "Tiêu đề email" })).toHaveValue("Nhắc hẹn {{benh_nhan.ho_ten}}");

    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    const body = vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] };
    expect(body.actions[0]).toMatchObject({ type: "email_alert", to: "{{benh_nhan.email}}", subject: "Nhắc hẹn {{benh_nhan.ho_ten}}" });
  });
});
