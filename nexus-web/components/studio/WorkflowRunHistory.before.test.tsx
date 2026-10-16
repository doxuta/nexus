// WorkflowRunHistory for before rules (specs/048 FE-before-save-hooks-4, T164 / S3 review leftovers):
// "Đã chặn" badge, a refused create / a batch summary no longer read "quét theo lịch", the batch
// detail card is not empty, and `skipped` reads in words, not as the engine token.
import { describe, it, expect, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowRunHistory } from "./WorkflowRunHistory";
import { listWorkflowRuns } from "@/lib/api";
import type { WorkflowRun } from "@/lib/workflowRuns";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return { ...actual, listWorkflowRuns: vi.fn(), listRecentWorkflowRuns: vi.fn() };
});

const base = { workflowId: "wf-1", startedAt: "2026-10-14T01:00:00Z", finishedAt: "2026-10-14T01:00:01Z", error: null };
const RUNS = [
  { ...base, id: "b1", triggerType: "before_save", recordId: null, status: "blocked", actionsResult: [{ type: "veto", ok: true }] },
  { ...base, id: "b2", triggerType: "before_save", recordId: null, status: "blocked", actionsResult: { blocked: 2, applied: 0, failed: 0 } },
  { ...base, id: "b3", triggerType: "before_save", recordId: "rec-b3-0000", status: "skipped", actionsResult: [{ type: "veto", skipped: "approval_outcome" }] },
  { ...base, id: "s1", triggerType: "scheduled", recordId: null, status: "success", actionsResult: { scanned: 4, matched: 1, failed: 0 } },
] as unknown as WorkflowRun[];

describe("WorkflowRunHistory — before rules", () => {
  it("labels a record-less run by what it is, and opens a batch / skipped run into words", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: RUNS, total: 4, limit: 20, offset: 0 });
    render(<WorkflowRunHistory workflowId="wf-1" entityName="deals" />);
    const grid = await screen.findByRole("grid", { name: "Lịch sử chạy" });
    const rows = within(grid).getAllByRole("row").slice(1);

    expect(within(rows[0]).getByText("Đã chặn")).toHaveClass("slds-theme_error");
    expect(within(rows[0]).getByText("bản ghi mới (chưa tạo)")).toBeInTheDocument();
    expect(within(rows[1]).getByText("cả lô nhiều dòng")).toBeInTheDocument();
    expect(within(rows[3]).getByText("quét theo lịch")).toBeInTheDocument();

    await userEvent.click(within(rows[1]).getByRole("button", { expanded: false }));
    const detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    expect(within(detail).getByText("Dòng bị chặn").nextElementSibling).toHaveTextContent("2");
    expect(within(detail).queryByText(/Không có chi tiết/)).toBeNull();

    await userEvent.click(within(rows[2]).getByRole("button", { expanded: false }));
    const skipped = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    expect(within(skipped).queryByText(/skipped:/)).toBeNull();
    expect(within(skipped).getByText("Bỏ qua (ghi kết quả duyệt)")).toBeInTheDocument();
  });
});
