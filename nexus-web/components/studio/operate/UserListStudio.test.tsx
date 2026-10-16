// specs/078 lát u7-nguoi-dung-danh-sach: the Người dùng page of the Studio work surface (FR-020..FR-025).
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, createResetLink, createUser, deleteUser, endUserSession, getUser, listRoles, listUserSessions, listUsers, setUserManager, updateUser, type AdminUser } from "@/lib/admin";
import { UserListStudio } from "./UserListStudio";

vi.mock("@/lib/admin", async (orig) => ({
  ...(await orig<typeof import("@/lib/admin")>()),
  listUsers: vi.fn(),
  listRoles: vi.fn(),
  createUser: vi.fn(),
  updateUser: vi.fn(),
  createResetLink: vi.fn(),
  getUser: vi.fn(),
  deleteUser: vi.fn(),
  listUserSessions: vi.fn(),
  endUserSession: vi.fn(),
  setUserManager: vi.fn(),
}));
const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));
vi.mock("@/lib/studioContext", () => ({ useStudioIdentity: () => ({ matches: () => true, me: { id: "u-me" } }) }));
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const u = (id: string, name: string, extra: Partial<AdminUser> = {}): AdminUser =>
  ({ id, tenantId: "t", email: `${id}@x.vn`, fullName: name, status: "active", isAdmin: false, ...extra });
const ROLES = [{ id: "r1", name: "sales", label: "Nhân viên kinh doanh" }];
const THREE = [u("u-me", "Tôi Dựng", { isAdmin: true }), u("u-an", "An Nguyễn", { roleId: "r1" }), u("u-bo", "Bo Lê", { status: "inactive" })];

let qc: QueryClient;
function mount(users: AdminUser[] = THREE, person?: string) {
  vi.mocked(listUsers).mockResolvedValue(users);
  vi.mocked(listRoles).mockResolvedValue(ROLES as never);
  qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <UserListStudio slug="northwind" person={person} />
    </QueryClientProvider>,
  );
}
const rowOf = (name: string) => screen.getByText(name).closest("tr") as HTMLElement;
const err = (status: number, code: string, msg = "x") => new ApiError(status, code, msg);

beforeEach(() => vi.clearAllMocks());

describe("states (FR-011, FR-018)", () => {
  it("loading, then the lede counts the loaded list", async () => {
    mount();
    expect(screen.getByRole("status")).toBeInTheDocument();
    const lede = await screen.findByText(/3 người/);
    expect(lede.closest("p")).toHaveTextContent("3 người, 2 đang hoạt động, 1 đã khoá.");
    expect(lede.tagName).toBe("B");
  });
  it("error: the sentence and Thử lại", async () => {
    vi.mocked(listUsers).mockRejectedValue(new Error("boom"));
    vi.mocked(listRoles).mockResolvedValue([] as never);
    qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={qc}><UserListStudio slug="northwind" /></QueryClientProvider>);
    expect(await screen.findByText("Không đọc được danh sách người dùng.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Thử lại" })).toBeInTheDocument();
  });
  it("empty: the sentence and the Người dùng mới button", async () => {
    mount([]);
    expect(await screen.findByText("Chưa có nhân viên nào. Thêm người đầu tiên để họ đăng nhập app.")).toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "Người dùng mới" }).length).toBeGreaterThan(0);
  });
});

describe("table (FR-021)", () => {
  it("shows name + email, role label, status text; no avatar, badge or last-seen column; caption", async () => {
    const { container } = mount();
    await screen.findByText("An Nguyễn");
    expect(within(rowOf("An Nguyễn")).getByText("Nhân viên kinh doanh")).toBeInTheDocument();
    expect(within(rowOf("An Nguyễn")).getByText("Đang hoạt động")).toBeInTheDocument();
    expect(within(rowOf("Bo Lê")).getByText("Đã khoá")).toBeInTheDocument();
    expect(container.querySelector(".slds-avatar, .slds-badge, .slds-pill")).toBeNull();
    expect(screen.queryByText("Lần cuối")).toBeNull();
    expect(screen.getByRole("grid", { name: "Danh sách người dùng của dự án" })).toBeInTheDocument();
  });
  it("tier column only when the server sent canManageUsers; ordinary employee cell stays empty", async () => {
    mount([u("u-me", "Tôi", { isAdmin: true, canManageUsers: false }), u("o", "Chủ", { canManageUsers: true }), u("s", "Staff", { canManageUsers: false })]);
    await screen.findByText("Staff");
    expect(screen.getByRole("columnheader", { name: "Cấp" })).toBeInTheDocument();
    expect(within(rowOf("Chủ")).getByText("Chủ khách")).toBeInTheDocument();
    expect(within(rowOf("Tôi")).getByText("Quản trị viên khách")).toBeInTheDocument();
    expect(within(rowOf("Staff")).queryByText(/Nhân viên$|Chủ khách|Quản trị/)).toBeNull();
  });
  it("no tier column when canManageUsers is absent", async () => {
    mount();
    await screen.findByText("An Nguyễn");
    expect(screen.queryByRole("columnheader", { name: "Cấp" })).toBeNull();
  });
  it("12 people: first 8, 'Xem tất cả 12 người' then 'Thu gọn'; search box only above 8", async () => {
    const twelve = Array.from({ length: 12 }, (_, i) => u(`u${i}`, `Người ${i + 1}`));
    mount(twelve);
    await screen.findByText("Người 1");
    expect(screen.getAllByRole("row")).toHaveLength(1 + 8);
    await userEvent.click(screen.getByRole("button", { name: "Xem tất cả 12 người" }));
    expect(screen.getAllByRole("row")).toHaveLength(1 + 12);
    await userEvent.click(screen.getByRole("button", { name: "Thu gọn" }));
    expect(screen.getAllByRole("row")).toHaveLength(1 + 8);
    await userEvent.type(screen.getByRole("searchbox", { name: "Tìm theo tên hoặc email" }), "Người 12");
    expect(screen.getAllByRole("row")).toHaveLength(1 + 1);
    await userEvent.clear(screen.getByRole("searchbox"));
    await userEvent.type(screen.getByRole("searchbox"), "zzz");
    expect(await screen.findByText("Không có người nào khớp “zzz”.")).toBeInTheDocument();
  });
  it("3 people: no search box", async () => {
    mount();
    await screen.findByText("An Nguyễn");
    expect(screen.queryByRole("searchbox")).toBeNull();
  });
  it("row buttons carry the person's name; own row can be locked but not by deletion", async () => {
    mount();
    await screen.findByText("An Nguyễn");
    expect(screen.getByRole("button", { name: "Đặt lại mật khẩu của An Nguyễn" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Khoá An Nguyễn" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Mở khoá Bo Lê" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Khoá Tôi Dựng" })).toBeEnabled();
  });
});

describe("Người dùng mới (FR-023)", () => {
  async function open() {
    mount();
    await screen.findByText("An Nguyễn");
    await userEvent.click(screen.getByRole("button", { name: "Người dùng mới" }));
  }
  it("opens in place: no dialog, focus on Họ tên, Esc closes", async () => {
    await open();
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.getByRole("textbox", { name: /Họ tên/ })).toHaveFocus();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("textbox", { name: /Họ tên/ })).toBeNull();
  });
  it("password box is masked, autocomplete new-password, email autocomplete off; Hiện reveals", async () => {
    await open();
    const pw = screen.getByLabelText(/Mật khẩu/);
    expect(pw).toHaveAttribute("type", "password");
    expect(pw).toHaveAttribute("autocomplete", "new-password");
    expect(screen.getByRole("textbox", { name: /Email/ })).toHaveAttribute("autocomplete", "off");
    await userEvent.click(screen.getByRole("button", { name: "Hiện" }));
    expect(pw).toHaveAttribute("type", "text");
  });
  it("Tạo ngẫu nhiên fills 16 characters", async () => {
    await open();
    await userEvent.click(screen.getByRole("button", { name: "Tạo ngẫu nhiên" }));
    expect((screen.getByLabelText(/Mật khẩu/) as HTMLInputElement).value).toHaveLength(16);
  });
  it("typed password: created, 'Đã tạo', the reset-link button, NO secret block; nothing in the mutation cache", async () => {
    await open();
    vi.mocked(createUser).mockResolvedValue(u("u-new", "Chị Thu"));
    await userEvent.type(screen.getByRole("textbox", { name: /Họ tên/ }), "Chị Thu");
    await userEvent.type(screen.getByRole("textbox", { name: /Email/ }), "thu@x.vn");
    await userEvent.type(screen.getByLabelText(/Mật khẩu/), "matkhau-go-tay");
    await userEvent.click(screen.getByRole("button", { name: "Lưu người dùng" }));
    expect(await screen.findByText("Đã tạo Chị Thu.")).toBeInTheDocument();
    expect(createUser).toHaveBeenCalledWith(expect.objectContaining({ email: "thu@x.vn", password: "matkhau-go-tay", fullName: "Chị Thu" }));
    expect(document.querySelector("[data-secret-once]")).toBeNull();
    expect(screen.getByRole("button", { name: "Tạo liên kết để Chị Thu tự đặt mật khẩu" })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain("matkhau-go-tay");
    expect(JSON.stringify(qc.getMutationCache().getAll().map((m) => [m.state.variables, m.state.data]))).not.toContain("matkhau-go-tay");
  });
  it("generated password: one-time block with email + password and the tick; gone at Xong; not in any cache", async () => {
    await open();
    vi.mocked(createUser).mockResolvedValue(u("u-new", "Chị Thu"));
    await userEvent.type(screen.getByRole("textbox", { name: /Họ tên/ }), "Chị Thu");
    await userEvent.type(screen.getByRole("textbox", { name: /Email/ }), "thu@x.vn");
    await userEvent.click(screen.getByRole("button", { name: "Tạo ngẫu nhiên" }));
    const typed = (screen.getByLabelText(/Mật khẩu/) as HTMLInputElement).value;
    await userEvent.click(screen.getByRole("button", { name: "Lưu người dùng" }));
    const block = await secretBlock();
    expect(within(block).getByText("thu@x.vn")).toBeInTheDocument();
    expect(within(block).getByText(typed)).toBeInTheDocument();
    expect(JSON.stringify(qc.getMutationCache().getAll().map((m) => [m.state.variables, m.state.data]))).not.toContain(typed);
    expect(within(block).getByRole("button", { name: "Xong" })).toBeDisabled();
    await userEvent.click(within(block).getByRole("checkbox", { name: "Tôi đã lưu" }));
    await userEvent.click(within(block).getByRole("button", { name: "Xong" }));
    expect(document.body.textContent).not.toContain(typed);
  });
  it("409 USER_ALREADY_EXISTS under the email box; 422 PASSWORD_TOO_WEAK shows the server's sentence", async () => {
    await open();
    await userEvent.type(screen.getByRole("textbox", { name: /Họ tên/ }), "A");
    await userEvent.type(screen.getByRole("textbox", { name: /Email/ }), "a@x.vn");
    await userEvent.type(screen.getByLabelText(/Mật khẩu/), "12345678");
    vi.mocked(createUser).mockRejectedValueOnce(err(409, "USER_ALREADY_EXISTS"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu người dùng" }));
    expect(await screen.findByText("Email này đã có người dùng.")).toBeInTheDocument();
    vi.mocked(createUser).mockRejectedValueOnce(err(422, "PASSWORD_TOO_WEAK", "mật khẩu phải có ít nhất 8 ký tự"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu người dùng" }));
    expect(await screen.findByText("mật khẩu phải có ít nhất 8 ký tự")).toBeInTheDocument();
  });
});

describe("Đặt lại mật khẩu (FR-024)", () => {
  const link = { resetLink: "https://app/reset-password?token=SECRET123", token: "SECRET123", expiresAt: "", ttlMinutes: 45, emailQueued: true };
  async function reset(result = link) {
    mount();
    await screen.findByText("An Nguyễn");
    vi.mocked(createResetLink).mockResolvedValue(result);
    await userEvent.click(screen.getByRole("button", { name: "Đặt lại mật khẩu của An Nguyễn" }));
    return secretBlock();
  }
  it("block sits in the row right after the person's row, focused, real ttl and email sentence, warning", async () => {
    const block = await reset();
    const row = rowOf("An Nguyễn");
    expect(row.nextElementSibling).toContainElement(block);
    expect(block).toHaveFocus();
    expect(within(block).getByText(link.resetLink)).toBeInTheDocument();
    expect(within(block).getByText(/Hết hạn sau 45 phút/)).toBeInTheDocument();
    expect(within(block).getByText("Đã đưa email vào hàng đợi gửi tới An Nguyễn.")).toBeInTheDocument();
    expect(within(block).getByText(/Ai có liên kết này đặt được mật khẩu mới cho An Nguyễn/)).toBeInTheDocument();
    expect(createResetLink).toHaveBeenCalledWith("u-an");
  });
  it("emailQueued=false: says to hand the link over", async () => {
    const block = await reset({ ...link, emailQueued: false });
    expect(within(block).getByText(/Chưa gửi được email \(máy chủ chưa cấu hình gửi thư\): đưa liên kết cho người đó/)).toBeInTheDocument();
  });
  it("the token reaches no URL, storage, query cache or mutation cache; Xong removes it", async () => {
    const setItem = vi.spyOn(Storage.prototype, "setItem");
    const block = await reset();
    const seen = JSON.stringify([
      setItem.mock.calls, location.href,
      qc.getQueryCache().getAll().map((q) => q.state.data),
      qc.getMutationCache().getAll().map((m) => [m.state.variables, m.state.data]),
    ]);
    expect(seen).not.toContain("SECRET123");
    await userEvent.click(within(block).getByRole("checkbox", { name: "Tôi đã lưu" }));
    await userEvent.click(within(block).getByRole("button", { name: "Xong" }));
    expect(document.body.textContent).not.toContain("SECRET123");
  });
  it("422 USER_NOT_ACTIVE: 'Mở khoá trước khi đặt lại mật khẩu' at the row", async () => {
    mount();
    await screen.findByText("Bo Lê");
    vi.mocked(createResetLink).mockRejectedValue(err(422, "USER_NOT_ACTIVE"));
    await userEvent.click(screen.getByRole("button", { name: "Đặt lại mật khẩu của Bo Lê" }));
    expect(await screen.findByText("Mở khoá trước khi đặt lại mật khẩu.")).toBeInTheDocument();
    expect(document.querySelector("[data-secret-once]")).toBeNull();
  });
});

describe("reset block for a person outside the drawn rows", () => {
  it("12 people, creating the 13th and pressing the link button still shows the secret block", async () => {
    const twelve = Array.from({ length: 12 }, (_, i) => u(`p${i}`, `Người ${i}`));
    mount(twelve);
    await screen.findByText("Người 0");
    await userEvent.click(screen.getByRole("button", { name: "Người dùng mới" }));
    vi.mocked(createUser).mockResolvedValue(u("u-new", "Chị Thu"));
    await userEvent.type(screen.getByRole("textbox", { name: /Họ tên/ }), "Chị Thu");
    await userEvent.type(screen.getByRole("textbox", { name: /Email/ }), "thu@x.vn");
    await userEvent.type(screen.getByLabelText(/Mật khẩu/), "matkhau-go-tay");
    await userEvent.click(screen.getByRole("button", { name: "Lưu người dùng" }));
    await screen.findByText("Đã tạo Chị Thu.");
    vi.mocked(createResetLink).mockResolvedValue({ resetLink: "https://app/reset-password?token=ZZ", token: "ZZ", expiresAt: "", ttlMinutes: 45, emailQueued: false });
    await userEvent.click(screen.getByRole("button", { name: "Tạo liên kết để Chị Thu tự đặt mật khẩu" }));
    expect(await secretBlock()).toBeInTheDocument();
  });
});

describe("Khoá và Mở khoá (FR-025)", () => {
  it("Khoá asks in the row right after, selects the row, focuses the safe button; confirm sends status inactive", async () => {
    mount();
    await screen.findByText("An Nguyễn");
    await userEvent.click(screen.getByRole("button", { name: "Khoá An Nguyễn" }));
    const row = rowOf("An Nguyễn");
    expect(row).toHaveClass("slds-is-selected");
    const ask = row.nextElementSibling as HTMLElement;
    expect(ask).toHaveTextContent("Khoá An Nguyễn? Người này bị đăng xuất");
    expect(ask).toHaveTextContent("có thể trễ tối đa 30 giây");
    expect(within(ask).getByRole("button", { name: "Huỷ" })).toHaveFocus();
    vi.mocked(updateUser).mockResolvedValue(u("u-an", "An Nguyễn", { status: "inactive" }));
    await userEvent.click(within(ask).getByRole("button", { name: "Khoá" }));
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith("u-an", { status: "inactive" }));
  });
  it("Esc cancels and focus returns to that row's Khoá button", async () => {
    mount();
    await screen.findByText("An Nguyễn");
    await userEvent.click(screen.getByRole("button", { name: "Khoá An Nguyễn" }));
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("button", { name: "Khoá An Nguyễn" })).toHaveFocus();
    expect(updateUser).not.toHaveBeenCalled();
  });
  it("Mở khoá runs straight away with a toast", async () => {
    mount();
    await screen.findByText("Bo Lê");
    vi.mocked(updateUser).mockResolvedValue(u("u-bo", "Bo Lê"));
    await userEvent.click(screen.getByRole("button", { name: "Mở khoá Bo Lê" }));
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith("u-bo", { status: "active" }));
    const { toast } = await import("@/components/ui/Toaster");
    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Đã mở khoá Bo Lê."));
  });
  it.each([
    ["LAST_ADMIN", 422, "Không khoá được quản trị viên cuối cùng."],
    ["USER_STATUS_INVALID", 422, "Trạng thái này không hợp lệ."],
    ["USER_NOT_FOUND", 404, "Người này đã bị xoá."],
  ])("%s gets its own sentence in the row and the confirm stays reachable", async (code, status, sentence) => {
    mount();
    await screen.findByText("An Nguyễn");
    await userEvent.click(screen.getByRole("button", { name: "Khoá An Nguyễn" }));
    vi.mocked(updateUser).mockRejectedValue(err(status, code));
    await userEvent.click(within(rowOf("An Nguyễn").nextElementSibling as HTMLElement).getByRole("button", { name: "Khoá" }));
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    if (code === "USER_NOT_FOUND") await waitFor(() => expect(listUsers).toHaveBeenCalledTimes(2));
  });
});

// SecretOnceBlock (from #585) is a labelled div, not a role=group; find it by its data hook.
async function secretBlock(): Promise<HTMLElement> {
  return waitFor(() => {
    const b = document.querySelector<HTMLElement>("[data-secret-once]");
    expect(b).not.toBeNull();
    return b as HTMLElement;
  });
}

// ── lát u7-nguoi-dung-chi-tiet (T034): ?nguoi=<id> ──
const SESSION = { id: "s-1", browser: "Chrome", os: "macOS", ip: "10.0.0.1", lastUsedAt: "2026-10-09T01:00:00Z", expiresAt: "2026-10-30T01:00:00Z", current: false };
function mountDetail(id = "u-an", users: AdminUser[] = THREE) {
  vi.mocked(getUser).mockImplementation(async (uid: string) => {
    const hit = users.find((x) => x.id === uid);
    if (!hit) throw err(404, "USER_NOT_FOUND");
    return hit;
  });
  vi.mocked(listUserSessions).mockResolvedValue([SESSION] as never);
  return mount(users, id);
}

describe("Một người dùng ?nguoi= (FR-026..FR-028)", () => {
  it("the list's name is a link to ?nguoi=<id> (N16)", async () => {
    mount();
    const link = await screen.findByRole("link", { name: "An Nguyễn" });
    expect(link).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/nguoi-dung?nguoi=u-an");
  });
  it("opens the detail: back link, name as H2, no dialog; not the list", async () => {
    mountDetail();
    expect(await screen.findByRole("heading", { level: 2, name: "An Nguyễn" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Tất cả người dùng" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/nguoi-dung");
    expect(screen.queryByRole("grid")).toBeNull();
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    expect(getUser).toHaveBeenCalledWith("u-an");
    expect(screen.getByRole("link", { name: "Nhật ký hoạt động của người này" })).toHaveAttribute("href", "/studio/du-an/northwind/van-hanh/nguoi-dung/nhat-ky?nguoi=u-an");
  });
  it("saves ONLY the changed field", async () => {
    mountDetail();
    const name = await screen.findByLabelText("Họ tên");
    await userEvent.clear(name);
    await userEvent.type(name, "An Mới");
    vi.mocked(updateUser).mockResolvedValue(u("u-an", "An Mới"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith("u-an", { fullName: "An Mới" }));
  });
  it("'Không vai' sends roleId '' (clear), not undefined", async () => {
    mountDetail();
    const role = await screen.findByLabelText("Vai");
    await userEvent.selectOptions(role, "");
    vi.mocked(updateUser).mockResolvedValue(u("u-an", "An Nguyễn"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith("u-an", { roleId: "" }));
  });
  it("after a save the devices list is read again (a role change ends the person's logins)", async () => {
    mountDetail();
    await screen.findByText("Chrome · macOS");
    const name = await screen.findByLabelText("Họ tên");
    await userEvent.type(name, "x");
    vi.mocked(updateUser).mockResolvedValue(u("u-an", "An Nguyễnx"));
    vi.mocked(listUserSessions).mockResolvedValue([]);
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(listUserSessions).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("Chưa có thiết bị nào đăng nhập.")).toBeInTheDocument();
  });
  it("404 on open: the sentence and the way back, no form", async () => {
    mountDetail("u-zz");
    expect(await screen.findByText("Không tìm thấy người này, có thể đã bị xoá.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "← Tất cả người dùng" })).toBeInTheDocument();
    expect(screen.queryByLabelText("Họ tên")).toBeNull();
  });
  it("USER_NOT_FOUND on save: 'Người này đã bị xoá.' and back to the list", async () => {
    mountDetail();
    const name = await screen.findByLabelText("Họ tên");
    await userEvent.type(name, "x");
    vi.mocked(updateUser).mockRejectedValue(err(404, "USER_NOT_FOUND"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    // The page unmounts on the way back, so the sentence must be a toast (it outlives the page).
    const { toast } = await import("@/components/ui/Toaster");
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Người này đã bị xoá."));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/studio/du-an/northwind/van-hanh/nguoi-dung"));
  });
  it("USER_NOT_FOUND on delete: same toast and back to the list", async () => {
    mountDetail();
    await screen.findByLabelText("Họ tên");
    await userEvent.click(screen.getByRole("button", { name: "Xoá người này" }));
    vi.mocked(deleteUser).mockRejectedValue(err(404, "USER_NOT_FOUND"));
    await userEvent.click(screen.getByRole("button", { name: "Xoá" }));
    const { toast } = await import("@/components/ui/Toaster");
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Người này đã bị xoá."));
    await waitFor(() => expect(push).toHaveBeenCalled());
  });
  it("status and isAdmin are sent as their own fields", async () => {
    mountDetail();
    await userEvent.click(await screen.findByRole("radio", { name: "Đã khoá" }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Quản trị viên khách" }));
    vi.mocked(updateUser).mockResolvedValue(u("u-an", "An Nguyễn"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith("u-an", { status: "inactive", isAdmin: true }));
  });
  it("cancelling the delete confirm gives focus back to the delete button", async () => {
    mountDetail();
    await screen.findByLabelText("Họ tên");
    await userEvent.click(screen.getByRole("button", { name: "Xoá người này" }));
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Xoá người này" })).toHaveFocus());
  });
  it("unsaved change shows the Lưu bar and arms leave-page protection", async () => {
    mountDetail();
    const name = await screen.findByLabelText("Họ tên");
    expect(screen.queryByRole("button", { name: "Lưu" })).toBeNull();
    await userEvent.type(name, "x");
    expect(await screen.findByRole("button", { name: "Lưu" })).toBeInTheDocument();
    const ev = new Event("beforeunload", { cancelable: true });
    window.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
  });
  it("Xoá asks in the page with the consequence, then DELETEs and goes back to the list", async () => {
    mountDetail();
    await screen.findByLabelText("Họ tên");
    await userEvent.click(screen.getByRole("button", { name: "Xoá người này" }));
    expect(deleteUser).not.toHaveBeenCalled();
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    expect(screen.getByText(/Xoá An Nguyễn\? .*không hoàn tác được/)).toBeInTheDocument();
    vi.mocked(deleteUser).mockResolvedValue(undefined);
    await userEvent.click(screen.getByRole("button", { name: "Xoá" }));
    await waitFor(() => expect(deleteUser).toHaveBeenCalledWith("u-an"));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/studio/du-an/northwind/van-hanh/nguoi-dung"));
  });
  it("own page: Xoá is off with a reason", async () => {
    mountDetail("u-me");
    await screen.findByLabelText("Họ tên");
    const del = screen.getByRole("button", { name: "Xoá người này" });
    expect(del).toBeDisabled();
    expect(del).toHaveAttribute("title", "Bạn không xoá được chính mình.");
  });
  it("delete refused (SELF_DELETE_FORBIDDEN) is said in the page, the person stays", async () => {
    mountDetail();
    await screen.findByLabelText("Họ tên");
    await userEvent.click(screen.getByRole("button", { name: "Xoá người này" }));
    vi.mocked(deleteUser).mockRejectedValue(err(422, "SELF_DELETE_FORBIDDEN"));
    await userEvent.click(screen.getByRole("button", { name: "Xoá" }));
    expect(await screen.findByText("Bạn không xoá được chính mình.")).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });
  it("sessions card sits in the page: Đăng xuất thiết bị asks in the row, then DELETEs that session", async () => {
    mountDetail();
    expect(await screen.findByText("Chrome · macOS")).toBeInTheDocument();
    expect(listUserSessions).toHaveBeenCalledWith("u-an");
    await userEvent.click(screen.getByRole("button", { name: "Đăng xuất Chrome · macOS" }));
    expect(document.querySelectorAll("[role=dialog]")).toHaveLength(0);
    expect(screen.getByText(/Đăng xuất Chrome · macOS\? Thiết bị này bị từ chối/)).toBeInTheDocument();
    vi.mocked(endUserSession).mockResolvedValue(undefined);
    vi.mocked(listUserSessions).mockResolvedValue([]);
    await userEvent.click(screen.getByRole("button", { name: "Đăng xuất thiết bị" }));
    await waitFor(() => expect(endUserSession).toHaveBeenCalledWith("u-an", "s-1"));
  });
  it.each(["../roles/x", "a/b", "x y", "%2e%2e"])("hostile ?nguoi=%s is dropped: the list shows and nothing is fetched for it (FR-133)", async (bad) => {
    mount(THREE, bad);
    expect(await screen.findByRole("grid")).toBeInTheDocument();
    expect(getUser).not.toHaveBeenCalled();
    expect(listUserSessions).not.toHaveBeenCalled();
  });
});

describe("Chủ khách từng người (u7-cap-chu-khach-tung-nguoi)", () => {
  const TIERED = [u("u-me", "Tôi", { isAdmin: true, canManageUsers: false }), u("o", "Chủ", { canManageUsers: true }), u("s", "Staff", { canManageUsers: false })];
  it("no switch anywhere when canManageUsers is absent", async () => {
    mount();
    await screen.findByText("An Nguyễn");
    expect(screen.queryByRole("button", { name: /chủ khách/i })).toBeNull();
  });
  it("list: no switch on an admin row; Bật asks in the row, then calls setUserManager(id, true)", async () => {
    mount(TIERED);
    await screen.findByText("Staff");
    expect(within(rowOf("Tôi")).queryByRole("button", { name: /chủ khách/i })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Đặt Staff làm chủ khách" }));
    const ask = rowOf("Staff").nextElementSibling as HTMLElement;
    expect(ask).toHaveTextContent("Staff vào được khu quản lý người dùng của app khách; không dùng khi nghi tài khoản bị lộ.");
    expect(setUserManager).not.toHaveBeenCalled();
    vi.mocked(setUserManager).mockResolvedValue(u("s", "Staff", { canManageUsers: true }));
    await userEvent.click(within(ask).getByRole("button", { name: "Đặt làm chủ khách" }));
    await waitFor(() => expect(setUserManager).toHaveBeenCalledWith("s", true));
  });
  it("list: Bỏ runs straight away (setUserManager(id, false))", async () => {
    mount(TIERED);
    await screen.findByText("Chủ");
    vi.mocked(setUserManager).mockResolvedValue(u("o", "Chủ", { canManageUsers: false }));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ chủ khách của Chủ" }));
    await waitFor(() => expect(setUserManager).toHaveBeenCalledWith("o", false));
  });
  it("422 USER_TIER_CONFLICT says the plain sentence in the row", async () => {
    mount(TIERED);
    await screen.findByText("Chủ");
    vi.mocked(setUserManager).mockRejectedValue(err(422, "USER_TIER_CONFLICT"));
    await userEvent.click(screen.getByRole("button", { name: "Bỏ chủ khách của Chủ" }));
    expect(await screen.findByText("Không thể vừa là quản trị viên khách vừa là chủ khách")).toBeInTheDocument();
  });
  it("detail: switch hidden without canManageUsers; with it, enabling asks first then calls", async () => {
    mountDetail("u-an");
    await screen.findByRole("heading", { level: 2, name: "An Nguyễn" });
    expect(screen.queryByRole("checkbox", { name: "Chủ khách" })).toBeNull();
  });
  it("detail: enabling asks, confirm calls setUserManager; admin person shows no switch", async () => {
    mountDetail("s", TIERED);
    const box = await screen.findByRole("checkbox", { name: "Chủ khách" });
    await userEvent.click(box);
    expect(setUserManager).not.toHaveBeenCalled();
    vi.mocked(setUserManager).mockResolvedValue(u("s", "Staff", { canManageUsers: true }));
    await userEvent.click(screen.getByRole("button", { name: "Đặt làm chủ khách" }));
    await waitFor(() => expect(setUserManager).toHaveBeenCalledWith("s", true));
  });
  it("detail: an admin person has no Chủ khách switch", async () => {
    mountDetail("u-me", TIERED);
    await screen.findByRole("heading", { level: 2, name: "Tôi" });
    expect(screen.queryByRole("checkbox", { name: "Chủ khách" })).toBeNull();
  });
  it("detail: saving isAdmin on an owner => 422 USER_TIER_CONFLICT sentence", async () => {
    mountDetail("o", TIERED);
    await userEvent.click(await screen.findByRole("checkbox", { name: "Quản trị viên khách" }));
    vi.mocked(updateUser).mockRejectedValue(err(422, "USER_TIER_CONFLICT"));
    await userEvent.click(screen.getByRole("button", { name: "Lưu" }));
    expect(await screen.findByText("Không thể vừa là quản trị viên khách vừa là chủ khách")).toBeInTheDocument();
  });
  it("the confirm says the person is signed out of every device (server revokes refresh tokens)", async () => {
    mount(TIERED);
    await screen.findByText("Staff");
    await userEvent.click(screen.getByRole("button", { name: "Đặt Staff làm chủ khách" }));
    expect(rowOf("Staff").nextElementSibling).toHaveTextContent("Staff sẽ bị đăng xuất khỏi mọi thiết bị");
  });
  it("list: cancelling the confirm gives focus back to the row's button", async () => {
    mount(TIERED);
    await screen.findByText("Staff");
    await userEvent.click(screen.getByRole("button", { name: "Đặt Staff làm chủ khách" }));
    await userEvent.click(within(rowOf("Staff").nextElementSibling as HTMLElement).getByRole("button", { name: "Huỷ" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Đặt Staff làm chủ khách" })).toHaveFocus());
  });
  it("detail: cancelling the confirm leaves the box unticked and calls nothing", async () => {
    mountDetail("s", TIERED);
    const box = await screen.findByRole("checkbox", { name: "Chủ khách" });
    await userEvent.click(box);
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(setUserManager).not.toHaveBeenCalled();
    expect(screen.getByRole("checkbox", { name: "Chủ khách" })).not.toBeChecked();
  });
  it("detail: unticking an owner calls setUserManager(id, false) at once", async () => {
    mountDetail("o", TIERED);
    vi.mocked(setUserManager).mockResolvedValue(u("o", "Chủ", { canManageUsers: false }));
    await userEvent.click(await screen.findByRole("checkbox", { name: "Chủ khách" }));
    await waitFor(() => expect(setUserManager).toHaveBeenCalledWith("o", false));
  });
  it("detail: 422 USER_TIER_CONFLICT on the switch says the sentence; a later try clears it", async () => {
    mountDetail("o", TIERED);
    vi.mocked(setUserManager).mockRejectedValueOnce(err(422, "USER_TIER_CONFLICT"));
    await userEvent.click(await screen.findByRole("checkbox", { name: "Chủ khách" }));
    expect(await screen.findByText("Không thể vừa là quản trị viên khách vừa là chủ khách")).toBeInTheDocument();
    vi.mocked(setUserManager).mockResolvedValueOnce(u("o", "Chủ", { canManageUsers: false }));
    await userEvent.click(screen.getByRole("checkbox", { name: "Chủ khách" }));
    await waitFor(() => expect(screen.queryByText("Không thể vừa là quản trị viên khách vừa là chủ khách")).toBeNull());
  });
  it("detail: USER_NOT_FOUND on the switch drops the page back to the list", async () => {
    mountDetail("o", TIERED);
    vi.mocked(setUserManager).mockRejectedValue(err(404, "USER_NOT_FOUND"));
    await userEvent.click(await screen.findByRole("checkbox", { name: "Chủ khách" }));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/studio/du-an/northwind/van-hanh/nguoi-dung"));
  });
});
