// GlobalHeader — blueprint global-header/base (SLDS 2.264.1). The presentational header is tested on
// plain props; ShellGlobalHeader (the session-wired one AppShell mounts) is tested with its context
// hooks stubbed at the module seam, like CommandPalette.test.tsx.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import type { ApiNotification } from "@/lib/api";
import { applyBranding, clearBranding } from "@/lib/branding";
import { qk } from "@/lib/query/keys";
import { GlobalHeader, ShellGlobalHeader, type GlobalHeaderProps } from "./GlobalHeader";

const nav = vi.hoisted(() => ({ pathname: "/home", push: vi.fn() }));
const session = vi.hoisted(() => ({
  user: null as null | Record<string, unknown>,
  pins: [] as string[],
  toggle: vi.fn(),
  logout: vi.fn(async () => undefined),
}));

vi.mock("next/navigation", () => ({ usePathname: () => nav.pathname, useRouter: () => ({ push: nav.push }) }));
vi.mock("@/lib/navigation-blocker", () => ({ useGuardedRouter: () => ({ push: nav.push }) }));
vi.mock("@/lib/auth/AuthProvider", () => ({ useAuth: () => ({ user: session.user, logout: session.logout }) }));
vi.mock("@/lib/pins", () => ({ usePins: () => ({ pins: session.pins, toggle: session.toggle }) }));
vi.mock("@/lib/metadata/hooks", () => ({
  useEntities: () => ({
    data: [
      { name: "crm_accounts", label: "Khách hàng" },
      { name: "payroll", label: "Bảng lương" },
    ],
  }),
}));
// The rep cannot view "payroll": a stale pin to it must not surface anywhere.
vi.mock("@/lib/metadata/permissions", () => ({ usePermissions: () => ({ can: (_a: string, e: string) => e !== "payroll" }) }));
vi.mock("@/components/ui/CommandPalette", () => ({
  CommandPalette: ({ open }: { open: boolean }) => (open ? <div role="dialog" aria-label="Bảng lệnh" /> : null),
  useCommandPalette: () => ({ open: false, setOpen: vi.fn() }),
}));
vi.mock("@/lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/lib/api")>()),
  tenantSlug: () => "northwind",
  getNotificationUnreadCount: vi.fn(async () => 0),
  listNotifications: vi.fn(async () => ({ notifications: [] })),
  markNotificationRead: vi.fn(async () => undefined),
  markAllNotificationsRead: vi.fn(async () => undefined),
}));

const NOTIS: ApiNotification[] = [
  { id: "n1", type: "assignment", title: "Được giao khách hàng", message: "Công ty Minh Phát", href: "/crm_accounts/1", isRead: false, createdAt: "2026-09-28T08:00:00Z" },
  { id: "n2", type: "system", message: "Đã nhập xong 120 dòng", isRead: true, createdAt: "2026-09-27T08:00:00Z" },
] as ApiNotification[];

function props(over: Partial<GlobalHeaderProps> = {}): GlobalHeaderProps {
  return {
    brandName: "Minh Phát CRM",
    onSearch: vi.fn(),
    favorites: [],
    onHelp: vi.fn(),
    isAdmin: false,
    setupLinks: [],
    notifications: { unread: 0, items: [], onRead: vi.fn(), onMarkAll: vi.fn() },
    user: { name: "Nguyễn Thị Lan", subtitle: "Nhân viên bán hàng · northwind" },
    onNavigate: vi.fn(),
    onLogout: vi.fn(),
    ...over,
  };
}

async function openMenu(name: string) {
  screen.getByRole("button", { name }).focus();
  await userEvent.keyboard("{Enter}");
  return screen.findByRole("menu", { name });
}

describe("GlobalHeader (presentational)", () => {
  it("renders the blueprint container, skip links and three header items", () => {
    const { container } = render(<GlobalHeader {...props()} />);
    const header = container.querySelector("header")!;
    expect(header).toHaveClass("slds-global-header_container");
    const [skipNav, skipMain, bar] = Array.from(header.children);
    for (const a of [skipNav, skipMain]) expect(a).toHaveClass("slds-assistive-text", "slds-assistive-text_focus");
    expect([skipNav, skipMain].map((a) => a.getAttribute("href"))).toEqual(["#nx-global-nav", "#nx-main"]);
    expect(bar).toHaveClass("slds-global-header", "slds-grid", "slds-grid_align-spread");
    expect(Array.from(bar.children).every((c) => c.classList.contains("slds-global-header__item"))).toBe(true);
    expect(bar.children).toHaveLength(3);
    expect(bar.querySelector(".slds-global-header__item_search .slds-global-search__form-element")).not.toBeNull();
    expect(bar.querySelector("ul.slds-global-actions > li.slds-global-actions__item")).not.toBeNull();
    // Tenant brand, never the Salesforce logo asset.
    expect(screen.getByText("Minh Phát CRM")).toHaveClass("slds-truncate");
  });

  // #343: SLDS fixes the logo at 12.5rem and gives each item 1rem side padding, so at 390px the bar was 440px wide.
  // jsdom has no layout; this pins the classes that let the bar fit (measured in a real browser at acceptance).
  it("lets the logo shrink and tightens item padding below md so the bar fits 390px", () => {
    const { container } = render(<GlobalHeader {...props()} />);
    const logo = container.querySelector(".slds-global-header__logo")!;
    expect(logo).toHaveClass("max-md:w-auto", "min-w-0");
    expect(logo.parentElement).toHaveClass("min-w-0", "max-md:px-2");
    expect(container.querySelector("ul.slds-global-actions")!.parentElement).toHaveClass("shrink-0", "max-md:px-2");
  });

  it("draws the Nexus mark on an SLDS brand tile when the tenant has no logo", () => {
    const { container } = render(<GlobalHeader {...props()} />);
    const logo = container.querySelector(".slds-global-header__logo")!;
    const tile = logo.querySelector(".slds-icon_container")!;
    expect(tile.getAttribute("style")).toContain("--slds-c-icon-color-background: var(--slds-g-color-brand-base-50)");
    expect(tile.querySelector("svg")).toHaveClass("slds-icon", "nx-lucide", "nx-lucide_tile");
    // No Nexus Be tokens (removed at the Phase 2 exit gate).
    expect(logo.querySelector("[class*='bg-ac'], [class*='text-acf']")).toBeNull();
  });

  it("opens the command palette from the search field", async () => {
    const onSearch = vi.fn();
    render(<GlobalHeader {...props({ onSearch, searchPlaceholder: "Tìm trong khách hàng…" })} />);
    const search = screen.getByRole("button", { name: "Tìm trong khách hàng…" });
    expect(search).toHaveClass("slds-input", "slds-combobox__input");
    expect(search).toHaveAttribute("aria-haspopup", "dialog");
    await userEvent.click(search);
    expect(onSearch).toHaveBeenCalledTimes(1);
  });

  it("disables the favorite star when the page has nothing to pin", () => {
    render(<GlobalHeader {...props()} />);
    const star = screen.getByRole("button", { name: "Mở một danh sách dữ liệu để ghim" });
    expect(star).toBeDisabled();
    expect(star).toHaveClass("slds-global-actions__favorites-action", "slds-is-disabled");
    expect(star).toHaveAttribute("aria-pressed", "false");
  });

  it("toggles the current entity's pin and shows the pressed state", async () => {
    const onToggle = vi.fn();
    const { rerender } = render(<GlobalHeader {...props({ pinTarget: { label: "Khách hàng", pinned: false, onToggle } })} />);
    await userEvent.click(screen.getByRole("button", { name: "Ghim Khách hàng lên thanh điều hướng" }));
    expect(onToggle).toHaveBeenCalledTimes(1);
    rerender(<GlobalHeader {...props({ pinTarget: { label: "Khách hàng", pinned: true, onToggle } })} />);
    const star = screen.getByRole("button", { name: "Bỏ ghim Khách hàng" });
    expect(star).toHaveAttribute("aria-pressed", "true");
    expect(star).toHaveClass("slds-is-selected");
    expect(star.parentElement).toHaveClass("slds-button-group");
    expect(star.closest(".slds-global-actions__favorites")).toHaveClass("slds-dropdown-trigger", "slds-dropdown-trigger_click");
  });

  it("lists pinned entities under ▾ and navigates with the keyboard", async () => {
    const onNavigate = vi.fn();
    render(
      <GlobalHeader
        {...props({
          onNavigate,
          favorites: [
            { href: "/crm_accounts", label: "Khách hàng" },
            { href: "/crm_deals", label: "Cơ hội" },
          ],
        })}
      />,
    );
    const menu = await openMenu("Xem mục đã ghim");
    expect(menu).toHaveClass("slds-dropdown", "slds-dropdown_right");
    expect(menu.closest(".slds-global-actions__favorites")).toHaveClass("slds-is-open");
    await waitFor(() => expect(screen.getAllByRole("menuitem")[0]).toHaveFocus());
    await userEvent.keyboard("{ArrowDown}");
    expect(screen.getByRole("menuitem", { name: "Cơ hội" })).toHaveFocus();
    await userEvent.keyboard("{Enter}");
    expect(onNavigate).toHaveBeenCalledWith("/crm_deals");
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("closes the favorites menu on Tab and moves on to the next header action", async () => {
    render(<GlobalHeader {...props({ favorites: [{ href: "/crm_accounts", label: "Khách hàng" }] })} />);
    await openMenu("Xem mục đã ghim");
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Khách hàng" })).toHaveFocus());
    await userEvent.keyboard("{Tab}");
    await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
    // Next focusable in DOM order. happy-dom has no CSS, so that is the small-screen search action;
    // on a desktop it is `md:hidden`, checkVisibility() skips it and focus lands on Help.
    await waitFor(() => expect(screen.getByRole("button", { name: "Tìm kiếm" })).toHaveFocus());
  });

  it("says so when nothing is pinned", async () => {
    render(<GlobalHeader {...props()} />);
    await openMenu("Xem mục đã ghim");
    expect(screen.getByRole("menuitem", { name: "Chưa ghim mục nào" })).toHaveAttribute("aria-disabled", "true");
  });

  it("hides the Setup gear and /about from non-admins", async () => {
    render(<GlobalHeader {...props()} />);
    expect(screen.queryByRole("button", { name: "Thiết lập" })).toBeNull();
    await openMenu("Trợ giúp");
    expect(screen.getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Phím tắt & trợ giúp"]);
  });

  it("opens the shortcut sheet from the help menu", async () => {
    const onHelp = vi.fn();
    render(<GlobalHeader {...props({ onHelp })} />);
    await openMenu("Trợ giúp");
    await userEvent.keyboard("{Enter}");
    expect(onHelp).toHaveBeenCalledTimes(1);
  });

  it("gives admins the Setup menu (admin pages + Studio bridge) and /about", async () => {
    const onNavigate = vi.fn();
    render(
      <GlobalHeader
        {...props({
          isAdmin: true,
          onNavigate,
          setupLinks: [
            { href: "/admin/users", label: "Người dùng" },
            { href: "/admin/roles", label: "Vai trò & Phân quyền" },
          ],
          studioHref: "/studio/apps/crm",
        })}
      />,
    );
    const setup = await openMenu("Thiết lập");
    expect(within(setup).getByRole("group", { name: "Quản trị" })).toBeInTheDocument();
    expect(screen.getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Người dùng", "Vai trò & Phân quyền", "Tạo trong Studio"]);
    await userEvent.keyboard("{ArrowUp}{Enter}");
    expect(onNavigate).toHaveBeenCalledWith("/studio/apps/crm");

    await openMenu("Trợ giúp");
    expect(screen.getByRole("menuitem", { name: "Giới thiệu Nexus" })).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("menu")).toBeNull();
    expect(screen.getByRole("button", { name: "Trợ giúp" })).toHaveFocus();
  });

  it("keeps the notification badge hidden at zero", () => {
    const { container } = render(<GlobalHeader {...props()} />);
    const bell = screen.getByRole("button", { name: "Không có thông báo mới" });
    expect(bell).toHaveClass("slds-global-actions__notifications");
    expect(bell).not.toHaveClass("slds-incoming-notification");
    const badge = container.querySelector(".slds-notification-badge")!;
    expect(badge).toHaveAttribute("aria-hidden", "true");
    expect(badge).not.toHaveClass("slds-show-notification");
  });

  it("shows the unread count and the tray rows", async () => {
    const onRead = vi.fn();
    const onMarkAll = vi.fn();
    const onOpenChange = vi.fn();
    const { container } = render(
      <GlobalHeader {...props({ notifications: { unread: 12, items: NOTIS, onRead, onMarkAll, onOpenChange } })} />,
    );
    const badge = container.querySelector(".slds-notification-badge")!;
    expect(badge).toHaveClass("slds-show-notification");
    expect(badge).toHaveTextContent("9+");
    const bell = screen.getByRole("button", { name: "12 thông báo mới" });
    expect(bell).toHaveClass("slds-incoming-notification");

    await userEvent.click(bell);
    expect(onOpenChange).toHaveBeenCalledWith(true);
    const tray = screen.getByRole("dialog", { name: "Thông báo" });
    expect(tray).toHaveClass("slds-popover", "slds-popover_large");
    const rows = tray.querySelectorAll("li.slds-global-header__notification");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveClass("slds-global-header__notification_unread");
    expect(rows[1]).not.toHaveClass("slds-global-header__notification_unread");

    // Showcase global-header/base tray: header outside the (scrolling) body, list straight in a
    // padding-less body, one grid row per notification, unread marked by more than colour.
    const [, header, body] = Array.from(tray.children);
    expect(tray.firstElementChild).toHaveClass("slds-popover__close");
    expect(header.tagName).toBe("HEADER");
    expect(header).toHaveClass("slds-popover__header");
    expect(header).toHaveTextContent("Thông báo");
    expect(body).toHaveClass("slds-popover__body", "slds-p-around_none");
    expect(body.firstElementChild!.tagName).toBe("UL");
    const link0 = within(rows[0] as HTMLElement).getByRole("link");
    expect(link0.parentElement).toHaveClass("slds-grid", "slds-grid_align-spread");
    expect(link0.parentElement!.parentElement).toHaveClass("slds-media__body");
    const dot = rows[0].querySelector("abbr")!;
    expect(dot).toHaveClass("slds-text-link", "slds-m-horizontal_xxx-small");
    expect(dot).toHaveTextContent("●");
    expect(within(link0).getByText("Chưa đọc")).toHaveClass("slds-assistive-text");
    expect(rows[1].querySelector("abbr")).toBeNull();
    expect(within(rows[1] as HTMLElement).queryByText("Chưa đọc")).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: "Đánh dấu đã đọc" }));
    expect(onMarkAll).toHaveBeenCalledTimes(1);
    await userEvent.click(within(rows[0] as HTMLElement).getByRole("link"));
    expect(onRead).toHaveBeenCalledWith(NOTIS[0]);
  });

  it("disables 'mark all read' when nothing is unread", async () => {
    render(<GlobalHeader {...props()} />);
    await userEvent.click(screen.getByRole("button", { name: "Không có thông báo mới" }));
    expect(screen.getByRole("button", { name: "Đánh dấu đã đọc" })).toBeDisabled();
    expect(screen.getAllByText("Không có thông báo mới").length).toBeGreaterThan(0);
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens the account menu with name · role · tenant and signs out", async () => {
    const onLogout = vi.fn();
    const onNavigate = vi.fn();
    render(<GlobalHeader {...props({ onLogout, onNavigate })} />);
    const avatar = screen.getByRole("button", { name: "Menu tài khoản" });
    expect(avatar).toHaveClass("slds-global-actions__avatar");
    expect(avatar.querySelector(".slds-avatar.slds-avatar_circle.slds-avatar_medium abbr.slds-avatar__initials")).toHaveTextContent("NT");
    const menu = await openMenu("Menu tài khoản");
    expect(menu).toHaveClass("slds-dropdown_right");
    const head = menu.querySelector("li.slds-dropdown__header")!;
    expect(head).toHaveTextContent("Nguyễn Thị Lan");
    expect(head.nextElementSibling).toHaveTextContent("Nhân viên bán hàng · northwind");
    expect(head.nextElementSibling).toHaveAttribute("role", "presentation");
    await waitFor(() => expect(screen.getByRole("menuitem", { name: "Hồ sơ & mật khẩu" })).toHaveFocus());
    await userEvent.keyboard("{Enter}");
    expect(onNavigate).toHaveBeenCalledWith("/profile");
    await openMenu("Menu tài khoản");
    await userEvent.keyboard("{ArrowUp}{Enter}");
    expect(onLogout).toHaveBeenCalledTimes(1);
  });
});

function wrap(ui: ReactNode, qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

// specs/073 T053 / FR-052: Studio slots on the same header.
describe("GlobalHeader Studio slots", () => {
  it("mounts the switcher beside the brand and the assistant toggle as a stateful button", async () => {
    const onToggle = vi.fn();
    const { rerender } = render(
      <GlobalHeader {...props({ switcher: <button type="button">Bộ chuyển dự án</button>, assistantToggle: { pressed: false, onToggle } })} />,
    );
    expect(screen.getByRole("button", { name: "Bộ chuyển dự án" })).toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Trợ lý" });
    expect(toggle).toHaveClass("slds-button_stateful");
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(toggle);
    expect(onToggle).toHaveBeenCalledTimes(1);
    rerender(<GlobalHeader {...props({ assistantToggle: { pressed: true, onToggle } })} />);
    expect(screen.getByRole("button", { name: "Trợ lý" })).toHaveAttribute("aria-pressed", "true");
  });

  it("shows no switcher or toggle unless given", () => {
    render(<GlobalHeader {...props()} />);
    expect(screen.queryByRole("button", { name: "Trợ lý" })).toBeNull();
  });

  it("hideSetup hides the Setup gear even for an admin", () => {
    render(<GlobalHeader {...props({ isAdmin: true, hideSetup: true })} />);
    expect(screen.queryByRole("button", { name: "Thiết lập" })).toBeNull();
    render(<GlobalHeader {...props({ isAdmin: true })} />);
    expect(screen.getByRole("button", { name: "Thiết lập" })).toBeInTheDocument();
  });

  it("below 768px: brand suffix hides, the toggle label becomes assistive-only, ⌘K lives in the account menu", async () => {
    const onSearch = vi.fn();
    render(
      <GlobalHeader
        {...props({ brandName: "Nexus", brandSuffix: "Studio", searchInMenu: true, onSearch, assistantToggle: { pressed: false, onToggle: vi.fn() } })}
      />,
    );
    expect(screen.getByText("Studio")).toHaveClass("max-md:hidden");
    expect(screen.getByText("Trợ lý")).toHaveClass("max-md:sr-only");
    // the compact search icon is gone from the bar …
    expect(screen.queryByRole("button", { name: "Tìm kiếm" })).toBeNull();
    // … and ⌘K is an account-menu item, md:hidden.
    await openMenu("Menu tài khoản");
    const item = screen.getByRole("menuitem", { name: /Tìm kiếm/ });
    expect(item.closest("li")).toHaveClass("md:hidden");
    await userEvent.click(item);
    expect(onSearch).toHaveBeenCalledTimes(1);
  });

  it("brandHref makes the brand a link home (specs/073 FR-052 'Nexus Studio' button)", () => {
    render(<GlobalHeader {...props({ brandName: "Nexus", brandSuffix: "Studio", brandHref: "/studio" })} />);
    expect(screen.getByRole("link", { name: "Nexus Studio" })).toHaveAttribute("href", "/studio");
  });

  it("without brandHref the brand stays plain text", () => {
    render(<GlobalHeader {...props()} />);
    expect(screen.queryByRole("link", { name: "Minh Phát CRM" })).toBeNull();
  });

  it("accountLinks replace profile and preferences in the account menu", async () => {
    const onNavigate = vi.fn();
    render(
      <GlobalHeader
        {...props({
          onNavigate,
          accountLinks: [
            { href: "/home/me", label: "Tài khoản của tôi" },
            { href: "/home", label: "Mở app của xưởng Nexus" },
          ],
        })}
      />,
    );
    await openMenu("Menu tài khoản");
    expect(screen.queryByRole("menuitem", { name: "Hồ sơ & mật khẩu" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Tuỳ chọn" })).toBeNull();
    await userEvent.click(screen.getByRole("menuitem", { name: "Mở app của xưởng Nexus" }));
    expect(onNavigate).toHaveBeenCalledWith("/home");
    expect(screen.queryByRole("menuitem", { name: "Tài khoản của tôi" })).toBeNull(); // menu closed after select
  });

  it("with a switcher the brand name hides below 768px so the switcher cannot collide with the toggle", () => {
    render(<GlobalHeader {...props({ brandName: "Nexus", switcher: <button type="button">Minh Phát CRM</button> })} />);
    expect(screen.getByText("Nexus")).toHaveClass("max-md:hidden");
  });

  it("no control is smaller than 24x24 (every header action is the small icon-button size)", () => {
    const { container } = render(<GlobalHeader {...props({ assistantToggle: { pressed: false, onToggle: vi.fn() } })} />);
    for (const b of container.querySelectorAll(".slds-global-actions button")) {
      expect(b.className).toMatch(/slds-button_icon-small|slds-global-actions__avatar|slds-button_stateful/);
    }
    // the stateful toggle is not an icon-button: its 24px floor is the explicit utility pair
    expect(screen.getByRole("button", { name: "Trợ lý" })).toHaveClass("min-h-6", "min-w-6");
  });
});

describe("ShellGlobalHeader (session-wired)", () => {
  beforeEach(() => {
    nav.pathname = "/crm_accounts";
    nav.push.mockReset();
    session.pins = ["payroll", "crm_accounts"];
    session.toggle.mockReset();
  });

  it("gives a rep no Setup gear and only the pins they can view", async () => {
    session.user = { id: "u2", email: "test@example.com", fullName: "Trần Văn Bình", isAdmin: false, roleName: "Nhân viên bán hàng" };
    wrap(<ShellGlobalHeader />);
    expect(screen.queryByRole("button", { name: "Thiết lập" })).toBeNull();
    await userEvent.click(screen.getByRole("button", { name: "Bỏ ghim Khách hàng" }));
    expect(session.toggle).toHaveBeenCalledWith("crm_accounts");
    await openMenu("Xem mục đã ghim");
    expect(screen.getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Khách hàng"]);
    await userEvent.keyboard("{Escape}");
    const menu = await openMenu("Menu tài khoản");
    expect(menu.querySelector("li.slds-dropdown__header")!.nextElementSibling).toHaveTextContent("Nhân viên bán hàng · northwind");
  });

  it("gives a customer owner (not an admin) a gear with exactly one link, Người dùng, to /settings/users (specs/065)", async () => {
    session.user = { id: "u3", email: "chu@khach.vn", fullName: "Chủ Khách", isAdmin: false, canManageUsers: true };
    wrap(<ShellGlobalHeader />);
    await openMenu("Thiết lập");
    const items = screen.getAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual(["Người dùng"]);
    await userEvent.click(items[0]);
    expect(nav.push).toHaveBeenCalledWith("/settings/users");
  });

  it("gives a customer admin the admin pages minus the operator-only ones, and no Studio", async () => {
    session.user = { id: "u1", email: "admin@khach.vn", isAdmin: true, isPlatformOperator: false };
    wrap(<ShellGlobalHeader />);
    await openMenu("Thiết lập");
    const items = screen.getAllByRole("menuitem").map((i) => i.textContent);
    expect(items).toContain("Người dùng");
    expect(items).toContain("Việc nền"); // the delivery log is the customer admin's own (#33)
    expect(items).toContain("Kho khoá kết nối"); // the customer admin keeps partner keys (specs/050 S8a)
    expect(items).not.toContain("Khoá API");
    expect(items).not.toContain("Bác sĩ hệ thống");
    expect(items).not.toContain("Tạo trong Studio");
  });

  it("gives a platform operator every admin page and the Studio bridge (Doctor and API keys moved into the project frame)", async () => {
    session.user = { id: "u0", email: "admin@example.com", isAdmin: true, isPlatformOperator: true };
    wrap(<ShellGlobalHeader />);
    await openMenu("Thiết lập");
    const items = screen.getAllByRole("menuitem").map((i) => i.textContent);
    expect(items).toHaveLength(14);
    expect(items).not.toContain("Khoá API");
    expect(items.at(-1)).toBe("Tạo trong Studio");
  });

  it("carries the tenant brand on the customer face and Nexus's own in the Studio", () => {
    session.user = { id: "u0", email: "admin@example.com", isAdmin: true, isPlatformOperator: true };
    const { container } = wrap(
      <>
        <div data-face="app"><ShellGlobalHeader /></div>
        <div data-face="studio"><ShellGlobalHeader studio /></div>
      </>,
    );
    const logo = (face: string) => container.querySelector(`[data-face="${face}"] .slds-global-header__logo`)!;
    const brand = { appName: "Minh Phát CRM", logoUrl: "https://cdn.minhphat.vn/logo.png", accentColor: "#c80000" };
    try {
      // Applied while both headers are mounted, so useBranding's shared snapshot is reset in finally.
      act(() => applyBranding(brand, { persist: false }));
      expect(logo("app")).toHaveTextContent("Minh Phát CRM");
      expect(logo("app").querySelector("img")).toHaveAttribute("src", brand.logoUrl);
      // contracts/shell.md "Studio KHÔNG nhuộm màu tenant": the old StudioSidebar hard-coded the Nexus logo.
      expect(logo("studio")).toHaveTextContent("Nexus");
      expect(logo("studio")).not.toHaveTextContent("Minh Phát");
      expect(logo("studio").querySelector("img")).toBeNull();
      expect(logo("studio").querySelector("svg.nx-lucide_tile")).not.toBeNull();
    } finally {
      act(() => clearBranding());
    }
  });

  it("in the Studio the brand is 'Nexus Studio' → /studio and the account menu is Studio's own", async () => {
    session.user = { id: "u0", email: "admin@example.com", fullName: "Admin", isAdmin: true, isPlatformOperator: true };
    wrap(<ShellGlobalHeader studio />);
    expect(screen.getByRole("link", { name: "Nexus Studio" })).toHaveAttribute("href", "/studio");
    await openMenu("Menu tài khoản");
    expect(screen.queryByRole("menuitem", { name: "Tài khoản của tôi" })).toBeNull(); // no link to the page until kh4 lands it
    expect(screen.getByRole("menuitem", { name: "Mở app của xưởng Nexus" })).toBeInTheDocument();
    expect(screen.getByRole("menuitem", { name: "Đăng xuất" })).toBeInTheDocument();
    expect(screen.queryByRole("menuitem", { name: "Hồ sơ & mật khẩu" })).toBeNull();
    await userEvent.click(screen.getByRole("menuitem", { name: "Mở app của xưởng Nexus" }));
    expect(nav.push).toHaveBeenCalledWith("/home");
  });

  it("in a build session the Studio's account menu says 'Mở app của khách' (same /home, same tab), never 'xưởng Nexus' (specs/073 T431)", async () => {
    session.user = { id: "u0", email: "admin@example.com", fullName: "Admin", isAdmin: true, isPlatformOperator: true };
    // A state is void unless the home login that opened it is still there (lib/api liveWorkspace, D-21).
    window.localStorage.setItem("nexus_token", "HOME");
    window.localStorage.setItem("nexus_user", JSON.stringify({ id: "u0" }));
    window.sessionStorage.setItem(
      "nexus_workspace",
      JSON.stringify({
        accessToken: "WS", expiresAt: "2099-01-01T00:00:00Z", homeUserId: "u0",
        session: { id: "s", tenantId: "t", tenantSlug: "nhakhoa", tenantName: "Nha khoa An Tâm", openedAt: "2098-01-01T00:00:00Z", expiresAt: "2099-01-01T00:00:00Z" },
      }),
    );
    try {
      wrap(<ShellGlobalHeader studio />);
      await openMenu("Menu tài khoản");
      expect(screen.getByRole("menuitem", { name: "Mở app của khách" })).toBeInTheDocument();
      expect(screen.queryByRole("menuitem", { name: "Mở app của xưởng Nexus" })).toBeNull();
      await userEvent.click(screen.getByRole("menuitem", { name: "Mở app của khách" }));
      expect(nav.push).toHaveBeenCalledWith("/home");
    } finally {
      window.sessionStorage.clear();
      window.localStorage.clear();
    }
  });

  it("the Studio has no pins: no favorites star in its header, the customer face keeps it", () => {
    session.user = { id: "u0", email: "admin@example.com", isAdmin: true, isPlatformOperator: true };
    const { container } = wrap(
      <>
        <div data-face="app"><ShellGlobalHeader /></div>
        <div data-face="studio"><ShellGlobalHeader studio /></div>
      </>,
    );
    const stars = (face: string) => container.querySelectorAll(`[data-face="${face}"] .slds-global-actions__favorites-action`).length;
    expect(stars("app")).toBeGreaterThan(0);
    expect(stars("studio")).toBe(0);
  });

  it("reads the unread count from the shared notification query key", async () => {
    session.user = { id: "u2", email: "test@example.com", isAdmin: false };
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    qc.setQueryData(qk.notificationCount(), 4);
    wrap(<ShellGlobalHeader />, qc);
    expect(await screen.findByRole("button", { name: "4 thông báo mới" })).toBeInTheDocument();
  });
});

// specs/064 T061 / FR-017: a preview tab has no profile, settings or sign-out of its own — the account
// menu says whose eyes these are and offers the one way out.
describe("account menu while previewing a role", () => {
  const PREVIEW_USER = {
    id: "principal", email: "", fullName: "Xem như: Nhân viên kinh doanh", isAdmin: false, roleName: "Nhân viên kinh doanh",
    preview: { sessionId: "s1", roleId: "r1", roleName: "Nhân viên kinh doanh", actorName: "An Nguyễn", expiresAt: "2026-10-08T09:00:00Z", draft: false },
  };

  it("presentational: header names the role and the only item is 'Thoát xem thử'", async () => {
    const onLogout = vi.fn();
    const onNavigate = vi.fn();
    render(<GlobalHeader {...props({ previewRole: "Nhân viên kinh doanh", onLogout, onNavigate })} />);
    const menu = await openMenu("Menu tài khoản");
    expect(within(menu).getByText("Xem như Nhân viên kinh doanh")).toBeInTheDocument();
    expect(within(menu).getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Thoát xem thử"]);
    await userEvent.click(within(menu).getByRole("menuitem", { name: "Thoát xem thử" }));
    expect(onLogout).toHaveBeenCalledTimes(1);
    expect(onNavigate).not.toHaveBeenCalled();
  });

  it("not previewing: the usual profile / settings / sign-out items stay", async () => {
    render(<GlobalHeader {...props()} />);
    const menu = await openMenu("Menu tài khoản");
    expect(within(menu).getAllByRole("menuitem")).toHaveLength(3);
  });

  it("session-wired: a user carrying `preview` gets the preview menu", async () => {
    nav.pathname = "/crm_accounts";
    session.pins = [];
    session.user = PREVIEW_USER;
    wrap(<ShellGlobalHeader />);
    const menu = await openMenu("Menu tài khoản");
    expect(within(menu).getByText("Xem như Nhân viên kinh doanh")).toBeInTheDocument();
    expect(within(menu).getAllByRole("menuitem").map((i) => i.textContent)).toEqual(["Thoát xem thử"]);
  });

  // logout() in a preview tab already navigates back to the Studio page the preview started from;
  // the header must not then overwrite that with /login (caught on a real browser: exit landed on /login).
  describe("leaving", () => {
    const original = window.location;
    let hrefs: string[];
    beforeEach(() => {
      hrefs = [];
      session.logout.mockClear();
      nav.pathname = "/crm_accounts";
      session.pins = [];
      Object.defineProperty(window, "location", {
        configurable: true,
        value: { ...original, set href(v: string) { hrefs.push(v); } },
      });
    });
    afterEach(() => Object.defineProperty(window, "location", { configurable: true, value: original }));

    it("'Thoát xem thử' calls logout() (= leave preview) and sends the page nowhere else", async () => {
      session.user = PREVIEW_USER;
      wrap(<ShellGlobalHeader />);
      const menu = await openMenu("Menu tài khoản");
      await userEvent.click(within(menu).getByRole("menuitem", { name: "Thoát xem thử" }));
      await waitFor(() => expect(session.logout).toHaveBeenCalledTimes(1));
      expect(hrefs).toEqual([]);
    });

    it("a normal 'Đăng xuất' still goes to /login", async () => {
      session.user = { id: "u2", email: "test@example.com", fullName: "Trần Văn Bình", isAdmin: false };
      wrap(<ShellGlobalHeader />);
      const menu = await openMenu("Menu tài khoản");
      await userEvent.click(within(menu).getByRole("menuitem", { name: "Đăng xuất" }));
      await waitFor(() => expect(hrefs).toEqual(["/login"]));
      expect(session.logout).toHaveBeenCalledTimes(1);
    });
  });
});
