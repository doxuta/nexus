// Component test — /profile on SLDS (specs/038 T032): identity from /auth/me in the record-home
// header + record-detail fields, change-password error routing (401 → current field, 422 → new
// field, client checks before any request), the login-session list (GET /auth/sessions,
// specs/055) and the logout-all confirm that never fires the revoke on "Huỷ".
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Profile } from "@/components/screens/Profile";

vi.mock("@/lib/auth/AuthProvider", () => ({
  useAuth: () => ({
    user: { id: "u1", email: "rep@example.com", fullName: "Trần Sales Rep", isAdmin: false, mfaEnabled: false },
    refetchMe: vi.fn(),
  }),
}));

type FetchFn = (url: RequestInfo | URL, init?: RequestInit) => Promise<Response>;

function jsonRes(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

const SESSIONS = [
  {
    id: "s-mac", browser: "Chrome", os: "macOS", ip: "113.161.42.x", current: true,
    userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) Chrome/128.0.0.0 Safari/537.36",
    lastUsedAt: "2026-09-30T01:15:00Z", expiresAt: "2026-10-07T01:15:00Z",
  },
  { id: "s-phone", browser: "Safari", os: "iOS", ip: "42.118.7.x", lastUsedAt: "2026-09-29T12:00:00Z", expiresAt: "2026-10-06T12:00:00Z" },
];

type FetchMock = ReturnType<typeof vi.fn<FetchFn>>;

// Every fetch goes through here: GET /auth/sessions answers `sessions` (the card loads it on mount);
// anything else goes to `other`. Returns the mock so a test can look at the calls it cares about.
function routeFetch(
  other: FetchFn = async () => jsonRes({}, 200),
  sessions: () => Response | Promise<Response> = () => jsonRes({ data: SESSIONS }, 200),
): FetchMock {
  const mock = vi.fn<FetchFn>(async (url, init) => {
    if (String(url).endsWith("/auth/sessions") && (init?.method ?? "GET") === "GET") return sessions();
    return other(url, init);
  });
  global.fetch = mock as unknown as typeof fetch;
  return mock;
}

const callsTo = (mock: FetchMock, path: string) => mock.mock.calls.filter(([u]) => String(u).includes(path));

describe("Profile (SLDS)", () => {
  let originalFetch: FetchFn;

  beforeEach(() => {
    originalFetch = global.fetch as unknown as FetchFn;
    window.localStorage.clear();
    window.localStorage.setItem("nexus_tenant", "northwind");
    routeFetch();
  });

  afterEach(() => {
    global.fetch = originalFetch as unknown as typeof fetch;
    window.localStorage.clear();
  });

  it("shows the /auth/me identity: name in the record-home header, email, role, workspace, MFA state", () => {
    render(<Profile />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Trần Sales Rep");
    expect(screen.getAllByText("rep@example.com").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Người dùng").length).toBeGreaterThan(0);
    expect(screen.getAllByText("northwind").length).toBeGreaterThan(0);
    expect(screen.getAllByText("Chưa bật").length).toBeGreaterThan(0);
  });

  it("record-detail rows keep two columns: an odd field count ends with the blueprint's empty item", () => {
    render(<Profile />);
    // 5 fields with a workspace → the last row pairs 'Xác thực 2 lớp' with an empty listitem
    // (form-element/record-detail), so that field does not stretch across both columns.
    const card = screen.getByText("Thông tin tài khoản").closest("article")!;
    const rows = card.querySelectorAll(".slds-form__row");
    expect(rows).toHaveLength(3);
    // (happy-dom has no `:scope >`, so read the direct children.)
    const items = (row: Element) => Array.from(row.children).filter((el) => el.matches(".slds-form__item[role='listitem']"));
    rows.forEach((row) => expect(items(row)).toHaveLength(2));
    const last = items(rows[2]);
    expect(last[0]).toHaveTextContent("Xác thực 2 lớp");
    expect(last[1]).toBeEmptyDOMElement();
  });

  it("lists every live login session: device, masked IP and last activity in VN time", async () => {
    render(<Profile />);
    expect(await screen.findByText("Chrome · macOS")).toBeInTheDocument();
    expect(screen.getByText("IP 113.161.42.x · Hoạt động lần cuối 08:15 30/09/2026")).toBeInTheDocument();
    expect(screen.getByText("Safari · iOS")).toBeInTheDocument();
    expect(screen.getByText("Phiên đăng nhập (2)")).toBeInTheDocument();
    // The hover title carries the raw User-Agent; the row text never does.
    expect(screen.getByText("Chrome · macOS")).toHaveAttribute("title", SESSIONS[0].userAgent);
    expect(screen.queryByText(/không lưu danh sách thiết bị/)).not.toBeInTheDocument();
  });

  it("an unknown device reads as such, an empty list says so", async () => {
    routeFetch(undefined, () => jsonRes({ data: [{ id: "s-x", lastUsedAt: "2026-09-30T01:15:00Z", expiresAt: "2026-10-07T01:15:00Z" }] }, 200));
    const { unmount } = render(<Profile />);
    expect(await screen.findByText("Thiết bị không xác định")).toBeInTheDocument();
    expect(screen.getByText("Hoạt động lần cuối 08:15 30/09/2026")).toBeInTheDocument();
    unmount();

    routeFetch(undefined, () => jsonRes({ data: [] }, 200));
    render(<Profile />);
    expect(await screen.findByText("Không thấy phiên đăng nhập nào đang hoạt động.")).toBeInTheDocument();
  });

  it("marks the caller's own session 'Phiên này' (success badge) and offers no logout on it", async () => {
    render(<Profile />);
    const macRow = (await screen.findByText("Chrome · macOS")).closest("li")!;
    const badge = within(macRow).getByText("Phiên này").closest(".slds-badge")!;
    expect(badge).toHaveClass("slds-theme_success");
    expect(within(macRow).queryByRole("button")).toBeNull();
    const phoneRow = screen.getByText("Safari · iOS").closest("li")!;
    expect(within(phoneRow).queryByText("Phiên này")).toBeNull();
    expect(within(phoneRow).getByRole("button", { name: "Đăng xuất Safari · iOS" })).toBeInTheDocument();
  });

  it("ending another device asks first, DELETEs that session, and reloads the list", async () => {
    let sessions = SESSIONS;
    const fetchMock = routeFetch(
      async (url, init) => {
        if (init?.method === "DELETE") {
          sessions = SESSIONS.filter((s) => s.id !== "s-phone");
          return new Response(null, { status: 204 });
        }
        return jsonRes({}, 200);
      },
      () => jsonRes({ data: sessions }, 200),
    );
    render(<Profile />);
    fireEvent.click(await screen.findByRole("button", { name: "Đăng xuất Safari · iOS" }));
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent("Đăng xuất thiết bị này?");
    expect(dialog).toHaveTextContent("Safari · iOS");
    fireEvent.click(within(dialog).getByRole("button", { name: "Đăng xuất thiết bị" }));

    await waitFor(() => expect(screen.queryByText("Safari · iOS")).not.toBeInTheDocument());
    const deletes = fetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE");
    expect(deletes).toHaveLength(1);
    expect(String(deletes[0][0])).toMatch(/\/auth\/sessions\/s-phone$/);
    expect(screen.getByText("Chrome · macOS")).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("after ending a device, focus lands on the session list, not <body> (WCAG 2.4.3)", async () => {
    let sessions = SESSIONS;
    routeFetch(
      async (_url, init) => {
        if (init?.method === "DELETE") {
          sessions = SESSIONS.filter((s) => s.id !== "s-phone");
          return new Response(null, { status: 204 });
        }
        return jsonRes({}, 200);
      },
      // The reload is slower than the dialog close — the order a real network gives.
      () => new Promise<Response>((r) => setTimeout(() => r(jsonRes({ data: sessions }, 200)), 30)),
    );
    render(<Profile />);
    await userEvent.click(await screen.findByRole("button", { name: "Đăng xuất Safari · iOS" }));
    await userEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Đăng xuất thiết bị" }));

    await waitFor(() => expect(screen.queryByText("Safari · iOS")).not.toBeInTheDocument());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("list", { name: "Phiên đăng nhập" })).toHaveFocus());
  });

  it("Huỷ on the end-session confirm ends nothing", async () => {
    const fetchMock = routeFetch();
    render(<Profile />);
    fireEvent.click(await screen.findByRole("button", { name: "Đăng xuất Safari · iOS" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Huỷ" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(fetchMock.mock.calls.filter(([, init]) => init?.method === "DELETE")).toHaveLength(0);
  });

  it("a session that is already gone (404) is dropped from the list instead of erroring forever", async () => {
    let sessions = SESSIONS;
    routeFetch(
      async (_url, init) => {
        if (init?.method === "DELETE") {
          sessions = SESSIONS.filter((s) => s.id !== "s-phone");
          return jsonRes({ error: "không tìm thấy phiên đăng nhập", code: "SESSION_NOT_FOUND" }, 404);
        }
        return jsonRes({}, 200);
      },
      () => jsonRes({ data: sessions }, 200),
    );
    render(<Profile />);
    fireEvent.click(await screen.findByRole("button", { name: "Đăng xuất Safari · iOS" }));
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Đăng xuất thiết bị" }));
    await waitFor(() => expect(screen.queryByText("Safari · iOS")).not.toBeInTheDocument());
  });

  it("a failed load says so, and 'Thử lại' loads again", async () => {
    let fail = true;
    routeFetch(undefined, () => (fail ? jsonRes({ error: "boom", code: "SESSION_OP_FAILED" }, 500) : jsonRes({ data: SESSIONS }, 200)));
    render(<Profile />);
    expect(await screen.findByText("Không tải được danh sách thiết bị.")).toBeInTheDocument();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Thử lại" }));
    expect(await screen.findByText("Chrome · macOS")).toBeInTheDocument();
    expect(screen.queryByText("Không tải được danh sách thiết bị.")).not.toBeInTheDocument();
  });

  it("client-side checks stop a mismatched confirmation before any request", () => {
    const fetchMock = routeFetch();
    render(<Profile />);
    fireEvent.change(screen.getByLabelText("Mật khẩu hiện tại"), { target: { value: "old-pass-1" } });
    fireEvent.change(screen.getByLabelText("Mật khẩu mới"), { target: { value: "new-pass-12" } });
    fireEvent.change(screen.getByLabelText("Xác nhận mật khẩu mới"), { target: { value: "new-pass-99" } });
    fireEvent.click(screen.getByRole("button", { name: "Cập nhật mật khẩu" }));
    expect(screen.getByText("Mật khẩu mới và xác nhận không khớp.")).toBeInTheDocument();
    expect(screen.getByLabelText("Mật khẩu mới")).toHaveAttribute("aria-invalid", "true");
    expect(callsTo(fetchMock, "/auth/me/password")).toHaveLength(0);
  });

  it("401 from PUT /auth/me/password lands inline on the current-password field", async () => {
    routeFetch(async () => jsonRes({ error: "Mật khẩu không đúng", code: "UNAUTHORIZED" }, 401));
    render(<Profile />);
    fireEvent.change(screen.getByLabelText("Mật khẩu hiện tại"), { target: { value: "wrong-pass" } });
    fireEvent.change(screen.getByLabelText("Mật khẩu mới"), { target: { value: "new-pass-12" } });
    fireEvent.change(screen.getByLabelText("Xác nhận mật khẩu mới"), { target: { value: "new-pass-12" } });
    fireEvent.click(screen.getByRole("button", { name: "Cập nhật mật khẩu" }));
    await waitFor(() => expect(screen.getByText("Mật khẩu hiện tại không đúng.")).toBeInTheDocument());
    expect(screen.getByLabelText("Mật khẩu hiện tại")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText("Mật khẩu mới")).not.toHaveAttribute("aria-invalid");
  });

  it("'Hiện mật khẩu' reveals all three password fields", () => {
    render(<Profile />);
    expect(screen.getByLabelText("Mật khẩu mới")).toHaveAttribute("type", "password");
    fireEvent.click(screen.getByLabelText("Hiện mật khẩu"));
    expect(screen.getByLabelText("Mật khẩu hiện tại")).toHaveAttribute("type", "text");
    expect(screen.getByLabelText("Mật khẩu mới")).toHaveAttribute("type", "text");
    expect(screen.getByLabelText("Xác nhận mật khẩu mới")).toHaveAttribute("type", "text");
  });

  it("'Đăng xuất mọi thiết bị' asks first, says every device is signed out at once, and Huỷ revokes nothing", () => {
    const fetchMock = routeFetch(async () => new Response(null, { status: 204 }));
    render(<Profile />);
    fireEvent.click(screen.getByRole("button", { name: "Đăng xuất mọi thiết bị" }));
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveTextContent("Đăng xuất mọi thiết bị?");
    expect(dialog).toHaveTextContent("đăng xuất ngay ở thao tác kế tiếp");
    expect(dialog).not.toHaveTextContent("15 phút");
    expect(screen.getByRole("button", { name: "Đăng xuất tất cả" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(callsTo(fetchMock, "/auth/logout")).toHaveLength(0);
  });

  it("closing the logout-all confirm returns focus to 'Đăng xuất mọi thiết bị' (Escape and Huỷ)", async () => {
    render(<Profile />);
    const opener = screen.getByRole("button", { name: "Đăng xuất mọi thiết bị" });
    await userEvent.click(opener);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(opener).toHaveFocus());

    await userEvent.click(opener);
    await userEvent.click(screen.getByRole("button", { name: "Huỷ" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await waitFor(() => expect(opener).toHaveFocus());
  });
});
