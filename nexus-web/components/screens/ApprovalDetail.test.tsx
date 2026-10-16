// Component test — ApprovalDetail wiring (FE-must-mirror-BE): the inbox deep-link must expose the
// SAME four verbs the record Approvals tab does. The orphan-endpoint gate is method-blind and
// blind to buttons, so a POST verb with no control is invisible to it — this test is the guard
// that /approvals/:id can actually recall and delegate, not just approve/reject.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApprovalDetail } from "@/components/screens/ApprovalDetail";
import * as api from "@/lib/api";
import { toast } from "@/components/ui/Toaster";

const nav = vi.hoisted(() => ({ push: vi.fn(), replace: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => nav }));

// Who is looking (useAuth → /auth/me). Default: u1, the current approver of PENDING, not an admin.
const viewer = vi.hoisted(() => ({ me: { id: "u1", isAdmin: false } as { id: string; isAdmin: boolean; roleId?: string } }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: viewer.me }) }));
vi.mock("@/lib/metadata/hooks", () => ({ useEntities: () => ({ data: [{ name: "crm_quotes", label: "Báo giá" }] }) }));

// Keep ApiError and the pure helpers real; stub only the HTTP-touching functions.
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => {}),
    getApprovalDetail: vi.fn(),
    getRecord: vi.fn(async () => ({ id: "rec-1", data: {} })),
    listAdminUsers: vi.fn(async () => []),
    listRoles: vi.fn(async () => []),
    listUserDirectory: vi.fn(async () => [{ id: "u2", fullName: "Trần B", email: "b@example.com" }]),
    approveRecord: vi.fn(async () => {}),
    rejectRecord: vi.fn(async () => {}),
    overrideApproval: vi.fn(async () => {}),
    recallApproval: vi.fn(async () => {}),
    delegateApproval: vi.fn(async () => {}),
  };
});

beforeEach(() => {
  viewer.me = { id: "u1", isAdmin: false };
});

const PENDING = {
  id: "ai-1",
  processId: "p-1",
  entityName: "crm_quotes",
  recordId: "rec-1",
  currentStep: 0,
  status: "pending",
  currentApproverType: "user",
  currentApproverId: "u1",
  history: [],
  createdBy: "u9",
  createdAt: "2026-08-11T00:00:00Z",
  updatedAt: "2026-08-11T00:00:00Z",
  process: { id: "p-1", name: "Duyệt báo giá", entityName: "crm_quotes", trigger: "manual", isActive: true, steps: [{ approverType: "user", approverId: "u1" }] },
};

describe("ApprovalDetail — recall & delegate wiring", () => {
  beforeEach(() => {
    viewer.me = { id: "u1", isAdmin: false };
    // u1 is the current approver AND sent it — the one viewer the server lets use all four verbs.
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, createdBy: "u1" } as never);
    vi.mocked(api.recallApproval).mockClear();
    vi.mocked(api.delegateApproval).mockClear();
  });

  it("renders all four verbs for a pending instance", async () => {
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Quyết định của bạn")).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /Phê duyệt/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Từ chối/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Thu hồi/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Uỷ quyền/ })).toBeInTheDocument();
  });

  it("Thu hồi → POST recall for this instance", async () => {
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByRole("button", { name: /Thu hồi/ }));
    fireEvent.click(screen.getByRole("button", { name: /Thu hồi/ }));
    await waitFor(() => expect(api.recallApproval).toHaveBeenCalledWith("ai-1", undefined));
  });

  it("Uỷ quyền → pick a user → POST delegate with that user id", async () => {
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByRole("button", { name: /Uỷ quyền/ }));
    await userEvent.click(screen.getByRole("button", { name: /Uỷ quyền/ }));
    // The directory menu (menus/dropdown) lists the named person, not a role.
    const pick = await screen.findByRole("menuitem", { name: "Trần B" });
    await userEvent.click(pick);
    await waitFor(() => expect(api.delegateApproval).toHaveBeenCalledWith("ai-1", "u2", undefined));
  });
});

// A process that is still ALIVE but was edited after arming. The engine walks the pinned steps, so
// the timeline is the pinned chain — and without a note the screen presents that stale chain as if
// it were current, while the admin's Studio shows the new one. Neither screen would say which chain
// the engine actually walks, which is how a mis-set approval chain stays mis-set.
describe("ApprovalDetail — a live-but-edited process says its steps are the pinned copy", () => {
  it("shows the pinned-copy note without claiming the process was deleted", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...PENDING,
      process: { ...PENDING.process, pinned: true },
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText(/bản đã ghim khi gửi duyệt/)).toBeInTheDocument());
    expect(screen.queryByText(/đã bị xoá/)).not.toBeInTheDocument();
    expect(screen.getByText(/vẫn đi hết các bước đã ghim/)).toBeInTheDocument();
    // The note is about the steps above it, so it sits in their card — not loose on the page
    // background, where the light scoped notification's grey fill disappears.
    const steps = screen.getByText("Các bước duyệt").closest("article") as HTMLElement;
    expect(within(steps).getByText(/bản đã ghim khi gửi duyệt/)).toBeInTheDocument();
  });

  it("with no process left at all, the note still sits in a 'Các bước duyệt' card", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, process: null } as never);
    render(<ApprovalDetail id="ai-1" />);
    const note = await screen.findByText(/lịch sử bên trái vẫn được giữ nguyên/);
    const card = note.closest("article") as HTMLElement;
    expect(card).not.toBeNull();
    expect(within(card).getByText("Các bước duyệt")).toBeInTheDocument();
  });
});

// A process soft-deleted AFTER arming: the engine keeps walking the PINNED steps (§9.5), so the
// approver must still see them — timeline, "/N" in the header, and the note that these are the
// pinned copy. Only the live name is gone, so the header falls back to the generic title.
describe("ApprovalDetail — deleted process still shows the pinned steps", () => {
  it("renders the pinned timeline, the 'bản đã ghim' note and the fallback title", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...PENDING,
      process: { ...PENDING.process, name: "", trigger: "", isActive: false, deleted: true },
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText(/bản đã ghim khi gửi duyệt/)).toBeInTheDocument());
    // record-home h1 = object label + title; the title is the generic one
    expect(screen.getByRole("heading", { level: 1, name: /Quy trình phê duyệt$/ })).toBeInTheDocument();
    expect(screen.getByText("Các bước duyệt")).toBeInTheDocument();
    expect(screen.getByText("Bước 1/1")).toBeInTheDocument();
  });
});

// DAILY-QUEUE #69: the pinned step NAME is in the payload, but the step table printed only the
// approver — for a role step seen by a non-admin (no role directory) that is "vai trò #<uuid8>".
// On a deleted process the header has lost the process name too, so the step name is the only
// human label left on the screen. It must lead the row; the approver stays on the sub-line.
describe("ApprovalDetail — the step table names each step (#69)", () => {
  it("shows the pinned step names to a non-admin instead of role UUID fragments", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...PENDING,
      currentStep: 1,
      currentApproverType: "role",
      currentApproverId: "role-dir-0001",
      history: [{ step: 0, action: "approved", actor: "u2", at: "2026-08-11T01:00:00Z" }],
      process: {
        ...PENDING.process, name: "", trigger: "", isActive: false, deleted: true, pinned: true,
        steps: [
          { name: "Trưởng phòng duyệt", approverType: "role", approverId: "role-mgr-0001" },
          { name: "Giám đốc duyệt", approverType: "role", approverId: "role-dir-0001" },
          { approverType: "user", approverId: "u2" }, // unnamed step → falls back to the approver
        ],
      },
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Các bước duyệt")).toBeInTheDocument());
    const steps = within(screen.getByText("Các bước duyệt").closest("article") as HTMLElement);
    expect(steps.getByText("Trưởng phòng duyệt")).toBeInTheDocument();
    expect(steps.getByText("Giám đốc duyệt")).toBeInTheDocument();
    // The unnamed step keeps the approver as its label — nothing invented.
    await waitFor(() => expect(steps.getByText("Trần B")).toBeInTheDocument());
    // The timeline says which step was decided and which one is waiting, by name.
    expect(screen.getByRole("heading", { name: /bước 1 · Trưởng phòng duyệt/ })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Bước 2 · Giám đốc duyệt — đang chờ duyệt/ })).toBeInTheDocument();
  });
});

// specs/043 S1 (#39): the approver sees WHEN the step is due (a time, not only a countdown) and
// what that time is counted in — a wall-clock fallback never reads as working hours.
describe("ApprovalDetail — deadline and its basis", () => {
  it("shows the due time and 'giờ làm việc' for a business-hours deadline", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, slaDueAt: "2026-10-05T04:00:00Z", slaBasis: "business" } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Tính theo")).toBeInTheDocument());
    expect(screen.getByText("giờ làm việc")).toBeInTheDocument();
    expect(screen.getByText(/05\/10\/2026/)).toHaveTextContent("11:00");
  });

  it("says why when the deadline fell back to the wall clock", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, slaDueAt: "2026-10-02T13:00:00Z", slaBasis: "wall_no_calendar" } as never);
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText("giờ đồng hồ — tổ chức chưa khai lịch làm việc")).toBeInTheDocument();
  });

  it("shows no basis row when the step has no deadline", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByText("Quyết định của bạn"));
    expect(screen.queryByText("Tính theo")).not.toBeInTheDocument();
  });
});

// specs/043 S2 (#40): the detail answers "how long did this take" — and names each decided step by
// the name it was recorded under, even when there is no process context left to look it up in.
describe("ApprovalDetail — how long it took", () => {
  it("shows 'Mất 3 giờ làm việc' on a finished approval, and the recorded step name", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...PENDING,
      status: "approved",
      history: [{ step: 0, action: "approved", actor: "u2", node: "s1", stepName: "Trưởng phòng", at: "2026-10-05T03:00:00Z" }],
      durations: { basis: "business", totalBusinessSeconds: 10800, totalWallSeconds: 237600 },
      process: null,
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText("Mất 3 giờ làm việc")).toBeInTheDocument();
    expect(screen.getByText("Thời gian")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Đã duyệt — bước 1 · Trưởng phòng/ })).toBeInTheDocument();
  });

  it("shows no elapsed row when the server sent none", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByText("Quyết định của bạn"));
    expect(screen.queryByText("Thời gian")).not.toBeInTheDocument();
  });
});

// specs/043 S2 (FR-027): the reason is required by the SERVER now. A note the screen accepts but the
// server does not (U+0085 is blank to Go's TrimSpace, not to JS trim) must come back as the server's
// refusal, in words, in the decision box — not a raw code, and the approval stays open.
describe("ApprovalDetail — reject reason refused by the server", () => {
  it("shows APPROVAL_REJECT_REASON_REQUIRED in words at the reason box", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    vi.mocked(api.rejectRecord).mockRejectedValueOnce(
      new api.ApiError(422, "APPROVAL_REJECT_REASON_REQUIRED", "cần ghi lý do khi từ chối"),
    );
    render(<ApprovalDetail id="ai-1" />);
    const box = await screen.findByPlaceholderText(/bắt buộc khi từ chối/);
    fireEvent.change(box, { target: { value: "\u0085" } });
    fireEvent.click(screen.getByRole("button", { name: /Từ chối/ }));
    await waitFor(() => expect(api.rejectRecord).toHaveBeenCalledWith("ai-1", "\u0085", 0));
    const msg = "Cần ghi lý do khi từ chối — người gửi cần biết vì sao hồ sơ bị trả.";
    expect(await screen.findByText(msg)).toBeInTheDocument();
    // At the box = announced with it: the field's description, and the field marked invalid.
    expect(box).toHaveAccessibleDescription(msg);
    expect(box).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("Quyết định của bạn")).toBeInTheDocument(); // still pending, form still there
  });

  // specs/043 S3b (T075): the second tab's Approve loses to the first → 409 APPROVAL_CONFLICT. Said in
  // words above the decision box, and the screen re-reads the instance so it shows the state that won.
  it("a 409 conflict says someone got there first and re-reads the approval", async () => {
    vi.mocked(api.getApprovalDetail).mockClear();
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    vi.mocked(api.approveRecord).mockRejectedValueOnce(
      new api.ApiError(409, "APPROVAL_CONFLICT", "bước phê duyệt vừa thay đổi, vui lòng thử lại"),
    );
    render(<ApprovalDetail id="ai-1" />);
    fireEvent.click(await screen.findByRole("button", { name: /Phê duyệt/ }));
    expect(await screen.findByText(/Phiếu này vừa có người xử lý/)).toBeInTheDocument();
    await waitFor(() => expect(api.getApprovalDetail).toHaveBeenCalledTimes(2)); // first load + the re-read
  });

  // The 409 loser whose phiếu is no longer theirs to read (finished / handed on ⇒ 404 for a
  // non-admin): the page is replaced by the inbox, so the sentence must ride along as a toast —
  // otherwise the message put in decideErr vanishes with the screen and the loser is just moved.
  it("a 409 whose re-read 404s still tells the loser why, before leaving for the inbox", async () => {
    nav.push.mockClear();
    const toastErr = vi.spyOn(toast, "error").mockImplementation(() => {});
    vi.mocked(api.getApprovalDetail)
      .mockResolvedValueOnce(PENDING as never)
      .mockRejectedValueOnce(new api.ApiError(404, "NOT_FOUND", "không có"));
    vi.mocked(api.approveRecord).mockRejectedValueOnce(
      new api.ApiError(409, "APPROVAL_CONFLICT", "bước phê duyệt vừa thay đổi, vui lòng thử lại"),
    );
    render(<ApprovalDetail id="ai-1" />);
    fireEvent.click(await screen.findByRole("button", { name: /Phê duyệt/ }));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith("/approvals"));
    expect(toastErr).toHaveBeenCalledWith(expect.stringMatching(/Phiếu này vừa có người xử lý/));
    toastErr.mockRestore();
  });

  it("a 409 keeps the reason the user typed", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    vi.mocked(api.rejectRecord).mockRejectedValueOnce(
      new api.ApiError(409, "APPROVAL_CONFLICT", "bước phê duyệt vừa thay đổi, vui lòng thử lại"),
    );
    render(<ApprovalDetail id="ai-1" />);
    const box = await screen.findByRole("textbox");
    fireEvent.change(box, { target: { value: "thiếu chứng từ" } });
    fireEvent.click(await screen.findByRole("button", { name: /Từ chối/ }));
    expect(await screen.findByText(/Phiếu này vừa có người xử lý/)).toBeInTheDocument();
    expect(screen.getByRole("textbox")).toHaveValue("thiếu chứng từ");
  });

  // The late second click: the server cannot tell it from a fresh one, so the screen sends the
  // step it RENDERED (BE: Decider.DecideAt).
  it("sends the step it rendered with approve and reject", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, currentStep: 1 } as never);
    vi.mocked(api.rejectRecord).mockClear();
    render(<ApprovalDetail id="ai-1" />);
    const box = await screen.findByRole("textbox");
    fireEvent.change(box, { target: { value: "không đạt" } });
    fireEvent.click(await screen.findByRole("button", { name: /Từ chối/ }));
    await waitFor(() => expect(api.rejectRecord).toHaveBeenCalledWith("ai-1", "không đạt", 1));
  });

  it("a blank reason is refused at the box and nothing is sent", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    vi.mocked(api.rejectRecord).mockClear();
    render(<ApprovalDetail id="ai-1" />);
    const box = await screen.findByPlaceholderText(/bắt buộc khi từ chối/);
    fireEvent.click(screen.getByRole("button", { name: /Từ chối/ }));
    const msg = "Ghi chú là bắt buộc khi từ chối.";
    expect(await screen.findByText(msg)).toBeInTheDocument();
    expect(box).toHaveAccessibleDescription(msg);
    expect(box).toHaveAttribute("aria-invalid", "true");
    expect(api.rejectRecord).not.toHaveBeenCalled();
  });
});

// specs/038 "Nợ còn mở" (found by #64's real click): the decision buttons showed — and could be
// clicked — for someone who is NOT the current step's approver; the server refused with
// APPROVAL_NOT_AUTHORIZED, so the screen promised what it could not do. Mirror the server instead.
describe("ApprovalDetail — only the people the server lets act get the buttons", () => {
  beforeEach(() => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
  });

  it("a colleague who can see the record but is neither approver nor sender gets no decision controls", async () => {
    viewer.me = { id: "u7", isAdmin: false };
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Dòng thời gian")).toBeInTheDocument());
    expect(screen.queryByText("Quyết định của bạn")).not.toBeInTheDocument();
    for (const verb of [/Phê duyệt/, /Từ chối/, /Thu hồi/, /Uỷ quyền/]) {
      expect(screen.queryByRole("button", { name: verb })).not.toBeInTheDocument();
    }
  });

  it("the sender who is not the approver can only recall", async () => {
    viewer.me = { id: "u9", isAdmin: false };
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByRole("button", { name: /Thu hồi/ }));
    expect(screen.queryByRole("button", { name: /Phê duyệt/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Từ chối/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Uỷ quyền/ })).not.toBeInTheDocument();
  });

  it("a role-targeted step is decided by whoever holds the role", async () => {
    viewer.me = { id: "u7", isAdmin: false, roleId: "role-mgr" };
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, currentApproverType: "role", currentApproverId: "role-mgr" } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByRole("button", { name: /Phê duyệt/ }));
    expect(screen.getByRole("button", { name: /Từ chối/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /Uỷ quyền/ })).toBeInTheDocument();
  });

  it("an admin gets every verb (the server's CanDecide lets an admin decide any step)", async () => {
    viewer.me = { id: "adm", isAdmin: true };
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => screen.getByRole("button", { name: /Phê duyệt/ }));
    for (const verb of [/Từ chối/, /Thu hồi/, /Uỷ quyền/]) expect(screen.getByRole("button", { name: verb })).toBeInTheDocument();
  });
});

// specs/038 "Nợ còn mở" (2): the customer face printed the table name ("crm_quotes · bước 1/1",
// "Đối tượng crm_quotes"). The entity's label is what a person reads.
describe("ApprovalDetail — entity label, not the machine name", () => {
  it("shows 'Báo giá' and never 'crm_quotes'", async () => {
    viewer.me = { id: "u1", isAdmin: false };
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    const { container } = render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Báo giá")).toBeInTheDocument());
    expect(container).not.toHaveTextContent("crm_quotes");
  });
});

// Since S0 the engine writes its own "submitted" history entry; the screen's synthetic "Gửi duyệt"
// (from createdAt) only stands in for older instances — never both.
describe("ApprovalDetail — one 'Gửi duyệt' on the timeline", () => {
  it("uses the engine's submitted entry when there is one", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...PENDING,
      history: [{ step: 0, action: "submitted", actor: "u9", at: "2026-08-11T00:00:00Z" }],
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Dòng thời gian")).toBeInTheDocument());
    expect(screen.getAllByRole("heading", { name: /^Gửi duyệt/ })).toHaveLength(1);
  });

  it("falls back to createdAt for an instance armed before the engine logged submissions", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Dòng thời gian")).toBeInTheDocument());
    expect(screen.getAllByRole("heading", { name: /^Gửi duyệt/ })).toHaveLength(1);
  });
});

// A failed read is said in its own words; "không tìm thấy" is for a 404 only. A 500, a dropped
// connection or a 403 used to fall through to the not-found illustration ("không tồn tại… hoặc bạn
// không có quyền xem"), which hid the real reason.
describe("ApprovalDetail — load failures", () => {
  it("a 404 shows 'không tìm thấy' with the way back to the inbox", async () => {
    vi.mocked(api.getApprovalDetail).mockRejectedValueOnce(new api.ApiError(404, "NOT_FOUND", "không có"));
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText("Không tìm thấy yêu cầu phê duyệt")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Về hộp thư duyệt" })).toHaveAttribute("href", "/approvals");
  });

  it("any other failure says what went wrong, not 'không tìm thấy'", async () => {
    vi.mocked(api.getApprovalDetail).mockRejectedValueOnce(new api.ApiError(500, "C8_TEST_DOWN", "mất kết nối tới máy chủ"));
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText("mất kết nối tới máy chủ")).toBeInTheDocument();
    expect(screen.queryByText("Không tìm thấy yêu cầu phê duyệt")).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Về hộp thư duyệt" })).toHaveAttribute("href", "/approvals");
  });

  it("after a decision the request can no longer be read (a non-admin's finished one) ⇒ back to the inbox", async () => {
    nav.push.mockClear();
    vi.mocked(api.getApprovalDetail)
      .mockResolvedValueOnce(PENDING as never)
      .mockRejectedValueOnce(new api.ApiError(404, "NOT_FOUND", "không có"));
    render(<ApprovalDetail id="ai-1" />);
    await userEvent.click(await screen.findByRole("button", { name: /Phê duyệt/ }));
    await waitFor(() => expect(api.approveRecord).toHaveBeenCalledWith("ai-1", undefined, 0));
    await waitFor(() => expect(nav.push).toHaveBeenCalledWith("/approvals"));
  });
});

// A recall stops the request: it is neither approved (green tile) nor rejected (a failed step).
describe("ApprovalDetail — a recalled request reads as stopped", () => {
  const steps = [
    { name: "Trưởng phòng", approverType: "user", approverId: "u2" },
    { name: "Giám đốc", approverType: "user", approverId: "u1" },
  ];
  const ended = (status: string, action: string) => ({
    ...PENDING,
    status,
    currentStep: 1,
    history: [
      { step: 0, action: "approved", actor: "u2", at: "2026-08-11T01:00:00Z" },
      { step: 1, action, actor: "u9", at: "2026-08-11T02:00:00Z" },
    ],
    process: { ...PENDING.process, steps },
  });
  const tileOf = (heading: HTMLElement) => heading.closest("li")?.querySelector(".slds-timeline__icon");

  it("recall and the end of the flow get a neutral tile, and no step is marked failed", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(ended("cancelled", "recalled") as never);
    render(<ApprovalDetail id="ai-1" />);
    const end = await screen.findByRole("heading", { name: /Quy trình kết thúc — đã thu hồi/ });
    const recall = screen.getByRole("heading", { name: /Đã thu hồi — bước 2 · Giám đốc/ });
    const approved = screen.getByRole("heading", { name: /Đã duyệt — bước 1 · Trưởng phòng/ });
    for (const h of [end, recall]) {
      expect(tileOf(h)).toHaveClass("slds-icon-standard-default");
      // not the approval tile's colour: slds-icon-standard-record is the same green as -approval
      expect(tileOf(h)).not.toHaveClass("slds-icon-standard-record");
    }
    expect(tileOf(approved)).toHaveClass("slds-icon-standard-approval");
    const card = screen.getByText("Các bước duyệt").closest("article") as HTMLElement;
    expect(card.querySelector(".slds-has-error")).toBeNull();
  });

  it("a rejection still marks the step it stopped at", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(ended("rejected", "rejected") as never);
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByRole("heading", { name: /Quy trình kết thúc — đã từ chối/ });
    const card = screen.getByText("Các bước duyệt").closest("article") as HTMLElement;
    expect(card.querySelectorAll(".slds-has-error")).toHaveLength(1);
  });
});

// specs/043 S10 — "mỗi hồ sơ ghi rõ chạy phiên bản nào": the detail names the process version the
// approval pinned when it started, and says nothing (never a guessed "1") for one started before
// versions existed.
describe("ApprovalDetail — the process version the approval runs", () => {
  it("shows 'Phiên bản N' from process.version", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...PENDING, process: { ...PENDING.process, version: 2 } } as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Phiên bản 2")).toBeInTheDocument());
  });

  it("shows no version for an approval started before versions existed", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(PENDING as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByText("Duyệt báo giá")).toBeInTheDocument());
    expect(screen.queryByText(/Phiên bản \d/)).not.toBeInTheDocument();
  });
});

// specs/043 S4 — a step held by SEVERAL people. current_approver_* is empty there, so the verbs come
// from the server's awaitsMe (seats), and the screen says where the votes stand and who decided what.
describe("ApprovalDetail — a step held by several people", () => {
  const COUNCIL = {
    ...PENDING,
    currentApproverType: undefined,
    currentApproverId: undefined,
    awaitsMe: true,
    completion: { mode: "all", total: 3, approved: 1, rejected: 0, open: 2, required: 3 },
    seats: [
      { id: "s-a", assigneeType: "user", assigneeId: "u2", status: "done", outcome: "approved", decidedBy: "u2", comment: "đồng ý" },
      { id: "s-b", assigneeType: "user", assigneeId: "u1", status: "open" },
      { id: "s-c", assigneeType: "role", assigneeId: "r-9", status: "open" },
    ],
    process: {
      ...PENDING.process,
      steps: [{ name: "Hội đồng", approvers: [{ type: "user", id: "u2" }, { type: "user", id: "u1" }, { type: "role", id: "r-9" }], completion: "all" }],
    },
  };

  it("says what a quorum step needs: the tally with the required count and the quorum rule (specs/043 S6b)", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...COUNCIL,
      completion: { mode: "quorum", total: 3, approved: 1, rejected: 0, open: 2, required: 2, percent: 60 },
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    const card = (await screen.findByText("Duyệt song song")).closest("article") as HTMLElement;
    expect(within(card).getByText("1/3 đã duyệt — cần 2")).toBeInTheDocument();
    expect(within(card).getByText(/Đủ 60% số người duyệt/)).toBeInTheDocument();
    expect(within(card).queryByText(/Tất cả phải duyệt/)).not.toBeInTheDocument(); // not the `all` caption
  });

  it("names the quorum policy in the steps list too, not «tất cả» (specs/043 S6b)", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...COUNCIL,
      completion: { mode: "quorum", total: 3, approved: 1, rejected: 0, open: 2, required: 2, percent: 60 },
      process: {
        ...COUNCIL.process,
        steps: [{ ...COUNCIL.process.steps[0], completion: "quorum", completionPercent: 60 }],
      },
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText("3 người duyệt · đủ 60%")).toBeInTheDocument();
    expect(screen.queryByText("3 người duyệt · tất cả")).not.toBeInTheDocument();
  });

  it("offers Duyệt / Từ chối to a seat holder although the projection is empty, and shows the tally", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(COUNCIL as never);
    render(<ApprovalDetail id="ai-1" />);
    await waitFor(() => expect(screen.getByRole("button", { name: /Phê duyệt/ })).toBeInTheDocument());
    expect(screen.getByRole("button", { name: /Từ chối/ })).toBeInTheDocument();
    const card = (await screen.findByText("Duyệt song song")).closest("article") as HTMLElement;
    expect(within(card).getByText("1/3 đã duyệt — cần 3")).toBeInTheDocument();
    expect(within(card).getByText(/Tất cả phải duyệt/)).toBeInTheDocument();
    expect(within(card).getByText("Trần B")).toBeInTheDocument(); // u2 resolved from the directory
    expect(within(card).getByText("Đã duyệt")).toBeInTheDocument();
    expect(within(card).getByText(/đồng ý/)).toBeInTheDocument();
    // The steps card names the head-count and the policy instead of a blank approver.
    expect(screen.getByText("3 người duyệt · tất cả")).toBeInTheDocument();
  });

  it("an admin with no seat gets «Quyết thay» (reason mandatory), not Phê duyệt", async () => {
    viewer.me = { id: "admin-1", isAdmin: true };
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...COUNCIL, awaitsMe: false } as never);
    vi.mocked(api.overrideApproval).mockClear();
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByRole("button", { name: /Quyết thay — Duyệt/ });
    expect(screen.queryByRole("button", { name: /^Phê duyệt/ })).not.toBeInTheDocument();
    // No reason → said at the box, nothing sent.
    fireEvent.click(screen.getByRole("button", { name: /Quyết thay — Duyệt/ }));
    expect(await screen.findByText(/Quyết thay cần ghi lý do/)).toBeInTheDocument();
    expect(api.overrideApproval).not.toHaveBeenCalled();
    // With one → the override endpoint, for this approval, with the trimmed reason.
    fireEvent.change(screen.getByPlaceholderText(/Ghi chú/), { target: { value: "  hội đồng họp bất thường " } });
    fireEvent.click(screen.getByRole("button", { name: /Quyết thay — Từ chối/ }));
    await waitFor(() => expect(api.overrideApproval).toHaveBeenCalledWith("ai-1", "reject", "hội đồng họp bất thường", 0));
  });

  it("a step settled by «Quyết thay» (every seat closed, no vote) shows its seats but no «0/0» tally", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...COUNCIL, status: "approved", awaitsMe: false,
      completion: { mode: "all", total: 0, approved: 0, rejected: 0, open: 0, required: 0 },
      seats: COUNCIL.seats.map((s) => ({ ...s, status: "cancelled", outcome: "superseded", comment: undefined })),
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    const card = (await screen.findByText("Duyệt song song")).closest("article") as HTMLElement;
    expect(within(card).queryByText(/0\/0/)).not.toBeInTheDocument();
    expect(within(card).getAllByText("Đã đóng — bước đã có kết quả")).toHaveLength(3);
  });

  it("the timeline says a first vote left the step open, and that an admin settled it in place of the seats", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...COUNCIL, awaitsMe: false,
      history: [
        { step: 0, action: "approved", actor: "u2", partial: true, at: "2026-10-08T01:00:00Z" },
        { step: 0, action: "rejected", actor: "admin-1", via: "override", comment: "hội đồng không họp", at: "2026-10-08T02:00:00Z" },
      ],
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText(/một phiếu — bước chưa xong/)).toBeInTheDocument();
    expect(screen.getByText(/quyết thay \(quản trị viên\)/)).toBeInTheDocument();
  });

  it("an admin who holds a seat votes it like anyone (no override)", async () => {
    viewer.me = { id: "admin-1", isAdmin: true };
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...COUNCIL, awaitsMe: true } as never);
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByRole("button", { name: /Phê duyệt/ });
    expect(screen.queryByRole("button", { name: /Quyết thay/ })).not.toBeInTheDocument();
  });

  it("offers no verb to someone the server says is not awaited", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({ ...COUNCIL, awaitsMe: false } as never);
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByText("Duyệt song song");
    expect(screen.queryByRole("button", { name: /Phê duyệt/ })).not.toBeInTheDocument();
  });
});

// specs/043 S8: the approval was returned to its submitter. Nobody approves or rejects it until
// they press "Gửi lại" — not even the admin — and the screen says why it is waiting.
describe("ApprovalDetail — an approval returned to its submitter (S8)", () => {
  const RETURNED = { ...PENDING, awaitingRework: true, returnReason: "Sai đơn giá", awaitsMe: true };

  it("says it waits for the submitter, with the reason, and offers no approve/reject", async () => {
    viewer.me = { id: "u1", isAdmin: false };
    vi.mocked(api.getApprovalDetail).mockResolvedValue(RETURNED as never);
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText(/đang chờ người gửi sửa lại và gửi lại/)).toBeInTheDocument();
    expect(screen.getByText(/Lý do bị trả về: Sai đơn giá/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Phê duyệt/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Từ chối/ })).not.toBeInTheDocument();
  });

  it("offers an admin no verb either (the engine would answer 422)", async () => {
    viewer.me = { id: "admin-1", isAdmin: true };
    vi.mocked(api.getApprovalDetail).mockResolvedValue(RETURNED as never);
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByText(/đang chờ người gửi sửa lại và gửi lại/);
    expect(screen.queryByRole("button", { name: /Phê duyệt/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Từ chối/ })).not.toBeInTheDocument();
  });

  it("offers an admin no «Quyết thay» on a council step waiting for the submitter (server: 409)", async () => {
    viewer.me = { id: "admin-1", isAdmin: true };
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...RETURNED, awaitsMe: false, currentApproverType: undefined, currentApproverId: undefined,
      completion: { mode: "all", total: 3, approved: 1, rejected: 0, open: 2, required: 3 },
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByText(/đang chờ người gửi sửa lại và gửi lại/);
    expect(screen.queryByRole("button", { name: /Quyết thay/ })).not.toBeInTheDocument();
  });
});

// FE-BPM-DETAIL (#130, specs/043 US3/US6): the path through steps and branches, and the vote tally bar.
describe("ApprovalDetail — path & council progress", () => {
  const GRAPH = {
    ...PENDING,
    currentStep: 2,
    gateway: { gw: "Trên 50 triệu" },
    history: [{ step: 0, action: "approve", actor: "u2", node: "mgr", at: "2026-08-11T01:00:00Z" }],
    process: {
      ...PENDING.process,
      steps: [
        { id: "mgr", kind: "approval", name: "Trưởng phòng", approverType: "user", approverId: "u2", next: "gw" },
        { id: "gw", kind: "gateway", name: "Theo giá trị", branches: [{ name: "Trên 50 triệu", to: "dir" }], default: "fin" },
        { id: "dir", kind: "approval", name: "Giám đốc", approverType: "user", approverId: "u1", next: "end" },
        { id: "fin", kind: "approval", name: "Kế toán", approverType: "user", approverId: "u2", next: "end" },
        { id: "end", kind: "end" },
      ],
    },
  };

  it("names the branch taken and marks the arm not taken", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue(GRAPH as never);
    render(<ApprovalDetail id="ai-1" />);
    expect(await screen.findByText("Đi nhánh «Trên 50 triệu»")).toBeInTheDocument();
    expect(screen.getByText("Giám đốc")).toBeInTheDocument();
    expect(screen.getByText("Kế toán")).toBeInTheDocument();
    expect(screen.getByText("Không đi qua bước này")).toBeInTheDocument();
  });

  it("council card draws a progress bar for 1 of 2 required", async () => {
    vi.mocked(api.getApprovalDetail).mockResolvedValue({
      ...PENDING,
      completion: { mode: "all", total: 3, approved: 1, rejected: 0, open: 2, required: 2 },
      seats: [{ id: "s1", assigneeType: "user", assigneeId: "u2", status: "done", outcome: "approved" }],
    } as never);
    render(<ApprovalDetail id="ai-1" />);
    await screen.findByText("Duyệt song song");
    const bars = screen.getAllByRole("progressbar");
    expect(bars.some((b) => b.getAttribute("aria-valuenow") === "50")).toBe(true);
  });
});
