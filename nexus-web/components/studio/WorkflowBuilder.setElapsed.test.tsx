// DAILY-QUEUE #41: a rule carrying the new `set_elapsed` action (built via the API until the SLDS
// editor lands — FE-stop-clock-action) must survive being opened and re-saved in this builder.
// toActionPayload's default arm returned a bare `{type}`, so renaming or toggling such a rule sent
// the action without its `field` and the server answered 422 — the rule became uneditable here.
//
// FE-stop-clock-action (specs/046 tasks.md): the editor row — "Thêm hành động" offers it, the
// target dropdown lists `number` fields only (the one type the action may write), a stored target
// that no longer fits stays visible instead of blanking, and SET_ELAPSED_FIELD_TYPE_INVALID pins
// on the row.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, listFields, listWorkflows, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const FIELDS = [
  { name: "subject", label: "Tiêu đề", fieldType: "text" },
  { name: "status", label: "Trạng thái", fieldType: "picklist" },
  { name: "resolution_minutes", label: "Số phút xử lý", fieldType: "number" },
  { name: "reopen_count", label: "Số lần mở lại", fieldType: "number" },
  { name: "budget", label: "Ngân sách", fieldType: "currency" },
];

const RULE = {
  id: "rule-elapsed",
  name: "Ghi số phút xử lý",
  entityName: "tk_tickets",
  trigger: "after_update",
  isActive: true,
  condition: { match: "and", rules: [{ field: "status", op: "changedTo", value: "Resolved" }] },
  actions: [{ type: "set_elapsed", field: "resolution_minutes" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    listFields: vi.fn(async () => FIELDS),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return { ...actual, listBusinessCalendars: vi.fn(async () => []), listTeams: vi.fn(async () => []) };
});


describe("WorkflowBuilder — a set_elapsed rule re-saved from the old builder", () => {
  it("keeps the action's target field on save", async () => {
    render(<WorkflowBuilder />);
    await touchRule();
    const save = await screen.findByRole("button", { name: "Cập nhật" }, { timeout: 3000 });
    fireEvent.click(save);
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled(), { timeout: 3000 });
    const payload = vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: unknown[] };
    expect(payload.actions).toEqual([{ type: "set_elapsed", field: "resolution_minutes" }]);
  });
});

const TARGET = "Ghi vào trường (kiểu số)";
const LABEL = "Ghi thời gian xử lý (phút giờ làm việc)";
const thi = () => screen.getByRole("heading", { name: "Thì" }).closest("section") as HTMLElement;
const actionCards = () => within(thi()).getAllByRole("article");
const payloadActions = () => (vi.mocked(updateWorkflow).mock.calls.at(-1)![1] as { actions: unknown[] }).actions;
async function opened() {
  render(<WorkflowBuilder />);
  // The target list fills once the entity's fields are in.
  await waitFor(() => expect(screen.getByRole("option", { name: "Số phút xử lý" })).toBeInTheDocument(), { timeout: 3000 });
}

describe("WorkflowBuilder — the set_elapsed editor row", () => {
  beforeEach(() => vi.mocked(updateWorkflow).mockClear());

  it("opens a stored rule as a boundary card whose target lists number fields only", async () => {
    await opened();
    const [row] = actionCards();
    expect(row).toHaveClass("slds-box");
    expect(within(row).getByTitle(LABEL)).toBeInTheDocument();
    const target = within(row).getByRole("combobox", { name: TARGET });
    expect(target).toHaveValue("resolution_minutes");
    const options = within(target).getAllByRole("option").map((o) => o.textContent);
    expect(options).toEqual(["— chọn trường —", "Số phút xử lý", "Số lần mở lại"]);
    // Which calendar counts, what happens without one, and why a closing rule wants changedTo.
    const note = within(row).getByText(/lịch làm việc mặc định/);
    expect(note).toHaveTextContent("giờ thường");
    expect(note).toHaveTextContent("vừa đổi thành");
  });

  it("'Thêm hành động' adds one; the picked number field is what gets saved", async () => {
    await opened();
    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: LABEL }));
    expect(actionCards()).toHaveLength(2);
    const added = within(actionCards()[1]).getByRole("combobox", { name: TARGET });
    expect(added).toHaveValue("");
    await userEvent.selectOptions(added, "reopen_count");
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(payloadActions()).toEqual([
      { type: "set_elapsed", field: "resolution_minutes" },
      { type: "set_elapsed", field: "reopen_count" },
    ]);
  });

  it("a stored target that is no longer a number stays selected instead of blanking", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([{ ...RULE, actions: [{ type: "set_elapsed", field: "subject" }] }]);
    await opened();
    const target = within(actionCards()[0]).getByRole("combobox", { name: TARGET });
    expect(target).toHaveValue("subject");
    expect(within(target).getByRole("option", { name: "subject · không còn hợp kiểu" })).toBeInTheDocument();
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(payloadActions()).toEqual([{ type: "set_elapsed", field: "subject" }]);
  });

  it("SET_ELAPSED_FIELD_TYPE_INVALID is pinned on the set_elapsed row", async () => {
    const msg = 'field "resolution_minutes" phải là kiểu number';
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "SET_ELAPSED_FIELD_TYPE_INVALID", msg, { field: "resolution_minutes" }),
    );
    await opened();
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    const row = actionCards()[0];
    expect(await within(row).findByText(msg)).toBeInTheDocument();
    expect(within(row).getByRole("combobox", { name: TARGET })).toHaveAttribute("aria-invalid", "true");
  });

  it("says so when the entity has no number field yet", async () => {
    vi.mocked(listFields).mockResolvedValue(FIELDS.filter((f) => f.fieldType !== "number"));
    try {
      render(<WorkflowBuilder />);
      await waitFor(() => expect(listFields).toHaveBeenCalled(), { timeout: 3000 });
      const row = await waitFor(() => actionCards()[0], { timeout: 3000 });
      expect(await within(row).findByText(/chưa có trường kiểu số/)).toBeInTheDocument();
      expect(within(row).getByRole("option", { name: "resolution_minutes · không còn hợp kiểu" })).toBeInTheDocument();
    } finally {
      vi.mocked(listFields).mockResolvedValue(FIELDS);
    }
  });
});
