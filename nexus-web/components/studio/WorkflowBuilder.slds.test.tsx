// WorkflowBuilder on SLDS (specs/038 T043): split-view rule list, page header with the on/off toggle
// and "Chạy thử", tabs Cấu hình / Lịch sử chạy, cards KHI → NẾU → THÌ with the expression builder
// for conditions and one boundary card per action. Behaviour pinned here is the old builder's:
// the condition payload round-trips (OR, null operators without a value), the toggle drives
// isActive, the WF-01 422 lands on the offending action, delete asks first, the runs tab and the
// dry-run dialog open for a saved rule. The flows the migration rewrote are pinned one by one: the
// trigger radios (schedule interval, date offset timer), the AND/OR select, the "Thêm hành động"
// menu, removing an action, creating a rule (and the empty list), and dates without relative tokens.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, createWorkflow, deleteWorkflow, listWorkflowRuns, listWorkflows, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-1",
  name: "Đặt hạn SLA — phiếu thường",
  entityName: "tk_tickets",
  // after_update: the `changed` row below only makes sense on an update (WORKFLOW_CONDITION_NEVER_TRUE).
  trigger: "after_update",
  isActive: true,
  condition: {
    match: "or",
    rules: [
      { field: "due_date", op: "isnull" },
      { field: "priority", op: "neq", value: "Urgent" },
      { field: "status", op: "changed" },
    ],
  },
  actions: [
    { type: "set_deadline", field: "due_date", offset: { amount: 40, unit: "business_hours" } },
    { type: "in_app_notification", userId: "$owner", title: "Có phiếu mới", message: "Xem ngay" },
  ],
};
const TIMER_RULE = {
  id: "rule-2",
  name: "Nhắc hạn SLA",
  entityName: "tk_tickets",
  trigger: "date_offset",
  isActive: false,
  condition: { match: "and", rules: [], timer: { dateField: "due_date", offset: { amount: 2, unit: "hours" }, direction: "before" } },
  actions: [{ type: "in_app_notification", userId: "$owner", title: "Sắp hạn", message: "Còn 2 giờ" }],
};

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listWorkflows: vi.fn(async () => [RULE, TIMER_RULE]),
    updateWorkflow: vi.fn(async () => RULE),
    createWorkflow: vi.fn(async () => ({ ...RULE, id: "rule-new", name: "Luật mới" })),
    deleteWorkflow: vi.fn(async () => undefined),
    listEntities: vi.fn(async () => [{ name: "tk_tickets", label: "Phiếu hỗ trợ" }]),
    listFields: vi.fn(async () => [
      { name: "subject", label: "Tiêu đề", fieldType: "text" },
      { name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["Open", "Closed"] } },
      { name: "priority", label: "Ưu tiên", fieldType: "picklist", options: { values: ["High", "Urgent"], valueLabels: { High: "Cao", Urgent: "Khẩn" } } },
      { name: "due_date", label: "Hạn xử lý", fieldType: "datetime" },
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
  vi.mocked(createWorkflow).mockClear();
  vi.mocked(deleteWorkflow).mockClear();
  vi.mocked(listWorkflowRuns).mockClear();
});

async function loaded() {
  render(<WorkflowBuilder />);
  // The saved condition rows resolve their field labels once the entity's fields are in.
  await waitFor(() => expect(document.querySelectorAll("li.slds-expression__row")).toHaveLength(3), { timeout: 3000 });
  await waitFor(() => expect(within(conditionRows()[0]).getByRole("combobox", { name: "Trường" })).toHaveTextContent("Hạn xử lý"));
}
const conditionRows = () => Array.from(document.querySelectorAll<HTMLElement>("li.slds-expression__row"));
const card = (title: string) => screen.getByRole("heading", { name: title }).closest("section") as HTMLElement;
const saveBody = () => vi.mocked(updateWorkflow).mock.calls[0][1] as Record<string, unknown>;
const actionCards = () => within(card("Thì")).getAllByRole("article");
const actionTypes = (body: Record<string, unknown>) => (body.actions as { type: string }[]).map((a) => a.type);
async function save() {
  await touchRule();
  await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
  await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
  return saveBody();
}
async function addAction(label: string) {
  await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
  await userEvent.click(await screen.findByRole("menuitem", { name: label }));
}

describe("WorkflowBuilder on SLDS", () => {
  it("frames the screen: split-view rule list, tabs, KHI/NẾU/THÌ cards, trigger radio group, toggle", async () => {
    await loaded();
    const item = screen.getByRole("button", { name: /^Đặt hạn SLA — phiếu thường/ });
    expect(item).toHaveClass("slds-split-view__list-item-action");
    expect(item).toHaveAttribute("aria-current", "page");

    // specs/074 lát 11: flat sections, no tabs; the run history is a section of the same page.
    expect(screen.queryByRole("tab")).toBeNull();
    expect(screen.getByRole("heading", { name: "Lần chạy gần đây" })).toBeInTheDocument();
    for (const title of ["Khi", "Nếu", "Thì"]) {
      expect(card(title)).not.toBeNull();
    }
    expect(within(card("Khi")).getByRole("radio", { name: "Khi CẬP NHẬT bản ghi" })).toBeChecked();
    expect(screen.getByRole("switch", { name: /Bật luật/ })).toBeChecked();

    // One nested boundary card per action.
    const thi = card("Thì");
    const actionCards = within(thi).getAllByRole("article");
    expect(actionCards).toHaveLength(2);
    expect(actionCards[0]).toHaveClass("slds-box");
    expect(within(actionCards[0]).getByTitle("Đặt hạn xử lý")).toBeInTheDocument();
  });

  it("conditions are the expression builder: labels, OR match, null operators need no value — and round-trip on save", async () => {
    await loaded();
    const [dueDate, priority, status] = conditionRows();
    expect(within(dueDate).getByRole("combobox", { name: "Toán tử" })).toHaveTextContent("rỗng");
    expect(within(priority).getByRole("combobox", { name: "Giá trị" })).toHaveTextContent("Khẩn");
    expect(within(status).getByRole("combobox", { name: "Toán tử" })).toHaveTextContent("vừa đổi");
    expect(within(status).getByRole("textbox", { name: "Giá trị" })).toBeDisabled();
    expect(screen.getByRole("combobox", { name: "Áp dụng khi" })).toHaveTextContent("Bất kỳ điều kiện nào");

    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(saveBody().condition).toEqual({
      match: "or",
      rules: [
        { field: "due_date", op: "isnull" },
        { field: "priority", op: "neq", value: "Urgent" },
        { field: "status", op: "changed" },
      ],
    });
    expect(saveBody().isActive).toBe(true);
    expect(saveBody()).not.toHaveProperty("intervalSeconds");
    expect(await screen.findByText("Đã lưu")).toBeInTheDocument();
  });

  it("the toggle turns the rule off", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("switch", { name: /Bật luật/ }));
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
    expect(saveBody().isActive).toBe(false);
  });

  it("a WF-01 422 is pinned on the offending action and repeated in the footer", async () => {
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "SET_DEADLINE_FIELD_TYPE_INVALID", "trường \"due_date\" phải là ngày giờ", { field: "due_date" }),
    );
    await loaded();
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    const actionCard = within(card("Thì")).getAllByRole("article")[0];
    expect(await within(actionCard).findByText("trường \"due_date\" phải là ngày giờ")).toBeInTheDocument();
    expect(within(actionCard).getByRole("combobox", { name: "Ghi vào trường (kiểu ngày giờ)" })).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByTitle(/xem hành động được đánh dấu đỏ/)).toBeInTheDocument();
  });

  it("the date_offset rule shows its timer; the runs tab and the dry-run dialog open for a saved rule", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("button", { name: /^Nhắc hạn SLA/ }));
    await waitFor(() => expect(screen.getByRole("radio", { name: "Theo MỐC THỜI GIAN" })).toBeChecked());
    expect(screen.getByText(/Sẽ chạy:/)).toHaveTextContent("2 giờ trước Hạn xử lý");

    await waitFor(() => expect(listWorkflowRuns).toHaveBeenCalledWith("rule-2", 20, 0));

    await userEvent.click(screen.getByRole("button", { name: "Chạy thử" }));
    expect(await screen.findByRole("group", { name: "Chạy thử — Nhắc hạn SLA" })).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("the history button of a list row opens that rule straight on its runs", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("button", { name: "Lịch sử chạy của Nhắc hạn SLA" }));
    await waitFor(() => expect(listWorkflowRuns).toHaveBeenCalledWith("rule-2", 20, 0));
  });

  it("delete asks in a prompt before calling the API", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("button", { name: "Xoá" }));
    const prompt = await screen.findByRole("group", { name: "Xoá luật?" });
    expect(deleteWorkflow).not.toHaveBeenCalled();
    await userEvent.click(within(prompt).getByRole("button", { name: "Xoá luật" }));
    await waitFor(() => expect(deleteWorkflow).toHaveBeenCalledWith("rule-1"));
  });
  it("the AND/OR select drives condition.match", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("combobox", { name: "Áp dụng khi" }));
    await userEvent.click(screen.getByRole("option", { name: "Tất cả điều kiện đúng" }));
    expect((await save()).condition).toMatchObject({ match: "and" });
  });

  it("date conditions take a plain date: the engine resolves no relative-date token", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([
      { ...RULE, condition: { match: "and", rules: [{ field: "due_date", op: "gt", value: "2026-10-01" }] } },
    ]);
    render(<WorkflowBuilder />);
    await waitFor(() => expect(conditionRows()).toHaveLength(1), { timeout: 3000 });
    const row = conditionRows()[0];
    await waitFor(() => expect(within(row).getByRole("combobox", { name: "Trường" })).toHaveTextContent("Hạn xử lý"));
    expect(within(row).queryByRole("option", { name: "Hôm nay" })).toBeNull();
    expect(within(row).getByLabelText("Giá trị")).toHaveAttribute("type", "date");
    expect((await save()).condition).toEqual({ match: "and", rules: [{ field: "due_date", op: "gt", value: "2026-10-01" }] });
  });

  it("switching KHI to the schedule asks for the interval and sends it", async () => {
    await loaded();
    expect(screen.queryByRole("spinbutton", { name: /Lặp mỗi/ })).toBeNull();
    await userEvent.click(screen.getByRole("radio", { name: "Theo LỊCH (định kỳ)" }));
    fireEvent.change(await screen.findByRole("spinbutton", { name: /Lặp mỗi/ }), { target: { value: "7200" } });
    expect(screen.getByText(/≈ 2\.0 giờ/)).toBeInTheDocument();
    const body = await save();
    expect(body.trigger).toBe("scheduled");
    expect(body.intervalSeconds).toBe(7200);
  });

  it("switching KHI to a date offset shows the timer and saves it inside the condition", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("radio", { name: "Theo MỐC THỜI GIAN" }));
    await userEvent.selectOptions(await screen.findByRole("combobox", { name: /Trường ngày làm mốc/ }), "due_date");
    expect(screen.getByText(/Sẽ chạy:/)).toHaveTextContent("3 ngày trước Hạn xử lý");
    const body = await save();
    expect(body.trigger).toBe("date_offset");
    expect(body.condition).toMatchObject({
      match: "or",
      timer: { dateField: "due_date", offset: { amount: 3, unit: "days" }, direction: "before" },
    });
  });

  it("'Thêm hành động' adds one card of the picked type", async () => {
    await loaded();
    await addAction("Cập nhật trường");
    expect(actionCards()).toHaveLength(3);
    expect(within(actionCards()[2]).getByTitle("Cập nhật trường")).toBeInTheDocument();
    expect(actionTypes(await save())).toEqual(["set_deadline", "in_app_notification", "field_update"]);
  });

  it("every action is a flat titled box — no coloured icon tile", async () => {
    await loaded();
    for (const label of ["Cập nhật trường", "Thông báo trong app", "Gửi email", "Gọi webhook", "Chia việc", "Tạo bản ghi mới", "Đặt hạn xử lý", "Ghi thời gian xử lý (phút giờ làm việc)"]) {
      await addAction(label);
    }
    // Flat page (specs/074 lát 11): an action is a titled box, no coloured icon tile per action.
    expect(actionCards()).toHaveLength(10);
    for (const c of actionCards()) expect(c.querySelector("[class*='slds-icon-standard-']")).toBeNull();
  });

  it("the remove button of an action drops that action", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("button", { name: "Xoá hành động 1" }));
    expect(actionCards()).toHaveLength(1);
    expect(actionTypes(await save())).toEqual(["in_app_notification"]);
  });

  it("a new rule starts unsaved (no run history, no dry run, no delete) and saving creates it", async () => {
    await loaded();
    await userEvent.click(screen.getByRole("button", { name: "Tạo luật mới" }));
    expect(screen.getByRole("textbox", { name: /Tên luật/ })).toHaveValue("Luật mới");
    expect(screen.getByRole("button", { name: "Chạy thử" })).toBeDisabled();
    expect(screen.queryByRole("button", { name: "Xoá" })).toBeNull();
    vi.mocked(listWorkflowRuns).mockClear(); // the rule left open read its history; the unsaved one must not
    expect(await screen.findByText("Lưu luật trước")).toBeInTheDocument();
    expect(listWorkflowRuns).not.toHaveBeenCalled();

    vi.mocked(listWorkflows).mockResolvedValueOnce([RULE, TIMER_RULE, { ...RULE, id: "rule-new", name: "Luật mới", isActive: false }]);
    await userEvent.click(screen.getByRole("button", { name: "Tạo luật" }));
    await waitFor(() => expect(createWorkflow).toHaveBeenCalledTimes(1));
    expect(updateWorkflow).not.toHaveBeenCalled();
    expect(vi.mocked(createWorkflow).mock.calls[0][0]).toMatchObject({
      name: "Luật mới",
      entityName: "tk_tickets",
      trigger: "after_create",
      isActive: false,
      actions: [{ type: "in_app_notification", userId: "$owner" }],
    });
    expect(await screen.findByText("Đã lưu")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Luật mới/ })).toHaveAttribute("aria-current", "page");
  });

  it("with no rules the editor is an empty state whose button starts a new rule", async () => {
    vi.mocked(listWorkflows).mockResolvedValueOnce([]);
    render(<WorkflowBuilder />);
    await userEvent.click(await screen.findByRole("button", { name: "Tạo luật đầu tiên" }));
    expect(screen.getByRole("textbox", { name: /Tên luật/ })).toHaveValue("Luật mới");
  });
});
