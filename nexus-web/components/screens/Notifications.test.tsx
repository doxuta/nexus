// Notifications (/notifications) on SLDS — object-home page header + a card list (specs/038 T032).
// Behaviour kept from the pre-SLDS screen: GET /notifications page 1 × 50 (unread-only when that
// view is picked), row click marks the notification read, "Đánh dấu tất cả đã đọc" only while
// something is unread. lib/admin is mocked at the module seam.
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Notifications as Screen } from "./Notifications";
import {
  listNotificationsPage, markNotificationRead, markAllNotificationsRead, type ApiNotification,
} from "@/lib/admin";

vi.mock("@/lib/admin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/lib/admin")>();
  return {
    ...actual,
    ensureAuth: vi.fn(async () => undefined),
    listNotificationsPage: vi.fn(),
    markNotificationRead: vi.fn(async () => undefined),
    markAllNotificationsRead: vi.fn(async () => undefined),
  };
});
vi.mock("@/components/ui/Toaster", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

const noti = (over: Partial<ApiNotification>): ApiNotification => ({
  id: "n1",
  type: "assignment",
  title: "Bạn được giao một bản ghi",
  message: "Một bản ghi Khách hàng vừa được giao cho bạn phụ trách.",
  href: "/crm_accounts/a1",
  isRead: false,
  createdAt: new Date().toISOString(),
  ...over,
});

function page(items: ApiNotification[]) {
  return { items, pagination: { page: 1, limit: 50, total: items.length, totalPages: 1 } };
}

const list = vi.mocked(listNotificationsPage);

// The screen reads through react-query; a fresh client per render keeps tests independent.
function Notifications() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <QueryClientProvider client={qc}>
      <Screen />
    </QueryClientProvider>
  );
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("Notifications (SLDS)", () => {
  it("renders the object-home header and one list row per notification, with no Nexus Be tokens", async () => {
    list.mockResolvedValue(page([noti({}), noti({ id: "n2", type: "approval", title: undefined, message: "Chờ bạn duyệt.", isRead: true })]));
    const { container } = render(<Notifications />);
    expect(await screen.findByText(/vừa được giao cho bạn phụ trách/)).toBeInTheDocument();
    expect(screen.getByText("Chờ bạn duyệt.")).toBeInTheDocument();
    expect(container.querySelector(".slds-page-header")).not.toBeNull();
    expect(container.querySelectorAll("ul.slds-has-dividers_bottom-space > li.slds-item")).toHaveLength(2);
    expect(screen.getByText("Bạn có 1 thông báo chưa đọc")).toBeInTheDocument();
    expect(container.innerHTML).not.toMatch(/\b(bg-panel2?|text-fg|border-bd|text-fnt|text-mut|rounded-card|bg-acd|text-ac-hi)\b/);
    expect(list).toHaveBeenCalledWith(1, 50, false);
  });

  it("says in words (not just colour) which rows are unread, and shows the type label", async () => {
    list.mockResolvedValue(page([noti({}), noti({ id: "n2", isRead: true, message: "Đã đọc rồi." })]));
    render(<Notifications />);
    const unreadRow = (await screen.findByText(/vừa được giao/)).closest("li")!;
    expect(within(unreadRow).getByText("Chưa đọc")).toBeInTheDocument();
    expect(within(unreadRow).getByText("Được giao việc")).toBeInTheDocument();
    const readRow = screen.getByText("Đã đọc rồi.").closest("li")!;
    expect(within(readRow).queryByText("Chưa đọc")).toBeNull();
  });

  it("clicking a row marks that notification read", async () => {
    list.mockResolvedValue(page([noti({})]));
    render(<Notifications />);
    const link = (await screen.findByText(/vừa được giao/)).closest("a")!;
    expect(link).toHaveAttribute("href", "/crm_accounts/a1");
    fireEvent.click(link);
    await waitFor(() => expect(markNotificationRead).toHaveBeenCalledWith("n1"));
    await waitFor(() => expect(within(link.closest("li")!).queryByText("Chưa đọc")).toBeNull());
  });

  it("the list-view switcher loads unread-only", async () => {
    list.mockResolvedValue(page([noti({})]));
    render(<Notifications />);
    await screen.findByText(/vừa được giao/);
    screen.getByRole("button", { name: "Đổi danh sách thông báo" }).focus();
    await userEvent.keyboard("{Enter}");
    await userEvent.click(await screen.findByRole("menuitemcheckbox", { name: /Chưa đọc/ }));
    await waitFor(() => expect(list).toHaveBeenLastCalledWith(1, 50, true));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Chưa đọc");
  });

  it("mark-all is disabled with nothing unread and marks everything otherwise", async () => {
    list.mockResolvedValueOnce(page([noti({ isRead: true })]));
    const { unmount } = render(<Notifications />);
    await screen.findByText(/vừa được giao/);
    expect(screen.getByRole("button", { name: "Đánh dấu tất cả đã đọc" })).toBeDisabled();
    unmount();

    list.mockResolvedValueOnce(page([noti({})]));
    render(<Notifications />);
    await screen.findByText("Chưa đọc");
    const btn = screen.getByRole("button", { name: "Đánh dấu tất cả đã đọc" });
    expect(btn).toBeEnabled();
    fireEvent.click(btn);
    await waitFor(() => expect(markAllNotificationsRead).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByText("Chưa đọc")).toBeNull());
  });

  it("shows the empty illustration when there is nothing", async () => {
    list.mockResolvedValue(page([]));
    const { container } = render(<Notifications />);
    expect(await screen.findByText("Không có thông báo")).toBeInTheDocument();
    expect(container.querySelector(".slds-illustration")).not.toBeNull();
  });
});
