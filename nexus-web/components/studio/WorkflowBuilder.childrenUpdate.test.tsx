// specs/084 — the "Cập nhật trường" card can write EVERY child record ("lớp Đã đóng ⇒ mọi ghi danh
// đổi trạng thái"): the record picker lists the other entities' relations that point back at the
// rule's entity, the field list is the CHILD entity's, and the payload carries childEntity+childVia.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-close",
  name: "Lớp đóng thì đóng ghi danh",
  entityName: "nk_classes",
  trigger: "after_update",
  isActive: true,
  condition: { match: "and", rules: [{ field: "status", op: "changedTo", value: "closed" }] },
  actions: [{ type: "field_update", childEntity: "nk_enrollments", childVia: "class", field: "status", value: "closed" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    createWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [
      { name: "nk_classes", label: "Lớp học" },
      { name: "nk_enrollments", label: "Ghi danh" },
    ]),
    listFields: vi.fn(async (entity: string) => entity === "nk_enrollments"
      ? [
          { name: "status", label: "Tình trạng ghi danh", fieldType: "picklist", options: { values: ["active", "closed"] } },
          { name: "class", label: "Lớp", fieldType: "relation", options: { relatedEntity: "nk_classes" } },
          { name: "note", label: "Ghi chú ghi danh", fieldType: "text" },
        ]
      : [
          { name: "status", label: "Trạng thái lớp", fieldType: "picklist", options: { values: ["open", "closed"] } },
        ]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
    listRecords: vi.fn(async () => []),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


beforeEach(() => {
  vi.mocked(updateWorkflow).mockClear();
});

const CHILD_LABEL = "Mọi Ghi danh có “Lớp” là bản ghi này";
const actionCard = () => {
  const then = screen.getByRole("heading", { name: "Thì" }).closest("section") as HTMLElement;
  return within(then).getAllByRole("article")[0];
};
async function saved() {
  await touchRule();
  await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
  await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
  return (vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] }).actions;
}

describe("WorkflowBuilder — field_update on every child record (specs/084)", () => {
  it("opens a saved children rule on the child's fields and re-saves it untouched", async () => {
    render(<WorkflowBuilder />);
    await waitFor(() => {
      const picker = within(actionCard()).getByRole("combobox", { name: "Sửa bản ghi" });
      expect(picker).toHaveValue("child:nk_enrollments:class");
      expect(within(picker).getByRole("option", { name: CHILD_LABEL })).toBeInTheDocument();
      expect(within(within(actionCard()).getByRole("combobox", { name: "Trường" })).getByRole("option", { name: "Ghi chú ghi danh" })).toBeInTheDocument();
    }, { timeout: 3000 });
    expect(within(actionCard()).getByText(/tối đa 200 bản ghi/)).toBeInTheDocument();
    expect(await saved()).toEqual([
      { type: "field_update", childEntity: "nk_enrollments", childVia: "class", field: "status", value: "closed" },
    ]);
  });

  it("picking a child relation then a child field writes a typed value with the children pair", async () => {
    render(<WorkflowBuilder />);
    const card = () => within(actionCard());
    await waitFor(() => expect(card().getByRole("combobox", { name: "Sửa bản ghi" })).toHaveValue("child:nk_enrollments:class"), { timeout: 3000 });
    // Back to the triggering record, then forward to the children again: the old field and value are dropped each time.
    await userEvent.selectOptions(card().getByRole("combobox", { name: "Sửa bản ghi" }), "");
    expect(card().getByRole("combobox", { name: "Trường" })).toHaveValue("");
    await userEvent.selectOptions(card().getByRole("combobox", { name: "Sửa bản ghi" }), "child:nk_enrollments:class");
    await userEvent.selectOptions(card().getByRole("combobox", { name: "Trường" }), "note");
    await userEvent.type(card().getByRole("textbox", { name: "Ghi chú ghi danh" }), "Lớp đã đóng");
    expect(await saved()).toEqual([
      { type: "field_update", childEntity: "nk_enrollments", childVia: "class", field: "note", value: "Lớp đã đóng" },
    ]);
  });
});
