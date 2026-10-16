// Queue #282: opening a rule in the builder and pressing "Cập nhật" without touching anything must
// save the rule back as it was read. The builder used to (a) stringify every key it had no special
// loader for (`String(v)`: an object became "[object Object]"), (b) keep only the keys it had a box
// for — an action type with no card (normalize) lost its field and ops — and (c) fill a blank box with
// an invented value on save (a notification with no title got "Thông báo", a new email went to
// "user@example.com", a new webhook to "https://example.com/hook").
//
// A webhook's own signing key / headers are the exception, by design (specs/050 FR-023/FR-025): the
// read masks them and every write refuses them, so the builder never sends them back — the server then
// answers 409 WEBHOOK_LEGACY_SECRET_PENDING and the stored key stays where it is.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, within, fireEvent, cleanup } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, createWorkflow, listWorkflows, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-rt",
  name: "Đơn đã thanh toán",
  entityName: "orders",
  trigger: "after_update",
  isActive: true,
  condition: { match: "and", rules: [{ field: "status", op: "changedTo", value: "paid" }] },
  actions: [
    { type: "field_update", field: "amount", value: 100 },
    { type: "in_app_notification", userId: "$owner", message: "Có đơn đã thanh toán" },
    { type: "email_alert", to: "{{email}}", subject: "Cảm ơn", body: "Đã nhận tiền" },
    { type: "assign", strategy: "load_based", team: "team-1", field: "handler" },
    { type: "outbound_webhook", credentialId: "cred-1", path: "/orders" },
    { type: "set_deadline", field: "due_at", offset: { amount: 4, unit: "hours" }, mode: "shorten_only" },
    { type: "set_elapsed", field: "minutes" },
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
    listEntities: vi.fn(async () => [{ name: "orders", label: "Đơn hàng" }]),
    listFields: vi.fn(async () => [
      { name: "amount", label: "Số tiền", fieldType: "number" },
      { name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["new", "paid"] } },
      { name: "email", label: "Email", fieldType: "email" },
      { name: "handler", label: "Người xử lý", fieldType: "user" },
      { name: "due_at", label: "Hạn", fieldType: "datetime" },
      { name: "minutes", label: "Số phút", fieldType: "number" },
      { name: "total", label: "Tổng", fieldType: "currency" },
      { name: "paid", label: "Đã trả", fieldType: "boolean" },
      { name: "issued_at", label: "Phát hành", fieldType: "datetime" },
    ]),
    listWorkflowRuns: vi.fn(async () => ({ runs: [], total: 0, limit: 20, offset: 0 })),
    listRecentWorkflowRuns: vi.fn(async () => []),
  };
});

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return {
    ...actual,
    listBusinessCalendars: vi.fn(async () => []),
    listTeams: vi.fn(async () => [{ id: "team-1", name: "Kế toán" }]),
  };
});


const savedActions = () => (vi.mocked(updateWorkflow).mock.calls.at(-1)![1] as { actions: unknown[] }).actions;

const savedPayload = () => vi.mocked(updateWorkflow).mock.calls.at(-1)![1] as Record<string, unknown>;

async function saveUntouched(rule: Record<string, unknown>) {
  vi.mocked(listWorkflows).mockResolvedValueOnce([rule as never]);
  render(<WorkflowBuilder />);
  // Wait for the rule's fields: until they are in, a field_update has no typed widget yet.
  await screen.findByDisplayValue(String(rule.name), {}, { timeout: 3000 });
  await touchRule();
  fireEvent.click(await screen.findByRole("button", { name: "Cập nhật" }, { timeout: 3000 }));
  await waitFor(() => expect(updateWorkflow).toHaveBeenCalled(), { timeout: 3000 });
  return savedActions();
}

describe("WorkflowBuilder — open a rule, save it untouched", () => {
  beforeEach(() => {
    vi.mocked(updateWorkflow).mockClear();
    vi.mocked(createWorkflow).mockClear();
  });

  it("every action goes back exactly as it was read — types kept, no blank box filled in", async () => {
    expect(await saveUntouched(RULE)).toEqual(RULE.actions);
  });

  it("create_record: a value resolved at fire time and a datetime with seconds/offset go back verbatim", async () => {
    const rule = {
      ...RULE,
      id: "rule-cr",
      name: "Tạo hợp đồng",
      actions: [{
        type: "create_record",
        entityName: "orders",
        values: {
          total: "$record.amount",
          paid: "$record.active",
          issued_at: "2026-10-01T08:00:30+07:00",
          title: "Hợp đồng {{name}}",
          handler: "$owner",
        },
      }],
    };
    expect(await saveUntouched(rule)).toEqual(rule.actions);
  });

  it("a scheduled rule keeps its cadence, including 0 (fire once), not the box default", async () => {
    for (const [i, intervalSeconds] of [604800, 0].entries()) {
      const rule = { ...RULE, id: `rule-sch-${i}`, name: `Định kỳ ${i}`, trigger: "scheduled", condition: null, intervalSeconds };
      await saveUntouched(rule);
      expect(savedPayload().intervalSeconds).toBe(intervalSeconds);
      cleanup();
      vi.mocked(updateWorkflow).mockClear();
    }
  });

  it("a date_offset rule keeps its recurrence (condition.timer.repeat)", async () => {
    const timer = {
      dateField: "due_at", offset: { amount: 1, unit: "days" }, direction: "before",
      repeat: { every: 2, unit: "days", count: 5 },
    };
    const rule = { ...RULE, id: "rule-timer", name: "Nhắc hạn", trigger: "date_offset", condition: { timer }, actions: [RULE.actions[1]] };
    await saveUntouched(rule);
    expect((savedPayload().condition as { timer: unknown }).timer).toEqual(timer);
  });

  it("an action the builder has no card for, and a key no card edits, ride along verbatim", async () => {
    const rule = {
      ...RULE,
      id: "rule-before",
      name: "Chuẩn hoá email",
      trigger: "before_save",
      condition: null,
      actions: [
        { type: "normalize", field: "email", ops: ["trim", "lower"] },
        { type: "field_update", field: "status", value: "new", onlyIfEmpty: true },
      ],
    };
    expect(await saveUntouched(rule)).toEqual(rule.actions);
  });

  it("a key added to an action after this screen was written keeps its type", async () => {
    const rule = {
      ...RULE,
      id: "rule-future",
      name: "Khoá mới",
      actions: [{ type: "in_app_notification", userId: "$owner", title: "Đơn", message: "Có đơn", channel: { push: true, priority: 2 } }],
    };
    expect(await saveUntouched(rule)).toEqual(rule.actions);
  });

  it("a legacy webhook's masked headers are never sent back (the server keeps the key and answers 409)", async () => {
    const rule = {
      ...RULE,
      id: "rule-legacy",
      name: "Webhook cũ",
      actions: [{ type: "outbound_webhook", url: "https://erp.test/in", headers: { Authorization: "••••" } }],
    };
    expect(await saveUntouched(rule)).toEqual([{ type: "outbound_webhook", url: "https://erp.test/in" }]);
  });

  it("a value the server refuses at save time is pinned on that action, not only a generic footer", async () => {
    // Since #282 the server checks a field_update value on save (a rule the old builder stringified
    // comes back RECORD_INVALID_TYPE); the footer's mapped sentence alone would not say which action.
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "RECORD_INVALID_TYPE", "field \"amount\" phải là số", { field: "amount" }),
    );
    await saveUntouched(RULE);
    const first = within(screen.getByRole("heading", { name: "Thì" }).closest("section") as HTMLElement).getAllByRole("article")[0];
    expect(await within(first).findByText("field \"amount\" phải là số")).toBeInTheDocument();
    expect(screen.getByTitle(/xem hành động được đánh dấu đỏ/)).toBeInTheDocument();
  });

  it("a new action sends what its boxes show — no placeholder address, no placeholder host", async () => {
    render(<WorkflowBuilder />);
    await screen.findByDisplayValue(RULE.name, {}, { timeout: 3000 });
    await userEvent.click(screen.getByRole("button", { name: "Tạo luật mới" }));
    for (const label of ["Gửi email", "Gọi webhook", "Thông báo trong app"]) {
      await userEvent.click(screen.getByRole("button", { name: "Thêm hành động" }));
      await userEvent.click(await screen.findByRole("menuitem", { name: label }));
    }
    await userEvent.click(screen.getByRole("button", { name: "Tạo luật" }));
    await waitFor(() => expect(createWorkflow).toHaveBeenCalledTimes(1));
    const actions = (vi.mocked(createWorkflow).mock.calls[0][0] as { actions: unknown[] }).actions;
    expect(actions).toEqual([
      // The new rule's first card is pre-filled on screen, so its values are what the user sees.
      { type: "in_app_notification", userId: "$owner", title: "Thông báo", message: "Workflow đã kích hoạt." },
      { type: "email_alert", to: "", subject: "", body: "" },
      { type: "outbound_webhook", url: "" },
      { type: "in_app_notification", userId: "$owner", title: "Thông báo", message: "Workflow đã kích hoạt." },
    ]);
  });
});
