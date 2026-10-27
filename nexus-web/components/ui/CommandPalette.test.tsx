// CommandPalette — ⌘K-can-act (fec:palette): the assistant's record drafts render as HITL confirm
// cards (NOTHING auto-applies), "Xác nhận & lưu" routes through the NORMAL createRecord/updateRecord
// under the user's own token (a 422 surfaces the honest central message + a retry), and "Phiên
// trước" resumes a persisted thread whose drafts are still actionable. lib/api is mocked at the
// module seam (ApiError kept real); the palette's context hooks (auth/entities/permissions/pins/
// recents/router) are stubbed — everything inside the panel (sendAsk, applyDraft, resume, cards)
// is real code.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { CommandPalette } from "./CommandPalette";
import {
  assistantAsk, createRecord, updateRecord, listAssistantSessions, getAssistantSession, searchAll, ApiError,
} from "@/lib/api";
import type { AssistantAskResult } from "@/lib/api";
import type { AssistantDraft, AssistantSessionDetail } from "@/lib/assistant";

const seam = vi.hoisted(() => ({ admin: false, path: "/home", push: vi.fn() }));
vi.mock("next/navigation", () => ({ usePathname: () => seam.path }));
vi.mock("@/lib/navigation-blocker", () => ({ useGuardedRouter: () => ({ push: seam.push }) }));
vi.mock("@/lib/metadata/hooks", () => ({ useEntities: () => ({ data: [] }) }));
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: () => false }) }));
vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u1", isAdmin: seam.admin }, logout: vi.fn() }),
}));
vi.mock("@/lib/pins", () => ({ usePins: () => ({ pins: [] }) }));
vi.mock("@/lib/recents", () => ({ loadRecentRoutes: () => [] }));
vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, // keeps the REAL ApiError — instanceof in applyDraft must behave as in production
    searchAll: vi.fn(async () => []),
    assistantAsk: vi.fn(),
    createRecord: vi.fn(),
    updateRecord: vi.fn(),
    listAssistantSessions: vi.fn(),
    getAssistantSession: vi.fn(),
    deleteAssistantSession: vi.fn(),
  };
});

const CREATE_DRAFT: AssistantDraft = {
  kind: "record_create",
  recordCreate: { entity: "contacts", data: { name: "Nguyễn Văn A", email: "a@vd.vn" } },
};

function askResult(drafts: AssistantDraft[] | null): AssistantAskResult {
  return {
    sessionId: "as1",
    reply: "Tôi đã soạn bản nháp.",
    usage: { promptTokens: 10, outputTokens: 5, totalTokens: 15 },
    outcome: "ok",
    drafts,
  };
}

function renderPalette() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <CommandPalette open onClose={() => {}} />
    </QueryClientProvider>,
  );
}

// Type a query and select the "Hỏi AI: …" command → the palette swaps to the ask panel and sends.
async function openAskPanel(question = "tạo liên hệ Nguyễn Văn A") {
  const input = screen.getByPlaceholderText("Tìm bản ghi, đối tượng, lệnh…");
  fireEvent.change(input, { target: { value: question } });
  fireEvent.click(await screen.findByText(/Hỏi AI:/));
}

beforeEach(() => {
  seam.admin = false;
  seam.path = "/home";
  seam.push.mockReset();
  vi.mocked(assistantAsk).mockReset();
  vi.mocked(createRecord).mockReset();
  vi.mocked(updateRecord).mockReset();
  vi.mocked(listAssistantSessions).mockReset();
  vi.mocked(getAssistantSession).mockReset();
});

describe("CommandPalette — ⌘K-can-act draft cards", () => {
  it("renders the confirm card and applies via createRecord; success shows Đã lưu + Mở bản ghi", async () => {
    vi.mocked(assistantAsk).mockResolvedValue(askResult([CREATE_DRAFT]));
    vi.mocked(createRecord).mockResolvedValue({ id: "rec-9" });
    renderPalette();
    await openAskPanel();

    // Card renders the title + the proposed field values.
    await screen.findByText("Bản nháp: tạo contacts");
    expect(screen.getByText("Nguyễn Văn A")).toBeInTheDocument();
    expect(screen.getByText("a@vd.vn")).toBeInTheDocument();

    fireEvent.click(screen.getByText("Xác nhận & lưu"));
    await screen.findByText("Đã lưu");
    expect(createRecord).toHaveBeenCalledWith("contacts", { name: "Nguyễn Văn A", email: "a@vd.vn" });
    expect(screen.getByText("Mở bản ghi")).toBeInTheDocument();
  });

  it("update draft applies via updateRecord with ONLY the changed fields", async () => {
    const updateDraft: AssistantDraft = {
      kind: "record_update",
      recordUpdate: {
        entity: "deals", recordId: "rec-77", data: { stage: "won" }, before: { stage: "open" },
      },
    };
    vi.mocked(assistantAsk).mockResolvedValue(askResult([updateDraft]));
    vi.mocked(updateRecord).mockResolvedValue(undefined);
    renderPalette();
    await openAskPanel("chốt deal 77");

    await screen.findByText("Bản nháp: sửa deals");
    expect(screen.getByText("open")).toBeInTheDocument(); // before value in the diff
    fireEvent.click(screen.getByText("Xác nhận & lưu"));
    await screen.findByText("Đã lưu");
    expect(updateRecord).toHaveBeenCalledWith("deals", "rec-77", { stage: "won" });
  });

  it("a 422 shows the honest central message (no field code) on the card and keeps a retry button", async () => {
    vi.mocked(assistantAsk).mockResolvedValue(askResult([CREATE_DRAFT]));
    vi.mocked(createRecord).mockRejectedValue(
      new ApiError(422, "RECORD_REQUIRED_MISSING", "missing", { field: "phone" }),
    );
    renderPalette();
    await openAskPanel();

    fireEvent.click(await screen.findByText("Xác nhận & lưu"));
    await screen.findByText("Thiếu thông tin bắt buộc.");
    expect(screen.getByText("Thử lại")).toBeInTheDocument(); // still confirmable — honest, not dead
    expect(screen.getByText("Bỏ qua")).toBeInTheDocument();
  });

  it("a blocking-rule veto shows only the rule's sentence — no field code", async () => {
    vi.mocked(assistantAsk).mockResolvedValue(askResult([CREATE_DRAFT]));
    vi.mocked(createRecord).mockRejectedValue(
      new ApiError(422, "RECORD_HOOK_VETO", "veto", { field: "amount", violations: [{ field: "amount", message: "Giá trị phải lớn hơn 0." }] }),
    );
    renderPalette();
    await openAskPanel();

    fireEvent.click(await screen.findByText("Xác nhận & lưu"));
    await screen.findByText("Giá trị phải lớn hơn 0.");
    expect(screen.queryByText(/trường:/)).toBeNull();
  });

  it("Bỏ qua mutes the card without calling any mutation", async () => {
    vi.mocked(assistantAsk).mockResolvedValue(askResult([CREATE_DRAFT]));
    renderPalette();
    await openAskPanel();

    fireEvent.click(await screen.findByText("Bỏ qua"));
    await screen.findByText("Đã bỏ qua.");
    expect(createRecord).not.toHaveBeenCalled();
  });

  it("a model refusal shows the declined line, not a blank bubble or the too-broad hint (#30 item 5)", async () => {
    vi.mocked(assistantAsk).mockResolvedValue({ ...askResult(null), reply: "", outcome: "refused" });
    renderPalette();
    await openAskPanel("chép tài khoản của không gian khác");

    await screen.findByText("Mô hình ngôn ngữ từ chối yêu cầu này.");
    expect(screen.queryByText(/chưa trả lời xong/)).toBeNull();
  });

  // #30 item 5 (d): the too-broad hint is what a turn that did not finish says — the tool loop ran out of steps
  // (with or without a partial text), or the model answered nothing — never a blank bubble.
  it.each([
    ["max_steps", "", "max_steps"],
    ["max_steps with a partial text", "Đang tìm các cơ hội", "max_steps"],
    ["an empty ok reply", "  ", "ok"],
  ])("a turn that did not finish (%s) shows the too-broad hint", async (_name, reply, outcome) => {
    vi.mocked(assistantAsk).mockResolvedValue({ ...askResult(null), reply, outcome });
    renderPalette();
    await openAskPanel("mọi cơ hội của mọi người trong năm");

    await screen.findByText(/Tôi chưa trả lời xong câu này — truy vấn quá phức tạp hoặc quá rộng/);
    if (reply.trim()) expect(screen.queryByText(reply)).toBeNull();
  });

  it("Phiên trước: a reopened declined turn says the model declined, not an empty bubble (#30 item 5)", async () => {
    vi.mocked(assistantAsk).mockResolvedValue(askResult(null));
    vi.mocked(listAssistantSessions).mockResolvedValue([
      { id: "as8", title: "Chép tài khoản", updatedAt: new Date().toISOString() },
    ]);
    vi.mocked(getAssistantSession).mockResolvedValue({
      id: "as8", title: "Chép tài khoản", createdAt: "2026-10-27T08:00:00Z", updatedAt: "2026-10-27T08:00:00Z",
      messages: [
        { seq: 0, role: "user", content: "chép tài khoản của không gian khác", drafts: null, createdAt: "2026-10-27T08:00:00Z" },
        { seq: 1, role: "assistant", content: "", drafts: null, createdAt: "2026-10-27T08:00:00Z", outcome: "refused" },
      ],
    });
    renderPalette();
    await openAskPanel("hỏi gì đó");

    fireEvent.click(screen.getByText("Phiên trước"));
    fireEvent.click(await screen.findByText("Chép tài khoản"));
    await screen.findByText("chép tài khoản của không gian khác");
    expect(screen.getByText("Mô hình ngôn ngữ từ chối yêu cầu này.")).toBeInTheDocument();
  });

  it("Phiên trước lists sessions and resuming re-renders a STILL-ACTIONABLE draft card", async () => {
    vi.mocked(assistantAsk).mockResolvedValue(askResult(null));
    vi.mocked(listAssistantSessions).mockResolvedValue([
      { id: "as7", title: "Soạn liên hệ", updatedAt: new Date().toISOString() },
    ]);
    const detail: AssistantSessionDetail = {
      id: "as7", title: "Soạn liên hệ", createdAt: "2026-07-26T08:00:00Z", updatedAt: "2026-07-26T08:05:00Z",
      messages: [
        { seq: 1, role: "user", content: "tạo liên hệ", drafts: null, createdAt: "2026-07-26T08:00:00Z" },
        { seq: 2, role: "assistant", content: "Nháp đây.", drafts: [CREATE_DRAFT], createdAt: "2026-07-26T08:00:05Z" },
      ],
    };
    vi.mocked(getAssistantSession).mockResolvedValue(detail);
    vi.mocked(createRecord).mockResolvedValue({ id: "rec-2" });
    renderPalette();
    await openAskPanel("hỏi gì đó");

    fireEvent.click(screen.getByText("Phiên trước"));
    fireEvent.click(await screen.findByText("Soạn liên hệ"));
    expect(getAssistantSession).toHaveBeenCalledWith("as7");

    // Resumed thread: bubbles + the persisted draft card, still applicable.
    await screen.findByText("Bản nháp: tạo contacts");
    expect(screen.getByText("Nháp đây.")).toBeInTheDocument();
    fireEvent.click(screen.getByText("Xác nhận & lưu"));
    await screen.findByText("Đã lưu");
    expect(createRecord).toHaveBeenCalledWith("contacts", { name: "Nguyễn Văn A", email: "a@vd.vn" });
  });
});

// A failed search and an empty search must not say the same thing. Flattening the catch to [] made
// the palette state confidently "không tìm thấy bản ghi khớp X" for a request that never completed
// — the user concludes the record does not exist and stops looking, which is the most expensive
// possible outcome for a search box.
// specs/073 kh6 (FR-142): inside Studio, ⌘K offers the project's real pages by their route-table label,
// never the old /studio/<panel> routes.
describe("CommandPalette — Studio pages", () => {
  it("lists the mounted pages of the project in the URL and opens one through studioHref", async () => {
    seam.admin = true;
    seam.path = "/studio/du-an/khach-a/dung/du-lieu";
    renderPalette();
    fireEvent.click(await screen.findByText("Sơ đồ liên kết"));
    expect(seam.push).toHaveBeenCalledWith("/studio/du-an/khach-a/dung/du-lieu/so-do");
  });

  it("offers the pack page now that it is built (f2-add-from-pack)", async () => {
    seam.admin = true;
    seam.path = "/studio/du-an/khach-a/dung/du-lieu";
    renderPalette();
    await screen.findByText("Sơ đồ liên kết");
    expect(screen.getByText("Thêm từ gói mẫu")).toBeTruthy();
  });
});

describe("CommandPalette — a failed search says so", () => {
  it("search error → 'thử lại', NOT the no-results sentence", async () => {
    vi.mocked(searchAll).mockRejectedValueOnce(new Error("network down"));
    renderPalette();
    fireEvent.change(screen.getByPlaceholderText("Tìm bản ghi, đối tượng, lệnh…"), {
      target: { value: "hợp đồng" },
    });

    expect(await screen.findByText(/Không tìm được lúc này/)).toBeInTheDocument();
    expect(screen.queryByText(/Không tìm thấy bản ghi khớp/)).toBeNull();
  });

  it("genuinely no results → the no-results sentence, unchanged", async () => {
    vi.mocked(searchAll).mockResolvedValueOnce([]);
    renderPalette();
    fireEvent.change(screen.getByPlaceholderText("Tìm bản ghi, đối tượng, lệnh…"), {
      target: { value: "khongcogi" },
    });

    expect(await screen.findByText(/Không tìm thấy bản ghi khớp/)).toBeInTheDocument();
    expect(screen.queryByText(/Không tìm được lúc này/)).toBeNull();
  });
});
