// WorkflowTestDialog on SLDS (T043): modals/base, a record picked from the rule's entity (the top rows
// before anything is typed — the old picker's browse behaviour), then the dry-run trace.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowTestDialog } from "./WorkflowTestDialog";
import { listRecords, testWorkflow } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listRecords: vi.fn(async () => [
      { id: "11111111-aaaa", data: { subject: "Máy in hỏng" } },
      { id: "22222222-bbbb", data: { subject: "Mất mạng" } },
      { id: "33333333-cccc", data: { subject: "Quên mật khẩu" } },
    ]),
    testWorkflow: vi.fn(async () => ({
      workflowId: "wf-1", entityName: "tk_tickets", recordId: "11111111-aaaa", trigger: "after_create", matched: true,
      trace: [
        { step: 1, kind: "trigger", matched: true, detail: { trigger: "after_create" } },
        { step: 2, kind: "condition", matched: true, detail: { matchAll: true } },
        { step: 3, kind: "action", evaluated: true, detail: { type: "set_deadline", wouldApply: true } },
      ],
    })),
  };
});

beforeEach(() => vi.mocked(testWorkflow).mockClear());

function open() {
  render(<WorkflowTestDialog onClose={vi.fn()} workflowId="wf-1" workflowName="Đặt hạn SLA" entityName="tk_tickets" />);
  return screen.getByRole("group", { name: "Chạy thử — Đặt hạn SLA" });
}

describe("WorkflowTestDialog (SLDS)", () => {
  it("opens in place (no dialog), browses records, picks one, and shows the dry-run trace", async () => {
    const onClose = vi.fn();
    render(<WorkflowTestDialog onClose={onClose} workflowId="wf-1" workflowName="Đặt hạn SLA" entityName="tk_tickets" />);
    const dialog = screen.getByRole("group", { name: "Chạy thử — Đặt hạn SLA" });
    // specs/074 lát 11: the dry run is a section of the rule page, never a dialog.
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(dialog.closest(".slds-modal")).toBeNull();
    await waitFor(() => expect(listRecords).toHaveBeenCalledWith("tk_tickets", "limit=8"));

    const run = within(dialog).getByRole("button", { name: "Chạy thử" });
    expect(run).toBeDisabled();
    await userEvent.click(await within(dialog).findByRole("radio", { name: /Máy in hỏng/ }));
    expect(run).toBeEnabled();

    await userEvent.click(run);
    await waitFor(() => expect(testWorkflow).toHaveBeenCalledWith("wf-1", "11111111-aaaa"));
    // The tested record collapses into a pill with a remove button (the old chip + X).
    expect(within(dialog).getByRole("button", { name: /Máy in hỏng/ })).toBeInTheDocument();
    expect(within(dialog).queryByRole("radio")).toBeNull();
    expect(await within(dialog).findByText("KHỚP — luật sẽ chạy với bản ghi này")).toBeInTheDocument();
    expect(within(dialog).getByText("Trigger")).toBeInTheDocument();
    expect(within(dialog).getByText("Luôn khớp")).toHaveClass("slds-badge");
    expect(within(dialog).getByRole("button", { name: "Chạy lại" })).toBeEnabled();

    await userEvent.click(within(dialog).getByRole("button", { name: "Đóng" }));
    expect(onClose).toHaveBeenCalled();
  });

  it("arrow keys move the pick without committing it, so every record is reachable from the keyboard", async () => {
    const dialog = open();
    const first = await within(dialog).findByRole("radio", { name: /Máy in hỏng/ });
    first.focus();
    // Native radios tick the next one on an arrow press: the list has to survive that.
    await userEvent.keyboard("{ArrowDown}");
    expect(within(dialog).getByRole("radio", { name: /Mất mạng/ })).toBeChecked();
    await userEvent.keyboard("{ArrowDown}");
    const third = within(dialog).getByRole("radio", { name: /Quên mật khẩu/ });
    expect(third).toBeChecked();
    expect(third).toHaveFocus();
    expect(within(dialog).getAllByRole("radio")).toHaveLength(3);
    expect(testWorkflow).not.toHaveBeenCalled();

    await userEvent.click(within(dialog).getByRole("button", { name: "Chạy thử" }));
    await waitFor(() => expect(testWorkflow).toHaveBeenCalledWith("wf-1", "33333333-cccc"));
    expect(within(dialog).getByRole("button", { name: /Quên mật khẩu/ })).toBeInTheDocument();
  });

  it("removing the pill goes back to the list with the last pick still ticked", async () => {
    const dialog = open();
    await userEvent.click(await within(dialog).findByRole("radio", { name: /Mất mạng/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Chạy thử" }));
    await waitFor(() => expect(testWorkflow).toHaveBeenCalledWith("wf-1", "22222222-bbbb"));
    await userEvent.click(within(dialog).getByRole("button", { name: /Bỏ chọn/ }));
    expect(await within(dialog).findByRole("radio", { name: /Mất mạng/ })).toBeChecked();
    expect(within(dialog).queryByText("KHỚP — luật sẽ chạy với bản ghi này")).toBeNull();
  });

  // FE-stop-clock-action (specs/046): the set_elapsed rehearsal says the number it would write and
  // from when; an org without a working calendar gets the wall-clock note as a WARNING beside it.
  it("a set_elapsed step shows the minutes and warns when they are wall-clock", async () => {
    const note = "Tổ chức chưa khai lịch làm việc — số phút sẽ tính theo giờ thường";
    vi.mocked(testWorkflow).mockResolvedValueOnce({
      workflowId: "wf-1", entityName: "tk_tickets", recordId: "11111111-aaaa", trigger: "after_update", matched: true,
      trace: [
        { step: 1, kind: "trigger", matched: true, detail: { trigger: "after_update" } },
        { step: 2, kind: "condition", matched: true, detail: { matchAll: true } },
        {
          step: 3, kind: "action", evaluated: true,
          detail: { type: "set_elapsed", field: "resolution_minutes", before: null, from: "2026-09-25T09:00:00Z", after: 1234, calendar: note, wouldApply: true },
        },
      ],
    });
    const dialog = open();
    await userEvent.click(await within(dialog).findByRole("radio", { name: /Máy in hỏng/ }));
    await userEvent.click(within(dialog).getByRole("button", { name: "Chạy thử" }));
    const step = (await within(dialog).findByText("Ghi thời gian xử lý")).closest("li") as HTMLElement;
    expect(within(step).getByText("Sẽ chạy")).toHaveClass("slds-badge");
    expect(within(step).getByText("16:00 25/09/2026")).toBeInTheDocument();
    expect(within(step).getByText("1.234 phút (≈ 20,6 giờ)")).toBeInTheDocument();
    expect(within(step).getByText(note).closest(".slds-scoped-notification")).toHaveClass("slds-theme_warning");
  });
});
