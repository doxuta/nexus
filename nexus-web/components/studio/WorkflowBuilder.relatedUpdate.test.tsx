// specs/058 — the "Cập nhật trường" card picks WHICH record it writes: the triggering record or the
// record one of its relation fields points at ("hoá đơn Đã thanh toán ⇒ lịch hẹn đổi trạng thái"),
// then a field of THAT record's entity from a list, then a value typed for that field.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowBuilder } from "./WorkflowBuilder";
import { ApiError, listFields, updateWorkflow } from "@/lib/api";
import { touchRule } from "@/test/touchRule";

const RULE = {
  id: "rule-paid",
  name: "Hoá đơn trả xong thì chốt lịch hẹn",
  entityName: "nk_invoices",
  trigger: "after_update",
  isActive: true,
  condition: { match: "and", rules: [{ field: "status", op: "changedTo", value: "paid" }] },
  actions: [
    { type: "field_update", via: "appointment", field: "fee", value: 100 },
  ],
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
      { name: "nk_invoices", label: "Hoá đơn" },
      { name: "nk_appointments", label: "Lịch hẹn khám" },
    ]),
    listFields: vi.fn(async (entity: string) => entity === "nk_appointments"
      ? [
          { name: "status", label: "Trạng thái lịch", fieldType: "picklist", options: { values: ["booked", "paid"] } },
          { name: "fee", label: "Phí khám", fieldType: "number" },
          { name: "memo", label: "Ghi chú lịch", fieldType: "text" },
          { name: "total", label: "Tổng", fieldType: "formula" },
          { name: "deposit", label: "Tiền cọc", fieldType: "currency", options: { currencyCode: "USD" } },
        ]
      : [
          { name: "status", label: "Trạng thái", fieldType: "picklist", options: { values: ["unpaid", "paid"] } },
          { name: "appointment", label: "Lịch hẹn", fieldType: "relation", options: { relatedEntity: "nk_appointments" } },
          { name: "note", label: "Ghi chú", fieldType: "text" },
          // Same NAME as the appointment's currency field, different type: a value typed against the
          // rule's own entity instead of the related one would come out as the string "250".
          { name: "deposit", label: "Tiền cọc HĐ", fieldType: "text" },
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
  vi.mocked(listFields).mockClear();
});

const actionCard = () => {
  const then = screen.getByRole("heading", { name: "Thì" }).closest("section") as HTMLElement;
  return within(then).getAllByRole("article")[0];
};
async function opened() {
  render(<WorkflowBuilder />);
  // Settled = the RELATED entity's fields are in ("Phí khám" is an option). Before the rule's own
  // fields arrive the picker already shows "fee" as a stale option — waiting on the value alone
  // caught that frame whenever no earlier fetch had pre-filled the rule's fields (queue #297 dropped
  // the hardcoded "accounts" default that used to).
  await waitFor(() => {
    const field = within(actionCard()).getByRole("combobox", { name: "Trường" });
    expect(field).toHaveValue("fee");
    expect(within(field).getByRole("option", { name: "Phí khám" })).toBeInTheDocument();
  }, { timeout: 3000 });
}
async function saved() {
  await touchRule();
  await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
  await waitFor(() => expect(updateWorkflow).toHaveBeenCalled());
  return (vi.mocked(updateWorkflow).mock.calls[0][1] as { actions: Record<string, unknown>[] }).actions;
}

describe("WorkflowBuilder — field_update on a related record (specs/058)", () => {
  it("opens a saved rule on the related record and re-saves its value untouched and typed", async () => {
    await opened();
    const card = within(actionCard());
    expect(card.getByRole("combobox", { name: "Sửa bản ghi" })).toHaveValue("appointment");
    expect(card.getByRole("option", { name: "Bản ghi liên quan qua “Lịch hẹn” (Lịch hẹn khám)" })).toBeInTheDocument();
    // The field list is the RELATED entity's writable fields — not the invoice's, no formula.
    const fieldOpts = within(card.getByRole("combobox", { name: "Trường" })).getAllByRole("option").map((o) => o.textContent);
    expect(fieldOpts).toEqual(["— chọn trường —", "Trạng thái lịch", "Phí khám", "Ghi chú lịch", "Tiền cọc"]);
    expect(await saved()).toEqual([{ type: "field_update", via: "appointment", field: "fee", value: 100 }]);
  });

  it("switching to another field of the related record writes a typed value there", async () => {
    await opened();
    const card = within(actionCard());
    await userEvent.selectOptions(card.getByRole("combobox", { name: "Trường" }), "memo");
    await userEvent.type(card.getByRole("textbox", { name: "Ghi chú lịch" }), "Đã thu tiền");
    expect(await saved()).toEqual([{ type: "field_update", via: "appointment", field: "memo", value: "Đã thu tiền" }]);
  });

  // §2.9 "tiền là tiền" on the RELATED record: the value is typed against the related entity's field
  // (currency ⇒ { amount, currencyCode } of THAT field), not the rule's own same-named text field and
  // not passed through raw.
  it("a currency field of the related record is sent as money in that field's currency", async () => {
    await opened();
    const card = within(actionCard());
    await userEvent.selectOptions(card.getByRole("combobox", { name: "Trường" }), "deposit");
    await userEvent.type(card.getByRole("textbox", { name: /^Tiền cọc/ }), "250");
    expect(await saved()).toEqual([
      { type: "field_update", via: "appointment", field: "deposit", value: { amount: 250, currencyCode: "USD" } },
    ]);
  });

  // Review finding: an empty value saved as {type, via, field} and every run then CLEARED the related
  // record's field. The builder refuses to save it and says so on the value box.
  it("a chosen field with an empty value blocks the save and is flagged on the value box", async () => {
    await opened();
    const card = within(actionCard());
    await userEvent.selectOptions(card.getByRole("combobox", { name: "Trường" }), "status");
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    const value = card.getByRole("combobox", { name: "Trạng thái lịch" });
    await waitFor(() => expect(value).toHaveAttribute("aria-invalid", "true"));
    expect(card.getByText("Chọn giá trị để ghi vào trường này.")).toBeInTheDocument();
    expect(card.getByRole("combobox", { name: "Trường" })).not.toHaveAttribute("aria-invalid", "true");
    expect(updateWorkflow).not.toHaveBeenCalled();

    await userEvent.click(value);
    await userEvent.click(card.getByRole("option", { name: "paid" }));
    expect(card.getByRole("combobox", { name: "Trạng thái lịch" })).not.toHaveAttribute("aria-invalid", "true");
    expect(await saved()).toEqual([{ type: "field_update", via: "appointment", field: "status", value: "paid" }]);
  });

  it("switching back to the triggering record drops via and lists the rule's own fields", async () => {
    await opened();
    const card = within(actionCard());
    await userEvent.selectOptions(card.getByRole("combobox", { name: "Sửa bản ghi" }), "");
    const fieldOpts = within(card.getByRole("combobox", { name: "Trường" })).getAllByRole("option").map((o) => o.textContent);
    expect(fieldOpts).toEqual(["— chọn trường —", "Trạng thái", "Lịch hẹn", "Ghi chú", "Tiền cọc HĐ"]);
    await userEvent.selectOptions(card.getByRole("combobox", { name: "Trường" }), "note");
    await userEvent.type(card.getByRole("textbox", { name: "Ghi chú" }), "ok");
    expect(await saved()).toEqual([{ type: "field_update", field: "note", value: "ok" }]);
  });

  // Queue #333/#305: the value can be copied from the record that fired the rule — picked, not typed.
  it("takes the value from a same-type field of the triggering record and saves it as $record.<field>", async () => {
    await opened();
    const card = within(actionCard());
    await userEvent.selectOptions(card.getByRole("combobox", { name: "Trường" }), "memo");
    const src = card.getByRole("combobox", { name: "Giá trị lấy từ" });
    // Only text fields of the invoice fit a text target; none of the typed widget's own value leaks.
    expect(within(src).getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Tự nhập giá trị", "Trường “Ghi chú” của bản ghi kích hoạt luật", "Trường “Tiền cọc HĐ” của bản ghi kích hoạt luật",
    ]);
    await userEvent.selectOptions(src, "$record.note");
    expect(card.queryByRole("textbox", { name: "Ghi chú lịch" })).not.toBeInTheDocument();
    expect(card.getByText(/được chép từ bản ghi kích hoạt/)).toBeInTheDocument();
    expect(await saved()).toEqual([{ type: "field_update", via: "appointment", field: "memo", value: "$record.note" }]);
    // Back to typing restores the field's own input.
    await userEvent.selectOptions(src, "");
    expect(card.getByRole("textbox", { name: "Ghi chú lịch" })).toBeInTheDocument();
  });

  it("offers no source picker when no field of the triggering record fits the target", async () => {
    await opened(); // target = fee (number); the invoice has no number field
    expect(within(actionCard()).queryByRole("combobox", { name: "Giá trị lấy từ" })).not.toBeInTheDocument();
  });

  it("a 422 on the via is pinned on the record picker", async () => {
    vi.mocked(updateWorkflow).mockRejectedValueOnce(
      new ApiError(422, "WORKFLOW_RELATED_VIA_INVALID", "'via' \"appointment\" không phải trường quan hệ", { field: "appointment" }),
    );
    await opened();
    await touchRule();
    await userEvent.click(screen.getByRole("button", { name: "Cập nhật" }));
    await waitFor(() => expect(within(actionCard()).getByRole("combobox", { name: "Sửa bản ghi" })).toHaveAttribute("aria-invalid", "true"));
    expect(within(actionCard()).getByRole("combobox", { name: "Trường" })).not.toHaveAttribute("aria-invalid", "true");
  });
});
