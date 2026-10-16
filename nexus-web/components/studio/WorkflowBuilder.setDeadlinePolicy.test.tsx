// Queue #81 (specs/043 S14a): Ticketing 1.8.0 ships set_deadline rules with mode/stampField/anchor.
// This builder has no controls for them yet (FE-BPM-QUEUE-WF #137), and its per-action mapper used
// to rebuild set_deadline from {field, offset} alone — so opening "Tính lại hạn SLA — đổi sang Khẩn"
// and pressing "Cập nhật" silently turned a shorten-only rule into an OVERWRITE that replaces
// hand-typed deadlines on every priority change. Re-saving must carry the three params back.
import { describe, it, expect, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-81",
  name: "Tính lại hạn SLA — đổi sang Khẩn",
  entityName: "tk_tickets",
  trigger: "after_update",
  isActive: true,
  condition: { match: "and", rules: [{ field: "priority", op: "changedTo", value: "Urgent" }] },
  actions: [{
    type: "set_deadline", field: "due_date", offset: { amount: 4, unit: "business_hours" },
    mode: "tighten_only", stampField: "due_date_auto", anchor: "created_at",
  }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    listFields: vi.fn(async () => [
      { name: "priority", label: "Ưu tiên", fieldType: "picklist" },
      { name: "due_date", label: "Hạn xử lý", fieldType: "datetime" },
      { name: "due_date_auto", label: "Hạn SLA do máy đặt", fieldType: "datetime" },
    ]),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


describe("WorkflowBuilder — re-saving a tighten_only set_deadline", () => {
  it("keeps mode, stampField and anchor", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => expect(screen.getByRole("option", { name: "Hạn xử lý" })).toBeInTheDocument(), { timeout: 3000 });
    await touchRule();
    const save = screen.getAllByRole("button").find((b) => b.textContent?.trim() === "Cập nhật");
    expect(save).toBeDefined();
    fireEvent.click(save!);
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    const body = vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] };
    expect(body.actions[0]).toMatchObject({
      type: "set_deadline", field: "due_date", offset: { amount: 4, unit: "business_hours" },
      mode: "tighten_only", stampField: "due_date_auto", anchor: "created_at",
    });
  });
});
