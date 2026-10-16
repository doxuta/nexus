// ApprovalsTab — the record page's "Duyệt" tab (specs/043 S2, FR-027 / US2 AS1).
// The server refuses a blank reject note (APPROVAL_REJECT_REASON_REQUIRED), but this tab used to
// reject in one click with a hard-coded note ("Từ chối"), so the check was walked around and the
// submitter got a "no" with no reason. The tab has no reason box; "Từ chối" must hand over to
// /approvals/:id, which asks for the reason and shows the server's 422 sentence.
// lib/api is mocked at the module seam (HistoryTab/NotesTab pattern); the tab itself is real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApprovalsTab } from "./LayoutDetail";
import { ensureAuth, listRecordApprovals, listStartableProcesses, listUserDirectory, rejectRecord, submitForApproval } from "@/lib/api";
import type { ApprovalDetailData } from "@/lib/approvalDetail";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listRecordApprovals: vi.fn(),
    listUserDirectory: vi.fn(),
    rejectRecord: vi.fn(),
    listStartableProcesses: vi.fn(),
    submitForApproval: vi.fn(),
  };
});

const PENDING: ApprovalDetailData = {
  id: "ai-1",
  entityName: "crm_quotes",
  recordId: "r1",
  currentStep: 0,
  status: "pending",
  history: [],
};

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listRecordApprovals).mockReset().mockResolvedValue([PENDING]);
  vi.mocked(listUserDirectory).mockReset().mockResolvedValue([]);
  vi.mocked(rejectRecord).mockReset().mockResolvedValue(undefined);
  vi.mocked(listStartableProcesses).mockReset().mockResolvedValue([]);
  vi.mocked(submitForApproval).mockReset().mockResolvedValue(undefined);
});

describe("ApprovalsTab — reject needs a reason (specs/043 FR-027)", () => {
  it("sends 'Từ chối' to the detail page instead of rejecting with a fixed note", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <ApprovalsTab entity="crm_quotes" recordId="r1" />
      </QueryClientProvider>,
    );

    const reject = await screen.findByText("Từ chối");
    expect(reject.closest("a")).toHaveAttribute("href", "/approvals/ai-1");
    fireEvent.click(reject);
    expect(rejectRecord).not.toHaveBeenCalled();
  });
});

// DAILY-QUEUE #251: a record seen only through a manual share is read-only — "Gửi duyệt" would be
// refused by the server, so the empty state offers no button.
describe("ApprovalsTab — readOnly (hồ sơ chỉ thấy nhờ chia sẻ)", () => {
  const renderTab = (readOnly?: boolean) => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <ApprovalsTab entity="crm_quotes" recordId="r1" readOnly={readOnly} />
      </QueryClientProvider>,
    );
  };

  it("chưa gửi duyệt + readOnly: không có nút 'Gửi duyệt'", async () => {
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    renderTab(true);
    expect(await screen.findByText("Chưa gửi duyệt")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Gửi duyệt" })).toBeNull();
  });

  it("chưa gửi duyệt + không readOnly: có nút 'Gửi duyệt' (đối chứng)", async () => {
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    renderTab();
    expect(await screen.findByRole("button", { name: "Gửi duyệt" })).toBeInTheDocument();
  });
});

// FE-BPM-DETAIL T217 (specs/043 US7): the sender picks WHICH manual process a record goes through.
describe("ApprovalsTab — chọn quy trình khi Gửi duyệt", () => {
  const renderTab = () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={qc}>
        <ApprovalsTab entity="crm_quotes" recordId="r1" />
      </QueryClientProvider>,
    );
  };

  it("hai quy trình: chọn quy trình B rồi gửi => processId của B", async () => {
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    vi.mocked(listStartableProcesses).mockResolvedValue([
      { processId: "pA", name: "Duyệt thường" },
      { processId: "pB", name: "Duyệt khẩn" },
    ]);
    renderTab();
    const combo = await screen.findByRole("combobox", { name: /Quy trình duyệt/ });
    fireEvent.click(combo);
    fireEvent.click(await screen.findByRole("option", { name: "Duyệt khẩn" }));
    fireEvent.click(screen.getByRole("button", { name: "Gửi duyệt" }));
    await waitFor(() => expect(submitForApproval).toHaveBeenCalledWith("crm_quotes", "r1", "pB"));
  });

  it("một quy trình: không hỏi, gửi thẳng với processId đó", async () => {
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    vi.mocked(listStartableProcesses).mockResolvedValue([{ processId: "pA", name: "Duyệt thường" }]);
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Gửi duyệt" }));
    expect(screen.queryByRole("combobox")).toBeNull();
    await waitFor(() => expect(submitForApproval).toHaveBeenCalledWith("crm_quotes", "r1", "pA"));
  });

  it("không có quy trình thủ công: gửi không processId, server trả lời thật", async () => {
    vi.mocked(listRecordApprovals).mockResolvedValue([]);
    renderTab();
    fireEvent.click(await screen.findByRole("button", { name: "Gửi duyệt" }));
    await waitFor(() => expect(submitForApproval).toHaveBeenCalledWith("crm_quotes", "r1", undefined));
  });
});
