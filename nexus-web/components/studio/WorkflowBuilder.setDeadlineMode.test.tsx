// Queue #81 / FE-BPM-QUEUE-WF T281: the set_deadline card edits its re-run policy — "Khi chạy lại"
// (mode: overwrite | tighten_only), "Trường dấu máy" (stampField, datetime only, never the target) and
// "Mốc tính" (anchor: now | created_at) — and pins SET_DEADLINE_STAMP_FIELD_INVALID on the stamp box.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-mode", name: "Tính lại hạn SLA", entityName: "tk_tickets", trigger: "after_update", isActive: true,
  condition: { match: "and", rules: [{ field: "priority", op: "changedTo", value: "Urgent" }] },
  actions: [{
    type: "set_deadline", field: "due_date", offset: { amount: 4, unit: "hours" },
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
      { name: "note", label: "Ghi chú", fieldType: "text" },
    ]),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});

beforeEach(() => vi.mocked(updateWorkflow).mockClear());

const save = () => fireEvent.click(screen.getAllByRole("button").find((b) => b.textContent?.trim() === "Cập nhật")!);
const sent = () => (vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] }).actions[0];

describe("WorkflowBuilder — set_deadline re-run policy controls", () => {
  it("reads the stored policy into the three boxes; the stamp list is datetime fields except the target", async () => {
    render(<WorkflowBuilder />);
    const mode = (await screen.findByLabelText("Khi chạy lại", {}, { timeout: 3000 })) as HTMLSelectElement;
    expect(mode.value).toBe("tighten_only");
    expect((screen.getByLabelText("Mốc tính") as HTMLSelectElement).value).toBe("created_at");
    const stamp = screen.getByLabelText("Trường dấu máy") as HTMLSelectElement;
    expect(stamp.value).toBe("due_date_auto");
    const labels = Array.from(stamp.options).map((o) => o.textContent);
    expect(labels).toContain("Hạn SLA do máy đặt");
    expect(labels).not.toContain("Ghi chú");
    expect(labels).not.toContain("Hạn xử lý");
  });

  it("choosing 'Ghi đè' / 'Lúc luật chạy' sends no mode / anchor key; stampField stays", async () => {
    render(<WorkflowBuilder />);
    fireEvent.change(await screen.findByLabelText("Khi chạy lại", {}, { timeout: 3000 }), { target: { value: "overwrite" } });
    fireEvent.change(screen.getByLabelText("Mốc tính"), { target: { value: "now" } });
    await touchRule();
    save();
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    const a = sent();
    expect(a).not.toHaveProperty("mode");
    expect(a).not.toHaveProperty("anchor");
    expect(a).toMatchObject({ stampField: "due_date_auto" });
  });

  it("changing the stamp field is sent", async () => {
    render(<WorkflowBuilder />);
    fireEvent.change(await screen.findByLabelText("Trường dấu máy", {}, { timeout: 3000 }), { target: { value: "" } });
    await touchRule();
    save();
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(sent()).not.toHaveProperty("stampField");
  });

  it("pins SET_DEADLINE_STAMP_FIELD_INVALID on the stamp box", async () => {
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "SET_DEADLINE_STAMP_FIELD_INVALID", "mode tighten_only cần 'stampField' — một trường datetime giữ dấu hạn máy"),
    );
    render(<WorkflowBuilder />);
    await screen.findByLabelText("Trường dấu máy", {}, { timeout: 3000 });
    await touchRule();
    save();
    const stamp = screen.getByLabelText("Trường dấu máy");
    await waitFor(() => expect(stamp).toHaveAttribute("aria-invalid", "true"));
    expect(screen.getAllByText(/mode tighten_only cần 'stampField'/).length).toBeGreaterThanOrEqual(2);
  });
});
