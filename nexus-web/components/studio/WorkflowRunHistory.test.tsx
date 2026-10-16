// WorkflowRunHistory on SLDS (T043 · queue #106): runs are a data-tables grid, one run opens into a
// detail card. A `business_hours` deadline that fell back to wall-clock time still ends `success`, and
// actionsResult[].warning is the ONLY trace of it (api-conventions §9.3) — so the row must say a run
// carries a warning, and the detail must print the sentence verbatim with the warning marker.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { WorkflowRunHistory, RecentRunsStrip } from "./WorkflowRunHistory";
import { listWorkflowRuns, listRecentWorkflowRuns, type WorkflowSummary } from "@/lib/api";
import type { WorkflowRun } from "@/lib/workflowRuns";

const WARNING = "Chưa có lịch làm việc mặc định — hạn được tính theo giờ thường.";

const run = (id: string, patch: Partial<WorkflowRun> = {}): WorkflowRun => ({
  id,
  workflowId: "wf-1",
  recordId: `rec-${id}-0000-0000`,
  triggerType: "after_create",
  status: "success",
  actionsResult: [{ type: "set_deadline", ok: true, field: "due_date" }],
  startedAt: "2026-09-29T01:00:00Z",
  finishedAt: "2026-09-29T01:00:01Z",
  ...patch,
} as WorkflowRun);

const RUNS = [
  run("r1", { actionsResult: [{ type: "set_deadline", ok: true, field: "due_date", warning: WARNING }] }),
  run("r2", { status: "failed", error: "hành động lỗi", actionsResult: [{ type: "outbound_webhook", ok: false, error: "timeout 5s" }] }),
  run("r3", { recordId: undefined, triggerType: "scheduled", actionsResult: { scanned: 12, matched: 3, failed: 0 } }),
];

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    listWorkflowRuns: vi.fn(),
    listRecentWorkflowRuns: vi.fn(),
  };
});

beforeEach(() => {
  vi.mocked(listWorkflowRuns).mockReset();
  vi.mocked(listRecentWorkflowRuns).mockReset();
});

const gridRow = (text: string) => screen.getAllByRole("row").find((r) => r.textContent?.includes(text))!;

describe("WorkflowRunHistory (SLDS)", () => {
  it("lists runs in the data table; the warning run is flagged in its row and opened prints the sentence verbatim (#106)", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: RUNS, total: 3, limit: 20, offset: 0 });
    render(<WorkflowRunHistory workflowId="wf-1" entityName="tk_tickets" />);
    const grid = await screen.findByRole("grid", { name: "Lịch sử chạy" });
    expect(grid).toHaveClass("slds-table");
    expect(screen.getByText("3 lần chạy — mới nhất trước")).toBeInTheDocument();

    // Row level: the success run carries a warning marker (yellow), the others do not.
    const warned = gridRow("rec-r1");
    expect(within(warned).getByText("Thành công")).toBeInTheDocument();
    const marker = within(warned).getByText("Có cảnh báo").closest(".slds-icon_container")!;
    expect(marker.querySelector(".slds-icon-text-warning")).not.toBeNull();
    expect(within(gridRow("rec-r2")).queryByText("Có cảnh báo")).toBeNull();

    // Detail: warning sentence verbatim inside a warning-themed notification + warning marker on the action.
    await userEvent.click(within(warned).getByRole("button", { expanded: false }));
    const detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    const sentence = within(detail).getByText(WARNING);
    expect(sentence.closest(".slds-theme_warning")).not.toBeNull();
    // The action reads in Vietnamese (lib/workflowRuns actionTitle), not as the engine token.
    const action = within(detail).getByText("Đặt hạn xử lý").closest("li")!;
    expect(action.querySelector(".slds-icon-text-warning")).not.toBeNull();
    expect(within(detail).getByRole("link", { name: /Mở bản ghi/ })).toHaveAttribute("href", "/tk_tickets/rec-r1-0000-0000");
  });

  it("a failed action prints its error; a scheduled scan shows its counts", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: RUNS, total: 3, limit: 20, offset: 0 });
    render(<WorkflowRunHistory workflowId="wf-1" entityName="tk_tickets" />);
    await screen.findByRole("grid", { name: "Lịch sử chạy" });

    await userEvent.click(within(gridRow("rec-r2")).getByRole("button", { expanded: false }));
    let detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    expect(within(detail).getByText("hành động lỗi")).toBeInTheDocument();
    expect(within(detail).getByText("timeout 5s")).toHaveClass("slds-text-color_error");
    expect(within(detail).getByText("Gọi webhook").closest("li")!.querySelector(".slds-icon-text-error")).not.toBeNull();

    await userEvent.click(within(gridRow("quét theo lịch")).getByRole("button", { expanded: false }));
    detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    expect(within(detail).getByText("12")).toBeInTheDocument();
    expect(within(detail).getByText("3")).toBeInTheDocument();
  });

  // DAILY-QUEUE #105 (a): a scheduled scan's wall-clock deadlines reach the one summary row as
  // warned + warnings — the row is flagged and the detail prints the count and each sentence verbatim.
  it("a scheduled scan with warned actions is flagged and prints each sentence", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValue({
      runs: [run("r6", { recordId: undefined, triggerType: "scheduled",
        actionsResult: { scanned: 40, matched: 7, failed: 0, warned: 7, warnings: [WARNING] } })],
      total: 1, limit: 20, offset: 0,
    });
    render(<WorkflowRunHistory workflowId="wf-1" entityName="tk_tickets" />);
    await screen.findByRole("grid", { name: "Lịch sử chạy" });
    const row = gridRow("quét theo lịch");
    expect(within(row).getByText("Có cảnh báo")).toBeInTheDocument();
    await userEvent.click(within(row).getByRole("button", { expanded: false }));
    const detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    expect(within(detail).getByText("Hành động có cảnh báo")).toBeInTheDocument();
    expect(within(detail).getByText(WARNING).closest(".slds-theme_warning")).not.toBeNull();
  });

  // FE-stop-clock-action (specs/046): a set_elapsed run on the no-calendar fallback ends `success`
  // with a warning — the detail names the action and prints the sentence under the warning marker.
  it("a set_elapsed run names the action and carries its wall-clock warning", async () => {
    const note = "Tổ chức chưa có lịch làm việc mặc định dùng được — số phút này tính theo giờ treo tường.";
    vi.mocked(listWorkflowRuns).mockResolvedValue({
      runs: [run("r5", { triggerType: "after_update", actionsResult: [{ type: "set_elapsed", ok: true, field: "resolution_minutes", warning: note }] })],
      total: 1, limit: 20, offset: 0,
    });
    render(<WorkflowRunHistory workflowId="wf-1" entityName="tk_tickets" />);
    await screen.findByRole("grid", { name: "Lịch sử chạy" });
    await userEvent.click(within(gridRow("rec-r5")).getByRole("button", { expanded: false }));
    const detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
    const action = within(detail).getByText("Ghi thời gian xử lý").closest("li")!;
    expect(action.querySelector(".slds-icon-text-warning")).not.toBeNull();
    expect(within(action).getByText("resolution_minutes")).toBeInTheDocument();
    expect(within(action).getByText(note).closest(".slds-theme_warning")).not.toBeNull();
  });

  it("opening a run brings its detail card into view (the grid can be 20+ rows above it)", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: RUNS, total: 3, limit: 20, offset: 0 });
    const scroll = vi.fn();
    const orig = Element.prototype.scrollIntoView;
    Element.prototype.scrollIntoView = scroll;
    try {
      render(<WorkflowRunHistory workflowId="wf-1" entityName="tk_tickets" />);
      await screen.findByRole("grid", { name: "Lịch sử chạy" });
      await userEvent.click(within(gridRow("rec-r2")).getByRole("button", { expanded: false }));
      const detail = screen.getByRole("region", { name: /Chi tiết lần chạy/ });
      await waitFor(() => expect(scroll).toHaveBeenCalled());
      expect(scroll.mock.contexts.at(-1)).toBe(detail);
    } finally {
      Element.prototype.scrollIntoView = orig;
    }
  });

  it("loads the next page from the current offset", async () => {
    vi.mocked(listWorkflowRuns)
      .mockResolvedValueOnce({ runs: RUNS.slice(0, 2), total: 3, limit: 20, offset: 0 })
      .mockResolvedValueOnce({ runs: RUNS.slice(2), total: 3, limit: 20, offset: 2 });
    render(<WorkflowRunHistory workflowId="wf-1" entityName="tk_tickets" />);
    await userEvent.click(await screen.findByRole("button", { name: "Tải thêm (1 còn lại)" }));
    await waitFor(() => expect(listWorkflowRuns).toHaveBeenLastCalledWith("wf-1", 20, 2));
    expect(await screen.findByText("quét theo lịch")).toBeInTheDocument();
  });

  it("empty history is the SLDS empty state", async () => {
    vi.mocked(listWorkflowRuns).mockResolvedValue({ runs: [], total: 0, limit: 20, offset: 0 });
    render(<WorkflowRunHistory workflowId="wf-1" />);
    expect(await screen.findByText("Chưa có lần chạy nào")).toBeInTheDocument();
  });
});

describe("RecentRunsStrip (SLDS)", () => {
  it("lists tenant-wide runs as split-view items and opens the rule", async () => {
    vi.mocked(listRecentWorkflowRuns).mockResolvedValue([run("r9", { workflowId: "wf-2", status: "failed" })]);
    const onOpen = vi.fn();
    const wfs = [{ id: "wf-2", name: "Nhắc hạn SLA" }] as WorkflowSummary[];
    render(<RecentRunsStrip workflows={wfs} onOpen={onOpen} />);
    const item = await screen.findByRole("button", { name: /Nhắc hạn SLA/ });
    expect(item).toHaveClass("slds-split-view__list-item-action");
    expect(item.querySelector(".slds-icon-text-error")).not.toBeNull();
    await userEvent.click(item);
    expect(onOpen).toHaveBeenCalledWith("wf-2");
  });
});
