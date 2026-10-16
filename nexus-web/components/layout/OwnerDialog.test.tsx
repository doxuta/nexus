// OwnerDialog — hợp đồng "Đổi chủ" (TRỤ 2 mục 2): KHÔNG đoán quyền ở FE — PUT /:entity/:id/owner
// trả 403 thì hiện ĐÚNG thông báo server trong dialog (nút không bị ẩn trước); user không active
// bị lọc vì BE chắc chắn trả 422 RECORD_OWNER_INVALID cho họ. lib/api mock ở module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { OwnerDialog } from "./LayoutDetail";
import { ensureAuth, listUserDirectory, setRecordOwner, ApiError, type DirectoryUser } from "@/lib/api";

vi.mock("@/lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/api")>();
  return {
    ...actual,
    ensureAuth: vi.fn(),
    listUserDirectory: vi.fn(),
    setRecordOwner: vi.fn(),
  };
});

// Danh bạ (GET /users/directory) — BE đã lọc sẵn user còn hoạt động và cố ý CHỈ trả 3 trường:
// không status, không isAdmin, không roleId. Fixture phải khớp shape thật đó, không phải AdminUser.
const USERS: DirectoryUser[] = [
  { id: "u1", email: "an@example.com", fullName: "An Nguyễn" },
  { id: "u2", email: "binh@example.com", fullName: "Bình Trần" },
];

// The people picker is the lookup blueprint (a combobox): the list opens from the search field.
async function openPeople() {
  const box = await screen.findByRole("combobox", { name: "Tìm người dùng" });
  fireEvent.click(box);
  return box;
}

beforeEach(() => {
  vi.mocked(ensureAuth).mockReset().mockResolvedValue(undefined);
  vi.mocked(listUserDirectory).mockReset().mockResolvedValue(USERS);
  vi.mocked(setRecordOwner).mockReset();
});

describe("OwnerDialog", () => {
  it("danh bạ dùng /users/directory (không phải /admin/users); 403 → hiện đúng thông báo server, dialog không đóng", async () => {
    vi.mocked(setRecordOwner).mockRejectedValue(new ApiError(
      403, "FORBIDDEN",
      "chỉ quản lý (quyền sửa ở mức toàn bộ) hoặc quản trị viên mới được đổi chủ bản ghi",
    ));
    const onOpenChange = vi.fn();
    render(
      <OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={onOpenChange} />,
    );

    await openPeople();
    expect(screen.getByRole("option", { name: /An Nguyễn/ })).toBeInTheDocument();
    // Lọc user-còn-hoạt-động nay thuộc BACKEND (/users/directory chỉ trả user active), không phải
    // lọc lại ở FE. Khẳng định đúng chỗ trách nhiệm: FE gọi ĐÚNG danh bạ và render nguyên những gì
    // BE trả — lọc hai lần là chỗ hai bên dễ trôi khỏi nhau.
    expect(vi.mocked(listUserDirectory)).toHaveBeenCalled();
    expect(screen.getByRole("option", { name: /Bình Trần/ })).toBeInTheDocument();

    fireEvent.click(screen.getByRole("option", { name: /An Nguyễn/ }));
    fireEvent.click(screen.getByRole("button", { name: "Đổi chủ" }));

    await waitFor(() =>
      expect(vi.mocked(setRecordOwner)).toHaveBeenCalledWith("crm_accounts", "r1", "u1"),
    );
    // Thông báo THẬT từ server hiện trong dialog — không generic, không ẩn nút.
    expect(
      await screen.findByText(/chỉ quản lý .* mới được đổi chủ bản ghi/),
    ).toBeInTheDocument();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
  });

  it("422 RECORD_OWNER_INVALID → hiện đúng thông báo server", async () => {
    vi.mocked(setRecordOwner).mockRejectedValue(new ApiError(
      422, "RECORD_OWNER_INVALID",
      "userId phải là id của một người dùng đang hoạt động trong tổ chức",
    ));
    render(<OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={() => {}} />);

    await openPeople();
    fireEvent.click(screen.getByRole("option", { name: /An Nguyễn/ }));
    fireEvent.click(screen.getByRole("button", { name: "Đổi chủ" }));

    expect(
      await screen.findByText("userId phải là id của một người dùng đang hoạt động trong tổ chức"),
    ).toBeInTheDocument();
  });

  it("thành công → gọi onChanged và đóng dialog", async () => {
    vi.mocked(setRecordOwner).mockResolvedValue(undefined);
    const onChanged = vi.fn();
    const onOpenChange = vi.fn();
    render(
      <OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={onOpenChange} onChanged={onChanged} />,
    );

    await openPeople();
    fireEvent.click(screen.getByRole("option", { name: /An Nguyễn/ }));
    fireEvent.click(screen.getByRole("button", { name: "Đổi chủ" }));

    await waitFor(() => expect(onChanged).toHaveBeenCalled());
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  // Lookup blueprint keyboard: focus stays in the search field; ArrowDown walks the options via
  // aria-activedescendant, Enter picks. Options are not tab stops — Tab leaves for the footer.
  it("bàn phím: ArrowDown chọn dần, Enter chốt; Tab rời ô tìm tới nút Huỷ", async () => {
    vi.mocked(setRecordOwner).mockResolvedValue(undefined);
    const onOpenChange = vi.fn();
    render(<OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={onOpenChange} />);
    const box = await screen.findByRole("combobox", { name: "Tìm người dùng" });
    box.focus();

    await userEvent.keyboard("{ArrowDown}");
    const listbox = screen.getByRole("listbox");
    expect(box).toHaveAttribute("aria-controls", listbox.id);
    const first = screen.getByRole("option", { name: /An Nguyễn/ });
    expect(box).toHaveAttribute("aria-activedescendant", first.id);

    await userEvent.keyboard("{ArrowDown}");
    const second = screen.getByRole("option", { name: /Bình Trần/ });
    expect(box).toHaveAttribute("aria-activedescendant", second.id);
    for (const o of screen.getAllByRole("option")) expect(o).not.toHaveAttribute("tabindex", "0");

    await userEvent.keyboard("{Enter}");
    expect(screen.queryByRole("listbox")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Đổi chủ" }));
    await waitFor(() => expect(vi.mocked(setRecordOwner)).toHaveBeenCalledWith("crm_accounts", "r1", "u2"));
  });

  it("Tab từ ô tìm đi thẳng tới footer (danh sách không phải chuỗi tab stop)", async () => {
    render(<OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={() => {}} />);
    const box = await screen.findByRole("combobox", { name: "Tìm người dùng" });
    box.focus();
    await userEvent.keyboard("{ArrowDown}");
    await userEvent.tab();
    expect(screen.getByRole("button", { name: "Huỷ" })).toHaveFocus();
  });

  it("tìm theo email vẫn lọc được", async () => {
    render(<OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={() => {}} />);
    const box = await screen.findByRole("combobox", { name: "Tìm người dùng" });
    fireEvent.change(box, { target: { value: "binh@" } });
    expect(screen.getByRole("option", { name: /Bình Trần/ })).toBeInTheDocument();
    expect(screen.queryByRole("option", { name: /An Nguyễn/ })).toBeNull();
  });
});

describe("OwnerDialog — tìm phía server (#252)", () => {
  it("gõ tên người ngoài trang đầu ⇒ hỏi server ?q= và chọn được người đó", async () => {
    const FAR: DirectoryUser = { id: "u300", email: "xa@example.com", fullName: "Xa Lắm" };
    vi.mocked(listUserDirectory).mockImplementation(async (q?: string) => (q ? [FAR] : USERS));
    vi.mocked(setRecordOwner).mockResolvedValue(undefined as never);
    render(<OwnerDialog entity="crm_accounts" recordId="r1" open onOpenChange={vi.fn()} />);

    const box = await openPeople();
    fireEvent.change(box, { target: { value: "xa lam" } });
    const opt = await screen.findByRole("option", { name: /Xa Lắm/ });
    expect(vi.mocked(listUserDirectory)).toHaveBeenCalledWith("xa lam", expect.any(Number));
    fireEvent.click(opt);
    fireEvent.click(screen.getByRole("button", { name: "Đổi chủ" }));
    await waitFor(() => expect(vi.mocked(setRecordOwner)).toHaveBeenCalledWith("crm_accounts", "r1", "u300"));
  });
});
