// NotesTab — hợp đồng tab "Trao đổi" (đoạn-nối slice, docs/NEXUS-MOT-XUONG-2026-07-29.md §3d
// mục 1): 4 trạng thái (loading / empty-có-CTA / error / data), danh sách mới-nhất-trước với tên
// người viết + thời gian tương đối, và tư thế XSS: body render là PLAIN TEXT (React escape mặc
// định) — chuỗi HTML trong ghi chú không bao giờ thành element. lib/api mock ở module seam
// (RelatedTab.test.tsx pattern); UI + state machine là code thật.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { NotesTab } from "./LayoutDetail";
import {
  ensureAuth, listRecordNotes, createRecordNote, deleteRecordNote, ApiError,
  type RecordNote, type RecordNotesPage,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual, // keeps ApiError etc. real
    ensureAuth: vi.fn(),
    listRecordNotes: vi.fn(),
    createRecordNote: vi.fn(),
    deleteRecordNote: vi.fn(),
  };
});

const NOTE: RecordNote = {
  id: "n1",
  body: "Đã gọi khách, hẹn thứ 5",
  createdBy: "user-1234-5678",
  createdByName: "Trần B",
  createdAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(),
};

function page(notes: RecordNote[], total = notes.length): RecordNotesPage {
  return { notes, pagination: { page: 1, limit: 20, total, totalPages: 1 } };
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listRecordNotes).mockReset();
  vi.mocked(createRecordNote).mockReset();
  vi.mocked(deleteRecordNote).mockReset();
});

describe("NotesTab — 4 trạng thái", () => {
  it("loading: skeleton aria-busy khi danh sách còn in-flight", () => {
    vi.mocked(listRecordNotes).mockReturnValue(new Promise(() => {})); // never resolves
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    expect(screen.getByLabelText("Đang tải trao đổi")).toBeInTheDocument();
  });

  it("rỗng: hiện CTA 'Viết ghi chú đầu tiên'", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([]));
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    expect(await screen.findByText("Chưa có trao đổi nào")).toBeInTheDocument();
    expect(screen.getByText("Viết ghi chú đầu tiên")).toBeInTheDocument();
  });

  it("lỗi: hiện đúng thông báo lỗi thật từ server", async () => {
    vi.mocked(listRecordNotes).mockRejectedValue(
      new ApiError(404, "RECORD_NOT_FOUND", "Bản ghi không tồn tại"),
    );
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    expect(await screen.findByText("Không tải được trao đổi")).toBeInTheDocument();
    expect(screen.getByText("Bản ghi không tồn tại")).toBeInTheDocument();
  });

  it("có dữ liệu: tên người viết + body PLAIN TEXT (HTML trơ), fallback id khi thiếu tên", async () => {
    const xss: RecordNote = {
      ...NOTE, id: "n2", createdByName: undefined,
      body: "<img src=x onerror=alert(1)>",
    };
    vi.mocked(listRecordNotes).mockResolvedValue(page([NOTE, xss]));
    const { container } = render(<NotesTab entity="crm_accounts" recordId="r1" />);

    expect(await screen.findByText("Đã gọi khách, hẹn thứ 5")).toBeInTheDocument();
    expect(screen.getByText("Trần B")).toBeInTheDocument();
    // createdByName vắng → raw short id, không bao giờ bịa tên.
    expect(screen.getByText("user-123")).toBeInTheDocument();
    // Tư thế XSS: markup là TEXT; không <img> nào được tạo ra trong DOM.
    expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
    expect(container.querySelector("img")).toBeNull();
  });
});

describe("NotesTab — gửi & xoá", () => {
  it("gửi: prepend ghi chú mới (mới nhất trước), gọi đúng contract", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([]));
    vi.mocked(createRecordNote).mockResolvedValue({ ...NOTE, id: "n3", body: "Ghi chú mới" });
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    await screen.findByText("Chưa có trao đổi nào");

    fireEvent.change(screen.getByLabelText("Soạn ghi chú"), { target: { value: "Ghi chú mới" } });
    fireEvent.click(screen.getByText("Gửi"));

    expect(await screen.findByText("Ghi chú mới")).toBeInTheDocument();
    expect(vi.mocked(createRecordNote)).toHaveBeenCalledWith("crm_accounts", "r1", "Ghi chú mới");
  });

  it("xoá 403: hiện đúng thông báo server, nút không bị ẩn theo phỏng đoán", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([NOTE]));
    vi.mocked(deleteRecordNote).mockRejectedValue(
      new ApiError(403, "FORBIDDEN", "chỉ tác giả hoặc người có quyền xoá mới xoá được ghi chú"),
    );
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    await screen.findByText("Đã gọi khách, hẹn thứ 5");

    // Nút xoá LUÔN render (không đoán quyền ở FE) — two-click confirm rồi mới gọi API.
    fireEvent.click(screen.getByTitle("Xoá ghi chú"));
    fireEvent.click(screen.getByText("Xoá?"));

    expect(
      await screen.findByText("chỉ tác giả hoặc người có quyền xoá mới xoá được ghi chú"),
    ).toBeInTheDocument();
    // Ghi chú vẫn còn — không optimistic-remove khi server từ chối.
    expect(screen.getByText("Đã gọi khách, hẹn thứ 5")).toBeInTheDocument();
  });

  // Keyboard: the two presses land on the SAME button — swapping elements between the presses
  // would drop focus on <body> and the second Enter would delete nothing.
  it("xoá bằng bàn phím: Enter hai lần trên cùng một nút", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([NOTE]));
    vi.mocked(deleteRecordNote).mockResolvedValue(undefined);
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    await screen.findByText("Đã gọi khách, hẹn thứ 5");

    const del = screen.getByRole("button", { name: "Xoá ghi chú" });
    del.focus();
    await userEvent.keyboard("{Enter}");
    expect(screen.getByText("Xoá?")).toBeInTheDocument();
    expect(deleteRecordNote).not.toHaveBeenCalled();
    expect(screen.getByText("Xoá?").closest("button")).toHaveFocus();

    await userEvent.keyboard("{Enter}");
    await waitFor(() => expect(deleteRecordNote).toHaveBeenCalledWith("crm_accounts", "r1", "n1"));
    await waitFor(() => expect(screen.queryByText("Đã gọi khách, hẹn thứ 5")).toBeNull());
  });

  it("xoá bị từ chối: focus vẫn ở nút xoá của ghi chú đó", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([NOTE]));
    vi.mocked(deleteRecordNote).mockRejectedValue(new ApiError(403, "FORBIDDEN", "không được xoá"));
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    await screen.findByText("Đã gọi khách, hẹn thứ 5");

    screen.getByRole("button", { name: "Xoá ghi chú" }).focus();
    await userEvent.keyboard("{Enter}{Enter}");
    expect(await screen.findByText("không được xoá")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Xoá ghi chú" })).toHaveFocus();
  });
});

// DAILY-QUEUE #251: a record seen only through a manual share is read-only (specs/051). The server
// answers every write there with 403/404, so the tab offers no composer, no delete and no
// "write the first" CTA — the feed itself stays readable.
describe("NotesTab — readOnly (hồ sơ chỉ thấy nhờ chia sẻ)", () => {
  it("không có ô soạn, không nút Gửi, không nút xoá; ghi chú vẫn đọc được", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([NOTE]));
    render(<NotesTab entity="crm_accounts" recordId="r1" readOnly />);
    expect(await screen.findByText("Đã gọi khách, hẹn thứ 5")).toBeInTheDocument();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Gửi" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Xoá ghi chú" })).toBeNull();
  });

  it("rỗng: nói chưa có ghi chú, không có CTA 'Viết ghi chú đầu tiên'", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([]));
    render(<NotesTab entity="crm_accounts" recordId="r1" readOnly />);
    expect(await screen.findByText("Chưa có trao đổi nào")).toBeInTheDocument();
    expect(screen.queryByText("Viết ghi chú đầu tiên")).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("không readOnly: ô soạn + nút xoá vẫn có (đối chứng)", async () => {
    vi.mocked(listRecordNotes).mockResolvedValue(page([NOTE]));
    render(<NotesTab entity="crm_accounts" recordId="r1" />);
    expect(await screen.findByRole("textbox")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Xoá ghi chú" })).toBeInTheDocument();
  });
});
