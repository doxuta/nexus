// specs/048 FE-before-save-hooks-3 (queue #187) — a before_save rule with tự điền + chuẩn hoá opens in the
// editor and SAVES BACK UNTOUCHED. Before this slice the loader turned `ops` into "trim,lower" and the
// default branch of the payload sent only {type}, so open → Cập nhật was a 422 NORMALIZE_OPS_INVALID.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { createWorkflow, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-mutate",
  name: "Làm sạch lead",
  entityName: "crm_leads",
  trigger: "before_save",
  isActive: true,
  condition: { match: "and", rules: [] },
  actions: [
    { type: "field_update", field: "source", value: "Website", valueSource: "constant", onlyIfEmpty: true },
    { type: "field_update", field: "full_name", value: "{{first}} {{last}}", valueSource: "template" },
    { type: "normalize", field: "email", ops: ["trim", "lower", "collapse_spaces"] },
  ],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    createWorkflow: vi.fn(async () => ({ ...RULE, id: "rule-new" })),
    listEntities: vi.fn(async () => [{ name: "crm_leads", label: "Khách tiềm năng" }]),
    listFields: vi.fn(async () => [
      { name: "first", label: "Tên", fieldType: "text" },
      { name: "last", label: "Họ", fieldType: "text" },
      { name: "full_name", label: "Họ tên", fieldType: "text" },
      { name: "source", label: "Nguồn", fieldType: "text" },
      { name: "email", label: "Email", fieldType: "email" },
      { name: "owner", label: "Phụ trách", fieldType: "user" },
    ]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
  };
});
vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});

beforeEach(() => {
  vi.mocked(updateWorkflow).mockClear();
  vi.mocked(createWorkflow).mockClear();
});

const actionCards = () =>
  within(screen.getByRole("heading", { name: "Thì" }).closest("section") as HTMLElement).getAllByRole("article");

describe("WorkflowBuilder — before_save tự điền + chuẩn hoá", () => {
  it("opens a stored rule and saves it back with ops in order, valueSource and onlyIfEmpty intact", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => expect(actionCards()).toHaveLength(3), { timeout: 3000 });
    // The chuẩn hoá card shows the stored operations in their order; the required-field note is up.
    const norm = actionCards()[2];
    await waitFor(() => expect(within(norm).getByRole("combobox", { name: "Trường cần chuẩn hoá" })).toHaveValue("email"));
    expect(screen.getByText(/Tự điền không lấp được trường bắt buộc/)).toBeInTheDocument();
    expect(within(actionCards()[0]).getByRole("switch", { name: /Chỉ điền khi trống/ })).toBeChecked();

    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalledTimes(1));
    const body = vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: unknown[] };
    expect(body.actions).toEqual(RULE.actions);
  });

  it("a veto cannot join a rule that already fills or normalizes (server: WORKFLOW_BEFORE_ACTIONS_MIXED)", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => expect(actionCards()).toHaveLength(3), { timeout: 3000 });
    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    expect((await screen.findAllByRole("menuitem")).map((m) => m.textContent)).toEqual(["Cập nhật trường", "Chuẩn hoá chữ"]);
  });
});
