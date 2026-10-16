// specs/048 FE-before-save-hooks-2 — the Tự động panel builds "before" rules itself. Until this
// slice a before rule opened read-only (S1a guard, queue #216): the editor knew neither the triggers
// nor the veto action, and re-saving would have stripped the refusal. Now:
//   • a stored before_save rule opens in the editor and a save round-trips trigger, saveOn, the
//     condition and the veto {message, field} untouched;
//   • picking "Trước khi LƯU" offers only the actions the server accepts there, flags any other,
//     and a veto is built with its message + optional field;
//   • "vừa đổi"/"vừa đổi thành" are not offered on a create-only rule, and a row still holding one
//     is named;
//   • a refusal of the message lands on the message box; the rule list badges before rules;
//   • a before_delete refusal has no "Gắn lỗi vào trường" (a delete has no form — queue #323).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, createWorkflow, updateWorkflow, listWorkflowRuns } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const BEFORE_RULE = {
  id: "rule-before",
  name: "Chốt Thắng cần Ngày chốt",
  entityName: "crm_opportunities",
  trigger: "before_save",
  isActive: true,
  condition: {
    match: "and",
    saveOn: "update",
    rules: [
      { field: "stage", op: "changedTo", value: "Closed Won" },
      { field: "close_date", op: "isnull" },
    ],
  },
  actions: [{ type: "veto", field: "close_date", message: "Chốt Thắng phải có Ngày chốt" }],
};
// The runs this very rule left (specs/048 S3): one refused save, one exempt system write let through.
const BEFORE_RUNS = [
  {
    id: "run-blocked", workflowId: "rule-before", recordId: "rec-opp-0000-0000", triggerType: "before_save",
    status: "blocked", actionsResult: [{ type: "veto", ok: true, field: "close_date" }],
    startedAt: "2026-09-29T01:00:00Z", finishedAt: "2026-09-29T01:00:00Z",
  },
  {
    id: "run-skipped", workflowId: "rule-before", recordId: "rec-opp-1111-1111", triggerType: "before_save",
    status: "skipped", actionsResult: [{ type: "veto", ok: true, skipped: "approval_outcome" }],
    startedAt: "2026-09-29T00:00:00Z", finishedAt: "2026-09-29T00:00:00Z",
  },
];
const AFTER_RULE = {
  id: "rule-after",
  name: "Báo khi thắng",
  entityName: "crm_opportunities",
  trigger: "after_update",
  isActive: true,
  condition: { match: "and", rules: [{ field: "stage", op: "changedTo", value: "Closed Won" }] },
  actions: [{ type: "in_app_notification", userId: "$owner", title: "Thắng", message: "Có cơ hội thắng" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [BEFORE_RULE, AFTER_RULE]),
    updateWorkflow: vi.fn(async () => BEFORE_RULE),
    createWorkflow: vi.fn(async () => ({ ...BEFORE_RULE, id: "rule-new" })),
    listEntities: vi.fn(async () => [{ name: "crm_opportunities", label: "Cơ hội" }]),
    listFields: vi.fn(async () => [
      { name: "name", label: "Tên cơ hội", fieldType: "text" },
      { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["Prospecting", "Closed Won"] } },
      { name: "close_date", label: "Ngày chốt", fieldType: "date" },
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

const card = (title: string) => screen.getByRole("heading", { name: title }).closest("section") as HTMLElement;
const actionCards = () => within(card("Thì")).getAllByRole("article");
const messageBox = () => screen.getByRole("textbox", { name: /Thông điệp cho người dùng/ });

async function openedOnBeforeRule() {
  render(<WorkflowBuilder />);
  // The before rule is first in the list, so it is the one opened on load — in the EDITOR now.
  await waitFor(() => expect(messageBox()).toHaveValue("Chốt Thắng phải có Ngày chốt"), { timeout: 3000 });
  await waitFor(() => expect(within(card("Nếu")).getAllByRole("combobox", { name: "Trường" })[0]).toHaveTextContent("Giai đoạn"));
}

describe("WorkflowBuilder — before rules", () => {
  it("a stored before_save rule opens in the editor and saving round-trips trigger, saveOn, conditions and the veto", async () => {
    await openedOnBeforeRule();
    expect(screen.queryByTestId("wf-before-readonly")).toBeNull();
    expect(screen.getByRole("radio", { name: "Trước khi LƯU (chặn được lần lưu)" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "Chỉ khi cập nhật" })).toBeChecked();
    expect(screen.getByText(/MỌI lần lưu/)).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Gắn lỗi vào trường" })).toHaveTextContent("Ngày chốt");

    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalledTimes(1));
    const body = vi.mocked(updateWorkflow).mock.calls[0][1] as Record<string, unknown>;
    expect(body.trigger).toBe("before_save");
    expect(body.isActive).toBe(true);
    expect(body.condition).toEqual({
      match: "and",
      saveOn: "update",
      rules: [
        { field: "stage", op: "changedTo", value: "Closed Won" },
        { field: "close_date", op: "isnull" },
      ],
    });
    expect(body.actions).toEqual([{ type: "veto", field: "close_date", message: "Chốt Thắng phải có Ngày chốt" }]);
  });

  it("the rule list badges before rules and keeps after rules as text", async () => {
    await openedOnBeforeRule();
    const list = screen.getByRole("list", { name: "Luật tự động" });
    const beforeRow = within(list).getByText("Chốt Thắng cần Ngày chốt").closest("li")!;
    expect(within(beforeRow).getByText("Trước khi lưu")).toHaveClass("slds-badge");
    const afterRow = within(list).getByText("Báo khi thắng").closest("li")!;
    expect(afterRow.querySelector(".slds-badge")).toBeNull();
  });

  it("a new before_save rule: only the refusal is offered, the default action is flagged, the veto is built and created", async () => {
    await openedOnBeforeRule();
    await userEvent.click(screen.getByRole("button", { name: "Tạo luật mới" }));
    await userEvent.click(screen.getByRole("radio", { name: "Trước khi LƯU (chặn được lần lưu)" }));

    // The new rule's default notification is not allowed inside a save: flagged, not dropped.
    expect(within(actionCards()[0]).getByText(/không dùng được với quy tắc/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    const offered = (await screen.findAllByRole("menuitem")).map((m) => m.textContent);
    expect(offered).toEqual(["Cập nhật trường", "Chuẩn hoá chữ", "Chặn và báo lỗi"]);
    await userEvent.click(screen.getByRole("menuitem", { name: "Chặn và báo lỗi" }));
    await userEvent.click(screen.getByRole("button", { name: "Xoá hành động 1" }));
    expect(actionCards()).toHaveLength(1);

    fireEvent.change(messageBox(), { target: { value: "Chốt Thắng phải có Ngày chốt" } });
    await userEvent.click(screen.getByRole("combobox", { name: "Gắn lỗi vào trường" }));
    await userEvent.click(screen.getByRole("option", { name: "Ngày chốt" }));
    await userEvent.click(screen.getByRole("radio", { name: "Chỉ khi tạo mới" }));

    await userEvent.click(screen.getByRole("button", { name: "Tạo luật" }));
    await waitFor(() => expect(createWorkflow).toHaveBeenCalledTimes(1));
    const body = vi.mocked(createWorkflow).mock.calls[0][0] as Record<string, unknown>;
    expect(body.trigger).toBe("before_save");
    expect(body.condition).toEqual({ match: "and", rules: [], saveOn: "create" });
    expect(body.actions).toEqual([{ type: "veto", field: "close_date", message: "Chốt Thắng phải có Ngày chốt" }]);
  });

  // Queue #323: the delete rule of US4, built the way admin@example.com built it in the click-through —
  // "Trước khi XOÁ", Giai đoạn bằng Thắng, the refusal alone. A delete has no form, so the refusal
  // carries no anchor picker (nor its "vừa đổi thành" advice — that operator is not offered here).
  it("a new before_delete rule: no 'Áp dụng khi', no anchor picker, the veto is created without a field", async () => {
    await openedOnBeforeRule();
    await userEvent.click(screen.getByRole("button", { name: "Tạo luật mới" }));
    fireEvent.change(screen.getByRole("textbox", { name: /Tên luật/ }), { target: { value: "Không xoá cơ hội đã Thắng" } });
    await userEvent.click(screen.getByRole("radio", { name: "Trước khi XOÁ (chặn được lần xoá)" }));
    expect(screen.queryByRole("radio", { name: "Chỉ khi cập nhật" })).toBeNull();
    expect(screen.getByText(/MỌI lần xoá/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    expect((await screen.findAllByRole("menuitem")).map((m) => m.textContent)).toEqual(["Chặn và báo lỗi"]);
    await userEvent.click(screen.getByRole("menuitem", { name: "Chặn và báo lỗi" }));
    await userEvent.click(screen.getByRole("button", { name: "Xoá hành động 1" }));
    const veto = actionCards()[0];
    expect(within(veto).queryByRole("combobox", { name: "Gắn lỗi vào trường" })).toBeNull();
    expect(veto.textContent).not.toMatch(/vừa đổi thành/);
    fireEvent.change(messageBox(), { target: { value: "Cơ hội đã chốt Thắng — không được xoá." } });

    await userEvent.click(screen.getByRole("button", { name: "Tạo luật" }));
    await waitFor(() => expect(createWorkflow).toHaveBeenCalledTimes(1));
    const body = vi.mocked(createWorkflow).mock.calls[0][0] as Record<string, unknown>;
    expect(body.trigger).toBe("before_delete");
    expect(body.condition).toBeUndefined(); // no rows ⇒ no condition — and never a saveOn on a delete
    expect(body.actions).toEqual([{ type: "veto", message: "Cơ hội đã chốt Thắng — không được xoá." }]);
  });

  it("switching an after rule to before_save does not offer after actions any more", async () => {
    await openedOnBeforeRule();
    fireEvent.click(screen.getByText("Báo khi thắng"));
    await waitFor(() => expect(screen.getByRole("radio", { name: "Khi CẬP NHẬT bản ghi" })).toBeChecked());
    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    expect((await screen.findAllByRole("menuitem")).map((m) => m.textContent)).not.toContain("Chặn và báo lỗi");
    await userEvent.keyboard("{Escape}");
    await userEvent.click(screen.getByRole("radio", { name: "Trước khi LƯU (chặn được lần lưu)" }));
    await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
    expect((await screen.findAllByRole("menuitem")).map((m) => m.textContent)).toEqual(["Cập nhật trường", "Chuẩn hoá chữ", "Chặn và báo lỗi"]);
  });

  it("create-only: the edge operators are not offered, and a row still holding one is named", async () => {
    await openedOnBeforeRule();
    expect(screen.queryByText(/không bao giờ đúng khi tạo mới/)).toBeNull();
    await userEvent.click(screen.getByRole("radio", { name: "Chỉ khi tạo mới" }));

    const warn = screen.getByText(/không bao giờ đúng khi tạo mới/);
    expect(warn).toHaveTextContent("Giai đoạn");
    expect(warn.textContent).not.toContain("Ngày chốt");
    // The second row's operator list has no "vừa đổi" options any more.
    const rows = Array.from(document.querySelectorAll<HTMLElement>("li.slds-expression__row"));
    await userEvent.click(within(rows[1]).getByRole("combobox", { name: "Toán tử" }));
    const ops = screen.getAllByRole("option").map((o) => o.textContent ?? "");
    expect(ops.some((o) => /vừa đổi/i.test(o))).toBe(false);
  });

  // FE-before-save-hooks-4: the server rehearses a before rule (veto / fill / normalize previews), so
  // "Chạy thử" is on for a saved before rule; its runs ("Đã chặn" / "Bỏ qua") read in the runs tab.
  it("a saved before rule: 'Chạy thử' is on and the runs tab shows its blocked / skipped runs", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValueOnce({ runs: BEFORE_RUNS, total: 2, limit: 20, offset: 0 } as never);
    await openedOnBeforeRule();
    const test = screen.getByRole("button", { name: "Chạy thử" });
    expect(test).toBeEnabled();

    // specs/074 lát 11: "Lần chạy gần đây" is a section of the page (no tab), read when the saved rule opens.
    const grid = await screen.findByRole("grid", { name: "Lịch sử chạy" });
    expect(within(grid).getByText("Đã chặn")).toBeInTheDocument();
    expect(within(grid).getByText("Bỏ qua (ghi kết quả duyệt)")).toBeInTheDocument();
    expect(listWorkflowRuns).toHaveBeenCalledWith("rule-before", 20, 0);

    // An after rule keeps "Chạy thử".
    fireEvent.click(screen.getByText("Báo khi thắng"));
    await waitFor(() => expect(screen.getByRole("button", { name: "Chạy thử" })).toBeEnabled());
  });

  it("the list row's History button opens a before rule straight into its runs", async () => {
    await openedOnBeforeRule();
    fireEvent.click(screen.getByText("Báo khi thắng"));
    await waitFor(() => expect(screen.getByRole("button", { name: "Chạy thử" })).toBeEnabled());
    vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: BEFORE_RUNS, total: 2, limit: 20, offset: 0 } as never);
    fireEvent.click(screen.getByRole("button", { name: "Lịch sử chạy của Chốt Thắng cần Ngày chốt" }));
    const grid = await screen.findByRole("grid", { name: "Lịch sử chạy" });
    expect(within(grid).getByText("Đã chặn")).toBeInTheDocument();
  });

  it("the server's refusal of the message lands on the message box", async () => {
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "WORKFLOW_VETO_MESSAGE_INVALID", "thông điệp chặn phải có 1–500 ký tự (đang có 0)", { max: 500 }),
    );
    await openedOnBeforeRule();
    fireEvent.change(messageBox(), { target: { value: "   " } });
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(messageBox().closest(".slds-form-element")).toHaveClass("slds-has-error"));
    expect(messageBox().closest(".slds-form-element")).toHaveTextContent("thông điệp chặn phải có 1–500 ký tự");
    // Editing the message clears its error.
    fireEvent.change(messageBox(), { target: { value: "Cần Ngày chốt" } });
    expect(messageBox().closest(".slds-form-element")).not.toHaveClass("slds-has-error");
  });
});
