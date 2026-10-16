// ShareDialog — hợp đồng "Chia sẻ" hồ sơ (specs/051-record-manual-share, DAILY-QUEUE #35):
//   • mở ra là đọc CẢ danh sách đang được chia (GET /:entity/:id/shares) LẪN danh bạ (GET /users/directory);
//   • ô chọn người không mời lại chính mình hay người đã được chia (BE vẫn chặn — 422 — nhưng đừng bày
//     ra một lựa chọn chắc chắn hỏng);
//   • Chia sẻ ⇒ POST {userId} rồi đọc lại danh sách; Gỡ ⇒ DELETE …/shares/:userId rồi đọc lại;
//   • lỗi của server (403 RECORD_SHARED_READ_ONLY, 422 …) hiện ĐÚNG câu server trong hộp thoại.
// lib/api mock ở module seam (OwnerDialog.test.tsx pattern).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { ShareDialog } from "./LayoutDetail";
import {
  ensureAuth, listUserDirectory, listRecordShares, shareRecord, shareRecordWithTeam, unshareRecord, listTeamDirectory, ApiError,
  type DirectoryUser, type RecordShare, type TeamDirectoryEntry,
} from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listUserDirectory: vi.fn(),
    listRecordShares: vi.fn(),
    shareRecord: vi.fn(),
    shareRecordWithTeam: vi.fn(),
    listTeamDirectory: vi.fn(),
    unshareRecord: vi.fn(),
  };
});

const USERS: DirectoryUser[] = [
  { id: "me", email: "test@example.com", fullName: "Người Thử" },
  { id: "u1", email: "an@example.com", fullName: "An Nguyễn" },
  { id: "u2", email: "binh@example.com", fullName: "Bình Trần" },
];
const SHARE_BINH: RecordShare = {
  userId: "u2", userName: "Bình Trần", userEmail: "binh@example.com", sharedBy: "me", createdAt: "2026-09-30T02:00:00Z",
};

const TEAMS: TeamDirectoryEntry[] = [
  { id: "t1", name: "Phòng Kinh doanh" },
  { id: "t2", name: "Phòng Hỗ trợ" },
];
const SHARE_TEAM_T2: RecordShare = {
  teamId: "t2", teamName: "Phòng Hỗ trợ", sharedBy: "me", createdAt: "2026-10-09T02:00:00Z",
};

function renderDialog(onOpenChange = vi.fn()) {
  render(
    <ShareDialog entity="crm_opportunities" recordId="r1" currentUserId="me" open onOpenChange={onOpenChange} />,
  );
  return { onOpenChange };
}

async function openPeople() {
  const box = await screen.findByRole("combobox", { name: "Chia sẻ với" });
  fireEvent.click(box);
  return box;
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listUserDirectory).mockReset().mockResolvedValue(USERS);
  vi.mocked(listRecordShares).mockReset().mockResolvedValue([SHARE_BINH]);
  vi.mocked(shareRecord).mockReset();
  vi.mocked(shareRecordWithTeam).mockReset();
  vi.mocked(listTeamDirectory).mockReset().mockResolvedValue(TEAMS);
  vi.mocked(unshareRecord).mockReset();
});

describe("ShareDialog", () => {
  it("liệt kê người đang được chia (tên, email, ai chia) và chỉ mời được người chưa được chia, không phải mình", async () => {
    renderDialog();
    const list = await screen.findByRole("list", { name: "Đang được chia sẻ" });
    expect(within(list).getByText("Bình Trần")).toBeInTheDocument();
    expect(within(list).getByText("binh@example.com")).toBeInTheDocument();
    expect(within(list).getByText(/Chia bởi Người Thử/)).toBeInTheDocument();
    expect(listRecordShares).toHaveBeenCalledWith("crm_opportunities", "r1");

    await openPeople();
    expect(screen.getByRole("option", { name: /An Nguyễn/ })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /Bình Trần/ })).toBeNull(); // already shared
    expect(screen.queryByRole("option", { name: /Người Thử/ })).toBeNull(); // yourself
  });

  it("Chia sẻ ⇒ POST đúng người rồi đọc lại danh sách", async () => {
    vi.mocked(shareRecord).mockResolvedValue({ ...SHARE_BINH, userId: "u1", userName: "An Nguyễn", userEmail: "an@example.com" });
    renderDialog();
    await screen.findByRole("list", { name: "Đang được chia sẻ" });
    const submit = screen.getByRole("button", { name: "Chia sẻ" });
    expect(submit).toBeDisabled(); // nobody picked yet

    vi.mocked(listRecordShares).mockResolvedValue([
      SHARE_BINH,
      { userId: "u1", userName: "An Nguyễn", userEmail: "an@example.com", sharedBy: "me", createdAt: "2026-09-30T03:00:00Z" },
    ]);
    await openPeople();
    fireEvent.click(screen.getByRole("option", { name: /An Nguyễn/ }));
    fireEvent.click(submit);

    await waitFor(() => expect(shareRecord).toHaveBeenCalledWith("crm_opportunities", "r1", "u1"));
    const list = screen.getByRole("list", { name: "Đang được chia sẻ" });
    expect(await within(list).findByText("An Nguyễn")).toBeInTheDocument();
    expect(listRecordShares).toHaveBeenCalledTimes(2);
  });

  it("Gỡ ⇒ DELETE đúng người rồi đọc lại danh sách", async () => {
    vi.mocked(unshareRecord).mockResolvedValue(undefined);
    renderDialog();
    const remove = await screen.findByRole("button", { name: "Gỡ chia sẻ với Bình Trần" });
    vi.mocked(listRecordShares).mockResolvedValue([]);
    fireEvent.click(remove);

    await waitFor(() => expect(unshareRecord).toHaveBeenCalledWith("crm_opportunities", "r1", "u2"));
    expect(await screen.findByText("Chưa chia sẻ cho ai.")).toBeInTheDocument();
  });

  it("server từ chối (403 RECORD_SHARED_READ_ONLY) ⇒ hiện đúng câu server, không danh sách giả", async () => {
    vi.mocked(listRecordShares).mockRejectedValue(
      new ApiError(403, "RECORD_SHARED_READ_ONLY", "Hồ sơ này được chia sẻ để xem, không sửa được"),
    );
    renderDialog();
    expect(await screen.findByText("Hồ sơ này được chia sẻ để xem, không sửa được")).toBeInTheDocument();
    expect(screen.queryByText("Chưa chia sẻ cho ai.")).toBeNull();
  });

  it("422 khi chia ⇒ câu server trong hộp thoại, hộp thoại không đóng", async () => {
    vi.mocked(shareRecord).mockRejectedValue(
      new ApiError(422, "RECORD_SHARE_USER_INVALID", "Người nhận phải là người dùng đang hoạt động trong tổ chức"),
    );
    const { onOpenChange } = renderDialog();
    await openPeople();
    fireEvent.click(screen.getByRole("option", { name: /An Nguyễn/ }));
    fireEvent.click(screen.getByRole("button", { name: "Chia sẻ" }));

    expect(await screen.findByText("Người nhận phải là người dùng đang hoạt động trong tổ chức")).toBeInTheDocument();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });
});

describe("ShareDialog — tìm phía server (#252)", () => {
  it("gõ tên người ngoài trang đầu ⇒ hỏi server ?q= và chia sẻ được cho người đó", async () => {
    const FAR: DirectoryUser = { id: "u300", email: "xa@example.com", fullName: "Xa Lắm" };
    vi.mocked(listUserDirectory).mockImplementation(async (q?: string) => (q ? [FAR] : USERS));
    vi.mocked(shareRecord).mockResolvedValue({ ...SHARE_BINH, userId: "u300" });
    renderDialog();
    await screen.findByRole("list", { name: "Đang được chia sẻ" });
    const box = await openPeople();
    fireEvent.change(box, { target: { value: "xa lam" } });
    const opt = await screen.findByRole("option", { name: /Xa Lắm/ });
    expect(vi.mocked(listUserDirectory)).toHaveBeenCalledWith("xa lam", expect.any(Number));
    fireEvent.click(opt);
    fireEvent.click(screen.getByRole("button", { name: "Chia sẻ" }));
    await waitFor(() => expect(vi.mocked(shareRecord)).toHaveBeenCalledWith("crm_opportunities", "r1", "u300"));
  });
});

// Chia cho cả NHÓM (DAILY-QUEUE #224): ô chọn nhóm riêng, danh sách có hàng "nhóm", gỡ theo id nhóm.
describe("ShareDialog — chia cho nhóm (#224)", () => {
  async function openTeams() {
    const box = await screen.findByRole("combobox", { name: "Chia sẻ với nhóm" });
    fireEvent.click(box);
    return box;
  }

  it("hàng nhóm hiện tên nhóm + \"Mọi thành viên\"; gỡ ⇒ DELETE theo id nhóm", async () => {
    vi.mocked(listRecordShares).mockResolvedValue([SHARE_BINH, SHARE_TEAM_T2]);
    vi.mocked(unshareRecord).mockResolvedValue(undefined);
    renderDialog();
    const list = await screen.findByRole("list", { name: "Đang được chia sẻ" });
    expect(within(list).getByText("Phòng Hỗ trợ")).toBeInTheDocument();
    expect(within(list).getByText(/Mọi thành viên của nhóm/)).toBeInTheDocument();
    vi.mocked(listRecordShares).mockResolvedValue([SHARE_BINH]);
    fireEvent.click(screen.getByRole("button", { name: "Gỡ chia sẻ với Phòng Hỗ trợ" }));
    await waitFor(() => expect(unshareRecord).toHaveBeenCalledWith("crm_opportunities", "r1", "t2"));
  });

  it("chọn nhóm + Chia sẻ cho nhóm ⇒ POST teamId rồi đọc lại; nhóm đã được chia không còn trong ô chọn", async () => {
    vi.mocked(listRecordShares).mockResolvedValue([SHARE_TEAM_T2]);
    vi.mocked(shareRecordWithTeam).mockResolvedValue({ ...SHARE_TEAM_T2, teamId: "t1", teamName: "Phòng Kinh doanh" });
    renderDialog();
    await screen.findByRole("list", { name: "Đang được chia sẻ" });
    const submit = screen.getByRole("button", { name: "Chia sẻ cho nhóm" });
    expect(submit).toBeDisabled();
    await openTeams();
    expect(screen.queryByRole("option", { name: /Phòng Hỗ trợ/ })).toBeNull(); // already shared
    vi.mocked(listRecordShares).mockResolvedValue([SHARE_TEAM_T2, { ...SHARE_TEAM_T2, teamId: "t1", teamName: "Phòng Kinh doanh" }]);
    fireEvent.click(screen.getByRole("option", { name: /Phòng Kinh doanh/ }));
    fireEvent.click(submit);
    await waitFor(() => expect(shareRecordWithTeam).toHaveBeenCalledWith("crm_opportunities", "r1", "t1"));
    expect(shareRecord).not.toHaveBeenCalled();
    const list = screen.getByRole("list", { name: "Đang được chia sẻ" });
    expect(await within(list).findByText("Phòng Kinh doanh")).toBeInTheDocument();
  });

  it("danh bạ nhóm lỗi ⇒ báo cạnh ô nhóm, phần chia cho người vẫn dùng được", async () => {
    vi.mocked(listTeamDirectory).mockRejectedValue(new ApiError(500, "TEAM_DIRECTORY_FAILED", "Không thể liệt kê danh sách nhóm"));
    renderDialog();
    expect(await screen.findByText("Không thể liệt kê danh sách nhóm")).toBeInTheDocument();
    await openPeople();
    expect(screen.getByRole("option", { name: /An Nguyễn/ })).toBeInTheDocument();
  });
});
