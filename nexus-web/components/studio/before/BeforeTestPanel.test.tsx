// BeforeTestPanel inside WorkflowTestDialog (specs/048 FE-before-save-hooks-4, T163): pick the write to
// rehearse, type the simulated changes by field kind, read "Sẽ chặn: …" and the before → after rows.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowTestDialog } from "../WorkflowTestDialog";
import { testWorkflow, listRecords, type FieldMeta } from "@/lib/api";
import { changesFromRows, parseChangeValue } from "@/lib/workflowBefore";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listRecords: vi.fn(async () => [{ id: "11111111-aaaa", data: { name: "Cơ hội A" } }]),
    testWorkflow: vi.fn(),
  };
});

const FIELDS: FieldMeta[] = [
  { name: "stage", label: "Giai đoạn", fieldType: "picklist", options: { values: ["Proposal", "Closed Won"] } },
  { name: "probability", label: "Xác suất", fieldType: "number" },
  { name: "weighted", label: "Giá trị gia quyền", fieldType: "formula" },
];

const BLOCK_RESULT = {
  workflowId: "wf-1", entityName: "deals", recordId: "", trigger: "before_save", op: "create", matched: true,
  trace: [
    { step: 1, kind: "trigger", matched: true, detail: { trigger: "before_save", op: "create" } },
    { step: 2, kind: "condition", matched: true, detail: { matchAll: true } },
    { step: 3, kind: "action", evaluated: true, detail: { type: "veto", field: "", message: "Chốt Thắng phải có Ngày chốt", wouldBlock: true } },
  ],
};

beforeEach(() => {
  vi.mocked(testWorkflow).mockReset();
  vi.mocked(listRecords).mockClear();
});

function open(trigger = "before_save", saveOn?: string) {
  render(
    <WorkflowTestDialog onClose={vi.fn()} workflowId="wf-1" workflowName="Chặn" entityName="deals"
      trigger={trigger} fields={FIELDS} saveOn={saveOn} />,
  );
  return screen.getByRole("group", { name: "Chạy thử — Chặn" });
}

describe("BeforeTestPanel", () => {
  it("a simulated create needs no record: sends op + typed changes, shows 'Sẽ chặn' and no write", async () => {
    vi.mocked(testWorkflow).mockResolvedValue(BLOCK_RESULT as never);
    const dialog = open("before_save", "create");
    expect(within(dialog).getByRole("radio", { name: "Tạo mới một bản ghi" })).toBeChecked();
    expect(within(dialog).queryByText("Chọn một bản ghi")).toBeNull();

    await userEvent.click(within(dialog).getByRole("button", { name: "Thêm một thay đổi" }));
    await userEvent.click(within(dialog).getByRole("combobox", { name: "Trường" }));
    // computed fields (formula) are not offered as something to type
    expect(screen.queryByRole("option", { name: "Giá trị gia quyền" })).toBeNull();
    await userEvent.click(screen.getByRole("option", { name: "Xác suất" }));
    await userEvent.type(within(dialog).getByLabelText("Giá trị"), "80");

    await userEvent.click(within(dialog).getByRole("button", { name: "Chạy thử" }));
    await waitFor(() =>
      expect(testWorkflow).toHaveBeenCalledWith("wf-1", undefined, { op: "create", changes: { probability: 80 }, withEntityRules: false }),
    );
    expect(await within(dialog).findByText("Sẽ chặn: Chốt Thắng phải có Ngày chốt")).toBeInTheDocument();
    expect(within(dialog).getByText("Sẽ chặn")).toHaveClass("slds-badge");
  });

  it("an update needs a record; the chain checkbox travels as withEntityRules", async () => {
    vi.mocked(testWorkflow).mockResolvedValue({ ...BLOCK_RESULT, op: "update", recordId: "11111111-aaaa" } as never);
    const dialog = open("before_save");
    expect(within(dialog).getByRole("radio", { name: "Sửa bản ghi đã chọn" })).toBeChecked();
    const run = within(dialog).getByRole("button", { name: "Chạy thử" });
    expect(run).toBeDisabled();
    await userEvent.click(await within(dialog).findByRole("radio", { name: /Cơ hội A/ }));
    await userEvent.click(within(dialog).getByRole("checkbox", { name: /Áp cả chuỗi quy tắc/ }));
    await userEvent.click(run);
    await waitFor(() =>
      expect(testWorkflow).toHaveBeenCalledWith("wf-1", "11111111-aaaa", { op: "update", changes: {}, withEntityRules: true }),
    );
  });

  it("a before_delete rule has no changes to type, only a note", () => {
    const dialog = open("before_delete");
    expect(within(dialog).getByText(/không thay đổi nào/)).toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Thêm một thay đổi" })).toBeNull();
  });

  it("an after rule keeps the plain record dry-run (no panel, two-argument call)", async () => {
    vi.mocked(testWorkflow).mockResolvedValue({ ...BLOCK_RESULT, trigger: "after_create" } as never);
    const dialog = open("after_create");
    expect(within(dialog).queryByText("Lần ghi để thử")).toBeNull();
    await userEvent.click(await within(dialog).findByRole("radio", { name: /Cơ hội A/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Chạy thử" }));
    await waitFor(() => expect(testWorkflow).toHaveBeenCalledWith("wf-1", "11111111-aaaa"));
  });
});

describe("changesFromRows", () => {
  it("types the value by field kind and skips a row with no field", () => {
    const typeOf = (f: string) => ({ n: "number", b: "boolean" } as Record<string, string>)[f] ?? "text";
    expect(changesFromRows([{ field: "n", value: "5" }, { field: "b", value: "true" }, { field: "t", value: "x" }, { field: "", value: "y" }], typeOf))
      .toEqual({ n: 5, b: true, t: "x" });
    expect(parseChangeValue("number", "")).toBeNull();
  });
});
